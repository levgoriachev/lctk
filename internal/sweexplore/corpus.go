package sweexplore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// CorpusSchemaVersion versions only the audit report. The emitted rows are the
// unchanged ExploreRecord and IssueRecord shapes, which is what lets a private
// corpus run through the same join, campaign, and scoring path as the public
// dataset.
const CorpusSchemaVersion = 1

// Selection bounds. A fix that touches many files or thousands of lines is a
// refactor: it has no single localization target, so scoring an agent against
// it measures breadth of reading rather than the ability to find the defect.
const (
	corpusMaxSourceFiles  = 8
	corpusMinChangedLines = 3
	corpusMaxChangedLines = 400
	corpusMinStatement    = 25
	corpusMaxStatement    = 2000
	// corpusRegionGap merges neighbouring hunks that a reader would open as one
	// span. Two untouched lines between two edits are context, not a boundary.
	corpusRegionGap = 2
	// corpusIdentifierFloor mirrors the identifier extraction length. Shorter
	// needles match too much ordinary prose to carry any leak signal.
	corpusIdentifierFloor = 4
	// corpusWordFloor and corpusWordRunFloor bound the sub-identifier leak
	// check. A run has to be long enough that a reporter could not have written
	// it by accident, or every statement containing "get user" would be dropped.
	corpusWordFloor    = 3
	corpusWordRunFloor = 8
)

// Rejection reasons. Every scanned commit that does not become an instance is
// recorded under exactly one of these so the corpus is auditable rather than
// merely reproducible.
const (
	CorpusRejectNotAFix             = "not_a_fix_subject"
	CorpusRejectRevert              = "revert"
	CorpusRejectMerge               = "merge_commit"
	CorpusRejectNoParent            = "no_parent"
	CorpusRejectNoSourceFiles       = "no_eligible_source_files"
	CorpusRejectTooManySourceFiles  = "too_many_source_files"
	CorpusRejectChangedLines        = "changed_lines_out_of_range"
	CorpusRejectNoPreExistingSource = "no_pre_existing_source"
	CorpusRejectNoRegions           = "no_ground_truth_regions"
	CorpusRejectStatementTooShort   = "statement_too_short"
	CorpusRejectStatementTooLong    = "statement_too_long"
	CorpusRejectLeakPath            = "statement_leaks_path"
	CorpusRejectLeakIdentifier      = "statement_leaks_identifier"
	CorpusRejectLeakLineReference   = "statement_leaks_line_reference"
	CorpusRejectDuplicateID         = "duplicate_instance_id"
)

// Statement provenance. A campaign report has to be able to say where the words
// an agent read came from, because the three sources have very different
// leakage profiles.
const (
	CorpusStatementTicketFile  = "ticket_file"
	CorpusStatementTrackerSlug = "tracker_slug"
	CorpusStatementSubject     = "commit_subject"
)

// CorpusRequest is the complete input to one corpus build.
type CorpusRequest struct {
	RepositoryPath string
	RepositoryName string
	Since          string
	Until          string
	Count          int
	Seed           string
	Tickets        CorpusTicketSet
}

// CorpusTicket is one pre-fetched reporter-written issue. It exists because the
// builder is hermetic: it never calls a tracker or forge API, so a richer
// statement set must arrive as a pinned file like every other campaign input.
// Unknown fields are ignored so the exporter can record extra provenance
// without invalidating a corpus.
type CorpusTicket struct {
	Ref        string `json:"ref"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	SourcePull int    `json:"source_pull,omitempty"`
}

// CorpusTicketSet indexes tickets by both keys a commit subject can carry. The
// two number spaces overlap, so they are kept apart deliberately: matching a
// pull-request number against a ticket ref would attach an unrelated bug's
// symptom text to an instance and silently corrupt the measurement.
type CorpusTicketSet struct {
	ByRef        map[string]CorpusTicket
	BySourcePull map[int][]CorpusTicket
	Count        int
}

// CorpusCandidate is one surviving fix commit and the two rows it emits.
type CorpusCandidate struct {
	InstanceID      string
	FixCommit       string
	Explore         ExploreRecord
	Issue           IssueRecord
	StatementSource string
	ChangedLines    int
}

// CorpusRejection records why one scanned commit did not become an instance.
// The subject is retained because this report is owner-facing evidence and is
// never shown to a benchmarked agent.
type CorpusRejection struct {
	Commit  string `json:"commit"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// CorpusSelection describes one candidate that passed every filter, whether or
// not the deterministic sample drew it.
type CorpusSelection struct {
	InstanceID      string   `json:"instance_id"`
	FixCommit       string   `json:"fix_commit"`
	BaseCommit      string   `json:"base_commit"`
	Selected        bool     `json:"selected"`
	StatementSource string   `json:"statement_source"`
	ChangedLines    int      `json:"changed_lines"`
	CoreFiles       []string `json:"core_files"`
	OptionalFiles   []string `json:"optional_files,omitempty"`
	MainFiles       []string `json:"main_files"`
}

// CorpusReport is the auditable record of one build: what was scanned, what
// survived, and why everything else did not.
type CorpusReport struct {
	SchemaVersion    int               `json:"schema_version"`
	Repository       string            `json:"repository"`
	RepositorySlug   string            `json:"repository_slug"`
	RepositoryPath   string            `json:"repository_path"`
	Since            string            `json:"since"`
	Until            string            `json:"until,omitempty"`
	Seed             string            `json:"seed"`
	RequestedCount   int               `json:"requested_count"`
	ScannedCommits   int               `json:"scanned_commits"`
	CandidateCount   int               `json:"candidate_count"`
	SelectedCount    int               `json:"selected_count"`
	TicketCount      int               `json:"ticket_count"`
	StatementSources map[string]int    `json:"statement_sources"`
	RejectionCounts  map[string]int    `json:"rejection_counts"`
	Rejections       []CorpusRejection `json:"rejections"`
	Candidates       []CorpusSelection `json:"candidates"`
}

// CorpusResult carries the emitted rows in commit-chronological order together
// with the report that explains them.
type CorpusResult struct {
	Candidates []CorpusCandidate
	Report     CorpusReport
}

var (
	corpusFixSubject    = regexp.MustCompile(`(?i)^(fix|bugfix|hotfix)\b`)
	corpusRevertSubject = regexp.MustCompile(`(?i)^revert\b`)
	// The optional "!" is the Conventional Commits breaking-change marker; a
	// real subject reads `fix(auth)!: ...` and would otherwise keep its prefix.
	corpusFixPrefix      = regexp.MustCompile(`(?i)^(fix|bugfix|hotfix)(\([^)]*\))?!?\s*:\s*`)
	corpusIssueRefGroup  = regexp.MustCompile(`\s*\(\s*#\d+(?:\s*,\s*#\d+)*\s*\)`)
	corpusIssueRef       = regexp.MustCompile(`#(\d+)`)
	corpusTrailingPull   = regexp.MustCompile(`\s*\(\s*#(\d+)\s*\)\s*$`)
	corpusHunkHeader     = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
	corpusLineReference  = regexp.MustCompile(`:\d+`)
	corpusIdentifier     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)
	corpusTrackerURL     = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)
	corpusSlugTrailingID = regexp.MustCompile(`[-_]\d+$`)
	corpusSlugExtension  = regexp.MustCompile(`\.[A-Za-z0-9]{1,5}$`)
	corpusWhitespace     = regexp.MustCompile(`\s+`)
	corpusUnsafePath     = regexp.MustCompile(`["\\\x00-\x1f]`)
	corpusDate           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// corpusRejectedExtensions are lock, generated, binary, and asset files. Their
