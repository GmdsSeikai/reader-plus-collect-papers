package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"reader.local/server/internal/scholar"
)

// Channels of the dual discovery windows.
const (
	channelLatest = "latest"
	channelHot    = "hot"
)

// CollectionBranch is one independent discovery query: a topic branch or one
// watched author. Authors never attach topic words or categories.
type CollectionBranch struct {
	Query      string
	BranchHash string
	Channel    string
	Name       string
}

// BuildBranches expands a task config into topic/author branches for both
// channels. Categories only constrain the topic branch.
func BuildBranches(cfg CollectionTaskConfig, latest, hot DateWindow) []CollectionBranch {
	out := []CollectionBranch{}
	add := func(channel, name, query string, dates scholar.DateRange) {
		if query == "" {
			return
		}
		b := CollectionBranch{Channel: channel, Name: name, Query: query}
		sum := sha256.Sum256([]byte(channel + "\x00" + name + "\x00" + query))
		b.BranchHash = hex.EncodeToString(sum[:8])
		out = append(out, b)
	}
	for _, window := range []struct {
		channel string
		dates   scholar.DateRange
	}{{channelLatest, latest.Range()}, {channelHot, hot.Range()}} {
		builder := scholar.ArxivQueryBuilder{
			KeywordGroups:   cfg.KeywordGroups,
			Categories:      cfg.Categories,
			GroupOperatorOR: cfg.GroupOperatorOR,
		}
		add(window.channel, "topic", builder.TopicQuery(window.dates), window.dates)
		for _, a := range cfg.Authors {
			names := append([]string{a.Name}, a.Aliases...)
			ab := scholar.ArxivQueryBuilder{Authors: names}
			add(window.channel, "author:"+a.Name, ab.AuthorQuery(window.dates), window.dates)
		}
	}
	return out
}

// DateWindow is a closed UTC discovery window computed from the run's fixed
// now. Latest uses a day back-off; hot uses whole calendar months.
type DateWindow struct {
	From, To time.Time
}

func (w DateWindow) Range() scholar.DateRange { return scholar.DateRange{From: w.From, To: w.To} }

// PlanWindows computes the two channels from the run's fixed clock. The hot
// window subtracts whole calendar months and clamps the day to the target
// month end (Mar 31 minus one month is Feb 28/29, never Mar 03).
func PlanWindows(now time.Time, latestDays, hotMonths int) (DateWindow, DateWindow) {
	now = now.UTC()
	latest := DateWindow{From: now.AddDate(0, 0, -latestDays), To: now}
	hot := DateWindow{From: subCalendarMonths(now, hotMonths), To: now}
	return latest, hot
}

// subCalendarMonths returns t moved back months calendar months, keeping the
// UTC clock time and clamping the day to the target month's last day.
func subCalendarMonths(t time.Time, months int) time.Time {
	year, month, day := t.Date()
	// Months-since-epoch arithmetic avoids negative-division pitfalls.
	abs := year*12 + int(month) - 1 - months
	year = abs / 12
	month = time.Month(abs%12 + 1)
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// SlicePlan is one coverage unit to scan. P1 uses one slice per branch and
// channel covering the full window; P2 refines totals into time buckets.
type SlicePlan struct {
	Branch CollectionBranch
	From   time.Time
	To     time.Time
}

// BuildSlices expands branches into scan slices.
func BuildSlices(branches []CollectionBranch, latest, hot DateWindow) []SlicePlan {
	slices := []SlicePlan{}
	for _, b := range branches {
		w := latest
		if b.Channel == channelHot {
			w = hot
		}
		slices = append(slices, SlicePlan{Branch: b, From: w.From, To: w.To})
	}
	return slices
}

// RuleScore matches the reference ranking: title hits weigh double; each group
// counts at most once per field. Verified phrases only — nothing is invented.
func RuleScore(cfg CollectionTaskConfig, title, abstract string) (int, []CollectionEvidence) {
	groups := cfg.KeywordGroups
	if len(groups) == 0 {
		return 0, nil
	}
	titleLower := strings.ToLower(title)
	abstractLower := strings.ToLower(abstract)
	score := 0
	evidence := []CollectionEvidence{}
	for _, g := range groups {
		titleHit, abstractHit := false, false
		for _, w := range g {
			word := strings.ToLower(strings.TrimSpace(w))
			if word == "" {
				continue
			}
			if !titleHit && strings.Contains(titleLower, word) {
				titleHit = true
				if q := verifyQuote(title, w); q != "" {
					evidence = append(evidence, CollectionEvidence{Field: "title", Quote: q})
				}
			}
			if !abstractHit && strings.Contains(abstractLower, word) {
				abstractHit = true
				if q := verifyQuote(abstract, w); q != "" {
					evidence = append(evidence, CollectionEvidence{Field: "abstract", Quote: q})
				}
			}
		}
		if titleHit {
			score += 2
		}
		if abstractHit {
			score++
		}
	}
	return score * 100 / (3 * len(groups)), evidence
}

// verifyQuote returns a short continuous excerpt containing the phrase in the
// original text, with only whitespace normalized. Empty when not verifiable.
func verifyQuote(original, phrase string) string {
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return ""
	}
	idx := strings.Index(strings.ToLower(original), strings.ToLower(phrase))
	if idx < 0 {
		return ""
	}
	start := idx
	if start > 40 {
		start = idx - 20
	} else {
		start = 0
	}
	end := idx + len(phrase)
	if end < len(original)-20 {
		end += 20
	} else {
		end = len(original)
	}
	return strings.Join(strings.Fields(original[start:end]), " ")
}

