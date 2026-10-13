package reader

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func submitAnalysisHTTP(t *testing.T, s *Server, candidateID string) CollectionAnalysisJob {
	t.Helper()
	response := request(t, s, "POST", "/api/collection/candidates/"+candidateID+"/analysis", strings.NewReader(`{}`))
	if response.Code != 202 {
		t.Fatalf("analysis submission: %d %s", response.Code, response.Body.String())
	}
	var job CollectionAnalysisJob
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil || job.ID == "" || job.ArXivID != candidateID {
		t.Fatalf("invalid accepted job: %s %v", response.Body.String(), err)
	}
	return job
}

func waitAnalysisJobHTTP(t *testing.T, s *Server, jobID string) CollectionAnalysisJob {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		response := request(t, s, "GET", "/api/collection/analysis/"+jobID, nil)
		var job CollectionAnalysisJob
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &job) != nil {
			t.Fatalf("job status: %d %s", response.Code, response.Body.String())
		}
		if job.Status != runQueued && job.Status != runRunning {
			return job
		}
		select {
		case <-deadline.C:
			t.Fatalf("analysis job did not terminate: %+v", job)
		case <-tick.C:
		}
	}
}

func TestPaperAnalysisAsyncHTTPAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "page-disconnected"
		if cancel {
			name = "explicit-cancel"
		}
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			candidate := seedAnalysisCandidate(t, s)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			calls := analysisAPIFixture(t, s, func(input AnalysisInput) string {
				close(entered)
				<-release
				return mustJSONText(fixtureAnalysisResult(input))
			})
			// The HTTP caller is gone before the native response is released.
			job := submitAnalysisHTTP(t, s, candidate.ArXivID)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("accepted job did not dispatch")
			}
			duplicate := submitAnalysisHTTP(t, s, candidate.ArXivID)
			if duplicate.ID != job.ID {
				t.Fatal("duplicate submission created another active job")
			}
			if cancel {
				response := request(t, s, "POST", "/api/collection/analysis/"+job.ID+"/cancel", nil)
				if response.Code != 202 {
					t.Fatalf("cancel: %d %s", response.Code, response.Body.String())
				}
			}
			unblock()
			terminal := waitAnalysisJobHTTP(t, s, job.ID)
			want := runSucceeded
			if cancel {
				want = runCancelled
			}
			if terminal.Status != want || terminal.FinishedAt == "" {
				t.Fatalf("terminal job: %+v", terminal)
			}
			// Wait for attempt cleanup, independently of the published cancellation.
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			manager := s.Collection()
			manager.mu.Lock()
			gen := manager.gen
			manager.mu.Unlock()
			gen.cancel()
			stopped := make(chan struct{})
			go func() { gen.analysis.Wait(); gen.work.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("analysis owner did not finish cleanup")
			}
			var succeeded, sent, reserved int
			if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_analyses WHERE status='succeeded'").Scan(&succeeded); err != nil {
				t.Fatal(err)
			}
			if (succeeded == 1) != !cancel {
				t.Fatalf("cancelled result became valid: %d", succeeded)
			}
			if err := s.Store.DB.QueryRow("SELECT sent,reserved FROM collection_quota_days").Scan(&sent, &reserved); err != nil || sent != 1 || reserved != 0 || calls.Load() != 1 {
				t.Fatalf("paid cancellation/cache accounting: %d %d calls=%d %v", sent, reserved, calls.Load(), err)
			}
			detail := request(t, s, "GET", "/api/collection/candidates/"+candidate.ArXivID, nil)
			var body struct {
				AnalysisJob *CollectionAnalysisJob `json:"analysisJob"`
			}
			if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &body) != nil || body.AnalysisJob == nil || body.AnalysisJob.Status != want {
				t.Fatalf("detail lost persistent status: %d %s", detail.Code, detail.Body.String())
			}
		})
	}
}

func TestPaperAnalysisOldCancellationCannotCancelNewJob(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	analysisAPIFixture(t, s, func(input AnalysisInput) string { return mustJSONText(fixtureAnalysisResult(input)) })
	first := submitAnalysisHTTP(t, s, candidate.ArXivID)
	if job := waitAnalysisJobHTTP(t, s, first.ID); job.Status != runSucceeded {
		t.Fatalf("first job: %+v", job)
	}
	second := submitAnalysisHTTP(t, s, candidate.ArXivID)
	if second.ID == first.ID {
		t.Fatal("terminal job reused")
	}
	if response := request(t, s, "POST", "/api/collection/analysis/"+first.ID+"/cancel", nil); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	if job := waitAnalysisJobHTTP(t, s, second.ID); job.Status != runSucceeded {
		t.Fatalf("old cancellation affected new job: %+v", job)
	}
}

func TestPaperAnalysisCancelledJobRejectsLateSaveWithoutContextSignal(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	analysisAPIFixture(t, s, func(input AnalysisInput) string { return mustJSONText(fixtureAnalysisResult(input)) })
	job := CollectionAnalysisJob{ID: "cancelled-job", ArXivID: candidate.ArXivID, ExpectedContentHash: candidate.ContentHash}
	if _, err := s.Store.DB.Exec("INSERT INTO collection_analysis_jobs(job_id,arxiv_id,expected_content_hash,status,created_at,finished_at) VALUES(?,?,?,'cancelled',?,?)", job.ID, job.ArXivID, job.ExpectedContentHash, now(), now()); err != nil {
		t.Fatal(err)
	}
	materials, err := analysisMaterials(s.Store, candidate.ArXivID)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := BuildAnalysisInputs(candidate.Title, candidate.Abstract, candidate.Categories, materials)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		t.Fatal(err)
	}
	row := CollectionAnalysis{CacheKey: "late-result", ArXivID: candidate.ArXivID, ContentHash: candidate.ContentHash, Status: analysisSucceeded}
	if err := s.Store.savePaperAnalysis(context.Background(), job, cfg, row, fixtureAnalysisResult(inputs[0]), inputs[0], time.Now()); err == nil {
		t.Fatal("persistent cancellation depended only on asynchronous context propagation")
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_analyses").Scan(&count); err != nil || count != 0 {
		t.Fatalf("late result persisted: %d %v", count, err)
	}
}

func TestPaperAnalysisRecoveryClosesQueuedJobWithoutDispatch(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	if _, err := s.Store.DB.Exec("INSERT INTO collection_analysis_jobs(job_id,arxiv_id,expected_content_hash,status,created_at) VALUES(?,?,?,'queued',?)", "queued-recovery", candidate.ArXivID, candidate.ContentHash, now()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RecoverImports(); err != nil {
			t.Fatal(err)
		}
		job, err := s.Store.AnalysisJob("queued-recovery")
		if err != nil || job.Status != runInterrupted || job.FinishedAt == "" {
			t.Fatalf("queued recovery: %+v %v", job, err)
		}
	}
	var attempts int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_attempts").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("queued recovery sent paid work: %d %v", attempts, err)
	}
}
