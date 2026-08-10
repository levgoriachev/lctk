package sweexplore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectedCorpusDerivesGroundTruthFromTheBSide(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 20))
	repo.write("app/small.go", numberedLines("small", 8))
	repo.write("tests/service_check.go", numberedLines("check", 6))
	base := repo.commit("seed the service", "")

	author := newDefectAuthor(t, repo)
	patch := author.capture("service.patch", func() {
		// A single-line modification and a pure insertion resolve to opposite
		// b-side shapes, which is exactly what a naive parser collapses.
		repo.replaceLine("app/service.go", 6, "step six adjusted")
		repo.insertAfter("app/service.go", 12, []string{"step twelve extra", "step twelve more"})
		repo.replaceLine("app/small.go", 3, "small three adjusted")
		repo.replaceLine("tests/service_check.go", 2, "check two adjusted")
	})
	author.record(DefectRecord{
		InstanceID: "flexus-inj__generic-01", Repository: "flexus", BaseCommit: base,
		ProblemStatement: "Weekly totals come out short for accounts created in the middle of a period.",
		DefectPatch:      patch, Stratum: StratumGeneric, CausalNote: "the running total skips one row",
	})

	result := author.build()
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	truth := result.Candidates[0].Explore.GroundTruth
	assertRegions(t, "core", truth.ReadCoreRegions, []Region{
		{Path: "app/service.go", Start: 6, End: 6},
		{Path: "app/service.go", Start: 13, End: 14},
		{Path: "app/small.go", Start: 3, End: 3},
	})
	if len(truth.ReadCoreFiles) != 2 || truth.ReadCoreFiles[0] != "app/service.go" || truth.ReadCoreFiles[1] != "app/small.go" {
		t.Fatalf("core files = %v", truth.ReadCoreFiles)
	}
	// The test file is optional evidence, never a core localization target.
	assertRegions(t, "optional", truth.ReadOptionalRegionsMap["tests/service_check.go"], []Region{{Path: "tests/service_check.go", Start: 2, End: 2}})
	if len(truth.ReadOptionalFilesMap) != 1 {
		t.Fatalf("optional files = %v", truth.ReadOptionalFilesMap)
	}
	if len(truth.MainFiles) != 1 || truth.MainFiles[0] != "app/service.go" {
		t.Fatalf("main files = %v, want the largest non-test file", truth.MainFiles)
	}
	issue := result.Candidates[0].Issue
	if issue.Stratum != StratumGeneric || issue.DefectPatch != "service.patch" || issue.BaseCommit != base {
		t.Fatalf("issue row = %+v", issue)
	}
	instance := result.Report.Instances[0]
	if instance.FileCount != 3 || instance.ChangedLines != 8 || instance.CoreRegions != 3 || instance.OptionalRegions != 1 {
		t.Fatalf("instance report = %+v", instance)
	}
	if totals := result.Report.StratumTotals[StratumGeneric]; totals.Instances != 1 || totals.ChangedLines != 8 {
		t.Fatalf("stratum totals = %+v", result.Report.StratumTotals)
	}
}

func TestInjectedCorpusRefusesAPatchThatDoesNotApply(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 12))
	repo.commit("seed the service", "")

	author := newDefectAuthor(t, repo)
	patch := author.capture("stale.patch", func() {
		repo.replaceLine("app/service.go", 4, "step four adjusted")
	})
	// The pinned commit moves under the patch, which is the exact way a corpus
	// silently stops describing the tree it claims to.
	repo.replaceLine("app/service.go", 4, "step four rewritten upstream")
	moved := repo.commit("rewrite the same line", "")
	author.record(DefectRecord{
		InstanceID: "flexus-inj__stale-01", Repository: "flexus", BaseCommit: moved,
		ProblemStatement: "Reports stop half way through the month for accounts on the annual plan.",
		DefectPatch:      patch, Stratum: StratumGeneric,
	})

	_, err := author.buildError()
	if err == nil || !strings.Contains(err.Error(), "flexus-inj__stale-01") || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("stale patch error = %v", err)
	}
}

