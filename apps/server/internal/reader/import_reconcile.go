package reader

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Startup recovery runs once in the startup chain after the store opens and
// before processing workers or request admission. It reconciles the
// import_commits log with real files and document rows, and closes any run a
// crash left open. All helpers here use Store-level pieces only: no
// re-acquiring the library write gate (it is not reentrant) and no Store.DB
// calls inside an open transaction.

// recoverOpenRuns marks runs a crash left open as interrupted and bumps their
// exec tokens so late responses can never advance their slices.
func (s *Store) recoverOpenRuns() error {
	rows, err := s.DB.Query("SELECT run_id,task_id FROM collection_runs WHERE status IN ('queued','running')")
	if err != nil {
		return err
	}
	type openRun struct{ runID, taskID string }
	open := []openRun{}
	for rows.Next() {
		var r openRun
		if err := rows.Scan(&r.runID, &r.taskID); err != nil {
			rows.Close()
			return err
		}
		open = append(open, r)
	}
	rows.Close()
	for _, r := range open {
		if err := s.BumpCollectionExecToken(r.taskID); err != nil {
			return err
		}
		if err := s.FinishCollectionRun(r.runID, runInterrupted, "startup"); err != nil {
			return err
		}
	}
	return nil
}

// RecoverImports reconciles open import_commits rows and interrupted
// collection_imports requests. It is idempotent: a second restart finds only
// closed rows. A newer module schema disables collection-log repairs while
// leaving reading fully functional.
func (s *Server) RecoverImports() error {
	if err := s.Store.recoverOpenRuns(); err != nil {
		return err
	}
	if err := s.Store.CollectionWritable(); err != nil {
		log.Printf("import recovery skipped: %v", err)
		return nil
	}
	rows, err := s.Store.DB.Query("SELECT commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage FROM import_commits WHERE stage IN ('prepared','installed') ORDER BY created_at")
	if err != nil {
		return err
	}
	type openCommit struct {
		commitID, owner, ownerRef, documentID, library, filename, targetRel, fullSHA string
		size                                                                         int64
		stage                                                                        string
	}
	commits := []openCommit{}
	for rows.Next() {
		var c openCommit
		if err := rows.Scan(&c.commitID, &c.owner, &c.ownerRef, &c.documentID, &c.library, &c.filename, &c.targetRel, &c.fullSHA, &c.size, &c.stage); err != nil {
			rows.Close()
			return err
		}
		commits = append(commits, c)
	}
	rows.Close()
	for _, c := range commits {
		s.recoverOneImport(c.commitID, c.owner, c.ownerRef, c.library, c.filename, c.targetRel, c.fullSHA, c.size)
	}
	s.sweepInterruptedImports()
	s.cleanupOwnerStagedFiles()
	return nil
}

// recoverOneImport verifies the real file against the registered full digest
// and either resumes the commit, repairs from the verified cache, or closes
// the row as failed. Unknown or mismatching target files are never
// overwritten or deleted.
func (s *Server) recoverOneImport(commitID, owner, ownerRef, library, filename, targetRel, fullSHA string, size int64) {
	dest := filepath.Join(s.Store.Root, filepath.FromSlash(targetRel))
	if st, err := os.Stat(dest); err == nil && st.Mode().IsRegular() {
		_, actual, actualSize, herr := hashImportFile(dest)
		if herr == nil && strings.EqualFold(actual, fullSHA) && (size == 0 || actualSize == size) {
			s.resumeImportCommit(commitID, owner, ownerRef, library, filename, targetRel, fullSHA, size)
			return
		}
		// A different file owns this path now: keep its bytes and refuse.
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "目标文件与登记摘要不符，未覆盖")
		return
	}
	// Missing official file: repair only from a verified, owned cache copy.
	if cache, ok := s.verifiedCacheCopy(ownerRef, fullSHA); ok {
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err == nil {
			if err := copyFile(cache, dest); err == nil {
				_, actual, actualSize, herr := hashImportFile(dest)
				if herr == nil && strings.EqualFold(actual, fullSHA) && (size == 0 || actualSize == size) {
					s.resumeImportCommit(commitID, owner, ownerRef, library, filename, targetRel, fullSHA, size)
					return
				}
				_ = os.Remove(dest)
			}
		}
	}
	_ = s.Store.markImportCommit(commitID, importStageFailed, "", "正式文件缺失且无可信缓存修复")
}

