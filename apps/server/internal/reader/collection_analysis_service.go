package reader

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"
)

type paperAnalysisAPIAdapter struct {
	conn    APIConnection
	request completionRequest
}

func (a paperAnalysisAPIAdapter) Stream(ctx context.Context, req GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
	return streamCompletion(ctx, a.conn, a.request, req.Input, emit)
}

type paperAnalysisConnection struct {
	adapter                                                     Adapter
	provider, model, effort, fingerprint, selection, connection string
}

func collectionDigest(v any) string {
	hash := sha256.Sum256([]byte(mustJSONText(v)))
	return hex.EncodeToString(hash[:])
}
func analysisSelection(cfg CollectionSettings, primary string) string {
	provider := cfg.AnalysisProvider
	if provider == "" {
		provider = primary
	}
	return collectionDigest([]string{provider, cfg.AnalysisModel, cfg.AnalysisEffort})
}

func (s *Server) paperAnalysisConnection(ctx context.Context, cfg CollectionSettings) (paperAnalysisConnection, error) {
	ai := s.aiConfig()
	p := cfg.AnalysisProvider
	if p == "" {
		p = ai.Primary
	}
	if !validAgent(p) {
		return paperAnalysisConnection{}, generationError(ErrorConfiguration, "请配置摘要分析服务")
	}
	var models []AgentModel
	var err error
	if p == "api" {
		models, err = s.apiAgentModels(ctx, ai)
	} else {
		models, err = s.modelCatalog(ctx, p)
	}
	model := cfg.AnalysisModel
	if err != nil && (model == "" || p != "api") {
		return paperAnalysisConnection{}, err
	}
	if model == "" {
		model = defaultTaskModel(taskPaperAnalysis, p, models)
	}
	if model == "" {
		for _, m := range models {
			if m.Default || p == "api" && m.Capability != nil && m.Capability.Text {
				model = m.ID
				break
			}
		}
	}
	if model == "" {
		return paperAnalysisConnection{}, generationError(ErrorConfiguration, "无法解析摘要分析模型")
	}
	for _, m := range models {
		if m.ID == model {
			break
		}
		for _, alias := range m.Aliases {
			if alias == model {
				model = m.ID
				break
			}
		}
	}
	capability := p
	if p == "api" {
		capability = apiCapabilityKey(model)
	}
	if !capable(ai, capability, false) {
		return paperAnalysisConnection{}, generationError(ErrorCapability, "摘要分析模型尚未通过能力检测")
	}
	level := cfg.AnalysisEffort
	if level == "" {
		level = "medium"
	}
	effort := nativeEffort(p, level, model, models)
	if cfg.AnalysisEffort != "" && effort == "" {
		return paperAnalysisConnection{}, generationError(ErrorConfiguration, "模型不支持指定的 Effort")
	}
	c := paperAnalysisConnection{provider: p, model: model, effort: effort, selection: analysisSelection(cfg, ai.Primary), connection: configPrint(ai, capability)}
	c.fingerprint = collectionDigest([]string{p, model, effort, c.connection, "native-analysis-v1"})
	if p == "api" {
		c.adapter = paperAnalysisAPIAdapter{ai.API, completionRequest{Model: model, Effort: effort}}
	} else {
		c.adapter = cliAdapter{s.Store.Root, p, model}
	}
	return c, nil
}

func analysisMaterials(s *Store, candidateID string) ([]AnalysisTaskMaterial, error) {
	tasks, err := s.CollectionTasks()
	if err != nil {
		return nil, err
	}
	materials := []AnalysisTaskMaterial{}
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		var matched int
		if err := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM collection_matches WHERE arxiv_id=? AND task_id=? AND query_revision=?)", candidateID, task.ID, task.QueryRevision).Scan(&matched); err != nil {
			return nil, err
		}
		if matched == 0 {
			continue
		}
		materials = append(materials, AnalysisTaskMaterial{TaskID: task.ID, ProfileHash: task.Config.SemanticHash(), Topic: task.Config.ResearchTopic, NotInterest: task.Config.NotInterested, Keywords: mustJSONText(task.Config.KeywordGroups), WantsScore: task.Config.WantsRelevance()})
	}
	sort.Slice(materials, func(i, j int) bool { return materials[i].TaskID < materials[j].TaskID })
	return materials, nil
}

func analysisRequestKey(candidate CollectionCandidate, input AnalysisInput, connection paperAnalysisConnection) string {
	return collectionDigest([]any{candidate.ArXivID, candidate.CanonicalVer, candidate.ContentHash, input.Tasks, connection.fingerprint, analysisPromptVersion, collectionResultVersion})
}

// Every request owns a fresh native session and cancellable manager handle.
// The serial admission avoids duplicate paid work without blocking reader chat.
func (m *CollectionManager) AnalyzeCandidate(ctx context.Context, candidateID string, manual bool) ([]CollectionAnalysis, error) {
	return m.analyzeCandidate(ctx, candidateID, manual, nil)
}

type analysisOperation struct{ sent int }

