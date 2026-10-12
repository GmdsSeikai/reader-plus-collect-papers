package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OpenCollectionRun returns the task's open run if one exists.
func (s *Store) OpenCollectionRun(taskID string) (CollectionRun, error) {
	var r CollectionRun
	var config, rangeJSON string
	err := s.DB.QueryRow("SELECT run_id,task_id,query_revision,trigger_kind,config_snapshot,range_json,exec_token,status,failure_class,COALESCE(started_at,''),COALESCE(finished_at,'') FROM collection_runs WHERE task_id=? AND status IN ('queued','running')", taskID).
		Scan(&r.ID, &r.TaskID, &r.QueryRevision, &r.Trigger, &config, &rangeJSON, &r.ExecToken, &r.Status, &r.FailureClass, &r.StartedAt, &r.FinishedAt)
	if err != nil {
		return r, err
	}
	r.ConfigSnapshot = json.RawMessage(config)
	r.RangeJSON = json.RawMessage(rangeJSON)
	return r, nil
}

// CreateCollectionRun inserts a run with a fresh exec token. A second open run
// for the task violates the partial unique index instead of double-running.
func (s *Store) CreateCollectionRun(t CollectionTask, trigger, rangeJSON string) (CollectionRun, error) {
	if err := s.CollectionWritable(); err != nil {
		return CollectionRun{}, err
	}
	r := CollectionRun{
		ID:             id(),
		TaskID:         t.ID,
		QueryRevision:  t.QueryRevision,
		Trigger:        trigger,
		ConfigSnapshot: mustJSON(t.Config),
		RangeJSON:      json.RawMessage(rangeJSON),
		ExecToken:      time.Now().UnixNano(),
		Status:         runRunning,
		StartedAt:      now(),
	}
	_, err := s.DB.Exec(`INSERT INTO collection_runs(run_id,task_id,query_revision,trigger_kind,config_snapshot,range_json,exec_token,status,failure_class,started_at,finished_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'',NULL,?)`,
		r.ID, r.TaskID, r.QueryRevision, r.Trigger, string(r.ConfigSnapshot), rangeJSON, r.ExecToken, r.Status, "", now())
	return r, err
}

// FinishCollectionRun records the terminal state unless the row already left
// the open states.
func (s *Store) FinishCollectionRun(runID, status, failureClass string) error {
	if !validRunStatus(status) {
		return fmt.Errorf("未知运行状态 %q", status)
	}
	_, err := s.DB.Exec("UPDATE collection_runs SET status=?,failure_class=?,finished_at=? WHERE run_id=? AND status IN ('queued','running')",
		status, failureClass, now(), runID)
	return err
}

// BumpCollectionExecToken invalidates late responses from cancelled runs.
func (s *Store) BumpCollectionExecToken(taskID string) error {
	_, err := s.DB.Exec("UPDATE collection_runs SET exec_token=exec_token+1 WHERE task_id=? AND status IN ('queued','running')", taskID)
	return err
}

// CollectionRunToken reads the current token for a run.
func (s *Store) CollectionRunToken(runID string) (int64, error) {
	var token int64
	err := s.DB.QueryRow("SELECT exec_token FROM collection_runs WHERE run_id=?", runID).Scan(&token)
	return token, err
}

// UpsertSlice records a scan slice; resuming keeps its page checkpoint.
func (s *Store) UpsertSlice(tx *sql.Tx, sl CollectionSlice, runID string, revision int) error {
	if !validSliceStatus(sl.Status) {
		return fmt.Errorf("未知切片状态 %q", sl.Status)
	}
	var sliceID string
	err := tx.QueryRow("SELECT slice_id FROM collection_slices WHERE task_id=? AND query_revision=? AND channel=? AND branch_hash=? AND from_utc=? AND to_utc=?",
		sl.TaskID, revision, sl.Channel, sl.BranchHash, sl.FromUTC, sl.ToUTC).Scan(&sliceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO collection_slices(slice_id,run_id,task_id,query_revision,channel,branch_hash,from_utc,to_utc,parent_id,query_text,page_offset,total_results,processed,status,failure_class,source_limit,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id(), runID, sl.TaskID, revision, sl.Channel, sl.BranchHash, sl.FromUTC, sl.ToUTC, nullIfEmpty(sl.ParentID), sl.Query, sl.PageOffset, sl.TotalResults, sl.Processed, sl.Status, sl.FailureClass, boolInt(sl.SourceLimit), now())
		return err
	case err != nil:
		return err
	}
	_, err = tx.Exec(`UPDATE collection_slices SET page_offset=?,total_results=?,processed=?,status=?,failure_class=?,source_limit=?,retry_at=?,checked_at=?,run_id=? WHERE slice_id=?`,
		sl.PageOffset, sl.TotalResults, sl.Processed, sl.Status, sl.FailureClass, boolInt(sl.SourceLimit), nullIfEmpty(sl.RetryAt), nullIfEmpty(sl.CheckedAt), runID, sliceID)
	return err
}

