package sweexplore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// InjectedSchemaVersion versions only the audit report. The emitted rows stay
// the ordinary ExploreRecord and IssueRecord shapes, which is what lets an
// authored corpus run through the same join, campaign, and scoring path as the
// mined and public ones.
const InjectedSchemaVersion = 1

// CorpusStatementAuthored marks a symptom description written by us rather than
// recovered from a repository's history. It cannot leak by construction, which
// is the entire reason the injected corpus exists — but it is still held to the
// same automated leak filter, because "we were careful" is not evidence.
const CorpusStatementAuthored = "authored"

// Authoring bounds from the injected-defect contract. A larger defect stops
// measuring search and starts measuring reading.
const (
	injectedMaxFiles        = 3
	injectedMaxChangedLines = 50
	injectedMinStatement    = 25
)

var injectedDoubleQuoted = regexp.MustCompile(`"([^"\n]*)"`)

// DefectRecord is one authored defect as its author wrote it. Unknown fields
// are ignored so the authoring workflow can carry extra provenance without
// invalidating a corpus; CausalNote and AuthorRationale are review material and
// are never shown to an agent.
type DefectRecord struct {
	InstanceID       string `json:"instance_id"`
	Repository       string `json:"repo"`
	BaseCommit       string `json:"base_commit"`
	ProblemStatement string `json:"problem_statement"`
	DefectPatch      string `json:"defect_patch"`
	Stratum          string `json:"stratum"`
	CausalNote       string `json:"causal_note"`
	AuthorRationale  string `json:"author_rationale"`
}

// InjectedRequest is the complete input to one injected-corpus build.
type InjectedRequest struct {
	DefectsPath    string
	RepositoryPath string
	RepositoryName string
	IssuesPath     string
}

// InjectedExemption records one leak the literal stratum was allowed to keep.
// Every exemption is reported rather than silently applied: the exemption
// exists so a symptom may quote the string a user would see, and only a human
// can confirm that is what actually happened.
type InjectedExemption struct {
	InstanceID string `json:"instance_id"`
	Identifier string `json:"identifier"`
	Match      string `json:"match"`
	Quote      string `json:"quote"`
}

// InjectedInstanceReport is the per-instance audit row.
type InjectedInstanceReport struct {
	InstanceID      string              `json:"instance_id"`
	Stratum         string              `json:"stratum"`
	BaseCommit      string              `json:"base_commit"`
	DefectPatch     string              `json:"defect_patch"`
	FileCount       int                 `json:"file_count"`
	ChangedLines    int                 `json:"changed_lines"`
	CoreRegions     int                 `json:"core_regions"`
	OptionalRegions int                 `json:"optional_regions"`
	CoreFiles       []string            `json:"core_files"`
	OptionalFiles   []string            `json:"optional_files,omitempty"`
	MainFiles       []string            `json:"main_files"`
	Exemptions      []InjectedExemption `json:"literal_exemptions,omitempty"`
}

// InjectedStratumTotals is the per-stratum summary the report leads with,
// because per stratum is the only unit an authored corpus may be read in.
type InjectedStratumTotals struct {
	Instances    int `json:"instances"`
	Files        int `json:"files"`
	ChangedLines int `json:"changed_lines"`
	CoreRegions  int `json:"core_regions"`
	Exemptions   int `json:"exemptions"`
}

// InjectedReport is the auditable record of one injected-corpus build.
type InjectedReport struct {
	SchemaVersion  int                              `json:"schema_version"`
	Repository     string                           `json:"repository"`
	RepositoryPath string                           `json:"repository_path"`
	DefectsPath    string                           `json:"defects_path"`
	InstanceCount  int                              `json:"instance_count"`
	StratumTotals  map[string]InjectedStratumTotals `json:"stratum_totals"`
	Exemptions     []InjectedExemption              `json:"literal_exemptions"`
	Instances      []InjectedInstanceReport         `json:"instances"`
}

