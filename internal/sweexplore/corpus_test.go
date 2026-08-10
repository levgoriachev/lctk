package sweexplore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// realHunkFixture reproduces the exact `--unified=0` headers of a measured
// Flexus fix. It carries all three header forms at once: an omitted count that
// means one, an explicit ",0" that means a pure insertion, and an explicit
// multi-line count. Confusing the first two silently relocates half of every
// ground truth, so this stays a literal fixture rather than a generated one.
const realHunkFixture = `diff --git a/flexus_frontend/src/features/externalAuth/metaEmbeddedSignup.ts b/flexus_frontend/src/features/externalAuth/metaEmbeddedSignup.ts
index 1111111..2222222 100644
--- a/flexus_frontend/src/features/externalAuth/metaEmbeddedSignup.ts
+++ b/flexus_frontend/src/features/externalAuth/metaEmbeddedSignup.ts
@@ -3 +3 @@
-const metaProduct = 'whatsapp';
+const metaProduct = resolveProduct();
@@ -20,0 +21 @@
+  bootstrapSdk(metaProduct);
@@ -22 +23 @@
-  return cachedSdk;
+  return cachedSdk[metaProduct];
@@ -26,4 +27 @@
-  alpha
-  bravo
-  charlie
-  delta
+  merged
@@ -41,0 +40,7 @@
+  one
+  two
+  three
+  four
+  five
+  six
+  seven
@@ -73 +78 @@
-  previous
+  replacement
diff --git a/flexus_frontend/src/features/externalAuth/__tests__/metaEmbeddedSignup.test.ts b/flexus_frontend/src/features/externalAuth/__tests__/metaEmbeddedSignup.test.ts
index 3333333..4444444 100644
--- a/flexus_frontend/src/features/externalAuth/__tests__/metaEmbeddedSignup.test.ts
+++ b/flexus_frontend/src/features/externalAuth/__tests__/metaEmbeddedSignup.test.ts
@@ -12,0 +13,3 @@
+  it('initializes once per product', () => {});
+  it('does not reinitialize', () => {});
+  it('reuses the cached handle', () => {});
`

const realCorePath = "flexus_frontend/src/features/externalAuth/metaEmbeddedSignup.ts"
const realTestPath = "flexus_frontend/src/features/externalAuth/__tests__/metaEmbeddedSignup.test.ts"

func TestParseCorpusDiffProducesExactPreFixRegions(t *testing.T) {
	core := &corpusFile{path: realCorePath}
	tests := &corpusFile{path: realTestPath}
	identifiers, err := parseCorpusDiff(realHunkFixture, "f8bb52b72abc", []*corpusFile{core, tests})
	if err != nil {
		t.Fatal(err)
	}
	assertRegions(t, "core", mergeCorpusRegions(core.regions), []Region{
		{Path: realCorePath, Start: 3, End: 3},
		{Path: realCorePath, Start: 20, End: 22},
		{Path: realCorePath, Start: 26, End: 29},
		{Path: realCorePath, Start: 41, End: 41},
		{Path: realCorePath, Start: 73, End: 73},
	})
	assertRegions(t, "test", mergeCorpusRegions(tests.regions), []Region{
		{Path: realTestPath, Start: 12, End: 12},
	})
	if _, ok := identifiers["metaProduct"]; !ok {
		t.Fatalf("changed-line identifiers = %v, want metaProduct", identifiers)
	}
	// The fix's own subject names the changed identifier, so the instance it
	// would produce must not reach an agent.
	truth, _ := corpusGroundTruth([]*corpusFile{
		{path: realCorePath, changed: core.changed, regions: mergeCorpusRegions(core.regions)},
		{path: realTestPath, test: true, changed: tests.changed, regions: mergeCorpusRegions(tests.regions)},
	})
	if len(truth.ReadCoreFiles) != 1 || truth.ReadCoreFiles[0] != realCorePath {
		t.Fatalf("core files = %v, want only %s", truth.ReadCoreFiles, realCorePath)
	}
	if len(truth.MainFiles) != 1 || truth.MainFiles[0] != realCorePath {
		t.Fatalf("main files = %v, want only %s", truth.MainFiles, realCorePath)
	}
	if _, optional := truth.ReadOptionalRegionsMap[realTestPath]; !optional {
		t.Fatalf("optional regions = %v, want the test file", truth.ReadOptionalRegionsMap)
	}
	reason, detail := corpusLeakReason("init the Facebook SDK per Meta product, not once per page", truth, identifiers)
	if reason != CorpusRejectLeakIdentifier || detail != "metaProduct" {
		t.Fatalf("leak reason = %q %q, want %q metaProduct", reason, detail, CorpusRejectLeakIdentifier)
	}
}

