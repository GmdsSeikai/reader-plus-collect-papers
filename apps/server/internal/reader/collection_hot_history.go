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

const hotHistoryOwner = "hot-history-cycle"
const hotHistoryFormatVersion = 2
const hotHistoryRoundBudget = 10
const hotHistoryRecentZone = 7 * 24 * time.Hour

type hotHistorySliceItem struct {
	TaskID     string `json:"taskId"`
	BranchHash string `json:"branchHash"`
	From       string `json:"from"`
	To         string `json:"to"`
	Attempts   int    `json:"attempts"`
	Blocked    string `json:"blocked,omitempty"`
}

type hotHistoryCycle struct {
	Version     int                   `json:"version"`
	Rev         int64                 `json:"rev"`
	Phase       string                `json:"phase"`
	NextStartAt string                `json:"nextStartAt"`
	WindowFrom  string                `json:"windowFrom"`
	WindowTo    string                `json:"windowTo"`
	TaskRev     map[string]string     `json:"taskRev"`
	Pending     []hotHistorySliceItem `json:"pending"`
	Error       string                `json:"error,omitempty"`
}

func loadHotHistoryCycle(s *Store) (hotHistoryCycle, string, error) {
	cycle := hotHistoryCycle{Version: hotHistoryFormatVersion, Phase: "idle", TaskRev: map[string]string{}}
	var body, stamp string
	err := s.DB.QueryRow("SELECT body,updated_at FROM collection_cursors WHERE owner_key=?", hotHistoryOwner).Scan(&body, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return cycle, "", nil
	}
	if err != nil {
		return cycle, "", err
	}
	if err := json.Unmarshal([]byte(body), &cycle); err != nil {
		return cycle, stamp, fmt.Errorf("历史复核状态损坏: %w", err)
	}
	// Version 1 did not identify branches. Rebuild all branch/range work,
	// retaining candidates and existing slice evidence.
	var wire struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return cycle, stamp, err
	}
	if wire.Version < hotHistoryFormatVersion {
		return hotHistoryCycle{Version: hotHistoryFormatVersion, Phase: "idle", TaskRev: map[string]string{}}, stamp, nil
	}
	if wire.Version != hotHistoryFormatVersion {
		return cycle, stamp, errors.New("历史复核状态来自更新版本")
	}
	if cycle.Phase != "idle" && cycle.Phase != "running" {
		return cycle, stamp, errors.New("历史复核状态无效")
	}
	if cycle.TaskRev == nil {
		cycle.TaskRev = map[string]string{}
	}
	for _, item := range cycle.Pending {
		if item.TaskID == "" || item.BranchHash == "" || item.From == "" || item.To == "" {
			return cycle, stamp, errors.New("历史复核待办缺少身份或范围")
		}
	}
	return cycle, stamp, nil
}

func saveHotHistoryCycle(s *Store, cycle hotHistoryCycle, prev string) (string, bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	stamp, ok, err := saveHotHistoryCycleTx(tx, cycle, prev)
	if err != nil || !ok {
		return stamp, ok, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return stamp, true, nil
}

func saveHotHistoryCycleTx(tx *sql.Tx, cycle hotHistoryCycle, prev string) (string, bool, error) {
	if err := collectionWritableTx(tx); err != nil {
		return "", false, err
	}
	cycle.Version = hotHistoryFormatVersion
	cycle.Rev++
	body, err := json.Marshal(cycle)
	if err != nil {
		return "", false, err
	}
	stamp := now()
	var res sql.Result
	if prev == "" {
		res, err = tx.Exec("INSERT INTO collection_cursors(owner_key,body,updated_at) VALUES(?,?,?) ON CONFLICT(owner_key) DO NOTHING", hotHistoryOwner, string(body), stamp)
	} else {
		res, err = tx.Exec("UPDATE collection_cursors SET body=?,updated_at=? WHERE owner_key=? AND updated_at=?", string(body), stamp, hotHistoryOwner, prev)
	}
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if n != 1 {
		return "", false, nil
	}
	return stamp, true, nil
}

func (m *CollectionManager) runHotHistoryCycle(ctx context.Context) {
	if ctx.Err() != nil || !m.schedulingAllowed() {
		return
	}
	if err := m.server.Store.CollectionWritable(); err != nil {
		return
	}
	cycle, stamp, err := loadHotHistoryCycle(m.server.Store)
	if err != nil {
		log.Printf("hot history: %v", err)
		return
	}
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		log.Printf("hot history tasks: %v", err)
		return
	}
	changed := !historyTaskSnapshotMatches(tasks, cycle.TaskRev)
	if cycle.Phase == "idle" && !changed && cycle.NextStartAt != "" {
		next, err := time.Parse(time.RFC3339, cycle.NextStartAt)
		if err != nil {
			log.Printf("hot history: invalid next start")
			return
		}
		if m.clockNow().Before(next) {
			return
		}
	}
	if cycle.Phase == "idle" || changed {
		if err := m.buildHotHistoryQueue(ctx, &cycle, &stamp); err != nil {
			log.Printf("hot history queue: %v", err)
			return
		}
	}
	if err := m.runHotHistoryRound(ctx, &cycle, &stamp); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("hot history round: %v", err)
	}
}

