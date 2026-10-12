package reader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"reader.local/server/internal/scholar"
)

// atomPage builds a search Atom page with the given total and entries.
func atomPage(total int, ids ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom" xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/"><opensearch:totalResults>%d</opensearch:totalResults>`, total)
	for i, id := range ids {
		fmt.Fprintf(&b, `<entry><id>http://arxiv.org/abs/%sv1</id><published>2026-03-01T0%d:00:00Z</published><updated>2026-03-01T0%d:00:00Z</updated><title>Object Detection %s</title><summary>object detection body</summary><author><name>Ada Lovelace</name></author><category term="cs.CV"/></entry>`, id, i%10, i%10, id)
	}
	b.WriteString("</feed>")
	return b.String()
}

func coverageFixture(t *testing.T, s *Server, feed func(w http.ResponseWriter, r *http.Request)) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/query", feed)
	mux.HandleFunc("/pdf/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(sample(t, "reading-notes.pdf"))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	c := scholar.New(maxPaperBytes)
	c.ArXivAPI = srv.URL + "/api/query"
	c.ArXivPDF = srv.URL + "/pdf/"
	c.HTTP = srv.Client()
	c.AllowPrivateHosts = true
	s.scholarMu.Lock()
	s.scholar = c
	s.scholarMu.Unlock()
	s.collectionManager.gate = scholar.NewSourceGate("test-"+t.Name(), time.Millisecond)
}

