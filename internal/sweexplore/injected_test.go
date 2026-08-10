package sweexplore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefectDigestCatchesEveryKindOfAgentChange(t *testing.T) {
	workspace, base, patch := newInjectedWorkspace(t)
	digest, err := ApplyDefect(context.Background(), workspace, patch)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("applying a defect produced no digest")
	}
	if err := VerifyDefectState(context.Background(), workspace, base, digest); err != nil {
		t.Fatalf("an untouched patched tree must verify: %v", err)
	}
	// An edited tracked file changes the tracked diff.
	servicePath := filepath.Join(workspace, "app", "service.go")
	original, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servicePath, append(original, []byte("agent note\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDefectState(context.Background(), workspace, base, digest); err == nil || !strings.Contains(err.Error(), "digest is") {
		t.Fatalf("an edited file must be fatal and read as a mutation, got %v", err)
	}
	if err := os.WriteFile(servicePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDefectState(context.Background(), workspace, base, digest); err != nil {
		t.Fatalf("restoring the file must restore the digest: %v", err)
	}
	// A new file appears in no tracked diff at all, which is why the digest
	// covers the untracked listing as well.
	if err := os.WriteFile(filepath.Join(workspace, "agent-notes.md"), []byte("scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDefectState(context.Background(), workspace, base, digest); err == nil || !strings.Contains(err.Error(), "digest is") {
		t.Fatalf("an untracked file must be fatal, got %v", err)
	}
}

func TestExpectedWorkingTreeSeparatesAMissingPatchFromAMutation(t *testing.T) {
	workspace, base, patch := newInjectedWorkspace(t)
	instance := Instance{InstanceID: "flexus-inj__generic-01", BaseCommit: base, DefectPatch: patch, Stratum: StratumGeneric}
	if _, err := ExpectedWorkingTree(context.Background(), workspace, instance); err == nil || !strings.Contains(err.Error(), "is not applied") {
		t.Fatalf("an unprepared workspace must name the missing patch, got %v", err)
	}
	digest, err := ApplyDefect(context.Background(), workspace, patch)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectedWorkingTree(context.Background(), workspace, instance)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Digest != digest || expected.BaseCommit != base || !expected.Injected() {
		t.Fatalf("expected tree = %+v, want the applied digest %s", expected, digest)
	}
	// Reverting the defect is a different operator problem from editing the
	// patched tree, so the two must not report the same way.
	if _, err := defectGit(context.Background(), workspace, "apply", "--reverse", "--whitespace=nowarn", patch); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWorkingTree(context.Background(), workspace, expected); err == nil || !strings.Contains(err.Error(), "no longer applied") {
		t.Fatalf("a reverted defect must be reported as such, got %v", err)
	}
}

func TestApplyDefectRefusesADirtyCheckoutAndForeignChanges(t *testing.T) {
	workspace, _, patch := newInjectedWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "stray.txt"), []byte("left behind\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDefect(context.Background(), workspace, patch); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("applying onto a dirty checkout must be fatal, got %v", err)
	}
}

func TestRunArmMeasuresAnInjectedInstanceAndKeepsTheMutationGuarantee(t *testing.T) {
	workspace, base, patch := newInjectedWorkspace(t)
	if _, err := ApplyDefect(context.Background(), workspace, patch); err != nil {
		t.Fatal(err)
	}
	config := Config{
		SchemaVersion: SchemaVersion,
		Workspace:     WorkspaceConfig{Root: workspace, ProjectID: "fixture-project", ExpectedLCTKVersion: "fixture"},
		Run:           RunConfig{TopK: 2, TimeoutSeconds: 60},
	}
	instance := Instance{
		InstanceID: "flexus-inj__generic-01", BaseCommit: base, ProblemStatement: "Totals come out short.",
		DefectPatch: patch, Stratum: StratumGeneric,
	}
	arm := ArmConfig{ID: "fixture-native", Provider: ProviderFixture, Mode: ModeNative, Executable: os.Args[0], Model: "fixture"}
	result, err := RunArm(context.Background(), config, arm, instance, filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("injected arm: %v", err)
	}
	if result.Stratum != StratumGeneric || result.DefectDigest == "" {
		t.Fatalf("result does not prove which tree was measured: %+v", result)
	}
	// The post-arm guarantee is unchanged in spirit: any agent write is fatal,
	// it simply surfaces as a digest mismatch instead of a dirty checkout.
	t.Setenv("SWE_EXPLORE_FIXTURE_MUTATION", "agent-scratch.txt")
	if _, err := RunArm(context.Background(), config, arm, instance, filepath.Join(t.TempDir(), "mutated")); err == nil || !strings.Contains(err.Error(), "agent mutated benchmark repository") {
		t.Fatalf("an agent write to an injected checkout must be fatal, got %v", err)
	}
}

func TestRunArmLeavesNonInjectedInstancesUnchanged(t *testing.T) {
	workspace, base, _ := newInjectedWorkspace(t)
	config := Config{
		SchemaVersion: SchemaVersion,
		Workspace:     WorkspaceConfig{Root: workspace, ProjectID: "fixture-project", ExpectedLCTKVersion: "fixture"},
		Run:           RunConfig{TopK: 2, TimeoutSeconds: 60},
	}
	instance := Instance{InstanceID: "flexus__mined-01", BaseCommit: base, ProblemStatement: "Totals come out short."}
	arm := ArmConfig{ID: "fixture-native", Provider: ProviderFixture, Mode: ModeNative, Executable: os.Args[0], Model: "fixture"}
	result, err := RunArm(context.Background(), config, arm, instance, filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("mined arm: %v", err)
	}
	if result.Stratum != "" || result.DefectDigest != "" {
		t.Fatalf("a mined instance must record no defect state: %+v", result)
	}
	t.Setenv("SWE_EXPLORE_FIXTURE_MUTATION", "agent-scratch.txt")
	if _, err := RunArm(context.Background(), config, arm, instance, filepath.Join(t.TempDir(), "mutated")); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("a mined checkout keeps the historical cleanliness message, got %v", err)
	}
}

