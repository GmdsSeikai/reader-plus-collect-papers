package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedOpenCommit inserts an import_commits row and returns its commit id.
func seedOpenCommit(t *testing.T, s *Server, stage, owner, ownerRef, filename, fullSHA string, size int64) string {
	t.Helper()
	docID := fullSHA[:32]
	target := importTargetRel(docID, filename)
	commitID := id()
	_, err := s.Store.DB.Exec(`INSERT INTO import_commits(commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage,error,created_at,updated_at)
		VALUES(?,?,?,'','papers',?,?,?,?,?,'',?,?)`,
		commitID, owner, ownerRef, filename, target, fullSHA, size, stage, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	return commitID
}

func writeTargetFile(t *testing.T, s *Server, filename, fullSHA string, data []byte) string {
	t.Helper()
	dest := filepath.Join(s.Store.Root, filepath.FromSlash(importTargetRel(fullSHA[:32], filename)))
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, data, 0600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func commitStage(t *testing.T, s *Server, commitID string) string {
	t.Helper()
	var stage string
	if err := s.Store.DB.QueryRow("SELECT stage FROM import_commits WHERE commit_id=?", commitID).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	return stage
}

// TestRecoverOpenRunsMarksInterrupted proves a crash-opened run is closed at
// startup and its exec token bumped so late responses are dead.
func TestRecoverOpenRunsMarksInterrupted(t *testing.T) {
	s := testServer(t)
	task := newScanFixture(t, s)
	run, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	tokenBefore, _ := s.Store.CollectionRunToken(run.ID)
	if err := s.Store.recoverOpenRuns(); err != nil {
		t.Fatal(err)
	}
	var status string
	s.Store.DB.QueryRow("SELECT status FROM collection_runs WHERE run_id=?", run.ID).Scan(&status)
	if status != runInterrupted {
		t.Fatalf("status = %q, want interrupted", status)
	}
	tokenAfter, _ := s.Store.CollectionRunToken(run.ID)
	if tokenAfter <= tokenBefore {
		t.Fatalf("exec token not bumped: %d -> %d", tokenBefore, tokenAfter)
	}
	// Idempotent on double restart.
	if err := s.Store.recoverOpenRuns(); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverInstalledCommitResumes proves an installed row with a verified
// file resumes its commit into the document table, linked and idempotent.
func TestRecoverInstalledCommitResumes(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	commitID := seedOpenCommit(t, s, importStageInstalled, "upload", "", "paper.pdf", full, int64(len(data)))
	writeTargetFile(t, s, "paper.pdf", full, data)
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commitID); stage != importStageCommitted {
		t.Fatalf("stage = %q, want committed", stage)
	}
	docID := full[:32]
	if _, err := s.Store.Document(docID); err != nil {
		t.Fatalf("document not restored: %v", err)
	}
	// Double restart is a no-op.
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commitID); stage != importStageCommitted {
		t.Fatalf("stage after second restart = %q", stage)
	}
	var docs int
	s.Store.DB.QueryRow("SELECT count(*) FROM documents").Scan(&docs)
	if docs != 1 {
		t.Fatalf("duplicate document on double restart: %d", docs)
	}
}

// TestRecoverPreparedWithCacheKeepsRetryable proves a prepared row with a
// verified cache copy and no target file stays retryable, and one without any
// recoverable copy closes as failed.
func TestRecoverPreparedWithCacheKeepsRetryable(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	// With a verified cache copy: missing target is repaired and committed.
	commitID := seedOpenCommit(t, s, importStagePrepared, "collection", "req-1", "2301.00001v1.pdf", full, int64(len(data)))
	cacheDir := filepath.Join(s.Store.Root, "cache", "paper-collection", "req-1")
	os.MkdirAll(cacheDir, 0700)
	os.WriteFile(filepath.Join(cacheDir, "2301.00001v1.pdf"), data, 0600)
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commitID); stage != importStageCommitted {
		t.Fatalf("cache-repaired stage = %q, want committed", stage)
	}
	if _, err := os.Stat(filepath.Join(s.Store.Root, filepath.FromSlash(importTargetRel(full[:32], "2301.00001v1.pdf")))); err != nil {
		t.Fatalf("official file not repaired: %v", err)
	}
	// Without any copy: closed as failed, kept out of the open-target index.
	full2 := stringsRepeatHex("ab", 32)
	commit2 := seedOpenCommit(t, s, importStagePrepared, "upload", "", "lost.pdf", full2, 10)
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commit2); stage != importStageFailed {
		t.Fatalf("lost row stage = %q, want failed", stage)
	}
}