// diffs are large and carry no localization signal.
var corpusRejectedExtensions = map[string]struct{}{
	".lock": {}, ".sum": {}, ".map": {}, ".snap": {}, ".png": {}, ".jpg": {},
	".jpeg": {}, ".webp": {}, ".gif": {}, ".svg": {}, ".ico": {}, ".pdf": {},
	".ttf": {}, ".zip": {}, ".csv": {}, ".parquet": {},
}

var corpusRejectedNames = map[string]struct{}{
	"package-lock.json": {}, "yarn.lock": {}, "pnpm-lock.yaml": {},
	"Cargo.lock": {}, "poetry.lock": {}, "go.sum": {},
}

var corpusRejectedSegments = []string{
	"node_modules", "vendor", "dist", "build", "target", ".venv",
	"__pycache__", "migrations", "generated", ".git",
}

var corpusSourceExtensions = map[string]struct{}{
	".py": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".rs": {},
	".go": {}, ".java": {}, ".kt": {}, ".rb": {}, ".c": {}, ".h": {},
	".cc": {}, ".cpp": {}, ".cs": {}, ".php": {}, ".sql": {}, ".graphql": {},
	".scala": {}, ".swift": {}, ".m": {}, ".mm": {},
}

type corpusCommit struct {
	sha     string
	parents []string
	subject string
	body    string
}

type corpusFile struct {
	path    string
	test    bool
	changed int
	regions []Region
}

// BuildCorpus turns a repository's own fix history into benchmark instances.
// It reads the repository through Git only and never contacts a network
// service, so the same inputs produce the same corpus on any machine.
func BuildCorpus(ctx context.Context, request CorpusRequest) (CorpusResult, error) {
	if strings.TrimSpace(request.RepositoryPath) == "" {
		return CorpusResult{}, errors.New("corpus repository path is required")
	}
	if !isGitRepositoryDirectory(request.RepositoryPath) {
		return CorpusResult{}, fmt.Errorf("corpus repository %q is not a Git repository", request.RepositoryPath)
	}
	slug, err := corpusSlug(request.RepositoryName)
	if err != nil {
		return CorpusResult{}, err
	}
	if request.Count <= 0 {
		return CorpusResult{}, errors.New("corpus count must be positive")
	}
	if strings.TrimSpace(request.Seed) == "" {
		return CorpusResult{}, errors.New("corpus seed is required")
	}
	if err := corpusValidateDate("since", request.Since, true); err != nil {
		return CorpusResult{}, err
	}
	if err := corpusValidateDate("until", request.Until, false); err != nil {
		return CorpusResult{}, err
	}
	commits, err := corpusCommits(ctx, request)
	if err != nil {
		return CorpusResult{}, err
	}
	report := CorpusReport{
		SchemaVersion: CorpusSchemaVersion, Repository: request.RepositoryName, RepositorySlug: slug,
		RepositoryPath: request.RepositoryPath, Since: request.Since, Until: request.Until,
		Seed: request.Seed, RequestedCount: request.Count, ScannedCommits: len(commits),
		TicketCount: request.Tickets.Count, StatementSources: map[string]int{}, RejectionCounts: map[string]int{},
		Rejections: []CorpusRejection{}, Candidates: []CorpusSelection{},
	}
	reject := func(commit corpusCommit, reason, detail string) {
		report.Rejections = append(report.Rejections, CorpusRejection{Commit: commit.sha, Subject: commit.subject, Reason: reason, Detail: detail})
		report.RejectionCounts[reason]++
	}
	candidates := make([]CorpusCandidate, 0, request.Count)
	seen := map[string]struct{}{}
	for _, commit := range commits {
		candidate, reason, detail, err := corpusCandidateFor(ctx, request, slug, commit)
		if err != nil {
			return CorpusResult{}, err
		}
		if reason != "" {
			reject(commit, reason, detail)
			continue
		}
		if _, duplicate := seen[candidate.InstanceID]; duplicate {
			reject(commit, CorpusRejectDuplicateID, candidate.InstanceID)
			continue
		}
		seen[candidate.InstanceID] = struct{}{}
		candidates = append(candidates, candidate)
	}
	report.CandidateCount = len(candidates)
	selected := selectCorpusCandidates(candidates, request.Count, request.Seed)
	selectedIDs := make(map[string]struct{}, len(selected))
	for _, candidate := range selected {
		selectedIDs[candidate.InstanceID] = struct{}{}
		report.StatementSources[candidate.StatementSource]++
	}
	for _, candidate := range candidates {
		_, drawn := selectedIDs[candidate.InstanceID]
		report.Candidates = append(report.Candidates, CorpusSelection{
			InstanceID: candidate.InstanceID, FixCommit: candidate.FixCommit,
			BaseCommit: candidate.Issue.BaseCommit, Selected: drawn,
			StatementSource: candidate.StatementSource, ChangedLines: candidate.ChangedLines,
			CoreFiles:     candidate.Explore.GroundTruth.ReadCoreFiles,
			OptionalFiles: sortedKeys(candidate.Explore.GroundTruth.ReadOptionalFilesMap),
			MainFiles:     candidate.Explore.GroundTruth.MainFiles,
		})
	}
	report.SelectedCount = len(selected)
	return CorpusResult{Candidates: selected, Report: report}, nil
}

