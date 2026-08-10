// Package codeintel is the host-side adapter to a project's code-intelligence
// service.
//
// It is the boundary [ADR-0004] requires: the types here are LCTK's public
// vocabulary, and no backend concept crosses into them. The search engine lives
// in a Linux container per [ADR-0011] and is never linked into this executable,
// so the only thing the host knows about it is this HTTP contract.
//
// [ADR-0004]: ../../docs/adr/0004-stable-aggregated-tool-api.md
// [ADR-0011]: ../../docs/adr/0011-zoekt-exact-search-backend.md
package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// SchemaVersion is the version of the public search request and response
// shapes. It is independent of the product version and of the backend, so a
// client can reason about the contract it is speaking.
const SchemaVersion = 1

// Backend names the engine behind the adapter. It is reported as provenance
// rather than used for dispatch: a caller may want to know what answered, but
// nothing in the protocol depends on the value.
const Backend = "zoekt"

// SemanticBackend names LCTK's owned exact cosine adapter. It is provenance,
// not a selector exposed to callers.
const SemanticBackend = "lctk_exact_cosine"

// IntelligenceSchemaVersion covers persistent semantic, graph, and memory
// contracts independently of the exact-search wire schema.
const IntelligenceSchemaVersion = 2

// GraphBackend names the owned SQLite/name-match adapter as provenance.
const GraphBackend = "lctk_sqlite_name_match"

// DefaultSearchTimeout bounds a search when the caller sets no deadline of its
// own. Index operations are deliberately not bounded here: a full rebuild of a
// large project legitimately takes minutes, and the caller that asked for one
// owns the deadline.
const DefaultSearchTimeout = 30 * time.Second

// DefaultSemanticTimeout includes the measured full exact-vector scan at the
// one-million-file stress target plus headroom for local inference and host
// contention. A caller-supplied deadline remains authoritative.
const DefaultSemanticTimeout = 2 * time.Minute

// DefaultWatchSetTimeout bounds the directory enumeration a watcher needs before
// it can start. It is longer than a search because it is a whole-tree walk, and
// shorter than an index build because nothing is being written.
const DefaultWatchSetTimeout = 2 * time.Minute

// Error codes the adapter reports. They are LCTK's, not the service's, though
// the two vocabularies deliberately coincide where the meaning is the same.
const (
	CodeIndexNotReady     = "INDEX_NOT_READY"
	CodeIndexCorrupt      = "INDEX_CORRUPT"
	CodeInvalidPattern    = "INVALID_PATTERN"
	CodeInvalidCursor     = "INVALID_CURSOR"
	CodeLimitExceeded     = "LIMIT_EXCEEDED"
	CodeServiceOffline    = "SERVICE_UNAVAILABLE"
	CodeInternalError     = "INTERNAL_ERROR"
	CodeSearchUnsupported = "SEARCH_UNAVAILABLE"
	// CodeInvalidPath reports a path that is not project-relative.
	CodeInvalidPath = "INVALID_PATH"
	// CodeFileNotFound reports a path the project does not hold, which includes a
	// path its own ignore rules exclude.
	CodeFileNotFound = "FILE_NOT_FOUND"
	// CodeFileTooLarge reports a file above the limit for the operation asked for.
	CodeFileTooLarge = "FILE_TOO_LARGE"
	// CodeLanguageUnsupported reports a file this build has no grammar for. It is a
	// stated gap rather than an empty answer, which would read as "this file
	// declares nothing".
	CodeLanguageUnsupported = "LANGUAGE_UNSUPPORTED"
	// CodeParseIncomplete reports a file the parser did not finish inside its
	// budget.
	CodeParseIncomplete = "PARSE_INCOMPLETE"
	// CodeParseBusy reports that the project is already parsing as many files as its
	// resource policy allows. It is retryable, which is the distinction: the file is
	// fine and the answer exists, the project was busy at that moment.
	CodeParseBusy = "PARSE_BUSY"
	// Semantic failures are separate from exact index failures because retrying
	// inference and rebuilding corrupt semantic state are different actions.
	CodeSemanticNotReady     = "SEMANTIC_NOT_READY"
	CodeEmbeddingUnavailable = "EMBEDDING_UNAVAILABLE"
	CodeSemanticCorrupt      = "SEMANTIC_CORRUPT"
	CodeModelMismatch        = "MODEL_MISMATCH"
	CodeInvalidQuery         = "INVALID_QUERY"
	CodeMemoryNotFound       = "MEMORY_NOT_FOUND"
	CodeMemoryConflict       = "MEMORY_REVISION_CONFLICT"
)

