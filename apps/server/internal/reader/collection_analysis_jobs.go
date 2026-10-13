package reader

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"
)

type CollectionAnalysisJob struct {
	ID                  string `json:"id"`
	ArXivID             string `json:"arxivId"`
	ExpectedContentHash string `json:"expectedContentHash"`
	Status              string `json:"status"`
	Error               string `json:"error,omitempty"`
	CreatedAt           string `json:"createdAt"`
	FinishedAt          string `json:"finishedAt,omitempty"`
}

type analysisJobContextKey struct{}

func (s *Store) latestAnalysisJob(candidateID string) (*CollectionAnalysisJob, error) {
	var jobID string
	err := s.DB.QueryRow("SELECT job_id FROM collection_analysis_jobs WHERE arxiv_id=? ORDER BY created_at DESC,job_id DESC LIMIT 1", candidateID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job, err := s.AnalysisJob(jobID)
	return &job, err
}

func (s *Store) AnalysisJob(jobID string) (CollectionAnalysisJob, error) {
	var job CollectionAnalysisJob
	err := s.DB.QueryRow("SELECT job_id,arxiv_id,expected_content_hash,status,error,created_at,COALESCE(finished_at,'') FROM collection_analysis_jobs WHERE job_id=?", jobID).Scan(&job.ID, &job.ArXivID, &job.ExpectedContentHash, &job.Status, &job.Error, &job.CreatedAt, &job.FinishedAt)
	return job, err
}

func (m *CollectionManager) submitAnalysis(candidateID string) (CollectionAnalysisJob, error) {
	if m.Draining() || !m.schedulingAllowed() {
		return CollectionAnalysisJob{}, errCollectionPaused
	}
	s := m.server.Store
	cfg, err := s.CollectionSettings()
	if err != nil {
		return CollectionAnalysisJob{}, err
	}
	if !cfg.AnalysisEnabled {
		return CollectionAnalysisJob{}, errors.New("摘要分析未启用")
	}
	candidate, _, err := s.CollectionCandidate(candidateID)
	if err != nil {
		return CollectionAnalysisJob{}, err
	}
	if candidate.Ignored {
		return CollectionAnalysisJob{}, errors.New("候选已忽略")
	}
	materials, err := analysisMaterials(s, candidateID)
	if err != nil {
		return CollectionAnalysisJob{}, err
	}
	if len(materials) == 0 {
		return CollectionAnalysisJob{}, errors.New("候选没有当前启用任务")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return CollectionAnalysisJob{}, err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return CollectionAnalysisJob{}, err
	}
	cfg, err = collectionSettingsTx(tx)
	if err != nil {
		return CollectionAnalysisJob{}, err
	}
	if cfg.Paused || !cfg.AnalysisEnabled {
		return CollectionAnalysisJob{}, errCollectionPaused
	}
	var currentHash string
	var ignored int
	if err := tx.QueryRow("SELECT content_hash,ignored FROM collection_candidates WHERE arxiv_id=?", candidateID).Scan(&currentHash, &ignored); err != nil {
		return CollectionAnalysisJob{}, err
	}
	if currentHash != candidate.ContentHash || ignored != 0 {
		return CollectionAnalysisJob{}, errors.New("候选已改变，请重新提交")
	}
	var activeID string
	err = tx.QueryRow("SELECT job_id FROM collection_analysis_jobs WHERE arxiv_id=? AND status IN ('queued','running')", candidateID).Scan(&activeID)
	if err == nil {
		tx.Rollback()
		job, err := s.AnalysisJob(activeID)
		if err == nil && job.ExpectedContentHash != currentHash {
			return CollectionAnalysisJob{}, errors.New("已有分析处理旧正文，请先取消")
		}
		return job, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CollectionAnalysisJob{}, err
	}
	job := CollectionAnalysisJob{ID: "analysis-job-" + id(), ArXivID: candidateID, ExpectedContentHash: candidate.ContentHash, Status: runQueued, CreatedAt: now()}
	if _, err := tx.Exec("INSERT INTO collection_analysis_jobs(job_id,arxiv_id,expected_content_hash,status,created_at) VALUES(?,?,?,?,?)", job.ID, job.ArXivID, job.ExpectedContentHash, job.Status, job.CreatedAt); err != nil {
		return job, err
	}
	if err := tx.Commit(); err != nil {
		return job, err
	}
	select {
	case m.analysisReady <- struct{}{}:
	default:
	}
	return job, nil
}

func (m *CollectionManager) analysisWorker(ctx context.Context, gen *collectionGeneration) {
	defer gen.analysis.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if m.schedulingAllowed() {
			cfg, err := m.server.Store.CollectionSettings()
			if err == nil && cfg.AnalysisEnabled {
				var jobID string
				err = m.server.Store.DB.QueryRow("SELECT job_id FROM collection_analysis_jobs WHERE status='queued' ORDER BY created_at,job_id LIMIT 1").Scan(&jobID)
				if err == nil {
					if m.executeAnalysisJob(ctx, jobID) {
						continue
					}
				}
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					log.Printf("analysis jobs: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-m.analysisReady:
		case <-ticker.C:
		}
	}
}

func (m *CollectionManager) executeAnalysisJob(ctx context.Context, jobID string) bool {
	job, err := m.server.Store.AnalysisJob(jobID)
	if err != nil {
		return false
	}
	workCtx, ok := m.registerWork(jobID)
	if !ok {
		return false
	}
	defer m.releaseWork(jobID)
	stop := context.AfterFunc(ctx, func() { m.cancelWork(jobID) })
	defer stop()
	if ctx.Err() != nil {
		return false
	}
	if err := m.server.Store.claimAnalysisJob(jobID); err != nil {
		return false
	}
	candidate, _, err := m.server.Store.CollectionCandidate(job.ArXivID)
	if err == nil && candidate.ContentHash != job.ExpectedContentHash {
		err = errors.New("论文正文已改变，请重新分析")
	}
	if err == nil {
		_, err = m.AnalyzeCandidate(context.WithValue(workCtx, analysisJobContextKey{}, job), job.ArXivID, true)
	}
	status, message := runSucceeded, ""
	if err != nil {
		status, message = runFailed, err.Error()
		if workCtx.Err() != nil {
			status = runCancelled
		}
	}
	if err := m.server.Store.finishAnalysisJob(jobID, status, message); err != nil {
		log.Printf("analysis job cleanup: %v", err)
		return false
	}
	return true
}

func (s *Store) claimAnalysisJob(jobID string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	cfg, err := collectionSettingsTx(tx)
	if err != nil {
		return err
	}
	if cfg.Paused || !cfg.AnalysisEnabled {
		return errCollectionPaused
	}
	res, err := tx.Exec("UPDATE collection_analysis_jobs SET status='running' WHERE job_id=? AND status='queued'", jobID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("分析命令已关闭")
	}
	return tx.Commit()
}

func (s *Store) finishAnalysisJob(jobID, status, message string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE collection_analysis_jobs SET status=?,error=?,finished_at=? WHERE job_id=? AND status IN ('queued','running')", status, message, now(), jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func cancelOpenAnalysisJobsTx(tx *sql.Tx, message string) error {
	_, err := tx.Exec("UPDATE collection_analysis_jobs SET status='cancelled',error=?,finished_at=? WHERE status IN ('queued','running')", message, now())
	return err
}

func (s *Server) collectionAnalysisJobGet(w http.ResponseWriter, r *http.Request) {
	job, err := s.Store.AnalysisJob(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "analysis-unavailable", "分析命令不存在")
		return
	}
	respond(w, 200, job)
}

func (s *Server) collectionAnalysisJobCancel(w http.ResponseWriter, r *http.Request) {
	job, err := s.Store.AnalysisJob(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "analysis-unavailable", "分析命令不存在")
		return
	}
	if err := s.Store.finishAnalysisJob(job.ID, runCancelled, "用户取消"); err != nil {
		collectionFail(w, 500, "analysis-unavailable", "取消失败")
		return
	}
	s.collection().cancelWork(job.ID)
	respond(w, 202, map[string]string{"id": job.ID})
}
