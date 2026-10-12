package scholar

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const searchAtom = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:opensearch="http://a9.com/-/spec/opensearch/1.1/" xmlns:arxiv="http://arxiv.org/schemas/atom">
 <opensearch:totalResults>1234</opensearch:totalResults>
 <entry>
  <id>http://arxiv.org/abs/2301.12345v2</id>
  <published>2023-01-02T00:00:00Z</published>
  <updated>2023-02-01T00:00:00Z</updated>
  <title>Detection with
  Knowledge</title>
  <summary>An abstract about object detection.</summary>
  <author><name>Ada Lovelace</name><arxiv:affiliation>Engine Lab</arxiv:affiliation></author>
  <author><name>Grace Hopper</name></author>
  <arxiv:doi>10.1/example</arxiv:doi>
  <arxiv:journal_ref>J. Example 1</arxiv:journal_ref>
  <category term="cs.CV"/>
  <category term="cs.AI"/>
 </entry>
</feed>`

func TestArxivQueryBuilderBranches(t *testing.T) {
	dates := DateRange{From: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	b := ArxivQueryBuilder{
		KeywordGroups:   [][]string{{"object detection", `visual "detection"`}, {"knowledge distillation"}},
		Categories:      []string{"cs.CV"},
		GroupOperatorOR: true,
	}
	q := b.TopicQuery(dates)
	if !strings.HasPrefix(q, "submittedDate:[202609120000 TO 202610120000]") {
		t.Fatalf("date window missing: %s", q)
	}
	if !strings.Contains(q, `ti:"object detection" OR abs:"object detection"`) {
		t.Fatalf("word terms missing: %s", q)
	}
	if !strings.Contains(q, `ti:"visual detection"`) {
		t.Fatalf("quotes not normalized: %s", q)
	}
	if !strings.Contains(q, ") AND (cat:\"cs.CV\"") {
		t.Fatalf("category constraint missing: %s", q)
	}
	b.GroupOperatorOR = false
	if !strings.Contains(b.TopicQuery(dates), ") AND (") {
		t.Fatalf("AND mode not applied: %s", b.TopicQuery(dates))
	}
	// Authors get their own branch: only au terms and dates.
	a := ArxivQueryBuilder{Authors: []string{"Ada Lovelace", "A. Lovelace"}}
	aq := a.AuthorQuery(dates)
	if !strings.Contains(aq, `au:"Ada Lovelace" OR au:"A. Lovelace"`) {
		t.Fatalf("author terms: %s", aq)
	}
	if strings.Contains(aq, "ti:") || strings.Contains(aq, "cat:") {
		t.Fatalf("author branch leaks topic terms: %s", aq)
	}
}

func TestParseArxivSearchKeepsDatesAndIdentity(t *testing.T) {
	page, err := ParseArxivSearch([]byte(searchAtom))
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalResults != 1234 {
		t.Fatalf("total = %d", page.TotalResults)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("entries = %d", len(page.Entries))
	}
	e := page.Entries[0]
	if e.ID != "2301.12345" || e.Version != 2 {
		t.Fatalf("identity = %q v%d", e.ID, e.Version)
	}
	if !e.Published.Equal(time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("published = %v", e.Published)
	}
	if !e.Updated.Equal(time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated = %v", e.Updated)
	}
	if len(e.Authors) != 2 || e.Authors[0].Name != "Ada Lovelace" || e.Authors[0].Affiliation != "Engine Lab" {
		t.Fatalf("authors = %+v", e.Authors)
	}
	if len(e.Categories) != 2 {
		t.Fatalf("categories = %v", e.Categories)
	}
	if e.DOI != "10.1/example" || e.JournalRef != "J. Example 1" {
		t.Fatalf("doi/journal = %q %q", e.DOI, e.JournalRef)
	}
}

func TestParseArxivSearchRejectsErrorEntry(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>http://arxiv.org/api/errors#bad</id><title>Error</title></entry></feed>`
	if _, err := ParseArxivSearch([]byte(body)); err == nil {
		t.Fatal("error entry must fail the page, not count as empty")
	}
}

