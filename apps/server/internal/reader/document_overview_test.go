package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func overviewTestJob(t *testing.T, s *Server) overviewJob {
	t.Helper()
	job := overviewJob{DocumentID: "doc", ID: id(), Status: "running"}
	if _, err := s.Store.DB.Exec("INSERT INTO document_overview_jobs VALUES(?,?,?,'') ON CONFLICT(document_id) DO UPDATE SET job_id=excluded.job_id,status=excluded.status", job.DocumentID, job.ID, job.Status); err != nil {
		t.Fatal(err)
	}
	return job
}
func overviewTestService(t *testing.T, calls *int, failAt *int) *GenerationService {
	t.Helper()
	adapter := adapterFunc(func(ctx context.Context, r GenerateRequest, emit func(ProviderEvent) error) (GenerateResult, error) {
		*calls++
		if *calls == *failAt {
			return GenerateResult{}, errors.New("upstream interrupted")
		}
		raw := strings.SplitN(r.Input.Prompt, "资料：\n", 2)[1]
		var passages []documentPassage
		_ = json.Unmarshal([]byte(raw), &passages)
		block := ""
		if len(passages) > 0 {
			block = passages[0].ID
		}
		if block == "" {
			var notes []overviewContent
			_ = json.Unmarshal([]byte(raw), &notes)
			block = notes[0].Findings[0].BlockIDs[0]
		}
		text := jsonText(overviewContent{Summary: "研究 cobalt 协议及其适用条件。", Findings: []overviewPoint{{Text: "协议用于串行交易", BlockIDs: []string{block}}}, Terms: []string{"cobalt", "COBALT-19"}, Limitations: []overviewPoint{}})
		if err := emit(ProviderEvent{Text: text}); err != nil {
			return GenerateResult{}, err
		}
		return GenerateResult{Text: text, FinishReason: "stop"}, nil
	})
	return &GenerationService{primary: "codex", connections: map[string]generationConnection{"codex": {Adapter: adapter, VerifiedText: true, Incremental: true}}}
}
func TestOverviewGeneratePersistAndReuse(t *testing.T) {
	s, a := originalDocumentFixture(t)
	job := overviewTestJob(t, s)
	calls, failAt := 0, -1
	g := overviewTestService(t, &calls, &failAt)
	if err := s.compileOverview(t.Context(), job, g); err != nil {
		t.Fatal(err)
	}
	o, err := s.currentOverview(t.Context(), a.corpus)
	if err != nil || o == nil || o.Status != "complete" || o.CompiledPassages != len(a.corpus.Passages) || calls != 1 {
		t.Fatal(o, err, calls)
	}
	if err = s.compileOverview(t.Context(), job, g); err != nil || calls != 1 {
		t.Fatal("cached overview regenerated", err, calls)
	}
	store, err := OpenStore(s.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB.Close()
	saved, err := NewServer(store, "test-secret", "").currentOverview(t.Context(), a.corpus)
	if err != nil || saved == nil || saved.Content.Summary != o.Content.Summary {
		t.Fatal("overview lost after restart", err)
	}
	var indexed int
	if err = store.DB.QueryRow("SELECT count(*) FROM document_overviews_fts WHERE document_id='doc'").Scan(&indexed); err != nil || indexed != 1 {
		t.Fatal("overview index missing", err, indexed)
	}
	_, _ = store.DB.Exec("DELETE FROM documents WHERE id='doc'")
	_ = store.DB.QueryRow("SELECT count(*) FROM document_overviews_fts WHERE document_id='doc'").Scan(&indexed)
	if indexed != 0 {
		t.Fatal("orphan overview index")
	}
}
func TestOverviewResumeChaptersAndSourceBoundaries(t *testing.T) {
	s, a := originalDocumentFixture(t)
	_, _ = s.Store.DB.Exec(`INSERT INTO settings VALUES('reader','{"documentContextTokens":8192}')`)
	c := a.corpus
	c.Passages = nil
	for i := 0; i < 6; i++ {
		c.Passages = append(c.Passages, documentPassage{ID: fmt.Sprint(i), Section: fmt.Sprint(i), Label: fmt.Sprintf("Chapter %d", i), Text: strings.Repeat("original evidence ", 350), Location: json.RawMessage(`{"type":"epub","href":"a.xhtml"}`)})
	}
	c.Revision = "large-corpus"
	_, _ = s.Store.DB.Exec("UPDATE document_corpora SET revision=?,body=? WHERE document_id='doc'", c.Revision, jsonText(c))
	job := overviewTestJob(t, s)
	calls, failAt := 0, 2
	g := overviewTestService(t, &calls, &failAt)
	if err := s.compileOverview(t.Context(), job, g); err == nil {
		t.Fatal("failed request accepted")
	}
	partial, err := s.Store.loadOverview(t.Context(), "doc")
	if err != nil || len(partial.Sections) != 1 || partial.Content != nil || partial.Status != "partial" {
		t.Fatal("checkpoint missing", partial, err)
	}
	failAt = -1
	if err = s.compileOverview(t.Context(), job, g); err != nil {
		t.Fatal(err)
	}
	complete, _ := s.Store.loadOverview(t.Context(), "doc")
	if complete.Status != "complete" || complete.CompiledPassages != 6 || calls != 8 {
		t.Fatal("retry did not reuse first chapter", complete.Status, complete.CompiledPassages, calls)
	}
	c.Revision = "changed"
	if o, e := s.currentOverview(t.Context(), c); e != nil || o != nil {
		t.Fatal("stale overview reused", e)
	}
	_, _ = s.Store.DB.Exec("UPDATE document_overview_jobs SET status='canceled' WHERE document_id='doc'")
	if err = s.Store.saveOverview(t.Context(), job, complete); errorKind(err) != ErrorCanceled {
		t.Fatal("canceled job published", err)
	}
}
func TestOverviewValidationAndOptIn(t *testing.T) {
	s, a := originalDocumentFixture(t)
	for _, raw := range []string{`{"summary":"ok","findings":[{"text":"fabricated","blockIds":["unknown"]}]}`, `{"summary":""}`, `{"summary":"ok","limitations":[{"text":"x","blockIds":[]}]}`} {
		if _, err := parseOverviewContent(raw, map[string]bool{a.corpus.Passages[0].ID: true}); err == nil {
			t.Fatal("invalid overview accepted", raw)
		}
	}
	for _, settings := range []string{"", `{}`, `{"autoGenerateOverview":false}`} {
		if err := queueImportedOverview(s.Store.DB, "doc", settings); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM document_overview_jobs").Scan(&n)
	if n != 0 {
		t.Fatal("historical/default document triggered paid job")
	}
	if err := queueImportedOverview(s.Store.DB, "doc", `{"autoGenerateOverview":true,"autoTranslatePDF":false}`); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.DB.QueryRow("SELECT count(*) FROM document_overview_jobs").Scan(&n)
	if n != 1 {
		t.Fatal("overview incorrectly depends on translation")
	}
}

func TestOverviewHTTPJobsAndSettings(t *testing.T) {
	s, _ := originalDocumentFixture(t)
	w := request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":false}`))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var first string
	_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&first)
	w = request(t, s, "POST", "/api/documents/doc/overview", strings.NewReader(`{"regenerate":true}`))
	var second string
	_ = s.Store.DB.QueryRow("SELECT job_id FROM document_overview_jobs WHERE document_id='doc'").Scan(&second)
	if w.Code != 200 || first != second {
		t.Fatal("duplicate request replaced queued job")
	}
	w = request(t, s, "DELETE", "/api/documents/doc/overview/job", nil)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	o, err := s.overviewState(t.Context(), "doc")
	if err != nil || o.Status != "canceled" {
		t.Fatal(o, err)
	}
	for _, body := range []string{`{"documentContextTokens":1}`, `{"documentContextTokens":8192.5}`, `{"autoGenerateOverview":"true"}`} {
		if w = request(t, s, "PUT", "/api/settings", strings.NewReader(body)); w.Code != 400 {
			t.Fatal("invalid settings accepted", w.Code)
		}
	}
}
