package reader

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Analysis input budget in characters for the whole request (system + user).
// Splitting a task set produces separately metered requests; base material
// alone over budget is input_too_large, never silently truncated.
const analysisInputBudget = 24000

const analysisPromptVersion = collectionPromptVersion

// AnalysisTaskMaterial is one task's semantic material sent to the model.
type AnalysisTaskMaterial struct {
	ProfileHash string
	TaskID      string
	Name        string
	Topic       string
	NotInterest string
	Keywords    string
	WantsScore  bool
}

// AnalysisInput is one built request: original paper material plus the task
// materials that fit the budget.
type AnalysisInput struct {
	Tasks        []AnalysisTaskMaterial
	Title        string
	Abstract     string
	Categories   []string
	NeedsScore   bool
}

var errInputTooLarge = errors.New("输入超过预算")

// BuildAnalysisInputs packs paper material and task semantics under the
// character budget, one or more requests each separately metered. The paper
// base material is never truncated; a base alone over budget fails.
func BuildAnalysisInputs(title, abstract string, categories []string, tasks []AnalysisTaskMaterial) ([]AnalysisInput, error) {
	base := len(title) + len(abstract) + len(strings.Join(categories, " ")) + 512
	if base > analysisInputBudget {
		return nil, errInputTooLarge
	}
	remaining := analysisInputBudget - base
	out := []AnalysisInput{}
	current := AnalysisInput{Title: title, Abstract: abstract, Categories: categories}
	used := 0
	for _, t := range tasks {
		cost := len(t.Name) + len(t.Topic) + len(t.NotInterest) + len(t.Keywords) + 64
		if cost > remaining {
			return nil, errInputTooLarge
		}
		if used+cost > remaining {
			out = append(out, current)
			current = AnalysisInput{Title: title, Abstract: abstract, Categories: categories}
			used = 0
		}
		current.Tasks = append(current.Tasks, t)
		current.NeedsScore = current.NeedsScore || t.WantsScore
		used += cost
	}
	if len(current.Tasks) > 0 {
		out = append(out, current)
	}
	return out, nil
}

// AnalysisEvidence is one verifiable original-text quote.
type AnalysisEvidence struct {
	Field string `json:"field"`
	Quote string `json:"quote"`
}

// AnalysisAssessment is one task's validated judgment.
type AnalysisAssessment struct {
	Evidence       []AnalysisEvidence `json:"evidence"`
	TaskID         string             `json:"task_id"`
	ProfileHash    string             `json:"profile_hash"`
	ReasonZH       string             `json:"reason_zh"`
	RelevanceScore int                `json:"relevance_score"`
}

// AnalysisResult is the strict schema v1 output.
type AnalysisResult struct {
	Assessments   []AnalysisAssessment `json:"assessments"`
	SchemaVersion int                  `json:"schema_version"`
	TitleZH       string               `json:"title_zh"`
	AbstractZH    string               `json:"abstract_zh"`
}

var errInvalidAnalysis = errors.New("分析结果不符合合同")

// ValidateAnalysisResult strictly checks one request's model output against
// schema v1 and the requested task set: exactly one assessment per task, no
// unknown or duplicate tasks, matching profile hashes, integer 0–100 scores,
// non-empty Chinese reasons, and evidence quotes that appear continuously in
// the corresponding original field (whitespace-normalized only). Translation
// may stand alone for translation-only requests (empty assessments).
func ValidateAnalysisResult(raw AnalysisResult, input AnalysisInput, title, abstract string) error {
	if raw.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema_version 必须为 1", errInvalidAnalysis)
	}
	if strings.TrimSpace(raw.TitleZH) == "" || strings.TrimSpace(raw.AbstractZH) == "" {
		return fmt.Errorf("%w: 缺少完整中文译文", errInvalidAnalysis)
	}
	if !hasCJK(raw.TitleZH) || !hasCJK(raw.AbstractZH) {
		return fmt.Errorf("%w: 译文须为中文", errInvalidAnalysis)
	}
	wanted := map[string]AnalysisTaskMaterial{}
	for _, t := range input.Tasks {
		wanted[t.TaskID] = t
	}
	needsScore := false
	for _, t := range input.Tasks {
		if t.WantsScore {
			needsScore = true
		}
	}
	seen := map[string]bool{}
	for _, a := range raw.Assessments {
		t, ok := wanted[a.TaskID]
		if !ok {
			return fmt.Errorf("%w: 出现未请求的任务 %q", errInvalidAnalysis, a.TaskID)
		}
		if seen[a.TaskID] {
			return fmt.Errorf("%w: 任务 %q 重复判断", errInvalidAnalysis, a.TaskID)
		}
		seen[a.TaskID] = true
		if a.ProfileHash != t.ProfileHash {
			return fmt.Errorf("%w: 任务 %q 的 profile 不符", errInvalidAnalysis, a.TaskID)
		}
		if t.WantsScore {
			if a.RelevanceScore < 0 || a.RelevanceScore > 100 {
				return fmt.Errorf("%w: 分数超出范围", errInvalidAnalysis)
			}
			if strings.TrimSpace(a.ReasonZH) == "" || !hasCJK(a.ReasonZH) {
				return fmt.Errorf("%w: 任务 %q 缺少中文判断依据", errInvalidAnalysis, a.TaskID)
			}
			if len(a.Evidence) == 0 {
				return fmt.Errorf("%w: 任务 %q 缺少原文证据", errInvalidAnalysis, a.TaskID)
			}
		}
		for _, e := range a.Evidence {
			if e.Field != "title" && e.Field != "abstract" {
				return fmt.Errorf("%w: 证据字段只能是 title 或 abstract", errInvalidAnalysis)
			}
			if strings.TrimSpace(e.Quote) == "" {
				return fmt.Errorf("%w: 证据片段为空", errInvalidAnalysis)
			}
			source := title
			if e.Field == "abstract" {
				source = abstract
			}
			if !quoteInSource(source, e.Quote) {
				return fmt.Errorf("%w: 证据片段不在原文中连续出现", errInvalidAnalysis)
			}
		}
	}
	if needsScore {
		for _, t := range input.Tasks {
			if t.WantsScore && !seen[t.TaskID] {
				return fmt.Errorf("%w: 缺少任务 %q 的判断", errInvalidAnalysis, t.TaskID)
			}
		}
	}
	return nil
}

