package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// collectionFail reports a stable machine-readable error code alongside the
// Chinese message, matching the existing error envelope.
func collectionFail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": message, "code": code})
}

func (s *Server) collection() *CollectionManager {
	return s.collectionManager
}

// collectionTaskView adds the derived open-run pointer to list items. The
// run is never persisted on the task row.
type collectionTaskView struct {
	CollectionTask
	ActiveRun *CollectionRun `json:"activeRun"`
}

func (s *Server) taskView(task CollectionTask) collectionTaskView {
	v := collectionTaskView{CollectionTask: task}
	if run, err := s.Store.OpenCollectionRun(task.ID); err == nil {
		v.ActiveRun = &run
	}
	return v
}

func (s *Server) collectionTasksList(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.CollectionTasks()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取任务")
		return
	}
	views := make([]collectionTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, s.taskView(task))
	}
	respond(w, 200, views)
}

type collectionTaskBody struct {
	Name     *string               `json:"name"`
	Config   *CollectionTaskConfig `json:"config"`
	Enabled  *bool                 `json:"enabled"`
	Priority *int                  `json:"priority"`
	// ExpectedRevision guards updates against stale forms; omitted on create.
	ExpectedRevision *int `json:"expectedRevision"`
}

func (s *Server) collectionTaskCreate(w http.ResponseWriter, r *http.Request) {
	var body collectionTaskBody
	if !decode(w, r, &body) {
		return
	}
	task := CollectionTask{Enabled: true, Priority: 3}
	if body.Name != nil {
		task.Name = *body.Name
	}
	if body.Enabled != nil {
		task.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		task.Priority = *body.Priority
	}
	if body.Config != nil {
		task.Config = *body.Config
	} else {
		task.Config = DefaultCollectionTaskConfig()
	}
	saved, err := s.Store.SaveCollectionTask(task, -1)
	if err != nil {
		if errors.Is(err, errInvalidTaskConfig) {
			collectionFail(w, 400, "query-invalid", err.Error())
			return
		}
		collectionFail(w, 500, "query-invalid", "任务保存失败")
		return
	}
	respond(w, 201, saved)
}

func (s *Server) collectionTaskUpdate(w http.ResponseWriter, r *http.Request) {
	var body collectionTaskBody
	if !decode(w, r, &body) {
		return
	}
	task, err := s.Store.CollectionTask(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "query-invalid", "任务不存在")
		return
	}
	if body.Name != nil {
		task.Name = *body.Name
	}
	if body.Enabled != nil {
		task.Enabled = *body.Enabled
	}
	if body.Priority != nil {
		task.Priority = *body.Priority
	}
	if body.Config != nil {
		task.Config = *body.Config
	}
	expected := task.QueryRevision
	if body.ExpectedRevision != nil {
		expected = *body.ExpectedRevision
	}
	saved, err := s.Store.SaveCollectionTask(task, expected)
	if errors.Is(err, errTaskConflict) {
		collectionFail(w, 409, "task-conflict", "任务已被其他修改更新，请刷新后重试")
		return
	}
	if err != nil {
		if errors.Is(err, errInvalidTaskConfig) {
			collectionFail(w, 400, "query-invalid", err.Error())
			return
		}
		collectionFail(w, 500, "query-invalid", "任务保存失败")
		return
	}
	respond(w, 200, saved)
}

func (s *Server) collectionRunCreate(w http.ResponseWriter, r *http.Request) {
	run, err := s.collection().EnqueueRun(r.PathValue("id"), "manual")
	if errors.Is(err, errCollectionPaused) {
		collectionFail(w, 409, "import-busy", err.Error())
		return
	}
	if err != nil {
		collectionFail(w, 400, "query-invalid", err.Error())
		return
	}
	respond(w, 202, run)
}