// Error is a typed failure a caller can act on.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	Action    string
	Cause     error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

// Request is the public search request.
type Request struct {
	Pattern       string   `json:"pattern"`
	Mode          string   `json:"mode,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	PathGlobs     []string `json:"path_globs,omitempty"`
	Languages     []string `json:"languages,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Cursor        string   `json:"cursor,omitempty"`
}

// Match is one result line, with a path relative to the project root.
type Match struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Preview string `json:"preview"`
	Match   string `json:"match"`
}

// Provenance says where an answer came from and how fresh it is, so a caller can
// judge it rather than assume it.
type Provenance struct {
	Backend         string `json:"backend"`
	SchemaVersion   int    `json:"schema_version"`
	IndexGeneration uint64 `json:"index_generation"`
	IndexedAt       string `json:"indexed_at,omitempty"`
	FileCount       int    `json:"file_count"`
	// Freshness is filled in by the caller that knows what the host has observed
	// since this generation was built. The adapter cannot judge it: the service
	// knows what it indexed, not what has changed on disk since.
	//
	// "fresh" is a claim about the disk and nothing else. It says the index matches
	// the files as they are written, not that it accounts for every edit a caller
	// has in mind: an unwritten buffer and an unapplied patch are invisible here by
	// design, because the alternative is trusting a client's word about what a file
	// contains.
	Freshness string `json:"freshness,omitempty"`
}

// Response is the public search response.
type Response struct {
	Matches    []Match    `json:"matches"`
	Total      int        `json:"total"`
	Truncated  bool       `json:"truncated"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// Status is the project service's own view of its index.
type Status struct {
	Ready          bool     `json:"ready"`
	Indexing       bool     `json:"indexing"`
	Generation     uint64   `json:"generation"`
	FileCount      int      `json:"file_count"`
	SkippedBig     int      `json:"skipped_too_large"`
	SkippedIgnored int      `json:"skipped_ignored"`
	IgnoreSources  []string `json:"ignore_sources,omitempty"`
	DeltaDepth     int      `json:"delta_depth"`
	// SourceBytes and IndexBytes are what the project costs on disk.
	SourceBytes int64  `json:"source_bytes"`
	IndexBytes  int64  `json:"index_bytes"`
	IndexedAt   string `json:"indexed_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// OutlineLanguages names what the service can outline. It is empty for a
	// service that predates the symbol layer, which reads correctly as "none".
	OutlineLanguages []string `json:"outline_languages,omitempty"`
	// SymbolParallelism is how many files the project will parse at once, zero
	// meaning unbounded. It explains a PARSE_BUSY refusal without guesswork.
	SymbolParallelism int             `json:"symbol_parallelism,omitempty"`
	Semantic          *SemanticStatus `json:"semantic,omitempty"`
	Graph             *GraphStatus    `json:"graph,omitempty"`
}

// GraphStatus identifies the graph generation and honest precision.
type GraphStatus struct {
	Ready       bool   `json:"ready"`
	Generation  uint64 `json:"generation"`
	FileCount   int    `json:"file_count"`
	NodeCount   int    `json:"node_count"`
	ImportCount int    `json:"import_count"`
	CallCount   int    `json:"call_count"`
	IndexedAt   string `json:"indexed_at,omitempty"`
	Precision   string `json:"precision"`
	Freshness   string `json:"freshness"`
	Reason      string `json:"reason,omitempty"`
}