// InjectedResult carries the emitted rows in input order together with the
// report that explains them.
type InjectedResult struct {
	Candidates []CorpusCandidate
	Report     InjectedReport
}

type injectedFile struct {
	path    string
	test    bool
	changed int
	regions []Region
}

// BuildInjectedCorpus turns authored defects into benchmark instances. Every
// rule it enforces is fatal rather than a filter: a mined commit that fails a
// check is one of thousands and is skipped, but a skipped authored defect
// produces a corpus that no longer matches the patches its author wrote, and
// nothing downstream would notice.
func BuildInjectedCorpus(ctx context.Context, request InjectedRequest) (InjectedResult, error) {
	if strings.TrimSpace(request.RepositoryPath) == "" || !isGitRepositoryDirectory(request.RepositoryPath) {
		return InjectedResult{}, fmt.Errorf("corpus repository %q is not a Git repository", request.RepositoryPath)
	}
	if strings.TrimSpace(request.RepositoryName) == "" {
		return InjectedResult{}, errors.New("repository name is required")
	}
	if strings.TrimSpace(request.IssuesPath) == "" {
		return InjectedResult{}, errors.New("issues output path is required")
	}
	defects, err := LoadDefectRecords(request.DefectsPath)
	if err != nil {
		return InjectedResult{}, err
	}
	if len(defects) == 0 {
		return InjectedResult{}, fmt.Errorf("%s contains no defect records", request.DefectsPath)
	}
	// Patches are handed to Git commands that run inside a temporary worktree,
	// so a path relative to the caller's directory would resolve against that
	// worktree instead and read as "the patch does not apply".
	defectsDirectory, err := filepath.Abs(filepath.Dir(request.DefectsPath))
	if err != nil {
		return InjectedResult{}, err
	}
	issuesDirectory, err := filepath.Abs(filepath.Dir(request.IssuesPath))
	if err != nil {
		return InjectedResult{}, err
	}
	worktrees, err := newDefectWorktrees(request.RepositoryPath)
	if err != nil {
		return InjectedResult{}, err
	}
	defer worktrees.Close(ctx)
	report := InjectedReport{
		SchemaVersion: InjectedSchemaVersion, Repository: request.RepositoryName,
		RepositoryPath: request.RepositoryPath, DefectsPath: request.DefectsPath,
		StratumTotals: map[string]InjectedStratumTotals{}, Exemptions: []InjectedExemption{},
		Instances: []InjectedInstanceReport{},
	}
	candidates := make([]CorpusCandidate, 0, len(defects))
	seen := make(map[string]struct{}, len(defects))
	for _, defect := range defects {
		if _, duplicate := seen[defect.InstanceID]; duplicate {
			return InjectedResult{}, fmt.Errorf("instance %q occurs more than once in %s", defect.InstanceID, request.DefectsPath)
		}
		seen[defect.InstanceID] = struct{}{}
		candidate, instanceReport, err := injectedCandidate(ctx, request, defectsDirectory, issuesDirectory, worktrees, defect)
		if err != nil {
			return InjectedResult{}, err
		}
		candidates = append(candidates, candidate)
		report.Instances = append(report.Instances, instanceReport)
		report.Exemptions = append(report.Exemptions, instanceReport.Exemptions...)
		totals := report.StratumTotals[defect.Stratum]
		totals.Instances++
		totals.Files += instanceReport.FileCount
		totals.ChangedLines += instanceReport.ChangedLines
		totals.CoreRegions += instanceReport.CoreRegions
		totals.Exemptions += len(instanceReport.Exemptions)
		report.StratumTotals[defect.Stratum] = totals
	}
	report.InstanceCount = len(candidates)
	return InjectedResult{Candidates: candidates, Report: report}, nil
}

