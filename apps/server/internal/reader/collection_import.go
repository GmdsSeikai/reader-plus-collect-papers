package reader

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// collectionImportCache is the on-disk staging root for fixed-version
// downloads. Files stay recoverable until the shared commit finishes.
func (s *Server) collectionImportCache(requestID string) string {
	return filepath.Join(s.Store.Root, "cache", "paper-collection", requestID)
}

// importLocalName is the safe local file name for a downloaded version. The
// source ID may contain "/" (old category paths); it never becomes a path.
func importLocalName(arxivID string, version int) string {
	safe := strings.ReplaceAll(arxivID, "/", "_")
	safe = strings.ReplaceAll(safe, "\\", "_")
	return fmt.Sprintf("%sv%d.pdf", safe, version)
}

type collectionImportBody struct {
	Items []struct {
		ArXivID             string `json:"arxivId"`
		Version             int    `json:"version"`
		ExpectedContentHash string `json:"expectedContentHash"`
	} `json:"items"`
}

func (s *Server) collectionImportCreate(w http.ResponseWriter, r *http.Request) {
	var body collectionImportBody
	if !decode(w, r, &body) {
		return
	}
	if len(body.Items) == 0 || len(body.Items) > 50 {
		collectionFail(w, 400, "query-invalid", "请勾选 1–50 篇论文")
		return
	}
	type result struct {
		RequestID string `json:"requestId"`
		ArXivID   string `json:"arxivId"`
		Error     string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(body.Items))
	// One failed item never stops the rest; each item goes through the
	// manager's serial queue rather than an unowned goroutine.
	for _, item := range body.Items {
		req, err := s.startCollectionImport(item.ArXivID, item.Version, item.ExpectedContentHash)
		if err != nil {
			results = append(results, result{ArXivID: item.ArXivID, Error: err.Error()})
			continue
		}
		results = append(results, result{RequestID: req.RequestID, ArXivID: req.ArXivID})
	}
	respond(w, 202, map[string]any{"items": results})
}

// startCollectionImport registers one pinned-version request and queues the
// download. Repeated clicks return the open request.
func (s *Server) startCollectionImport(arxivID string, version int, expectedHash string) (CollectionImportRequest, error) {
	stableID, err := NormalizeArXivID(arxivID)
	if err != nil {
		return CollectionImportRequest{}, err
	}
	if !ValidArXivVersion(version) {
		return CollectionImportRequest{}, fmt.Errorf("必须指定要下载的版本")
	}
	if s.collection().Draining() {
		return CollectionImportRequest{}, errAdmissionClosed
	}
	c, _, err := s.Store.CollectionCandidate(stableID)
	if err != nil {
		return CollectionImportRequest{}, fmt.Errorf("候选不存在")
	}
	if expectedHash != "" && expectedHash != c.ContentHash {
		return CollectionImportRequest{}, fmt.Errorf("候选内容已变化，请刷新后重新选择")
	}
	if existing, err := s.Store.ActiveCollectionImport(stableID); err == nil {
		return existing, nil
	}
	req := CollectionImportRequest{
		RequestID:       id(),
		ArXivID:         stableID,
		DownloadVersion: version,
		Status:          importQueued,
		CreatedAt:       now(),
		UpdatedAt:       now(),
	}
	snapshot, _ := json.Marshal(c)
	req.MetadataSnapshot = snapshot
	if err := s.Store.InsertCollectionImport(req); err != nil {
		if existing, e := s.Store.ActiveCollectionImport(stableID); e == nil {
			return existing, nil
		}
		return CollectionImportRequest{}, err
	}
	s.collection().queueImport(s, req)
	return req, nil
}