// corpusCandidateFor applies every selection rule to one commit. A non-empty
// reason means the commit is excluded; the returned candidate is then unusable.
func corpusCandidateFor(ctx context.Context, request CorpusRequest, slug string, commit corpusCommit) (CorpusCandidate, string, string, error) {
	if len(commit.parents) >= 2 {
		return CorpusCandidate{}, CorpusRejectMerge, "", nil
	}
	if len(commit.parents) == 0 {
		return CorpusCandidate{}, CorpusRejectNoParent, "", nil
	}
	if corpusRevertSubject.MatchString(commit.subject) {
		return CorpusCandidate{}, CorpusRejectRevert, "", nil
	}
	if !corpusFixSubject.MatchString(commit.subject) {
		return CorpusCandidate{}, CorpusRejectNotAFix, "", nil
	}
	parent := commit.parents[0]
	changed, err := corpusChangedPaths(ctx, request.RepositoryPath, parent, commit.sha)
	if err != nil {
		return CorpusCandidate{}, "", "", err
	}
	files := make([]*corpusFile, 0, len(changed))
	sourceFiles := 0
	for _, changedPath := range changed {
		eligible, test := classifyCorpusPath(changedPath)
		if !eligible {
			continue
		}
		files = append(files, &corpusFile{path: changedPath, test: test})
		if !test {
			sourceFiles++
		}
	}
	if sourceFiles == 0 {
		return CorpusCandidate{}, CorpusRejectNoSourceFiles, "", nil
	}
	if sourceFiles > corpusMaxSourceFiles {
		return CorpusCandidate{}, CorpusRejectTooManySourceFiles, strconv.Itoa(sourceFiles) + " source files", nil
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	identifiers, err := corpusParseDiff(ctx, request.RepositoryPath, parent, commit.sha, paths, files)
	if err != nil {
		return CorpusCandidate{}, "", "", err
	}
	total := 0
	for _, file := range files {
		if !file.test {
			total += file.changed
		}
	}
	if total < corpusMinChangedLines || total > corpusMaxChangedLines {
		return CorpusCandidate{}, CorpusRejectChangedLines, strconv.Itoa(total) + " changed lines", nil
	}
	// The agent explores the parent commit, so a file that does not exist there
	// has no a-side line numbers and cannot be a localization target.
	surviving := make([]*corpusFile, 0, len(files))
	for _, file := range files {
		lineCount, exists, err := corpusBlobLineCount(ctx, request.RepositoryPath, parent, file.path)
		if err != nil {
			return CorpusCandidate{}, "", "", err
		}
		if !exists {
			continue
		}
		file.regions = clampCorpusRegions(mergeCorpusRegions(file.regions), lineCount)
		if len(file.regions) == 0 {
			continue
		}
		surviving = append(surviving, file)
	}
	truth, mainChanged := corpusGroundTruth(surviving)
	if len(truth.ReadCoreFiles) == 0 {
		if mainChanged == 0 {
			return CorpusCandidate{}, CorpusRejectNoPreExistingSource, "", nil
		}
		return CorpusCandidate{}, CorpusRejectNoRegions, "", nil
	}
	statement, source := corpusStatement(commit, request.Tickets)
	if reason, detail := corpusLeakReason(statement, truth, identifiers); reason != "" {
		return CorpusCandidate{}, reason, detail, nil
	}
	switch length := len([]rune(statement)); {
	case length < corpusMinStatement:
		return CorpusCandidate{}, CorpusRejectStatementTooShort, strconv.Itoa(length) + " characters", nil
	case length > corpusMaxStatement:
		return CorpusCandidate{}, CorpusRejectStatementTooLong, strconv.Itoa(length) + " characters", nil
	}
	instanceID := slug + "__" + commit.sha[:12]
	total = 0
	for _, file := range surviving {
		if !file.test {
			total += file.changed
		}
	}
	return CorpusCandidate{
		InstanceID: instanceID, FixCommit: commit.sha, StatementSource: source, ChangedLines: total,
		Explore: ExploreRecord{InstanceID: instanceID, GroundTruth: truth},
		Issue: IssueRecord{
			InstanceID: instanceID, Repository: request.RepositoryName,
			BaseCommit: parent, ProblemStatement: statement,
		},
	}, "", "", nil
}

// corpusGroundTruth splits surviving files into the core (non-test) and
// optional (test) halves and elects one main file. Upstream weights a main file
// 3.0 against 2.0 for other core files, so electing every core file as main
// would silently flatten that weighting.
func corpusGroundTruth(files []*corpusFile) (GroundTruth, int) {
	truth := GroundTruth{
		ReadCoreFiles: []string{}, ReadCoreRegions: []Region{},
		ReadOptionalFilesMap: map[string][]string{}, ReadOptionalRegionsMap: map[string][]Region{},
		MainFiles: []string{},
	}
	mainPath, mainChanged := "", 0
	for _, file := range files {
		if file.test {
			truth.ReadOptionalFilesMap[file.path] = []string{file.path}
			truth.ReadOptionalRegionsMap[file.path] = file.regions
			continue
		}
		truth.ReadCoreFiles = append(truth.ReadCoreFiles, file.path)
		truth.ReadCoreRegions = append(truth.ReadCoreRegions, file.regions...)
		if file.changed > mainChanged || (file.changed == mainChanged && (mainPath == "" || file.path < mainPath)) {
			mainPath, mainChanged = file.path, file.changed
		}
	}
	sort.Strings(truth.ReadCoreFiles)
	sort.Slice(truth.ReadCoreRegions, func(left, right int) bool {
		if truth.ReadCoreRegions[left].Path != truth.ReadCoreRegions[right].Path {
			return truth.ReadCoreRegions[left].Path < truth.ReadCoreRegions[right].Path
		}
		return truth.ReadCoreRegions[left].Start < truth.ReadCoreRegions[right].Start
	})
	if mainPath != "" {
		truth.MainFiles = []string{mainPath}
	}
	return truth, mainChanged
}

// corpusStatement resolves the words the agent will read. Commit bodies are
// never used directly: measured against a real repository they name the changed
// functions and constants in their first paragraph, which removes the entire
// exploration task. A body contributes only tracker links.
func corpusStatement(commit corpusCommit, tickets CorpusTicketSet) (string, string) {
	if statement := corpusTicketStatement(commit.subject, tickets); statement != "" {
		return statement, CorpusStatementTicketFile
	}
	if statement := corpusTrackerStatement(commit.body); statement != "" {
		return statement, CorpusStatementTrackerSlug
	}
	return corpusStrippedSubject(commit.subject), CorpusStatementSubject
}

// corpusTicketStatement resolves a commit to reporter-written text. The
// trailing `(#NNNN)` of a squash-merged subject is the commit's own pull
// request and is the most reliable link, so it is tried first against
// `source_pull`; only the remaining inner references are matched against ticket
// refs. A trailing pull number is never matched against a ref.
func corpusTicketStatement(subject string, tickets CorpusTicketSet) string {
	if tickets.Count == 0 {
		return ""
	}
	pull, inner := corpusSubjectRefs(subject)
	if matched, ok := tickets.BySourcePull[pull]; pull > 0 && ok {
		return truncateStatement(joinCorpusTickets(matched))
	}
	matched := make([]CorpusTicket, 0, len(inner))
	seen := map[string]struct{}{}
	for _, ref := range inner {
		if _, already := seen[ref]; already {
			continue
		}
		seen[ref] = struct{}{}
		if ticket, ok := tickets.ByRef[ref]; ok {
			matched = append(matched, ticket)
		}
	}
	return truncateStatement(joinCorpusTickets(matched))
}

func joinCorpusTickets(tickets []CorpusTicket) string {
	parts := make([]string, 0, len(tickets))
	for _, ticket := range tickets {
		text := strings.TrimSpace(ticket.Title)
		if body := strings.TrimSpace(ticket.Body); body != "" {
			text = strings.TrimSpace(text + "\n\n" + body)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "; ")
}

// corpusSubjectRefs separates the trailing squash pull-request number from the
// issue references written inside the subject.
func corpusSubjectRefs(subject string) (int, []string) {
	trimmed := strings.TrimSpace(subject)
	pull := 0
	if match := corpusTrailingPull.FindStringSubmatch(trimmed); match != nil {
		pull, _ = strconv.Atoi(match[1])
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, match[0]))
	}
	inner := make([]string, 0, 2)
	for _, match := range corpusIssueRef.FindAllStringSubmatch(trimmed, -1) {
		inner = append(inner, match[1])
	}
	return pull, inner
}

