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
	// gen is the wait group of the current StartCollection generation. Each
	// start (including a resumed drain) gets a fresh group so one generation's
	// stop never waits on the next generation's work.
	gen              *collectionGeneration
	paused           bool
	stopped          bool
	draining         bool
	queue            chan collectionJob
	baseCtx          context.Context
	clock            func() time.Time
	analysisSerial   chan struct{}
	analysisSchedule sync.Mutex
}

type collectionJob struct {
	server *Server
	req    CollectionImportRequest
	commit bool
}

// NewCollectionManager builds the manager with the shared gates.
func NewCollectionManager(s *Server) *CollectionManager {
	return &CollectionManager{
		server:         s,
		gate:           scholar.NewSourceGate("arxiv", 3*time.Second),
		s2Gate:         scholar.NewSourceGate("s2", time.Second),
		running:        map[string]context.CancelFunc{},
		queue:          make(chan collectionJob, 64),
		analysisSerial: make(chan struct{}, 1),
	}
}

// Draining reports whether new collection commands are refused because the
// service is draining toward exit.
func (m *CollectionManager) Draining() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draining || m.stopped
}

// collectionGeneration owns one StartCollection lifetime. Each start —
// including a resume after a cancelled drain — gets fresh wait groups so one
// generation's stop never waits on the next generation's workers.
type collectionGeneration struct {
	work, queue, ticker sync.WaitGroup
	cancel              context.CancelFunc
}

// StartCollection launches the manager under the service context. The
// returned stop closes admission, cancels owned work and waits for every
// registered unit of this generation before the store may close. It is safe
// to start again after a cancelled drain, on a fresh context.
func (m *CollectionManager) StartCollection(ctx context.Context) func() {
	gen := &collectionGeneration{}
	genCtx, cancel := context.WithCancel(ctx)
	gen.cancel = cancel
	m.mu.Lock()
	m.stopped, m.draining = false, false
	m.baseCtx = genCtx
	m.gen = gen
	m.mu.Unlock()
	gen.queue.Add(1)
	go m.importWorker(genCtx, gen)
	gen.ticker.Add(1)
	go m.scheduleTick(genCtx, gen)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-genCtx.Done()
		m.stopOwnedWork(gen)
	}()
	return func() {
		m.mu.Lock()
		m.draining = true
		m.mu.Unlock()
		cancel()
		<-done
		gen.queue.Wait()
		gen.ticker.Wait()
	}
}

// stopOwnedWork refuses new work, cancels registered handles and waits for
// every unit of this generation to finish its cleanup. Handles stay in the
// map so each releaseWork still pairs its Done; entries are removed only by
// their owners.
func (m *CollectionManager) stopOwnedWork(gen *collectionGeneration) {
	m.mu.Lock()
	m.stopped = true
	for _, cancel := range m.running {
		cancel()
	}
	m.mu.Unlock()
	gen.work.Wait()
}

// registerWork publishes a cancellable handle before the work starts. ok=false
// when the manager is stopped or draining.
func (m *CollectionManager) registerWork(id string) (context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.draining || m.paused {
		return nil, false
	}
	if _, exists := m.running[id]; exists {
		return nil, false
	}
	parent := m.baseCtx
	gen := m.gen
	if parent == nil || gen == nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	m.running[id] = cancel
	gen.work.Add(1)
	return ctx, true
}

