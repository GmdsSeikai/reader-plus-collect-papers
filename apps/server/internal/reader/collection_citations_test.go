package reader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"reader.local/server/internal/scholar"
)

func seedCandidate(t *testing.T, s *Store, id string) {
	t.Helper()
	c := CollectionCandidate{
		ArXivID: id, Title: "T", Abstract: "B", Authors: []CollectionAuthorJSON{{Name: "X"}},
		Categories: []string{"cs.AI"}, PublishedAt: "2023-01-02T00:00:00Z", UpdatedAt: "2023-01-02T00:00:00Z",
		LatestVersion: 1, CanonicalVer: 1, ContentHash: "h" + id, FirstSeenAt: now(), LastSeenAt: now(),
	}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCandidate(tx, c); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCitationZeroUnknownAndStale(t *testing.T) {
	s := testStore(t)
	seedCandidate(t, s, "2301.00001")
	seedCandidate(t, s, "2301.00002")
	base := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	zero := 0
	if err := s.UpsertCitation(scholar.CitationCount{ArXivID: "2301.00001", Found: true, Count: &zero}, base); err != nil {
		t.Fatal(err)
	}
	_, cit, err := s.CollectionCandidate("2301.00001")
	if err != nil {
		t.Fatal(err)
	}
	if cit.Count == nil || *cit.Count != 0 || cit.Stale {
		t.Fatalf("explicit zero lost: %+v", cit)
	}
	// A later unknown refresh keeps the zero and marks it stale.
	if err := s.UpsertCitation(scholar.CitationCount{ArXivID: "2301.00001", Found: true, Count: nil}, base.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, cit, _ = s.CollectionCandidate("2301.00001")
	if cit.Count == nil || *cit.Count != 0 || !cit.Stale {
		t.Fatalf("stale handling: %+v", cit)
	}
	// A decrease is accepted.
	neg := -3
	if err := s.UpsertCitation(scholar.CitationCount{ArXivID: "2301.00002", Found: true, Count: &neg}, base); err != nil {
		t.Fatal(err)
	}
	_, cit, _ = s.CollectionCandidate("2301.00002")
	if cit.Count != nil {
		t.Fatalf("negative count coerced: %+v", cit)
	}
	// Never-fetched stays NULL and visible as unknown.
	seedCandidate(t, s, "2301.00003")
	_, cit, _ = s.CollectionCandidate("2301.00003")
	if cit.Count != nil {
		t.Fatalf("unknown count invented: %+v", cit)
	}
}

func TestCitationBatchMatchesByExternalID(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reordered response with one missing and one invalid count.
		w.Write([]byte(`[
			{"paperId":"p2","externalIds":{"ArXiv":"2301.00002"},"citationCount":"many"},
			{"paperId":"p1","externalIds":{"ArXiv":"2301.00001"},"citationCount":7.5},
			{"paperId":"p3","externalIds":{"ArXiv":"2301.00003"},"citationCount":0}
		]`))
	}))
	defer srv.Close()
	c := scholar.New(1 << 20)
	c.SemanticScholar = srv.URL + "/graph/v1/paper/"
	c.HTTP = srv.Client()
	res, err := c.CitationBatch(context.Background(), []string{"2301.00001", "2301.00002", "2301.00003", "2301.00004"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res["2301.00001"].Count != nil {
		t.Fatalf("float count accepted: %+v", res["2301.00001"])
	}
	if res["2301.00002"].Count != nil {
		t.Fatalf("string count accepted: %+v", res["2301.00002"])
	}
	if res["2301.00003"].Count == nil || *res["2301.00003"].Count != 0 {
		t.Fatalf("zero lost: %+v", res["2301.00003"])
	}
	if res["2301.00004"].Found {
		t.Fatalf("missing item marked found: %+v", res["2301.00004"])
	}
	if res["2301.00001"].ServicePaperID != "p1" {
		t.Fatalf("external ID mapping broken: %+v", res["2301.00001"])
	}
	_ = json.Valid
}