// corpusTrackerStatement decodes reporter-written ticket titles out of tracker
// URLs. The slug is the only part of a commit body that describes the symptom
// instead of the fix.
func corpusTrackerStatement(body string) string {
	parts := make([]string, 0, 2)
	seen := map[string]struct{}{}
	for _, raw := range corpusTrackerURL.FindAllString(body, -1) {
		title := corpusDecodeTrackerSlug(raw)
		if title == "" {
			continue
		}
		if _, already := seen[title]; already {
			continue
		}
		seen[title] = struct{}{}
		parts = append(parts, title)
	}
	return truncateStatement(strings.Join(parts, "; "))
}

func corpusDecodeTrackerSlug(raw string) string {
	parsed, err := url.Parse(strings.TrimRight(raw, ".,;:"))
	if err != nil || parsed.Path == "" {
		return ""
	}
	segment := path.Base(strings.TrimRight(parsed.Path, "/"))
	if segment == "" || segment == "." || segment == "/" {
		return ""
	}
	if decoded, err := url.PathUnescape(segment); err == nil {
		segment = decoded
	}
	// A file or a bare numeric identifier is a link, not a reported symptom.
	if corpusSlugExtension.MatchString(segment) {
		return ""
	}
	segment = corpusSlugTrailingID.ReplaceAllString(segment, "")
	title := corpusWhitespace.ReplaceAllString(strings.NewReplacer("-", " ", "_", " ").Replace(segment), " ")
	title = strings.TrimSpace(title)
	if len(strings.Fields(title)) < 3 {
		return ""
	}
	return title
}

func corpusStrippedSubject(subject string) string {
	stripped := corpusFixPrefix.ReplaceAllString(strings.TrimSpace(subject), "")
	stripped = corpusIssueRefGroup.ReplaceAllString(stripped, "")
	return strings.TrimSpace(corpusWhitespace.ReplaceAllString(stripped, " "))
}