func TestCorpusLeakFilterNormalizesSeparators(t *testing.T) {
	truth := GroundTruth{ReadCoreFiles: []string{"flexus_backend/flexus_v1/utils_socket.py"}}
	identifiers := map[string]struct{}{"SESSION_DEAD_KEYWORDS": {}, "dig_out_sesh": {}}
	// A decoded tracker slug turns underscores into spaces, so the identifier
	// survives as ordinary-looking words that a plain substring test misses.
	slugged := "Dead session hangs on Loading workspaces Not sure we met before missing from SESSION DEAD KEYWORDS"
	if reason, detail := corpusLeakReason(slugged, truth, identifiers); reason != CorpusRejectLeakIdentifier || detail != "SESSION_DEAD_KEYWORDS" {
		t.Fatalf("slug leak = %q %q, want %q SESSION_DEAD_KEYWORDS", reason, detail, CorpusRejectLeakIdentifier)
	}
	if reason, _ := corpusLeakReason("the utils socket py helper rejects a live session", truth, nil); reason != CorpusRejectLeakPath {
		t.Fatalf("path leak = %q, want %q", reason, CorpusRejectLeakPath)
	}
	if reason, _ := corpusLeakReason("stop reading rows back through pgpool's replica on workspace_create", GroundTruth{}, map[string]struct{}{"workspace_create": {}}); reason != CorpusRejectLeakIdentifier {
		t.Fatalf("identifier leak = %q, want %q", reason, CorpusRejectLeakIdentifier)
	}
	// The line-number check runs against the statement as written, because
	// normalization would erase the colon it looks for.
	if reason, _ := corpusLeakReason("the socket helper rejects a live session at line 206:206", GroundTruth{}, nil); reason != CorpusRejectLeakLineReference {
		t.Fatalf("line reference = %q, want %q", reason, CorpusRejectLeakLineReference)
	}
	clean := "stop background tabs 403-ing forever, and redirect a genuinely dead session"
	if reason, detail := corpusLeakReason(clean, truth, map[string]struct{}{"dig_out_sesh": {}, "renewSessionIfNeeded": {}}); reason != "" {
		t.Fatalf("clean symptom statement was dropped: %q %q", reason, detail)
	}
}

func TestStatementLeakCatchesIdentifierWordRuns(t *testing.T) {
	// Measured on the real corpus: the statement said "voice mode" while the
	// changed line declared `is_voice_mode`, so the whole-identifier substring
	// test passed and the instance would have shipped with the answer in it.
	statement := "Simplify user message addition logic by removing unnecessary condition for voice mode in bot activation."
	identifiers := map[string]struct{}{"is_voice_mode": {}, "first_question": {}}
	if reason, detail := StatementLeak(statement, []string{"flexus_backend/flexus_v1/v1_bot_activate.py"}, identifiers); reason != CorpusRejectLeakIdentifier {
		t.Fatalf("word-run leak = %q %q, want %q", reason, detail, CorpusRejectLeakIdentifier)
	}
	// Naming one word of an identifier is not a leak; only a contiguous run of
	// its words is, and only when the run is long enough that a reporter could
	// not have written it by accident.
	for _, safe := range []struct {
		statement  string
		identifier string
	}{
		{"the recording drops in voice calls after a retry", "is_voice_mode"},
		{"totals drift after a retry on a slow connection", "retryBudget"},
	} {
		if reason, detail := StatementLeak(safe.statement, nil, map[string]struct{}{safe.identifier: {}}); reason != "" {
			t.Errorf("statement %q was dropped against %q: %s %s", safe.statement, safe.identifier, reason, detail)
		}
	}
}

