package scholar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// CitationCount is one verified Semantic Scholar cumulative count. Unknown
// stays nil; a literal zero is a real zero. Counts may decrease.
type CitationCount struct {
	Count          *int
	ServicePaperID string
	ArXivID        string
	Found          bool
}

// s2BatchPaper is one entry of the batch response. CitationCount stays raw so
// floats, strings and negatives are rejected instead of coerced to zero.
type s2BatchPaper struct {
	PaperID       string          `json:"paperId"`
	CitationCount json.RawMessage `json:"citationCount"`
	ExternalIDs   struct {
		ArXiv string `json:"ArXiv"`
	} `json:"externalIds"`
}

// parseCitationCount accepts an explicit JSON integer >= 0 only.
func parseCitationCount(raw json.RawMessage) *int {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	switch n := v.(type) {
	case float64:
		// JSON numbers decode as float64; only integral values count.
		if n < 0 || n != float64(int(n)) {
			return nil
		}
		count := int(n)
		return &count
	default:
		return nil
	}
}

// CitationBatch queries cumulative counts by exact arXiv identity
// (ARXIV:<id>), matching responses by external ID instead of array position.
// apiKey may be empty; missing or malformed counts stay unknown, never zero.
// The caller holds the shared source gate.
func (c *Client) CitationBatch(ctx context.Context, ids []string, apiKey string) (map[string]CitationCount, error) {
	out := map[string]CitationCount{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 100 {
		ids = ids[:100]
	}
	reqIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		stable, err := NormalizeStableArxivID(id)
		if err != nil {
			continue
		}
		reqIDs = append(reqIDs, "ARXIV:"+stable)
		out[stable] = CitationCount{ArXivID: stable}
	}
	if len(reqIDs) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string][]string{"ids": reqIDs})
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.SemanticScholar, "/")+"/batch?fields=paperId,externalIds,citationCount", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, Error{fmt.Sprintf("Semantic Scholar 返回 %d", res.StatusCode)}
	}
	var papers []s2BatchPaper
	if err := json.NewDecoder(res.Body).Decode(&papers); err != nil {
		return out, Error{"Semantic Scholar 返回了无法识别的数据"}
	}
	// Responses map by external ID; array order and missing entries are not
	// positional promises.
	for _, p := range papers {
		stable, err := NormalizeStableArxivID(p.ExternalIDs.ArXiv)
		if err != nil {
			continue
		}
		entry, ok := out[stable]
		if !ok {
			continue
		}
		entry.Found = true
		entry.ServicePaperID = p.PaperID
		entry.Count = parseCitationCount(p.CitationCount)
		out[stable] = entry
	}
	return out, nil
}