func (m *CollectionManager) releaseWork(id string) {
	m.mu.Lock()
	cancel, ok := m.running[id]
	if ok {
		cancel()
		delete(m.running, id)
	}
	gen := m.gen
	m.mu.Unlock()
	if ok && gen != nil {
		gen.work.Done()
	}
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
func (m *CollectionManager) importWorker(ctx context.Context, gen *collectionGeneration) {
	defer gen.queue.Done()
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

// scheduleTick drives the periodic scheduler; tickOnce holds the per-tick
// production sequence so tests can drive the same path deterministically.
func (m *CollectionManager) tickOnce(ctx context.Context) {
	if !m.schedulingAllowed() || ctx.Err() != nil {
		return
	}
	m.startDueTasks(ctx)
	m.runHotHistoryCycle(ctx)
	m.runAnalysisRound(ctx)
	if _, err := m.RefreshCitations(ctx, m.citationKey()); err != nil {
		log.Printf("citation refresh: %v", err)
	}
}

func (m *CollectionManager) schedulingAllowed() bool {
	m.mu.Lock()
	allowed := !m.paused && !m.stopped && !m.draining
	m.mu.Unlock()
	if !allowed {
		return false
	}
	cfg, err := m.server.Store.CollectionSettings()
	return err == nil && !cfg.Paused
}

func (m *CollectionManager) clockNow() time.Time {
	m.mu.Lock()
	clock := m.clock
	m.mu.Unlock()
	if clock == nil {
		return time.Now().UTC().Truncate(time.Second)
	}
	return clock().UTC().Truncate(time.Second)
}

// scheduleTick starts due automatic runs and refreshes due citations. The
// scheduler is owned by the manager and stops with it.
func (m *CollectionManager) scheduleTick(ctx context.Context, gen *collectionGeneration) {
	defer gen.ticker.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.tickOnce(ctx)
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

// CancelRun invalidates this run and cancels owned work. Committed
// pages stay; late responses cannot advance checkpoints.
func (m *CollectionManager) CancelRun(runID string) error {
	tx, err := m.server.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = collectionWritableTx(tx); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE collection_runs SET exec_token=exec_token+1,status=?,finished_at=? WHERE run_id=? AND status IN ('queued','running')", runCancelled, now(), runID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.cancelWork(runID)
	return nil
}

// SetPaused stops new work and cancels active runs; committed pages remain.
func (m *CollectionManager) SetPaused(paused bool) error {
	m.mu.Lock()
	m.paused = paused
	m.mu.Unlock()
	if paused {
		tx, err := m.server.Store.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = collectionWritableTx(tx); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE collection_runs SET exec_token=exec_token+1,status=?,failure_class='paused',finished_at=? WHERE status IN ('queued','running')", runInterrupted, now()); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
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
	// The recent window rescans every run to absorb delayed arXiv indexing.
	rescanFrom := time.Now().UTC().AddDate(0, 0, -7)
	for i := range slices {
		if slices[i].To.After(rescanFrom) {
			slices[i].ForceRescan = true
		}
	}
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

// splitThreshold is the per-slice source total above which a slice subdivides
// into finer time buckets instead of paging a huge result set.
const splitThreshold = 1000

// sliceBudget caps one slice's processing per run as a defensive execution
// budget; hitting it leaves the slice recoverable and never drops candidates.
const sliceBudget = 5000

// scanSlice pages one branch slice. The exec token is captured when the
// request is sent and re-checked inside the commit transaction, so a
// cancellation between response and commit rejects the page. Slices whose
// total exceeds the threshold subdivide into finer buckets; a parent only
// completes when every expected child is covered. Coverage is proven by this
// scan's own fetched-entry progress, never by accumulated commit counts. A
// changed source total reopens the slice; a minute-bucket still over the
// threshold is marked source_limit; an empty or premature page against a
// positive total is an anomaly, not coverage.
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
			Query:        plan.Branch.QueryFor(plan.From, plan.To),
			ParentID:     plan.ParentID,
			TotalResults: -1,
			Status:       sliceRunning,
		}
	} else if err != nil {
		return err
	}
	if sl.SourceLimit {
		// A proven source limit is terminal for coverage; never claim success.
		return errCollectionSourceLimit
	}
	if sl.FailureClass == "split" {
		// A split parent is proven only through its expected children. A
		// forced rescan pushes into the children even when covered.
		if !plan.ForceRescan {
			done, err := m.childrenCovered(task, sl)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
		return m.subdivideSlice(ctx, run, task, plan, sl)
	}
	if sl.Status == sliceCompleted && !plan.ForceRescan {
		return nil
	}
	if plan.ForceRescan && sl.Status == sliceCompleted {
		sl.Status = sliceRunning
	}
	client := m.server.scholarClient()
	var sendToken int64
	scanFetched := 0 // entries fetched in THIS scan; the coverage proof
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if scanFetched >= sliceBudget {
			// Defensive per-round budget: keep the slice recoverable, never
			// claim coverage and never truncate candidates.
			return errors.New("切片执行预算耗尽，保留待办")
		}
		var err error
		sendToken = run.ExecToken
		release, err := m.gate.Acquire(ctx)
		if err != nil {
			return err
		}
		result, err := client.ArxivSearch(ctx, sl.Query, page*discoveryPageSize, discoveryPageSize)
		release()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if result.TotalResults == 0 && len(result.Entries) == 0 {
			// The source explicitly reports nothing in this window.
			return m.finishSlice(sl, run, task, sendToken)
		}
		if len(result.Entries) == 0 {
			// A page with no entries against any non-zero total is a source
			// anomaly (or a filtered error entry): coverage is not proven.
			return errors.New("来源返回异常空页")
		}
		// Total drift reopens the slice: committed rows stay, this scan's
		// progress resets and paging restarts; stable IDs deduplicate.
		if page > 0 && sl.TotalResults >= 0 && result.TotalResults != sl.TotalResults {
			sl.PageOffset = 0
			sl.TotalResults = result.TotalResults
			scanFetched = 0
			page = -1
			continue
		}
		if page == 0 && result.TotalResults > splitThreshold {
			if children := splitSliceRange(plan.From, plan.To); len(children) > 0 {
				sl.TotalResults = result.TotalResults
				return m.subdivideSlice(ctx, run, task, plan, sl)
			}
			// A minute bucket still over the limit proves a source limit.
			sl.TotalResults = result.TotalResults
			sl.SourceLimit = true
			sl.Status = sliceCompleted
			sl.FailureClass = "source_limit"
			if err := m.finishSlice(sl, run, task, sendToken); err != nil {
				return err
			}
			return errCollectionSourceLimit
		}
		validCount := len(result.Entries)
		nowStr := now()
		candidates, matches, localRejects := discoveryPage(task, plan.Branch, result, nowStr)
		validCount -= localRejects
		if validCount < 0 {
			validCount = 0
		}
		scanFetched += validCount
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
		// Rejected source entries (bad identity/date/fields) make the page
		// anomalous: valid candidates stay committed but the slice cannot
		// claim coverage and keeps a retryable failure.
		if result.Rejected+localRejects > 0 {
			sl.Status = sliceFailed
			sl.FailureClass = "bad-entry"
			sl.CheckedAt = now()
			_ = m.persistSlice(sl, run, task, sendToken)
			return errors.New("来源返回无效条目，未证明覆盖")
		}
		if len(result.Entries) < discoveryPageSize {
			// A premature short page contradicting the declared remainder is
			// an anomaly, not the end of the result set.
			if sl.TotalResults >= 0 && scanFetched < sl.TotalResults {
				return errors.New("来源返回与总量矛盾的短页")
			}
			break
		}
		if sl.TotalResults >= 0 && scanFetched >= sl.TotalResults {
			break
		}
	}
	return m.finishSlice(sl, run, task, sendToken)
}

// subdivideSlice records the split parent and scans every expected child. The
// parent stays incomplete until all expected children prove coverage; a
// crash resumes by re-deriving the same expected children and continuing.
func (m *CollectionManager) subdivideSlice(ctx context.Context, run CollectionRun, task CollectionTask, plan SlicePlan, parent CollectionSlice) error {
	children := splitSliceRange(plan.From, plan.To)
	if len(children) == 0 {
		return errors.New("来源总量超限且无法继续细分")
	}
	token, err := m.server.Store.CollectionRunToken(run.ID)
	if err != nil {
		return err
	}
	// The parent keeps the split marker and is NOT completed here.
	parent.FailureClass = "split"
	parent.Status = sliceRunning
	if err := m.persistSlice(parent, run, task, token); err != nil {
		return err
	}
	// Re-read the persisted row so children carry a real parent identity.
	if stored, err := m.server.Store.FindSlice(parent.TaskID, task.QueryRevision, parent.Channel, parent.BranchHash, parent.FromUTC, parent.ToUTC); err == nil {
		parent = stored
		parent.FailureClass = "split"
		parent.Status = sliceRunning
	}
	failed := 0
	sourceLimited := false
	for _, child := range children {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		childPlan := SlicePlan{
			Branch:      plan.Branch,
			From:        child.From,
			To:          child.To,
			ParentID:    parent.ID,
			ForceRescan: plan.ForceRescan,
		}
		if err := m.scanSlice(ctx, run, task, childPlan); err != nil {
			failed++
			if errors.Is(err, errCollectionSourceLimit) {
				sourceLimited = true
			}
		}
	}
	done, err := m.childrenCovered(task, parent)
	if err != nil {
		return err
	}
	if !done || failed > 0 {
		if sourceLimited {
			return errCollectionSourceLimit
		}
		return errors.New("子切片未完全覆盖")
	}
	// Every expected child covered: the parent may now claim completion.
	parent.Status = sliceCompleted
	parent.CheckedAt = now()
	token, err = m.server.Store.CollectionRunToken(run.ID)
	if err != nil {
		return err
	}
	return m.persistSlice(parent, run, task, token)
}

// childrenCovered proves coverage of a split parent through its expected
// children: every bucket derived from the parent range must exist as a row
// and be recursively covered. Completed split children need their own
// children covered; source limits and failures never count as coverage.
func (m *CollectionManager) childrenCovered(task CollectionTask, parent CollectionSlice) (bool, error) {
	from, err := time.Parse(time.RFC3339, parent.FromUTC)
	if err != nil {
		return false, err
	}
	to, err := time.Parse(time.RFC3339, parent.ToUTC)
	if err != nil {
		return false, err
	}
	expected := splitSliceRange(from, to)
	if len(expected) == 0 {
		return false, nil
	}
	for _, child := range expected {
		childFrom := child.From.UTC().Format(time.RFC3339)
		childTo := child.To.UTC().Format(time.RFC3339)
		sl, err := m.server.Store.FindSlice(parent.TaskID, task.QueryRevision, parent.Channel, parent.BranchHash, childFrom, childTo)
		if err != nil {
			// A missing expected child is uncovered, not an error.
			return false, nil
		}
		switch {
		case sl.SourceLimit:
			return false, nil
		case sl.FailureClass == "split":
			done, err := m.childrenCovered(task, sl)
			if err != nil || !done {
				return false, err
			}
		case sl.Status == sliceCompleted && sl.FailureClass == "":
			// covered leaf
		default:
			return false, nil
		}
	}
	return true, nil
}

// persistSlice writes a slice row under the same run-identity gate as page
// commits, without changing the claimed completion state.
func (m *CollectionManager) persistSlice(sl CollectionSlice, run CollectionRun, task CollectionTask, sendToken int64) error {
	tx, err := m.server.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCollectionRunTx(tx, run.ID, sendToken, task.QueryRevision); err != nil {
		return err
	}
	if err := m.server.Store.UpsertSlice(tx, sl, run.ID, task.QueryRevision); err != nil {
		return err
	}
	return tx.Commit()
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
	if err := validateCollectionRunTx(tx, run.ID, sendToken, task.QueryRevision); err != nil {
		return err
	}
	if err := m.server.Store.UpsertSlice(tx, sl, run.ID, task.QueryRevision); err != nil {
		return err
	}
	return tx.Commit()
}
