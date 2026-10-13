package reader

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAnalysisUnicodeBudgetUsesCharacters(t *testing.T) {
	inputs, err := BuildAnalysisInputs("论文", strings.Repeat("中", 10000), nil, []AnalysisTaskMaterial{analysisTask("t", "p", true)})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("10000 Unicode characters rejected: %v", err)
	}
}

func TestAnalysisWireRejectsMissingNullAndDuplicateFields(t *testing.T) {
	input := AnalysisInput{Title: analysisTitle, Abstract: analysisAbstract, Tasks: []AnalysisTaskMaterial{analysisTask("t1", "p1", true)}}
	body, err := json.Marshal(validResult())
	if err != nil {
		t.Fatal(err)
	}
	good := string(body)
	if _, err := DecodeAnalysisResult(good, input); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"missing score":   strings.Replace(good, `"relevance_score":80`, `"other_score":80`, 1),
		"null score":      strings.Replace(good, `"relevance_score":80`, `"relevance_score":null`, 1),
		"float score":     strings.Replace(good, `"relevance_score":80`, `"relevance_score":80.0`, 1),
		"duplicate score": strings.Replace(good, `"relevance_score":80`, `"relevance_score":80,"relevance_score":0`, 1),
		"wrong case":      strings.Replace(good, `"relevance_score":80`, `"RELEVANCE_SCORE":80`, 1),
		"unknown field":   strings.Replace(good, `"schema_version":1`, `"schema_version":1,"extra":0`, 1),
		"missing tasks":   strings.Replace(good, `"assessments":`, `"other":`, 1),
		"trailing object": good + `{}`,
		"partial output":  good[:len(good)-2],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAnalysisResult(bad, input); err == nil {
				t.Fatal("invalid raw JSON accepted")
			}
		})
	}
}

func TestAnalysisBudgetCountsSerializedEscapes(t *testing.T) {
	if _, err := BuildAnalysisInputs("title", strings.Repeat("\x00", 5000), nil, []AnalysisTaskMaterial{analysisTask("t", "p", true)}); err != errInputTooLarge {
		t.Fatalf("serialized budget not enforced: %v", err)
	}
}

func TestAnalysisDispatchRechecksDayAndLoweredLimit(t *testing.T) {
	s := testStore(t)
	cfg := defaultCollectionSettings()
	cfg.AnalysisEnabled, cfg.DailyLimit = true, 1
	if err := s.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReserveAnalysisAttempt("midnight", "req", "cache", "2026-10-12", 1); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	at := time.Date(2026, 10, 13, 0, 0, 1, 0, time.UTC)
	if err := s.BeginAnalysisDispatch("midnight", at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAttemptSent("midnight"); err != nil {
		t.Fatal(err)
	}
	var sent, reserved int
	if err := s.DB.QueryRow("SELECT sent,reserved FROM collection_quota_days WHERE utc_day='2026-10-13'").Scan(&sent, &reserved); err != nil || sent != 1 || reserved != 0 {
		t.Fatalf("new day counters: %d %d %v", sent, reserved, err)
	}
	if err := s.DB.QueryRow("SELECT sent,reserved FROM collection_quota_days WHERE utc_day='2026-10-12'").Scan(&sent, &reserved); err != nil || sent != 0 || reserved != 0 {
		t.Fatalf("old day counters: %d %d %v", sent, reserved, err)
	}
	if ok, err := s.ReserveAnalysisAttempt("lowered", "req", "cache", "2026-10-14", 1); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	cfg.DailyLimit = 0
	if err := s.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAnalysisDispatch("lowered", at.Add(24*time.Hour)); err == nil {
		t.Fatal("lowered zero limit ignored at dispatch")
	}
	if err := s.ReleaseAttempt("lowered"); err != nil {
		t.Fatal(err)
	}
}

func TestAnalysisStartupSettlesAttemptsExactlyOnce(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"reserved", "dispatching", "sent"} {
		if ok, err := s.ReserveAnalysisAttempt(name, "req", "cache", "2026-10-13", 3); err != nil || !ok {
			t.Fatalf("reserve: %v %v", ok, err)
		}
	}
	if err := s.MarkAttemptSent("sent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE collection_attempts SET status='dispatching' WHERE attempt_id='dispatching'"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RecoverAnalysisAttempts(); err != nil {
			t.Fatal(err)
		}
	}
	var sent, reserved, uncertain int
	if err := s.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days WHERE utc_day='2026-10-13'").Scan(&sent, &reserved, &uncertain); err != nil || sent != 2 || reserved != 0 || uncertain != 1 {
		t.Fatalf("restart counters drifted: %d %d %d %v", sent, reserved, uncertain, err)
	}
	var active int
	if err := s.DB.QueryRow("SELECT count(*) FROM collection_attempts WHERE status IN ('reserved','dispatching','sent')").Scan(&active); err != nil || active != 0 {
		t.Fatalf("restart retained active attempts: %d %v", active, err)
	}
}

func TestAnalysisTranslationOnlyRejectsJudgments(t *testing.T) {
	input := AnalysisInput{Tasks: []AnalysisTaskMaterial{analysisTask("t1", "p1", false)}, Title: analysisTitle, Abstract: analysisAbstract}
	if err := ValidateAnalysisResult(validResult(), input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("translation-only request accepted a judgment")
	}
}

func TestAnalysisQuotaUsesLatestStoredLimit(t *testing.T) {
	s := testStore(t)
	cfg := defaultCollectionSettings()
	cfg.DailyLimit = 1
	if err := s.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReserveAnalysisAttempt("first", "req", "cache", "2026-10-13", 100); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if ok, err := s.ReserveAnalysisAttempt("second", "req", "cache", "2026-10-13", 100); err != nil || ok {
		t.Fatalf("stale limit admitted request: %v %v", ok, err)
	}
}

func TestAnalysisZeroQuotaAllowsNoReservation(t *testing.T) {
	s := testStore(t)
	cfg := defaultCollectionSettings()
	cfg.DailyLimit = 0
	if err := s.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReserveAnalysisAttempt("zero", "req", "cache", "2026-10-13", 100); err != nil || ok {
		t.Fatalf("zero quota admitted request: %v %v", ok, err)
	}
}

func TestAnalysisUnknownDispatchCannotBeRefunded(t *testing.T) {
	s := testStore(t)
	if ok, err := s.ReserveAnalysisAttempt("uncertain", "req", "cache", "2026-10-13", 1); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if _, err := s.DB.Exec("UPDATE collection_attempts SET status='dispatching' WHERE attempt_id='uncertain'"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAttempt("uncertain"); err == nil {
		t.Fatal("unknown dispatch refunded quota")
	}
}

func TestAnalysisUnknownDispatchSettlesAsSent(t *testing.T) {
	s := testStore(t)
	if ok, err := s.ReserveAnalysisAttempt("uncertain", "req", "cache", "2026-10-13", 1); err != nil || !ok {
		t.Fatalf("reserve: %v %v", ok, err)
	}
	if _, err := s.DB.Exec("UPDATE collection_attempts SET status='dispatching' WHERE attempt_id='uncertain'"); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleSentAttempt("uncertain", true); err != nil {
		t.Fatal(err)
	}
	var sent, reserved, uncertain int
	if err := s.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days WHERE utc_day='2026-10-13'").Scan(&sent, &reserved, &uncertain); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || reserved != 0 || uncertain != 1 {
		t.Fatalf("unknown dispatch counters: sent=%d reserved=%d uncertain=%d", sent, reserved, uncertain)
	}
}
