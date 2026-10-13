package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedAnalysisCandidate(t *testing.T, s *Server) CollectionCandidate {
	t.Helper()
	task := hotHistoryFixtureTask(t, s, 1)
	c := CollectionCandidate{ArXivID: "2301.12345", Title: analysisTitle, Abstract: analysisAbstract, Authors: []CollectionAuthorJSON{{Name: "Ada"}}, Categories: []string{"cs.CV"}, PublishedAt: now(), UpdatedAt: now(), LatestVersion: 1, CanonicalVer: 1, FirstSeenAt: now(), LastSeenAt: now()}
	c.ContentHash = CandidateContentHash(c.Title, c.Abstract, []string{"Ada"}, c.Categories)
	c.PublishedAt = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := s.Store.UpsertCandidate(tx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertMatch(tx, CollectionMatch{TaskID: task.ID, QueryRevision: task.QueryRevision, ArXivID: c.ArXivID, Channel: channelLatest, BranchHash: "topic", RuleScore: 20, Evidence: []CollectionEvidence{}, FirstSeenAt: now(), LastSeenAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return c
}

func fixtureAnalysisResult(input AnalysisInput) AnalysisResult {
	r := AnalysisResult{SchemaVersion: 1, TitleZH: "目标检测方法", AbstractZH: "本文研究目标检测和知识蒸馏。", Assessments: []AnalysisAssessment{}}
	for _, task := range input.Tasks {
		if task.WantsScore {
			r.Assessments = append(r.Assessments, AnalysisAssessment{TaskID: task.TaskID, ProfileHash: task.ProfileHash, RelevanceScore: 80, ReasonZH: "符合研究方向。", Evidence: []AnalysisEvidence{{Field: "abstract", Quote: input.Abstract}}})
		}
	}
	return r
}

func analysisAPIFixture(t *testing.T, s *Server, response func(AnalysisInput) string) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			respond(w, 200, map[string]any{"data": []any{map[string]string{"id": "test-model"}}})
			return
		}
		calls.Add(1)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request: %v", err)
			w.WriteHeader(400)
			return
		}
		if req.Model != "test-model" || len(req.Messages) != 2 || req.Messages[0].Content != analysisSystemPrompt {
			t.Error("analysis task did not fix its own model/system/session")
		}
		var input AnalysisInput
		if err := json.Unmarshal([]byte(req.Messages[1].Content), &input); err != nil {
			t.Errorf("input: %v", err)
			w.WriteHeader(400)
			return
		}
		writeAPIReply(w, response(input))
	}))
	t.Cleanup(upstream.Close)
	ai := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "fixture-key"}, Models: map[string]string{}, APIModels: []string{"test-model"}}
	passModels(&ai, map[string]bool{"test-model": false})
	if err := s.writeAIConfig(ai); err != nil {
		t.Fatal(err)
	}
	cfg := defaultCollectionSettings()
	cfg.AnalysisEnabled, cfg.AnalysisProvider, cfg.AnalysisModel = true, "api", "test-model"
	if err := s.Store.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	return calls
}

