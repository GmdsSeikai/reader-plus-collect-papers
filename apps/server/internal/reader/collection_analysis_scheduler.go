package reader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"
)

const analysisScheduleOwner = "analysis-scheduler"
const analysisScanBudget = 200
const analysisBatchBudget = 5

type analysisPosition struct {
	Date string `json:"date"`
	ID   string `json:"id"`
}
type analysisSchedule struct {
	Version   int                         `json:"version"`
	Identity  string                      `json:"identity"`
	Channel   int                         `json:"channel"`
	Task      int                         `json:"task"`
	Positions map[string]analysisPosition `json:"positions"`
}

func (m *CollectionManager) runAnalysisRound(ctx context.Context) {
	if !m.schedulingAllowed() || ctx.Err() != nil {
		return
	}
	cfg, err := m.server.Store.CollectionSettings()
	if err != nil || !cfg.AnalysisEnabled || cfg.DailyLimit == 0 {
		return
	}
	if !m.analysisSchedule.TryLock() {
		return
	}
	defer m.analysisSchedule.Unlock()
	tasks, err := m.server.Store.CollectionTasks()
	if err != nil {
		log.Printf("analysis scheduler tasks: %v", err)
		return
	}
	enabled := []CollectionTask{}
	weighted := []CollectionTask{}
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		enabled = append(enabled, task)
		for i := 0; i < 6-task.Priority; i++ {
			weighted = append(weighted, task)
		}
	}
	if len(weighted) == 0 {
		return
	}
	snapshot := []any{}
	for _, task := range enabled {
		snapshot = append(snapshot, []any{task.ID, task.QueryRevision, task.Priority, task.Config.LatestWindowDays, task.Config.HotWindowMonths})
	}
	ai := m.server.aiConfig()
	identity := collectionDigest([]any{analysisSelection(cfg, ai.Primary), ai.API.URL, collectionDigest(ai.API.Key), snapshot})
	state := analysisSchedule{Version: 1, Identity: identity, Positions: map[string]analysisPosition{}}
	var body string
	err = m.server.Store.DB.QueryRow("SELECT body FROM collection_cursors WHERE owner_key=?", analysisScheduleOwner).Scan(&body)
	if err == nil {
		if err := json.Unmarshal([]byte(body), &state); err != nil {
			log.Printf("analysis scheduler state: %v", err)
			return
		}
		if state.Version != 1 || state.Channel < 0 || state.Task < 0 {
			log.Printf("analysis scheduler state unsupported")
			return
		}
		if state.Identity != identity {
			state = analysisSchedule{Version: 1, Identity: identity, Positions: map[string]analysisPosition{}}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("analysis scheduler: %v", err)
		return
	}
	if state.Positions == nil {
		state.Positions = map[string]analysisPosition{}
	}
	processed := 0
	for inspected := 0; inspected < analysisScanBudget && processed < analysisBatchBudget; inspected++ {
		if ctx.Err() != nil || !m.schedulingAllowed() {
			return
		}
		task := weighted[state.Task%len(weighted)]
		channel := channelLatest
		if state.Channel%10 >= 7 {
			channel = channelHot
		}
		state.Task = (state.Task + 1) % len(weighted)
		state.Channel = (state.Channel + 1) % 10
		candidate, position, key, err := m.analysisNextCandidate(task, channel, state.Positions)
		if errors.Is(err, sql.ErrNoRows) {
			if channel == channelLatest {
				channel = channelHot
			} else {
				channel = channelLatest
			}
			candidate, position, key, err = m.analysisNextCandidate(task, channel, state.Positions)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			log.Printf("analysis queue: %v", err)
			return
		}
		operation := &analysisOperation{}
		_, err = m.analyzeCandidate(ctx, candidate, false, operation)
		if errors.Is(err, errAnalysisQuota) {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("analysis candidate %s: %v", candidate, err)
		}
		state.Positions[key] = position
		if operation.sent > 0 {
			processed++
		}
		if err := m.saveAnalysisSchedule(state); err != nil {
			log.Printf("analysis cursor: %v", err)
			return
		}
	}
	if err := m.saveAnalysisSchedule(state); err != nil {
		log.Printf("analysis cursor: %v", err)
	}
}

func (m *CollectionManager) analysisNextCandidate(task CollectionTask, channel string, positions map[string]analysisPosition) (string, analysisPosition, string, error) {
	key := task.ID + ":" + channel
	position := positions[key]
	latest, hot := PlanWindows(m.clockNow(), task.Config.LatestWindowDays, task.Config.HotWindowMonths)
	window := latest
	if channel == channelHot {
		window = hot
	}
	query := `SELECT c.arxiv_id,c.published_at FROM collection_candidates c WHERE c.ignored=0 AND c.published_at>=? AND c.published_at<? AND EXISTS(SELECT 1 FROM collection_matches m WHERE m.arxiv_id=c.arxiv_id AND m.task_id=? AND m.query_revision=? AND m.channel=?)`
	args := []any{window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), task.ID, task.QueryRevision, channel}
	if position.ID != "" {
		query += " AND (c.published_at<? OR (c.published_at=? AND c.arxiv_id<?))"
		args = append(args, position.Date, position.Date, position.ID)
	}
	query += " ORDER BY c.published_at DESC,c.arxiv_id DESC LIMIT 1"
	var next analysisPosition
	err := m.server.Store.DB.QueryRow(query, args...).Scan(&next.ID, &next.Date)
	if errors.Is(err, sql.ErrNoRows) && position.ID != "" {
		delete(positions, key)
	}
	return next.ID, next, key, err
}

func (m *CollectionManager) saveAnalysisSchedule(state analysisSchedule) error {
	tx, err := m.server.Store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO collection_cursors(owner_key,body,updated_at) VALUES(?,?,?) ON CONFLICT(owner_key) DO UPDATE SET body=excluded.body,updated_at=excluded.updated_at", analysisScheduleOwner, mustJSONText(state), now()); err != nil {
		return err
	}
	return tx.Commit()
}
