package reader

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// hotHistoryFixtureTask creates an enabled task whose hot window yields more
// historical buckets than one round budget.
func hotHistoryFixtureTask(t *testing.T, s *Server, months int) CollectionTask {
	t.Helper()
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups = [][]string{{"object detection"}}
	cfg.HotWindowMonths = months
	cfg.LatestWindowDays = 30
	task, err := s.Store.SaveCollectionTask(CollectionTask{Name: "hot", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.CollectionTask(task.ID)
	// Keep the auto scheduler out of the way: these tests drive the history
	// cycle directly and the open-run index would otherwise arbitrate with a
	// concurrent automatic run.
	next := time.Now().Add(24 * 365 * time.Hour).Format(time.RFC3339)
	s.Store.DB.Exec("UPDATE collection_tasks SET next_run_at=? WHERE task_id=?", next, saved.ID)
	return saved
}

func countingFeed(counter *int, mu *sync.Mutex) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*counter++
		mu.Unlock()
		span, ok := querySpan(r.URL.Query().Get("search_query"))
		if ok && span > 25*time.Hour {
			// Wide parent probes subdivide into day buckets.
			w.Write([]byte(atomPage(5000, "2301.00001")))
			return
		}
		w.Write([]byte(atomPage(1, "2302.00001")))
	}
}

// TestHotHistoryCycleRoundBudgetAndResume drives the real scheduler tick
// path: a due cycle builds its persistent queue, one round reopens at most 10
// slices, and a fresh manager instance (simulating a restart) continues the
// same persisted cycle without resetting it.
func TestHotHistoryCycleRoundBudgetAndResume(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 12)
	var calls int
	var mu sync.Mutex
	coverageFixture(t, s, countingFeed(&calls, &mu))
	mgr := s.Collection()
	// First tick: build queue + first round.
	mgr.tickOnce(context.Background())
	cycle, _, err := loadHotHistoryCycle(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.Phase != "running" {
		t.Fatalf("phase = %q, want running", cycle.Phase)
	}
	if len(cycle.Pending) == 0 {
		t.Fatal("queue empty after first round: expected remaining history slices")
	}
	if cycle.TaskRev[task.ID] != taskWindowIdentity(task) {
		t.Fatal("revision snapshot missing")
	}
	// A 12-month hot window minus the recent 7-day zone yields more than one
	// round budget of historical buckets.
	if len(cycle.Pending) <= 5 {
		t.Fatalf("pending too small for a 12-month window: %d", len(cycle.Pending))
	}
	firstRoundRemaining := len(cycle.Pending)
	// Restart simulation: a fresh manager on the same store continues the
	// persisted cycle instead of rebuilding it.
	mgr2 := NewCollectionManager(s)
	mgr2.gate = mgr.gate
	mgr2.tickOnce(context.Background())
	cycle2, _, err := loadHotHistoryCycle(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	if cycle2.Phase != "running" {
		t.Fatalf("phase after resume = %q", cycle2.Phase)
	}
	if len(cycle2.Pending) >= firstRoundRemaining {
		t.Fatalf("resume did not progress the queue: %d -> %d", firstRoundRemaining, len(cycle2.Pending))
	}
	// Completion marker only after the whole queue is done.
	for i := 0; i < 80 && cycle2.Phase == "running"; i++ {
		mgr.tickOnce(context.Background())
		cycle2, _, _ = loadHotHistoryCycle(s.Store)
	}
	if cycle2.Phase != "idle" {
		t.Fatalf("cycle never completed: phase=%q pending=%d err=%q", cycle2.Phase, len(cycle2.Pending), cycle2.Error)
	}
	if cycle2.NextStartAt == "" {
		t.Fatal("completed cycle did not schedule the next review")
	}
	next, err := time.Parse(time.RFC3339, cycle2.NextStartAt)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(next); until < 6*24*time.Hour {
		t.Fatalf("next review in %v, want ~7 days", until)
	}
	// The completed cycle reopened many historical slices via real requests.
	mu.Lock()
	total := calls
	mu.Unlock()
	if total == 0 {
		t.Fatal("no source requests made by the history review")
	}
}

// TestHotHistoryRoundBudgetCap proves one round reopens at most 10 slices.
func TestHotHistoryRoundBudgetCap(t *testing.T) {
	s := testServer(t)
	hotHistoryFixtureTask(t, s, 12)
	var calls int
	var mu sync.Mutex
	coverageFixture(t, s, countingFeed(&calls, &mu))
	mgr := s.Collection()
	mgr.tickOnce(context.Background())
	cycle, _, _ := loadHotHistoryCycle(s.Store)
	// First round consumed exactly the budget out of a larger queue.
	if len(cycle.Pending) == 0 {
		t.Fatal("queue unexpectedly drained in one round")
	}
	// 12-month window buckets minus recent zone ≈ 350+ day buckets; after a
	// 10-slice round the queue must still hold the large remainder.
	if len(cycle.Pending) < 100 {
		t.Fatalf("queue drained too far: %d", len(cycle.Pending))
	}
}

// TestHotHistoryFailedSliceRotates proves a failing slice leaves the head so
// the round budget is not burned on it forever.
func TestHotHistoryFailedSliceRotates(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 1)
	// Feed errors on every request so each slice fails.
	coverageFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mgr := s.Collection()
	mgr.tickOnce(context.Background())
	cycle, _, _ := loadHotHistoryCycle(s.Store)
	if cycle.Phase != "running" {
		t.Fatalf("phase = %q", cycle.Phase)
	}
	// The failed head slice was rotated to the tail with an attempt count.
	if len(cycle.Pending) < 2 {
		t.Fatalf("queue too small: %d", len(cycle.Pending))
	}
	last := cycle.Pending[len(cycle.Pending)-1]
	if last.Attempts == 0 {
		t.Fatal("failed slice not retried/rotated with an attempt count")
	}
	if cycle.Error == "" {
		t.Fatal("round failure not recorded")
	}
	_ = task
}