func truncateStatement(statement string) string {
	statement = strings.TrimSpace(statement)
	runes := []rune(statement)
	if len(runes) <= corpusMaxStatement {
		return statement
	}
	return strings.TrimSpace(string(runes[:corpusMaxStatement]))
}

// corpusLeakReason applies StatementLeak to one candidate's ground truth.
func corpusLeakReason(statement string, truth GroundTruth, identifiers map[string]struct{}) (string, string) {
	paths := append([]string{}, truth.ReadCoreFiles...)
	return StatementLeak(statement, append(paths, flattenStrings(truth.ReadOptionalFilesMap)...), identifiers)
}

// StatementLeak reports why a problem statement already answers the question it
// is supposed to pose, returning a CorpusRejectLeak* reason and the matched
// needle, or two empty strings when the statement is safe to show an agent.
//
// Both sides are reduced to letters and digits before matching. A decoded
// tracker slug turns `SESSION_DEAD_KEYWORDS` into three ordinary-looking words
// that a plain substring test would miss, and the same erasure hides a path
// written as "utils socket py". The line-number check deliberately runs against
// the statement as written, because normalization would erase the colon.
//
// It is exported because a corpus of injected defects must hold its
// hand-authored symptom descriptions to exactly this standard; the check is a
// property of the measurement, not of how the instance was produced.
func StatementLeak(statement string, paths []string, identifiers map[string]struct{}) (string, string) {
	if corpusLineReference.MatchString(statement) {
		return CorpusRejectLeakLineReference, ""
	}
	normalized := normalizeLeakText(statement)
	if normalized == "" {
		return "", ""
	}
	for _, needle := range corpusPathNeedles(paths) {
		if strings.Contains(normalized, needle) {
			return CorpusRejectLeakPath, needle
		}
	}
	needles := make([]string, 0, len(identifiers))
	for identifier := range identifiers {
		needles = append(needles, identifier)
	}
	sort.Strings(needles)
	for _, identifier := range needles {
		needle := normalizeLeakText(identifier)
		if len(needle) < corpusIdentifierFloor {
			continue
		}
		if strings.Contains(normalized, needle) {
			return CorpusRejectLeakIdentifier, identifier
		}
		// Measured on a real corpus: a statement saying "voice mode" against a
		// changed line declaring `is_voice_mode` escapes the whole-identifier
		// test, because the leak is a run of the identifier's own words rather
		// than the identifier itself.
		if match := identifierWordRun(identifier, normalized); match != "" {
			return CorpusRejectLeakIdentifier, identifier + " (" + match + ")"
		}
	}
	return "", ""
}

// identifierWordRun reports the first run of two or more of an identifier's own
// words that appears contiguously in the normalized statement. Short words are
// ignored on both ends because a two-letter fragment carries no leak signal.
func identifierWordRun(identifier, normalized string) string {
	words := splitIdentifierWords(identifier)
	for start := 0; start < len(words); start++ {
		joined := words[start]
		for end := start + 1; end < len(words); end++ {
			joined += words[end]
			if len(joined) >= corpusWordRunFloor && strings.Contains(normalized, joined) {
				return joined
			}
		}
	}
	return ""
}

// splitIdentifierWords breaks snake_case, camelCase, and PascalCase into the
// words a person would type, dropping fragments too short to mean anything.
func splitIdentifierWords(identifier string) []string {
	var words []string
	var current strings.Builder
	flush := func() {
		if word := normalizeLeakText(current.String()); len(word) >= corpusWordFloor {
			words = append(words, word)
		} else if current.Len() > 0 {
			// A short fragment breaks the run rather than joining it, so
			// `user_id` never contributes a "userid" needle.
			words = append(words, "")
		}
		current.Reset()
	}
	for _, symbol := range identifier {
		if symbol == '_' || unicode.IsUpper(symbol) || unicode.IsDigit(symbol) {
			flush()
		}
		if symbol != '_' {
			current.WriteRune(symbol)
		}
	}
	flush()
	compacted := make([]string, 0, len(words))
	for _, word := range words {
		if word != "" {
			compacted = append(compacted, word)
			continue
		}
		if len(compacted) > 0 && compacted[len(compacted)-1] != "" {
			compacted = append(compacted, "")
		}
	}
	return splitOnBlanks(compacted)
}

// splitOnBlanks keeps only the longest usable neighbouring words so a run never
// spans a dropped fragment.
func splitOnBlanks(words []string) []string {
	longest, current := []string{}, []string{}
	for _, word := range words {
		if word == "" {
			if len(current) > len(longest) {
				longest = current
			}
			current = nil
			continue
		}
		current = append(current, word)
	}
	if len(current) > len(longest) {
		longest = current
	}
	return longest
}

