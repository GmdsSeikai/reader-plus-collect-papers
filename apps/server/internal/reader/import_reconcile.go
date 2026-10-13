package reader

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if err := s.CollectionWritable(); err != nil {
		return err
	}
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
	if err := s.Store.CollectionWritable(); err != nil {
		if errors.Is(err, errCollectionSchemaTooNew) {
			return nil
		}
		return err
	}
	release, err := s.importGate.Acquire()
	if err != nil {
		return err
	}
	defer release()
	if err := s.Store.recoverOpenRuns(); err != nil {
		return err
	}
	// Committed logs still need reconciliation when a crash left their request
	// unfinished. Completed historical imports are verified when opened.
	rows, err := s.Store.DB.Query(`SELECT commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage FROM import_commits
		WHERE stage IN ('prepared','installed') OR (stage='committed' AND
		(owner_ref IN (SELECT request_id FROM collection_imports WHERE status IN ('queued','downloading','importing','interrupted')) OR
		document_id IN (SELECT document_id FROM collection_imports WHERE status IN ('queued','downloading','importing','interrupted')))) ORDER BY created_at`)
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
		if c.stage == importStageCommitted && c.documentID != "" {
			if _, err := s.Store.Document(c.documentID); errors.Is(err, sql.ErrNoRows) {
				s.failRecoveredImport(c.commitID, c.ownerRef, c.fullSHA, "原文档已移除，不自动重新创建")
				continue
			} else if err != nil {
				return err
			}
		}
		s.recoverOneImport(c.commitID, c.owner, c.ownerRef, c.library, c.filename, c.targetRel, c.fullSHA, c.size)
	}
	s.sweepInterruptedImports()
	var remaining int
	if err := s.Store.DB.QueryRow(`SELECT (SELECT count(*) FROM import_commits WHERE stage IN ('prepared','installed'))+
		(SELECT count(*) FROM collection_imports WHERE status IN ('queued','downloading','importing'))`).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("导入恢复仍有 %d 项未结束，未启动后台任务", remaining)
	}
	s.cleanupOwnerStagedFiles()
	return nil
}

