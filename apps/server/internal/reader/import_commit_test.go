package reader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func pdfBytes(tb *testing.T) []byte {
	tb.Helper()
	return sample(tb, "reading-notes.pdf")
}

func TestImportCommitLogLifecycle(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	w := uploadTo(t, s, "papers", "paper.pdf", data)
	if w.Code != 201 {
		t.Fatalf("upload status = %d body=%s", w.Code, w.Body.String())
	}
	var doc Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	row := s.Store.DB.QueryRow("SELECT owner,document_id,library,target_rel,full_sha256,size,stage FROM import_commits WHERE document_id=?", doc.ID)
	var owner, docID, library, target, gotHash, stage string
	var size int64
	if err := row.Scan(&owner, &docID, &library, &target, &gotHash, &size, &stage); err != nil {
		t.Fatalf("import_commits missing: %v", err)
	}
	if stage != "committed" || gotHash != full || owner != "upload" || library != "papers" || docID != doc.ID {
		t.Fatalf("commit row = %q %q %q %q %q", stage, gotHash, owner, library, docID)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d want %d", size, len(data))
	}
	if filepath.Base(target) != doc.ID+".pdf" {
		t.Fatalf("target = %q", target)
	}
}

func TestImportDuplicateReturnsExistingDocument(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	w1 := uploadTo(t, s, "papers", "paper.pdf", data)
	if w1.Code != 201 {
		t.Fatalf("first upload = %d", w1.Code)
	}
	var doc1 Document
	if err := json.Unmarshal(w1.Body.Bytes(), &doc1); err != nil {
		t.Fatal(err)
	}
	w2 := uploadTo(t, s, "papers", "paper.pdf", data)
	if w2.Code != 200 {
		t.Fatalf("second upload = %d body=%s", w2.Code, w2.Body.String())
	}
	var doc2 Document
	if err := json.Unmarshal(w2.Body.Bytes(), &doc2); err != nil {
		t.Fatal(err)
	}
	if doc2.ID != doc1.ID {
		t.Fatalf("duplicate import created %s, want %s", doc2.ID, doc1.ID)
	}
	var n int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM import_commits WHERE document_id=? AND stage='committed'", doc1.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
}

func TestImportCommitTargetReservedWhileOpen(t *testing.T) {
	s := testStore(t)
	hash := "aabbcc"
	if _, err := s.DB.Exec(`INSERT INTO import_commits(commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage,error,created_at,updated_at)
		VALUES('c1','upload','','','papers','x.pdf','papers/x.pdf',?,10,'prepared','',?,?)`, hash, now(), now()); err != nil {
		t.Fatal(err)
	}
	// A second open commit on the same target must violate the partial unique index.
	_, err := s.DB.Exec(`INSERT INTO import_commits(commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage,error,created_at,updated_at)
		VALUES('c2','upload','','','papers','x.pdf','papers/x.pdf',?,10,'prepared','',?,?)`, hash, now(), now())
	if err == nil {
		t.Fatal("second open commit on same target must fail")
	}
}

func TestImportFullDigestConflictRejected(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	w1 := uploadTo(t, s, "papers", "paper.pdf", data)
	if w1.Code != 201 {
		t.Fatalf("first upload = %d", w1.Code)
	}
	var doc1 Document
	if err := json.Unmarshal(w1.Body.Bytes(), &doc1); err != nil {
		t.Fatal(err)
	}
	// Corrupt the recorded digest; re-import of the same bytes must refuse to
	// treat the file as the same document instead of trusting the short ID.
	if _, err := s.Store.DB.Exec("UPDATE import_commits SET full_sha256='deadbeef' WHERE document_id=?", doc1.ID); err != nil {
		t.Fatal(err)
	}
	w2 := uploadTo(t, s, "papers", "paper.pdf", data)
	if w2.Code == 200 || w2.Code == 201 {
		t.Fatalf("digest mismatch import succeeded: %d", w2.Code)
	}
}

func TestImportSourceFileSurvivesFailedInstall(t *testing.T) {
	s := testServer(t)
	// A commit whose target path is blocked keeps the staged file and leaves a
	// prepared log row for retry/reconciliation.
	tmp := filepath.Join(t.TempDir(), "staged.pdf")
	data := pdfBytes(t)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Compute the document id the import will target, then occupy that path.
	sum := sha256.Sum256(data)
	docID := hex.EncodeToString(sum[:])[:32]
	target := s.Store.File(Document{ID: docID, Type: "pdf"})
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	_, status, err := s.importFile(context.Background(), tmp, "paper.pdf", "papers", false)
	if err == nil {
		t.Fatalf("import into blocked target succeeded, status=%d", status)
	}
	if _, statErr := os.Stat(tmp); statErr != nil {
		t.Fatalf("staged file removed after failed install: %v", statErr)
	}
	var n int
	if err := s.Store.DB.QueryRow("SELECT count(*) FROM import_commits WHERE stage='prepared'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("prepared rows = %d, want 1", n)
	}
}

func TestWriteGateRejectsNewWritesWhileDraining(t *testing.T) {
	var g libraryWriteGate
	release, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	idle := g.CloseForDrain()
	release()
	select {
	case <-idle:
	default:
		t.Fatal("drain must finish after the last write releases")
	}
	if _, err := g.Acquire(); err == nil {
		t.Fatal("admission must reject new writes while draining")
	}
	g.Reopen()
	release2, err := g.Acquire()
	if err != nil {
		t.Fatalf("reopen must restore admission: %v", err)
	}
	release2()
}

func TestWriteGateDrainWaitsForInflight(t *testing.T) {
	var g libraryWriteGate
	release, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	idle := g.CloseForDrain()
	select {
	case <-idle:
		t.Fatal("drain must not finish while a write is in flight")
	default:
	}
	release()
	<-idle
}