func newScanFixture(t *testing.T, s *Server) CollectionTask {
	t.Helper()
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups = [][]string{{"object detection"}}
	task, err := s.Store.SaveCollectionTask(CollectionTask{Name: "n", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Store.CollectionTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// TestCancelWhileDrainStopInitializes (T02) cancels while the stop hook has
// not yet returned its stop channel: the intent's conditions are installed
// under the drain lock, so the resume cannot race ahead of them.
func TestCancelWhileDrainStopInitializes(t *testing.T) {
	s := testServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	backgroundDone := make(chan struct{})
	started := make(chan struct{}, 1)
	s.SetDrainHooks(func() <-chan struct{} {
		close(entered)
		<-release
		return backgroundDone
	}, func() {
		started <- struct{}{}
	})
	go func() {
		s.BeginDrain()
	}()
	<-entered
	// Cancel while the stop hook is still initializing the stop condition.
	cancelResult := make(chan struct{})
	go func() {
		s.drain.mu.Lock()
		id := ""
		if s.drain.current != nil {
			id = s.drain.current.ID
		}
		s.drain.mu.Unlock()
		if id != "" {
			s.CancelDrain(id)
		}
		close(cancelResult)
	}()
	// Let the stop hook finish returning its channel; then the resume may
	// proceed only after backgroundDone.
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-cancelResult
	select {
	case <-started:
		t.Fatal("resume started before the stop condition was even installed")
	default:
	}
	// Admission stays closed until the old background actually stops.
	if _, err := s.importGate.Acquire(); err == nil {
		t.Fatal("admission reopened before the old background stopped")
	}
	close(backgroundDone)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("resume never ran after the background stopped")
	}
}

// TestParentRequiresEveryExpectedChild (T04) proves a split parent is not
// covered by a proper subset of its expected children.
func TestParentRequiresEveryExpectedChild(t *testing.T) {
	s := testStore(t)
	task := CollectionTask{ID: "t1", Name: "n", Priority: 2, Enabled: true, QueryRevision: 1, Config: DefaultCollectionTaskConfig()}
	task.Config.KeywordGroups = [][]string{{"x"}}
	if _, err := s.SaveCollectionTask(task, -1); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.CollectionTask("t1")
	from := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	to := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)
	parent := CollectionSlice{
		TaskID: "t1", Channel: channelLatest, BranchHash: "b",
		FromUTC: from.Format(time.RFC3339), ToUTC: to.Format(time.RFC3339),
		Query: "q", TotalResults: 5000, Status: sliceCompleted, FailureClass: "split",
	}
	mgr := &CollectionManager{server: &Server{Store: s}}
	if ok, err := mgr.childrenCovered(saved, parent); err != nil || ok {
		t.Fatalf("empty parent considered covered: %v %v", ok, err)
	}
	// Only the first expected child exists and is completed.
	first := splitSliceRange(from, to)[0]
	tx, _ := s.DB.Begin()
	if err := s.UpsertSlice(tx, CollectionSlice{
		TaskID: "t1", Channel: channelLatest, BranchHash: "b",
		FromUTC: first.From.UTC().Format(time.RFC3339), ToUTC: first.To.UTC().Format(time.RFC3339),
		Query: "q", TotalResults: 1, Status: sliceCompleted,
	}, "run", 1); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if ok, _ := mgr.childrenCovered(saved, parent); ok {
		t.Fatal("1-of-N children considered full coverage")
	}
}

// TestNestedChildrenCanProveCoverage (T04) proves multi-level splits count as
// covered when every leaf is covered, despite intermediate split markers.
func TestNestedChildrenCanProveCoverage(t *testing.T) {
	s := testStore(t)
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups = [][]string{{"x"}}
	task, err := s.SaveCollectionTask(CollectionTask{Name: "n", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := s.CollectionTask(task.ID)
	mgr := &CollectionManager{server: &Server{Store: s}}
	// Parent: one hour; split into minutes as leaves.
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	parent := CollectionSlice{
		TaskID: task.ID, Channel: channelLatest, BranchHash: "b",
		FromUTC: from.Format(time.RFC3339), ToUTC: to.Format(time.RFC3339),
		Query: "q", TotalResults: 5000, Status: sliceRunning, FailureClass: "split",
	}
	tx, _ := s.DB.Begin()
	if err := s.UpsertSlice(tx, parent, "run", 1); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	var parentID string
	s.DB.QueryRow("SELECT slice_id FROM collection_slices WHERE from_utc=? AND to_utc=?", parent.FromUTC, parent.ToUTC).Scan(&parentID)
	minutes := splitSliceRange(from, to)
	for _, min := range minutes {
		tx, _ := s.DB.Begin()
		err := s.UpsertSlice(tx, CollectionSlice{
			TaskID: task.ID, Channel: channelLatest, BranchHash: "b", ParentID: parentID,
			FromUTC: min.From.UTC().Format(time.RFC3339), ToUTC: min.To.UTC().Format(time.RFC3339),
			Query: "q", TotalResults: 1, Status: sliceCompleted,
		}, "run", 1)
		tx.Commit()
		if err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := mgr.childrenCovered(saved, parent); err != nil || !ok {
		t.Fatalf("all leaves covered but parent not proven: %v %v", ok, err)
	}
	// Removing one leaf breaks the proof.
	one := minutes[0]
	s.DB.Exec("DELETE FROM collection_slices WHERE from_utc=? AND to_utc=?", one.From.UTC().Format(time.RFC3339), one.To.UTC().Format(time.RFC3339))
	if ok, _ := mgr.childrenCovered(saved, parent); ok {
		t.Fatal("missing leaf still considered covered")
	}
}

// TestResumeMustFetchFinalPage (T05) resumes after a mid-slice failure and
// must fetch the remaining page instead of trusting accumulated counts.
func TestResumeMustFetchFinalPage(t *testing.T) {
	s := testServer(t)
	task := newScanFixture(t, s)
	var page2Calls int
	var mu sync.Mutex
	failPage2 := true
	ids1 := make([]string, 100)
	for i := range ids1 {
		ids1[i] = fmt.Sprintf("2301.%05d", i+1)
	}
	ids2 := make([]string, 50)
	for i := range ids2 {
		ids2[i] = fmt.Sprintf("2302.%05d", i+1)
	}
	coverageFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		mu.Lock()
		defer mu.Unlock()
		if start == "0" {
			w.Write([]byte(atomPage(150, ids1...)))
			return
		}
		page2Calls++
		if failPage2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(atomPage(150, ids2...)))
	})
	run, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	plan := SlicePlan{Branch: CollectionBranch{Channel: channelLatest, BranchHash: "b", Body: "ti:\"object detection\"", Query: "q"}, From: time.Now().Add(-24 * time.Hour), To: time.Now()}
	mgr := s.Collection()
	if err := mgr.scanSlice(context.Background(), run, task, plan); err == nil {
		t.Fatal("scan must fail when page 2 errors")
	}
	mu.Lock()
	failPage2 = false
	mu.Unlock()
	if err := mgr.scanSlice(context.Background(), run, task, plan); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	var unique int
	s.Store.DB.QueryRow("SELECT count(*) FROM collection_candidates").Scan(&unique)
	if unique != 150 {
		t.Fatalf("resume ended with %d unique candidates, want 150", unique)
	}
	mu.Lock()
	calls := page2Calls
	mu.Unlock()
	if calls < 2 {
		t.Fatalf("final page never re-fetched after resume (page2 calls=%d)", calls)
	}
	// The slice claims completion only with real full coverage.
	var status string
	s.Store.DB.QueryRow("SELECT status FROM collection_slices WHERE branch_hash='b' AND from_utc=?", plan.From.UTC().Format(time.RFC3339)).Scan(&status)
	if status != sliceCompleted {
		t.Fatalf("slice status = %q, want completed", status)
	}
}

// TestPositiveTotalEmptyPageCannotComplete (T06) treats a positive total with
// an empty page as a source anomaly.
func TestPositiveTotalEmptyPageCannotComplete(t *testing.T) {
	s := testServer(t)
	task := newScanFixture(t, s)
	coverageFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		// total 1 but zero entries: a source anomaly, never coverage.
		w.Write([]byte(atomPage(1)))
	})
	run, _ := s.Store.CreateCollectionRun(task, "manual", "{}")
	plan := SlicePlan{Branch: CollectionBranch{Channel: channelLatest, BranchHash: "b", Body: "x", Query: "q"}, From: time.Now().Add(-time.Hour), To: time.Now()}
	err := s.Collection().scanSlice(context.Background(), run, task, plan)
	if err == nil {
		t.Fatal("positive-total empty page completed the slice")
	}
	var status string
	s.Store.DB.QueryRow("SELECT status FROM collection_slices WHERE branch_hash='b'").Scan(&status)
	if status == sliceCompleted {
		t.Fatal("slice marked completed after anomaly")
	}
}

