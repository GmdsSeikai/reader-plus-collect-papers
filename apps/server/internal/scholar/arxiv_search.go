package scholar

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ArxivSearchEntry is one Atom search entry with full first-submission and
// version dates. Published is the first submission; Updated is the version
// revision time and never substitutes for window decisions.
type ArxivSearchEntry struct {
	Authors     []ArxivAuthor
	Categories  []string
	ID          string
	Title       string
	Abstract    string
	DOI         string
	JournalRef  string
	Published   time.Time
	Updated     time.Time
	Version     int
}

// ArxivAuthor keeps the source order and any affiliation the source provided.
type ArxivAuthor struct {
	Name        string
	Affiliation string
}

// ArxivSearchPage is one page of a search response.
type ArxivSearchPage struct {
	Entries      []ArxivSearchEntry
	TotalResults int
}

// ArxivQueryBuilder turns keyword groups, categories and authors into the
// supported query language. User words are quoted; the caller never passes
// hand-written source expressions.
type ArxivQueryBuilder struct {
	KeywordGroups   [][]string
	Categories      []string
	Authors         []string // name or alias; one branch per call
	GroupOperatorOR bool
}

func quoteTerm(word string) string {
	return `"` + strings.ReplaceAll(strings.TrimSpace(word), `"`, "") + `"`
}

// DateRange is the UTC minute-precision submittedDate window, both ends
// inclusive per the source convention.
type DateRange struct {
	From, To time.Time
}

func (d DateRange) String() string {
	return fmt.Sprintf("submittedDate:[%s TO %s]", d.From.UTC().Format("200601021504"), d.To.UTC().Format("200601021504"))
}

// TopicBody is the topic branch without the date window.
func (b ArxivQueryBuilder) TopicBody() string {
	groups := make([]string, 0, len(b.KeywordGroups))
	for _, g := range b.KeywordGroups {
		terms := make([]string, 0, len(g))
		for _, w := range g {
			if strings.TrimSpace(w) == "" {
				continue
			}
			q := quoteTerm(w)
			terms = append(terms, "ti:"+q+" OR abs:"+q)
		}
		if len(terms) == 0 {
			continue
		}
		groups = append(groups, "("+strings.Join(terms, " OR ")+")")
	}
	if len(groups) == 0 {
		return ""
	}
	joiner := " OR "
	if !b.GroupOperatorOR {
		joiner = " AND "
	}
	body := strings.Join(groups, joiner)
	if len(b.Categories) > 0 {
		cats := make([]string, 0, len(b.Categories))
		for _, c := range b.Categories {
			if strings.TrimSpace(c) != "" {
				cats = append(cats, "cat:"+quoteTerm(c))
			}
		}
		if len(cats) > 0 {
			body = "(" + body + ") AND (" + strings.Join(cats, " OR ") + ")"
		}
	}
	return body
}

// TopicQuery is the topic branch constrained to a date window.
func (b ArxivQueryBuilder) TopicQuery(dates DateRange) string {
	body := b.TopicBody()
	if body == "" {
		return ""
	}
	return dates.String() + " AND " + body
}

// AuthorBody is one author branch without the date window.
func (b ArxivQueryBuilder) AuthorBody() string {
	if len(b.Authors) == 0 {
		return ""
	}
	terms := make([]string, 0, len(b.Authors))
	for _, name := range b.Authors {
		if strings.TrimSpace(name) == "" {
			continue
		}
		terms = append(terms, "au:"+quoteTerm(name))
	}
	if len(terms) == 0 {
		return ""
	}
	return "(" + strings.Join(terms, " OR ") + ")"
}

// AuthorQuery is one author branch constrained to a date window.
func (b ArxivQueryBuilder) AuthorQuery(dates DateRange) string {
	body := b.AuthorBody()
	if body == "" {
		return ""
	}
	return dates.String() + " AND " + body
}