func TestPaperAnalysisProductionAPIAndCache(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	calls := analysisAPIFixture(t, s, func(input AnalysisInput) string { return mustJSONText(fixtureAnalysisResult(input)) })
	for i := 0; i < 2; i++ {
		job := submitAnalysisHTTP(t, s, candidate.ArXivID)
		if job := waitAnalysisJobHTTP(t, s, job.ID); job.Status != runSucceeded {
			t.Fatalf("analysis HTTP job: %+v", job)
		}
		response := request(t, s, "GET", "/api/collection/candidates/"+candidate.ArXivID, nil)
		var body struct {
			Analyses []CollectionAnalysis `json:"analyses"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Analyses) != 1 || body.Analyses[0].Result == nil || body.Analyses[0].Result.Assessments[0].TaskID == "" {
			t.Fatalf("result not persisted/decoded: %s %v", response.Body.String(), err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cache hit sent another native request: %d", calls.Load())
	}
	var sent, reserved int
	if err := s.Store.DB.QueryRow("SELECT sent,reserved FROM collection_quota_days").Scan(&sent, &reserved); err != nil || sent != 1 || reserved != 0 {
		t.Fatalf("actual dispatch count: %d %d %v", sent, reserved, err)
	}
	var score int
	if err := s.Store.DB.QueryRow("SELECT ai_score FROM collection_matches WHERE arxiv_id=?", candidate.ArXivID).Scan(&score); err != nil || score != 80 {
		t.Fatalf("successful result did not project score: %d %v", score, err)
	}
	var sentAt string
	if err := s.Store.DB.QueryRow("SELECT sent_at FROM collection_attempts").Scan(&sentAt); err != nil || sentAt == "" {
		t.Fatalf("missing native dispatch timestamp: %v", err)
	}
}

func TestPaperAnalysisInvalidJSONCountsWithoutScore(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	calls := analysisAPIFixture(t, s, func(input AnalysisInput) string {
		return strings.Replace(mustJSONText(fixtureAnalysisResult(input)), `"relevance_score":80`, `"relevance_score":null`, 1)
	})
	if _, err := s.Collection().AnalyzeCandidate(context.Background(), candidate.ArXivID, true); err == nil {
		t.Fatal("invalid provider result accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("implicit retry or fallback sent another request")
	}
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_matches WHERE ai_score IS NOT NULL").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid result projected score: %d %v", count, err)
	}
	var sent int
	if err := s.Store.DB.QueryRow("SELECT sent FROM collection_quota_days").Scan(&sent); err != nil || sent != 1 {
		t.Fatalf("invalid sent result not metered: %d %v", sent, err)
	}
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_analyses WHERE status='failed' AND attempt=1 AND retry_at IS NOT NULL").Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure retry state missing: %d %v", count, err)
	}
}

func TestPaperAnalysisChangedIdentityRemovesStaleScores(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	analysisAPIFixture(t, s, func(input AnalysisInput) string { return mustJSONText(fixtureAnalysisResult(input)) })
	if _, err := s.Collection().AnalyzeCandidate(context.Background(), candidate.ArXivID, true); err != nil {
		t.Fatal(err)
	}
	score := func() int {
		t.Helper()
		response := request(t, s, "GET", "/api/collection/candidates?channel=latest", nil)
		if response.Code != 200 {
			t.Fatalf("list: %d %s", response.Code, response.Body.String())
		}
		var body struct {
			Items []struct {
				Score int `json:"score"`
			} `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
			t.Fatalf("list items: %s %v", response.Body.String(), err)
		}
		return body.Items[0].Score
	}
	if got := score(); got != 80 {
		t.Fatalf("valid score not used: %d", got)
	}
	cfg, err := s.Store.CollectionSettings()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AnalysisModel = "new-model"
	if err := s.Store.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if got := score(); got != 20 {
		t.Fatalf("changed model kept stale score: %d", got)
	}
	cfg.AnalysisModel = "test-model"
	if err := s.Store.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if got := score(); got != 80 {
		t.Fatalf("original identity did not reuse score: %d", got)
	}
	candidate.LatestVersion++
	candidate.Title += " revised"
	candidate.ContentHash = CandidateContentHash(candidate.Title, candidate.Abstract, []string{"Ada"}, candidate.Categories)
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertCandidate(tx, candidate); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := score(); got != 20 {
		t.Fatalf("changed content kept stale score: %d", got)
	}
}

