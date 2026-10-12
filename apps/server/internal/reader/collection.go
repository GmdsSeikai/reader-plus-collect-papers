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

// CollectionManager owns discovery runs, import downloads, their cancellation
// handles and the shared source gates. The Go service holds one instance;
// pages never time runs — they only submit commands and read persisted state.
// Every unit of background work is registered here before it can publish
// results, so drain can close admission, cancel and wait for real cleanup.
type CollectionManager struct {
	server  *Server
	gate    *scholar.SourceGate
	s2Gate  *scholar.SourceGate
	mu      sync.Mutex
	running map[string]context.CancelFunc // runID/import requestID → cancel
	// workWG counts registered background units; Add happens before the
	// goroutine starts and before results can publish, so Wait cannot race.
	workWG   sync.WaitGroup
	paused   bool
	stopped  bool
	draining bool
	queue    chan collectionJob
	queueWG  sync.WaitGroup
	tickerWG sync.WaitGroup
	baseCtx  context.Context
}

type collectionJob struct {
	server *Server
	req    CollectionImportRequest
	commit bool
}

// NewCollectionManager builds the manager with the shared gates.
func NewCollectionManager(s *Server) *CollectionManager {
	return &CollectionManager{
		server:  s,
		gate:    scholar.NewSourceGate("arxiv", 3*time.Second),
		s2Gate:  scholar.NewSourceGate("s2", time.Second),
		running: map[string]context.CancelFunc{},
		queue:   make(chan collectionJob, 64),
	}
}

// Draining reports whether new collection commands are refused because the
// service is draining toward exit.
func (m *CollectionManager) Draining() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draining || m.stopped
}

// StartCollection launches the manager under the service context. The
// returned stop closes admission, cancels owned work and waits for every
// registered unit before the store may close. It is safe to start again after
// a cancelled drain, on a fresh context.
func (m *CollectionManager) StartCollection(ctx context.Context) func() {
	m.mu.Lock()
	m.stopped, m.draining = false, false
	m.baseCtx = ctx
	m.mu.Unlock()
	m.queueWG.Add(1)
	go m.importWorker(ctx)
	m.tickerWG.Add(1)
	go m.scheduleTick(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		m.stopOwnedWork()
	}()
	return func() {
		m.mu.Lock()
		m.draining = true
		m.mu.Unlock()
		<-done
		m.queueWG.Wait()
		m.tickerWG.Wait()
	}
}

// stopOwnedWork refuses new work, cancels registered handles and waits for
// every unit to finish its cleanup.
func (m *CollectionManager) stopOwnedWork() {
	m.mu.Lock()
	m.stopped = true
	for id, cancel := range m.running {
		cancel()
		delete(m.running, id)
	}
	m.mu.Unlock()
	m.workWG.Wait()
}

// registerWork publishes a cancellable handle before the work starts. ok=false
// when the manager is stopped or draining.
func (m *CollectionManager) registerWork(id string) (context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.draining {
		return nil, false
	}
	if _, exists := m.running[id]; exists {
		return nil, false
	}
	parent := m.baseCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.running[id] = cancel
	m.workWG.Add(1)
	return ctx, true
}

func (m *CollectionManager) releaseWork(id string) {
	m.mu.Lock()
	if cancel, ok := m.running[id]; ok {
		cancel()
		delete(m.running, id)
	}
	m.mu.Unlock()
	m.workWG.Done()
}

// cancelWork closes the transport of one owned unit (an import request).
func (m *CollectionManager) cancelWork(id string) {
	m.mu.Lock()
	cancel := m.running[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// queueImport enqueues one download on the serial import queue.
func (m *CollectionManager) queueImport(s *Server, req CollectionImportRequest) {
	select {
	case m.queue <- collectionJob{server: s, req: req}:
	default:
		_, _ = s.Store.claimImport(req.RequestID, importActive, map[string]any{"status": importFailed, "error": "导入队列已满，请稍后重试"})
	}
}

// queueCommit enqueues one confirmed commit.
func (m *CollectionManager) queueCommit(s *Server, req CollectionImportRequest) {
	select {
	case m.queue <- collectionJob{server: s, req: req, commit: true}:
	default:
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": "导入队列已满，请稍后重试"})
	}
}

// importWorker runs one download or commit at a time. Jobs are owned: each
// registers before publishing anything and releases after cleanup.
func (m *CollectionManager) importWorker(ctx context.Context) {
	defer m.queueWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.queue:
			ctxWork, ok := m.registerWork(job.req.RequestID)
			if !ok {
				_, _ = job.server.Store.claimImport(job.req.RequestID, importActive, map[string]any{"status": importInterrupted, "error": "服务退出，导入中断"})
				continue
			}
			if job.commit {
				job.server.commitCollectionImport(job.req)
			} else {
				job.server.runCollectionImport(ctxWork, job.req)
			}
			m.releaseWork(job.req.RequestID)
		}
	}
}

// scheduleTick starts due automatic runs and refreshes due citations. The
// scheduler is owned by the manager and stops with it.
func (m *CollectionManager) scheduleTick(ctx context.Context) {
	defer m.tickerWG.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.startDueTasks(ctx)
			if _, err := m.RefreshCitations(ctx, m.citationKey()); err != nil {
				log.Printf("citation refresh: %v", err)
			}
		}
	}
}

func (m *CollectionManager) citationKey() string {
	cfg, err := m.server.Store.CollectionSettings()
	if err != nil {
		return ""
	}
	return cfg.CitationsAPIKey
}