func (s *Server) collectionRunGet(w http.ResponseWriter, r *http.Request) {
	var run CollectionRun
	var config, rangeJSON string
	err := s.Store.DB.QueryRow("SELECT run_id,task_id,query_revision,trigger_kind,config_snapshot,range_json,exec_token,status,failure_class,COALESCE(started_at,''),COALESCE(finished_at,'') FROM collection_runs WHERE run_id=?", r.PathValue("id")).
		Scan(&run.ID, &run.TaskID, &run.QueryRevision, &run.Trigger, &config, &rangeJSON, &run.ExecToken, &run.Status, &run.FailureClass, &run.StartedAt, &run.FinishedAt)
	if err != nil {
		collectionFail(w, 404, "query-invalid", "运行不存在")
		return
	}
	run.ConfigSnapshot = json.RawMessage(config)
	run.RangeJSON = json.RawMessage(rangeJSON)
	respond(w, 200, run)
}

func (s *Server) collectionRunCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.collection().CancelRun(r.PathValue("id")); err != nil {
		collectionFail(w, 404, "query-invalid", "运行不存在或已结束")
		return
	}
	respond(w, 202, map[string]string{"status": "cancelled"})
}

func (s *Server) collectionCandidatesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := CandidateFilter{
		TaskID:   q.Get("task"),
		Channel:  q.Get("channel"),
		Search:   q.Get("q"),
		Citation: q.Get("citation"),
		Sort:     q.Get("sort"),
	}
	if f.Channel != channelLatest && f.Channel != channelHot && f.Channel != "" {
		collectionFail(w, 400, "query-invalid", "无效的通道")
		return
	}
	if f.Sort == "" {
		if f.Channel == channelHot {
			f.Sort = "citations"
		} else {
			f.Sort = "date"
		}
	}
	// The explicit since filter is part of the cursor identity; pages under a
	// cursor keep the cursor's since snapshot so the auto-window cannot drift
	// with the clock between pages.
	explicitSince := q.Get("since")
	f.Since = explicitSince
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := q.Get("ignored"); v == "1" || v == "true" {
		t := true
		f.Ignored = &t
	} else if v == "0" || v == "false" {
		t := false
		f.Ignored = &t
	}
	f.TaskRev = 0
	if f.TaskID != "" {
		if t, err := s.Store.CollectionTask(f.TaskID); err == nil {
			f.TaskRev = t.QueryRevision
		}
	}
	if cursor := q.Get("cursor"); cursor != "" {
		key, date, id, since, err := DecodeCursor(cursor, f)
		if err != nil {
			collectionFail(w, 400, "query-invalid", "无效的游标")
			return
		}
		f.CursorNum, f.CursorDate, f.CursorID = key, date, id
		f.Since = since
	} else if explicitSince == "" && f.Channel != "" {
		// The window lower bound is the selected task's own window, or the
		// widest enabled-task window, or the design defaults only when no
		// task is enabled.
		f.Since = s.windowStart(f.Channel, f.TaskID)
	}
	var identityErr error
	f.AnalysisFingerprint, identityErr = s.activeAnalysisFingerprint()
	if identityErr != nil {
		collectionFail(w, 500, "analysis-unavailable", "无法读取分析身份")
		return
	}
	page, err := s.Store.CollectionCandidates(f)
	if err != nil {
		collectionFail(w, 500, "query-invalid", "查询失败")
		return
	}
	type row struct {
		Candidate CollectionCandidate `json:"candidate"`
		Score     int                 `json:"score"`
	}
	rows := make([]row, 0, len(page.Items))
	for i, c := range page.Items {
		rows = append(rows, row{Candidate: c, Score: page.Scores[i]})
	}
	respond(w, 200, map[string]any{"items": rows, "nextCursor": page.NextCursor})
}