func TestParseUnifiedDiffExposesBothSides(t *testing.T) {
	hunks, err := ParseUnifiedDiff(realHunkFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 7 {
		t.Fatalf("parsed %d hunks, want 7", len(hunks))
	}
	// An omitted count means one; an explicit ",0" means a pure insertion. The
	// two sides therefore disagree, which is exactly what the injection corpus
	// needs: it localizes against the patched tree.
	if got, want := hunks[0].RegionBefore(), (Region{Path: realCorePath, Start: 3, End: 3}); got != want {
		t.Fatalf("first hunk before = %+v, want %+v", got, want)
	}
	if got, want := hunks[1].RegionBefore(), (Region{Path: realCorePath, Start: 20, End: 20}); got != want {
		t.Fatalf("insertion before = %+v, want the a-side anchor %+v", got, want)
	}
	if got, want := hunks[1].RegionAfter(), (Region{Path: realCorePath, Start: 21, End: 21}); got != want {
		t.Fatalf("insertion after = %+v, want %+v", got, want)
	}
	if got, want := hunks[4].RegionAfter(), (Region{Path: realCorePath, Start: 40, End: 46}); got != want {
		t.Fatalf("multi-line insertion after = %+v, want %+v", got, want)
	}
	if got, want := hunks[3].RegionAfter(), (Region{Path: realCorePath, Start: 27, End: 27}); got != want {
		t.Fatalf("collapsing hunk after = %+v, want %+v", got, want)
	}
	if _, ok := CodeShapedIdentifiers(hunks[0].Lines)["metaProduct"]; !ok {
		t.Fatalf("hunk lines = %v, want a code-shaped identifier", hunks[0].Lines)
	}
}

func TestCorpusDecodesTrackerSlug(t *testing.T) {
	raw := "https://refact.fibery.io/Software_Development/Task/Dead-session-hangs-on-Loading-workspaces-Not-sure-we-met-before-missing-from-SESSION_DEAD_KEYWORDS-2708"
	want := "Dead session hangs on Loading workspaces Not sure we met before missing from SESSION DEAD KEYWORDS"
	if got := corpusDecodeTrackerSlug(raw); got != want {
		t.Fatalf("decoded slug = %q, want %q", got, want)
	}
	for _, ignored := range []string{
		"https://github.com/org/repo/pull/2156",
		"https://example.com/docs/guide.html",
		"https://example.com/one-two",
	} {
		if got := corpusDecodeTrackerSlug(ignored); got != "" {
			t.Fatalf("URL %q decoded to %q, want no statement", ignored, got)
		}
	}
}

func TestCorpusStatementPrefersTicketsThenSlugThenSubject(t *testing.T) {
	tickets := CorpusTicketSet{
		ByRef: map[string]CorpusTicket{
			"2707": {Ref: "2707", Title: "Session never renews in a background tab 403 storm until the user clicks the tab", SourcePull: 2161},
			"2580": {Ref: "2580", Title: "Bookings vanish from the public wizard after a refresh", SourcePull: 2153},
			"2628": {Ref: "2628", Title: "Branch selection resets when the form reopens", SourcePull: 2153},
		},
		BySourcePull: map[int][]CorpusTicket{
			2161: {{Ref: "2707", Title: "Session never renews in a background tab 403 storm until the user clicks the tab", SourcePull: 2161}},
			2153: {
				{Ref: "2580", Title: "Bookings vanish from the public wizard after a refresh", SourcePull: 2153},
				{Ref: "2628", Title: "Branch selection resets when the form reopens", SourcePull: 2153},
			},
		},
		Count: 3,
	}
	// The trailing group is the squash-merged pull request, so it resolves
	// through source_pull and never through a ticket ref.
	statement, source := corpusStatement(corpusCommit{subject: "fix(auth): renew a background tab's quicksession once it has actually expired (#2707) (#2161)"}, tickets)
	if source != CorpusStatementTicketFile || statement != tickets.ByRef["2707"].Title {
		t.Fatalf("pull-matched statement = %q from %q", statement, source)
	}
	statement, source = corpusStatement(corpusCommit{subject: "fix: booking wizard loses state (#2153)"}, tickets)
	want := "Bookings vanish from the public wizard after a refresh; Branch selection resets when the form reopens"
	if source != CorpusStatementTicketFile || statement != want {
		t.Fatalf("multi-ticket statement = %q from %q, want %q", statement, source, want)
	}
	// A trailing number that is nobody's pull request must not be matched
	// against a ticket ref that happens to share it.
	statement, source = corpusStatement(corpusCommit{subject: "fix: something unrelated (#2580)"}, tickets)
	if source != CorpusStatementSubject || statement != "something unrelated" {
		t.Fatalf("unmatched trailing pull = %q from %q, want the stripped subject", statement, source)
	}
	statement, source = corpusStatement(corpusCommit{
		subject: "fix: a dead session never recovers (#2001)",
		body:    "See https://refact.fibery.io/Software_Development/Task/Loading-workspaces-never-finishes-for-a-returning-user-2708",
	}, CorpusTicketSet{})
	if source != CorpusStatementTrackerSlug || statement != "Loading workspaces never finishes for a returning user" {
		t.Fatalf("slug statement = %q from %q", statement, source)
	}
	statement, source = corpusStatement(corpusCommit{subject: "fix(api): totals drift (#2708, #2707) (#2156) after a retry"}, CorpusTicketSet{})
	if source != CorpusStatementSubject || statement != "totals drift after a retry" {
		t.Fatalf("stripped subject = %q from %q", statement, source)
	}
}

func TestLoadCorpusTicketsIgnoresUnknownFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "statements.jsonl")
	writeTestFile(t, path, strings.Join([]string{
		`{"ref":"2707","title":"Session never renews in a background tab","source_pull":2161,"source_title":"fix(auth): renew (#2707)"}`,
		`{"ref":"2580","title":"UI appointment wizard","source_pull":2153,"source_title":"Feature"}`,
		`{"ref":"2628","title":"Branch selection resets","source_pull":2153}`,
		"",
	}, "\n"))
	tickets, err := LoadCorpusTickets(path)
	if err != nil {
		t.Fatal(err)
	}
	if tickets.Count != 3 || len(tickets.BySourcePull[2153]) != 2 {
		t.Fatalf("tickets = %+v", tickets)
	}
	if tickets.BySourcePull[2153][0].Ref != "2580" {
		t.Fatalf("shared pull order = %+v, want stable ref order", tickets.BySourcePull[2153])
	}
	writeTestFile(t, filepath.Join(root, "duplicate.jsonl"), `{"ref":"1","title":"a"}`+"\n"+`{"ref":"1","title":"b"}`+"\n")
	if _, err := LoadCorpusTickets(filepath.Join(root, "duplicate.jsonl")); err == nil {
		t.Fatal("duplicate ticket ref was accepted")
	}
}

