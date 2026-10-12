package reader

import (
	"strings"
	"testing"
	"time"
)

func analysisTask(id, profile string, wants bool) AnalysisTaskMaterial {
	return AnalysisTaskMaterial{TaskID: id, ProfileHash: profile, Name: "视觉", Topic: "目标检测", Keywords: "object detection", WantsScore: wants}
}

func validResult() AnalysisResult {
	return AnalysisResult{
		SchemaVersion: 1,
		TitleZH:       "目标检测方法",
		AbstractZH:    "本文研究目标检测与知识蒸馏。",
		Assessments: []AnalysisAssessment{{
			TaskID:         "t1",
			ProfileHash:    "p1",
			RelevanceScore: 80,
			ReasonZH:       "与目标检测高度相关。",
			Evidence:       []AnalysisEvidence{{Field: "abstract", Quote: "object detection and knowledge"}},
		}},
	}
}

const analysisTitle = "Object Detection with Knowledge"
const analysisAbstract = "We study object detection and knowledge distillation in depth."

func TestValidateAnalysisResultStrict(t *testing.T) {
	input := AnalysisInput{Tasks: []AnalysisTaskMaterial{analysisTask("t1", "p1", true)}, Title: analysisTitle, Abstract: analysisAbstract}
	raw := validResult()
	if err := ValidateAnalysisResult(raw, input, analysisTitle, analysisAbstract); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	// Float-like score is impossible in the typed struct; boundary values and
	// structural violations are what the contract can reject at this layer.
	bad := validResult()
	bad.SchemaVersion = 2
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("future schema accepted")
	}
	bad = validResult()
	bad.TitleZH = "  "
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("empty translation accepted")
	}
	bad = validResult()
	bad.Assessments[0].RelevanceScore = 101
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("out-of-range score accepted")
	}
	bad = validResult()
	bad.Assessments[0].TaskID = "other"
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("unknown task accepted")
	}
	bad = validResult()
	bad.Assessments = append(bad.Assessments, bad.Assessments[0])
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("duplicate task accepted")
	}
	bad = validResult()
	bad.Assessments[0].ProfileHash = "forged"
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("forged profile accepted")
	}
	bad = validResult()
	bad.Assessments[0].Evidence = []AnalysisEvidence{{Field: "abstract", Quote: "a forged quote"}}
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("forged quote accepted")
	}
	bad = validResult()
	bad.Assessments[0].Evidence = []AnalysisEvidence{{Field: "body", Quote: "object detection"}}
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("illegal evidence field accepted")
	}
	bad = validResult()
	bad.Assessments[0].ReasonZH = ""
	if err := ValidateAnalysisResult(bad, input, analysisTitle, analysisAbstract); err == nil {
		t.Fatal("empty reason accepted")
	}
	// A translation-only request accepts empty assessments but still needs
	// the full Chinese translation.
	only := AnalysisInput{Tasks: []AnalysisTaskMaterial{analysisTask("t1", "p1", false)}, Title: analysisTitle, Abstract: analysisAbstract}
	trans := AnalysisResult{SchemaVersion: 1, TitleZH: "标题", AbstractZH: "摘要内容"}
	if err := ValidateAnalysisResult(trans, only, analysisTitle, analysisAbstract); err != nil {
		t.Fatalf("translation-only rejected: %v", err)
	}
	// Whitespace-only differences in quotes are accepted; word gaps are not
	// fabricated beyond whitespace normalization.
	near := validResult()
	near.Assessments[0].Evidence = []AnalysisEvidence{{Field: "abstract", Quote: "object detection   and knowledge"}}
	if err := ValidateAnalysisResult(near, input, analysisTitle, analysisAbstract); err != nil {
		t.Fatalf("whitespace-normalized quote rejected: %v", err)
	}
}