// windowStart returns the lower bound for the channel: the selected task's
// own window when given, otherwise the widest enabled-task window, otherwise
// the design defaults. A narrowed task window must narrow the view.
func (s *Server) windowStart(channel, taskID string) string {
	days, months := 30, 12
	if taskID != "" {
		if t, err := s.Store.CollectionTask(taskID); err == nil {
			days, months = t.Config.LatestWindowDays, t.Config.HotWindowMonths
		}
	} else if tasks, err := s.Store.CollectionTasks(); err == nil {
		found := false
		for _, t := range tasks {
			if !t.Enabled {
				continue
			}
			if !found {
				days, months = t.Config.LatestWindowDays, t.Config.HotWindowMonths
				found = true
				continue
			}
			if t.Config.LatestWindowDays > days {
				days = t.Config.LatestWindowDays
			}
			if t.Config.HotWindowMonths > months {
				months = t.Config.HotWindowMonths
			}
		}
	}
	now := time.Now().UTC()
	if channel == channelHot {
		return subCalendarMonths(now, months).Format(time.RFC3339)
	}
	return now.AddDate(0, 0, -days).Format(time.RFC3339)
}

func (s *Server) collectionCandidateGet(w http.ResponseWriter, r *http.Request) {
	c, cit, err := s.Store.CollectionCandidate(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "query-invalid", "候选不存在")
		return
	}
	fingerprint, err := s.activeAnalysisFingerprint()
	if err != nil {
		collectionFail(w, 500, "analysis-unavailable", "无法读取分析身份")
		return
	}
	materials, err := analysisMaterials(s.Store, c.ArXivID)
	if err != nil {
		collectionFail(w, 500, "analysis-unavailable", "无法读取分析任务")
		return
	}
	analyses, _, err := s.Store.cachedAnalysisFragments(c, materials, fingerprint)
	if err != nil {
		collectionFail(w, 500, "analysis-unavailable", "分析缓存无效")
		return
	}
	job, err := s.Store.latestAnalysisJob(c.ArXivID)
	if err != nil {
		collectionFail(w, 500, "analysis-unavailable", "无法读取分析命令")
		return
	}
	// latestImport derives from existing requests so the detail view can
	// restore import status after switching pages; absent records stay null.
	var latestImport *collectionImportView
	if req, err := s.Store.LatestCollectionImport(c.ArXivID); err == nil {
		v := collectionImportView{CollectionImportRequest: req, DocumentState: s.importDocumentState(req)}
		latestImport = &v
	} else if !errors.Is(err, sql.ErrNoRows) {
		collectionFail(w, 500, "query-invalid", "无法读取导入状态")
		return
	}
	respond(w, 200, map[string]any{"candidate": c, "citation": cit, "analyses": analyses, "analysisJob": job, "latestImport": latestImport})
}

// collectionImportView reports the live document/library state beside the
// stored request so the UI never offers to open a deleted or trashed file.
type collectionImportView struct {
	CollectionImportRequest
	DocumentState string `json:"documentState"`
}

func (s *Server) importDocumentState(req CollectionImportRequest) string {
	if req.DocumentID == "" {
		return "missing"
	}
	doc, err := s.Store.Document(req.DocumentID)
	if err != nil {
		return "missing"
	}
	if doc.DeletedAt != "" {
		return "trash"
	}
	return "active"
}