// runCollectionImport downloads and validates one request under the manager's
// owned context. Normal papers commit directly after validation; papers with
// 51–150 pages wait for the user's explicit confirmation.
func (s *Server) runCollectionImport(ctx context.Context, req CollectionImportRequest) {
	dir := s.collectionImportCache(req.RequestID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		_, _ = s.Store.claimImport(req.RequestID, importActive, map[string]any{"status": importFailed, "error": "无法创建下载目录"})
		return
	}
	pdfPath := filepath.Join(dir, importLocalName(req.ArXivID, req.DownloadVersion))
	f, err := os.OpenFile(pdfPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		_, _ = s.Store.claimImport(req.RequestID, importActive, map[string]any{"status": importFailed, "error": "无法创建下载文件"})
		return
	}
	if ok, _ := s.Store.claimImport(req.RequestID, importActive, map[string]any{"status": importDownloading}); !ok {
		f.Close()
		return
	}
	client := s.scholarClient()
	err = client.DownloadArxivVersion(ctx, req.ArXivID, req.DownloadVersion, f, maxPaperBytes)
	if err != nil {
		f.Close()
		_ = os.Remove(pdfPath)
		status := importFailed
		message := err.Error()
		if ctx.Err() != nil {
			status = importCancelled
			message = "已取消"
		}
		// A late failure never overwrites another owner's terminal state.
		_, _ = s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, map[string]any{"status": status, "error": message})
		return
	}
	hash := sha256.New()
	if _, err := f.Seek(0, 0); err == nil {
		buf := make([]byte, 1<<16)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				hash.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
	}
	full := hex.EncodeToString(hash.Sum(nil))
	pages, pageErr := pdfPageCount(pdfPath)
	size := int64(0)
	if st, e := f.Stat(); e == nil {
		size = st.Size()
	}
	f.Close()
	if reason := paperLimit("pdf", size, pdfPath); reason != "" {
		_ = os.Remove(pdfPath)
		_, _ = s.Store.claimImport(req.RequestID, importActive, map[string]any{"status": importFailed, "error": reason})
		return
	}
	fields := map[string]any{
		"actual_sha256":  full,
		"cache_rel_path": filepath.ToSlash(filepath.Join("cache", "paper-collection", req.RequestID, filepath.Base(pdfPath))),
		"confirm_pages":  pages,
		"condition":      "",
	}
	// Long papers need the user's explicit page-count confirmation.
	if pageErr == nil && pages > largePaperPages {
		fields["status"] = importAwaitingConfirmation
		fields["condition"] = "pages"
		_, _ = s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, fields)
		return
	}
	// A duplicate whose existing document is in the trash needs the user's
	// explicit restore decision before anything is written.
	docID := full[:32]
	if existing, e := s.Store.Document(docID); e == nil && existing.DeletedAt != "" {
		fields["status"] = importAwaitingConfirmation
		fields["condition"] = "trash-restore"
		_, _ = s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, fields)
		return
	}
	// Normal papers share the exact claim path with manual confirmation: the
	// request must own importing before any library write starts.
	fields["status"] = importDownloaded
	if ok, _ := s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, fields); !ok {
		return
	}
	if ok, _ := s.Store.claimImport(req.RequestID, []string{importDownloaded}, map[string]any{"status": importImporting}); !ok {
		return
	}
	s.commitCollectionImport(s.Store.MustImport(req.RequestID))
}

func (s *Server) collectionImportGet(w http.ResponseWriter, r *http.Request) {
	req, err := s.Store.CollectionImport(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 404, "query-invalid", "导入请求不存在")
		return
	}
	respond(w, 200, collectionImportView{CollectionImportRequest: req, DocumentState: s.importDocumentState(req)})
}

func (s *Server) collectionImportList(w http.ResponseWriter, r *http.Request) {
	reqs, err := s.Store.ActiveCollectionImports()
	if err != nil {
		collectionFail(w, 500, "query-invalid", "无法读取导入请求")
		return
	}
	respond(w, 200, reqs)
}

func (s *Server) collectionImportConfirm(w http.ResponseWriter, r *http.Request) {
	// Atomic claim: exactly one confirm may move the request to importing.
	ok, err := s.Store.claimImport(r.PathValue("id"), []string{importDownloaded, importAwaitingConfirmation}, map[string]any{"status": importImporting})
	if err != nil {
		collectionFail(w, 404, "query-invalid", "导入请求不存在")
		return
	}
	if !ok {
		collectionFail(w, 409, "import-busy", "当前状态不能确认入库，或已有提交进行中")
		return
	}
	req, err := s.Store.CollectionImport(r.PathValue("id"))
	if err != nil {
		collectionFail(w, 500, "import-busy", "无法读取导入请求")
		return
	}
	s.collection().queueCommit(s, req)
	respond(w, 202, map[string]string{"status": importImporting})
}