type atomFeed struct {
	TotalResults string `xml:"http://a9.com/-/spec/opensearch/1.1/ totalResults"`
	Entries      []struct {
		ID          string   `xml:"id"`
		Title       string   `xml:"title"`
		Summary     string   `xml:"summary"`
		Published   string   `xml:"published"`
		Updated     string   `xml:"updated"`
		DOI         string   `xml:"http://arxiv.org/schemas/atom doi"`
		JournalRef  string   `xml:"http://arxiv.org/schemas/atom journal_ref"`
		Categories  []struct {
			Term string `xml:"term,attr"`
		} `xml:"category"`
		Authors []struct {
			Name        string `xml:"name"`
			Affiliation string `xml:"http://arxiv.org/schemas/atom affiliation"`
		} `xml:"author"`
	} `xml:"entry"`
}

// ParseArxivSearch parses a search Atom body. An HTTP 200 containing an error
// entry or no entries with a declared total above zero is a failure, not an
// empty page: callers must not advance coverage on it.
func ParseArxivSearch(data []byte) (ArxivSearchPage, error) {
	var feed atomFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return ArxivSearchPage{}, Error{"arXiv 返回了无法识别的数据"}
	}
	page := ArxivSearchPage{TotalResults: -1}
	if feed.TotalResults != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(feed.TotalResults)); err == nil {
			page.TotalResults = n
		}
	}
	for _, e := range feed.Entries {
		if strings.Contains(e.ID, "api/errors") {
			return ArxivSearchPage{}, Error{"arXiv 返回了错误响应"}
		}
		id, version := splitArxivRef(e.ID)
		if id == "" || clean(e.Title) == "" {
			continue
		}
		entry := ArxivSearchEntry{
			ID:         id,
			Version:    version,
			Title:      clean(e.Title),
			Abstract:   e.Summary,
			DOI:        clean(e.DOI),
			JournalRef: clean(e.JournalRef),
		}
		entry.Abstract = strings.TrimSpace(entry.Abstract)
		for _, c := range e.Categories {
			if c.Term != "" {
				entry.Categories = append(entry.Categories, c.Term)
			}
		}
		for _, a := range e.Authors {
			name := clean(a.Name)
			if name == "" {
				continue
			}
			entry.Authors = append(entry.Authors, ArxivAuthor{Name: name, Affiliation: clean(a.Affiliation)})
		}
		if t, err := time.Parse(time.RFC3339, e.Published); err == nil {
			entry.Published = t
		}
		if t, err := time.Parse(time.RFC3339, e.Updated); err == nil {
			entry.Updated = t
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, nil
}

// splitArxivRef extracts the stable ID and version from an Atom entry ID URL.
func splitArxivRef(raw string) (string, int) {
	s := raw
	if i := strings.LastIndex(s, "/abs/"); i >= 0 {
		s = s[i+5:]
	}
	s = strings.TrimSuffix(s, ".pdf")
	version := 0
	if i := strings.LastIndex(s, "v"); i > 0 {
		if n, err := strconv.Atoi(s[i+1:]); err == nil {
			version = n
			s = s[:i]
		}
	}
	if s == "" {
		return "", 0
	}
	return s, version
}

// ArxivSearch queries the official Atom API sorted by first submission date,
// newest first. The caller is expected to hold the shared source gate.
func (c *Client) ArxivSearch(ctx context.Context, query string, start, maxResults int) (ArxivSearchPage, error) {
	if maxResults < 1 {
		maxResults = 100
	}
	if maxResults > 100 {
		maxResults = 100
	}
	params := url.Values{}
	params.Set("search_query", query)
	params.Set("start", strconv.Itoa(start))
	params.Set("max_results", strconv.Itoa(maxResults))
	params.Set("sortBy", "submittedDate")
	params.Set("sortOrder", "descending")
	data, _, _, err := c.get(ctx, c.ArXivAPI+"?"+params.Encode(), "application/atom+xml", 8<<20)
	if err != nil {
		return ArxivSearchPage{}, err
	}
	return ParseArxivSearch(data)
}

// ArxivVersionPDFURL is the only download URL shape collection imports use.
func ArxivVersionPDFURL(base, id string, version int) string {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + id + "v" + strconv.Itoa(version)
}

// ErrDownloadRejected marks a download refused by validation rather than a
// transient network error.
var ErrDownloadRejected = errors.New("下载内容未通过校验")

// DownloadArxivVersion fetches exactly one pinned arXiv version as a PDF,
// streaming into dest. It never falls back to the latest version: a missing
// version fails. Every redirect hop is re-checked (HTTPS, allow-listed hosts,
// no private targets), at most 5 hops. Size is bounded by Content-Length and
// by the actual bytes read.
func (c *Client) DownloadArxivVersion(ctx context.Context, id string, version int, dest *os.File, maxBytes int64) error {
	if version < 1 {
		return fmt.Errorf("%w: 未指定版本", ErrDownloadRejected)
	}
	if _, err := NormalizeStableArxivID(id); err != nil {
		return fmt.Errorf("%w: arXiv ID 无效", ErrDownloadRejected)
	}
	startURL := ArxivVersionPDFURL(c.ArXivPDF, id, version)
	parsed, err := url.Parse(startURL)
	if err != nil {
		return fmt.Errorf("%w: 下载地址无效", ErrDownloadRejected)
	}
	if err := c.strictDownloadURL(parsed); err != nil {
		return err
	}
	client := *c.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("%w: 重定向次数过多", ErrDownloadRejected)
		}
		if err := c.strictDownloadURL(req.URL); err != nil {
			return err
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, startURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/pdf")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: 来源返回 %d", ErrDownloadRejected, res.StatusCode)
	}
	if res.ContentLength > maxBytes {
		return fmt.Errorf("%w: 文件超过大小限制", ErrDownloadRejected)
	}
	// A retry reuses the destination file; never mix bytes with a previous attempt.
	if err := dest.Truncate(0); err != nil {
		return err
	}
	if _, err := dest.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(dest, io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: 文件为空", ErrDownloadRejected)
	}
	if n > maxBytes {
		return fmt.Errorf("%w: 文件超过大小限制", ErrDownloadRejected)
	}
	if _, err := dest.Seek(0, io.SeekStart); err != nil {
		return err
	}
	head := make([]byte, 5)
	if _, err := io.ReadFull(dest, head); err != nil || string(head) != "%PDF-" {
		return fmt.Errorf("%w: 内容不是 PDF", ErrDownloadRejected)
	}
	return nil
}