func (s *Server) collectionCandidatePatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ignored *bool `json:"ignored"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Ignored == nil {
		collectionFail(w, 400, "query-invalid", "只允许修改 ignored")
		return
	}
	if err := s.Store.SetCandidateIgnored(r.PathValue("id"), *body.Ignored); err != nil {
		collectionFail(w, 500, "query-invalid", "保存失败")
		return
	}
	c, cit, err := s.Store.CollectionCandidate(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "query-invalid", "候选不存在")
		return
	}
	respond(w, 200, map[string]any{"candidate": c, "citation": cit})
}

func (s *Server) collectionSettingsGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.collectionSettingsView()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取采集设置")
		return
	}
	respond(w, 200, cfg)
}

func (s *Server) collectionSettingsView() (CollectionSettings, error) {
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		return cfg, err
	}
	cfg.HasCitationsKey = s.aiConfig().CitationsAPIKey != ""
	quota := &CollectionQuota{UTCDay: utcDay(s.collection().clockNow())}
	err = s.Store.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days WHERE utc_day=?", quota.UTCDay).Scan(&quota.Sent, &quota.Reserved, &quota.Uncertain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cfg, err
	}
	cfg.Quota = quota
	return cfg, nil
}

func (s *Server) collectionSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body CollectionSettings
	if !decode(w, r, &body) {
		return
	}
	// Secrets are written through the protected AI configuration owner only;
	// this endpoint never accepts key material.
	previous, err := s.Store.CollectionSettings()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取采集设置")
		return
	}
	if err := s.Store.SaveCollectionSettings(body); err != nil {
		collectionFail(w, 500, "query-invalid", "设置保存失败")
		return
	}
	if previous.Paused != body.Paused {
		if err := s.collection().SetPaused(body.Paused); err != nil {
			collectionFail(w, 500, "query-invalid", "无法调整暂停状态")
			return
		}
	}
	if !body.AnalysisEnabled || previous.AnalysisProvider != body.AnalysisProvider || previous.AnalysisModel != body.AnalysisModel || previous.AnalysisEffort != body.AnalysisEffort {
		s.collection().cancelAnalysisWork()
	}
	cfg, err := s.collectionSettingsView()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取采集设置")
		return
	}
	respond(w, 200, cfg)
}

func (s *Server) collectionPause(w http.ResponseWriter, r *http.Request) {
	if err := s.collection().SetPaused(true); err != nil {
		collectionFail(w, 500, "query-invalid", "暂停失败")
		return
	}
	respond(w, 200, map[string]bool{"paused": true})
}

func (s *Server) collectionResume(w http.ResponseWriter, r *http.Request) {
	if err := s.collection().SetPaused(false); err != nil {
		collectionFail(w, 500, "query-invalid", "恢复失败")
		return
	}
	respond(w, 200, map[string]bool{"paused": false})
}

// registerCollectionRoutes wires the collection API behind the existing
// authentication middleware.
func (s *Server) registerCollectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/collection/tasks", s.collectionTasksList)
	mux.HandleFunc("POST /api/collection/tasks", s.collectionTaskCreate)
	mux.HandleFunc("PUT /api/collection/tasks/{id}", s.collectionTaskUpdate)
	mux.HandleFunc("POST /api/collection/tasks/{id}/runs", s.collectionRunCreate)
	mux.HandleFunc("GET /api/collection/runs/{id}", s.collectionRunGet)
	mux.HandleFunc("POST /api/collection/runs/{id}/cancel", s.collectionRunCancel)
	mux.HandleFunc("GET /api/collection/candidates", s.collectionCandidatesList)
	mux.HandleFunc("GET /api/collection/candidates/{id}", s.collectionCandidateGet)
	mux.HandleFunc("PATCH /api/collection/candidates/{id}", s.collectionCandidatePatch)
	mux.HandleFunc("POST /api/collection/candidates/{id}/analysis", s.collectionAnalyze)
	mux.HandleFunc("GET /api/collection/analysis/{id}", s.collectionAnalysisJobGet)
	mux.HandleFunc("POST /api/collection/analysis/{id}/cancel", s.collectionAnalysisJobCancel)
	mux.HandleFunc("GET /api/collection/settings", s.collectionSettingsGet)
	mux.HandleFunc("PUT /api/collection/settings", s.collectionSettingsPut)
	mux.HandleFunc("POST /api/collection/pause", s.collectionPause)
	mux.HandleFunc("POST /api/collection/resume", s.collectionResume)
	s.registerCollectionImportRoutes(mux)
}

// Collection exposes the manager for service lifecycle wiring in main.
func (s *Server) Collection() *CollectionManager { return s.collection() }
