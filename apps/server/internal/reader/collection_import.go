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
	}
	// Normal papers proceed to the commit; long papers wait for the user.
	if pageErr == nil && pages > largePaperPages {
		fields["status"] = importAwaitingConfirmation
		if ok, _ := s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, fields); !ok {
			return
		}
		return
	}
	fields["status"] = importDownloaded
	if ok, _ := s.Store.claimImport(req.RequestID, []string{importQueued, importDownloading}, fields); !ok {
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
	respond(w, 200, req)
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
// commit so documents, processing rows and the install log land together. An
// existing identical document is reused with its file, notes and progress
// intact — never deleted or replaced.
func (s *Server) commitCollectionImport(req CollectionImportRequest) {
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
		if verr := s.Store.verifyExistingImport(docID, full); verr != nil {
			_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{"status": importFailed, "error": verr.Error()})
			return
		}
		if restored, rerr := s.restore(docID); rerr == nil {
			_ = s.Store.enqueueDocument(restored)
		}
		_, _ = s.Store.claimImport(req.RequestID, []string{importImporting}, map[string]any{
			"status": importImported, "document_id": docID, "library": existing.Library, "actual_sha256": full, "error": "",
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
		"status": importImported, "document_id": d.ID, "library": d.Library, "error": "",
	})
}

func (s *Server) collectionImportCancel(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("id")
	// Cancelling closes the owned transport; the terminal state is claimed
	// from the active states so a late download cannot revive the request.
	cancelled, err := s.Store.claimImport(reqID, importActive, map[string]any{"status": importCancelled, "error": ""})
	if err != nil {
		collectionFail(w, 404, "query-invalid", "导入请求不存在")
		return
	}
	s.collection().cancelWork(reqID)
	req, err := s.Store.CollectionImport(reqID)
	if err != nil {
		collectionFail(w, 404, "query-invalid", "导入请求不存在")
		return
	}
	if !cancelled && req.Status == importImporting {
		// The commit owns the request until the real write finishes.
		respond(w, 200, map[string]string{"status": importImporting})
		return
	}
	respond(w, 200, map[string]string{"status": req.Status})
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