// TestForcedRescanReachesSplitChildren (T07) reopens completed split
// children when the recent window rescans.
func TestForcedRescanReachesSplitChildren(t *testing.T) {
	s := testServer(t)
	task := newScanFixture(t, s)
	var calls int
	var mu sync.Mutex
	big := true
	coverageFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		span, ok := querySpan(r.URL.Query().Get("search_query"))
		if ok && span > 25*time.Hour && big {
			w.Write([]byte(atomPage(5000, "2301.00001")))
			return
		}
		w.Write([]byte(atomPage(1, "2302.00001")))
	})
	mgr := s.Collection()
	run, _ := s.Store.CreateCollectionRun(task, "manual", "{}")
	from := time.Now().Add(-30 * 24 * time.Hour)
	to := time.Now()
	plan := SlicePlan{Branch: CollectionBranch{Channel: channelLatest, BranchHash: "b", Body: "x", Query: "q"}, From: from, To: to}
	if err := mgr.scanSlice(context.Background(), run, task, plan); err != nil {
		t.Fatalf("first split scan: %v", err)
	}
	mu.Lock()
	big = false
	before := calls
	mu.Unlock()
	// A forced rescan of the same parent plan must reach the children.
	plan.ForceRescan = true
	if err := mgr.scanSlice(context.Background(), run, task, plan); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	mu.Lock()
	after := calls
	mu.Unlock()
	if after <= before {
		t.Fatalf("forced rescan made no source requests (before=%d after=%d)", before, after)
	}
}

// TestCursorCannotCrossSinceFilter (T09) rejects a cursor replayed under a
// different explicit since filter but keeps paging under its own snapshot.
func TestCursorCannotCrossSinceFilter(t *testing.T) {
	s := testStore(t)
	f := CandidateFilter{Sort: "date", Channel: channelLatest, Since: "2026-03-01T00:00:00Z"}
	cursor := encodeCursor(f, 0, "2026-03-05T00:00:00Z", "2301.00001")
	// Same snapshot keeps paging.
	key, date, id, since, err := DecodeCursor(cursor, f)
	if err != nil || since != f.Since || id != "2301.00001" {
		t.Fatalf("same-snapshot decode failed: %d %q %q %q %v", key, date, id, since, err)
	}
	// A different explicit since is a different filter identity.
	changed := f
	changed.Since = "2026-03-10T00:00:00Z"
	if _, _, _, _, err := DecodeCursor(cursor, changed); err == nil {
		t.Fatal("cursor crossed a changed since filter")
	}
	// A cursor page with no explicit since keeps the snapshot.
	implicit := f
	implicit.Since = ""
	_, _, _, since, err = DecodeCursor(cursor, implicit)
	if err != nil || since != "2026-03-01T00:00:00Z" {
		t.Fatalf("snapshot lost: %q %v", since, err)
	}
	_ = s
}