func TestBuildCorpusSelectsAndExcludesRealFixShapes(t *testing.T) {
	repo := newCorpusRepo(t)
	// A modification-only fix: two separated single-line hunks.
	repo.write("app/service.go", numberedLines("step", 10))
	repo.write("app/notes.md", "documentation\n")
	repo.commit("seed the service", "")
	repo.replaceLine("app/service.go", 4, "step four adjusted")
	repo.replaceLine("app/service.go", 8, "step eight adjusted")
	repo.write("app/notes.md", "documentation revised\n")
	modify := repo.commit("fix: the fourth and eighth steps report the wrong total", "")

	// A pure insertion: the anchor is the pre-fix line the code was added after.
	repo.write("app/insert.go", numberedLines("row", 6))
	repo.commit("seed the inserter", "")
	repo.insertAfter("app/insert.go", 3, []string{"row three guard", "row three retry", "row three bail"})
	insert := repo.commit("fix: a retried request is silently counted twice", "")

	// A fix that also edits a test: the test must land in optional, and the
	// larger non-test file must be the only main file.
	repo.write("app/handler.go", numberedLines("handle", 12))
	repo.write("app/format.go", numberedLines("format", 12))
	repo.write("app/handler_test.go", numberedLines("assert", 30))
	repo.commit("seed the handler", "")
	for _, line := range []int{2, 3, 4} {
		repo.replaceLine("app/handler.go", line, "handle "+strconv.Itoa(line)+" adjusted")
	}
	repo.replaceLine("app/format.go", 5, "format five adjusted")
	for line := 1; line <= 12; line++ {
		repo.replaceLine("app/handler_test.go", line, "assert "+strconv.Itoa(line)+" adjusted")
	}
	withTest := repo.commit("fix: a second attempt is charged twice when the queue drains slowly", "")

	// Excluded shapes.
	repo.write("package-lock.json", "{\n  \"one\": 1,\n  \"two\": 2\n}\n")
	repo.write("assets/logo.png", "binary-ish\npayload\nhere\n")
	lockOnly := repo.commit("fix: refresh the pinned dependency tree", "")

	for index := 0; index < 9; index++ {
		repo.write(fmt.Sprintf("wide/module%d.go", index), numberedLines("wide", 4))
	}
	repo.commit("seed the wide modules", "")
	for index := 0; index < 9; index++ {
		repo.replaceLine(fmt.Sprintf("wide/module%d.go", index), 1, "wide one adjusted")
	}
	wide := repo.commit("fix: normalize the shared prelude everywhere", "")

	repo.write("deep/large.go", numberedLines("deep", 600))
	repo.commit("seed the large module", "")
	repo.write("deep/large.go", numberedLines("deeper", 600))
	large := repo.commit("fix: recompute every entry in the ledger", "")

	// A leaked statement: the subject names an identifier the fix changed.
	repo.write("app/leaky.go", numberedLines("plain", 8))
	repo.commit("seed the leaky module", "")
	repo.replaceLine("app/leaky.go", 2, "var workspaceCreate = true")
	repo.replaceLine("app/leaky.go", 3, "var workspaceCreateRetry = false")
	repo.replaceLine("app/leaky.go", 4, "plain four adjusted")
	leaky := repo.commit("fix: stop reading rows back through the replica on workspace create", "")

	revert := repo.revertLast("Revert \"fix: stop reading rows back through the replica on workspace create\"")
	merge := repo.mergeBranch("fix: reconcile the parallel branch")

	result, err := BuildCorpus(context.Background(), CorpusRequest{
		RepositoryPath: repo.root, RepositoryName: "Flexus Backend",
		Since: "2000-01-01", Count: 50, Seed: "corpus-seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, rejection := range result.Report.Rejections {
		reasons[rejection.Commit] = rejection.Reason
	}
	for commit, want := range map[string]string{
		lockOnly: CorpusRejectNoSourceFiles,
		wide:     CorpusRejectTooManySourceFiles,
		large:    CorpusRejectChangedLines,
		leaky:    CorpusRejectLeakIdentifier,
		revert:   CorpusRejectRevert,
		merge:    CorpusRejectMerge,
	} {
		if reasons[commit] != want {
			t.Errorf("commit %s rejected as %q, want %q", commit[:12], reasons[commit], want)
		}
	}
	byCommit := map[string]CorpusCandidate{}
	for _, candidate := range result.Candidates {
		byCommit[candidate.FixCommit] = candidate
	}
	if len(byCommit) != 3 {
		t.Fatalf("selected %d candidates, want the three usable fixes: %+v", len(byCommit), result.Report.Candidates)
	}

	modified := byCommit[modify].Explore.GroundTruth
	assertRegions(t, "modify", modified.ReadCoreRegions, []Region{
		{Path: "app/service.go", Start: 4, End: 4},
		{Path: "app/service.go", Start: 8, End: 8},
	})
	// A Markdown edit is neither fatal nor ground truth.
	if len(modified.ReadCoreFiles) != 1 || modified.ReadCoreFiles[0] != "app/service.go" {
		t.Fatalf("modify core files = %v", modified.ReadCoreFiles)
	}

	inserted := byCommit[insert].Explore.GroundTruth
	assertRegions(t, "insert", inserted.ReadCoreRegions, []Region{{Path: "app/insert.go", Start: 3, End: 3}})

	tested := byCommit[withTest].Explore.GroundTruth
	if len(tested.MainFiles) != 1 || tested.MainFiles[0] != "app/handler.go" {
		t.Fatalf("main files = %v, want only the largest non-test file", tested.MainFiles)
	}
	if len(tested.ReadCoreFiles) != 2 {
		t.Fatalf("core files = %v, want both non-test files", tested.ReadCoreFiles)
	}
	if _, core := stringSet(tested.ReadCoreFiles)["app/handler_test.go"]; core {
		t.Fatal("a test file was classified as core")
	}
	if regions, optional := tested.ReadOptionalRegionsMap["app/handler_test.go"]; !optional || len(regions) == 0 {
		t.Fatalf("optional regions = %v, want the test file", tested.ReadOptionalRegionsMap)
	}
	if files := tested.ReadOptionalFilesMap["app/handler_test.go"]; len(files) != 1 || files[0] != "app/handler_test.go" {
		t.Fatalf("optional files = %v", tested.ReadOptionalFilesMap)
	}
	if got := byCommit[modify].Issue.ProblemStatement; got != "the fourth and eighth steps report the wrong total" {
		t.Fatalf("problem statement = %q", got)
	}
	if got := byCommit[modify].InstanceID; got != "flexus-backend__"+modify[:12] {
		t.Fatalf("instance id = %q", got)
	}
	if got := byCommit[modify].Issue.BaseCommit; got != repo.parentOf(modify) {
		t.Fatalf("base commit = %q, want the fix's parent", got)
	}
	if result.Report.StatementSources[CorpusStatementSubject] != 3 {
		t.Fatalf("statement sources = %v", result.Report.StatementSources)
	}
}

func TestBuildCorpusUsesTicketStatementsAndStillDropsLeakyTitles(t *testing.T) {
	repo := newCorpusRepo(t)
	// The file is deliberately not named after the symptom: the path-stem rule
	// is strict enough that an `app/session.go` would itself be a path leak
	// against a statement that says "session".
	repo.write("app/quicksession.go", numberedLines("row", 12))
	repo.commit("seed the renewal module", "")
	repo.replaceLine("app/quicksession.go", 3, "row three adjusted")
	repo.replaceLine("app/quicksession.go", 4, "row four adjusted")
	repo.replaceLine("app/quicksession.go", 5, "row five adjusted")
	clean := repo.commit("fix(auth): renew a background tab's quicksession (#2707) (#2161)", "")

	repo.write("app/booking.go", numberedLines("entry", 12))
	repo.commit("seed the booking module", "")
	repo.replaceLine("app/booking.go", 3, "var renewSessionIfNeeded = true")
	repo.replaceLine("app/booking.go", 4, "entry four adjusted")
	repo.replaceLine("app/booking.go", 5, "entry five adjusted")
	leaky := repo.commit("fix(booking): repair the renewal path (#2801) (#2162)", "")

	tickets := CorpusTicketSet{
		ByRef: map[string]CorpusTicket{
			"2707": {Ref: "2707", Title: "Session never renews in a background tab 403 storm until the user clicks the tab", SourcePull: 2161},
			"2801": {Ref: "2801", Title: "renewSessionIfNeeded is never reached for a background tab", SourcePull: 2162},
		},
		BySourcePull: map[int][]CorpusTicket{
			2161: {{Ref: "2707", Title: "Session never renews in a background tab 403 storm until the user clicks the tab", SourcePull: 2161}},
			2162: {{Ref: "2801", Title: "renewSessionIfNeeded is never reached for a background tab", SourcePull: 2162}},
		},
		Count: 2,
	}
	result, err := BuildCorpus(context.Background(), CorpusRequest{
		RepositoryPath: repo.root, RepositoryName: "flexus",
		Since: "2000-01-01", Count: 10, Seed: "ticket-seed", Tickets: tickets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].FixCommit != clean {
		t.Fatalf("candidates = %+v, want only the non-leaking ticket", result.Report.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.StatementSource != CorpusStatementTicketFile || candidate.Issue.ProblemStatement != tickets.ByRef["2707"].Title {
		t.Fatalf("statement = %q from %q", candidate.Issue.ProblemStatement, candidate.StatementSource)
	}
	// A hand-written ticket title can still name a function; the leak filter
	// applies to the ticket source exactly as it does to a commit subject.
	for _, rejection := range result.Report.Rejections {
		if rejection.Commit == leaky && rejection.Reason == CorpusRejectLeakIdentifier {
			return
		}
	}
	t.Fatalf("leaky ticket title was not dropped: %+v", result.Report.Rejections)
}

func TestSelectCorpusCandidatesIsDeterministicAndSeedSensitive(t *testing.T) {
	candidates := make([]CorpusCandidate, 0, 40)
	for index := 0; index < 40; index++ {
		candidates = append(candidates, CorpusCandidate{InstanceID: fmt.Sprintf("repo__%012d", index)})
	}
	first := selectCorpusCandidates(candidates, 8, "seed-one")
	if len(first) != 8 {
		t.Fatalf("selected %d, want 8", len(first))
	}
	if !equalCandidateIDs(first, selectCorpusCandidates(candidates, 8, "seed-one")) {
		t.Fatal("the same seed produced a different selection")
	}
	if equalCandidateIDs(first, selectCorpusCandidates(candidates, 8, "seed-two")) {
		t.Fatal("a different seed produced an identical selection")
	}
	// Emission stays in commit order regardless of the sampling rank.
	for index := 1; index < len(first); index++ {
		if first[index-1].InstanceID >= first[index].InstanceID {
			t.Fatalf("selection is not in commit order: %v", candidateIDs(first))
		}
	}
	if all := selectCorpusCandidates(candidates, 100, "seed-one"); len(all) != len(candidates) {
		t.Fatalf("oversized request returned %d, want all %d without padding", len(all), len(candidates))
	}
}

func TestCorpusRoundTripsThroughLoadInstanceAndScore(t *testing.T) {
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 20))
	repo.commit("seed the service", "")
	repo.replaceLine("app/service.go", 6, "step six adjusted")
	repo.replaceLine("app/service.go", 7, "step seven adjusted")
	repo.replaceLine("app/service.go", 8, "step eight adjusted")
	repo.commit("fix: the middle of the run reports a stale total", "")

	result, err := BuildCorpus(context.Background(), CorpusRequest{
		RepositoryPath: repo.root, RepositoryName: "flexus", Since: "2000-01-01", Count: 1, Seed: "round-trip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %+v", result.Report.Candidates)
	}
	artifacts := t.TempDir()
	explorePath := filepath.Join(artifacts, "explore.jsonl")
	issuesPath := filepath.Join(artifacts, "issues.jsonl")
	exploreDigest, issuesDigest, err := WriteCorpusJSONL(explorePath, issuesPath, result.Candidates)
	if err != nil {
		t.Fatal(err)
	}
	benchmark := BenchmarkConfig{
		ExploreDatasetPath: explorePath, ExploreDatasetSHA256: exploreDigest,
		IssueDatasetPath: issuesPath, IssueDatasetSHA256: issuesDigest,
		SourceOverrides: map[string]string{"flexus": repo.root},
	}
	instance, err := LoadInstance(benchmark, result.Candidates[0].InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	// The workspace acquires the private base commit from the declared local
	// source, exactly as a campaign would.
	workspace := t.TempDir()
	runCorpusGit(t, workspace, "init")
	if err := Materialize(context.Background(), workspace, instance.Repository, instance.BaseCommit, benchmark.SourceOverrides); err != nil {
		t.Fatal(err)
	}
	if head := runCorpusGit(t, workspace, "rev-parse", "HEAD"); head != instance.BaseCommit {
		t.Fatalf("workspace HEAD = %s, want %s", head, instance.BaseCommit)
	}
	metrics, err := Score(workspace, instance.GroundTruth.ReadCoreRegions, instance.GroundTruth)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Recall != 1 || metrics.Precision != 1 || metrics.WeightedCoreCoverage != 1 {
		t.Fatalf("scoring the ground truth against itself = %+v", metrics)
	}
}

func TestMaterializeSourceOverridesAndRejections(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "private")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	runCorpusGit(t, source, "init")
	overrides := map[string]string{"flexus": source}
	if resolved, err := materializeSource("flexus", overrides); err != nil || resolved != source {
		t.Fatalf("local override = %q, %v", resolved, err)
	}
	// A repository without an override stays on the strict public path.
	if _, err := materializeSource("https://example.com/repo", overrides); err == nil {
		t.Fatal("a non-GitHub repository name was accepted without an override")
	}
	if resolved, err := materializeSource("pydata/xarray", overrides); err != nil || resolved != "https://github.com/pydata/xarray.git" {
		t.Fatalf("non-overridden repository = %q, %v", resolved, err)
	}
	plain := filepath.Join(root, "not-a-repository")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, override := range map[string]string{
		"missing":  filepath.Join(root, "absent"),
		"plain":    plain,
		"empty":    "   ",
		"flaglike": "--upload-pack=calc",
	} {
		if _, err := materializeSource(name, map[string]string{name: override}); err == nil {
			t.Fatalf("override %q = %q was accepted", name, override)
		}
	}
	for _, remote := range []string{"https://git.example.com/private.git", "ssh://git@example.com/private.git", "git@example.com:owner/private.git"} {
		if resolved, err := materializeSource("private", map[string]string{"private": remote}); err != nil || resolved != remote {
			t.Fatalf("remote override %q = %q, %v", remote, resolved, err)
		}
	}
}

func TestConfigResolvesSourceOverridePaths(t *testing.T) {
	base := t.TempDir()
	config := overrideConfig(map[string]string{
		"flexus": "repos/flexus", "remote": "https://git.example.com/private.git",
	})
	// The campaign still needs its four arms, so validation must fail; what this
	// pins is that a local override became a configuration-relative path while a
	// remote one was left exactly as written.
	if err := config.Validate(base); err == nil {
		t.Fatal("an armless campaign was accepted")
	}
	if got, want := config.Benchmark.SourceOverrides["flexus"], filepath.Join(base, "repos", "flexus"); got != want {
		t.Fatalf("local override resolved to %q, want %q", got, want)
	}
	if got := config.Benchmark.SourceOverrides["remote"]; got != "https://git.example.com/private.git" {
		t.Fatalf("remote override was rewritten to %q", got)
	}
	blank := overrideConfig(map[string]string{"flexus": "  "})
	if err := blank.Validate(base); err == nil || !strings.Contains(err.Error(), "source_overrides") {
		t.Fatalf("blank override error = %v", err)
	}
}

func overrideConfig(overrides map[string]string) Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Benchmark: BenchmarkConfig{
			ExploreDatasetPath: "explore.jsonl", IssueDatasetPath: "issues.jsonl",
			OfficialRoot: "upstream", SourceOverrides: overrides,
		},
		Workspace: WorkspaceConfig{
			Root: "workspace", ProjectID: "project", LCTKExecutable: "lctk",
			ExpectedLCTKVersion: "0.1.14", FreshnessTimeoutSeconds: 600,
		},
		Run: RunConfig{TopK: 5, TimeoutSeconds: 600},
	}
}