// corpusPathNeedles expands each ground-truth path into the forms a reporter
// might type. The extension-stripped stem keeps the identifier length floor
// because a three-letter stem matches too much ordinary prose to mean anything.
func corpusPathNeedles(paths []string) []string {
	needles := map[string]struct{}{}
	for _, value := range paths {
		base := path.Base(value)
		stem := strings.TrimSuffix(base, path.Ext(base))
		for _, form := range []string{value, base} {
			if needle := normalizeLeakText(form); needle != "" {
				needles[needle] = struct{}{}
			}
		}
		if needle := normalizeLeakText(stem); len(needle) >= corpusIdentifierFloor {
			needles[needle] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(needles))
	for needle := range needles {
		ordered = append(ordered, needle)
	}
	sort.Strings(ordered)
	return ordered
}

func normalizeLeakText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, symbol := range strings.ToLower(value) {
		if unicode.IsLetter(symbol) || unicode.IsDigit(symbol) {
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}

// codeShapedIdentifier separates identifiers from ordinary prose words. A
// capitalized English word is not evidence of a leak, but `FooBar`, `fooBar`,
// `foo_bar`, and `foo2` are.
func codeShapedIdentifier(identifier string) bool {
	if len(identifier) < corpusIdentifierFloor {
		return false
	}
	if strings.Contains(identifier, "_") {
		return true
	}
	upper, lower, digit := 0, 0, false
	for _, symbol := range identifier {
		switch {
		case unicode.IsDigit(symbol):
			digit = true
		case unicode.IsUpper(symbol):
			upper++
		case unicode.IsLower(symbol):
			lower++
		}
	}
	if digit {
		return true
	}
	if unicode.IsLower(rune(identifier[0])) {
		return upper > 0
	}
	// PascalCase needs a second capitalized word; otherwise `Session` and every
	// sentence-initial noun would be treated as a leaked symbol.
	return upper >= 2 && lower > 0
}

func selectCorpusCandidates(candidates []CorpusCandidate, count int, seed string) []CorpusCandidate {
	ordered := make([]int, len(candidates))
	for index := range candidates {
		ordered[index] = index
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		leftRank := campaignRank(seed, candidates[ordered[left]].InstanceID)
		rightRank := campaignRank(seed, candidates[ordered[right]].InstanceID)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return candidates[ordered[left]].InstanceID < candidates[ordered[right]].InstanceID
	})
	if count > len(ordered) {
		count = len(ordered)
	}
	drawn := ordered[:count]
	sort.Ints(drawn)
	selected := make([]CorpusCandidate, 0, count)
	for _, index := range drawn {
		selected = append(selected, candidates[index])
	}
	return selected
}

func corpusCommits(ctx context.Context, request CorpusRequest) ([]corpusCommit, error) {
	args := []string{"log", "--reverse", "--since=" + request.Since, "--format=%x1e%H%x1f%P%x1f%s%x1f%b", "HEAD"}
	if strings.TrimSpace(request.Until) != "" {
		args = append(args[:3], append([]string{"--until=" + request.Until}, args[3:]...)...)
	}
	output, err := corpusGit(ctx, request.RepositoryPath, args...)
	if err != nil {
		return nil, fmt.Errorf("read fix history: %w", err)
	}
	commits := make([]corpusCommit, 0, 256)
	for _, record := range strings.Split(output, "\x1e") {
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.SplitN(record, "\x1f", 4)
		if len(fields) != 4 {
			return nil, fmt.Errorf("unparsable commit record %q", record)
		}
		commits = append(commits, corpusCommit{
			sha: strings.TrimSpace(fields[0]), parents: strings.Fields(fields[1]),
			subject: strings.TrimSpace(fields[2]), body: fields[3],
		})
	}
	return commits, nil
}

func corpusChangedPaths(ctx context.Context, root, parent, commit string) ([]string, error) {
	output, err := corpusGit(ctx, root, "diff", "--no-renames", "--name-only", "-z", parent, commit)
	if err != nil {
		return nil, fmt.Errorf("list changed paths of %s: %w", commit, err)
	}
	paths := make([]string, 0, 16)
	for _, value := range strings.Split(output, "\x00") {
		if value != "" {
			paths = append(paths, value)
		}
	}
	return paths, nil
}

// corpusParseDiff reads pre-fix hunk positions and the identifiers the fix
// touched. Both come from the same `--unified=0` diff so they cannot disagree.
func corpusParseDiff(ctx context.Context, root, parent, commit string, paths []string, files []*corpusFile) (map[string]struct{}, error) {
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", parent, commit, "--"}, paths...)
	output, err := corpusGit(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("diff %s: %w", commit, err)
	}
	return parseCorpusDiff(output, commit, files)
}

// parseCorpusDiff converts hunk headers into pre-fix regions. A unified-diff
// header omits the count when it is exactly one, so an absent group and an
// explicit ",0" mean opposite things: the first is a one-line modification, the
// second a pure insertion. Collapsing them would turn every single-line fix
// into an anchor and misplace half of the ground truth.
func parseCorpusDiff(output, commit string, files []*corpusFile) (map[string]struct{}, error) {
	hunks, err := ParseUnifiedDiff(output)
	if err != nil {
		return nil, fmt.Errorf("parse diff of %s: %w", commit, err)
	}
	byPath := make(map[string]*corpusFile, len(files))
	for _, file := range files {
		byPath[file.path] = file
	}
	changed := make([]string, 0, 64)
	for _, hunk := range hunks {
		file, tracked := byPath[hunk.Path]
		if !tracked {
			continue
		}
		file.regions = append(file.regions, hunk.RegionBefore())
		file.changed += len(hunk.Lines)
		changed = append(changed, hunk.Lines...)
	}
	return CodeShapedIdentifiers(changed), nil
}

// DiffHunk is one parsed `--unified=0` hunk with both sides resolved. Both are
// kept because the two ways of building a corpus localize against different
// trees: a fix mined from history is explored at the pre-fix commit, while an
// injected defect is explored with the defect already applied.
type DiffHunk struct {
	Path     string
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	// Lines are the changed body lines with their "+"/"-" prefix removed.
	Lines []string
}

// RegionBefore returns the pre-patch (a-side) interval. A zero old count is a
// pure insertion, which has no removed line to point at, so it collapses to a
// one-line anchor at the position the new code was placed after: "where this
// should have been written" is still a localization target.
func (hunk DiffHunk) RegionBefore() Region {
	return hunkRegion(hunk.Path, hunk.OldStart, hunk.OldCount)
}

// RegionAfter returns the post-patch (b-side) interval under the same rule.
func (hunk DiffHunk) RegionAfter() Region {
	return hunkRegion(hunk.Path, hunk.NewStart, hunk.NewCount)
}

func hunkRegion(path string, start, count int) Region {
	if count > 0 {
		return Region{Path: path, Start: start, End: start + count - 1}
	}
	anchor := max(1, start)
	return Region{Path: path, Start: anchor, End: anchor}
}

// ParseUnifiedDiff reads `git diff --unified=0` output into hunks. A unified
// header omits the count when it is exactly one, so an absent group and an
// explicit ",0" mean opposite things: the first is a one-line modification, the
// second a pure insertion. The distinction is resolved here, once, because
// collapsing it silently relocates ground truth rather than failing.
func ParseUnifiedDiff(diff string) ([]DiffHunk, error) {
	var hunks []DiffHunk
	path := ""
	// A removed body line is rendered with a leading "-", so a deleted SQL
	// comment arrives as "--- comment" and would otherwise be read as a file
	// header. Only the block between "diff --git" and the first hunk is one.
	header := false
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, header = "", true
		case header && strings.HasPrefix(line, "--- "):
			path = diffHeaderPath(line, "--- ", "a/")
		case header && strings.HasPrefix(line, "+++ "):
			// An added file has no a-side name, so the b-side supplies it.
			if path == "" {
				path = diffHeaderPath(line, "+++ ", "b/")
			}
		case strings.HasPrefix(line, "@@ "):
			header = false
			match := corpusHunkHeader.FindStringSubmatch(line)
			if match == nil {
				return nil, fmt.Errorf("unparsable hunk header %q", line)
			}
			if path == "" {
				continue
			}
			hunks = append(hunks, DiffHunk{
				Path:     path,
				OldStart: atoiDefault(match[1], 0), OldCount: atoiDefault(match[2], 1),
				NewStart: atoiDefault(match[3], 0), NewCount: atoiDefault(match[4], 1),
			})
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
			if len(hunks) == 0 || hunks[len(hunks)-1].Path != path {
				continue
			}
			hunks[len(hunks)-1].Lines = append(hunks[len(hunks)-1].Lines, line[1:])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan unified diff: %w", err)
	}
	return hunks, nil
}

