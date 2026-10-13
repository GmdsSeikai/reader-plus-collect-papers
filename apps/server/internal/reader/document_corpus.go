package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const corpusVersion = "document-text-v1"

type corpusCoverage struct {
	Status          string   `json:"status"`
	TotalUnits      int      `json:"totalUnits"`
	SearchableUnits int      `json:"searchableUnits"`
	Missing         []string `json:"missingRanges"`
	Warnings        []string `json:"warnings"`
}
type documentPassage struct {
	ID        string          `json:"blockId"`
	Section   string          `json:"sectionId"`
	Label     string          `json:"label"`
	Text      string          `json:"text"`
	Location  json.RawMessage `json:"location"`
	Precision string          `json:"precision"`
}
type documentCorpus struct {
	DocumentID  string            `json:"-"`
	Fingerprint string            `json:"-"`
	Revision    string            `json:"corpusRevision"`
	Coverage    corpusCoverage    `json:"coverage"`
	Passages    []documentPassage `json:"passages"`
}

func (s *Server) corpusFingerprint(d Document) (string, error) {
	paths := []string{s.Store.File(d)}
	if d.Type == "epub" {
		root := filepath.Join(s.Store.Root, "cache", d.ID)
		manifestPath := filepath.Join(root, "manifest.json")
		paths = append(paths, manifestPath)
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return "", err
		}
		var manifest struct {
			ReadingOrder []struct {
				Href string `json:"href"`
			} `json:"readingOrder"`
		}
		if err = json.Unmarshal(data, &manifest); err != nil {
			return "", err
		}
		for _, link := range manifest.ReadingOrder {
			name, e := safeResource(link.Href)
			if e != nil {
				return "", e
			}
			paths = append(paths, filepath.Join(root, filepath.FromSlash(name)))
		}
	}
	paths = append(paths, filepath.Join(s.analysisDir(d.ID), "manifest.json"))
	var parts []string
	for _, p := range paths {
		info, e := os.Stat(p)
		if e == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", p, info.Size(), info.ModTime().UnixNano()))
		} else if !os.IsNotExist(e) {
			return "", e
		}
	}
	return sourceKey(corpusVersion + "\n" + strings.Join(parts, "\n")), nil
}
func (s *Server) documentCorpus(ctx context.Context, documentID string) (*documentCorpus, error) {
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	d, err := s.Store.Document(documentID)
	if err != nil || d.DeletedAt != "" {
		return nil, generationError(ErrorCanceled, "文档已不可用")
	}
	fingerprint, err := s.corpusFingerprint(d)
	if err != nil {
		return nil, err
	}
	var body string
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT body FROM document_corpora WHERE document_id=? AND fingerprint=?", d.ID, fingerprint).Scan(&body); err == nil {
		var c documentCorpus
		if json.Unmarshal([]byte(body), &c) == nil {
			c.DocumentID = d.ID
			c.Fingerprint = fingerprint
			return &c, nil
		}
	}
	c := &documentCorpus{DocumentID: d.ID, Fingerprint: fingerprint, Coverage: corpusCoverage{Status: "ready", Missing: []string{}, Warnings: []string{}}}
	if d.Type == "epub" {
		err = s.epubCorpus(ctx, d, c)
	} else {
		err = s.pdfCorpus(ctx, d, c)
	}
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if current, e := s.corpusFingerprint(d); e != nil || current != fingerprint {
		return nil, generationError(ErrorProtocol, "原文已更新，请重试")
	}
	raw, _ := json.Marshal(c)
	if len(raw) > 32<<20 {
		return nil, generationError(ErrorLimit, "文档原文索引超过限制")
	}
	c.Revision = sourceKey(string(raw))
	for i := range c.Passages {
		c.Passages[i].ID = c.Revision[:12] + fmt.Sprintf("-%d", i+1)
	}
	raw, _ = json.Marshal(c)
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM document_passages_fts WHERE document_id=?", d.ID); err != nil {
		return nil, err
	}
	for _, p := range c.Passages {
		if _, err = tx.ExecContext(ctx, "INSERT INTO document_passages_fts(document_id,revision,block_id,content) VALUES(?,?,?,?)", d.ID, c.Revision, p.ID, p.Text); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO document_corpora(document_id,fingerprint,revision,body) VALUES(?,?,?,?) ON CONFLICT(document_id) DO UPDATE SET fingerprint=excluded.fingerprint,revision=excluded.revision,body=excluded.body", d.ID, fingerprint, c.Revision, string(raw)); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}
func addCorpusPassage(c *documentCorpus, section, label, text string, location any, precision string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	raw, _ := json.Marshal(location)
	// Bounded slices preserve reachability for long paragraphs.
	for text != "" {
		n := min(len(text), 2400)
		for n > 0 && !utf8.ValidString(text[:n]) {
			n--
		}
		c.Passages = append(c.Passages, documentPassage{Section: section, Label: label, Text: text[:n], Location: raw, Precision: precision})
		text = text[n:]
	}
}
func (s *Server) epubCorpus(ctx context.Context, d Document, c *documentCorpus) error {
	blocks, err := s.readEPUBBlocks(d.ID)
	if err != nil {
		return generationError(ErrorConfiguration, "EPUB 原文索引无法读取")
	}
	seen := map[string]bool{}
	readable := map[string]bool{}
	for _, b := range blocks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seen[b.Location.Href] = true
		if b.Image == "" && strings.TrimSpace(b.Text) != "" {
			readable[b.Location.Href] = true
			addCorpusPassage(c, b.Location.Href, b.Label, b.Text, b.Location, "paragraph")
		}
	}
	// Include spine resources with no text, rather than infer coverage from blocks.
	data, e := os.ReadFile(filepath.Join(s.Store.Root, "cache", d.ID, "manifest.json"))
	if e != nil {
		return e
	}
	var m struct {
		ReadingOrder []struct {
			Href string `json:"href"`
		} `json:"readingOrder"`
	}
	if json.Unmarshal(data, &m) != nil {
		return fmt.Errorf("EPUB manifest invalid")
	}
	for _, r := range m.ReadingOrder {
		seen[r.Href] = true
	}
	c.Coverage.TotalUnits = len(seen)
	c.Coverage.SearchableUnits = len(readable)
	for href := range seen {
		if !readable[href] {
			c.Coverage.Missing = append(c.Coverage.Missing, href)
		}
	}
	sort.Strings(c.Coverage.Missing)
	if len(c.Coverage.Missing) > 0 {
		c.Coverage.Status = "partial"
		c.Coverage.Warnings = append(c.Coverage.Warnings, "部分 EPUB 资源没有可用文字")
	}
	return nil
}