func TestClassifyCorpusPathSeparatesSourceTestAndNoise(t *testing.T) {
	for path, expected := range map[string][2]bool{
		"app/service.go":                     {true, false},
		"src/features/auth/session.ts":       {true, false},
		"app/service_test.go":                {true, true},
		"src/session.test.ts":                {true, true},
		"src/session.spec.ts":                {true, true},
		"tests/test_session.py":              {true, true},
		"app/__tests__/session.ts":           {true, true},
		"backend/test/helpers.py":            {true, true},
		"app/tests/helper.py":                {true, true},
		"package-lock.json":                  {false, false},
		"go.sum":                             {false, false},
		"Cargo.lock":                         {false, false},
		"assets/logo.png":                    {false, false},
		"web/static/app.min.js":              {false, false},
		"node_modules/left-pad/index.js":     {false, false},
		"vendor/github.com/x/y.go":           {false, false},
		"db/migrations/0001_initial.py":      {false, false},
		"build/output.js":                    {false, false},
		"project.egg-info/PKG-INFO":          {false, false},
		"docs/design.md":                     {false, false},
		"config/settings.yaml":               {false, false},
		"fonts/inter.woff2":                  {false, false},
		"src/generated/schema.ts":            {false, false},
		"app/__pycache__/service.cpython.py": {false, false},
	} {
		eligible, test := classifyCorpusPath(path)
		if eligible != expected[0] || test != expected[1] {
			t.Errorf("classify(%q) = (%v, %v), want (%v, %v)", path, eligible, test, expected[0], expected[1])
		}
	}
}

