package reader

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectionM1RemovedDocumentIsNotRecreated(t *testing.T) {
	s := testServer(t)
	d, data, full := m1RegressionPDF(t, s)
	m1RegressionRequest(t, s, d, data, full)
	if _, err := s.Store.DB.Exec("DELETE FROM documents WHERE id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.Store.File(d)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Document(d.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed document recreated at startup: %v", err)
	}
}

func TestCollectionM1FutureSchemaStartupDoesNotWrite(t *testing.T) {
	s := testServer(t)
	task := newScanFixture(t, s)
	run, err := s.Store.CreateCollectionRun(task, "manual", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.DB.Exec("UPDATE collection_meta SET schema_version=?", collectionSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	var status string
	var token int64
	if err = s.Store.DB.QueryRow("SELECT status,exec_token FROM collection_runs WHERE run_id=?", run.ID).Scan(&status, &token); err != nil {
		t.Fatal(err)
	}
	if status != run.Status || token != run.ExecToken {
		t.Fatalf("future-schema startup mutated protected run: status=%s token=%d (before %s %d)", status, token, run.Status, run.ExecToken)
	}
}

func m1RegressionPDF(t *testing.T, s *Server) (Document, []byte, string) {
	data := pdfBytes(t)
	w := uploadTo(t, s, "papers", "paper.pdf", data)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var d Document
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return d, data, hex.EncodeToString(sum[:])
}

func m1RegressionRequest(t *testing.T, s *Server, d Document, data []byte, full string) CollectionImportRequest {
	seedCandidate(t, s.Store, "2301.12345")
	rid := id()
	dir := s.collectionImportCache(rid)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.pdf"), data, 0600); err != nil {
		t.Fatal(err)
	}
	req := CollectionImportRequest{RequestID: rid, ArXivID: "2301.12345", DownloadVersion: 1, Status: importImporting, DocumentID: d.ID, Library: d.Library, ActualSHA256: full, MetadataSnapshot: json.RawMessage("{}"), CacheRelPath: filepath.ToSlash(filepath.Join("cache", "paper-collection", rid, "cache.pdf")), CreatedAt: now(), UpdatedAt: now()}
	if err := s.Store.InsertCollectionImport(req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestCollectionM1CommittedMissingFileRepairsAtStartup(t *testing.T) {
	s := testServer(t)
	d, data, full := m1RegressionPDF(t, s)
	req := m1RegressionRequest(t, s, d, data, full)
	if err := os.Remove(s.Store.File(d)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(s.Store.File(d))
	fresh, _ := s.Store.CollectionImport(req.RequestID)
	if err != nil || string(file) != string(data) {
		t.Fatalf("committed log skipped, missing official PDF not repaired from verified cache; status=%s err=%v", fresh.Status, err)
	}
}

func TestCollectionM1CommittedChangedFileCannotReportImported(t *testing.T) {
	s := testServer(t)
	d, data, full := m1RegressionPDF(t, s)
	req := m1RegressionRequest(t, s, d, data, full)
	if err := os.WriteFile(s.Store.File(d), []byte("%PDF-1.4 unknown changed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Store.CollectionImport(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == importImported {
		t.Fatal("startup reported imported from committed log/document ID without verifying the changed formal PDF")
	}
}

func TestCollectionM1ExistingDocumentFileIdentityIsChecked(t *testing.T) {
	s := testServer(t)
	d, data, full := m1RegressionPDF(t, s)
	req := m1RegressionRequest(t, s, d, data, full)
	// A mismatched existing row points the reader at a different file from the verified log target.
	if _, err := s.Store.DB.Exec("UPDATE documents SET type='epub' WHERE id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec("UPDATE import_commits SET stage='installed' WHERE document_id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverImports(); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Store.CollectionImport(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == importImported {
		t.Fatal("matched only short Document ID; reported imported although Store.File(existing) is missing and differs from the log target")
	}
}