func (m *CollectionManager) analyzeCandidate(ctx context.Context, candidateID string, manual bool, operation *analysisOperation) ([]CollectionAnalysis, error) {
	owner := "analysis-" + id()
	workCtx, ok := m.registerWork(owner)
	if !ok {
		return nil, errCollectionPaused
	}
	defer m.releaseWork(owner)
	stop := context.AfterFunc(ctx, func() { m.cancelWork(owner) })
	defer stop()
	workCtx, cancel := context.WithTimeout(workCtx, 5*time.Minute)
	defer cancel()
	select {
	case m.analysisSerial <- struct{}{}:
	case <-workCtx.Done():
		return nil, workCtx.Err()
	}
	defer func() { <-m.analysisSerial }()
	s := m.server.Store
	if err := s.CollectionWritable(); err != nil {
		return nil, err
	}
	cfg, err := s.CollectionSettings()
	if err != nil {
		return nil, err
	}
	if !cfg.AnalysisEnabled || cfg.Paused {
		return nil, errors.New("摘要分析未启用或已暂停")
	}
	candidate, _, err := s.CollectionCandidate(candidateID)
	if err != nil {
		return nil, err
	}
	if candidate.Ignored {
		return nil, errors.New("候选已忽略")
	}
	materials, err := analysisMaterials(s, candidateID)
	if err != nil {
		return nil, err
	}
	if len(materials) == 0 {
		return nil, errors.New("候选没有当前启用任务")
	}
	connection, err := m.server.paperAnalysisConnection(workCtx, cfg)
	if err != nil {
		return nil, err
	}
	if err := m.server.rememberAnalysisIdentity(connection); err != nil {
		return nil, err
	}
	results, missing, err := s.cachedAnalysisFragments(candidate, materials, connection.fingerprint)
	if err != nil {
		return nil, err
	}
	if len(missing) == 0 {
		return results, nil
	}
	inputs, err := BuildAnalysisInputs(candidate.Title, candidate.Abstract, candidate.Categories, missing)
	if err != nil {
		return results, err
	}
	for _, input := range inputs {
		key := analysisRequestKey(candidate, input, connection)
		cached, err := s.cachedPaperAnalysis(key, input)
		if err == nil {
			results = append(results, cached)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return results, err
		}
		if !manual {
			var attempt int
			var retry sql.NullString
			if err := s.DB.QueryRow("SELECT count(*) FROM collection_attempts WHERE request_key=? AND status IN ('dispatching','sent','settled')", key).Scan(&attempt); err != nil {
				return results, err
			}
			err := s.DB.QueryRow("SELECT retry_at FROM collection_analyses WHERE cache_key=?", key).Scan(&retry)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return results, err
			}
			if attempt >= 3 {
				continue
			}
			if retry.Valid {
				at, err := time.Parse(time.RFC3339Nano, retry.String)
				if err != nil {
					return results, err
				}
				if m.clockNow().Before(at) {
					continue
				}
			}
		}
		attemptID := id()
		ok, err := s.ReserveAnalysisAttempt(attemptID, key, key, utcDay(m.clockNow()), cfg.DailyLimit)
		if err != nil {
			return results, err
		}
		if !ok {
			return results, errAnalysisQuota
		}
		hook := &generationDispatch{
			Before: func(ctx context.Context) error {
				if err := m.server.waitModelRPM(ctx); err != nil {
					return err
				}
				latest, err := s.CollectionSettings()
				if err != nil {
					return err
				}
				ai := m.server.aiConfig()
				capability := connection.provider
				if capability == "api" {
					capability = apiCapabilityKey(connection.model)
				}
				if analysisSelection(latest, ai.Primary) != connection.selection || configPrint(ai, capability) != connection.connection {
					return errors.New("摘要分析配置已改变")
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return s.BeginAnalysisDispatch(attemptID, m.clockNow())
			},
			Sent:         func() error { return s.MarkAttemptSent(attemptID) },
			NotDelivered: func() error { return s.releaseUndeliveredAnalysis(attemptID) },
		}
		generated, generateErr := runAttempt(workCtx, connection.adapter, AIInput{System: analysisSystemPrompt, Prompt: analysisPrompt(input), Effort: connection.effort, Dispatch: hook, NoRetry: true}, nil)
		var attemptStatus string
		if err := s.DB.QueryRow("SELECT status FROM collection_attempts WHERE attempt_id=?", attemptID).Scan(&attemptStatus); err != nil {
			return results, err
		}
		sent := attemptStatus == attemptSent || attemptStatus == attemptDispatching
		if sent && operation != nil {
			operation.sent++
		}
		switch attemptStatus {
		case attemptReserved:
			err = s.ReleaseAttempt(attemptID)
		case attemptDispatching:
			err = s.SettleSentAttempt(attemptID, true)
		case attemptSent:
			err = s.SettleSentAttempt(attemptID, false)
		case attemptReleased:
		default:
			err = errors.New("分析请求状态无效")
		}
		if err != nil {
			return results, err
		}
		var result AnalysisResult
		if generateErr == nil {
			result, generateErr = DecodeAnalysisResult(generated.Text, input)
		}
		row := CollectionAnalysis{CacheKey: key, ArXivID: candidate.ArXivID, ContentHash: candidate.ContentHash, ProfileHash: collectionDigest(input.Tasks), ModelFingerprint: connection.fingerprint, PromptVersion: analysisPromptVersion, ResultVersion: collectionResultVersion, Status: analysisSucceeded}
		if generateErr != nil {
			row.Status, row.Error = analysisFailed, generateErr.Error()
		}
		if err := s.savePaperAnalysis(row, result, input, m.clockNow()); err != nil {
			return results, err
		}
		if generateErr != nil {
			return results, generateErr
		}
		cached, err = s.cachedPaperAnalysis(key, input)
		if err != nil {
			return results, err
		}
		results = append(results, cached)
	}
	return results, nil
}