// CommitScanPage saves one discovered page: candidates, matches and the slice
// checkpoint advance in the same transaction. Nothing outside this function
// may advance a checkpoint. A stale exec token rejects the whole page.
func (s *Store) CommitScanPage(tx *sql.Tx, sl CollectionSlice, revision int, runToken int64, runID string, candidates []CollectionCandidate, matches []CollectionMatch) error {
	var token int64
	if err := tx.QueryRow("SELECT exec_token FROM collection_runs WHERE run_id=?", runID).Scan(&token); err != nil {
		return err
	}
	if token != runToken {
		return errors.New("运行已被取消或替换，响应不再有效")
	}
	for _, c := range candidates {
		if err := s.UpsertCandidate(tx, c); err != nil {
			return err
		}
	}
	for _, m := range matches {
		if err := s.UpsertMatch(tx, m); err != nil {
			return err
		}
	}
	sl.PageOffset++
	sl.Processed += len(candidates)
	return s.UpsertSlice(tx, sl, runID, revision)
}

// CandidateFilter selects candidate list pages.
type CandidateFilter struct {
	CursorDate string
	CursorID   string
	CursorNum  int
	TaskID     string
	Search     string
	Citation   string
	Sort       string
	Limit      int
	Ignored    *bool
}

// CollectionCandidates returns one keyset page of candidates with the caller's
// best effective score. Sorting uses the stable arXiv ID as tie-breaker.
func (s *Store) CollectionCandidates(f CandidateFilter) ([]CollectionCandidate, []int, string, error) {
	if f.Limit < 1 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Sort == "" {
		f.Sort = "date"
	}
	args := []any{}
	join := "FROM collection_candidates c"
	where := []string{"1=1"}
	if f.TaskID != "" {
		join += " JOIN collection_matches m ON m.arxiv_id=c.arxiv_id AND m.task_id=?"
		args = append(args, f.TaskID)
	}
	if f.Ignored != nil {
		where = append(where, "c.ignored=?")
		args = append(args, boolInt(*f.Ignored))
	}
	if f.Search != "" {
		where = append(where, "(c.title LIKE ? ESCAPE '\\' OR c.abstract LIKE ? ESCAPE '\\' OR c.authors_json LIKE ? ESCAPE '\\')")
		like := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(f.Search) + "%"
		args = append(args, like, like, like)
	}
	if f.Citation == "known" {
		where = append(where, "(SELECT citation_count FROM collection_citations x WHERE x.arxiv_id=c.arxiv_id) IS NOT NULL")
	} else if f.Citation == "unknown" {
		where = append(where, "(SELECT citation_count FROM collection_citations x WHERE x.arxiv_id=c.arxiv_id) IS NULL")
	}
	scoreCol := "(SELECT MAX(COALESCE(m2.ai_score,m2.rule_score)) FROM collection_matches m2 WHERE m2.arxiv_id=c.arxiv_id)"
	citCol := "(SELECT citation_count FROM collection_citations x WHERE x.arxiv_id=c.arxiv_id)"
	order := "c.published_at DESC, c.arxiv_id DESC"
	keyCol := "c.published_at"
	switch f.Sort {
	case "citations":
		order = citCol + " DESC, c.published_at DESC, c.arxiv_id DESC"
		keyCol = citCol
	case "relevance":
		order = scoreCol + " DESC, c.published_at DESC, c.arxiv_id DESC"
		keyCol = scoreCol
	}
	if f.CursorID != "" {
		switch f.Sort {
		case "citations", "relevance":
			where = append(where, fmt.Sprintf("(COALESCE(%s,-1) < ? OR (COALESCE(%s,-1) = ? AND (c.published_at < ? OR (c.published_at = ? AND c.arxiv_id < ?))))", keyCol, keyCol))
			args = append(args, f.CursorNum, f.CursorNum, f.CursorDate, f.CursorDate, f.CursorID)
		default:
			where = append(where, "(c.published_at < ? OR (c.published_at = ? AND c.arxiv_id < ?))")
			args = append(args, f.CursorDate, f.CursorDate, f.CursorID)
		}
	}
	query := fmt.Sprintf(`SELECT c.arxiv_id,c.title,c.abstract,c.authors_json,c.categories_json,c.published_at,c.updated_at_src,c.latest_version,c.doi,c.journal_ref,c.canonical_version,c.content_hash,c.first_seen_at,c.last_seen_at,c.ignored,
		COALESCE(%s,-1) %s WHERE %s ORDER BY %s LIMIT ?`, scoreCol, join, strings.Join(where, " AND "), order)
	args = append(args, f.Limit+1)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	out := []CollectionCandidate{}
	scores := []int{}
	for rows.Next() {
		var c CollectionCandidate
		var authors, cats string
		var ignored int
		var score int
		if err := rows.Scan(&c.ArXivID, &c.Title, &c.Abstract, &authors, &cats, &c.PublishedAt, &c.UpdatedAt, &c.LatestVersion, &c.DOI, &c.JournalRef, &c.CanonicalVer, &c.ContentHash, &c.FirstSeenAt, &c.LastSeenAt, &ignored, &score); err != nil {
			return nil, nil, "", err
		}
		c.Ignored = ignored != 0
		_ = json.Unmarshal([]byte(authors), &c.Authors)
		_ = json.Unmarshal([]byte(cats), &c.Categories)
		out = append(out, c)
		scores = append(scores, score)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		last := out[f.Limit-1]
		next = fmt.Sprintf("%s|%s|%d", last.PublishedAt, last.ArXivID, scores[f.Limit-1])
		out = out[:f.Limit]
		scores = scores[:f.Limit]
	}
	return out, scores, next, nil
}