var downloadHosts = map[string]bool{"arxiv.org": true, "www.arxiv.org": true, "export.arxiv.org": true}

// strictDownloadURL enforces HTTPS and the download allow-list on every hop.
// AllowPrivateHosts exists only for fixtures and is never enabled in production.
func (c *Client) strictDownloadURL(u *url.URL) error {
	if c.AllowPrivateHosts {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: 只允许 HTTPS 下载", ErrDownloadRejected)
	}
	host := strings.ToLower(u.Hostname())
	if !downloadHosts[host] {
		return fmt.Errorf("%w: 不允许的下载来源", ErrDownloadRejected)
	}
	if err := rejectPrivateHost(host); err != nil {
		return err
	}
	return nil
}

func rejectPrivateHost(host string) error {
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return fmt.Errorf("%w: 不能访问本机或局域网地址", ErrDownloadRejected)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("%w: 不能访问本机或局域网地址", ErrDownloadRejected)
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()
}

// NormalizeStableArxivID validates and strips a version suffix from an arXiv
// ID used for collection identity.
func NormalizeStableArxivID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	id = strings.TrimPrefix(id, "arxiv:")
	id = strings.TrimPrefix(id, "arXiv:")
	for len(id) > 0 {
		i := strings.LastIndex(id, "v")
		if i <= 0 {
			break
		}
		if n, err := strconv.Atoi(id[i+1:]); err == nil && n > 0 {
			id = id[:i]
			continue
		}
		break
	}
	if id == "" {
		return "", errors.New("arXiv ID 无效")
	}
	return id, nil
}
