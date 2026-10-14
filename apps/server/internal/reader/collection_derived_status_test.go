package reader

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCollectionTasksListDerivesActiveRun (R02) shows the open run on the
// task list while it runs and clears it once the run is terminal.
func TestCollectionTasksListDerivesActiveRun(t *testing.T) {
	s := testServer(t)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	list := func() (map[string]any, bool) {
		w := request(t, s, "GET", "/api/collection/tasks", nil)
		if w.Code != 200 {
			t.Fatalf("tasks list = %d %s", w.Code, w.Body.String())
		}
		var items []struct {
			ID        string         `json:"id"`
			ActiveRun map[string]any `json:"activeRun"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ID == task.ID {
				// The field must be present (null when absent), not omitted.
				if !strings.Contains(w.Body.String(), `"activeRun"`) {
					t.Fatalf("activeRun field missing: %s", w.Body.String())
				}
				return item.ActiveRun, item.ActiveRun != nil
			}
		}
		t.Fatal("task missing from list")
		return nil, false
	}
	if _, open := list(); open {
		t.Fatal("no run yet but activeRun set")
	}
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	if w.Code != 202 {
		t.Fatalf("run create = %d %s", w.Code, w.Body.String())
	}
	var run CollectionRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	active, open := list()
	if !open || active["id"] != run.ID || active["status"] != runRunning {
		t.Fatalf("open run not derived: %v", active)
	}
	waitRunStatus(t, s, run.ID, runSucceeded)
	if _, open := list(); open {
		t.Fatal("finished run still reported active")
	}
}

// TestCollectionCandidateDetailDerivesLatestImport (R02) restores the newest
// import record on the detail response, ordered by created_at then request_id.
func TestCollectionCandidateDetailDerivesLatestImport(t *testing.T) {
	s := testServer(t)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	json.Unmarshal(w.Body.Bytes(), &run)
	waitRunStatus(t, s, run.ID, runSucceeded)

	detail := func() map[string]any {
		w := request(t, s, "GET", "/api/collection/candidates/2301.12345", nil)
		if w.Code != 200 {
			t.Fatalf("detail = %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"latestImport"`) {
			t.Fatalf("latestImport field missing: %s", w.Body.String())
		}
		var body struct {
			LatestImport *CollectionImportRequest `json:"latestImport"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.LatestImport == nil {
			return nil
		}
		out, _ := json.Marshal(body.LatestImport)
		var m map[string]any
		json.Unmarshal(out, &m)
		return m
	}
	if got := detail(); got != nil {
		t.Fatalf("no import yet but latestImport set: %v", got)
	}
	w = request(t, s, "POST", "/api/collection/imports", strings.NewReader(`{"items":[{"arxivId":"2301.12345","version":2,"expectedContentHash":""}]}`))
	var created struct {
		Items []struct {
			RequestID string `json:"requestId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || len(created.Items) != 1 {
		t.Fatalf("import create = %s %v", w.Body.String(), err)
	}
	first := created.Items[0].RequestID
	waitImport(t, s, first, importImported, importAwaitingConfirmation, importDownloaded, importImporting, importQueued, importDownloading)
	got := detail()
	if got == nil || got["requestId"] != first {
		t.Fatalf("latestImport not restored: %v", got)
	}
	// A newer request for the same paper must win regardless of status.
	older := *mustImport(t, s, first)
	older.RequestID = "req-older"
	older.Status = importFailed
	older.CreatedAt = "2020-01-01T00:00:00Z"
	older.UpdatedAt = older.CreatedAt
	if err := s.Store.InsertCollectionImport(older); err != nil {
		t.Fatal(err)
	}
	got = detail()
	if got == nil || got["requestId"] != first {
		t.Fatalf("older request overrode latest: %v", got)
	}
}

func mustImport(t *testing.T, s *Server, requestID string) *CollectionImportRequest {
	t.Helper()
	w := request(t, s, "GET", "/api/collection/imports/"+requestID, nil)
	if w.Code != 200 {
		t.Fatalf("import get = %d %s", w.Code, w.Body.String())
	}
	var req CollectionImportRequest
	if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	return &req
}