// recoverOneImport verifies the real file against the registered full digest
// and either resumes the commit, repairs from the verified cache, or closes
// the row as failed. Unknown or mismatching target files are never
// overwritten or deleted.
func (s *Server) recoverOneImport(commitID, owner, ownerRef, library, filename, targetRel, fullSHA string, size int64) {
	if len(fullSHA) != 64 {
		s.failRecoveredImport(commitID, ownerRef, fullSHA, "登记摘要无效")
		return
	}
	if _, err := hex.DecodeString(fullSHA); err != nil {
		s.failRecoveredImport(commitID, ownerRef, fullSHA, "登记摘要无效")
		return
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(targetRel))) != importTargetRel(fullSHA[:32], filename) {
		s.failRecoveredImport(commitID, ownerRef, fullSHA, "登记路径与文档身份不符")
		return
	}
	dest, pathErr := s.Store.ownedFilePath(targetRel)
	if pathErr != nil {
		s.failRecoveredImport(commitID, ownerRef, fullSHA, pathErr.Error())
		return
	}
	if st, err := os.Stat(dest); err == nil && st.Mode().IsRegular() {
		_, actual, actualSize, herr := hashImportFile(dest)
		if herr == nil && strings.EqualFold(actual, fullSHA) && (size == 0 || actualSize == size) {
			s.resumeImportCommit(commitID, owner, ownerRef, library, filename, targetRel, fullSHA, size)
			return
		}
		// A different file owns this path now: keep its bytes and refuse.
		s.failRecoveredImport(commitID, ownerRef, fullSHA, "目标文件与登记摘要不符，未覆盖")
		return
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		s.failRecoveredImport(commitID, ownerRef, fullSHA, "目标文件不可验证，未覆盖")
		return
	}
	// Missing official file: repair only from a verified, owned cache copy.
	if cache, ok := s.recoveryCache(ownerRef, fullSHA); ok {
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err == nil {
			if err := copyRecoveryFile(cache, dest); err == nil {
				_, actual, actualSize, herr := hashImportFile(dest)
				if herr == nil && strings.EqualFold(actual, fullSHA) && (size == 0 || actualSize == size) {
					s.resumeImportCommit(commitID, owner, ownerRef, library, filename, targetRel, fullSHA, size)
					return
				}
				_ = os.Remove(dest)
			}
		}
	}
	s.failRecoveredImport(commitID, ownerRef, fullSHA, "正式文件缺失且无可信缓存修复")
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
		if existing.Type != strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".") || filepath.ToSlash(s.Store.File(existing)) != filepath.ToSlash(filepath.Join(s.Store.Root, filepath.FromSlash(targetRel))) {
			s.failRecoveredImport(commitID, ownerRef, fullSHA, "已有文档类型或文件身份不符")
			return
		}
		// The document landed before the crash (or predates the log): close
		// the log and repair the association without touching reading data.
		tx, terr := s.Store.DB.Begin()
		if terr != nil {
			_ = s.Store.markImportCommit(commitID, importStageFailed, "", "恢复提交失败")
			return
		}
		if werr := collectionWritableTx(tx); werr == nil {
			if werr = markImportCommitTx(tx, commitID, importStageCommitted, docID); werr == nil {
				terr = linkRecoveredRequestTx(tx, ownerRef, existing, fullSHA)
				if terr == nil {
					terr = tx.Commit()
				}
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
	if err := linkRecoveredRequestTx(tx, ownerRef, doc, fullSHA); err != nil {
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
				req, rerr := s.Store.CollectionImport(r.requestID)
				file, ferr := s.Store.ownedFilePath(importTargetRel(d.ID, d.ID+"."+d.Type))
				if rerr == nil && ferr == nil && d.Type == "pdf" && len(req.ActualSHA256) == 64 {
					_, actual, _, herr := hashImportFile(file)
					if herr == nil && strings.EqualFold(actual, req.ActualSHA256) {
						tx, terr := s.Store.DB.Begin()
						if terr == nil {
							terr = linkRecoveredRequestTx(tx, r.requestID, d, actual)
							if terr == nil {
								terr = tx.Commit()
							}
							if terr != nil {
								tx.Rollback()
							}
						}
						if terr == nil {
							continue
						}
					}
				}
				_, _ = s.Store.claimImport(r.requestID, []string{importQueued, importDownloading, importImporting}, map[string]any{"status": importFailed, "error": "已有文档的正式文件未通过校验"})
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
		if filepath.Base(rid) != rid || rid == "." || rid == ".." {
			continue
		}
		if staged, err := s.Store.ownedFilePath(filepath.ToSlash(filepath.Join("cache", "paper-collection", rid, "staged.pdf"))); err == nil {
			_ = os.Remove(staged)
		}
	}
}

// verifiedCacheCopy finds an owner-attributed cache file whose full digest
// matches. Only cache/paper-collection/<request>/ copies are ever considered.
func (s *Server) verifiedCacheCopy(requestID, fullSHA string) (string, bool) {
	if requestID == "" || filepath.Base(requestID) != requestID || requestID == "." || requestID == ".." {
		return "", false
	}
	req, err := s.Store.CollectionImport(requestID)
	if err != nil || (req.ActualSHA256 != "" && !strings.EqualFold(req.ActualSHA256, fullSHA)) {
		return "", false
	}
	dir, err := s.Store.ownedFilePath(filepath.ToSlash(filepath.Join("cache", "paper-collection", requestID)))
	if err != nil {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p, err := s.Store.ownedFilePath(filepath.ToSlash(filepath.Join("cache", "paper-collection", requestID, e.Name())))
		if err != nil {
			continue
		}
		if e.Name() != "staged.pdf" && filepath.ToSlash(filepath.Clean(filepath.FromSlash(req.CacheRelPath))) != filepath.ToSlash(filepath.Join("cache", "paper-collection", requestID, e.Name())) {
			continue
		}
		if _, actual, _, err := hashImportFile(p); err == nil && strings.EqualFold(actual, fullSHA) {
			return p, true
		}
	}
	return "", false
}

func linkRecoveredRequestTx(tx *sql.Tx, requestID string, d Document, fullSHA string) error {
	if err := collectionWritableTx(tx); err != nil {
		return err
	}
	status, condition := importImported, ""
	if d.DeletedAt != "" {
		status, condition = importAwaitingConfirmation, "trash-restore"
	}
	_, err := tx.Exec(`UPDATE collection_imports SET status=?,document_id=?,library=?,actual_sha256=?,error='',"condition"=?,updated_at=?
		WHERE request_id=? AND status IN ('queued','downloading','downloaded','awaiting_confirmation','importing','interrupted')`, status, d.ID, d.Library, fullSHA, condition, now(), requestID)
	return err
}

func (s *Server) failRecoveredImport(commitID, ownerRef, fullSHA, message string) {
	tx, err := s.Store.DB.Begin()
	if err != nil {
		log.Printf("import recovery %s: %v", commitID, err)
		return
	}
	defer tx.Rollback()
	if err = collectionWritableTx(tx); err == nil {
		_, err = tx.Exec("UPDATE import_commits SET stage='failed',error=?,updated_at=? WHERE commit_id=?", message, now(), commitID)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE collection_imports SET status='failed',error=?,updated_at=?
		WHERE (request_id=? OR (actual_sha256=? AND document_id<>'')) AND status IN ('queued','downloading','importing','interrupted')`, message, now(), ownerRef, fullSHA)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.Printf("import recovery %s: %v", commitID, err)
	}
}

func (s *Server) recoveryCache(ownerRef, fullSHA string) (string, bool) {
	if file, ok := s.verifiedCacheCopy(ownerRef, fullSHA); ok {
		return file, true
	}
	rows, err := s.Store.DB.Query("SELECT request_id FROM collection_imports WHERE actual_sha256=? AND status NOT IN ('cancelled','failed')", fullSHA)
	if err != nil {
		return "", false
	}
	ids := []string{}
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return "", false
		}
		ids = append(ids, rid)
	}
	rows.Close()
	for _, rid := range ids {
		if file, ok := s.verifiedCacheCopy(rid, fullSHA); ok {
			return file, true
		}
	}
	return "", false
}

func (s *Store) ownedFilePath(relative string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("文件路径越界")
	}
	current := s.Root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("文件路径包含链接")
		}
	}
	return current, nil
}

func copyRecoveryFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