// CollectionCandidate loads one candidate with its citation state.
func (s *Store) CollectionCandidate(arxivID string) (CollectionCandidate, CollectionCitation, error) {
	var c CollectionCandidate
	var authors, cats string
	var ignored int
	err := s.DB.QueryRow("SELECT arxiv_id,title,abstract,authors_json,categories_json,published_at,updated_at_src,latest_version,doi,journal_ref,canonical_version,content_hash,first_seen_at,last_seen_at,ignored FROM collection_candidates WHERE arxiv_id=?", arxivID).
		Scan(&c.ArXivID, &c.Title, &c.Abstract, &authors, &cats, &c.PublishedAt, &c.UpdatedAt, &c.LatestVersion, &c.DOI, &c.JournalRef, &c.CanonicalVer, &c.ContentHash, &c.FirstSeenAt, &c.LastSeenAt, &ignored)
	if err != nil {
		return c, CollectionCitation{}, err
	}
	c.Ignored = ignored != 0
	_ = json.Unmarshal([]byte(authors), &c.Authors)
	_ = json.Unmarshal([]byte(cats), &c.Categories)
	cit := CollectionCitation{ArXivID: arxivID}
	var count sql.NullInt64
	var stale int
	err = s.DB.QueryRow("SELECT citation_count,service_paper_id,COALESCE(success_at,''),COALESCE(attempted_at,''),error,stale FROM collection_citations WHERE arxiv_id=?", arxivID).
		Scan(&count, &cit.ServicePaperID, &cit.SuccessAt, &cit.AttemptedAt, &cit.Error, &stale)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, cit, err
	}
	if err == nil {
		if count.Valid {
			v := int(count.Int64)
			cit.Count = &v
		}
		cit.Stale = stale != 0
	}
	return c, cit, nil
}

// SetCandidateIgnored flips the global ignored marker.
func (s *Store) SetCandidateIgnored(arxivID string, ignored bool) error {
	_, err := s.DB.Exec("UPDATE collection_candidates SET ignored=? WHERE arxiv_id=?", boolInt(ignored), arxivID)
	return err
}