func historyTaskSnapshotMatches(tasks []CollectionTask, snapshot map[string]string) bool {
	count := 0
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		count++
		if snapshot[task.ID] != taskWindowIdentity(task) {
			return false
		}
	}
	return count == len(snapshot)
}

// Weekly ranges share a fixed Monday UTC anchor. Partial edges never exceed
// the captured window; changing its edges does not move interior buckets.
func weeklyHistoryRanges(from, to time.Time) []DateWindow {
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil
	}
	anchor := time.Date(1970, 1, 5, 0, 0, 0, 0, time.UTC)
	const step = 7 * 24 * time.Hour
	delta := from.Sub(anchor)
	bucket := delta / step
	if delta < 0 && delta%step != 0 {
		bucket--
	}
	boundary := anchor.Add((bucket + 1) * step)
	ranges := []DateWindow{}
	for boundary.Before(to) {
		ranges = append(ranges, DateWindow{From: from, To: boundary})
		from = boundary
		boundary = boundary.Add(step)
	}
	return append(ranges, DateWindow{From: from, To: to})
}

func (m *CollectionManager) hotBranchesFor(task CollectionTask, fixedNow time.Time) []CollectionBranch {
	latest, hot := PlanWindows(fixedNow, task.Config.LatestWindowDays, task.Config.HotWindowMonths)
	result := []CollectionBranch{}
	for _, branch := range BuildBranches(task.Config, latest, hot) {
		if branch.Channel == channelHot {
			result = append(result, branch)
		}
	}
	return result
}

func (m *CollectionManager) buildHotHistoryQueue(ctx context.Context, cycle *hotHistoryCycle, stamp *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		return err
	}
	fixedNow := m.clockNow()
	pending := []hotHistorySliceItem{}
	snapshot := map[string]string{}
	var windowFrom, windowTo string
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		_, hot := PlanWindows(fixedNow, task.Config.LatestWindowDays, task.Config.HotWindowMonths)
		cutoff := fixedNow.Add(-hotHistoryRecentZone)
		if windowFrom == "" || hot.From.Before(mustParseRFC3339(windowFrom)) {
			windowFrom = hot.From.Format(time.RFC3339)
		}
		windowTo = cutoff.Format(time.RFC3339)
		snapshot[task.ID] = taskWindowIdentity(task)
		for _, interval := range weeklyHistoryRanges(hot.From, cutoff) {
			for _, branch := range m.hotBranchesFor(task, fixedNow) {
				pending = append(pending, hotHistorySliceItem{TaskID: task.ID, BranchHash: branch.BranchHash, From: interval.From.Format(time.RFC3339), To: interval.To.Format(time.RFC3339)})
			}
		}
	}
	cycle.Version, cycle.Phase = hotHistoryFormatVersion, "running"
	cycle.TaskRev, cycle.Pending = snapshot, pending
	cycle.WindowFrom, cycle.WindowTo = windowFrom, windowTo
	cycle.NextStartAt, cycle.Error = "", ""
	return m.persistCycle(cycle, stamp)
}

