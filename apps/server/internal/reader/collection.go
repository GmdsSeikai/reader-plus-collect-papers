package reader

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"reader.local/server/internal/scholar"
)

// CollectionManager owns discovery runs, their cancellation tokens and the
// shared source gate. The Go service holds one instance; pages never time
// runs — they only submit commands and read persisted state.
type CollectionManager struct {
	server   *Server
	gate     *scholar.SourceGate
	mu       sync.Mutex
	running  map[string]context.CancelFunc // runID → cancel
	inflight sync.WaitGroup
	paused   bool
	stopped  bool
}

// NewCollectionManager builds the manager with the shared arXiv gate.
func NewCollectionManager(s *Server) *CollectionManager {
	return &CollectionManager{
		server:  s,
		gate:    scholar.NewSourceGate("arxiv", 3*time.Second),
		running: map[string]context.CancelFunc{},
	}
}

// StartCollection launches the manager under the service context. The
// returned stop waits for owned runs to unwind before the store may close.
func (m *CollectionManager) StartCollection(ctx context.Context) func() {
	m.mu.Lock()
	m.stopped = false
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		m.mu.Lock()
		m.stopped = true
		for runID, cancel := range m.running {
			cancel()
			delete(m.running, runID)
		}
		m.mu.Unlock()
		m.inflight.Wait()
	}()
	return func() {
		<-done
	}
}

