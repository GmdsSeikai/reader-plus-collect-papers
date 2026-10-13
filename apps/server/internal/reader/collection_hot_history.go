package reader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// The hot-history review cycle reopens old hot-window slices every 7 days to
// re-check the source (data corrections, later indexing). State lives in
// collection_cursors under this owner so the pending queue and progress
// survive restarts; the current-bucket rescan of normal runs stays separate.
const hotHistoryOwner = "hot-history-cycle"

// hotHistoryRoundBudget is the maximum number of historical slices one round
// reopens, per the design contract.
const hotHistoryRoundBudget = 10

// hotHistoryRecentZone keeps the last 7 days out of the history queue: normal
// runs already rescan that zone for delayed indexing.
const hotHistoryRecentZone = 7 * 24 * time.Hour

// hotHistorySliceItem is one pending day-range of one task's hot branch.
type hotHistorySliceItem struct {
	TaskID   string `json:"taskId"`
	From     string `json:"from"`
	To       string `json:"to"`
	Attempts int    `json:"attempts"`
}

// hotHistoryCycle is the persisted cycle document. Rev is a CAS counter;
// every mutation must write with the Rev it read and increment it.
type hotHistoryCycle struct {
	Rev         int64                  `json:"rev"`
	Phase       string                 `json:"phase"` // idle | running
	NextStartAt string                 `json:"nextStartAt"`
	WindowFrom  string                 `json:"windowFrom"`
	WindowTo    string                 `json:"windowTo"`
	TaskRev     map[string]string       `json:"taskRev"`
	Pending     []hotHistorySliceItem  `json:"pending"`
	Error       string                 `json:"error,omitempty"`
}

func loadHotHistoryCycle(s *Store) (hotHistoryCycle, string, error) {
	cycle := hotHistoryCycle{Phase: "idle", TaskRev: map[string]string{}}
	var body, updatedAt string
	err := s.DB.QueryRow("SELECT body,updated_at FROM collection_cursors WHERE owner_key=?", hotHistoryOwner).Scan(&body, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cycle, "", nil
	}
	if err != nil {
		return cycle, "", err
	}
	if err := json.Unmarshal([]byte(body), &cycle); err != nil {
		// A corrupt document restarts the cycle rather than stalling forever.
		return hotHistoryCycle{Phase: "idle", TaskRev: map[string]string{}}, updatedAt, nil
	}
	if cycle.TaskRev == nil {
		cycle.TaskRev = map[string]string{}
	}
	return cycle, updatedAt, nil
}

// saveHotHistoryCycle writes with CAS on the row's updated_at and returns the
// exact stamp it wrote so the caller can chain further CAS writes. ok=false
// means a concurrent writer won and the caller should reload and retry.
func saveHotHistoryCycle(s *Store, cycle hotHistoryCycle, prevUpdatedAt string) (string, bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return "", false, err
	}
	cycle.Rev++
	body, err := json.Marshal(cycle)
	if err != nil {
		return "", false, err
	}
	stamp := now()
	if prevUpdatedAt == "" {
		res, err := tx.Exec("INSERT INTO collection_cursors(owner_key,body,updated_at) VALUES(?,?,?)", hotHistoryOwner, string(body), stamp)
		if err != nil {
			return "", false, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return "", false, nil
		}
		return stamp, true, tx.Commit()
	}
	res, err := tx.Exec("UPDATE collection_cursors SET body=?,updated_at=? WHERE owner_key=? AND updated_at=?", string(body), stamp, hotHistoryOwner, prevUpdatedAt)
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", false, nil
	}
	return stamp, true, tx.Commit()
}