// SemanticStatus states which exact generation the persistent semantic store
// describes. It is nil when the project stack has no inference configuration.
type SemanticStatus struct {
	Ready          bool   `json:"ready"`
	Indexing       bool   `json:"indexing"`
	Generation     uint64 `json:"generation"`
	FileCount      int    `json:"file_count"`
	ChunkCount     int    `json:"chunk_count"`
	Model          string `json:"model,omitempty"`
	Dimensions     int    `json:"dimensions,omitempty"`
	IndexedAt      string `json:"indexed_at,omitempty"`
	Freshness      string `json:"freshness"`
	Reason         string `json:"reason,omitempty"`
	ChunksTotal    int    `json:"chunks_total,omitempty"`
	ChunksEmbedded int    `json:"chunks_embedded,omitempty"`
	ChunksReused   int    `json:"chunks_reused,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	ProgressAt     string `json:"progress_at,omitempty"`
	Stalled        bool   `json:"stalled,omitempty"`
	StallSeconds   int64  `json:"stall_seconds,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// SemanticRequest is the host-owned request contract for conceptual retrieval.
type SemanticRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// SemanticMatch is one syntax-aware or bounded text chunk. Independent rank
// fields make the hybrid result inspectable by an agent.
type SemanticMatch struct {
	Path           string  `json:"path"`
	Language       string  `json:"language"`
	ChunkPrecision string  `json:"chunk_precision"`
	Anchor         string  `json:"anchor"`
	StartLine      int     `json:"start_line"`
	EndLine        int     `json:"end_line"`
	Preview        string  `json:"preview"`
	VectorScore    float64 `json:"vector_score,omitempty"`
	LexicalScore   float64 `json:"lexical_score,omitempty"`
	HybridScore    float64 `json:"hybrid_score"`
	VectorRank     int     `json:"vector_rank,omitempty"`
	LexicalRank    int     `json:"lexical_rank,omitempty"`
}

// SemanticResponse carries both semantic and exact generations so stale state
// is a field, never an inference from timestamps.
type SemanticResponse struct {
	Matches         []SemanticMatch `json:"matches"`
	Total           int             `json:"total"`
	Truncated       bool            `json:"truncated"`
	Generation      uint64          `json:"generation"`
	ExactGeneration uint64          `json:"exact_generation"`
	Freshness       string          `json:"freshness"`
	Model           string          `json:"model"`
	Dimensions      int             `json:"dimensions"`
}

// GraphRequest is the common request for caller and callee evidence.
type GraphRequest struct {
	Name   string `json:"name"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type GraphEvidence struct {
	Path   string `json:"path"`
	Caller string `json:"caller,omitempty"`
	Callee string `json:"callee"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type GraphMatches struct {
	Name         string          `json:"name"`
	Direction    string          `json:"direction"`
	Matches      []GraphEvidence `json:"matches"`
	Total        int             `json:"total"`
	Truncated    bool            `json:"truncated"`
	NextCursor   string          `json:"next_cursor,omitempty"`
	Ambiguous    bool            `json:"ambiguous"`
	Declarations int             `json:"declarations"`
	Generation   uint64          `json:"generation"`
	Precision    string          `json:"precision"`
}

type DependencyRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	MaxDepth int    `json:"max_depth,omitempty"`
}
type DependencyResponse struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Path       []string `json:"path"`
	Found      bool     `json:"found"`
	MaxDepth   int      `json:"max_depth"`
	Generation uint64   `json:"generation"`
	Precision  string   `json:"precision"`
}
type ImpactRequest struct {
	Target string `json:"target"`
	Limit  int    `json:"limit,omitempty"`
}
type ImpactResponse struct {
	Target     string          `json:"target"`
	Files      []string        `json:"files"`
	Calls      []GraphEvidence `json:"calls"`
	Total      int             `json:"total"`
	Truncated  bool            `json:"truncated"`
	Ambiguous  bool            `json:"ambiguous"`
	Generation uint64          `json:"generation"`
	Precision  string          `json:"precision"`
}
type MapRequest struct {
	MaxChars int `json:"max_chars,omitempty"`
}
type MapResponse struct {
	Map        string `json:"map"`
	Characters int    `json:"characters"`
	MaxChars   int    `json:"max_chars"`
	Truncated  bool   `json:"truncated"`
	FileCount  int    `json:"file_count"`
	NodeCount  int    `json:"node_count"`
	Generation uint64 `json:"generation"`
	Precision  string `json:"precision"`
}