func TestBuildAnalysisInputsBudget(t *testing.T) {
	base := AnalysisTaskMaterial{TaskID: "t1", ProfileHash: "p1", Name: "n", WantsScore: true}
	// A base material alone over budget is input_too_large, never truncated.
	_, err := BuildAnalysisInputs(strings.Repeat("t", analysisInputBudget), "a", nil, []AnalysisTaskMaterial{base})
	if err != errInputTooLarge {
		t.Fatalf("oversize base err = %v", err)
	}
	// Many tasks split into separate metered requests.
	tasks := make([]AnalysisTaskMaterial, 0, 30)
	for i := 0; i < 30; i++ {
		tasks = append(tasks, AnalysisTaskMaterial{
			TaskID: "t" + string(rune('a'+i%26)) + string(rune('0'+i/26)), ProfileHash: "p",
			Name: "n", Topic: strings.Repeat("x", 400), Keywords: strings.Repeat("y", 400), WantsScore: true,
		})
	}
	inputs, err := BuildAnalysisInputs("title", "abstract", []string{"cs.AI"}, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) < 2 {
		t.Fatalf("expected split requests, got %d", len(inputs))
	}
	seen := 0
	for _, in := range inputs {
		seen += len(in.Tasks)
	}
	if seen != len(tasks) {
		t.Fatalf("split lost tasks: %d != %d", seen, len(tasks))
	}
}

func TestQuotaAttemptLifecycle(t *testing.T) {
	s := testStore(t)
	day := "2026-10-12"
	// limit=1: one reservation only.
	ok, err := s.ReserveAnalysisAttempt("a1", "req1", "c1", day, 1)
	if err != nil || !ok {
		t.Fatalf("first reserve: %v %v", ok, err)
	}
	if ok, _ := s.ReserveAnalysisAttempt("a2", "req2", "c2", day, 1); ok {
		t.Fatal("second reservation exceeded the daily limit")
	}
	// A release frees the slot; a later reservation may use it.
	if err := s.ReleaseAttempt("a1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ReserveAnalysisAttempt("a2", "req2", "c2", day, 1); !ok {
		t.Fatal("released slot not reusable")
	}
	// Sent counts against the day; settle keeps the count (failures count too).
	if err := s.MarkAttemptSent("a2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleSentAttempt("a2", false); err != nil {
		t.Fatal(err)
	}
	var sent, reserved, uncertain int
	s.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days WHERE utc_day=?", day).Scan(&sent, &reserved, &uncertain)
	if sent != 1 || reserved != 0 || uncertain != 0 {
		t.Fatalf("quota day = sent %d reserved %d uncertain %d", sent, reserved, uncertain)
	}
	if ok, _ := s.ReserveAnalysisAttempt("a3", "req3", "c3", day, 1); ok {
		t.Fatal("sent request did not consume the limit")
	}
	// An uncertain dispatch is metered conservatively.
	if ok, _ := s.ReserveAnalysisAttempt("a4", "req4", "c4", day, 2); !ok {
		t.Fatal("limit=2 reservation failed")
	}
	if err := s.MarkAttemptSent("a4"); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleSentAttempt("a4", true); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow("SELECT sent,reserved,uncertain FROM collection_quota_days WHERE utc_day=?", day).Scan(&sent, &reserved, &uncertain)
	if sent != 2 || uncertain != 1 {
		t.Fatalf("uncertain metering: sent %d uncertain %d", sent, uncertain)
	}
	// Illegal transitions are refused.
	if err := s.SettleSentAttempt("a4", false); err == nil {
		t.Fatal("double settle accepted")
	}
	if err := s.ReleaseAttempt("a4"); err == nil {
		t.Fatal("release after settle accepted")
	}
	// A release from a previous day never touches the new day's counters.
	next := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	if ok, _ := s.ReserveAnalysisAttempt("a5", "req5", "c5", next, 1); !ok {
		t.Fatal("new day reservation failed")
	}
	if err := s.ReleaseAttempt("a5"); err != nil {
		t.Fatal(err)
	}
	s.DB.QueryRow("SELECT sent FROM collection_quota_days WHERE utc_day=?", next).Scan(&sent)
	if sent != 0 {
		t.Fatalf("cross-day release touched the new day: sent=%d", sent)
	}
}