type extractedPDF struct {
	Pages []struct {
		Page  int `json:"page"`
		Spans []struct {
			Text   string                                `json:"text"`
			Bounds struct{ X, Y, Width, Height float64 } `json:"bounds"`
		} `json:"spans"`
	} `json:"pages"`
	Outline []struct {
		Title string `json:"title"`
		Page  int    `json:"page"`
	} `json:"outline"`
}

func (s *Server) pdfCorpus(ctx context.Context, d Document, c *documentCorpus) error {
	var extracted extractedPDF
	worker := os.Getenv("READER_CORPUS_PROCESSOR")
	if worker == "" && os.Getenv("READER_PROCESSOR") != "" {
		worker = filepath.Join(filepath.Dir(os.Getenv("READER_PROCESSOR")), "corpus.mjs")
	}
	node := os.Getenv("READER_NODE")
	if worker != "" && node != "" {
		work, err := os.MkdirTemp(filepath.Join(s.Store.Root, "ai-work"), "corpus-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(work)
		out := filepath.Join(work, "text.json")
		cmd := exec.CommandContext(ctx, node, worker, "--input", s.Store.File(d), "--output", out)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "ELECTRON_RUN_AS_NODE=1")
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 3_000_000_000
		if err = cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return generationError(ErrorConfiguration, "全文文字提取失败，请检查 PDF 或本地解析程序")
		}
		info, err := os.Stat(out)
		if err != nil || info.Size() > 32<<20 {
			return generationError(ErrorLimit, "PDF 原文索引不可用或过大")
		}
		data, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		if json.Unmarshal(data, &extracted) != nil || len(extracted.Pages) < 1 {
			return fmt.Errorf("PDF corpus invalid")
		}
	}
	layout, layoutErr := s.readLayout(d.ID)
	if len(extracted.Pages) == 0 && layoutErr != nil {
		return generationError(ErrorConfiguration, "全文检索需要本地 PDF 文字提取程序，请重新构建 Reader")
	}
	if len(extracted.Pages) > 0 {
		c.Coverage.TotalUnits = len(extracted.Pages)
	} else {
		c.Coverage.TotalUnits = layout.Pages
		c.Coverage.Warnings = append(c.Coverage.Warnings, "当前仅可读取已解析页面，全文文字提取程序尚未就绪")
	}
	for number := 1; number <= c.Coverage.TotalUnits; number++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		section := fmt.Sprintf("p%d", number)
		label := fmt.Sprintf("第 %d 页", number)
		for _, o := range extracted.Outline {
			if o.Page <= number && o.Title != "" {
				label = o.Title
			}
		}
		start := len(c.Passages)
		// Full text extraction preserves spans omitted by a partial layout pass.
		if len(extracted.Pages) == 0 && layoutErr == nil && parsedPage(&layout, number) {
			for _, b := range layout.Blocks {
				if b.Page == number && !isPDFPageDecoration(b) && b.Image == "" {
					addCorpusPassage(c, section, label, b.Text, map[string]any{"type": "pdf", "page": number, "rects": []any{b.Bounds}}, "block")
				}
			}
		}
		if len(c.Passages) == start && len(extracted.Pages) >= number {
			page := extracted.Pages[number-1]
			if page.Page != number {
				return fmt.Errorf("PDF page order invalid")
			}
			var text strings.Builder
			flush := func() {
				addCorpusPassage(c, section, label, text.String(), map[string]any{"type": "pdf", "page": number}, "page")
				text.Reset()
			}
			for _, span := range page.Spans {
				if text.Len() > 1800 {
					flush()
				}
				text.WriteString(span.Text)
				text.WriteByte(' ')
			}
			flush()
		}
		if len(c.Passages) > start {
			c.Coverage.SearchableUnits++
		} else {
			c.Coverage.Missing = append(c.Coverage.Missing, section)
		}
	}
	if len(c.Coverage.Missing) > 0 {
		c.Coverage.Status = "partial"
		c.Coverage.Warnings = append(c.Coverage.Warnings, "缺失页可能为扫描或无文字页；当前未接入 OCR")
	}
	return nil
}