// startDueTasks launches automatic runs for enabled tasks whose next run is
// due. Manual runs never change this schedule.
func (m *CollectionManager) startDueTasks(ctx context.Context) {
	m.mu.Lock()
	paused, stopped := m.paused, m.stopped
	m.mu.Unlock()
	if paused || stopped {
		return
	}
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		return
	}
	nowStr := now()
	for _, t := range tasks {
		if !t.Enabled || (t.NextRunAt != "" && t.NextRunAt > nowStr) {
			continue
		}
		if _, err := m.EnqueueRun(t.ID, "auto"); err != nil {
			continue
		}
		next := time.Now().UTC().Add(time.Duration(t.Config.FrequencyHours) * time.Hour).Format(time.RFC3339)
		_, _ = m.server.Store.DB.Exec("UPDATE collection_tasks SET next_run_at=? WHERE task_id=?", next, t.ID)
	}
}

// EnqueueRun starts a manual or automatic run for the task. Repeated clicks
// share the open run instead of starting duplicate work. A closed task may
// still run manually; automatic runs only pick enabled tasks.
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
	// Register the handle before the goroutine can publish anything.
	ctxWork, ok := m.registerWork(run.ID)
	if !ok {
		_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "stopped")
		return run, errors.New("采集已停止")
	}
	go func() {
		defer m.releaseWork(run.ID)
		m.executeRun(ctxWork, run, task, latest, hot)
	}()
	return run, nil
}

// CancelRun bumps the task's exec token and cancels owned work. Committed
// pages stay; late responses cannot advance checkpoints.
func (m *CollectionManager) CancelRun(runID string) error {
	var taskID string
	if err := m.server.Store.DB.QueryRow("SELECT task_id FROM collection_runs WHERE run_id=?", runID).Scan(&taskID); err != nil {
		return err
	}
	if err := m.server.Store.BumpCollectionExecToken(taskID); err != nil {
		return err
	}
	m.cancelWork(runID)
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
			_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cancelled")
			return
		}
		if err := m.scanSlice(ctx, run, task, plan); err != nil {
			if ctx.Err() != nil {
				_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cancelled")
				return
			}
			failed++
			log.Printf("collection slice failed task=%s branch=%s: %v", task.ID, plan.Branch.Name, err)
		}
	}
	if ctx.Err() != nil {
		_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cancelled")
		return
	}
	if failed == 0 {
		_ = m.server.Store.FinishCollectionRun(run.ID, runSucceeded, "")
	} else {
		_ = m.server.Store.FinishCollectionRun(run.ID, runPartial, "slice-failed")
	}
}

const discoveryPageSize = 100

// scanSlice pages one branch slice. The exec token is captured when the
// request is sent and re-checked inside the commit transaction, so a
// cancellation between response and commit rejects the page.
func (m *CollectionManager) scanSlice(ctx context.Context, run CollectionRun, task CollectionTask, plan SlicePlan) error {
	from := plan.From.UTC().Format(time.RFC3339)
	to := plan.To.UTC().Format(time.RFC3339)
	sl, err := m.server.Store.FindSlice(task.ID, task.QueryRevision, plan.Branch.Channel, plan.Branch.BranchHash, from, to)
	if errors.Is(err, sql.ErrNoRows) {
		sl = CollectionSlice{
			TaskID:       task.ID,
			Channel:      plan.Branch.Channel,
			BranchHash:   plan.Branch.BranchHash,
			FromUTC:      from,
			ToUTC:        to,
			Query:        plan.Branch.Query,
			TotalResults: -1,
			Status:       sliceRunning,
		}
	} else if err != nil {
		return err
	}
	if sl.Status == sliceCompleted && sl.PageOffset > 0 && sl.TotalResults >= 0 && sl.Processed >= sl.TotalResults {
		return nil
	}
	sl.Status = sliceRunning
	client := m.server.scholarClient()
	var sendToken int64
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		sendToken, err = m.server.Store.CollectionRunToken(run.ID)
		if err != nil {
			return err
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
			return m.finishSlice(sl, run, task, sendToken)
		}
		if result.TotalResults < 0 && len(result.Entries) == 0 {
			// An empty page without a declared total is an anomaly, not
			// completion: coverage is not proven.
			return errors.New("来源返回异常空页")
		}
		nowStr := now()
		candidates, matches := discoveryPage(task, plan.Branch, result, nowStr)
		tx, err := m.server.Store.DB.Begin()
		if err != nil {
			return err
		}
		sl.TotalResults = result.TotalResults
		if err := m.server.Store.CommitScanPage(tx, sl, task.QueryRevision, sendToken, run.ID, candidates, matches); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		sl.PageOffset++
		sl.Processed += len(candidates)
		if len(result.Entries) < discoveryPageSize || (sl.Processed >= result.TotalResults && result.TotalResults >= 0) {
			break
		}
	}
	return m.finishSlice(sl, run, task, sendToken)
}

// finishSlice records the coverage watermark under the same identity gate as
// page commits: a cancelled run cannot claim coverage from a late response.
func (m *CollectionManager) finishSlice(sl CollectionSlice, run CollectionRun, task CollectionTask, sendToken int64) error {
	sl.Status = sliceCompleted
	sl.CheckedAt = now()
	tx, err := m.server.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var token int64
	var status string
	if err := tx.QueryRow("SELECT exec_token,status FROM collection_runs WHERE run_id=?", run.ID).Scan(&token, &status); err != nil {
		return err
	}
	if token != sendToken || (status != runRunning && status != runQueued) {
		return errors.New("运行已被取消或替换，响应不再有效")
	}
	if err := m.server.Store.UpsertSlice(tx, sl, run.ID, task.QueryRevision); err != nil {
		return err
	}
	return tx.Commit()
}