// runHotHistoryCycle consumes the persistent cycle: due cycles build their
// pending queue; running cycles reopen at most one round budget of slices
// under a short-lived run. Cycles never hold a run open across rounds, and a
// failure keeps the remaining queue for the next tick or restart.
func (m *CollectionManager) runHotHistoryCycle(ctx context.Context) {
	cycle, stamp, err := loadHotHistoryCycle(m.server.Store)
	if err != nil {
		log.Printf("hot history: %v", err)
		return
	}
	if cycle.Phase == "idle" {
		if cycle.NextStartAt != "" {
			next, err := time.Parse(time.RFC3339, cycle.NextStartAt)
			if err == nil && time.Now().Before(next) {
				return
			}
		}
		if err := m.buildHotHistoryQueue(ctx, &cycle, &stamp); err != nil {
			log.Printf("hot history queue: %v", err)
			return
		}
		if len(cycle.Pending) == 0 {
			return
		}
	}
	if err := m.runHotHistoryRound(ctx, &cycle, &stamp); err != nil {
		log.Printf("hot history round: %v", err)
	}
}

// buildHotHistoryQueue snapshots the historical day-ranges of every enabled
// task's hot window, excluding the recent rescan zone.
func (m *CollectionManager) buildHotHistoryQueue(ctx context.Context, cycle *hotHistoryCycle, stamp *string) error {
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		return err
	}
	pending := []hotHistorySliceItem{}
	taskRev := map[string]string{}
	var windowFrom, windowTo string
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		_, hot := PlanWindows(time.Now(), t.Config.LatestWindowDays, t.Config.HotWindowMonths)
		if windowFrom == "" || hot.From.Before(mustParseRFC3339(windowFrom)) {
			windowFrom = hot.From.Format(time.RFC3339)
		}
		if windowTo == "" || hot.To.After(mustParseRFC3339(windowTo)) {
			windowTo = hot.To.Format(time.RFC3339)
		}
		taskRev[t.ID] = taskWindowIdentity(t)
		recent := time.Now().Add(-hotHistoryRecentZone)
		for _, bucket := range splitSliceRange(hot.From, hot.To) {
			if bucket.To.After(recent) {
				// The recent zone is covered by normal-run rescans.
				continue
			}
			pending = append(pending, hotHistorySliceItem{
				TaskID: t.ID,
				From:   bucket.From.UTC().Format(time.RFC3339),
				To:     bucket.To.UTC().Format(time.RFC3339),
			})
		}
	}
	cycle.Phase = "running"
	cycle.TaskRev = taskRev
	cycle.Pending = pending
	cycle.WindowFrom = windowFrom
	cycle.WindowTo = windowTo
	cycle.Error = ""
	newStamp, ok, err := saveHotHistoryCycle(m.server.Store, *cycle, *stamp)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("游标并发更新")
	}
	*stamp = newStamp
	return nil
}