func (m *CollectionManager) runHotHistoryRound(ctx context.Context, cycle *hotHistoryCycle, stamp *string) error {
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		return err
	}
	if !historyTaskSnapshotMatches(tasks, cycle.TaskRev) {
		if err := m.buildHotHistoryQueue(ctx, cycle, stamp); err != nil {
			return err
		}
	}
	budget, inspected := hotHistoryRoundBudget, len(cycle.Pending)
	for budget > 0 && inspected > 0 && len(cycle.Pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.schedulingAllowed() {
			return errCollectionPaused
		}
		item := cycle.Pending[0]
		inspected--
		if item.Blocked == "source_limit" {
			cycle.Pending = append(cycle.Pending[1:], item)
			continue
		}
		task, err := m.server.Store.CollectionTask(item.TaskID)
		if err != nil {
			return err
		}
		if !task.Enabled || taskWindowIdentity(task) != cycle.TaskRev[task.ID] {
			return m.buildHotHistoryQueue(ctx, cycle, stamp)
		}
		if _, err := m.server.Store.OpenCollectionRun(task.ID); err == nil {
			cycle.Pending = append(cycle.Pending[1:], item)
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		latest, hot := PlanWindows(m.clockNow(), task.Config.LatestWindowDays, task.Config.HotWindowMonths)
		run, err := m.server.Store.CreateCollectionRun(task, "hot-history", snapshotRanges(latest, hot))
		if err != nil {
			if _, busy := m.server.Store.OpenCollectionRun(task.ID); busy == nil {
				continue
			}
			return err
		}
		workCtx, ok := m.registerWork(run.ID)
		if !ok {
			_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "admission-closed")
			return errCollectionPaused
		}
		stopCancellation := context.AfterFunc(ctx, func() { m.cancelWork(run.ID) })
		scanErr := m.scanHotHistorySlice(workCtx, run, task, item)
		cancelled := workCtx.Err() != nil || ctx.Err() != nil || !m.schedulingAllowed()
		stopCancellation()
		if cancelled {
			_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "cancelled")
			m.releaseWork(run.ID)
			return context.Canceled
		}
		cycle.Pending = cycle.Pending[1:]
		finish, failure := runSucceeded, ""
		if scanErr != nil {
			finish, failure = runPartial, "history-slice"
			item.Attempts++
			if errors.Is(scanErr, errCollectionSourceLimit) {
				item.Blocked = "source_limit"
				failure = "source_limit"
			}
			cycle.Pending = append(cycle.Pending, item)
			cycle.Error = scanErr.Error()
		}
		err = m.finishHotHistoryWork(workCtx, run, cycle, stamp, finish, failure)
		if err != nil {
			_ = m.server.Store.FinishCollectionRun(run.ID, runInterrupted, "history-state-changed")
		}
		m.releaseWork(run.ID)
		if err != nil {
			return err
		}
		budget--
	}
	if len(cycle.Pending) == 0 {
		cycle.Phase, cycle.Error = "idle", ""
		cycle.NextStartAt = m.clockNow().Add(7 * 24 * time.Hour).Format(time.RFC3339)
	}
	return m.persistCycle(cycle, stamp)
}

// The terminal run and the persistent queue advance together. A late response
// cannot consume work after cancellation, task edits or a competing cursor write.
func (m *CollectionManager) finishHotHistoryWork(ctx context.Context, run CollectionRun, cycle *hotHistoryCycle, stamp *string, status, failure string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := m.server.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateCollectionRunTx(tx, run.ID, run.ExecToken, run.QueryRevision); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE collection_runs SET status=?,failure_class=?,finished_at=? WHERE run_id=?", status, failure, now(), run.ID); err != nil {
		return err
	}
	next, ok, err := saveHotHistoryCycleTx(tx, *cycle, *stamp)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("历史复核游标并发更新")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	cycle.Rev++
	*stamp = next
	return nil
}

func (m *CollectionManager) scanHotHistorySlice(ctx context.Context, run CollectionRun, task CollectionTask, item hotHistorySliceItem) error {
	from, err := time.Parse(time.RFC3339, item.From)
	if err != nil {
		return err
	}
	to, err := time.Parse(time.RFC3339, item.To)
	if err != nil || !to.After(from) {
		return errors.New("历史复核范围无效")
	}
	for _, branch := range m.hotBranchesFor(task, m.clockNow()) {
		if branch.BranchHash == item.BranchHash {
			return m.scanSlice(ctx, run, task, SlicePlan{Branch: branch, From: from, To: to, ForceRescan: true})
		}
	}
	return errors.New("历史复核分支已改变")
}

func (m *CollectionManager) persistCycle(cycle *hotHistoryCycle, stamp *string) error {
	next, ok, err := saveHotHistoryCycle(m.server.Store, *cycle, *stamp)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("历史复核游标并发更新")
	}
	cycle.Rev++
	*stamp = next
	return nil
}

func taskWindowIdentity(task CollectionTask) string {
	return fmt.Sprintf("%d:%d:%d:%t", task.QueryRevision, task.Config.LatestWindowDays, task.Config.HotWindowMonths, task.Enabled)
}

func mustParseRFC3339(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}