func diffHeaderPath(line, prefix, side string) string {
	value := strings.TrimPrefix(line, prefix)
	if value == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(value, side)
}

// atoiDefault resolves an optional unified-diff count group: an absent group
// means one, and a present group means exactly what it says, including zero.
func atoiDefault(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// CodeShapedIdentifiers extracts the symbols a patch touched, discarding
// ordinary prose words. It is the input to StatementLeak for both corpus
// builders, so a hand-authored symptom description is held to the same
// standard as a mined commit subject.
func CodeShapedIdentifiers(lines []string) map[string]struct{} {
	identifiers := map[string]struct{}{}
	for _, line := range lines {
		for _, identifier := range corpusIdentifier.FindAllString(line, -1) {
			if codeShapedIdentifier(identifier) {
				identifiers[identifier] = struct{}{}
			}
		}
	}
	return identifiers
}

// MergeRegions collapses overlapping and near-adjacent intervals into the spans
// a reader would actually open.
func MergeRegions(regions []Region) []Region { return mergeCorpusRegions(regions) }

// ClampRegions confines intervals to a file's real length and drops any that
// start past its end.
func ClampRegions(regions []Region, lineCount int) []Region {
	return clampCorpusRegions(regions, lineCount)
}

func corpusBlobLineCount(ctx context.Context, root, parent, filePath string) (int, bool, error) {
	body, err := corpusGitRaw(ctx, root, "cat-file", "blob", parent+":"+filePath)
	if err != nil {
		// A path absent from the parent tree is an ordinary selection outcome,
		// not a Git failure worth aborting the whole corpus for.
		return 0, false, nil
	}
	if len(body) == 0 {
		return 0, true, nil
	}
	count := bytes.Count(body, []byte{'\n'})
	if body[len(body)-1] != '\n' {
		count++
	}
	return count, true, nil
}

func mergeCorpusRegions(regions []Region) []Region {
	if len(regions) == 0 {
		return nil
	}
	ordered := append([]Region{}, regions...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Start != ordered[right].Start {
			return ordered[left].Start < ordered[right].Start
		}
		return ordered[left].End < ordered[right].End
	})
	merged := []Region{ordered[0]}
	for _, region := range ordered[1:] {
		last := &merged[len(merged)-1]
		if region.Start <= last.End+corpusRegionGap+1 {
			if region.End > last.End {
				last.End = region.End
			}
			continue
		}
		merged = append(merged, region)
	}
	return merged
}

func clampCorpusRegions(regions []Region, lineCount int) []Region {
	clamped := make([]Region, 0, len(regions))
	for _, region := range regions {
		if lineCount < 1 || region.Start > lineCount {
			continue
		}
		region.Start = max(1, region.Start)
		region.End = min(region.End, lineCount)
		if region.End < region.Start {
			continue
		}
		clamped = append(clamped, region)
	}
	if len(clamped) == 0 {
		return nil
	}
	return clamped
}

// classifyCorpusPath reports whether a changed path belongs in ground truth and
// whether it is a test. Non-source text files are neither fatal nor counted:
// a fix that also edits a changelog is still a fix.
func classifyCorpusPath(value string) (bool, bool) {
	if value == "" || corpusUnsafePath.MatchString(value) {
		return false, false
	}
	normalized := strings.ToLower(value)
	if strings.Contains(normalized, "egg-info") {
		return false, false
	}
	guard := "/" + normalized
	for _, segment := range corpusRejectedSegments {
		if strings.Contains(guard, "/"+segment+"/") {
			return false, false
		}
	}
	base := path.Base(value)
	if _, rejected := corpusRejectedNames[base]; rejected {
		return false, false
	}
	extension := strings.ToLower(path.Ext(base))
	if _, rejected := corpusRejectedExtensions[extension]; rejected {
		return false, false
	}
	if strings.HasPrefix(extension, ".woff") || strings.HasSuffix(strings.ToLower(base), ".min.js") {
		return false, false
	}
	if _, source := corpusSourceExtensions[extension]; !source {
		return false, false
	}
	return true, isCorpusTestPath(value)
}