type MemoryRecord struct {
	Key           string   `json:"key"`
	Kind          string   `json:"kind"`
	Content       string   `json:"content"`
	Confidence    float64  `json:"confidence"`
	Provenance    []string `json:"provenance"`
	SourceCommit  string   `json:"source_commit,omitempty"`
	Revision      int      `json:"revision"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	ReviewedAt    string   `json:"reviewed_at,omitempty"`
	ReviewDue     bool     `json:"review_due"`
	LowConfidence bool     `json:"low_confidence"`
}
type MemoryPutRequest struct {
	Key              string   `json:"key"`
	Kind             string   `json:"kind"`
	Content          string   `json:"content"`
	Confidence       float64  `json:"confidence"`
	Provenance       []string `json:"provenance,omitempty"`
	SourceCommit     string   `json:"source_commit,omitempty"`
	ExpectedRevision *int     `json:"expected_revision,omitempty"`
	Reviewed         bool     `json:"reviewed,omitempty"`
}
type MemoryGetRequest struct {
	Key string `json:"key"`
}
type MemoryDeleteRequest struct {
	Key              string `json:"key"`
	ExpectedRevision int    `json:"expected_revision"`
}
type MemorySearchRequest struct {
	Query string `json:"query,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type MemoryMatch struct {
	Record       MemoryRecord `json:"record"`
	VectorScore  float64      `json:"vector_score,omitempty"`
	LexicalScore float64      `json:"lexical_score,omitempty"`
	HybridScore  float64      `json:"hybrid_score,omitempty"`
	VectorRank   int          `json:"vector_rank,omitempty"`
	LexicalRank  int          `json:"lexical_rank,omitempty"`
}
type MemorySearchResponse struct {
	Matches    []MemoryMatch `json:"matches"`
	Total      int           `json:"total"`
	Truncated  bool          `json:"truncated"`
	Modes      []string      `json:"modes"`
	Model      string        `json:"model,omitempty"`
	Dimensions int           `json:"dimensions,omitempty"`
}

// Symbol is one declaration in a file.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// StartLine and EndLine are 1-based and inclusive.
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	// StartByte and EndByte bound the declaration in the file, so a caller can be
	// shown the declaration rather than a cursor position.
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
	// Container is the enclosing declaration's name, empty at the top level.
	Container string `json:"container,omitempty"`
	// Depth is how deeply the declaration is nested, so a flat list renders as a
	// tree without recomputing containment.
	Depth int `json:"depth"`
	// Signature is the declaration's own first line.
	Signature string `json:"signature,omitempty"`
}

// Syntax is what the parser can say about a file being whole.
type Syntax struct {
	// Reported says a verdict is published for this language at all. False must be
	// read as "unknown" rather than as "fine".
	Reported bool `json:"reported"`
	Valid    bool `json:"valid"`
	// Errors counts the broken regions located: places to look, not the size of the
	// damage.
	Errors         int `json:"errors,omitempty"`
	FirstErrorLine int `json:"first_error_line,omitempty"`
	// Note explains a withheld verdict, so "reported: false" is not a silence a
	// caller has to interpret.
	Note string `json:"note,omitempty"`
}

// Outline is one file's declarations.
//
// It describes the file as it is on disk, not as the index holds it, so it carries
// a content digest rather than an index generation: there is nothing to flush and
// no generation it could be behind.
type Outline struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Bytes    int    `json:"bytes"`
	Lines    int    `json:"lines"`
	// Digest is the content this answer describes, so a caller can tell whether two
	// answers are about the same bytes.
	Digest        string   `json:"digest,omitempty"`
	SchemaVersion int      `json:"schema_version"`
	Symbols       []Symbol `json:"symbols"`
	Syntax        Syntax   `json:"syntax"`
}

// WatchSet is the set of project-relative directories a host watcher must
// observe to see every change that could reach the index.
//
// The service computes it, because the service owns the exclusion policy. A
// host-side reimplementation would be a second answer to "what belongs to this
// project", and the one that drifted would be the watcher, quietly missing edits.
type WatchSet struct {
	Directories []string `json:"directories"`
	Count       int      `json:"count"`
	// Truncated says the project has more directories than the service will
	// describe, so the set is a prefix rather than the whole project. A caller
	// must fall back to reconciliation instead of trusting it.
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit"`
}

// Client talks to one project's service.
type Client struct {
	// Address is the loopback host:port of the process-owned machine tunnel. It
	// is supplied per call site rather than stored globally because the daemon
	// assigns it and it changes across a restart.
	Address string
	HTTP    *http.Client
}

