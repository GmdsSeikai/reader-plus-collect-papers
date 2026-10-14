package reader

import (
	"encoding/json"
	"unicode/utf8"
)

const defaultDocumentContextTokens = 65536

// A conservative planning estimate, not billable usage. Providers do not expose
// a portable tokenizer or reliable context capacity in their model catalogs.
func estimateDocumentTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+2)/3 + other
}
func (s *Server) documentContextTokens() int {
	var body string
	var settings struct {
		DocumentContextTokens int `json:"documentContextTokens"`
	}
	if s.Store.DB.QueryRow("SELECT value FROM settings WHERE key='reader'").Scan(&body) == nil {
		_ = json.Unmarshal([]byte(body), &settings)
	}
	if settings.DocumentContextTokens < 8192 || settings.DocumentContextTokens > 1048576 {
		return defaultDocumentContextTokens
	}
	return settings.DocumentContextTokens
}

// Reserve one quarter for instructions, previous conversation, tool envelopes,
// and output. Callers subtract their actual prompt/overview estimate as well.
func documentSourceBudget(contextTokens int) int { return contextTokens * 3 / 4 }
func validOverviewText(text string, maxRunes int) bool {
	return utf8.ValidString(text) && utf8.RuneCountInString(text) <= maxRunes
}