// FindSlice locates a slice by its coverage key.
func (s *Store) FindSlice(taskID string, revision int, channel, branchHash, fromUTC, toUTC string) (CollectionSlice, error) {
	var sl CollectionSlice
	var parent sql.NullString
	var limit int
	err := s.DB.QueryRow(`SELECT slice_id,task_id,channel,branch_hash,from_utc,to_utc,COALESCE(parent_id,''),query_text,page_offset,total_results,processed,status,failure_class,source_limit,COALESCE(retry_at,''),COALESCE(checked_at,'')
		FROM collection_slices WHERE task_id=? AND query_revision=? AND channel=? AND branch_hash=? AND from_utc=? AND to_utc=?`,
		taskID, revision, channel, branchHash, fromUTC, toUTC).
		Scan(&sl.ID, &sl.TaskID, &sl.Channel, &sl.BranchHash, &sl.FromUTC, &sl.ToUTC, &parent, &sl.Query, &sl.PageOffset, &sl.TotalResults, &sl.Processed, &sl.Status, &sl.FailureClass, &limit, &sl.RetryAt, &sl.CheckedAt)
	sl.ParentID = parent.String
	sl.SourceLimit = limit != 0
	return sl, err
}

// InsertCollectionImport registers one pinned-version import request.
func (s *Store) InsertCollectionImport(req CollectionImportRequest) error {
	if !validImportStatus(req.Status) {
		return fmt.Errorf("未知导入状态 %q", req.Status)
	}
	_, err := s.DB.Exec(`INSERT INTO collection_imports(request_id,arxiv_id,download_version,metadata_snapshot,status,expected_sha256,actual_sha256,cache_rel_path,document_id,library,confirm_pages,error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.RequestID, req.ArXivID, req.DownloadVersion, string(req.MetadataSnapshot), req.Status, req.ExpectedSHA256, req.ActualSHA256, req.CacheRelPath, req.DocumentID, req.Library, req.ConfirmPages, req.Error, req.CreatedAt, req.UpdatedAt)
	return err
}

// ActiveCollectionImport returns the open request for a paper, if any.
func (s *Store) ActiveCollectionImport(arxivID string) (CollectionImportRequest, error) {
	var req CollectionImportRequest
	var snapshot string
	err := s.DB.QueryRow(`SELECT request_id,arxiv_id,download_version,metadata_snapshot,status,expected_sha256,actual_sha256,cache_rel_path,document_id,library,confirm_pages,error,created_at,updated_at
		FROM collection_imports WHERE arxiv_id=? AND status NOT IN ('imported','failed','cancelled','interrupted')`, arxivID).
		Scan(&req.RequestID, &req.ArXivID, &req.DownloadVersion, &snapshot, &req.Status, &req.ExpectedSHA256, &req.ActualSHA256, &req.CacheRelPath, &req.DocumentID, &req.Library, &req.ConfirmPages, &req.Error, &req.CreatedAt, &req.UpdatedAt)
	req.MetadataSnapshot = json.RawMessage(snapshot)
	return req, err
}

// CollectionImport loads one import request by id.
func (s *Store) CollectionImport(requestID string) (CollectionImportRequest, error) {
	var req CollectionImportRequest
	var snapshot string
	err := s.DB.QueryRow(`SELECT request_id,arxiv_id,download_version,metadata_snapshot,status,expected_sha256,actual_sha256,cache_rel_path,document_id,library,confirm_pages,error,created_at,updated_at
		FROM collection_imports WHERE request_id=?`, requestID).
		Scan(&req.RequestID, &req.ArXivID, &req.DownloadVersion, &snapshot, &req.Status, &req.ExpectedSHA256, &req.ActualSHA256, &req.CacheRelPath, &req.DocumentID, &req.Library, &req.ConfirmPages, &req.Error, &req.CreatedAt, &req.UpdatedAt)
	req.MetadataSnapshot = json.RawMessage(snapshot)
	return req, err
}

// UpdateCollectionImport applies status/detail fields and refreshes the
// timestamp. Unknown status codes are rejected instead of coerced.
func (s *Store) UpdateCollectionImport(requestID string, fields map[string]any) error {
	if status, ok := fields["status"].(string); ok && !validImportStatus(status) {
		return fmt.Errorf("未知导入状态 %q", status)
	}
	sets := []string{}
	args := []any{}
	for _, key := range []string{"status", "error", "actual_sha256", "cache_rel_path", "document_id", "library", "confirm_pages", "expected_sha256"} {
		if v, ok := fields[key]; ok {
			sets = append(sets, key+"=?")
			args = append(args, v)
		}
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), requestID)
	_, err := s.DB.Exec("UPDATE collection_imports SET "+strings.Join(sets, ", ")+" WHERE request_id=?", args...)
	return err
}