// New returns a client for a process-owned loopback service address.
//
// The HTTP client carries no overall timeout on purpose. A single deadline
// cannot suit both a search, which must not hang, and a full index rebuild,
// which is allowed to take minutes; each call site sets its own instead.
func New(address string) *Client {
	return &Client{
		Address: address,
		HTTP: &http.Client{
			// The service is on loopback and is not a general-purpose endpoint;
			// following a redirect out of it would be a bug, not a feature.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Search runs one query against the project service.
func (c *Client) Search(ctx context.Context, request Request) (Response, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSearchTimeout)
		defer cancel()
	}

	var service serviceResponse
	if err := c.call(ctx, "/search", request, &service); err != nil {
		return Response{}, err
	}
	response := Response{
		Matches:    service.Matches,
		Total:      service.Total,
		Truncated:  service.Truncated,
		NextCursor: service.NextCursor,
		Provenance: Provenance{
			Backend:         Backend,
			SchemaVersion:   SchemaVersion,
			IndexGeneration: service.Generation,
			IndexedAt:       service.IndexedAt,
			FileCount:       service.FileCount,
		},
	}
	if response.Matches == nil {
		response.Matches = []Match{}
	}
	return response, nil
}

// SemanticSearch asks the project-local store for hybrid conceptual retrieval.
// It uses its measured semantic bound because the upper stress target performs
// a full exact vector scan in addition to waiting for local inference.
func (c *Client) SemanticSearch(ctx context.Context, request SemanticRequest) (SemanticResponse, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSemanticTimeout)
		defer cancel()
	}
	var response SemanticResponse
	if err := c.call(ctx, "/semantic/search", request, &response); err != nil {
		return SemanticResponse{}, err
	}
	if response.Matches == nil {
		response.Matches = []SemanticMatch{}
	}
	return response, nil
}

// Callers returns saved call sites whose callee name matches exactly.
func (c *Client) Callers(ctx context.Context, request GraphRequest) (GraphMatches, error) {
	var response GraphMatches
	if err := c.boundedCall(ctx, "/graph/callers", request, &response); err != nil {
		return GraphMatches{}, err
	}
	if response.Matches == nil {
		response.Matches = []GraphEvidence{}
	}
	return response, nil
}

// Callees returns saved calls inside declarations with the requested name.
func (c *Client) Callees(ctx context.Context, request GraphRequest) (GraphMatches, error) {
	var response GraphMatches
	if err := c.boundedCall(ctx, "/graph/callees", request, &response); err != nil {
		return GraphMatches{}, err
	}
	if response.Matches == nil {
		response.Matches = []GraphEvidence{}
	}
	return response, nil
}

// DependencyPath traverses syntax import edges between project files.
func (c *Client) DependencyPath(ctx context.Context, request DependencyRequest) (DependencyResponse, error) {
	var response DependencyResponse
	if err := c.boundedCall(ctx, "/graph/dependency-path", request, &response); err != nil {
		return DependencyResponse{}, err
	}
	if response.Path == nil {
		response.Path = []string{}
	}
	return response, nil
}

// Impact returns direct reverse dependency and name-call evidence.
func (c *Client) Impact(ctx context.Context, request ImpactRequest) (ImpactResponse, error) {
	var response ImpactResponse
	if err := c.boundedCall(ctx, "/graph/impact", request, &response); err != nil {
		return ImpactResponse{}, err
	}
	if response.Files == nil {
		response.Files = []string{}
	}
	if response.Calls == nil {
		response.Calls = []GraphEvidence{}
	}
	return response, nil
}

// RepositoryMap requests a deterministic character-bounded project overview.
func (c *Client) RepositoryMap(ctx context.Context, request MapRequest) (MapResponse, error) {
	var response MapResponse
	if err := c.boundedCall(ctx, "/graph/repository-map", request, &response); err != nil {
		return MapResponse{}, err
	}
	return response, nil
}

// MemoryGet reads one explicit project-memory key.
func (c *Client) MemoryGet(ctx context.Context, request MemoryGetRequest) (MemoryRecord, error) {
	var response MemoryRecord
	if err := c.boundedCall(ctx, "/memory/get", request, &response); err != nil {
		return MemoryRecord{}, err
	}
	if response.Provenance == nil {
		response.Provenance = []string{}
	}
	return response, nil
}

// MemorySearch performs a bounded list or hybrid search.
func (c *Client) MemorySearch(ctx context.Context, request MemorySearchRequest) (MemorySearchResponse, error) {
	var response MemorySearchResponse
	if err := c.boundedCall(ctx, "/memory/search", request, &response); err != nil {
		return MemorySearchResponse{}, err
	}
	if response.Matches == nil {
		response.Matches = []MemoryMatch{}
	}
	if response.Modes == nil {
		response.Modes = []string{}
	}
	return response, nil
}