// EnqueueRun starts a manual run for the task. Repeated clicks share the open
// run instead of starting duplicate work. A closed task may still run
// manually; automatic runs only pick enabled tasks.
func (m *CollectionManager) EnqueueRun(taskID, trigger string) (CollectionRun, error) {
	m.mu.Lock()
	paused, stopped := m.paused, m.stopped
	m.mu.Unlock()
	if stopped {
		return CollectionRun{}, errors.New("采集已停止")
	}
	if paused {
		return CollectionRun{}, errCollectionPaused
	}
	if existing, err := m.server.Store.OpenCollectionRun(taskID); err == nil {
		return existing, nil
	}
	task, err := m.server.Store.CollectionTask(taskID)
	if err != nil {
		return CollectionRun{}, err
	}
	if trigger == "auto" && !task.Enabled {
		return CollectionRun{}, errors.New("任务未启用")
	}
	latest, hot := PlanWindows(time.Now(), task.Config.LatestWindowDays, task.Config.HotWindowMonths)
	run, err := m.server.Store.CreateCollectionRun(task, trigger, snapshotRanges(latest, hot))
	if err != nil {
		// A concurrent first click may have won the open-run slot.
		if existing, e := m.server.Store.OpenCollectionRun(taskID); e == nil {
			return existing, nil
		}
		return run, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.running[run.ID] = cancel
	m.mu.Unlock()
	m.inflight.Add(1)
	go func() {
		defer m.inflight.Done()
		defer cancel()
		m.executeRun(ctx, run, task, latest, hot)
		m.mu.Lock()
		delete(m.running, run.ID)
		m.mu.Unlock()
	}()
	return run, nil
}

// CancelRun bumps the task's exec token and cancels owned work. Committed
// pages stay; late responses cannot advance checkpoints.
func (m *CollectionManager) CancelRun(runID string) error {
	m.mu.Lock()
	cancel := m.running[runID]
	m.mu.Unlock()
	var taskID string
	if err := m.server.Store.DB.QueryRow("SELECT task_id FROM collection_runs WHERE run_id=?", runID).Scan(&taskID); err != nil {
		return err
	}
	if err := m.server.Store.BumpCollectionExecToken(taskID); err != nil {
		return err
	}
	if cancel != nil {
		cancel()
	}
	return m.server.Store.FinishCollectionRun(runID, runCancelled, "")
}

// SetPaused stops new work and cancels active runs; committed pages remain.
func (m *CollectionManager) SetPaused(paused bool) error {
	m.mu.Lock()
	m.paused = paused
	cancels := make([]context.CancelFunc, 0, len(m.running))
	for _, cancel := range m.running {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()
	if paused {
		for _, cancel := range cancels {
			cancel()
		}
	}
	return nil
}

// executeRun scans every slice of the run. Each page commits atomically; a
// failed page keeps prior pages and leaves the run partial, never silently
// claiming coverage.
func (m *CollectionManager) executeRun(ctx context.Context, run CollectionRun, task CollectionTask, latest, hot DateWindow) {
	branches := BuildBranches(task.Config, latest, hot)
	slices := BuildSlices(branches, latest, hot)
	failed := 0
	for _, plan := range slices {
		if ctx.Err() != nil {
			return
		}
		if err := m.scanSlice(ctx, run, task, plan); err != nil {
			if ctx.Err() != nil {
				return
			}
			failed++
			log.Printf("collection slice failed task=%s branch=%s: %v", task.ID, plan.Branch.Name, err)
		}
	}
	if ctx.Err() != nil {
		return
	}
	if failed == 0 {
		_ = m.server.Store.FinishCollectionRun(run.ID, runSucceeded, "")
	} else {
		_ = m.server.Store.FinishCollectionRun(run.ID, runPartial, "slice-failed")
	}
}

const discoveryPageSize = 100

// scanSlice pages one branch slice from its persisted checkpoint. The page
// offset may restart at 0 for coverage while the checkpoint records progress;
// stable IDs deduplicate already committed entries.
func (m *CollectionManager) scanSlice(ctx context.Context, run CollectionRun, task CollectionTask, plan SlicePlan) error {
	from := plan.From.UTC().Format(time.RFC3339)
	to := plan.To.UTC().Format(time.RFC3339)
	sl, err := m.server.Store.FindSlice(task.ID, task.QueryRevision, plan.Branch.Channel, plan.Branch.BranchHash, from, to)
	if errors.Is(err, sql.ErrNoRows) {
		sl = CollectionSlice{
			TaskID:     task.ID,
			Channel:    plan.Branch.Channel,
			BranchHash: plan.Branch.BranchHash,
			FromUTC:    from,
			ToUTC:      to,
			Query:      plan.Branch.Query,
			TotalResults: -1,
			Status:     sliceRunning,
		}
	} else if err != nil {
		return err
	}
	if sl.Status == sliceCompleted && sl.PageOffset > 0 && sl.TotalResults >= 0 && sl.Processed >= sl.TotalResults {
		return nil
	}
	sl.Status = sliceRunning
	client := m.server.scholarClient()
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		release, err := m.gate.Acquire(ctx)
		if err != nil {
			return err
		}
		result, err := client.ArxivSearch(ctx, plan.Branch.Query, page*discoveryPageSize, discoveryPageSize)
		release()
		if err != nil {
			return err
		}
		if result.TotalResults == 0 && len(result.Entries) == 0 {
			// The source explicitly reports nothing in this window.
			sl.Status = sliceCompleted
			sl.CheckedAt = now()
			return m.persistSlice(sl, run, task)
		}
		if result.TotalResults < 0 && len(result.Entries) == 0 {
			// An empty page without a declared total is an anomaly, not
			// completion: coverage is not proven.
			return errors.New("来源返回异常空页")
		}
		nowStr := now()
		candidates, matches := discoveryPage(task, plan.Branch, result, nowStr)
		token, tokenErr := m.server.Store.CollectionRunToken(run.ID)
		if tokenErr != nil {
			return tokenErr
		}
		tx, err := m.server.Store.DB.Begin()
		if err != nil {
			return err
		}
		sl.TotalResults = result.TotalResults
		if err := m.server.Store.CommitScanPage(tx, sl, task.QueryRevision, token, run.ID, candidates, matches); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		sl.PageOffset++
		sl.Processed += len(candidates)
		if len(result.Entries) < discoveryPageSize || sl.Processed >= result.TotalResults && result.TotalResults >= 0 {
			break
		}
	}
	sl.Status = sliceCompleted
	sl.CheckedAt = now()
	return m.persistSlice(sl, run, task)
}

func (m *CollectionManager) persistSlice(sl CollectionSlice, run CollectionRun, task CollectionTask) error {
	tx, err := m.server.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.server.Store.UpsertSlice(tx, sl, run.ID, task.QueryRevision); err != nil {
		return err
	}
	return tx.Commit()
}

// errNoSlice marks a missing slice row for readability at call sites.
var errNoSlice = errors.New("切片不存在")