// resumeImportCommit completes the database side of an import whose official
// file is verified. If the document row already exists it only closes the log
// and repairs links; otherwise it inserts the document, its initial
// processing row and the import association in one transaction. Documents in
// the trash are never restored automatically.
func (s *Server) resumeImportCommit(commitID, owner, ownerRef, library, filename, targetRel, fullSHA string, size int64) {
	docID := fullSHA[:32]
	existing, err := s.Store.Document(docID)
	if err == nil {
		// The document landed before the crash (or predates the log): close
		// the log and repair the association without touching reading data.
		tx, terr := s.Store.DB.Begin()
		if terr != nil {
			_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
			return
		}
		if werr := collectionWritableTx(tx); werr == nil {
			if werr = markImportCommitTx(tx, commitID, importStageCommitted, docID); werr == nil {
				terr = tx.Commit()
			} else {
				terr = werr
			}
		} else {
			terr = werr
		}
		if terr != nil {
			tx.Rollback()
			_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
			return
		}
		s.linkImportRequest(ownerRef, docID, existing.Library)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "无法读取文档")
		return
	}
	// Insert the missing document with the same defaults the original commit
	// path uses, in one transaction with the log's committed stage.
	var snapshot CollectionCandidate
	if owner == "collection" {
		if req, rerr := s.Store.CollectionImport(ownerRef); rerr == nil {
			_ = json.Unmarshal(req.MetadataSnapshot, &snapshot)
		}
	}
	title := snapshot.Title
	if title == "" {
		title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	kind := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if kind == "" {
		kind = "pdf"
	}
	doc := Document{
		ID:           docID,
		Type:         kind,
		Title:        title,
		Author:       joinNames(snapshot.Authors),
		Size:         size,
		CreatedAt:    now(),
		LastOpenedAt: now(),
		Library:      library,
	}
	doc.Category, doc.CategorySource, doc.ClassificationStatus = "paper", "manual", "done"
	doc.Tags, doc.Folders = []string{}, []string{}
	if library == "books" && kind != "pdf" {
		doc.Category = "book"
	}
	meta := "{}"
	if owner == "collection" {
		m, _ := json.Marshal(PaperMetadata{Lookup: "done", Sources: map[string]string{"title": "arxiv", "creators": "arxiv"}, Abstract: snapshot.Abstract, DOI: snapshot.DOI, ArXiv: snapshot.ArXivID})
		meta = string(m)
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	if err := collectionWritableTx(tx); err != nil {
		tx.Rollback()
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "模块版本不允许写入")
		return
	}
	if _, err := tx.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,category,category_source,classification_status,library,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		doc.ID, doc.Type, doc.Title, doc.Author, doc.Size, doc.CreatedAt, doc.LastOpenedAt, doc.Category, doc.CategorySource, doc.ClassificationStatus, doc.Library, meta); err != nil {
		tx.Rollback()
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	var settings string
	_ = tx.QueryRow("SELECT value FROM settings WHERE key='reader'").Scan(&settings)
	p := initialDocumentProcessing(doc, settings)
	body, _ := json.Marshal(p)
	if _, err := tx.Exec("INSERT INTO document_processing(document_id,phase,status,body) VALUES(?,?,?,?)", doc.ID, p.Phase, p.Status, body); err != nil {
		tx.Rollback()
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	if _, err := tx.Exec("UPDATE collection_imports SET document_id=?, library=? WHERE request_id=? AND document_id=''", doc.ID, doc.Library, ownerRef); err != nil {
		tx.Rollback()
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	if err := markImportCommitTx(tx, commitID, importStageCommitted, doc.ID); err != nil {
		tx.Rollback()
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	if err := tx.Commit(); err != nil {
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
		return
	}
	s.linkImportRequest(ownerRef, doc.ID, doc.Library)
}

// linkImportRequest converges a collection request whose document landed
// before the crash: it reports imported instead of staying interrupted.
func (s *Server) linkImportRequest(requestID, docID, library string) {
	if requestID == "" {
		return
	}
	if _, err := s.Store.CollectionImport(requestID); err != nil {
		return
	}
	_, _ = s.Store.claimImport(requestID, []string{importQueued, importDownloading, importDownloaded, importAwaitingConfirmation, importImporting, importInterrupted}, map[string]any{
		"status": importImported, "document_id": docID, "library": library, "error": "",
	})
}

// sweepInterruptedImports marks requests a crash left mid-preparation as
// interrupted. User-decision states (downloaded / awaiting_confirmation) stay
// untouched; requests whose document already landed converge to imported.
func (s *Server) sweepInterruptedImports() {
	rows, err := s.Store.DB.Query("SELECT request_id,document_id,library FROM collection_imports WHERE status IN ('queued','downloading','importing')")
	if err != nil {
		return
	}
	type midRequest struct{ requestID, documentID, library string }
	mid := []midRequest{}
	for rows.Next() {
		var r midRequest
		if rows.Scan(&r.requestID, &r.documentID, &r.library) == nil {
			mid = append(mid, r)
		}
	}
	rows.Close()
	for _, r := range mid {
		if r.documentID != "" {
			if d, err := s.Store.Document(r.documentID); err == nil {
				s.linkImportRequest(r.requestID, d.ID, d.Library)
				continue
			}
		}
		_, _ = s.Store.claimImport(r.requestID, []string{importQueued, importDownloading, importImporting}, map[string]any{"status": importInterrupted, "error": "服务中断，导入未完成"})
	}
}

// cleanupOwnerStagedFiles removes only staged copies whose request is closed
// as failed/committed — named staged.pdf inside their own request directory.
func (s *Server) cleanupOwnerStagedFiles() {
	rows, err := s.Store.DB.Query("SELECT request_id FROM collection_imports WHERE status IN ('failed','imported','interrupted')")
	if err != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var rid string
		if rows.Scan(&rid) == nil {
			ids = append(ids, rid)
		}
	}
	rows.Close()
	for _, rid := range ids {
		staged := filepath.Join(s.Store.Root, "cache", "paper-collection", rid, "staged.pdf")
		_ = os.Remove(staged)
	}
}

// verifiedCacheCopy finds an owner-attributed cache file whose full digest
// matches. Only cache/paper-collection/<request>/ copies are ever considered.
func (s *Server) verifiedCacheCopy(requestID, fullSHA string) (string, bool) {
	if requestID == "" {
		return "", false
	}
	dir := filepath.Join(s.Store.Root, "cache", "paper-collection", requestID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if _, actual, _, err := hashImportFile(p); err == nil && strings.EqualFold(actual, fullSHA) {
			return p, true
		}
	}
	return "", false
}
