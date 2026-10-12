package reader

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// collectionFail reports a stable machine-readable error code alongside the
// Chinese message, matching the existing error envelope.
func collectionFail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": message, "code": code})
}

func (s *Server) collection() *CollectionManager {
	return s.collectionManager
}

func (s *Server) collectionTasksList(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.CollectionTasks()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取任务")
		return
	}
	respond(w, 200, tasks)
}

type collectionTaskBody struct {
	Name    *string               `json:"name"`
	Config  *CollectionTaskConfig `json:"config"`
	Enabled *bool                 `json:"enabled"`
	Priority *int                 `json:"priority"`
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
		Search:   q.Get("q"),
		Citation: q.Get("citation"),
		Sort:     q.Get("sort"),
	}
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
	if cursor := q.Get("cursor"); cursor != "" {
		parts := strings.SplitN(cursor, "|", 3)
		if len(parts) != 3 {
			collectionFail(w, 400, "query-invalid", "无效的游标")
			return
		}
		f.CursorDate, f.CursorID = parts[0], parts[1]
		f.CursorNum, _ = strconv.Atoi(parts[2])
	}
	items, scores, next, err := s.Store.CollectionCandidates(f)
	if err != nil {
		collectionFail(w, 500, "query-invalid", "查询失败")
		return
	}
	type row struct {
		Candidate CollectionCandidate `json:"candidate"`
		Score     int                 `json:"score"`
	}
	rows := make([]row, 0, len(items))
	for i, c := range items {
		rows = append(rows, row{Candidate: c, Score: scores[i]})
	}
	respond(w, 200, map[string]any{"items": rows, "nextCursor": next})
}

func (s *Server) collectionCandidateGet(w http.ResponseWriter, r *http.Request) {
	c, cit, err := s.Store.CollectionCandidate(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "query-invalid", "候选不存在")
		return
	}
	respond(w, 200, map[string]any{"candidate": c, "citation": cit})
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
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取采集设置")
		return
	}
	respond(w, 200, cfg)
}

func (s *Server) collectionSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body CollectionSettings
	if !decode(w, r, &body) {
		return
	}
	// Secrets are written through the protected AI configuration owner only;
	// this endpoint never accepts key material.
	body.CitationsAPIKey = ""
	if err := s.Store.SaveCollectionSettings(body); err != nil {
		collectionFail(w, 500, "query-invalid", "设置保存失败")
		return
	}
	cfg, err := s.Store.CollectionSettings()
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
	cfg, _ := s.Store.CollectionSettings()
	cfg.Paused = true
	_ = s.Store.SaveCollectionSettings(cfg)
	respond(w, 200, map[string]bool{"paused": true})
}

func (s *Server) collectionResume(w http.ResponseWriter, r *http.Request) {
	if err := s.collection().SetPaused(false); err != nil {
		collectionFail(w, 500, "query-invalid", "恢复失败")
		return
	}
	cfg, _ := s.Store.CollectionSettings()
	cfg.Paused = false
	_ = s.Store.SaveCollectionSettings(cfg)
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
	mux.HandleFunc("GET /api/collection/settings", s.collectionSettingsGet)
	mux.HandleFunc("PUT /api/collection/settings", s.collectionSettingsPut)
	mux.HandleFunc("POST /api/collection/pause", s.collectionPause)
	mux.HandleFunc("POST /api/collection/resume", s.collectionResume)
	s.registerCollectionImportRoutes(mux)
}