func isCorpusTestPath(value string) bool {
	guard := strings.ToLower("/" + value)
	for _, segment := range []string{"/tests/", "/test/", "/__tests__/"} {
		if strings.Contains(guard, segment) {
			return true
		}
	}
	base := strings.ToLower(path.Base(value))
	if strings.HasPrefix(base, "test_") {
		return true
	}
	stem := strings.TrimSuffix(base, path.Ext(base))
	return strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
}

// corpusSlug produces the instance-identifier prefix. It must satisfy the
// campaign manifest's safe-identifier rule so a private corpus can be sampled
// by the same immutable manifest path as the public dataset.
func corpusSlug(name string) (string, error) {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(strings.TrimSpace(name)) {
		if (symbol >= 'a' && symbol <= 'z') || (symbol >= '0' && symbol <= '9') {
			builder.WriteRune(symbol)
			continue
		}
		builder.WriteByte('-')
	}
	slug := strings.Trim(corpusCollapseDashes(builder.String()), "-")
	if slug == "" {
		return "", fmt.Errorf("repository name %q has no identifier characters", name)
	}
	return slug, nil
}

func corpusCollapseDashes(value string) string {
	var builder strings.Builder
	previous := rune(0)
	for _, symbol := range value {
		if symbol == '-' && previous == '-' {
			continue
		}
		builder.WriteRune(symbol)
		previous = symbol
	}
	return builder.String()
}

func corpusValidateDate(name, value string, required bool) error {
	if strings.TrimSpace(value) == "" {
		if required {
			return fmt.Errorf("--%s is required", name)
		}
		return nil
	}
	if !corpusDate.MatchString(value) {
		return fmt.Errorf("--%s must be YYYY-MM-DD", name)
	}
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("--%s is not a calendar date: %w", name, err)
	}
	return nil
}

// LoadCorpusTickets reads pre-fetched reporter-written issues. The file is an
// ordinary pinned campaign input: its digest belongs in the audit report so a
// corpus can be rebuilt from the same words.
func LoadCorpusTickets(filePath string) (CorpusTicketSet, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return CorpusTicketSet{}, fmt.Errorf("read ticket statements: %w", err)
	}
	defer file.Close()
	tickets := CorpusTicketSet{ByRef: map[string]CorpusTicket{}, BySourcePull: map[int][]CorpusTicket{}}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var ticket CorpusTicket
		if err := json.Unmarshal(scanner.Bytes(), &ticket); err != nil {
			return CorpusTicketSet{}, fmt.Errorf("decode %s line %d: %w", filePath, line, err)
		}
		ref := strings.TrimPrefix(strings.TrimSpace(ticket.Ref), "#")
		if ref == "" || strings.TrimSpace(ticket.Title) == "" {
			return CorpusTicketSet{}, fmt.Errorf("%s line %d requires a ref and a title", filePath, line)
		}
		if _, duplicate := tickets.ByRef[ref]; duplicate {
			return CorpusTicketSet{}, fmt.Errorf("ticket ref %q occurs more than once in %s", ref, filePath)
		}
		ticket.Ref = ref
		tickets.ByRef[ref] = ticket
		if ticket.SourcePull > 0 {
			tickets.BySourcePull[ticket.SourcePull] = append(tickets.BySourcePull[ticket.SourcePull], ticket)
		}
		tickets.Count++
	}
	if err := scanner.Err(); err != nil {
		return CorpusTicketSet{}, fmt.Errorf("scan %s: %w", filePath, err)
	}
	// One pull request can fix several tickets; a stable join order keeps the
	// emitted statement byte-identical across rebuilds.
	for pull := range tickets.BySourcePull {
		group := tickets.BySourcePull[pull]
		sort.Slice(group, func(left, right int) bool { return group[left].Ref < group[right].Ref })
	}
	return tickets, nil
}

// WriteCorpusJSONL publishes the two joined datasets. It writes through the
// atomic helper, so an existing corpus is never silently replaced by a rebuild
// that a pinned digest still claims to describe.
func WriteCorpusJSONL(explorePath, issuesPath string, candidates []CorpusCandidate) (string, string, error) {
	explore, err := encodeCorpusJSONL(candidates, func(candidate CorpusCandidate) any { return candidate.Explore })
	if err != nil {
		return "", "", err
	}
	issues, err := encodeCorpusJSONL(candidates, func(candidate CorpusCandidate) any { return candidate.Issue })
	if err != nil {
		return "", "", err
	}
	if err := WriteFileAtomic(explorePath, explore, 0o600); err != nil {
		return "", "", err
	}
	if err := WriteFileAtomic(issuesPath, issues, 0o600); err != nil {
		return "", "", err
	}
	return digestBytes(explore), digestBytes(issues), nil
}

func encodeCorpusJSONL(candidates []CorpusCandidate, project func(CorpusCandidate) any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, candidate := range candidates {
		if err := encoder.Encode(project(candidate)); err != nil {
			return nil, fmt.Errorf("encode corpus row %q: %w", candidate.InstanceID, err)
		}
	}
	return buffer.Bytes(), nil
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func sortedKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func corpusGit(ctx context.Context, root string, args ...string) (string, error) {
	body, err := corpusGitRaw(ctx, root, args...)
	return string(body), err
}

// corpusGitRaw reads a foreign repository without letting its or the machine's
// Git configuration change what is measured: end-of-line conversion would move
// hunk offsets and blob line counts on Windows, and quoted paths would break
// the diff-to-path association.
func corpusGitRaw(ctx context.Context, root string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "core.autocrlf=false", "-c", "core.eol=lf", "-c", "core.quotepath=false"}, args...)
	command := exec.CommandContext(ctx, "git", full...)
	command.Dir = root
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return stdout.Bytes(), nil
}
