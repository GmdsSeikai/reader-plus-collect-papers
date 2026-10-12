package reader

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Import log stages. prepared→installed→committed are written by the shared
// commit path; failed closes a row after a verified non-recoverable error.
const (
	importStagePrepared  = "prepared"
	importStageInstalled = "installed"
	importStageCommitted = "committed"
	importStageFailed    = "failed"
)

// importInstall describes one staged file moving into a library through the
// shared commit path. The caller must hold the library write gate.
type importInstall struct {
	Owner    string
	OwnerRef string
	Temp     string
	Filename string
	Library  string
	DocID    string
	FullSHA  string
	Size     int64
}

// hashImportFile returns the short document ID and full SHA256 of a staged file.
func hashImportFile(temp string) (string, string, int64, error) {
	f, err := os.Open(temp)
	if err != nil {
		return "", "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return "", "", 0, err
	}
	full := hex.EncodeToString(hash.Sum(nil))
	return full[:32], full, size, nil
}

// verifyExistingImport checks that a committed import log for this document
// matches the file's full digest. The short content-derived ID alone is not
// trusted as proof that two files are identical.
func (s *Store) verifyExistingImport(docID, fullSHA string) error {
	var recorded string
	err := s.DB.QueryRow("SELECT full_sha256 FROM import_commits WHERE document_id=? AND stage='committed' ORDER BY updated_at DESC LIMIT 1", docID).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		// Legacy imports predate the log; there is no digest to disagree with.
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(recorded, fullSHA) {
		return fmt.Errorf("已有文档的文件摘要与本次文件不一致，已拒绝覆盖")
	}
	return nil
}

// beginImportCommit records the prepared stage as its own committed write so a
// crash before install leaves a durable, reconcilable row. A retry of the same
// target and digest reuses the open row instead of being blocked by it.
func (s *Store) beginImportCommit(spec importInstall) (string, error) {
	target := importTargetRel(spec.DocID, spec.Filename)
	var commitID, stage, digest string
	err := s.DB.QueryRow("SELECT commit_id,stage,full_sha256 FROM import_commits WHERE target_rel=? AND stage IN ('prepared','installed')", target).Scan(&commitID, &stage, &digest)
	if err == nil {
		if !strings.EqualFold(digest, spec.FullSHA) {
			return "", fmt.Errorf("目标文件已有未完成的导入登记，且摘要不同")
		}
		_, err = s.DB.Exec("UPDATE import_commits SET stage=?,owner=?,owner_ref=?,error='',updated_at=? WHERE commit_id=?", importStagePrepared, spec.Owner, spec.OwnerRef, now(), commitID)
		if err != nil {
			return "", fmt.Errorf("无法登记导入日志: %w", err)
		}
		return commitID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("无法登记导入日志: %w", err)
	}
	commitID = id()
	_, err = s.DB.Exec(`INSERT INTO import_commits(commit_id,owner,owner_ref,document_id,library,filename,target_rel,full_sha256,size,stage,error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'',?,?)`,
		commitID, spec.Owner, spec.OwnerRef, "", spec.Library, spec.Filename, target, spec.FullSHA, spec.Size, importStagePrepared, now(), now())
	if err != nil {
		return "", fmt.Errorf("无法登记导入日志: %w", err)
	}
	return commitID, nil
}

func (s *Store) markImportCommit(commitID, stage, documentID, errMsg string) error {
	_, err := s.DB.Exec("UPDATE import_commits SET stage=?,document_id=?,error=?,updated_at=? WHERE commit_id=?", stage, documentID, errMsg, now(), commitID)
	return err
}

// markImportCommitTx records the committed stage inside the caller's document
// transaction, so the log and the document row land together.
func markImportCommitTx(tx *sql.Tx, commitID, stage, documentID string) error {
	_, err := tx.Exec("UPDATE import_commits SET stage=?,document_id=?,updated_at=? WHERE commit_id=?", stage, documentID, now(), commitID)
	return err
}

// importTargetRel mirrors Store.File: PDFs live in papers/, other kinds in
// books/, independent of the library column.
func importTargetRel(docID, filename string) string {
	kind := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if kind == "" {
		kind = "pdf"
	}
	dir := "books"
	if kind == "pdf" {
		dir = "papers"
	}
	return filepath.ToSlash(filepath.Join(dir, docID+"."+kind))
}

// installImportFile moves the staged file to its library path and commits the
// caller's rows plus the log's committed stage in one transaction. On any
// failure the staged file is kept for retry and the log row stays open
// (prepared/installed) for startup reconciliation. A destination that already
// existed and matched the digest is reused, never deleted by failure cleanup,
// and out-of-transaction log writes happen only after the transaction has
// fully released the single store connection.
func (s *Server) installImportFile(ctx context.Context, spec importInstall, commit func(tx *sql.Tx) (Document, error)) (Document, error) {
	commitID, err := s.Store.beginImportCommit(spec)
	if err != nil {
		return Document{}, err
	}
	dest := filepath.Join(s.Store.Root, filepath.FromSlash(importTargetRel(spec.DocID, spec.Filename)))
	installedByUs := false
	if st, statErr := os.Stat(dest); statErr == nil {
		// A crash may have installed the file already. Only a verified digest
		// match may continue; anything else keeps the row open for
		// reconciliation and refuses to overwrite. Reused files are never
		// removed by this request.
		_, full, _, hashErr := hashImportFile(dest)
		if hashErr != nil || !strings.EqualFold(full, spec.FullSHA) || st.Size() != spec.Size {
			_ = s.Store.markImportCommit(commitID, importStagePrepared, "", "目标已存在未登记文件")
			return Document{}, fmt.Errorf("目标已存在未登记的同名文件，请先检查数据目录")
		}
	} else if err := os.Rename(spec.Temp, dest); err != nil {
		_ = s.Store.markImportCommit(commitID, importStagePrepared, "", err.Error())
		return Document{}, fmt.Errorf("无法保存原文件")
	} else {
		installedByUs = true
	}
	if err := s.Store.markImportCommit(commitID, importStageInstalled, "", ""); err != nil {
		if installedByUs {
			_ = os.Remove(dest)
		}
		_ = s.Store.markImportCommit(commitID, importStagePrepared, "", err.Error())
		return Document{}, fmt.Errorf("无法登记安装结果")
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		if installedByUs {
			_ = os.Remove(dest)
		}
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", err.Error())
		return Document{}, fmt.Errorf("无法写入书库")
	}
	d, err := commit(tx)
	if err == nil {
		err = markImportCommitTx(tx, commitID, importStageCommitted, d.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		// Release the single connection before any non-transactional write;
		// otherwise the log update deadlocks waiting for this very transaction.
		_ = tx.Rollback()
		if installedByUs {
			_ = os.Remove(dest)
		}
		_ = s.Store.markImportCommit(commitID, importStageFailed, "", err.Error())
		return Document{}, fmt.Errorf("书库保存失败")
	}
	if saved, e := s.Store.Document(d.ID); e == nil {
		d = saved
	}
	return d, nil
}

// zoteroFresh reports whether the document still needs a file install. The
// library write gate already serializes writers, so the pre-check outside the
// Zotero transaction is stable.
func (s *Store) documentExists(docID string) (bool, error) {
	var one int
	err := s.DB.QueryRow("SELECT 1 FROM documents WHERE id=?", docID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