// MemoryPut creates or revision-checks an explicit memory update.
func (c *Client) MemoryPut(ctx context.Context, request MemoryPutRequest) (MemoryRecord, error) {
	var response MemoryRecord
	if err := c.boundedCall(ctx, "/memory/put", request, &response); err != nil {
		return MemoryRecord{}, err
	}
	if response.Provenance == nil {
		response.Provenance = []string{}
	}
	return response, nil
}

// MemoryDelete deletes exactly the revision the client read.
func (c *Client) MemoryDelete(ctx context.Context, request MemoryDeleteRequest) error {
	var response struct {
		Deleted bool `json:"deleted"`
	}
	if err := c.boundedCall(ctx, "/memory/delete", request, &response); err != nil {
		return err
	}
	if !response.Deleted {
		return &Error{Code: CodeInternalError, Message: "The memory service did not confirm deletion."}
	}
	return nil
}

func (c *Client) boundedCall(ctx context.Context, endpoint string, request, response any) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSearchTimeout)
		defer cancel()
	}
	return c.call(ctx, endpoint, request, response)
}

// Status reports the project service's index state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	if err := c.get(ctx, "/status", &status); err != nil {
		return Status{}, err
	}
	return status, nil
}

// DefaultOutlineTimeout bounds one file's outline.
//
// It is shorter than a search because a search may wait behind an index build,
// while an outline is one parse of one file that the service bounds itself. A
// caller waiting longer than this is waiting on something that has already gone
// wrong.
const DefaultOutlineTimeout = 15 * time.Second

// Outline asks the service for one file's declarations.
func (c *Client) Outline(ctx context.Context, path string) (Outline, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultOutlineTimeout)
		defer cancel()
	}

	var outline Outline
	if err := c.call(ctx, "/outline", map[string]string{"path": path}, &outline); err != nil {
		return Outline{}, err
	}
	if outline.Symbols == nil {
		// An empty list, never a null: a null reads to a model as "no answer" rather
		// than "no declarations".
		outline.Symbols = []Symbol{}
	}
	return outline, nil
}

// Occurrence is one place a name appears as an identifier.
type Occurrence struct {
	Line   int `json:"line"`
	Column int `json:"column"`
	// StartByte and EndByte bound the identifier itself.
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
	// Declaration says this is where the name is declared rather than used.
	Declaration bool `json:"declaration"`
	// Kind is the declaration's kind, set only when Declaration is true.
	Kind string `json:"kind,omitempty"`
	// Container is the enclosing declaration's name.
	Container string `json:"container,omitempty"`
	Preview   string `json:"preview,omitempty"`
}

// FileOccurrences is one file's occurrences of one name.
type FileOccurrences struct {
	Path        string       `json:"path"`
	Language    string       `json:"language"`
	Digest      string       `json:"digest,omitempty"`
	Occurrences []Occurrence `json:"occurrences"`
	// Declarations counts how many of them declare the name.
	Declarations int `json:"declarations"`
	// Parsed reports whether the file is syntactically whole, and SyntaxReported
	// whether that verdict means anything for this language.
	Parsed         bool `json:"parsed"`
	SyntaxReported bool `json:"syntax_reported"`
}

// Located is one identifier lookup across the project.
//
// Unlike an outline, this answer depends on the index: the index chooses which
// files are worth parsing, so a file it has not caught up with is never opened. The
// generation is carried for that reason.
type Located struct {
	Name       string `json:"name"`
	Generation uint64 `json:"generation"`
	IndexedAt  string `json:"indexed_at,omitempty"`
	// Files carries one entry per file with at least one occurrence.
	Files        []FileOccurrences `json:"files"`
	Occurrences  int               `json:"occurrences"`
	Declarations int               `json:"declarations"`
	// FilesConsidered is how many files the index offered, which is not how many
	// appear in Files: a candidate whose only hits were in comments contributes
	// nothing.
	FilesConsidered int `json:"files_considered"`
	// FilesTruncated says the candidate list was cut short, so "no other
	// references" is not a conclusion a caller may draw.
	FilesTruncated bool `json:"files_truncated,omitempty"`
	// SkippedUnsupported and SkippedUnreadable count candidates that were never
	// examined, reported rather than dropped so a partial answer cannot be read as
	// a complete one.
	SkippedUnsupported int `json:"skipped_unsupported,omitempty"`
	SkippedUnreadable  int `json:"skipped_unreadable,omitempty"`
}

