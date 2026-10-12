package reader

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"reader.local/server/internal/scholar"
)

func TestPlanWindowsClampsMonthEnd(t *testing.T) {
	cases := []struct {
		now       time.Time
		months    int
		wantFrom  string
	}{
		{time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC), 1, "2026-02-28"},
		{time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC), 1, "2026-02-28"},
		{time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC), 1, "2024-02-29"}, // leap
		{time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), 1, "2025-12-31"}, // cross year
		{time.Date(2026, 10, 12, 15, 30, 0, 0, time.UTC), 12, "2025-10-12"},
		{time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), 12, "2025-03-31"},
	}
	for _, c := range cases {
		_, hot := PlanWindows(c.now, 30, c.months)
		got := hot.From.Format("2006-01-02")
		if got != c.wantFrom {
			t.Fatalf("hot window from %s months=%d = %s want %s", c.now.Format("2006-01-02"), c.months, got, c.wantFrom)
		}
		if !hot.From.Before(c.now) {
			t.Fatalf("hot from not before now: %v", hot.From)
		}
	}
}

func TestCitationFirstFailureSetsBackoff(t *testing.T) {
	s := testStore(t)
	seedCandidate(t, s, "2301.00009")
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	// First fetch unknown: the row must carry a 24h backoff, not NULL.
	if err := s.UpsertCitation(scholarCitationUnknown("2301.00009"), base); err != nil {
		t.Fatal(err)
	}
	var refresh any
	if err := s.DB.QueryRow("SELECT refresh_at FROM collection_citations WHERE arxiv_id=?", "2301.00009").Scan(&refresh); err != nil {
		t.Fatal(err)
	}
	if refresh == nil {
		t.Fatal("first failure left refresh_at NULL (immediately due)")
	}
	// Not due one minute later.
	due, err := s.DueCitations(base.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ArXivID == "2301.00009" {
			t.Fatal("failed item is due again one minute later")
		}
	}
	// Due after the backoff.
	due, err = s.DueCitations(base.Add(25*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		if d.ArXivID == "2301.00009" {
			found = true
		}
	}
	if !found {
		t.Fatal("failed item not due after 24h")
	}
}

func TestCandidateTaskFilterDedupesAcrossChannels(t *testing.T) {
	s := testStore(t)
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups = [][]string{{"x"}}
	task, err := s.SaveCollectionTask(CollectionTask{Name: "n", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.DB.Begin()
	cand := CollectionCandidate{ArXivID: "2301.12345", Title: "T", Abstract: "B", PublishedAt: "2023-01-02T00:00:00Z", UpdatedAt: "2023-01-02T00:00:00Z", LatestVersion: 1, CanonicalVer: 1, ContentHash: "h", FirstSeenAt: now(), LastSeenAt: now()}
	if err := s.UpsertCandidate(tx, cand); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{channelLatest, channelHot} {
		m := CollectionMatch{TaskID: task.ID, QueryRevision: task.QueryRevision, ArXivID: cand.ArXivID, Channel: ch, BranchHash: "b-" + ch, RuleScore: 40, FirstSeenAt: now(), LastSeenAt: now()}
		if err := s.UpsertMatch(tx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := s.CollectionCandidates(CandidateFilter{TaskID: task.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("task filter duplicated rows: %d", len(page.Items))
	}
	// Channel filters separate the two windows.
	page, err = s.CollectionCandidates(CandidateFilter{TaskID: task.ID, Channel: channelLatest, Limit: 50})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("latest channel rows=%d err=%v", len(page.Items), err)
	}
	if _, err = s.CollectionCandidates(CandidateFilter{TaskID: task.ID, Channel: "nope", Limit: 50}); err == nil {
		t.Fatal("unknown channel must be rejected")
	}
}

func TestCandidateCitationKeysetPagingNoLoss(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"2301.00001", "2301.00002"} {
		seedCandidate(t, s, id)
	}
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	one, five := 100, 50
	if err := s.UpsertCitation(scholarCitationCount("2301.00001", &one), base); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCitation(scholarCitationCount("2301.00002", &five), base); err != nil {
		t.Fatal(err)
	}
	page, err := s.CollectionCandidates(CandidateFilter{Sort: "citations", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ArXivID != "2301.00001" {
		t.Fatalf("first page = %+v", page.Items)
	}
	if page.NextCursor == "" {
		t.Fatal("no cursor for remaining row")
	}
	// The cursor must be accepted only under the same sort identity.
	if _, _, _, err := DecodeCursor(page.NextCursor, CandidateFilter{Sort: "date"}); err == nil {
		t.Fatal("cursor reused across sort modes")
	}
	key, date, id, err := DecodeCursor(page.NextCursor, CandidateFilter{Sort: "citations"})
	if err != nil {
		t.Fatal(err)
	}
	if key != 100 || id != "2301.00001" || date == "" {
		t.Fatalf("cursor values = %d %q %q", key, date, id)
	}
	next := CandidateFilter{Sort: "citations", Limit: 1, CursorNum: key, CursorDate: date, CursorID: id}
	page2, err := s.CollectionCandidates(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 1 || page2.Items[0].ArXivID != "2301.00002" {
		t.Fatalf("second page lost the 50-citation row: %+v", page2.Items)
	}
}

func TestImportLocalNameSafeForOldIDs(t *testing.T) {
	name := importLocalName("hep-th/9901001", 1)
	if strings.ContainsAny(name, "/\\") {
		t.Fatalf("unsafe local name %q", name)
	}
	if name != "hep-th_9901001v1.pdf" {
		t.Fatalf("local name = %q", name)
	}
}

func scholarCitationUnknown(id string) scholar.CitationCount {
	return scholar.CitationCount{ArXivID: id, Found: false}
}

func scholarCitationCount(id string, n *int) scholar.CitationCount {
	return scholar.CitationCount{ArXivID: id, Found: true, Count: n}
}

func TestSplitSliceRangeBuckets(t *testing.T) {
	from := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	to := time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC)
	days := splitSliceRange(from, to)
	if len(days) != 3 {
		t.Fatalf("day buckets = %d (%v)", len(days), days)
	}
	for i := 1; i < len(days); i++ {
		if !days[i].From.Equal(days[i-1].To) {
			t.Fatalf("gap between buckets %d: %v %v", i, days[i-1], days[i])
		}
	}
	// A single day splits into hours.
	hours := splitSliceRange(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	if len(hours) != 24 {
		t.Fatalf("hour buckets = %d", len(hours))
	}
	// An hour splits into minutes.
	minutes := splitSliceRange(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC))
	if len(minutes) != 60 {
		t.Fatalf("minute buckets = %d", len(minutes))
	}
	// Minute granularity cannot subdivide further: that is the source limit.
	if got := splitSliceRange(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 30, 0, time.UTC)); got != nil {
		t.Fatalf("minute range subdivided: %v", got)
	}
}

func TestOversizedSliceSubdividesAndCovers(t *testing.T) {
	s := testServer(t)
	// Two fixture papers spread over two days; the probe reports a huge total
	// so the slice subdivides, then each child covers.
	pages := map[string]string{}
	pages["0"] = strings.Replace(collectionSearchAtom, "<opensearch:totalResults>1</opensearch:totalResults>", "<opensearch:totalResults>5000</opensearch:totalResults>", 1)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("search_query")
		// Wide spans report the oversized total; subdivided buckets cover as
		// empty so the parent completes only through its children.
		if span, ok := querySpan(q); ok && span > 25*time.Hour {
			w.Write([]byte(pages["0"]))
			return
		}
		w.Write([]byte(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom" xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/"><opensearch:totalResults>0</opensearch:totalResults></feed>`))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	json.Unmarshal(w.Body.Bytes(), &run)
	waitRunStatus(t, s, run.ID, runSucceeded)
	// Parent slices recorded as split must have child slices.
	var parents int
	s.Store.DB.QueryRow("SELECT count(*) FROM collection_slices WHERE failure_class='split'").Scan(&parents)
	if parents == 0 {
		t.Fatal("oversized slice did not subdivide")
	}
	var children int
	s.Store.DB.QueryRow("SELECT count(*) FROM collection_slices WHERE failure_class='' AND status='completed'").Scan(&children)
	if children == 0 {
		t.Fatal("no child slice completed")
	}
}

// querySpan extracts the submittedDate window width from a built query.
func querySpan(q string) (time.Duration, bool) {
	i := strings.Index(q, "submittedDate:[")
	if i < 0 {
		return 0, false
	}
	rest := q[i+len("submittedDate:["):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return 0, false
	}
	parts := strings.SplitN(rest[:j], " TO ", 2)
	if len(parts) != 2 {
		return 0, false
	}
	from, err1 := time.Parse("200601021504", parts[0])
	to, err2 := time.Parse("200601021504", parts[1])
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return to.Sub(from), true
}