// runHotHistoryRound reopens up to the round budget of pending slices under
// one short-lived run per touched task.
func (m *CollectionManager) runHotHistoryRound(ctx context.Context, cycle *hotHistoryCycle, stamp *string) error {
	// A revision or window change invalidates the snapshot: rebuild the queue
	// from the live tasks so stale relationships never count as coverage.
	for _, item := range cycle.Pending {
		task, err := m.server.Store.CollectionTask(item.TaskID)
		if err != nil || taskWindowIdentity(task) != cycle.TaskRev[item.TaskID] {
			cycle.Phase = "idle"
			cycle.Pending = nil
			if newStamp, ok, err := saveHotHistoryCycle(m.server.Store, *cycle, *stamp); err == nil && ok {
				*stamp = newStamp
			}
			return m.buildHotHistoryQueue(ctx, cycle, stamp)
		}
	}
	budget := hotHistoryRoundBudget
	for budget > 0 && len(cycle.Pending) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		item := cycle.Pending[0]
		task, err := m.server.Store.CollectionTask(item.TaskID)
		if err != nil {
			// The task vanished: drop its items.
			cycle.Pending = cycle.Pending[1:]
			if err := m.persistCycle(cycle, stamp); err != nil {
				return err
			}
			continue
		}
		// One short-lived run per task per round; the open-run unique index
		// arbitrates with concurrent manual/auto runs.
		_, hot := PlanWindows(time.Now(), task.Config.LatestWindowDays, task.Config.HotWindowMonths)
		run, err := m.server.Store.CreateCollectionRun(task, "hot-history", snapshotRanges(hot, hot))
		if err != nil {
			// Task busy this tick; the queue keeps its place.
			return nil
		}
		scanned := 0
		for budget > 0 && len(cycle.Pending) > 0 && cycle.Pending[0].TaskID == task.ID {
			if ctx.Err() != nil {
				_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cancelled")
				return ctx.Err()
			}
			item := cycle.Pending[0]
			scanErr := m.scanHotHistorySlice(ctx, run, task, item)
			cycle.Pending = cycle.Pending[1:]
			if scanErr != nil {
				// Rotate the failed slice to the tail so one bad range cannot
				// burn every future round's budget at the queue head.
				item.Attempts++
				cycle.Pending = append(cycle.Pending, item)
				cycle.Error = scanErr.Error()
			}
			budget--
			scanned++
			if err := m.persistCycle(cycle, stamp); err != nil {
				_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cursor-write-failed")
				return err
			}
		}
		finish := runSucceeded
		if cycle.Error != "" {
			finish = runPartial
		}
		_ = m.server.Store.FinishCollectionRun(run.ID, finish, "")
		_ = scanned
		if len(cycle.Pending) == 0 {
			break
		}
	}
	if len(cycle.Pending) == 0 {
		cycle.Phase = "idle"
		cycle.Error = ""
		cycle.NextStartAt = time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339)
		return m.persistCycle(cycle, stamp)
	}
	return nil
}

// scanHotHistorySlice reopens one historical range with ForceRescan so its
// coverage is re-proven against the source.
func (m *CollectionManager) scanHotHistorySlice(ctx context.Context, run CollectionRun, task CollectionTask, item hotHistorySliceItem) error {
	from, err := time.Parse(time.RFC3339, item.From)
	if err != nil {
		return err
	}
	to, err := time.Parse(time.RFC3339, item.To)
	if err != nil {
		return err
	}
	branch := m.hotBranchFor(task)
	if branch.Name == "" {
		return errors.New("任务没有热门分支")
	}
	// A proven source limit is terminal: accept the slice as-is instead of
	// reopening it forever.
	if sl, err := m.server.Store.FindSlice(task.ID, task.QueryRevision, branch.Channel, branch.BranchHash, item.From, item.To); err == nil && sl.SourceLimit {
		return nil
	}
	plan := SlicePlan{Branch: branch, From: from, To: to, ForceRescan: true}
	return m.scanSlice(ctx, run, task, plan)
}

// hotBranchFor re-derives the task's hot-channel topic branch from the live
// config; the stored queue holds only ranges.
func (m *CollectionManager) hotBranchFor(task CollectionTask) CollectionBranch {
	_, hot := PlanWindows(time.Now(), task.Config.LatestWindowDays, task.Config.HotWindowMonths)
	branches := BuildBranches(task.Config, hot, hot)
	for _, b := range branches {
		if b.Channel == channelHot && b.Name == "topic" {
			return b
		}
	}
	// Fall back to the first hot branch (author-only tasks).
	for _, b := range branches {
		if b.Channel == channelHot {
			return b
		}
	}
	return CollectionBranch{}
}

func (m *CollectionManager) persistCycle(cycle *hotHistoryCycle, stamp *string) error {
	newStamp, ok, err := saveHotHistoryCycle(m.server.Store, *cycle, *stamp)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("游标并发更新")
	}
	*stamp = newStamp
	return nil
}

// taskWindowIdentity binds the revision and the window configuration, so
// window edits rebuild the queue even though they do not move the revision.
func taskWindowIdentity(t CollectionTask) string {
	return fmt.Sprintf("%d:%d:%d", t.QueryRevision, t.Config.LatestWindowDays, t.Config.HotWindowMonths)
}

func mustParseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