// DefaultLocateTimeout bounds one identifier lookup.
//
// It is longer than an outline because the lookup parses every candidate file the
// index offers, and shorter than an index build because nothing is written.
const DefaultLocateTimeout = 60 * time.Second

// Locate finds every occurrence of one identifier across the project.
func (c *Client) Locate(ctx context.Context, name string, declarationsOnly bool, maxFiles int) (Located, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultLocateTimeout)
		defer cancel()
	}

	body := struct {
		Name             string `json:"name"`
		DeclarationsOnly bool   `json:"declarations_only,omitempty"`
		MaxFiles         int    `json:"max_files,omitempty"`
	}{Name: name, DeclarationsOnly: declarationsOnly, MaxFiles: maxFiles}

	var located Located
	if err := c.call(ctx, "/locate", body, &located); err != nil {
		return Located{}, err
	}
	if located.Files == nil {
		located.Files = []FileOccurrences{}
	}
	for index := range located.Files {
		if located.Files[index].Occurrences == nil {
			located.Files[index].Occurrences = []Occurrence{}
		}
	}
	return located, nil
}

// WatchSet asks the service which directories a host watcher should observe.
func (c *Client) WatchSet(ctx context.Context) (WatchSet, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultWatchSetTimeout)
		defer cancel()
	}

	var set WatchSet
	if err := c.get(ctx, "/watchset", &set); err != nil {
		return WatchSet{}, err
	}
	return set, nil
}

// Change is one file event for the index to apply.
type Change struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted,omitempty"`
	// Subtree says the path names a removed directory, which takes every file
	// beneath it. The service expands it, because only the service knows what was
	// in there: by the time the change is reported the directory is gone.
	Subtree bool `json:"subtree,omitempty"`
}

// IndexResult is what an index operation published.
type IndexResult struct {
	Generation uint64 `json:"generation"`
	FileCount  int    `json:"file_count"`
	// Applied counts the paths that changed in the index, which is not the number
	// of changes submitted.
	Applied int `json:"applied"`
	// Unchanged counts submitted writes whose content already matched the index.
	// A batch that is entirely unchanged publishes no generation at all.
	Unchanged int `json:"unchanged,omitempty"`
	// FullBuild says the service decided to rebuild rather than apply a delta,
	// which it does for a batch large enough that a delta would cost more.
	FullBuild bool   `json:"full_build"`
	IndexedAt string `json:"indexed_at"`
}

// Apply submits an explicit batch of changes.
//
// The caller owns the deadline. A batch the service escalates to a full rebuild
// takes as long as a rebuild takes, and there is no useful single timeout that
// suits both that and a two-file edit.
func (c *Client) Apply(ctx context.Context, changes []Change) (IndexResult, error) {
	var result IndexResult
	body := struct {
		Mode    string   `json:"mode"`
		Changes []Change `json:"changes"`
	}{Mode: "apply", Changes: changes}
	if err := c.call(ctx, "/index", body, &result); err != nil {
		return IndexResult{}, err
	}
	return result, nil
}

// Reconcile asks the service to compare the workspace with its own inventory and
// apply the difference. It is the recovery path when the host's record of changes
// is known to be incomplete.
func (c *Client) Reconcile(ctx context.Context, full bool) (IndexResult, error) {
	mode := "reconcile"
	if full {
		mode = "full"
	}
	var result IndexResult
	if err := c.call(ctx, "/index", map[string]string{"mode": mode}, &result); err != nil {
		return IndexResult{}, err
	}
	return result, nil
}

// Reindex asks the service to catch up and reports the resulting status.
func (c *Client) Reindex(ctx context.Context, full bool) (Status, error) {
	if _, err := c.Reconcile(ctx, full); err != nil {
		return Status{}, err
	}
	return c.Status(ctx)
}

type serviceResponse struct {
	Matches    []Match `json:"matches"`
	Total      int     `json:"total"`
	Truncated  bool    `json:"truncated"`
	NextCursor string  `json:"next_cursor"`
	Generation uint64  `json:"generation"`
	IndexedAt  string  `json:"indexed_at"`
	FileCount  int     `json:"file_count"`
}