func TestCodeShapedIdentifierIgnoresOrdinaryProse(t *testing.T) {
	for identifier, want := range map[string]bool{
		"metaProduct":           true,
		"SESSION_DEAD_KEYWORDS": true,
		"workspace_create":      true,
		"MetaProduct":           true,
		"sha256":                true,
		"session":               false,
		"Session":               false,
		"return":                false,
		"NULL":                  false,
		"dig":                   false,
	} {
		if got := codeShapedIdentifier(identifier); got != want {
			t.Errorf("codeShapedIdentifier(%q) = %v, want %v", identifier, got, want)
		}
	}
}

func TestCorpusSlugProducesSafeInstanceIdentifiers(t *testing.T) {
	for name, want := range map[string]string{
		"flexus": "flexus", "Flexus Backend": "flexus-backend",
		"smallcloud/refact": "smallcloud-refact", "--weird--Name--": "weird-name",
	} {
		got, err := corpusSlug(name)
		if err != nil || got != want {
			t.Fatalf("corpusSlug(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	if _, err := corpusSlug("///"); err == nil {
		t.Fatal("a name with no identifier characters was accepted")
	}
	if !safeArtifactID.MatchString("flexus-backend__f8bb52b72abc") {
		t.Fatal("the emitted instance id shape is not a safe campaign artifact id")
	}
}

// corpusRepo builds a synthetic history with real Git so the builder is tested
// against the tool it actually shells out to, not a mock of it.
type corpusRepo struct {
	t    *testing.T
	root string
}

func newCorpusRepo(t *testing.T) *corpusRepo {
	t.Helper()
	root := t.TempDir()
	runCorpusGit(t, root, "init")
	runCorpusGit(t, root, "config", "user.name", "Benchmark")
	runCorpusGit(t, root, "config", "user.email", "benchmark@example.invalid")
	// Windows checkouts convert line endings by default, which would move every
	// hunk offset the builder reads.
	runCorpusGit(t, root, "config", "core.autocrlf", "false")
	return &corpusRepo{t: t, root: root}
}

func (repo *corpusRepo) write(relative, body string) {
	repo.t.Helper()
	path := filepath.Join(repo.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		repo.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		repo.t.Fatal(err)
	}
}

func (repo *corpusRepo) read(relative string) []string {
	repo.t.Helper()
	body, err := os.ReadFile(filepath.Join(repo.root, filepath.FromSlash(relative)))
	if err != nil {
		repo.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

func (repo *corpusRepo) replaceLine(relative string, line int, body string) {
	repo.t.Helper()
	lines := repo.read(relative)
	lines[line-1] = body
	repo.write(relative, strings.Join(lines, "\n")+"\n")
}

func (repo *corpusRepo) insertAfter(relative string, line int, body []string) {
	repo.t.Helper()
	lines := repo.read(relative)
	updated := append([]string{}, lines[:line]...)
	updated = append(updated, body...)
	updated = append(updated, lines[line:]...)
	repo.write(relative, strings.Join(updated, "\n")+"\n")
}

func (repo *corpusRepo) commit(subject, body string) string {
	repo.t.Helper()
	runCorpusGit(repo.t, repo.root, "add", "--all")
	args := []string{"commit", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	runCorpusGit(repo.t, repo.root, args...)
	return runCorpusGit(repo.t, repo.root, "rev-parse", "HEAD")
}

func (repo *corpusRepo) revertLast(subject string) string {
	repo.t.Helper()
	runCorpusGit(repo.t, repo.root, "revert", "--no-commit", "HEAD")
	runCorpusGit(repo.t, repo.root, "commit", "-m", subject)
	return runCorpusGit(repo.t, repo.root, "rev-parse", "HEAD")
}

func (repo *corpusRepo) mergeBranch(subject string) string {
	repo.t.Helper()
	trunk := runCorpusGit(repo.t, repo.root, "rev-parse", "--abbrev-ref", "HEAD")
	runCorpusGit(repo.t, repo.root, "checkout", "-b", "side")
	repo.write("side/branch.go", numberedLines("side", 6))
	repo.commit("fix: adjust the side module", "")
	runCorpusGit(repo.t, repo.root, "checkout", trunk)
	runCorpusGit(repo.t, repo.root, "merge", "--no-ff", "-m", subject, "side")
	return runCorpusGit(repo.t, repo.root, "rev-parse", "HEAD")
}

func (repo *corpusRepo) parentOf(commit string) string {
	repo.t.Helper()
	return runCorpusGit(repo.t, repo.root, "rev-parse", commit+"^")
}

func runCorpusGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "core.autocrlf=false", "-c", "commit.gpgsign=false", "-c", "core.eol=lf"}, args...)
	command := exec.Command("git", full...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(fmt.Errorf("git %v: %s: %w", args, output, err))
	}
	return strings.TrimSpace(string(output))
}

func numberedLines(prefix string, count int) string {
	var builder strings.Builder
	for index := 1; index <= count; index++ {
		fmt.Fprintf(&builder, "%s %d\n", prefix, index)
	}
	return builder.String()
}

func assertRegions(t *testing.T, name string, got, want []Region) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s regions = %v, want %v", name, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s regions = %v, want %v", name, got, want)
		}
	}
}

func candidateIDs(candidates []CorpusCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.InstanceID)
	}
	return ids
}

func equalCandidateIDs(left, right []CorpusCandidate) bool {
	return strings.Join(candidateIDs(left), ",") == strings.Join(candidateIDs(right), ",")
}