// commitCollectionImport installs the verified file through the shared import
// commit. The caller must already hold the importing claim; this function
// re-reads the authoritative request so a cancelled or stale snapshot can
// never write documents or files. An existing identical document is reused
// with its file, notes and progress intact — never deleted or replaced.
func (s *Server) commitCollectionImport(req CollectionImportRequest) {
	fresh, err := s.Store.CollectionImport(req.RequestID)
	if err != nil || fresh.Status != importImporting {
		// Ownership was lost (cancelled, replaced or already settled): the
		// stale snapshot must not perform any library write.
		return
	}
	req = fresh
	if req.CacheRelPath == "" {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": "缺少已下载文件"})
		return
	}
	cachePath := filepath.Join(s.Store.Root, filepath.FromSlash(req.CacheRelPath))
	// Cache reuse verifies ownership, size, magic and the real digest; the
	// database value alone is never trusted.
	_, full, size, err := hashImportFile(cachePath)
	if err != nil || !strings.EqualFold(full, req.ActualSHA256) {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": "下载文件校验失败，请重新下载"})
		return
	}
	if reason := paperLimit("pdf", size, cachePath); reason != "" {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": reason})
		return
	}
	docID := full[:32]
	// The library write gate is the single admission for library mutations.
	release, err := s.importGate.Acquire()
	if err != nil {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": err.Error()})
		return
	}
	defer release()
	// Reuse an existing identical document instead of re-inserting: keep its
	// library location, notes, annotations, chat and reading progress.
	if existing, e := s.Store.Document(docID); e == nil {
		if err := s.reuseExistingDocument(req, existing, full, cachePath); err != nil {
			_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": err.Error()})
			return
		}
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{
			"status": importImported, "document_id": docID, "library": existing.Library, "actual_sha256": full, "error": "", "condition": "",
		})
		return
	}
	staged := filepath.Join(s.Store.Root, "cache", "paper-collection", req.RequestID, "staged.pdf")
	if err := copyFile(cachePath, staged); err != nil {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": "无法准备入库文件"})
		return
	}
	d, err := s.installImportFile(context.Background(), importInstall{
		Owner:    "collection",
		OwnerRef: req.RequestID,
		Temp:     staged,
		Filename: importLocalName(req.ArXivID, req.DownloadVersion),
		Library:  "papers",
		DocID:    docID,
		FullSHA:  full,
		Size:     size,
	}, func(tx *sql.Tx) (Document, error) {
		return s.commitCollectionDocument(tx, req, docID, full, size)
	})
	if err != nil {
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": err.Error()})
		return
	}
	_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{
		"status": importImported, "document_id": d.ID, "library": d.Library, "error": "", "condition": "",
	})
}

// reuseExistingDocument verifies the real library file against the full
// digest before reporting success. A missing file is repaired from the
// verified download cache while keeping the document's reading state; a
// mismatching file refuses the reuse; a trashed document restores only on the
// user's confirmed condition, and a restore failure is a failure.
func (s *Server) reuseExistingDocument(req CollectionImportRequest, existing Document, full, cachePath string) error {
	file := s.Store.File(existing)
	if st, err := os.Stat(file); err == nil && st.Mode().IsRegular() {
		_, actual, _, err := hashImportFile(file)
		if err != nil {
			return fmt.Errorf("无法验证已有文件")
		}
		if !strings.EqualFold(actual, full) {
			return fmt.Errorf("已有文档的文件内容与本次文件不一致，已拒绝覆盖")
		}
		if err := s.Store.verifyExistingImport(existing.ID, full); err != nil {
			return err
		}
	} else {
		// The official file is missing: restore it only from the verified
		// cache copy, keeping the document and its reading data.
		if err := copyFile(cachePath, file); err != nil {
			return fmt.Errorf("已有文档缺少原文件，且无法用已下载副本修复")
		}
		_, actual, _, err := hashImportFile(file)
		if err != nil || !strings.EqualFold(actual, full) {
			_ = os.Remove(file)
			return fmt.Errorf("修复后的文件校验失败")
		}
	}
	if existing.DeletedAt != "" {
		// The user's confirm is the restore decision; a restore failure must
		// not look like a successful import.
		if req.Condition != "trash-restore" {
			return fmt.Errorf("相同文件在回收站中，需要确认恢复后入库")
		}
		restored, err := s.restore(existing.ID)
		if err != nil {
			return fmt.Errorf("无法恢复回收站中的文档")
		}
		if err := s.Store.enqueueDocument(restored); err != nil {
			return fmt.Errorf("无法创建解析任务")
		}
		return nil
	}
	return s.Store.verifyExistingImport(existing.ID, full)
}

