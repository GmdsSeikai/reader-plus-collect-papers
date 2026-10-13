package reader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reader.local/server/internal/scholar"
)

const collectionSearchAtom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/" xmlns:arxiv="http://arxiv.org/schemas/atom">
 <opensearch:totalResults>1</opensearch:totalResults>
 <entry>
  <id>http://arxiv.org/abs/2301.12345v2</id>
  <published>2023-01-02T00:00:00Z</published>
  <updated>2023-02-01T00:00:00Z</updated>
  <title>Object Detection with Knowledge</title>
  <summary>We study object detection and knowledge distillation.</summary>
  <author><name>Ada Lovelace</name></author>
  <category term="cs.CV"/>
 </entry>
</feed>`

// collectionFixture points the scholar client at a local Atom/PDF fixture and
// gives the manager a fast, isolated source gate.
func collectionFixture(t *testing.T, s *Server, feed func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/query", func(w http.ResponseWriter, r *http.Request) {
		feed(w, r)
	})
	mux.HandleFunc("/pdf/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(sample(t, "reading-notes.pdf"))
	})
	mux.HandleFunc("/s2/batch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		respond(w, 200, make([]any, len(body.IDs)))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	c := scholar.New(maxPaperBytes)
	c.ArXivAPI = srv.URL + "/api/query"
	c.ArXivPDF = srv.URL + "/pdf/"
	c.SemanticScholar = srv.URL + "/s2/"
	c.HTTP = srv.Client()
	c.AllowPrivateHosts = true
	s.scholarMu.Lock()
	s.scholar = c
	s.scholarMu.Unlock()
	s.collectionManager.gate = scholar.NewSourceGate("test-"+t.Name(), 10*time.Millisecond)
	return srv
}

func createCollectionTask(t *testing.T, s *Server, body string) CollectionTask {
	t.Helper()
	w := request(t, s, "POST", "/api/collection/tasks", strings.NewReader(body))
	if w.Code != 201 {
		t.Fatalf("create task = %d body=%s", w.Code, w.Body.String())
	}
	var task CollectionTask
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCollectionManualDiscoveryLoop(t *testing.T) {
	s := testServer(t)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("search_query")
		if !strings.Contains(q, "object detection") {
			t.Errorf("query missing keyword: %s", q)
		}
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	if w.Code != 202 {
		t.Fatalf("run = %d body=%s", w.Code, w.Body.String())
	}
	var run CollectionRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	// A second click shares the open run instead of duplicating work.
	w2 := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run2 CollectionRun
	if err := json.Unmarshal(w2.Body.Bytes(), &run2); err != nil {
		t.Fatal(err)
	}
	if run2.ID != run.ID {
		t.Fatalf("duplicate run started: %s vs %s", run2.ID, run.ID)
	}
	waitRunStatus(t, s, run.ID, runSucceeded)
	w = request(t, s, "GET", "/api/collection/candidates", nil)
	var list struct {
		Items []struct {
			Candidate CollectionCandidate `json:"candidate"`
			Score     int                 `json:"score"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("candidates = %d body=%s", len(list.Items), w.Body.String())
	}
	c := list.Items[0].Candidate
	if c.ArXivID != "2301.12345" || c.LatestVersion != 2 || c.ContentHash == "" {
		t.Fatalf("candidate = %+v", c)
	}
	if c.PublishedAt < "2023-01-01" || c.PublishedAt > "2023-01-03" {
		t.Fatalf("published date not preserved: %s", c.PublishedAt)
	}
	if list.Items[0].Score <= 0 {
		t.Fatalf("rule score = %d", list.Items[0].Score)
	}
	// Detail carries the original abstract and unknown citations stay visible.
	w = request(t, s, "GET", "/api/collection/candidates/2301.12345", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "knowledge distillation") {
		t.Fatalf("detail = %d %s", w.Code, w.Body.String())
	}
}