// injectedCandidate validates one authored defect and derives its ground truth
// mechanically from the patch. Ground truth is never hand-written, so an
// authoring mistake shows up as a refused build rather than as an instance
// scored against coordinates nobody checked.
func injectedCandidate(ctx context.Context, request InjectedRequest, defectsDirectory, issuesDirectory string, worktrees *defectWorktrees, defect DefectRecord) (CorpusCandidate, InjectedInstanceReport, error) {
	fail := func(format string, args ...any) (CorpusCandidate, InjectedInstanceReport, error) {
		return CorpusCandidate{}, InjectedInstanceReport{}, fmt.Errorf("instance %q: "+format, append([]any{defect.InstanceID}, args...)...)
	}
	if !safeArtifactID.MatchString(defect.InstanceID) {
		return CorpusCandidate{}, InjectedInstanceReport{}, fmt.Errorf("defect record has an unsafe instance id %q", defect.InstanceID)
	}
	if defect.Repository != request.RepositoryName {
		return fail("repo is %q, but this build declares %q", defect.Repository, request.RepositoryName)
	}
	if !fullGitCommit.MatchString(defect.BaseCommit) {
		// An abbreviated commit would resolve here and then fail every later
		// HEAD comparison, which reads as a broken workspace rather than a
		// broken corpus row.
		return fail("base_commit %q must be a full 40-character Git commit", defect.BaseCommit)
	}
	if !ValidStratum(defect.Stratum) {
		return fail("stratum %q is not one of %s, %s, %s, %s", defect.Stratum, StratumLiteral, StratumGeneric, StratumCrossFile, StratumFamily)
	}
	statement := strings.TrimSpace(defect.ProblemStatement)
	switch length := len([]rune(statement)); {
	case length == 0:
		return fail("problem_statement is empty")
	case length < injectedMinStatement:
		return fail("problem_statement is %d characters, want at least %d", length, injectedMinStatement)
	case length > corpusMaxStatement:
		return fail("problem_statement is %d characters, want at most %d", length, corpusMaxStatement)
	}
	patchPath, err := injectedPatchPath(defectsDirectory, defect.DefectPatch)
	if err != nil {
		return fail("%w", err)
	}
	emittedPatch, err := injectedRelativePatch(issuesDirectory, patchPath)
	if err != nil {
		return fail("%w", err)
	}
	root, err := worktrees.at(ctx, defect.BaseCommit)
	if err != nil {
		return fail("%w", err)
	}
	if _, err := corpusGitRaw(ctx, root, "apply", "--check", "--whitespace=nowarn", patchPath); err != nil {
		return fail("defect patch %s does not apply to %s: %w", patchPath, defect.BaseCommit, err)
	}
	if _, err := corpusGitRaw(ctx, root, "apply", "--whitespace=nowarn", patchPath); err != nil {
		return fail("apply defect patch %s: %w", patchPath, err)
	}
	hunks, err := injectedAppliedHunks(ctx, root)
	if err != nil {
		return fail("%w", err)
	}
	if len(hunks) == 0 {
		return fail("defect patch %s changes nothing at %s", patchPath, defect.BaseCommit)
	}
	files, changedLines, changedText := injectedPatchFiles(hunks)
	if len(files) > injectedMaxFiles {
		return fail("defect patch touches %d files, the contract allows at most %d", len(files), injectedMaxFiles)
	}
	if changedLines > injectedMaxChangedLines {
		return fail("defect patch changes %d lines, the contract allows at most %d", changedLines, injectedMaxChangedLines)
	}
	// The agent explores the patched tree, so ground truth is clamped to the
	// line counts of that tree and not of the commit the patch was written
	// against.
	for _, file := range files {
		lineCount, err := patchedLineCount(filepath.Join(root, filepath.FromSlash(file.path)))
		if err != nil {
			return fail("%w", err)
		}
		file.regions = ClampRegions(MergeRegions(file.regions), lineCount)
	}
	truth, mainChanged := injectedGroundTruth(files)
	if len(truth.ReadCoreFiles) == 0 || mainChanged == 0 {
		return fail("defect patch %s leaves no non-test ground-truth region", patchPath)
	}
	paths := append(append([]string{}, truth.ReadCoreFiles...), flattenStrings(truth.ReadOptionalFilesMap)...)
	reason, detail, exemptions := injectedLeak(defect.InstanceID, statement, defect.Stratum, paths, CodeShapedIdentifiers(changedText))
	if reason != "" {
		return fail("problem_statement %s (%s); rewrite the symptom, do not weaken the filter", reason, detail)
	}
	optionalRegions := 0
	for _, regions := range truth.ReadOptionalRegionsMap {
		optionalRegions += len(regions)
	}
	candidate := CorpusCandidate{
		InstanceID: defect.InstanceID, StatementSource: CorpusStatementAuthored, ChangedLines: changedLines,
		Explore: ExploreRecord{InstanceID: defect.InstanceID, GroundTruth: truth},
		Issue: IssueRecord{
			InstanceID: defect.InstanceID, Repository: defect.Repository, BaseCommit: defect.BaseCommit,
			ProblemStatement: statement, DefectPatch: emittedPatch, Stratum: defect.Stratum,
		},
	}
	return candidate, InjectedInstanceReport{
		InstanceID: defect.InstanceID, Stratum: defect.Stratum, BaseCommit: defect.BaseCommit,
		DefectPatch: emittedPatch, FileCount: len(files), ChangedLines: changedLines,
		CoreRegions: len(truth.ReadCoreRegions), OptionalRegions: optionalRegions,
		CoreFiles: truth.ReadCoreFiles, OptionalFiles: sortedKeys(truth.ReadOptionalFilesMap),
		MainFiles: truth.MainFiles, Exemptions: exemptions,
	}, nil
}