// TestRecoverMismatchedTargetKeepsUnknownFile proves a target holding unknown
// bytes is never overwritten and the row closes without touching it.
func TestRecoverMismatchedTargetKeepsUnknownFile(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	commitID := seedOpenCommit(t, s, importStageInstalled, "upload", "", "paper.pdf", full, int64(len(data)))
	dest := writeTargetFile(t, s, "paper.pdf", full, []byte("%PDF-1.4 unknown foreign bytes"))
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commitID); stage != importStageFailed {
		t.Fatalf("stage = %q, want failed", stage)
	}
	kept, err := os.ReadFile(dest)
	if err != nil || string(kept) != "%PDF-1.4 unknown foreign bytes" {
		t.Fatalf("unknown file altered: %q %v", kept, err)
	}
}

// TestRecoverNeverRestoresTrash proves recovery links but does not restore a
// document sitting in the trash.
func TestRecoverNeverRestoresTrash(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	up := uploadTo(t, s, "papers", "paper.pdf", data)
	if up.Code != 201 {
		t.Fatalf("seed = %d", up.Code)
	}
	var doc Document
	json.Unmarshal(up.Body.Bytes(), &doc)
	// Move to trash via the production endpoint.
	if w := request(t, s, "POST", "/api/documents/"+doc.ID+"/trash", nil); w.Code != 200 {
		t.Fatalf("trash = %d", w.Code)
	}
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	commitID := seedOpenCommit(t, s, importStageInstalled, "upload", "", "paper.pdf", full, int64(len(data)))
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if stage := commitStage(t, s, commitID); stage != importStageCommitted {
		t.Fatalf("stage = %q", stage)
	}
	d, err := s.Store.Document(doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.DeletedAt == "" {
		t.Fatal("recovery silently restored a trashed document")
	}
}

// TestSweepInterruptedImports proves mid-preparation requests become
// interrupted while user-decision states stay untouched.
func TestSweepInterruptedImports(t *testing.T) {
	s := testServer(t)
	mk := func(rid, status string) CollectionImportRequest {
		req := CollectionImportRequest{RequestID: rid, ArXivID: "2301." + strings.ReplaceAll(rid, "-", ""), DownloadVersion: 1, Status: status, CreatedAt: now(), UpdatedAt: now()}
		if err := s.Store.InsertCollectionImport(req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	mk("r-queued", importQueued)
	mk("r-downloading", importDownloading)
	mk("r-awaiting", importAwaitingConfirmation)
	mk("r-downloaded", importDownloaded)
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	st := func(rid string) string {
		var status string
		s.Store.DB.QueryRow("SELECT status FROM collection_imports WHERE request_id=?", rid).Scan(&status)
		return status
	}
	if st("r-queued") != importInterrupted || st("r-downloading") != importInterrupted {
		t.Fatalf("mid-prep requests not interrupted: %q %q", st("r-queued"), st("r-downloading"))
	}
	if st("r-awaiting") != importAwaitingConfirmation || st("r-downloaded") != importDownloaded {
		t.Fatalf("user-decision states altered: %q %q", st("r-awaiting"), st("r-downloaded"))
	}
}

func stringsRepeatHex(pair string, n int) string {
	out := ""
	for len(out) < n {
		out += pair
	}
	return out[:n]
}
