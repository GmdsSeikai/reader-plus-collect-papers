package reader

import (
	"errors"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.DB.Close() })
	return store
}

func TestCollectionMigrationCreatesModuleTables(t *testing.T) {
	s := testStore(t)
	tables := []string{
		"collection_meta", "collection_settings", "collection_tasks", "collection_candidates",
		"collection_matches", "collection_runs", "collection_slices", "collection_citations",
		"collection_analyses", "collection_quota_days", "collection_attempts", "collection_cursors",
		"collection_imports", "import_commits",
	}
	for _, name := range tables {
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("missing table %s (n=%d err=%v)", name, n, err)
		}
	}
	var version int
	if err := s.DB.QueryRow("SELECT schema_version FROM collection_meta WHERE id=1").Scan(&version); err != nil || version != collectionSchemaVersion {
		t.Fatalf("module version = %d, %v", version, err)
	}
}

func TestCollectionMigrationIdempotentAndPreservesData(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB.Exec("INSERT INTO settings VALUES('reader','{}')"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateCollection(); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='reader'").Scan(&v); err != nil {
		t.Fatalf("existing library data lost: %v", err)
	}
	var version int
	if err := s.DB.QueryRow("SELECT schema_version FROM collection_meta WHERE id=1").Scan(&version); err != nil || version != collectionSchemaVersion {
		t.Fatalf("version after re-migration = %d, %v", version, err)
	}
}

func TestCollectionSchemaTooNewDisablesWrites(t *testing.T) {
	s := testStore(t)
	if _, err := s.DB.Exec("UPDATE collection_meta SET schema_version=? WHERE id=1", collectionSchemaVersion+3); err != nil {
		t.Fatal(err)
	}
	if err := s.CollectionWritable(); !errors.Is(err, errCollectionSchemaTooNew) {
		t.Fatalf("CollectionWritable() = %v, want errCollectionSchemaTooNew", err)
	}
	_, err := s.SaveCollectionTask(CollectionTask{Name: "t", Priority: 3, Enabled: true, Config: DefaultCollectionTaskConfig()}, -1)
	if !errors.Is(err, errCollectionSchemaTooNew) {
		t.Fatalf("SaveCollectionTask = %v, want schema-too-new", err)
	}
	// Reading existing library data must keep working.
	if _, err := s.Documents(); err != nil {
		t.Fatalf("library read blocked by collection version: %v", err)
	}
	// The module tables survive; nothing is rebuilt or dropped.
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='collection_tasks'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("collection_tasks dropped: n=%d err=%v", n, err)
	}
}