// quoteInSource verifies the quote occurs continuously in the source after
// whitespace normalization only.
func quoteInSource(source, quote string) bool {
	return strings.Contains(normalizeSpace(source), normalizeSpace(quote))
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// ReserveAnalysisAttempt atomically reserves one metered request on the given
// UTC day. ok=false when the daily limit is reached; the reservation must be
// settled or released by the caller under its attempt identity.
func (s *Store) ReserveAnalysisAttempt(attemptID, requestKey, cacheKey, utcDay string, limit int) (bool, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var sent, reserved int
	err = tx.QueryRow("SELECT sent,reserved FROM collection_quota_days WHERE utc_day=?", utcDay).Scan(&sent, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec("INSERT INTO collection_quota_days(utc_day,sent,reserved,uncertain) VALUES(?,?,?,0)", utcDay, 0, 0); err != nil {
			return false, err
		}
		sent, reserved = 0, 0
	} else if err != nil {
		return false, err
	}
	if sent+reserved >= limit {
		return false, nil
	}
	if _, err := tx.Exec("UPDATE collection_quota_days SET reserved=reserved+1 WHERE utc_day=?", utcDay); err != nil {
		return false, err
	}
	if _, err := tx.Exec("INSERT INTO collection_attempts(attempt_id,request_key,cache_key,reserved_day,status,created_at) VALUES(?,?,?,?,?,?)",
		attemptID, requestKey, cacheKey, utcDay, attemptReserved, now()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// MarkAttemptSent records the real dispatch point: the reservation becomes a
// sent request and the day counter moves from reserved to sent.
func (s *Store) MarkAttemptSent(attemptID string) error {
	return s.transitionAttempt(attemptID, []string{attemptReserved, attemptDispatching}, attemptSent, func(tx *sql.Tx, day string) error {
		_, err := tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0),sent=sent+1 WHERE utc_day=?", day)
		return err
	})
}

// ReleaseAttempt frees a reservation that never dispatched (config failure or
// cancel before send). A cross-midnight release never touches the new day.
func (s *Store) ReleaseAttempt(attemptID string) error {
	return s.transitionAttempt(attemptID, []string{attemptReserved, attemptDispatching}, attemptReleased, func(tx *sql.Tx, day string) error {
		_, err := tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0) WHERE utc_day=?", day)
		return err
	})
}

// SettleSentAttempt finalizes a sent request (success and failure both count).
// An uncertain dispatch at recovery is settled with the uncertain marker:
// conservative metering, never refunded.
func (s *Store) SettleSentAttempt(attemptID string, uncertain bool) error {
	return s.transitionAttempt(attemptID, []string{attemptSent, attemptDispatching}, attemptSettled, func(tx *sql.Tx, day string) error {
		if uncertain {
			_, err := tx.Exec("UPDATE collection_quota_days SET uncertain=uncertain+1 WHERE utc_day=?", day)
			return err
		}
		return nil
	})
}

// transitionAttempt applies one state transition under the attempt identity
// plus the quota-day adjustment, all in one transaction.
func (s *Store) transitionAttempt(attemptID string, from []string, to string, adjust func(*sql.Tx, string) error) error {
	if !validAttemptStatus(to) {
		return fmt.Errorf("未知 attempt 状态 %q", to)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var day string
	var status string
	if err := tx.QueryRow("SELECT reserved_day,status FROM collection_attempts WHERE attempt_id=?", attemptID).Scan(&day, &status); err != nil {
		return err
	}
	allowed := false
	for _, f := range from {
		if status == f {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("attempt %s 当前状态 %q 不能转到 %q", attemptID, status, to)
	}
	finished := to == attemptSettled || to == attemptReleased
	var finishedAt any
	if finished {
		finishedAt = now()
	}
	if _, err := tx.Exec("UPDATE collection_attempts SET status=?,finished_at=? WHERE attempt_id=?", to, finishedAt, attemptID); err != nil {
		return err
	}
	if adjust != nil {
		if err := adjust(tx, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}