func (s *Store) cachedPaperAnalysis(key string, input AnalysisInput) (CollectionAnalysis, error) {
	var a CollectionAnalysis
	var assessments, title, abstract string
	err := s.DB.QueryRow("SELECT cache_key,arxiv_id,content_hash,profile_hash,model_fingerprint,prompt_version,result_version,title_zh,abstract_zh,assessments_json,attempt,created_at,updated_at FROM collection_analyses WHERE cache_key=? AND status='succeeded'", key).Scan(&a.CacheKey, &a.ArXivID, &a.ContentHash, &a.ProfileHash, &a.ModelFingerprint, &a.PromptVersion, &a.ResultVersion, &title, &abstract, &assessments, &a.Attempt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	result, err := DecodeAnalysisResult(assessments, input)
	if err != nil {
		return a, err
	}
	a.Status = analysisSucceeded
	if result.TitleZH != title || result.AbstractZH != abstract {
		return a, errors.New("已缓存译文与结果不一致")
	}
	a.Result = analysisWireResult(result)
	return a, nil
}

func (s *Store) savePaperAnalysis(a CollectionAnalysis, result AnalysisResult, input AnalysisInput, at time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	var currentHash string
	if err := tx.QueryRow("SELECT content_hash FROM collection_candidates WHERE arxiv_id=?", a.ArXivID).Scan(&currentHash); err != nil {
		return err
	}
	if currentHash != a.ContentHash {
		return errors.New("论文正文已改变，分析结果不再有效")
	}
	for _, material := range input.Tasks {
		var body string
		var enabled int
		if err := tx.QueryRow("SELECT config_json,enabled FROM collection_tasks WHERE task_id=?", material.TaskID).Scan(&body, &enabled); err != nil {
			return err
		}
		var cfg CollectionTaskConfig
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			return err
		}
		if enabled == 0 || cfg.SemanticHash() != material.ProfileHash {
			return errors.New("任务语义已改变，分析结果不再有效")
		}
	}
	var attempts int
	if err := tx.QueryRow("SELECT count(*) FROM collection_attempts WHERE request_key=? AND status IN ('dispatching','sent','settled')", a.CacheKey).Scan(&attempts); err != nil {
		return err
	}
	var retry any
	if a.Status == analysisFailed {
		delay := 30 * time.Minute
		if attempts >= 2 {
			delay = 60 * time.Minute
		}
		retry = at.Add(delay).Format(time.RFC3339Nano)
	}
	stamp := at.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO collection_analyses(cache_key,arxiv_id,content_hash,profile_hash,model_fingerprint,prompt_version,result_version,title_zh,abstract_zh,assessments_json,status,error,attempt,retry_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(cache_key) DO UPDATE SET title_zh=excluded.title_zh,abstract_zh=excluded.abstract_zh,assessments_json=excluded.assessments_json,status=excluded.status,error=excluded.error,attempt=excluded.attempt,retry_at=excluded.retry_at,updated_at=excluded.updated_at`, a.CacheKey, a.ArXivID, a.ContentHash, a.ProfileHash, a.ModelFingerprint, a.PromptVersion, a.ResultVersion, result.TitleZH, result.AbstractZH, mustJSONText(result), a.Status, a.Error, attempts, retry, stamp, stamp); err != nil {
		return err
	}
	if a.Status == analysisSucceeded {
		for _, assessment := range result.Assessments {
			if _, err := tx.Exec("UPDATE collection_matches SET ai_score=?,analysis_cache_key=? WHERE arxiv_id=? AND task_id=? AND query_revision=(SELECT query_revision FROM collection_tasks WHERE task_id=?)", assessment.RelevanceScore, a.CacheKey, a.ArXivID, assessment.TaskID, assessment.TaskID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Server) collectionAnalyze(w http.ResponseWriter, r *http.Request) {
	results, err := s.collection().AnalyzeCandidate(r.Context(), r.PathValue("id"), true)
	if err != nil {
		collectionFail(w, 409, "analysis-unavailable", err.Error())
		return
	}
	respond(w, 200, map[string]any{"analyses": results})
}