type serviceError struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return &Error{Code: CodeInternalError, Message: "The request could not be encoded.", Cause: err}
	}
	return c.do(ctx, http.MethodPost, path, bytes.NewReader(encoded), out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	if strings.TrimSpace(c.Address) == "" {
		// A running project always has a process-owned tunnel address. An empty
		// one means the stack predates service discovery, which a restart fixes.
		return &Error{
			Code:      CodeSearchUnsupported,
			Message:   "This project's code-intelligence service has no host tunnel.",
			Retryable: false,
			Action:    "Restart the project with lctk project restart so it is recreated with the current stack definition.",
		}
	}

	request, err := http.NewRequestWithContext(ctx, method, "http://"+c.Address+path, body)
	if err != nil {
		return &Error{Code: CodeInternalError, Message: "The request could not be built.", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return &Error{Code: CodeInternalError, Message: "The project service response could not be read.",
			Retryable: true, Cause: err}
	}

	if response.StatusCode != http.StatusOK {
		return decodeServiceError(response.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &Error{Code: CodeInternalError,
			Message: "The project service returned a response this build does not understand.", Cause: err}
	}
	return nil
}

// transportError distinguishes a cancelled call from a service that is not
// answering, because the two lead a caller to do different things.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &Error{Code: CodeInternalError, Message: "The search was cancelled.", Retryable: true, Cause: ctxErr}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{
			Code:      CodeServiceOffline,
			Message:   "The project's code-intelligence service did not answer in time.",
			Retryable: true,
			Action:    "Retry shortly; a first index build on a large project can be slow.",
			Cause:     err,
		}
	}
	return &Error{
		Code:      CodeServiceOffline,
		Message:   "The project's code-intelligence service is not reachable.",
		Retryable: true,
		Action:    "Check the project with lctk project status.",
		Cause:     err,
	}
}

func decodeServiceError(status int, payload []byte) error {
	var envelope serviceError
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Error.Code == "" {
		return &Error{
			Code:      CodeInternalError,
			Message:   fmt.Sprintf("The project service failed with status %d.", status),
			Retryable: status >= http.StatusInternalServerError,
		}
	}

	typed := &Error{
		Code:      envelope.Error.Code,
		Message:   envelope.Error.Message,
		Retryable: envelope.Error.Retryable,
	}
	typed.Action = ActionFor(typed.Code)
	return typed
}

// ActionFor is what a caller should do about a typed failure.
//
// It is a function rather than an inline switch so the advice can be read back and
// checked: the recommended action is part of the tool's interface, since it is the
// sentence an agent acts on.
func ActionFor(code string) string {
	switch code {
	case CodeIndexNotReady:
		return "The project is still building its index; retry shortly."
	case CodeIndexCorrupt:
		return "Rebuild the index with lctk project reindex --full."
	case CodeInvalidCursor:
		// More specific than "correct the request", because nothing about the
		// request was wrong when it was made: the index moved underneath it, and
		// the fix is to start again rather than to adjust an argument.
		return "Run the search again without the cursor; a cursor is only valid for the index generation that produced it."
	case CodeInvalidPattern, CodeLimitExceeded, CodeInvalidPath:
		return "Correct the request and try again."
	case CodeFileNotFound:
		// Naming the ignore rules matters: a file plainly present in the checkout can
		// be absent here, and without this the caller re-sends the same path.
		return "Check the path with exact_search or git_status; a file the project's ignore rules exclude is not readable either."
	case CodeFileTooLarge:
		return "Ask about a smaller file, or search within this one instead."
	case CodeLanguageUnsupported:
		// There is nothing to retry and nothing to correct. The useful advice is the
		// tool that does work on any file.
		return "Use exact_search on this file instead; project_info lists the languages this project can outline."
	case CodeParseIncomplete:
		return "This file is too costly to parse; search within it instead."
	case CodeParseBusy:
		// The only one of these that waiting fixes, so it is the only one that says so.
		return "The project is busy parsing; retry shortly, or ask about fewer files."
	case CodeSemanticNotReady:
		return "Wait for semantic indexing to finish, then retry. project_info reports its generation and reason."
	case CodeEmbeddingUnavailable:
		return "Check the shared local inference service with lctk doctor, then retry."
	case CodeSemanticCorrupt:
		return "Rebuild semantic state with lctk project reindex --full."
	case CodeModelMismatch:
		return "Run lctk update or rebuild semantic state with the installed model; do not mix model generations."
	case CodeInvalidQuery:
		return "Correct the request fields or bounds and try again."
	case CodeMemoryNotFound:
		return "List memory with memory_search, then use an existing key."
	case CodeMemoryConflict:
		return "Read the key with memory_get and retry with its current revision."
	default:
		return ""
	}
}