func TestCollectionTaskValidationAndRevision(t *testing.T) {
	s := testStore(t)
	cfg := DefaultCollectionTaskConfig()
	// Topic alone is not executable.
	cfg.ResearchTopic = "图像生成"
	if _, err := s.SaveCollectionTask(CollectionTask{Name: "n", Priority: 2, Enabled: true, Config: cfg}, -1); err == nil {
		t.Fatal("topic-only task must be rejected")
	}
	cfg.KeywordGroups = [][]string{{"object detection", "visual detection"}}
	task, err := s.SaveCollectionTask(CollectionTask{Name: "视觉", Priority: 2, Enabled: true, Config: cfg}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if task.QueryRevision != 1 || task.ID == "" || task.SemanticHash == "" {
		t.Fatalf("unexpected created task: %+v", task)
	}
	// Renaming keeps the query revision.
	cfg2 := cfg
	renamed := task
	renamed.Name = "改名"
	renamed.Config = cfg2
	got, err := s.SaveCollectionTask(renamed, task.QueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	if got.QueryRevision != 1 {
		t.Fatalf("rename raised revision to %d", got.QueryRevision)
	}
	// Changing keywords raises the revision.
	cfg3 := cfg2
	cfg3.KeywordGroups = [][]string{{"object detection"}, { "knowledge distillation"}}
	changed := got
	changed.Config = cfg3
	got2, err := s.SaveCollectionTask(changed, got.QueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	if got2.QueryRevision != 2 {
		t.Fatalf("semantic change kept revision %d", got2.QueryRevision)
	}
	// Stale revision is a conflict.
	if _, err := s.SaveCollectionTask(changed, 1); !errors.Is(err, errTaskConflict) {
		t.Fatalf("stale revision err = %v, want conflict", err)
	}
}

func TestCollectionTaskAuthorOnlyAllowed(t *testing.T) {
	s := testStore(t)
	cfg := DefaultCollectionTaskConfig()
	cfg.Authors = []CollectionTaskAuthor{{Name: "Ada Lovelace", Aliases: []string{"A. Lovelace"}}}
	if _, err := s.SaveCollectionTask(CollectionTask{Name: "作者", Priority: 1, Enabled: true, Config: cfg}, -1); err != nil {
		t.Fatalf("author-only task rejected: %v", err)
	}
	if cfg.WantsRelevance() {
		t.Fatal("author-only task must not want relevance")
	}
}

func TestCandidateContentHashStable(t *testing.T) {
	h1 := CandidateContentHash("T", "A  B", []string{"X", "Y"}, []string{"cs.AI"})
	h2 := CandidateContentHash("T", "A\n\tB", []string{"X", "Y"}, []string{"cs.AI"})
	if h1 != h2 {
		t.Fatal("whitespace normalization must not change the hash")
	}
	h3 := CandidateContentHash("T", "A B", []string{"Y", "X"}, []string{"cs.AI"})
	if h1 == h3 {
		t.Fatal("author order must change the hash")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length %d", len(h1))
	}
}

func TestNormalizeArXivID(t *testing.T) {
	cases := map[string]string{
		"2301.12345v3": "2301.12345",
		"arXiv:2301.12345": "2301.12345",
		"cs/0101001v2": "cs/0101001",
		"hep-th/9901001": "hep-th/9901001",
	}
	for in, want := range cases {
		got, err := NormalizeArXivID(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeArXivID(%q) = %q, %v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "not-an-id", "1234", "2301.123456789"} {
		if _, err := NormalizeArXivID(bad); err == nil {
			t.Fatalf("accepted bad id %q", bad)
		}
	}
}

func TestUpsertCandidateKeepsNewestVersion(t *testing.T) {
	s := testStore(t)
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	base := CollectionCandidate{
		ArXivID: "2301.12345", Title: "T2", Abstract: "B", Authors: []CollectionAuthorJSON{{Name: "X"}},
		Categories: []string{"cs.AI"}, PublishedAt: "2023-01-02T00:00:00Z", UpdatedAt: "2023-01-03T00:00:00Z",
		LatestVersion: 2, CanonicalVer: 1, ContentHash: "h2", FirstSeenAt: now(), LastSeenAt: now(),
	}
	if err := s.UpsertCandidate(tx, base); err != nil {
		t.Fatal(err)
	}
	stale := base
	stale.LatestVersion = 1
	stale.Title = "T1"
	stale.ContentHash = "h1"
	if err := s.UpsertCandidate(tx, stale); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var title string
	var version int
	if err := s.DB.QueryRow("SELECT title,latest_version FROM collection_candidates WHERE arxiv_id=?", base.ArXivID).Scan(&title, &version); err != nil {
		t.Fatal(err)
	}
	if title != "T2" || version != 2 {
		t.Fatalf("stale version overwrote candidate: %q v%d", title, version)
	}
}

func TestCollectionSettingsRoundTrip(t *testing.T) {
	s := testStore(t)
	cfg, err := s.CollectionSettings()
	if err != nil || cfg.DailyLimit != 100 || cfg.ConfigVersion != 1 {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	cfg.DailyLimit = 50
	cfg.AnalysisEnabled = true
	cfg.Paused = true
	if err := s.SaveCollectionSettings(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.CollectionSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyLimit != 50 || !got.AnalysisEnabled || !got.Paused || got.ConfigVersion != 2 {
		t.Fatalf("saved = %+v", got)
	}
}

func TestTaskConfigValidation(t *testing.T) {
	cfg := DefaultCollectionTaskConfig()
	cfg.KeywordGroups = [][]string{{"a"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.FrequencyHours = 5
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "频率") {
		t.Fatalf("bad frequency accepted: %v", err)
	}
	bad = cfg
	bad.Version = 2
	if err := bad.Validate(); err == nil {
		t.Fatal("future config version accepted")
	}
	bad = cfg
	bad.KeywordGroups = [][]string{{"  "}}
	if err := bad.Validate(); err == nil {
		t.Fatal("empty keyword group accepted")
	}
}