func TestSourceGateSerializesAndSpaces(t *testing.T) {
	g := newSourceGateAt(filepath.Join(t.TempDir(), "gate.lock"), 200*time.Millisecond)
	starts := make(chan time.Time, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			starts <- time.Now()
			time.Sleep(30 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	close(starts)
	var times []time.Time
	for ts := range starts {
		times = append(times, ts)
	}
	if len(times) != 2 {
		t.Fatalf("starts = %d", len(times))
	}
	if times[1].Sub(times[0]) < 150*time.Millisecond {
		t.Fatalf("interval not honored: %v", times[1].Sub(times[0]))
	}
}

func TestSourceGateWaitIsCancellable(t *testing.T) {
	g := newSourceGateAt(filepath.Join(t.TempDir(), "gate.lock"), time.Second)
	release, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(ctx); err == nil {
		t.Fatal("second acquire inside the interval must be cancellable")
	}
}

func TestSourceGateSharedAcrossGatesOnSameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	a := newSourceGateAt(path, 150*time.Millisecond)
	b := newSourceGateAt(path, 150*time.Millisecond)
	release, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	release()
	begin := time.Now()
	release2, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release2()
	if time.Since(begin) < 100*time.Millisecond {
		t.Fatalf("cross-gate interval not shared via file: %v", time.Since(begin))
	}
}

func TestDownloadArxivVersionStrict(t *testing.T) {
	c := New(0)
	if err := c.strictDownloadURL(mustURL(t, "https://arxiv.org/pdf/2301.12345v2")); err != nil {
		t.Fatalf("arxiv https rejected: %v", err)
	}
	if err := c.strictDownloadURL(mustURL(t, "http://arxiv.org/pdf/x")); err == nil {
		t.Fatal("http accepted")
	}
	if err := c.strictDownloadURL(mustURL(t, "https://evil.example/pdf/x")); err == nil {
		t.Fatal("non-allow-listed host accepted")
	}
	if err := c.strictDownloadURL(mustURL(t, "https://127.0.0.1/pdf/x")); err == nil {
		t.Fatal("loopback accepted")
	}
}

type stubTransport struct {
	body   string
	status int
	calls  []string
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls = append(s.calls, req.URL.String())
	code := s.status
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode:    code,
		Body:          io.NopCloser(strings.NewReader(s.body)),
		ContentLength: int64(len(s.body)),
		Request:       req,
	}, nil
}

func TestDownloadArxivVersionPinsVersionAndValidatesPDF(t *testing.T) {
	// The transport stands in for the network; URL checks run on the pinned
	// request URL itself.
	stub := &stubTransport{body: "%PDF-1.4\nok\n%%EOF"}
	c := New(50 << 20)
	c.HTTP = &http.Client{Transport: stub}
	dir := t.TempDir()
	dest, err := os.Create(filepath.Join(dir, "paper.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if err := c.DownloadArxivVersion(context.Background(), "2301.12345", 2, dest, 50<<20); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 || !strings.HasSuffix(stub.calls[0], "/2301.12345v2") {
		t.Fatalf("calls = %v, want one pinned version URL", stub.calls)
	}
	// A missing version is a hard failure: never fall back to latest.
	stub.body = "<html>nope</html>"
	if err := c.DownloadArxivVersion(context.Background(), "2301.12345", 3, dest, 50<<20); err == nil {
		t.Fatal("html body accepted")
	}
	stub.body = ""
	if err := c.DownloadArxivVersion(context.Background(), "2301.12345", 3, dest, 50<<20); err == nil {
		t.Fatal("empty body accepted")
	}
	if err := c.DownloadArxivVersion(context.Background(), "2301.12345", 0, dest, 50<<20); err == nil {
		t.Fatal("unversioned download accepted")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