func TestInjectedCorpusRefusesOversizedPatches(t *testing.T) {
	repo := newCorpusRepo(t)
	for _, name := range []string{"one", "two", "three", "four"} {
		repo.write("app/"+name+".go", numberedLines(name, 60))
	}
	base := repo.commit("seed four modules", "")

	files := newDefectAuthor(t, repo)
	wide := files.capture("wide.patch", func() {
		for _, name := range []string{"one", "two", "three", "four"} {
			repo.replaceLine("app/"+name+".go", 2, name+" two adjusted")
		}
	})
	files.record(DefectRecord{
		InstanceID: "flexus-inj__wide-01", Repository: "flexus", BaseCommit: base,
		ProblemStatement: "Every export finishes with the wrong number of rows since the last release.",
		DefectPatch:      wide, Stratum: StratumFamily,
	})
	if _, err := files.buildError(); err == nil || !strings.Contains(err.Error(), "touches 4 files") {
		t.Fatalf("four-file patch error = %v", err)
	}

	lines := newDefectAuthor(t, repo)
	long := lines.capture("long.patch", func() {
		for line := 1; line <= 30; line++ {
			repo.replaceLine("app/one.go", line, fmt.Sprintf("one %d adjusted", line))
		}
	})
	lines.record(DefectRecord{
		InstanceID: "flexus-inj__long-01", Repository: "flexus", BaseCommit: base,
		ProblemStatement: "Every export finishes with the wrong number of rows since the last release.",
		DefectPatch:      long, Stratum: StratumFamily,
	})
	if _, err := lines.buildError(); err == nil || !strings.Contains(err.Error(), "changes 60 lines") {
		t.Fatalf("sixty-line patch error = %v", err)
	}
}