// injectedAppliedHunks re-derives the patch from the tree it produced, at zero
// context. Ground truth must not depend on how the author generated the file:
// the same defect written with three lines of context would otherwise claim six
// extra lines per hunk as the answer, and the agent would be scored for finding
// code the defect never touched.
//
// Intent-to-add is what makes a file the patch created visible to `git diff`,
// which otherwise reports it as untracked and contributes no hunk at all.
func injectedAppliedHunks(ctx context.Context, root string) ([]DiffHunk, error) {
	if _, err := corpusGitRaw(ctx, root, "add", "--all", "--intent-to-add"); err != nil {
		return nil, fmt.Errorf("stage applied defect: %w", err)
	}
	diff, err := corpusGit(ctx, root, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("describe applied defect: %w", err)
	}
	hunks, err := ParseUnifiedDiff(diff)
	if err != nil {
		return nil, fmt.Errorf("parse applied defect: %w", err)
	}
	return hunks, nil
}

// injectedPatchFiles groups hunks by file in first-seen order and collects the
// changed body lines the leak filter runs against.
func injectedPatchFiles(hunks []DiffHunk) ([]*injectedFile, int, []string) {
	files := make([]*injectedFile, 0, 4)
	byPath := map[string]*injectedFile{}
	changedLines := 0
	changedText := make([]string, 0, 64)
	for _, hunk := range hunks {
		file, tracked := byPath[hunk.Path]
		if !tracked {
			file = &injectedFile{path: hunk.Path, test: isCorpusTestPath(hunk.Path)}
			byPath[hunk.Path] = file
			files = append(files, file)
		}
		file.regions = append(file.regions, hunk.RegionAfter())
		file.changed += len(hunk.Lines)
		changedLines += len(hunk.Lines)
		changedText = append(changedText, hunk.Lines...)
	}
	return files, changedLines, changedText
}