func TestCollectionAuthorBranchIsIndependent(t *testing.T) {
	s := testServer(t)
	var seenQueries []string
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("search_query")
		seenQueries = append(seenQueries, q)
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"作者","config":{"v":1,"keyword_groups":[],"group_operator_or":true,"authors":[{"name":"Ada Lovelace","aliases":["A. Lovelace"]}],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	waitRunStatus(t, s, run.ID, runSucceeded)
	if len(seenQueries) < 2 {
		t.Fatalf("expected author queries on both channels, got %v", seenQueries)
	}
	for _, q := range seenQueries {
		if strings.Contains(q, "ti:") || strings.Contains(q, "cat:") {
			t.Fatalf("author branch leaked topic terms: %s", q)
		}
		if !strings.Contains(q, `au:"Ada Lovelace"`) {
			t.Fatalf("author branch missing au term: %s", q)
		}
	}
}

func TestCollectionTopicOnlyTaskRejected(t *testing.T) {
	s := testServer(t)
	w := request(t, s, "POST", "/api/collection/tasks", strings.NewReader(`{"name":"空","config":{"v":1,"research_topic":"图像生成","keyword_groups":[],"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`))
	if w.Code != 400 {
		t.Fatalf("topic-only task accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestCollectionCandidateImportFlow(t *testing.T) {
	s := testServer(t)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	json.Unmarshal(w.Body.Bytes(), &run)
	waitRunStatus(t, s, run.ID, runSucceeded)
	// Select the pinned version explicitly and import.
	w = request(t, s, "POST", "/api/collection/imports", strings.NewReader(`{"items":[{"arxivId":"2301.12345","version":2,"expectedContentHash":""}]}`))
	if w.Code != 202 {
		t.Fatalf("import = %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Items []struct {
			RequestID string `json:"requestId"`
			Error     string `json:"error,omitempty"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Items) != 1 || created.Items[0].Error != "" {
		t.Fatalf("import items = %+v", created.Items)
	}
	reqID := created.Items[0].RequestID
	// Normal PDFs commit straight through the shared install path; the whole
	// "下载并入库" flow needs no second request.
	status := waitImportStatus(t, s, reqID, importImported, importFailed)
	if status != importImported {
		w = request(t, s, "GET", "/api/collection/imports/"+reqID, nil)
		t.Fatalf("import failed: %s", w.Body.String())
	}
	w = request(t, s, "GET", "/api/collection/imports/"+reqID, nil)
	var req CollectionImportRequest
	json.Unmarshal(w.Body.Bytes(), &req)
	if req.DocumentID == "" {
		t.Fatalf("no document linked: %+v", req)
	}
	// The document enters the existing paper library and reader path.
	docs, _ := s.Store.Documents()
	found := false
	for _, d := range docs {
		if d.ID == req.DocumentID && d.Library == "papers" && d.Type == "pdf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("document not in library: %+v", req)
	}
	// Re-clicking import returns the finished request state without a duplicate.
	w = request(t, s, "POST", "/api/collection/imports", strings.NewReader(`{"items":[{"arxivId":"2301.12345","version":2,"expectedContentHash":""}]}`))
	var again struct {
		Items []struct {
			RequestID string `json:"requestId"`
			Error     string `json:"error,omitempty"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &again)
	if len(again.Items) != 1 || again.Items[0].RequestID == "" {
		t.Fatalf("repeat import = %s", w.Body.String())
	}
}

func waitRunStatus(t *testing.T, s *Server, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := request(t, s, "GET", "/api/collection/runs/"+runID, nil)
		var run CollectionRun
		if json.Unmarshal(w.Body.Bytes(), &run) == nil {
			if run.Status == want {
				return
			}
			if run.Status == runFailed || run.Status == runPartial {
				if want == runSucceeded {
					t.Fatalf("run ended %s failure=%s", run.Status, run.FailureClass)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", runID, want)
}

func waitImportStatus(t *testing.T, s *Server, requestID string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := request(t, s, "GET", "/api/collection/imports/"+requestID, nil)
		var req CollectionImportRequest
		if json.Unmarshal(w.Body.Bytes(), &req) == nil {
			for _, status := range want {
				if req.Status == status {
					return req.Status
				}
			}
			if req.Status == importFailed {
				return req.Status
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("import %s did not reach %v", requestID, want)
	return ""
}