func TestPaperAnalysisBackgroundKeepsWeightedTasksMoving(t *testing.T) {
	s := testServer(t)
	template := seedAnalysisCandidate(t, s)
	tasks, err := s.Store.CollectionTasks()
	if err != nil {
		t.Fatal(err)
	}
	high := tasks[0]
	high.Priority = 1
	if high, err = s.Store.SaveCollectionTask(high, high.QueryRevision); err != nil {
		t.Fatal(err)
	}
	low, err := s.Store.SaveCollectionTask(CollectionTask{Name: "low", Enabled: true, Priority: 5, Config: high.Config}, -1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM collection_matches WHERE arxiv_id=?", template.ArXivID); err != nil {
		t.Fatal(err)
	}
	for group, task := range []CollectionTask{high, low} {
		for i := 0; i < 40; i++ {
			candidate := template
			candidate.ArXivID = fmt.Sprintf("2601.%05d", group*100+i+1)
			if err := s.Store.UpsertCandidate(tx, candidate); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.UpsertMatch(tx, CollectionMatch{TaskID: task.ID, QueryRevision: task.QueryRevision, ArXivID: candidate.ArXivID, Channel: channelLatest, BranchHash: "topic", RuleScore: 20, Evidence: []CollectionEvidence{}, FirstSeenAt: now(), LastSeenAt: now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	calls := analysisAPIFixture(t, s, func(input AnalysisInput) string {
		mu.Lock()
		for _, task := range input.Tasks {
			counts[task.TaskID]++
		}
		mu.Unlock()
		return mustJSONText(fixtureAnalysisResult(input))
	})
	for i := 0; i < 6; i++ {
		before := calls.Load()
		s.Collection().runAnalysisRound(context.Background())
		if got := calls.Load() - before; got != analysisBatchBudget {
			t.Fatalf("round sent %d requests, expected bounded batch %d", got, analysisBatchBudget)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[high.ID] != 25 || counts[low.ID] != 5 {
		t.Fatalf("weighted fairness/starvation: high=%d low=%d", counts[high.ID], counts[low.ID])
	}
	var body string
	if err := s.Store.DB.QueryRow("SELECT body FROM collection_cursors WHERE owner_key=?", analysisScheduleOwner).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var state analysisSchedule
	if err := json.Unmarshal([]byte(body), &state); err != nil || len(state.Positions) != 2 {
		t.Fatalf("fairness cursor not persistent: %s %v", body, err)
	}
}

func TestPaperAnalysisReusesIndependentJudgmentsAcrossTaskSets(t *testing.T) {
	s := testServer(t)
	candidate := seedAnalysisCandidate(t, s)
	var mu sync.Mutex
	sizes := []int{}
	calls := analysisAPIFixture(t, s, func(input AnalysisInput) string {
		mu.Lock()
		sizes = append(sizes, len(input.Tasks))
		mu.Unlock()
		return mustJSONText(fixtureAnalysisResult(input))
	})
	if _, err := s.Collection().AnalyzeCandidate(context.Background(), candidate.ArXivID, true); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Store.CollectionTasks()
	if err != nil {
		t.Fatal(err)
	}
	newTask, err := s.Store.SaveCollectionTask(CollectionTask{Name: "second", Enabled: true, Priority: 3, Config: tasks[0].Config}, -1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertMatch(tx, CollectionMatch{TaskID: newTask.ID, QueryRevision: newTask.QueryRevision, ArXivID: candidate.ArXivID, Channel: channelLatest, BranchHash: "topic", RuleScore: 20, Evidence: []CollectionEvidence{}, FirstSeenAt: now(), LastSeenAt: now()}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		results, err := s.Collection().AnalyzeCandidate(context.Background(), candidate.ArXivID, true)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, result := range results {
			for _, judgment := range result.Result.Assessments {
				if seen[judgment.TaskID] {
					t.Fatal("cached judgment repeated")
				}
				seen[judgment.TaskID] = true
			}
		}
		if len(seen) != 2 {
			t.Fatalf("independent cached/new judgments lost: %v", seen)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 2 || len(sizes) != 2 || sizes[0] != 1 || sizes[1] != 1 {
		t.Fatalf("task-set change resent valid judgment: calls=%d sizes=%v", calls.Load(), sizes)
	}
}