// AuthorHits reports whether an author branch matched the entry, keeping the
// source-provided author order and names for the match evidence.
func AuthorHits(name string, authors []scholar.ArxivAuthor) bool {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return false
	}
	for _, a := range authors {
		if strings.ToLower(a.Name) == target {
			return true
		}
	}
	return false
}

// RangeSnapshot is the fixed range recorded on the run.
type RangeSnapshot struct {
	LatestFrom string `json:"latestFrom"`
	LatestTo   string `json:"latestTo"`
	HotFrom    string `json:"hotFrom"`
	HotTo      string `json:"hotTo"`
}

func snapshotRanges(latest, hot DateWindow) string {
	b, _ := json.Marshal(RangeSnapshot{
		LatestFrom: latest.From.Format(time.RFC3339),
		LatestTo:   latest.To.Format(time.RFC3339),
		HotFrom:    hot.From.Format(time.RFC3339),
		HotTo:      hot.To.Format(time.RFC3339),
	})
	return string(b)
}

// discoveryPage converts one search page into candidates and matches at the
// task's revision. Candidates that fail identity checks skip the page entry
// without being invented.
func discoveryPage(task CollectionTask, branch CollectionBranch, page scholar.ArxivSearchPage, nowStr string) ([]CollectionCandidate, []CollectionMatch) {
	candidates := []CollectionCandidate{}
	matches := []CollectionMatch{}
	seen := map[string]bool{}
	for _, e := range page.Entries {
		id, err := NormalizeArXivID(e.ID)
		if err != nil || e.Published.IsZero() {
			continue
		}
		authors := make([]CollectionAuthorJSON, 0, len(e.Authors))
		names := make([]string, 0, len(e.Authors))
		for _, a := range e.Authors {
			authors = append(authors, CollectionAuthorJSON{Name: a.Name, Affiliation: a.Affiliation})
			names = append(names, a.Name)
		}
		version := e.Version
		if version < 1 {
			version = 1
		}
		c := CollectionCandidate{
			ArXivID:       id,
			Title:         e.Title,
			Abstract:      e.Abstract,
			Authors:       authors,
			Categories:    e.Categories,
			PublishedAt:   e.Published.Format(time.RFC3339),
			UpdatedAt:     e.Updated.Format(time.RFC3339),
			LatestVersion: version,
			DOI:           e.DOI,
			JournalRef:    e.JournalRef,
			CanonicalVer:  collectionCanonicalVersion,
			ContentHash:   CandidateContentHash(e.Title, e.Abstract, names, e.Categories),
			FirstSeenAt:   nowStr,
			LastSeenAt:    nowStr,
		}
		if !seen[id] {
			seen[id] = true
			candidates = append(candidates, c)
		}
		score, evidence := RuleScore(task.Config, e.Title, e.Abstract)
		if strings.HasPrefix(branch.Name, "author:") {
			// The branch identity is the author query itself; no title or
			// abstract phrase is invented for the relationship.
			evidence = nil
		}
		// The source returning the entry on this branch is the discovery fact;
		// verified phrases decorate the match and are never fabricated.
		matches = append(matches, CollectionMatch{
			TaskID:        task.ID,
			QueryRevision: task.QueryRevision,
			ArXivID:       id,
			Channel:       branch.Channel,
			BranchHash:    branch.BranchHash,
			Evidence:      evidence,
			RuleScore:     score,
			FirstSeenAt:   nowStr,
			LastSeenAt:    nowStr,
		})
	}
	return candidates, matches
}