// injectedGroundTruth splits the patched files into the core (non-test) and
// optional (test) halves and elects one main file, matching the mined builder
// so both corpora are scored by identical rules.
func injectedGroundTruth(files []*injectedFile) (GroundTruth, int) {
	truth := GroundTruth{
		ReadCoreFiles: []string{}, ReadCoreRegions: []Region{},
		ReadOptionalFilesMap: map[string][]string{}, ReadOptionalRegionsMap: map[string][]Region{},
		MainFiles: []string{},
	}
	mainPath, mainChanged := "", 0
	for _, file := range files {
		if len(file.regions) == 0 {
			continue
		}
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

// injectedLeak applies the mined corpus leak filter to an authored statement
// and returns the exemptions the literal stratum was granted.
//
// A literal instance exists precisely because its symptom quotes a distinctive
// user-visible string, so an identifier match inside a quoted run is expected
// evidence rather than a leak. Nothing else is exempt: a path or a line
// reference stays fatal in every stratum, and an identifier appearing outside
// quotation marks is an ordinary leak even here.
func injectedLeak(instanceID, statement, stratum string, paths []string, identifiers map[string]struct{}) (string, string, []InjectedExemption) {
	remaining := make(map[string]struct{}, len(identifiers))
	for identifier := range identifiers {
		remaining[identifier] = struct{}{}
	}
	quotes := quotedRuns(statement)
	exemptions := make([]InjectedExemption, 0, 2)
	for {
		reason, detail := StatementLeak(statement, paths, remaining)
		if reason == "" {
			return "", "", exemptions
		}
		if reason != CorpusRejectLeakIdentifier || stratum != StratumLiteral {
			return reason, detail, exemptions
		}
		identifier, match := splitLeakDetail(detail)
		if _, present := remaining[identifier]; !present {
			return reason, detail, exemptions
		}
		quote, quoted := quoteContaining(quotes, match)
		if !quoted {
			return reason, detail, exemptions
		}
		exemptions = append(exemptions, InjectedExemption{InstanceID: instanceID, Identifier: identifier, Match: match, Quote: quote})
		delete(remaining, identifier)
	}
}

// splitLeakDetail separates the leaked identifier from the normalized needle
// that matched, which StatementLeak appends in parentheses when the leak was a
// run of the identifier's own words rather than the whole identifier.
func splitLeakDetail(detail string) (string, string) {
	if open := strings.Index(detail, " ("); open > 0 && strings.HasSuffix(detail, ")") {
		return detail[:open], detail[open+2 : len(detail)-1]
	}
	return detail, normalizeLeakText(detail)
}

func quoteContaining(quotes []string, needle string) (string, bool) {
	if needle == "" {
		return "", false
	}
	for _, quote := range quotes {
		if strings.Contains(normalizeLeakText(quote), needle) {
			return quote, true
		}
	}
	return "", false
}

// quotedRuns returns the quoted literals a symptom description contains. A
// single quote counts as a delimiter only when it does not sit inside a word,
// so the apostrophe in "doesn't" cannot turn half a sentence into a quotation
// and launder a real leak through the literal exemption.
func quotedRuns(statement string) []string {
	runs := make([]string, 0, 4)
	for _, match := range injectedDoubleQuoted.FindAllStringSubmatch(statement, -1) {
		runs = append(runs, match[1])
	}
	symbols := []rune(statement)
	for index := 0; index < len(symbols); index++ {
		if symbols[index] != '\'' || (index > 0 && isWordSymbol(symbols[index-1])) {
			continue
		}
		for end := index + 1; end < len(symbols); end++ {
			if symbols[end] == '\n' {
				break
			}
			if symbols[end] != '\'' || (end+1 < len(symbols) && isWordSymbol(symbols[end+1])) {
				continue
			}
			runs = append(runs, string(symbols[index+1:end]))
			index = end
			break
		}
	}
	return runs
}

func isWordSymbol(symbol rune) bool {
	return unicode.IsLetter(symbol) || unicode.IsDigit(symbol)
}

// injectedPatchPath resolves a defect record's patch reference against the
// defects file's own directory and refuses one that climbs out of it.
func injectedPatchPath(defectsDirectory, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("defect_patch is required")
	}
	native := filepath.FromSlash(trimmed)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" || strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("defect_patch %q must be relative to the defects file", value)
	}
	resolved := filepath.Clean(filepath.Join(defectsDirectory, native))
	relative, err := filepath.Rel(defectsDirectory, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("defect_patch %q escapes the defects file directory", value)
	}
	return resolved, nil
}