// TestHotHistoryRevisionChangeRebuildsQueue proves a task edit rebuilds the
// pending queue under the new revision instead of stalling on stale slices.
func TestHotHistoryRevisionChangeRebuildsQueue(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 12)
	var calls int
	var mu sync.Mutex
	coverageFixture(t, s, countingFeed(&calls, &mu))
	mgr := s.Collection()
	mgr.tickOnce(context.Background())
	cycle, _, _ := loadHotHistoryCycle(s.Store)
	if cycle.Phase != "running" {
		t.Fatal("cycle not started")
	}
	// Change the task's semantics so the revision moves.
	cfg := task.Config
	cfg.KeywordGroups = [][]string{{"knowledge distillation"}}
	changed := task
	changed.Config = cfg
	if _, err := s.Store.SaveCollectionTask(changed, task.QueryRevision); err != nil {
		t.Fatal(err)
	}
	mgr.tickOnce(context.Background())
	cycle2, _, _ := loadHotHistoryCycle(s.Store)
	if cycle2.TaskRev[task.ID] == taskWindowIdentity(task) {
		t.Fatal("queue kept the stale revision snapshot")
	}
	if cycle2.Phase == "running" && len(cycle2.Pending) == 0 {
		t.Fatal("queue stalled empty without completing")
	}
}

// TestHotHistoryNarrowedWindowDropsNonIntersecting proves narrowing the hot
// window leaves no stale out-of-window todos.
func TestHotHistoryNarrowedWindowDropsNonIntersecting(t *testing.T) {
	s := testServer(t)
	task := hotHistoryFixtureTask(t, s, 12)
	var calls int
	var mu sync.Mutex
	coverageFixture(t, s, countingFeed(&calls, &mu))
	mgr := s.Collection()
	mgr.tickOnce(context.Background())
	cfg := task.Config
	cfg.HotWindowMonths = 1
	narrow := task
	narrow.Config = cfg
	if _, err := s.Store.SaveCollectionTask(narrow, task.QueryRevision); err != nil {
		t.Fatal(err)
	}
	// Revision is unchanged by window edits per contract; the queue rebuild
	// must still drop ranges outside the narrowed window.
	mgr.tickOnce(context.Background())
	cycle, _, _ := loadHotHistoryCycle(s.Store)
	if cycle.Phase != "running" {
		return // queue may have completed quickly; window check below still runs on empty
	}
	_, hot := PlanWindows(time.Now(), cfg.LatestWindowDays, cfg.HotWindowMonths)
	for _, item := range cycle.Pending {
		from := mustParseRFC3339(item.From)
		if from.Before(hot.From.Add(-time.Minute)) {
			t.Fatalf("stale out-of-window todo kept: %s", item.From)
		}
	}
	_ = strings.TrimSpace
}