func TestInjectedCorpusLeakFilterExemptsOnlyQuotedLiterals(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/billing.go", "package app\n\nfunc settle() error {\n\treturn newFailure(\"payment_accepted\")\n}\n")
	base := repo.commit("seed the settlement path", "")

	defect := func(statement, stratum string) error {
		author := newDefectAuthor(t, repo)
		patch := author.capture("billing.patch", func() {
			repo.replaceLine("app/billing.go", 4, "\treturn newFailure(\"payment_declined\")")
		})
		author.record(DefectRecord{
			InstanceID: "flexus-inj__literal-01", Repository: "flexus", BaseCommit: base,
			ProblemStatement: statement, DefectPatch: patch, Stratum: stratum,
		})
		_, err := author.buildError()
		return err
	}

	quoted := `Customers see the message "payment_declined" at the end of a purchase even when the card is charged.`
	if err := defect(quoted, StratumGeneric); err == nil || !strings.Contains(err.Error(), CorpusRejectLeakIdentifier) {
		t.Fatalf("a generic instance must not be exempt from the identifier filter: %v", err)
	}
	// The same statement is expected evidence for a literal instance, whose
	// whole premise is that the symptom quotes a distinctive string.
	author := newDefectAuthor(t, repo)
	patch := author.capture("billing.patch", func() {
		repo.replaceLine("app/billing.go", 4, "\treturn newFailure(\"payment_declined\")")
	})
	author.record(DefectRecord{
		InstanceID: "flexus-inj__literal-01", Repository: "flexus", BaseCommit: base,
		ProblemStatement: quoted, DefectPatch: patch, Stratum: StratumLiteral,
	})
	result := author.build()
	if len(result.Report.Exemptions) != 1 {
		t.Fatalf("exemptions = %+v", result.Report.Exemptions)
	}
	exemption := result.Report.Exemptions[0]
	if exemption.InstanceID != "flexus-inj__literal-01" || exemption.Identifier != "payment_declined" || exemption.Quote != "payment_declined" {
		t.Fatalf("exemption = %+v", exemption)
	}
	if totals := result.Report.StratumTotals[StratumLiteral]; totals.Exemptions != 1 {
		t.Fatalf("stratum totals lost the exemption: %+v", totals)
	}

	// A path leak is never exempt: naming the file removes the search entirely,
	// whatever the stratum claims.
	if err := defect(`The billing page keeps showing "payment_declined" after a successful charge.`, StratumLiteral); err == nil || !strings.Contains(err.Error(), CorpusRejectLeakPath) {
		t.Fatalf("literal path leak error = %v", err)
	}
	if err := defect(`Purchases fail at app/billing.go:4 with a declined message.`, StratumLiteral); err == nil || !strings.Contains(err.Error(), CorpusRejectLeakLineReference) {
		t.Fatalf("literal line-reference leak error = %v", err)
	}
	// An identifier that is not inside quotation marks stays an ordinary leak.
	if err := defect(`Customers see payment_declined at the end of a purchase even when the card is charged.`, StratumLiteral); err == nil || !strings.Contains(err.Error(), CorpusRejectLeakIdentifier) {
		t.Fatalf("unquoted literal leak error = %v", err)
	}
}

func TestInjectedCorpusRefusesIncompleteAuthoring(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 12))
	base := repo.commit("seed the service", "")
	valid := newDefectAuthor(t, repo).capture("service.patch", func() {
		repo.replaceLine("app/service.go", 4, "step four adjusted")
	})
	statement := "Reports stop half way through the month for accounts on the annual plan."
	for name, mutate := range map[string]func(*DefectRecord){
		"short statement":    func(record *DefectRecord) { record.ProblemStatement = "broken" },
		"blank statement":    func(record *DefectRecord) { record.ProblemStatement = "   " },
		"unknown stratum":    func(record *DefectRecord) { record.Stratum = "performance" },
		"missing stratum":    func(record *DefectRecord) { record.Stratum = "" },
		"abbreviated commit": func(record *DefectRecord) { record.BaseCommit = base[:12] },
		"foreign repository": func(record *DefectRecord) { record.Repository = "refact" },
		"escaping patch":     func(record *DefectRecord) { record.DefectPatch = "../outside.patch" },
	} {
		author := newDefectAuthor(t, repo)
		record := DefectRecord{
			InstanceID: "flexus-inj__generic-01", Repository: "flexus", BaseCommit: base,
			ProblemStatement: statement, DefectPatch: valid, Stratum: StratumGeneric,
		}
		mutate(&record)
		author.record(record)
		if _, err := author.buildError(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestInjectedInstanceRoundTripsThroughLoadInstanceAndScore(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 20))
	base := repo.commit("seed the service", "")
	author := newDefectAuthor(t, repo)
	patch := author.capture("service.patch", func() {
		repo.replaceLine("app/service.go", 9, "step nine adjusted")
		repo.insertAfter("app/service.go", 15, []string{"step fifteen extra"})
	})
	author.record(DefectRecord{
		InstanceID: "flexus-inj__cross-file-03", Repository: "flexus", BaseCommit: base,
		ProblemStatement: "Confirmation notices stop going out for orders placed at the end of a period.",
		DefectPatch:      patch, Stratum: StratumCrossFile,
	})
	result := author.build()

	explorePath := filepath.Join(author.directory, "explore.jsonl")
	exploreDigest, issuesDigest, err := WriteCorpusJSONL(explorePath, author.issuesPath, result.Candidates)
	if err != nil {
		t.Fatal(err)
	}
	benchmark := BenchmarkConfig{
		ExploreDatasetPath: explorePath, ExploreDatasetSHA256: exploreDigest,
		IssueDatasetPath: author.issuesPath, IssueDatasetSHA256: issuesDigest,
		SourceOverrides: map[string]string{"flexus": repo.root},
	}
	instance, err := LoadInstance(benchmark, "flexus-inj__cross-file-03")
	if err != nil {
		t.Fatal(err)
	}
	if !instance.Injected() || instance.Stratum != StratumCrossFile {
		t.Fatalf("loaded instance = %+v", instance)
	}
	if instance.DefectPatch != filepath.Join(author.directory, "service.patch") {
		t.Fatalf("resolved patch = %q", instance.DefectPatch)
	}
	workspace := t.TempDir()
	runCorpusGit(t, workspace, "init")
	if err := Materialize(context.Background(), workspace, instance.Repository, instance.BaseCommit, benchmark.SourceOverrides); err != nil {
		t.Fatal(err)
	}
	digest, err := ApplyDefect(context.Background(), workspace, instance.DefectPatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDefectState(context.Background(), workspace, instance.BaseCommit, digest); err != nil {
		t.Fatal(err)
	}
	// Ground truth is scored against the patched tree the agent actually sees.
	metrics, err := Score(workspace, instance.GroundTruth.ReadCoreRegions, instance.GroundTruth)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Recall != 1 || metrics.Precision != 1 || metrics.WeightedCoreCoverage != 1 {
		t.Fatalf("scoring the ground truth against itself = %+v", metrics)
	}
}

func TestQuotedRunsIgnoreApostrophes(t *testing.T) {
	runs := quotedRuns(`It doesn't send the user's copy; the banner says 'order_failed' and "retry_later" instead.`)
	want := map[string]bool{"order_failed": true, "retry_later": true}
	if len(runs) != 2 {
		t.Fatalf("quoted runs = %q", runs)
	}
	for _, run := range runs {
		if !want[run] {
			t.Fatalf("quoted runs = %q, want only %v", runs, want)
		}
	}
}

// defectAuthor writes a defect file and its patches the way a human author
// would, so the builder is exercised against real Git output rather than
// hand-written diff text.
type defectAuthor struct {
	t           *testing.T
	repo        *corpusRepo
	directory   string
	defectsFile string
	issuesPath  string
}

func newDefectAuthor(t *testing.T, repo *corpusRepo) *defectAuthor {
	t.Helper()
	directory := t.TempDir()
	return &defectAuthor{
		t: t, repo: repo, directory: directory,
		defectsFile: filepath.Join(directory, "defects.jsonl"),
		issuesPath:  filepath.Join(directory, "issues.jsonl"),
	}
}

// capture turns a working-tree edit into a patch file and restores the tree.
func (author *defectAuthor) capture(name string, edit func()) string {
	author.t.Helper()
	edit()
	runCorpusGit(author.t, author.repo.root, "add", "--all", "--intent-to-add")
	diff := captureGit(author.t, author.repo.root, "diff")
	runCorpusGit(author.t, author.repo.root, "reset", "--quiet")
	runCorpusGit(author.t, author.repo.root, "checkout", "--", ".")
	runCorpusGit(author.t, author.repo.root, "clean", "-fdq")
	if err := os.WriteFile(filepath.Join(author.directory, name), []byte(diff), 0o600); err != nil {
		author.t.Fatal(err)
	}
	return name
}

func (author *defectAuthor) record(record DefectRecord) {
	author.t.Helper()
	body, err := json.Marshal(record)
	if err != nil {
		author.t.Fatal(err)
	}
	file, err := os.OpenFile(author.defectsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		author.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(body, '\n')); err != nil {
		author.t.Fatal(err)
	}
}

func (author *defectAuthor) buildError() (InjectedResult, error) {
	author.t.Helper()
	return BuildInjectedCorpus(context.Background(), InjectedRequest{
		DefectsPath: author.defectsFile, RepositoryPath: author.repo.root,
		RepositoryName: "flexus", IssuesPath: author.issuesPath,
	})
}

func (author *defectAuthor) build() InjectedResult {
	author.t.Helper()
	result, err := author.buildError()
	if err != nil {
		author.t.Fatal(err)
	}
	return result
}

// captureGit keeps Git output byte for byte, because a patch file that lost its
// trailing newline no longer applies.
func captureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "core.autocrlf=false", "-c", "core.eol=lf"}, args...)
	command := exec.Command("git", full...)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatal(fmt.Errorf("git %v: %w", args, err))
	}
	return string(output)
}