// injectedRelativePatch rewrites the patch reference to be relative to the
// emitted issues file, which is how every consumer resolves it. A patch the
// issues file cannot reach without climbing out of its own directory is
// refused here rather than at campaign time.
func injectedRelativePatch(issuesDirectory, patchPath string) (string, error) {
	relative, err := filepath.Rel(issuesDirectory, patchPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("defect patch %s is outside the issues file directory %s; write the corpus beside its patches", patchPath, issuesDirectory)
	}
	return filepath.ToSlash(relative), nil
}

// patchedLineCount counts the lines of one file in the patched tree. A file the
// patch deleted has no lines, which clamps its regions away.
func patchedLineCount(path string) (int, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read patched file: %w", err)
	}
	if len(body) == 0 {
		return 0, nil
	}
	count := bytes.Count(body, []byte{'\n'})
	if body[len(body)-1] != '\n' {
		count++
	}
	return count, nil
}

// LoadDefectRecords reads the authored defect file. Unknown fields are ignored
// so the authoring workflow can record more than the harness consumes.
func LoadDefectRecords(path string) ([]DefectRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read defect records: %w", err)
	}
	defer file.Close()
	records := make([]DefectRecord, 0, 32)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record DefectRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode %s line %d: %w", path, line, err)
		}
		if strings.TrimSpace(record.InstanceID) == "" {
			return nil, fmt.Errorf("%s line %d has no instance id", path, line)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return records, nil
}

// defectWorktrees applies authored patches somewhere other than the author's
// own checkout. The corpus build must prove every patch applies to its pinned
// commit, and doing that in the source repository would leave it dirty and
// destroy whatever the author had in progress there.
type defectWorktrees struct {
	repository string
	temporary  string
	roots      map[string]string
}

func newDefectWorktrees(repository string) (*defectWorktrees, error) {
	temporary, err := os.MkdirTemp("", "lctk-defect-corpus-")
	if err != nil {
		return nil, fmt.Errorf("create defect verification directory: %w", err)
	}
	return &defectWorktrees{repository: repository, temporary: temporary, roots: map[string]string{}}, nil
}

// at returns a checkout of one base commit that the caller may modify freely.
// Checkouts are reused across defects that share a commit and are reset before
// each reuse, so verifying twenty defects does not mean twenty full checkouts.
func (worktrees *defectWorktrees) at(ctx context.Context, baseCommit string) (string, error) {
	if root, cached := worktrees.roots[baseCommit]; cached {
		if _, err := corpusGitRaw(ctx, root, "reset", "--hard", baseCommit); err != nil {
			return "", fmt.Errorf("reset verification worktree: %w", err)
		}
		if _, err := corpusGitRaw(ctx, root, "clean", "-fdq"); err != nil {
			return "", fmt.Errorf("clean verification worktree: %w", err)
		}
		return root, nil
	}
	root := filepath.Join(worktrees.temporary, fmt.Sprintf("base-%d", len(worktrees.roots)))
	if _, err := corpusGitRaw(ctx, worktrees.repository, "worktree", "add", "--detach", "--quiet", root, baseCommit); err != nil {
		return "", fmt.Errorf("check out base commit %s for verification: %w", baseCommit, err)
	}
	worktrees.roots[baseCommit] = root
	return root, nil
}

// Close removes every verification checkout and the administrative records the
// source repository keeps for them, so a corpus build leaves no trace.
func (worktrees *defectWorktrees) Close(ctx context.Context) {
	for _, root := range worktrees.roots {
		_, _ = corpusGitRaw(ctx, worktrees.repository, "worktree", "remove", "--force", root)
	}
	_ = os.RemoveAll(worktrees.temporary)
	_, _ = corpusGitRaw(ctx, worktrees.repository, "worktree", "prune")
}
