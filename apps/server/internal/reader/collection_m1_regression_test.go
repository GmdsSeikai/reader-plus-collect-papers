package reader

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func m1RegressionFixture(t *testing.T, s *Server, feed func(http.ResponseWriter, *http.Request)) {
	srv := collectionFixture(t, s, feed)
	// Keep tickOnce's citation path on the local TLS fixture, never a public service.
	s.scholarMu.Lock()
	s.scholar.SemanticScholar = srv.URL + "/s2/"
	s.scholarMu.Unlock()
}

func TestCollectionM1AllBranchesAreRechecked(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	task.Config.Authors = []CollectionTaskAuthor{{Name: "Ada Lovelace"}, {Name: "Grace Hopper"}}
	if _, err := s.Store.SaveCollectionTask(task, task.QueryRevision); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	queries := []string{}
	m1RegressionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("search_query"))
		mu.Unlock()
		w.Write([]byte(atomPage(1, "2301.00001")))
	})
	s.Collection().runHotHistoryCycle(context.Background())
	for i := 0; i < 100; i++ {
		cycle, _, err := loadHotHistoryCycle(s.Store)
		if err != nil {
			t.Fatal(err)
		}
		if cycle.Phase == "idle" {
			break
		}
		s.Collection().runHotHistoryCycle(context.Background())
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(queries, "\n")
	for _, author := range []string{"Ada Lovelace", "Grace Hopper"} {
		if !strings.Contains(joined, "au:") || !strings.Contains(joined, author) {
			t.Errorf("history never rechecked independent author branch %q: requests=%d", author, len(queries))
		}
	}
}

func TestCollectionM1PauseStopsHistory(t *testing.T) {
	s := testServer(t)
	hotHistoryFixtureTask(t, s, 1)
	var mu sync.Mutex
	calls := 0
	m1RegressionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Write([]byte(atomPage(1, "2301.00001")))
	})
	if err := s.Collection().SetPaused(true); err != nil {
		t.Fatal(err)
	}
	s.Collection().tickOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("paused scheduler still sent %d history source requests", calls)
	}
}

func TestCollectionM1DisabledTaskStopsHistory(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	m := s.Collection()
	cycle := hotHistoryCycle{}
	stamp := ""
	if err := m.buildHotHistoryQueue(context.Background(), &cycle, &stamp); err != nil {
		t.Fatal(err)
	}
	task.Enabled = false
	if _, err := s.Store.SaveCollectionTask(task, task.QueryRevision); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	m1RegressionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Write([]byte(atomPage(1, "2301.00001")))
	})
	m.runHotHistoryCycle(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("disabled task still sent %d historical source requests", calls)
	}
}

func TestCollectionM1SourceLimitCannotFinishCycle(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	m := s.Collection()
	from := time.Now().UTC().Add(-15 * 24 * time.Hour).Truncate(time.Minute)
	to := from.Add(time.Minute)
	item := hotHistorySliceItem{TaskID: task.ID, From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}
	branch := m.hotBranchesFor(task, m.clockNow())[0]
	sl := CollectionSlice{TaskID: task.ID, Channel: channelHot, BranchHash: branch.BranchHash, FromUTC: item.From, ToUTC: item.To, Query: branch.QueryFor(from, to), SourceLimit: true, FailureClass: "source_limit", Status: sliceCompleted, TotalResults: 1500}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.UpsertSlice(tx, sl, "prior", task.QueryRevision); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	item.BranchHash = branch.BranchHash
	cycle := hotHistoryCycle{Version: hotHistoryFormatVersion, Phase: "running", TaskRev: map[string]string{task.ID: taskWindowIdentity(task)}, Pending: []hotHistorySliceItem{item}}
	stamp, ok, err := saveHotHistoryCycle(s.Store, cycle, "")
	if err != nil || !ok {
		t.Fatalf("save cycle %v %v", ok, err)
	}
	if err = m.runHotHistoryRound(context.Background(), &cycle, &stamp); err != nil {
		t.Fatal(err)
	}
	if cycle.Phase == "idle" || len(cycle.Pending) == 0 || cycle.NextStartAt != "" {
		t.Fatalf("source_limit falsely completed the cycle: phase=%s pending=%d next=%s", cycle.Phase, len(cycle.Pending), cycle.NextStartAt)
	}
}

