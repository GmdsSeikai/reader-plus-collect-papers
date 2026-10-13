package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// collectionMetaRow holds the module schema version. A newer on-disk version
// disables collection writes without rebuilding or dropping any table.
const collectionMetaID = 1

// CollectionSettings are module-level non-secret preferences. API keys stay in
// the existing protected configuration owner and are only reported as hasKey.
type CollectionSettings struct {
	AnalysisEnabled  bool   `json:"analysisEnabled"`
	AnalysisProvider string `json:"analysisProvider,omitempty"`
	AnalysisModel    string `json:"analysisModel,omitempty"`
	AnalysisEffort   string `json:"analysisEffort,omitempty"`
	CitationsAPIKey  string `json:"-"`
	DailyLimit       int    `json:"dailyLimit"`
	RequestRPM       int    `json:"requestRpm"`
	Paused           bool   `json:"paused"`
	ConfigVersion    int    `json:"configVersion"`
	HasCitationsKey  bool   `json:"hasCitationsKey"`
}

func defaultCollectionSettings() CollectionSettings {
	return CollectionSettings{
		DailyLimit:    100,
		RequestRPM:    0,
		ConfigVersion: 1,
	}
}

// collectionSchemaStatements are idempotent table definitions for module
// version 1. They live beside the library tables in reader.sqlite so a
// candidate-to-document commit can share one transaction.
var collectionSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS collection_meta(id INTEGER PRIMARY KEY CHECK(id=1),schema_version INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_settings(id INTEGER PRIMARY KEY CHECK(id=1),body TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_tasks(task_id TEXT PRIMARY KEY,name TEXT NOT NULL,enabled INTEGER NOT NULL,priority INTEGER NOT NULL CHECK(priority BETWEEN 1 AND 5),config_json TEXT NOT NULL,config_version INTEGER NOT NULL,query_revision INTEGER NOT NULL,semantic_hash TEXT NOT NULL,next_run_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_candidates(arxiv_id TEXT PRIMARY KEY,title TEXT NOT NULL,abstract TEXT NOT NULL,authors_json TEXT NOT NULL,categories_json TEXT NOT NULL,published_at TEXT NOT NULL,updated_at_src TEXT NOT NULL,latest_version INTEGER NOT NULL,doi TEXT NOT NULL DEFAULT '',journal_ref TEXT NOT NULL DEFAULT '',canonical_version INTEGER NOT NULL,content_hash TEXT NOT NULL,first_seen_at TEXT NOT NULL,last_seen_at TEXT NOT NULL,ignored INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS collection_matches(task_id TEXT NOT NULL REFERENCES collection_tasks(task_id) ON DELETE CASCADE,query_revision INTEGER NOT NULL,arxiv_id TEXT NOT NULL REFERENCES collection_candidates(arxiv_id) ON DELETE CASCADE,channel TEXT NOT NULL,branch_hash TEXT NOT NULL,evidence_json TEXT NOT NULL,rule_score INTEGER NOT NULL DEFAULT 0,ai_score INTEGER,analysis_cache_key TEXT NOT NULL DEFAULT '',first_seen_at TEXT NOT NULL,last_seen_at TEXT NOT NULL,PRIMARY KEY(task_id,query_revision,arxiv_id,channel,branch_hash))`,
	`CREATE TABLE IF NOT EXISTS collection_runs(run_id TEXT PRIMARY KEY,task_id TEXT NOT NULL,query_revision INTEGER NOT NULL,trigger_kind TEXT NOT NULL,config_snapshot TEXT NOT NULL,range_json TEXT NOT NULL,exec_token INTEGER NOT NULL,status TEXT NOT NULL,failure_class TEXT NOT NULL DEFAULT '',started_at TEXT,finished_at TEXT,created_at TEXT NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS collection_runs_open ON collection_runs(task_id) WHERE status IN ('queued','running')`,
	`CREATE TABLE IF NOT EXISTS collection_slices(slice_id TEXT PRIMARY KEY,run_id TEXT NOT NULL,task_id TEXT NOT NULL,query_revision INTEGER NOT NULL,channel TEXT NOT NULL,branch_hash TEXT NOT NULL,from_utc TEXT NOT NULL,to_utc TEXT NOT NULL,parent_id TEXT,query_text TEXT NOT NULL,page_offset INTEGER NOT NULL DEFAULT 0,total_results INTEGER NOT NULL DEFAULT -1,processed INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL,failure_class TEXT NOT NULL DEFAULT '',source_limit INTEGER NOT NULL DEFAULT 0,retry_at TEXT,checked_at TEXT,created_at TEXT NOT NULL,UNIQUE(task_id,query_revision,channel,branch_hash,from_utc,to_utc))`,
	`CREATE TABLE IF NOT EXISTS collection_citations(arxiv_id TEXT PRIMARY KEY REFERENCES collection_candidates(arxiv_id) ON DELETE CASCADE,service_paper_id TEXT NOT NULL DEFAULT '',citation_count INTEGER,success_at TEXT,attempted_at TEXT,refresh_at TEXT,error TEXT NOT NULL DEFAULT '',stale INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS collection_analyses(cache_key TEXT PRIMARY KEY,arxiv_id TEXT NOT NULL,content_hash TEXT NOT NULL,profile_hash TEXT NOT NULL,model_fingerprint TEXT NOT NULL,prompt_version INTEGER NOT NULL,result_version INTEGER NOT NULL,title_zh TEXT NOT NULL DEFAULT '',abstract_zh TEXT NOT NULL DEFAULT '',assessments_json TEXT NOT NULL DEFAULT '',status TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',attempt INTEGER NOT NULL DEFAULT 0,retry_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_quota_days(utc_day TEXT PRIMARY KEY,sent INTEGER NOT NULL DEFAULT 0,reserved INTEGER NOT NULL DEFAULT 0,uncertain INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS collection_attempts(attempt_id TEXT PRIMARY KEY,request_key TEXT NOT NULL,cache_key TEXT NOT NULL,reserved_day TEXT NOT NULL,status TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,sent_at TEXT,finished_at TEXT)`,
	`CREATE TABLE IF NOT EXISTS collection_cursors(owner_key TEXT PRIMARY KEY,body TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_imports(request_id TEXT PRIMARY KEY,arxiv_id TEXT NOT NULL,download_version INTEGER NOT NULL,metadata_snapshot TEXT NOT NULL DEFAULT '{}',status TEXT NOT NULL,expected_sha256 TEXT NOT NULL DEFAULT '',actual_sha256 TEXT NOT NULL DEFAULT '',cache_rel_path TEXT NOT NULL DEFAULT '',document_id TEXT NOT NULL DEFAULT '',library TEXT NOT NULL DEFAULT '',confirm_pages INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',"condition" TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS collection_imports_active ON collection_imports(arxiv_id) WHERE status NOT IN ('imported','failed','cancelled','interrupted')`,
	`CREATE TABLE IF NOT EXISTS import_commits(commit_id TEXT PRIMARY KEY,owner TEXT NOT NULL,owner_ref TEXT NOT NULL DEFAULT '',document_id TEXT NOT NULL DEFAULT '',library TEXT NOT NULL,filename TEXT NOT NULL,target_rel TEXT NOT NULL,full_sha256 TEXT NOT NULL,size INTEGER NOT NULL,stage TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS import_commits_open_target ON import_commits(target_rel) WHERE stage IN ('prepared','installed')`,
	`CREATE INDEX IF NOT EXISTS collection_candidates_published ON collection_candidates(published_at)`,
	`CREATE INDEX IF NOT EXISTS collection_matches_candidate ON collection_matches(arxiv_id)`,
	`CREATE INDEX IF NOT EXISTS collection_slices_retry ON collection_slices(status,retry_at)`,
	`CREATE INDEX IF NOT EXISTS collection_analyses_candidate ON collection_analyses(arxiv_id,status)`,
	`CREATE INDEX IF NOT EXISTS collection_attempts_day ON collection_attempts(reserved_day,status)`,
	`CREATE INDEX IF NOT EXISTS import_commits_stage ON import_commits(stage)`,
}

// migrateCollection creates the collection_* tables at module version 1 and
// refuses collection writes when the on-disk module is newer. A failed
// migration rolls back this attempt; already committed module tables stay.
func (s *Store) migrateCollection() error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The meta table must exist before its version can be probed on a fresh library.
	if _, err := tx.Exec(collectionSchemaStatements[0]); err != nil {
		return fmt.Errorf("采集表创建失败: %w", err)
	}
	var version int
	var raw sql.NullInt64
	err = tx.QueryRow("SELECT schema_version FROM collection_meta WHERE id=?", collectionMetaID).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First installation of the module.
	case err != nil:
		return err
	default:
		version = int(raw.Int64)
	}
	if version > collectionSchemaVersion {
		// Newer module data: keep every table, disable collection writes.
		return fmt.Errorf("%w: 磁盘版本 %d", errCollectionSchemaTooNew, version)
	}
	for _, stmt := range collectionSchemaStatements[1:] {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("采集表创建失败: %w", err)
		}
	}
	// Additive column repairs for module v1 databases created before the
	// import condition field existed.
	{
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('collection_imports') WHERE name='condition'`).Scan(&n); err == nil && n == 0 {
			if _, err := tx.Exec(`ALTER TABLE collection_imports ADD COLUMN "condition" TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("采集表迁移失败: %w", err)
			}
		}
	}
	if version == 0 {
		if _, err := tx.Exec("INSERT INTO collection_meta(id,schema_version,created_at,updated_at) VALUES(?,?,?,?)", collectionMetaID, collectionSchemaVersion, now(), now()); err != nil {
			return err
		}
	} else if version < collectionSchemaVersion {
		// Version 1 is the first module version; later steps append here inside
		// this transaction so a failure rolls the whole step back.
		if _, err := tx.Exec("UPDATE collection_meta SET schema_version=?,updated_at=? WHERE id=?", collectionSchemaVersion, now(), collectionMetaID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CollectionWritable reports whether the module accepts writes. A schema from
// a newer application version disables the module without touching data.
func (s *Store) CollectionWritable() error {
	var version int
	err := s.StoreVersionQuery(&version)
	if err != nil {
		return err
	}
	if version > collectionSchemaVersion {
		return errCollectionSchemaTooNew
	}
	return nil
}

// collectionWritableTx is the in-transaction form of CollectionWritable.
// Callers holding a transaction must use this one: the non-tx form queries
// through the single connection and would deadlock against the open tx.
func collectionWritableTx(tx *sql.Tx) error {
	var version int
	err := tx.QueryRow("SELECT schema_version FROM collection_meta WHERE id=?", collectionMetaID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if version > collectionSchemaVersion {
		return errCollectionSchemaTooNew
	}
	return nil
}

func (s *Store) StoreVersionQuery(version *int) error {
	err := s.DB.QueryRow("SELECT schema_version FROM collection_meta WHERE id=?", collectionMetaID).Scan(version)
	if errors.Is(err, sql.ErrNoRows) {
		*version = 0
		return nil
	}
	return err
}

// CollectionSettings returns the current module settings with defaults.
func (s *Store) CollectionSettings() (CollectionSettings, error) {
	cfg := defaultCollectionSettings()
	var body string
	err := s.DB.QueryRow("SELECT body FROM collection_settings WHERE id=?", collectionMetaID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		return defaultCollectionSettings(), fmt.Errorf("采集设置损坏: %w", err)
	}
	if cfg.DailyLimit < 0 || cfg.RequestRPM < 0 {
		return cfg, errors.New("采集额度配置无效")
	}
	return cfg, nil
}

// SaveCollectionSettings writes the module settings as one versioned document.
// Callers must not put API keys into this body.
func (s *Store) SaveCollectionSettings(cfg CollectionSettings) error {
	if cfg.DailyLimit < 0 || cfg.RequestRPM < 0 {
		return errors.New("额度和请求频率不能为负数")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	current, err := collectionSettingsTx(tx)
	if err != nil {
		return err
	}
	cfg.ConfigVersion = current.ConfigVersion + 1
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO collection_settings(id,body,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,updated_at=excluded.updated_at", collectionMetaID, string(b), now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SaveCollectionTask inserts or updates a task with optimistic revision.
// expectedRevision < 0 creates a new task; conflicts return errTaskConflict.
func (s *Store) SaveCollectionTask(t CollectionTask, expectedRevision int) (CollectionTask, error) {
	if err := s.CollectionWritable(); err != nil {
		return t, err
	}
	if err := t.Config.Validate(); err != nil {
		return t, err
	}
	if strings.TrimSpace(t.Name) == "" {
		return t, fmt.Errorf("%w: 任务名称不能为空", errInvalidTaskConfig)
	}
	if t.Priority < 1 || t.Priority > 5 {
		return t, fmt.Errorf("%w: 优先级须为 1–5", errInvalidTaskConfig)
	}
	t.SemanticHash = t.Config.SemanticHash()
	nowStr := now()
	if expectedRevision < 0 {
		if t.ID == "" {
			t.ID = id()
		}
		if t.QueryRevision < 1 {
			t.QueryRevision = 1
		}
		t.CreatedAt, t.UpdatedAt = nowStr, nowStr
		_, err := s.DB.Exec("INSERT INTO collection_tasks(task_id,name,enabled,priority,config_json,config_version,query_revision,semantic_hash,next_run_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
			t.ID, t.Name, boolInt(t.Enabled), t.Priority, mustJSONText(t.Config), t.Config.Version, t.QueryRevision, t.SemanticHash, nullIfEmpty(t.NextRunAt), t.CreatedAt, t.UpdatedAt)
		return t, err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	var currentRev int
	var currentHash string
	err = tx.QueryRow("SELECT query_revision,semantic_hash,created_at FROM collection_tasks WHERE task_id=?", t.ID).Scan(&currentRev, &currentHash, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: 任务不存在", errTaskConflict)
	}
	if err != nil {
		return t, err
	}
	if currentRev != expectedRevision {
		return t, errTaskConflict
	}
	revision := currentRev
	if currentHash != t.SemanticHash {
		revision = currentRev + 1
	}
	t.QueryRevision = revision
	t.UpdatedAt = nowStr
	res, err := tx.Exec("UPDATE collection_tasks SET name=?,enabled=?,priority=?,config_json=?,config_version=?,query_revision=?,semantic_hash=?,next_run_at=?,updated_at=? WHERE task_id=? AND query_revision=?",
		t.Name, boolInt(t.Enabled), t.Priority, mustJSONText(t.Config), t.Config.Version, revision, t.SemanticHash, nullIfEmpty(t.NextRunAt), t.UpdatedAt, t.ID, expectedRevision)
	if err != nil {
		return t, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return t, errTaskConflict
	}
	if err := tx.Commit(); err != nil {
		return t, err
	}
	return t, nil
}

// CollectionTasks lists all tasks ordered by priority then creation.
func (s *Store) CollectionTasks() ([]CollectionTask, error) {
	rows, err := s.DB.Query("SELECT task_id,name,enabled,priority,config_json,query_revision,semantic_hash,COALESCE(next_run_at,''),created_at,updated_at FROM collection_tasks ORDER BY priority,created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollectionTask{}
	for rows.Next() {
		var t CollectionTask
		var enabled int
		var raw string
		if err := rows.Scan(&t.ID, &t.Name, &enabled, &t.Priority, &raw, &t.QueryRevision, &t.SemanticHash, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		if err := json.Unmarshal([]byte(raw), &t.Config); err != nil {
			return nil, fmt.Errorf("任务配置损坏: %w", err)
		}
		if t.Config.Version != 1 {
			return nil, fmt.Errorf("任务配置版本 %d 不受支持", t.Config.Version)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CollectionTask loads one task by ID.
func (s *Store) CollectionTask(taskID string) (CollectionTask, error) {
	var t CollectionTask
	var enabled int
	var raw string
	err := s.DB.QueryRow("SELECT task_id,name,enabled,priority,config_json,query_revision,semantic_hash,COALESCE(next_run_at,''),created_at,updated_at FROM collection_tasks WHERE task_id=?", taskID).
		Scan(&t.ID, &t.Name, &enabled, &t.Priority, &raw, &t.QueryRevision, &t.SemanticHash, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	t.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(raw), &t.Config); err != nil {
		return t, fmt.Errorf("任务配置损坏: %w", err)
	}
	return t, nil
}

// UpsertCandidate commits a scan page's candidate row, keeping the newest
// source info. A late response for an older version never lowers latest_version
// or overwrites newer content.
func (s *Store) UpsertCandidate(tx *sql.Tx, c CollectionCandidate) error {
	var currentVersion int
	var currentHash string
	err := tx.QueryRow("SELECT latest_version,content_hash FROM collection_candidates WHERE arxiv_id=?", c.ArXivID).Scan(&currentVersion, &currentHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO collection_candidates(arxiv_id,title,abstract,authors_json,categories_json,published_at,updated_at_src,latest_version,doi,journal_ref,canonical_version,content_hash,first_seen_at,last_seen_at,ignored)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,0)`,
			c.ArXivID, c.Title, c.Abstract, mustJSONText(c.Authors), mustJSONText(c.Categories), c.PublishedAt, c.UpdatedAt, c.LatestVersion, c.DOI, c.JournalRef, c.CanonicalVer, c.ContentHash, c.FirstSeenAt, c.LastSeenAt)
		return err
	case err != nil:
		return err
	}
	if c.LatestVersion < currentVersion {
		// Stale response: keep the newer record, only refresh last-seen time.
		_, err = tx.Exec("UPDATE collection_candidates SET last_seen_at=? WHERE arxiv_id=?", c.LastSeenAt, c.ArXivID)
		return err
	}
	_, err = tx.Exec(`UPDATE collection_candidates SET title=?,abstract=?,authors_json=?,categories_json=?,published_at=?,updated_at_src=?,latest_version=?,doi=?,journal_ref=?,canonical_version=?,content_hash=?,last_seen_at=? WHERE arxiv_id=?`,
		c.Title, c.Abstract, mustJSONText(c.Authors), mustJSONText(c.Categories), c.PublishedAt, c.UpdatedAt, c.LatestVersion, c.DOI, c.JournalRef, c.CanonicalVer, c.ContentHash, c.LastSeenAt, c.ArXivID)
	return err
}

// UpsertMatch merges one discovery relationship, keeping all evidence phrases.
func (s *Store) UpsertMatch(tx *sql.Tx, m CollectionMatch) error {
	var existing string
	err := tx.QueryRow("SELECT evidence_json FROM collection_matches WHERE task_id=? AND query_revision=? AND arxiv_id=? AND channel=? AND branch_hash=?", m.TaskID, m.QueryRevision, m.ArXivID, m.Channel, m.BranchHash).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec("INSERT INTO collection_matches(task_id,query_revision,arxiv_id,channel,branch_hash,evidence_json,rule_score,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?,?)",
			m.TaskID, m.QueryRevision, m.ArXivID, m.Channel, m.BranchHash, mustJSONText(m.Evidence), m.RuleScore, m.FirstSeenAt, m.LastSeenAt)
		return err
	case err != nil:
		return err
	}
	var old []CollectionEvidence
	_ = json.Unmarshal([]byte(existing), &old)
	merged := mergeEvidence(old, m.Evidence)
	_, err = tx.Exec("UPDATE collection_matches SET evidence_json=?,rule_score=?,last_seen_at=? WHERE task_id=? AND query_revision=? AND arxiv_id=? AND channel=? AND branch_hash=?",
		mustJSONText(merged), m.RuleScore, m.LastSeenAt, m.TaskID, m.QueryRevision, m.ArXivID, m.Channel, m.BranchHash)
	return err
}

func mergeEvidence(a, b []CollectionEvidence) []CollectionEvidence {
	seen := map[string]bool{}
	out := make([]CollectionEvidence, 0, len(a)+len(b))
	for _, list := range [][]CollectionEvidence{a, b} {
		for _, e := range list {
			k := e.Field + "\x00" + e.Quote
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, e)
		}
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSONText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// utcDay returns the YYYY-MM-DD UTC day used for quota accounting.
func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }
