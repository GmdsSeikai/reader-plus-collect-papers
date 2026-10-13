package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Collection schema version owned by collection_meta, independent of the
// library's global schema. Writes are refused for rows from a newer module.
const collectionSchemaVersion = 2

// Canonical content version 1: fixed field order UTF-8 JSON of title, abstract,
// ordered author names and categories, with whitespace-normalized values.
const collectionCanonicalVersion = 1

const collectionPromptVersion = 1

// collectionResultVersion identifies the analysis result JSON contract.
const collectionResultVersion = 1

// Status vocabularies. Every value must have real writers and consumers;
// defining an enum alone does not mean the state machine is complete.
const (
	runQueued      = "queued"
	runRunning     = "running"
	runSucceeded   = "succeeded"
	runPartial     = "partial"
	runFailed      = "failed"
	runCancelled   = "cancelled"
	runInterrupted = "interrupted"

	slicePending     = "pending"
	sliceRunning     = "running"
	sliceCompleted   = "completed"
	sliceFailed      = "failed"
	sliceInterrupted = "interrupted"

	analysisPending     = "pending"
	analysisRunning     = "running"
	analysisSucceeded   = "succeeded"
	analysisFailed      = "failed"
	analysisInterrupted = "interrupted"
	analysisBlocked     = "blocked"

	importQueued               = "queued"
	importDownloading          = "downloading"
	importDownloaded           = "downloaded"
	importAwaitingConfirmation = "awaiting_confirmation"
	importImporting            = "importing"
	importImported             = "imported"
	importFailed               = "failed"
	importCancelled            = "cancelled"
	importInterrupted          = "interrupted"

	attemptReserved    = "reserved"
	attemptDispatching = "dispatching"
	attemptSent        = "sent"
	attemptReleased    = "released"
	attemptSettled     = "settled"
)

var (
	errCollectionSchemaTooNew = errors.New("采集数据由更新版本创建，本版本不能修改")
	errTaskConflict           = errors.New("任务已被其他修改更新")
	errInvalidTaskConfig      = errors.New("任务配置无效")
	errInvalidArxivID         = errors.New("arXiv ID 无效")
	errCollectionPaused       = errors.New("采集已暂停")
	errCollectionSourceLimit  = errors.New("来源上限，未覆盖")
	errImportBusy             = errors.New("同一篇论文已有进行中的导入")
)

var (
	arxivNewID = regexp.MustCompile(`^\d{4}\.\d{4,5}$`)
	arxivOldID = regexp.MustCompile(`^[a-z\-]+(?:\.[A-Z]{2})?/\d{7}$`)
	arxivVerID = regexp.MustCompile(`v\d+$`)
)

// CollectionTaskAuthor is one watched author; aliases are name variants that
// the source search also matches. Identity disambiguation is out of scope.
type CollectionTaskAuthor struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// CollectionTaskConfig is the versioned semantic configuration of a task.
// Changes to research material raise the task's query_revision separately.
type CollectionTaskConfig struct {
	KeywordGroups    [][]string             `json:"keyword_groups"`
	Authors          []CollectionTaskAuthor `json:"authors"`
	Categories       []string               `json:"categories"`
	ResearchTopic    string                 `json:"research_topic"`
	NotInterested    string                 `json:"not_interested"`
	Version          int                    `json:"v"`
	GroupOperatorOR  bool                   `json:"group_operator_or"`
	LatestWindowDays int                    `json:"latest_window_days"`
	HotWindowMonths  int                    `json:"hot_window_months"`
	FrequencyHours   int                    `json:"frequency_hours"`
}

// CollectionTask is a stored discovery task with a stable UUID.
type CollectionTask struct {
	Config        CollectionTaskConfig `json:"config"`
	Name          string               `json:"name"`
	ID            string               `json:"id"`
	QueryRevision int                  `json:"queryRevision"`
	SemanticHash  string               `json:"semanticHash"`
	NextRunAt     string               `json:"nextRunAt,omitempty"`
	CreatedAt     string               `json:"createdAt"`
	UpdatedAt     string               `json:"updatedAt"`
	Priority      int                  `json:"priority"`
	Enabled       bool                 `json:"enabled"`
}

// CollectionAuthorJSON is the stored source order of one candidate author.
type CollectionAuthorJSON struct {
	Name        string `json:"name"`
	Affiliation string `json:"affiliation,omitempty"`
}

// CollectionEvidence is a phrase verified to occur in the original title or
// abstract at discovery time. Unverifiable hits are never invented.
type CollectionEvidence struct {
	Field string `json:"field"`
	Quote string `json:"quote"`
}

// CollectionCandidate is one paper discovered from arXiv, keyed by stable ID.
type CollectionCandidate struct {
	Authors       []CollectionAuthorJSON `json:"authors"`
	Categories    []string               `json:"categories"`
	Title         string                 `json:"title"`
	Abstract      string                 `json:"abstract"`
	ArXivID       string                 `json:"arxivId"`
	LatestVersion int                    `json:"latestVersion"`
	DOI           string                 `json:"doi,omitempty"`
	JournalRef    string                 `json:"journalRef,omitempty"`
	PublishedAt   string                 `json:"publishedAt"`
	UpdatedAt     string                 `json:"updatedAt"`
	FirstSeenAt   string                 `json:"firstSeenAt"`
	LastSeenAt    string                 `json:"lastSeenAt"`
	ContentHash   string                 `json:"contentHash"`
	CanonicalVer  int                    `json:"canonicalVersion"`
	Ignored       bool                   `json:"ignored"`
}

// CollectionMatch records one discovery relationship between a task revision
// and a candidate on a channel/branch, keeping every evidence phrase.
type CollectionMatch struct {
	Evidence      []CollectionEvidence `json:"evidence"`
	TaskID        string               `json:"taskId"`
	ArXivID       string               `json:"arxivId"`
	BranchHash    string               `json:"branchHash"`
	Channel       string               `json:"channel"`
	FirstSeenAt   string               `json:"firstSeenAt"`
	LastSeenAt    string               `json:"lastSeenAt"`
	RuleScore     int                  `json:"ruleScore"`
	QueryRevision int                  `json:"queryRevision"`
}

// CollectionRun is one discovery execution for a task with a fixed range.
type CollectionRun struct {
	ConfigSnapshot json.RawMessage `json:"configSnapshot"`
	TaskID         string          `json:"taskId"`
	ID             string          `json:"id"`
	Channel        string          `json:"channel,omitempty"`
	Trigger        string          `json:"trigger"`
	RangeJSON      json.RawMessage `json:"range"`
	ExecToken      int64           `json:"execToken"`
	Status         string          `json:"status"`
	FailureClass   string          `json:"failureClass,omitempty"`
	StartedAt      string          `json:"startedAt,omitempty"`
	FinishedAt     string          `json:"finishedAt,omitempty"`
	QueryRevision  int             `json:"queryRevision"`
}

// CollectionSlice is one coverage unit of a run's discovery range.
type CollectionSlice struct {
	Query        string `json:"query"`
	ParentID     string `json:"parentId,omitempty"`
	ID           string `json:"id"`
	TaskID       string `json:"taskId"`
	Channel      string `json:"channel"`
	BranchHash   string `json:"branchHash"`
	FromUTC      string `json:"fromUtc"`
	ToUTC        string `json:"toUtc"`
	PageOffset   int    `json:"pageOffset"`
	TotalResults int    `json:"totalResults"`
	Processed    int    `json:"processed"`
	Status       string `json:"status"`
	FailureClass string `json:"failureClass,omitempty"`
	SourceLimit  bool   `json:"sourceLimit"`
	RetryAt      string `json:"retryAt,omitempty"`
	CheckedAt    string `json:"checkedAt,omitempty"`
}

// CollectionCitation is the Semantic Scholar cumulative count for a candidate.
// Unknown stays NULL; an explicit zero is a real zero.
type CollectionCitation struct {
	ArXivID        string `json:"arxivId"`
	ServicePaperID string `json:"servicePaperID,omitempty"`
	Count          *int   `json:"count"`
	SuccessAt      string `json:"successAt,omitempty"`
	AttemptedAt    string `json:"attemptedAt,omitempty"`
	RefreshAt      string `json:"refreshAt,omitempty"`
	Error          string `json:"error,omitempty"`
	Stale          bool   `json:"stale"`
}

// CollectionAnalysisTask is one task judgment inside a stored analysis result.
type CollectionAnalysisTask struct {
	Evidence       []CollectionEvidence `json:"evidence"`
	TaskID         string               `json:"taskId"`
	ProfileHash    string               `json:"profileHash"`
	ReasonZH       string               `json:"reason_zh"`
	RelevanceScore int                  `json:"relevance_score"`
}

// CollectionAnalysisResult is the strict schema v1 model output.
type CollectionAnalysisResult struct {
	Assessments   []CollectionAnalysisTask `json:"assessments"`
	SchemaVersion int                      `json:"schema_version"`
	TitleZH       string                   `json:"title_zh"`
	AbstractZH    string                   `json:"abstract_zh"`
}

// CollectionAnalysis is one cached analysis keyed by material identity.
type CollectionAnalysis struct {
	ModelFingerprint string                    `json:"modelFingerprint"`
	Result           *CollectionAnalysisResult `json:"result,omitempty"`
	CacheKey         string                    `json:"cacheKey"`
	ArXivID          string                    `json:"arxivId"`
	ContentHash      string                    `json:"contentHash"`
	ProfileHash      string                    `json:"profileHash"`
	PromptVersion    int                       `json:"promptVersion"`
	ResultVersion    int                       `json:"resultVersion"`
	Status           string                    `json:"status"`
	Error            string                    `json:"error,omitempty"`
	Attempt          int                       `json:"attempt"`
	RetryAt          string                    `json:"retryAt,omitempty"`
	CreatedAt        string                    `json:"createdAt"`
	UpdatedAt        string                    `json:"updatedAt"`
}

// CollectionQuotaDay meters analysis requests actually sent on a UTC day.
type CollectionQuotaDay struct {
	UTCDate   string `json:"utcDate"`
	Sent      int    `json:"sent"`
	Reserved  int    `json:"reserved"`
	Uncertain int    `json:"uncertain"`
}

// CollectionAttempt is one metered analysis send identity.
type CollectionAttempt struct {
	ID          string `json:"id"`
	RequestKey  string `json:"requestKey"`
	CacheKey    string `json:"cacheKey"`
	ReservedDay string `json:"reservedDay"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"createdAt"`
	SentAt      string `json:"sentAt,omitempty"`
	FinishedAt  string `json:"finishedAt,omitempty"`
}

// CollectionImportRequest is one user-confirmed fixed-version download.
type CollectionImportRequest struct {
	MetadataSnapshot json.RawMessage `json:"metadataSnapshot,omitempty"`
	RequestID        string          `json:"requestId"`
	ArXivID          string          `json:"arxivId"`
	DownloadVersion  int             `json:"downloadVersion"`
	ExpectedSHA256   string          `json:"expectedSha256,omitempty"`
	ActualSHA256     string          `json:"actualSha256,omitempty"`
	CacheRelPath     string          `json:"cacheRelPath,omitempty"`
	Status           string          `json:"status"`
	DocumentID       string          `json:"documentId,omitempty"`
	Library          string          `json:"library,omitempty"`
	ConfirmPages     int             `json:"confirmPages,omitempty"`
	Error            string          `json:"error,omitempty"`
	Condition        string          `json:"condition,omitempty"`
	CreatedAt        string          `json:"createdAt"`
	UpdatedAt        string          `json:"updatedAt"`
}

// NormalizeArXivID strips a version suffix and validates the stable form.
// Old category-path IDs keep their case; new IDs keep the source digits.
func NormalizeArXivID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	id = strings.TrimPrefix(id, "arxiv:")
	id = strings.TrimPrefix(id, "arXiv:")
	id = arxivVerID.ReplaceAllString(id, "")
	if id == "" {
		return "", errInvalidArxivID
	}
	if arxivNewID.MatchString(id) || arxivOldID.MatchString(id) {
		return id, nil
	}
	return "", errInvalidArxivID
}

// ValidArXivVersion reports whether v is a positive version number.
func ValidArXivVersion(v int) bool { return v >= 1 }

// CanonicalContentBytes builds the canonical v1 JSON bytes for a candidate's
// title, abstract, ordered author names and categories. Values keep their
// wording; only newlines and whitespace runs normalize to single spaces.
func CanonicalContentBytes(title, abstract string, authors []string, categories []string) []byte {
	type canonical struct {
		Title      string   `json:"title"`
		Abstract   string   `json:"abstract"`
		Authors    []string `json:"authors"`
		Categories []string `json:"categories"`
	}
	c := canonical{
		Title:      normalizeSpace(title),
		Abstract:   normalizeSpace(abstract),
		Authors:    make([]string, 0, len(authors)),
		Categories: append([]string(nil), categories...),
	}
	for _, a := range authors {
		c.Authors = append(c.Authors, normalizeSpace(a))
	}
	b, err := json.Marshal(c)
	if err != nil {
		// All fields are plain strings; Marshal cannot fail.
		panic(err)
	}
	return b
}

// CandidateContentHash returns the SHA256 of the canonical v1 bytes.
func CandidateContentHash(title, abstract string, authors []string, categories []string) string {
	sum := sha256.Sum256(CanonicalContentBytes(title, abstract, authors, categories))
	return hex.EncodeToString(sum[:])
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// DefaultCollectionTaskConfig returns the version 1 defaults from the design.
func DefaultCollectionTaskConfig() CollectionTaskConfig {
	return CollectionTaskConfig{
		Version:          1,
		GroupOperatorOR:  true,
		FrequencyHours:   24,
		LatestWindowDays: 30,
		HotWindowMonths:  12,
		KeywordGroups:    [][]string{},
		Authors:          []CollectionTaskAuthor{},
		Categories:       []string{},
	}
}

// Validate checks structural rules of a task config. Executable tasks need at
// least one keyword group, category or author; topic alone is not runnable.
func (c CollectionTaskConfig) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("%w: 不支持的配置版本 %d", errInvalidTaskConfig, c.Version)
	}
	switch c.FrequencyHours {
	case 6, 12, 24, 48:
	default:
		return fmt.Errorf("%w: 频率须为 6、12、24 或 48 小时", errInvalidTaskConfig)
	}
	if c.LatestWindowDays < 1 || c.LatestWindowDays > 3650 {
		return fmt.Errorf("%w: 最新天数无效", errInvalidTaskConfig)
	}
	if c.HotWindowMonths < 1 || c.HotWindowMonths > 120 {
		return fmt.Errorf("%w: 热门月数无效", errInvalidTaskConfig)
	}
	usableGroups := 0
	for _, g := range c.KeywordGroups {
		words := 0
		for _, w := range g {
			if strings.TrimSpace(w) != "" {
				words++
			}
		}
		if words == 0 {
			return fmt.Errorf("%w: 关键词组不能为空", errInvalidTaskConfig)
		}
		usableGroups++
	}
	usableAuthors := 0
	for _, a := range c.Authors {
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("%w: 作者姓名不能为空", errInvalidTaskConfig)
		}
		usableAuthors++
	}
	cats := 0
	for _, cat := range c.Categories {
		if strings.TrimSpace(cat) != "" {
			cats++
		}
	}
	if usableGroups == 0 && usableAuthors == 0 && cats == 0 {
		return fmt.Errorf("%w: 至少需要关键词、分类或作者之一", errInvalidTaskConfig)
	}
	return nil
}

// WantsRelevance reports whether analysis must judge this task's relevance.
// Author/category-only discovery may be translation-only.
func (c CollectionTaskConfig) WantsRelevance() bool {
	if strings.TrimSpace(c.ResearchTopic) != "" {
		return true
	}
	for _, g := range c.KeywordGroups {
		for _, w := range g {
			if strings.TrimSpace(w) != "" {
				return true
			}
		}
	}
	return false
}

// SemanticHash is the SHA256 over the relevance material of the config:
// topic, keyword groups, group operator, authors, categories and the
// not-interested preference. Name, windows, frequency and model choices are
// excluded; query_revision owns that contract separately.
func (c CollectionTaskConfig) SemanticHash() string {
	type semantic struct {
		KeywordGroups   [][]string             `json:"keyword_groups"`
		Authors         []CollectionTaskAuthor `json:"authors"`
		Categories      []string               `json:"categories"`
		ResearchTopic   string                 `json:"research_topic"`
		NotInterested   string                 `json:"not_interested"`
		GroupOperatorOR bool                   `json:"group_operator_or"`
	}
	s := semantic{
		KeywordGroups:   make([][]string, 0, len(c.KeywordGroups)),
		Authors:         make([]CollectionTaskAuthor, 0, len(c.Authors)),
		Categories:      make([]string, 0, len(c.Categories)),
		ResearchTopic:   normalizeSpace(c.ResearchTopic),
		NotInterested:   normalizeSpace(c.NotInterested),
		GroupOperatorOR: c.GroupOperatorOR,
	}
	for _, g := range c.KeywordGroups {
		words := make([]string, 0, len(g))
		for _, w := range g {
			if v := normalizeSpace(w); v != "" {
				words = append(words, v)
			}
		}
		s.KeywordGroups = append(s.KeywordGroups, words)
	}
	for _, a := range c.Authors {
		aliases := make([]string, 0, len(a.Aliases))
		for _, al := range a.Aliases {
			if v := normalizeSpace(al); v != "" {
				aliases = append(aliases, v)
			}
		}
		s.Authors = append(s.Authors, CollectionTaskAuthor{Name: normalizeSpace(a.Name), Aliases: aliases})
	}
	for _, cat := range c.Categories {
		if v := normalizeSpace(cat); v != "" {
			s.Categories = append(s.Categories, v)
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// QueryAffectsRevision reports whether a config change raises query_revision.
// Renaming, frequency, windows and model fields never do.
func QueryAffectsRevision(a, b CollectionTaskConfig) bool {
	return a.SemanticHash() != b.SemanticHash()
}

// validRunStatus / validSliceStatus / ... keep persisted codes strict: an
// unknown code stops the corresponding operation instead of being coerced.
func validRunStatus(s string) bool {
	switch s {
	case runQueued, runRunning, runSucceeded, runPartial, runFailed, runCancelled, runInterrupted:
		return true
	}
	return false
}

func validSliceStatus(s string) bool {
	switch s {
	case slicePending, sliceRunning, sliceCompleted, sliceFailed, sliceInterrupted:
		return true
	}
	return false
}

func validAnalysisStatus(s string) bool {
	switch s {
	case analysisPending, analysisRunning, analysisSucceeded, analysisFailed, analysisInterrupted, analysisBlocked:
		return true
	}
	return false
}

func validImportStatus(s string) bool {
	switch s {
	case importQueued, importDownloading, importDownloaded, importAwaitingConfirmation, importImporting, importImported, importFailed, importCancelled, importInterrupted:
		return true
	}
	return false
}

func validAttemptStatus(s string) bool {
	switch s {
	case attemptReserved, attemptDispatching, attemptSent, attemptReleased, attemptSettled:
		return true
	}
	return false
}
