package reader

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"reader.local/server/internal/scholar"
)

// Citation refresh policy: successful counts refresh after 7 days; failed
// attempts retry after 24 hours and keep the old value marked stale.
const (
	citationRefreshDays = 7
	citationRetryHours  = 24
	citationBatchSize   = 20
)

// UpsertCitation stores one verified batch result. A missing or invalid count
// keeps the previous successful value and marks it stale; an explicit zero is
// stored as zero; decreases are accepted.
func (s *Store) UpsertCitation(res scholar.CitationCount, nowTime time.Time) error {
	if err := s.CollectionWritable(); err != nil {
		return err
	}
	var prevCount sql.NullInt64
	var prevSuccess string
	err := s.DB.QueryRow("SELECT citation_count,COALESCE(success_at,'') FROM collection_citations WHERE arxiv_id=?", res.ArXivID).Scan(&prevCount, &prevSuccess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !res.Found || res.Count == nil || *res.Count < 0 {
		// Unknown now: keep any old number, mark stale when one existed,
		// and schedule a retry.
		if err == nil && (prevCount.Valid || prevSuccess != "") {
			_, err = s.DB.Exec("UPDATE collection_citations SET attempted_at=?,stale=1,error=?,refresh_at=? WHERE arxiv_id=?",
				nowTime.Format(time.RFC3339), "引用数量未知或无效", nowTime.Add(citationRetryHours*time.Hour).Format(time.RFC3339), res.ArXivID)
			return err
		}
		_, err = s.DB.Exec(`INSERT INTO collection_citations(arxiv_id,service_paper_id,citation_count,attempted_at,error,stale)
			VALUES(?,?,NULL,?,'',0)
			ON CONFLICT(arxiv_id) DO UPDATE SET attempted_at=excluded.attempted_at,refresh_at=?`,
			res.ArXivID, res.ServicePaperID, nowTime.Format(time.RFC3339), nowTime.Add(citationRetryHours*time.Hour).Format(time.RFC3339))
		return err
	}
	refresh := nowTime.Add(citationRefreshDays * 24 * time.Hour).Format(time.RFC3339)
	_, err = s.DB.Exec(`INSERT INTO collection_citations(arxiv_id,service_paper_id,citation_count,success_at,attempted_at,refresh_at,error,stale)
		VALUES(?,?,?,?,?,?,'',0)
		ON CONFLICT(arxiv_id) DO UPDATE SET service_paper_id=excluded.service_paper_id,citation_count=excluded.citation_count,success_at=excluded.success_at,attempted_at=excluded.attempted_at,refresh_at=excluded.refresh_at,error='',stale=0`,
		res.ArXivID, res.ServicePaperID, *res.Count, nowTime.Format(time.RFC3339), nowTime.Format(time.RFC3339), refresh)
	return err
}

// CitationDue describes one candidate whose citation refresh is due.
type CitationDue struct {
	ArXivID string
	Fresh   bool // never successfully fetched
}

// DueCitations alternates new candidates and expired refreshes, capped per
// round. Unknown counts remain visible; this only schedules refresh work.
func (s *Store) DueCitations(nowTime time.Time, limit int) ([]CitationDue, error) {
	if limit < 1 {
		limit = citationBatchSize
	}
	nowStr := nowTime.Format(time.RFC3339)
	rows, err := s.DB.Query(`SELECT c.arxiv_id,
		CASE WHEN x.arxiv_id IS NULL THEN 1 ELSE 0 END
		FROM collection_candidates c
		LEFT JOIN collection_citations x ON x.arxiv_id=c.arxiv_id
		WHERE x.arxiv_id IS NULL
		   OR x.refresh_at IS NULL
		   OR x.refresh_at <= ?
		ORDER BY CASE WHEN x.arxiv_id IS NULL THEN 0 ELSE 1 END, c.arxiv_id
		LIMIT ?`, nowStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CitationDue{}
	for rows.Next() {
		var d CitationDue
		var fresh int
		if err := rows.Scan(&d.ArXivID, &fresh); err != nil {
			return nil, err
		}
		d.Fresh = fresh != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// RefreshCitations fetches and stores one batch of due counts. Failures mark
// per-item retries and never block discovery, analysis or downloads.
func (m *CollectionManager) RefreshCitations(ctx context.Context, apiKey string) (int, error) {
	due, err := m.server.Store.DueCitations(time.Now(), citationBatchSize)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	ids := make([]string, 0, len(due))
	for _, d := range due {
		ids = append(ids, d.ArXivID)
	}
	release, err := m.s2Gate.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	results, err := m.server.scholarClient().CitationBatch(ctx, ids, apiKey)
	if err != nil {
		// Whole-batch failure: record per-item backoff.
		for _, id := range ids {
			_ = m.server.Store.UpsertCitation(scholar.CitationCount{ArXivID: id, Found: false}, time.Now())
		}
		return 0, err
	}
	saved := 0
	for _, id := range ids {
		if res, ok := results[id]; ok {
			if err := m.server.Store.UpsertCitation(res, time.Now()); err == nil {
				saved++
			}
		}
	}
	return saved, nil
}