func TestCollectionM1CancelOldRunDoesNotInvalidateNewRun(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	old, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.FinishCollectionRun(old.ID, runSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	current, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Collection().CancelRun(old.ID); err != nil {
		t.Fatal(err)
	}
	token, err := s.Store.CollectionRunToken(current.ID)
	if err != nil || token != current.ExecToken {
		t.Fatalf("old run cancellation changed current token: %d, want %d: %v", token, current.ExecToken, err)
	}
}

func TestCollectionM1PauseInvalidatesPendingResponse(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	run, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Collection().SetPaused(true); err != nil {
		t.Fatal(err)
	}
	sl := CollectionSlice{TaskID: task.ID, Channel: channelHot, BranchHash: "branch", FromUTC: "2026-09-01T00:00:00Z", ToUTC: "2026-09-02T00:00:00Z", Status: sliceRunning}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = s.Store.CommitScanPage(tx, sl, task.QueryRevision, run.ExecToken, run.ID, nil, nil); err == nil {
		t.Fatal("paused run accepted a late page")
	}
}

func TestCollectionM1WindowChangeCannotConsumeHistoryWork(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	m := s.Collection()
	cycle, stamp := hotHistoryCycle{}, ""
	if err := m.buildHotHistoryQueue(context.Background(), &cycle, &stamp); err != nil {
		t.Fatal(err)
	}
	count := len(cycle.Pending)
	run, err := s.Store.CreateCollectionRun(task, "hot-history", "{}")
	if err != nil {
		t.Fatal(err)
	}
	task.Config.HotWindowMonths = 2
	if _, err := s.Store.SaveCollectionTask(task, task.QueryRevision); err != nil {
		t.Fatal(err)
	}
	cycle.Pending = cycle.Pending[1:]
	if err := m.finishHotHistoryWork(context.Background(), run, &cycle, &stamp, runSucceeded, ""); err == nil {
		t.Fatal("changed window consumed old work")
	}
	saved, _, err := loadHotHistoryCycle(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Pending) != count {
		t.Fatal("failed completion advanced cursor")
	}
	open, err := s.Store.OpenCollectionRun(task.ID)
	if err != nil || open.ID != run.ID {
		t.Fatalf("failed completion committed terminal run: %+v %v", open, err)
	}
}

func TestCollectionM1HistoryCursorConflictRollsBackRun(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	m := s.Collection()
	cycle, stamp := hotHistoryCycle{}, ""
	if err := m.buildHotHistoryQueue(context.Background(), &cycle, &stamp); err != nil {
		t.Fatal(err)
	}
	run, err := s.Store.CreateCollectionRun(task, "hot-history", "{}")
	if err != nil {
		t.Fatal(err)
	}
	stamp = "stale-cursor-stamp"
	if err := m.finishHotHistoryWork(context.Background(), run, &cycle, &stamp, runSucceeded, ""); err == nil {
		t.Fatal("conflicting completion succeeded")
	}
	open, err := s.Store.OpenCollectionRun(task.ID)
	if err != nil || open.ID != run.ID {
		t.Fatalf("cursor conflict committed terminal run: %+v %v", open, err)
	}
}

func TestCollectionM1DrainWaitsForStartupRecovery(t *testing.T) {
	s := testServer(t)
	// Hold the serial gate until recovery has registered as an in-flight writer.
	// Drain must wait even though recovery has not acquired the serial mutex yet.
	s.importGate.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(s.importGate.mu.Unlock) }
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- s.RecoverImports() }()
	deadline := time.After(5 * time.Second)
	for {
		s.importGate.stateMu.Lock()
		active := s.importGate.inflight
		s.importGate.stateMu.Unlock()
		if active == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("recovery did not register")
		case <-time.After(time.Millisecond):
		}
	}
	idle := s.importGate.CloseForDrain()
	select {
	case <-idle:
		t.Fatal("drain completed before startup recovery")
	default:
	}
	if release, err := s.importGate.Acquire(); err == nil {
		release()
		t.Fatal("drain admitted a new writer")
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not finish")
	}
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not observe recovery cleanup")
	}
}

func TestCollectionM1DelayedIndexAppearsOnNextRun(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	var indexed atomic.Bool
	published := time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	feed := strings.ReplaceAll(collectionSearchAtom, "2023-01-02T00:00:00Z", published)
	feed = strings.ReplaceAll(feed, "2023-02-01T00:00:00Z", published)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		if indexed.Load() {
			w.Write([]byte(feed))
		} else {
			w.Write([]byte(atomPage(0)))
		}
	})
	run := func() {
		t.Helper()
		response := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader(`{}`))
		if response.Code != http.StatusAccepted {
			t.Fatalf("run admission: %d %s", response.Code, response.Body.String())
		}
		var result CollectionRun
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(8 * time.Second)
		for {
			var status string
			if err := s.Store.DB.QueryRow("SELECT status FROM collection_runs WHERE run_id=?", result.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == runSucceeded {
				return
			}
			if status != runRunning && status != runQueued {
				t.Fatalf("run ended %s", status)
			}
			select {
			case <-deadline:
				t.Fatal("run did not finish")
			case <-time.After(time.Millisecond):
			}
		}
	}
	run()
	var count int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_candidates").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected first-run candidates: %d %v", count, err)
	}
	indexed.Store(true)
	run()
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM collection_candidates WHERE arxiv_id='2301.12345'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("delayed candidate missing or duplicated: %d %v", count, err)
	}
}

func TestCollectionM1UpgradeRebuildsBranchlessQueue(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB.Exec("UPDATE collection_meta SET schema_version=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO collection_cursors(owner_key,body,updated_at) VALUES(?,?,?)", hotHistoryOwner, `{"phase":"running","rev":4,"pending":[{"taskId":"old","from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z"}]}`, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO settings(key,value) VALUES('upgrade-fixture','preserved')"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateCollection(); err != nil {
		t.Fatal(err)
	}
	cycle, _, err := loadHotHistoryCycle(s)
	if err != nil || cycle.Phase != "idle" || len(cycle.Pending) != 0 || cycle.Version != hotHistoryFormatVersion {
		t.Fatalf("unsafe old queue reused: %+v %v", cycle, err)
	}
	var value string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='upgrade-fixture'").Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("upgrade changed reading settings: %q %v", value, err)
	}
	var version int
	if err := s.StoreVersionQuery(&version); err != nil || version != 2 {
		t.Fatalf("version guard did not advance: %d %v", version, err)
	}
}