func (s *Server) collectionImportCancel(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	// Preparing and downloading can be cancelled; a formal commit owns its
	// request until the real write ends and reports so instead of cancelling.
	cancelled, err := s.Store.claimImport(reqID, importActive, map[string]any{"status": importCancelled, "error": ""})
	if err != nil {
		collectionFail(w, 404, "query-invalid", "导入请求不存在")
		return
	}
	if !cancelled {
		req, err := s.Store.CollectionImport(reqID)
		if err != nil {
			collectionFail(w, 404, "query-invalid", "导入请求不存在")
			return
		}
		respond(w, 200, map[string]string{"status": req.Status})
		return
	}
	s.collection().cancelWork(reqID)
	respond(w, 200, map[string]string{"status": importCancelled})
}

func (s *Server) registerCollectionImportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/collection/imports", s.collectionImportCreate)
	mux.HandleFunc("GET /api/collection/imports", s.collectionImportList)
	mux.HandleFunc("GET /api/collection/imports/{id}", s.collectionImportGet)
	mux.HandleFunc("POST /api/collection/imports/{id}/confirm", s.collectionImportConfirm)
	mux.HandleFunc("POST /api/collection/imports/{id}/cancel", s.collectionImportCancel)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

// commitCollectionDocument writes the document, its initial processing row and
// the import association in one transaction, then links the request to the
// document so "imported" never appears without the association.
func (s *Server) commitCollectionDocument(tx *sql.Tx, req CollectionImportRequest, docID, fullSHA string, size int64) (Document, error) {
	var snapshot CollectionCandidate
	_ = json.Unmarshal(req.MetadataSnapshot, &snapshot)
	title := snapshot.Title
	if title == "" {
		title = req.ArXivID
	}
	meta, _ := json.Marshal(PaperMetadata{
		Lookup:  "done",
		Sources: map[string]string{"title": "arxiv", "creators": "arxiv"},
		Abstract: snapshot.Abstract,
		DOI:     snapshot.DOI,
		ArXiv:   req.ArXivID,
	})
	d := Document{
		ID:           docID,
		Type:         "pdf",
		Title:        title,
		Author:       joinNames(snapshot.Authors),
		Size:         size,
		CreatedAt:    now(),
		LastOpenedAt: now(),
		Library:      "papers",
	}
	d.Category, d.CategorySource, d.ClassificationStatus = "paper", "manual", "done"
	d.Tags, d.Folders = []string{}, []string{}
	_, err := tx.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at,category,category_source,classification_status,library,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		d.ID, d.Type, d.Title, d.Author, d.Size, d.CreatedAt, d.LastOpenedAt, d.Category, d.CategorySource, d.ClassificationStatus, d.Library, string(meta))
	if err != nil {
		return Document{}, err
	}
	var settings string
	_ = tx.QueryRow("SELECT value FROM settings WHERE key='reader'").Scan(&settings)
	p := initialDocumentProcessing(d, settings)
	body, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO document_processing(document_id,phase,status,body) VALUES(?,?,?,?)", d.ID, p.Phase, p.Status, body); err != nil {
		return Document{}, err
	}
	if _, err = tx.Exec("UPDATE collection_imports SET document_id=?,library=?,actual_sha256=? WHERE request_id=?", d.ID, d.Library, fullSHA, req.RequestID); err != nil {
		return Document{}, err
	}
	return d, nil
}

func joinNames(authors []CollectionAuthorJSON) string {
	names := make([]string, 0, len(authors))
	for _, a := range authors {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}
