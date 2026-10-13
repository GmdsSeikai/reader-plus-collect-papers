package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var errAnalysisQuota = errors.New("摘要分析今日额度已用完")

// Only transports that observed no request headers or prompt bytes use this
// transition. An ordinary cancellation cannot refund dispatching work.
func (s *Store) releaseUndeliveredAnalysis(attemptID string) error {
	return s.transitionAttempt(attemptID, []string{attemptDispatching}, attemptReleased, func(tx *sql.Tx, day, status string) error {
		_, err := tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0) WHERE utc_day=?", day)
		return err
	})
}

func collectionSettingsTx(tx *sql.Tx) (CollectionSettings, error) {
	cfg := defaultCollectionSettings()
	var body string
	err := tx.QueryRow("SELECT body FROM collection_settings WHERE id=1").Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return cfg, err
	}
	if cfg.DailyLimit < 0 || cfg.RequestRPM < 0 {
		return cfg, errors.New("采集额度配置无效")
	}
	return cfg, nil
}

// Immediately before delivering the native prompt, recheck the current limit
// and move an unsent reservation to its real UTC dispatch day atomically.
func (s *Store) BeginAnalysisDispatch(attemptID string, at time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	cfg, err := collectionSettingsTx(tx)
	if err != nil {
		return err
	}
	if !cfg.AnalysisEnabled || cfg.Paused {
		return errors.New("摘要分析未启用或已暂停")
	}
	var day, status string
	if err := tx.QueryRow("SELECT reserved_day,status FROM collection_attempts WHERE attempt_id=?", attemptID).Scan(&day, &status); err != nil {
		return err
	}
	if status != attemptReserved {
		return errors.New("分析请求已发送或关闭")
	}
	dispatchDay := utcDay(at)
	if _, err := tx.Exec("INSERT INTO collection_quota_days(utc_day,sent,reserved,uncertain) VALUES(?,0,0,0) ON CONFLICT DO NOTHING", dispatchDay); err != nil {
		return err
	}
	var sent, reserved int
	if err := tx.QueryRow("SELECT sent,reserved FROM collection_quota_days WHERE utc_day=?", dispatchDay).Scan(&sent, &reserved); err != nil {
		return err
	}
	needed := 1
	if dispatchDay == day {
		needed = 0
	}
	if cfg.DailyLimit == 0 || sent+reserved+needed > cfg.DailyLimit {
		return errAnalysisQuota
	}
	if dispatchDay != day {
		if _, err := tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0) WHERE utc_day=?", day); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE collection_quota_days SET reserved=reserved+1 WHERE utc_day=?", dispatchDay); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE collection_attempts SET status='dispatching',reserved_day=? WHERE attempt_id=? AND status='reserved'", dispatchDay, attemptID); err != nil {
		return err
	}
	return tx.Commit()
}

// Startup owns all unfinished attempts: reserved work never left the process;
// dispatching work may have reached the provider and consumes quota once.
func (s *Store) RecoverAnalysisAttempts() error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT attempt_id,reserved_day,status FROM collection_attempts WHERE status IN ('reserved','dispatching','sent')")
	if err != nil {
		return err
	}
	type attempt struct{ id, day, status string }
	unfinished := []attempt{}
	for rows.Next() {
		var a attempt
		if err := rows.Scan(&a.id, &a.day, &a.status); err != nil {
			rows.Close()
			return err
		}
		unfinished = append(unfinished, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range unfinished {
		state := attemptSettled
		switch a.status {
		case attemptReserved:
			state = attemptReleased
			_, err = tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0) WHERE utc_day=?", a.day)
		case attemptDispatching:
			_, err = tx.Exec("UPDATE collection_quota_days SET reserved=MAX(reserved-1,0),sent=sent+1,uncertain=uncertain+1 WHERE utc_day=?", a.day)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE collection_attempts SET status=?,finished_at=?,error='startup-interrupted' WHERE attempt_id=?", state, now(), a.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