func TestRevertDefectRestoresThePristineCheckout(t *testing.T) {
	workspace, base, patch := newInjectedWorkspace(t)
	digest, err := ApplyDefect(context.Background(), workspace, patch)
	if err != nil {
		t.Fatal(err)
	}
	state := DefectWorkspaceState{SchemaVersion: CampaignSchemaVersion, InstanceID: "flexus-inj__generic-01", BaseCommit: base, DefectPatch: patch, Digest: digest}
	if err := RevertDefect(context.Background(), workspace, state); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRepository(context.Background(), workspace, base); err != nil {
		t.Fatalf("reverting must leave the historical pristine state: %v", err)
	}
	// A workspace somebody else changed is never silently reset.
	if _, err := ApplyDefect(context.Background(), workspace, patch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "unrelated.txt"), []byte("someone else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RevertDefect(context.Background(), workspace, state); err == nil || !strings.Contains(err.Error(), "refusing to revert") {
		t.Fatalf("reverting a changed workspace must be refused, got %v", err)
	}
}

func TestCampaignPreparationSwapsOneDefectForTheNext(t *testing.T) {
	workspace, base, patch := newInjectedWorkspace(t)
	outputRoot := t.TempDir()
	instance := Instance{InstanceID: "flexus-inj__generic-01", BaseCommit: base, DefectPatch: patch, Stratum: StratumGeneric}
	config := Config{Workspace: WorkspaceConfig{Root: workspace}}
	digest, err := applyCampaignDefect(context.Background(), config, instance, outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Preparing the same instance again must recognize the workspace instead of
	// reverting a tree it is about to need.
	reverted, carriesThisDefect, err := revertRecordedDefect(context.Background(), config, instance, outputRoot)
	if err != nil || reverted || !carriesThisDefect {
		t.Fatalf("reverted=%v carries=%v err=%v", reverted, carriesThisDefect, err)
	}
	// The next instance is a different task, so its predecessor's defect has to
	// come off before Materialize is allowed to refuse a dirty checkout.
	next := Instance{InstanceID: "flexus__mined-02", BaseCommit: base}
	reverted, carriesThisDefect, err = revertRecordedDefect(context.Background(), config, next, outputRoot)
	if err != nil || !reverted || carriesThisDefect {
		t.Fatalf("reverted=%v carries=%v err=%v", reverted, carriesThisDefect, err)
	}
	if err := VerifyRepository(context.Background(), workspace, base); err != nil {
		t.Fatalf("the workspace was not returned to its pristine state: %v", err)
	}
	if _, recorded, err := readDefectWorkspaceState(outputRoot); err != nil || recorded {
		t.Fatalf("the recorded defect state survived the revert: recorded=%v err=%v", recorded, err)
	}
	// Re-applying the identical patch to the identical commit must reproduce the
	// digest exactly; that is what makes the receipt cross-check meaningful.
	again, err := applyCampaignDefect(context.Background(), config, instance, outputRoot)
	if err != nil || again != digest {
		t.Fatalf("re-application digest = %q, want %q (%v)", again, digest, err)
	}
}

func TestPreparationRefusesADigestThatContradictsAReceipt(t *testing.T) {
	outputRoot := t.TempDir()
	instance := Instance{InstanceID: "flexus-inj__generic-01", BaseCommit: "abc", DefectPatch: "service.patch"}
	if err := verifyRecordedDefectDigest(outputRoot, instance, "digest-one"); err != nil {
		t.Fatalf("an instance with no receipts has nothing to contradict: %v", err)
	}
	resultPath := filepath.Join(outputRoot, "instances", instance.InstanceID, "attempts", "codex-native.result.json")
	if err := WriteJSONAtomic(resultPath, Result{InstanceID: instance.InstanceID, DefectDigest: "digest-one"}); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(outputRoot, "instances", instance.InstanceID, "receipts", "codex-native.json")
	if err := WriteJSONAtomic(receiptPath, ArmReceipt{InstanceID: instance.InstanceID, Result: artifactReference(outputRoot, resultPath)}); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordedDefectDigest(outputRoot, instance, "digest-one"); err != nil {
		t.Fatalf("a matching digest must be accepted: %v", err)
	}
	// A corpus edited underneath a resumed campaign is the failure this catches.
	err := verifyRecordedDefectDigest(outputRoot, instance, "digest-two")
	if err == nil || !strings.Contains(err.Error(), "digest-one") {
		t.Fatalf("a contradicted digest must be fatal, got %v", err)
	}
}

func TestStratumAggregatesPartitionThePairsAndStayDeterministic(t *testing.T) {
	strata := []string{StratumLiteral, StratumGeneric, StratumLiteral, ""}
	quality := []map[string]float64{
		{"weighted_core_coverage": 0.2}, {"weighted_core_coverage": -0.1},
		{"weighted_core_coverage": 0.4}, {"weighted_core_coverage": 9},
	}
	telemetry := []map[string]float64{
		{"elapsed_ms": -100}, {"elapsed_ms": 50}, {"elapsed_ms": -300}, {"elapsed_ms": 9},
	}
	adopted := []bool{true, false, false, true}
	aggregates := stratumAggregates("campaign", ProviderCodex, strata, quality, telemetry, adopted)
	if len(aggregates) != 2 {
		t.Fatalf("aggregates = %+v, want only the declared strata", aggregates)
	}
	literal := aggregates[StratumLiteral]
	if literal.CompletedPairs != 2 || literal.MeanPairedDelta["weighted_core_coverage"] != 0.30000000000000004 {
		t.Fatalf("literal aggregate = %+v", literal)
	}
	if literal.MeanPairedTelemetryDelta["elapsed_ms"] != -200 || literal.AdoptionRate != 0.5 {
		t.Fatalf("literal telemetry = %+v", literal)
	}
	// A single-pair stratum degenerates to a point interval rather than
	// pretending to an inference it cannot support.
	if interval := aggregates[StratumGeneric].PrimaryBootstrap95CI["weighted_core_coverage"]; interval.Lower != -0.1 || interval.Upper != -0.1 {
		t.Fatalf("single-pair interval = %+v", interval)
	}
	repeated := stratumAggregates("campaign", ProviderCodex, strata, quality, telemetry, adopted)
	if repeated[StratumLiteral].TelemetryBootstrap95CI["elapsed_ms"] != literal.TelemetryBootstrap95CI["elapsed_ms"] {
		t.Fatal("per-stratum bootstrap intervals are not deterministic")
	}
	// A different stratum resamples a different sample, so its seed must differ
	// from the provider-level one it is nested under.
	if aggregates[StratumGeneric].TelemetryBootstrap95CI["elapsed_ms"] == literal.TelemetryBootstrap95CI["elapsed_ms"] {
		t.Fatal("two strata produced identical intervals from different samples")
	}
	if stratumAggregates("campaign", ProviderCodex, []string{"", ""}, quality[:2], telemetry[:2], adopted[:2]) != nil {
		t.Fatal("a corpus with no declared stratum must not invent a per-stratum view")
	}
}

// newInjectedWorkspace builds a benchmark checkout and an authored patch that
// applies to it, using real Git so the harness is exercised against the tool it
// actually shells out to.
func newInjectedWorkspace(t *testing.T) (string, string, string) {
	t.Helper()
	repo := newCorpusRepo(t)
	repo.write("app/service.go", numberedLines("step", 20))
	base := repo.commit("seed the service", "")
	author := newDefectAuthor(t, repo)
	name := author.capture("service.patch", func() {
		repo.replaceLine("app/service.go", 7, "step seven adjusted")
	})
	workspace := t.TempDir()
	runCorpusGit(t, workspace, "init")
	if err := Materialize(context.Background(), workspace, "flexus", base, map[string]string{"flexus": repo.root}); err != nil {
		t.Fatal(err)
	}
	return workspace, base, filepath.Join(author.directory, name)
}
