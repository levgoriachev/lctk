package sweexplore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDashboardRebuildsValidatedCampaignState(t *testing.T) {
	root := t.TempDir()
	workspace, configPath, manifestPath, outputRoot, config, manifest := dashboardFixture(t, root)
	runs := 0
	if _, err := runCampaign(context.Background(), config, manifest, configPath, manifestPath, outputRoot, "python", 0, false, dashboardCampaignOperations(t, &runs)); err != nil {
		t.Fatal(err)
	}
	telemetryDir := filepath.Join(outputRoot, "instances", manifest.Instances[0].InstanceID, "prepare-attempts", "dashboard-observation")
	if err := os.MkdirAll(telemetryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writer := newPreparationTelemetryWriter(filepath.Join(telemetryDir, "preparation-telemetry.jsonl"))
	if err := writer.Observe(PreparationSample{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{Indexing: true, ChunksTotal: 100, ChunksEmbedded: 40}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := BuildDashboardSnapshot(config, manifest, outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CampaignID != manifest.CampaignID || snapshot.CompletedInstances != 1 || snapshot.CompletedArms != 4 || snapshot.CompletedPairs != 2 || snapshot.TargetArms != 4 || snapshot.TargetPairs != 2 {
		t.Fatalf("unexpected dashboard summary: %+v", snapshot)
	}
	if snapshot.Activity.ChunksCompleted != 40 || snapshot.Activity.ChunksTotal != 100 || snapshot.Activity.InstanceID != manifest.Instances[0].InstanceID {
		t.Fatalf("unexpected dashboard activity: %+v", snapshot.Activity)
	}
	if snapshot.Providers[ProviderCodex].CompletedPairs != 1 || snapshot.Providers[ProviderClaude].CompletedPairs != 1 {
		t.Fatalf("dashboard aggregate did not reconcile: %+v", snapshot.Providers)
	}
	_ = workspace
}

func TestDashboardRejectsTamperedPrimaryArtifact(t *testing.T) {
	root := t.TempDir()
	_, configPath, manifestPath, outputRoot, config, manifest := dashboardFixture(t, root)
	runs := 0
	if _, err := runCampaign(context.Background(), config, manifest, configPath, manifestPath, outputRoot, "python", 0, false, dashboardCampaignOperations(t, &runs)); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(outputRoot, "instances", manifest.Instances[0].InstanceID, "receipts", "codex-native.json")
	var receipt ArmReceipt
	readJSONTest(t, receiptPath, &receipt)
	if err := os.WriteFile(filepath.Join(outputRoot, filepath.FromSlash(receipt.RawTrace.Path)), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDashboardSnapshot(config, manifest, outputRoot); err == nil {
		t.Fatal("dashboard accepted a tampered primary artifact")
	}
}

func dashboardCampaignOperations(t *testing.T, runs *int) campaignOperations {
	t.Helper()
	operations := fixtureCampaignOperations(t, runs)
	operations.prepare = func(_ context.Context, _ Config, selected Instance, outputRoot, attemptRoot string) (PrepareRecord, error) {
		telemetryPath := filepath.Join(attemptRoot, "preparation-telemetry.jsonl")
		writer := newPreparationTelemetryWriter(telemetryPath)
		if err := writer.Observe(PreparationSample{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Phase: "freshness", Semantic: PreparationSemanticStatus{Ready: true, ChunkCount: 1}}); err != nil {
			return PrepareRecord{}, err
		}
		return completedPrepareRecord(outputRoot, telemetryPath, writer, PrepareRecord{InstanceID: selected.InstanceID, Repository: selected.Repository, BaseCommit: selected.BaseCommit, Freshness: FreshnessProof{ExactGeneration: 1, SemanticGeneration: 1, GraphGeneration: 1}})
	}
	return operations
}

func dashboardFixture(t *testing.T, root string) (string, string, string, string, Config, CampaignManifest) {
	t.Helper()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	explorePath := filepath.Join(root, "explore.jsonl")
	issuePath := filepath.Join(root, "issues.jsonl")
	exploreBody := `{"instance_id":"fixture__repo-1","ground_truth":{"read_core_files":["main.go"],"read_core_regions":[{"path":"main.go","start":1,"end":2}],"read_optional_files_map":{},"read_optional_regions_map":{},"main_files":["main.go"]}}` + "\n"
	issueBody := `{"instance_id":"fixture__repo-1","repo":"owner/repository","base_commit":"1111111111111111111111111111111111111111","problem_statement":"Find the fixture."}` + "\n"
	if err := os.WriteFile(explorePath, []byte(exploreBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(issuePath, []byte(issueBody), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"fixture":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configDigest, _ := FileSHA256(configPath)
	config := Config{SchemaVersion: SchemaVersion,
		Benchmark: BenchmarkConfig{ExploreDatasetPath: explorePath, ExploreDatasetSHA256: testSHA256(exploreBody), IssueDatasetPath: issuePath, IssueDatasetSHA256: testSHA256(issueBody)},
		Workspace: WorkspaceConfig{Root: workspace}, Run: RunConfig{TopK: 1, TimeoutSeconds: 30},
		Arms: []ArmConfig{
			{ID: "codex-native", Provider: ProviderCodex, Mode: ModeNative, Model: "codex-model"},
			{ID: "codex-lctk", Provider: ProviderCodex, Mode: ModeLCTK, Model: "codex-model"},
			{ID: "claude-native", Provider: ProviderClaude, Mode: ModeNative, Model: "claude-model"},
			{ID: "claude-lctk", Provider: ProviderClaude, Mode: ModeLCTK, Model: "claude-model"},
		},
	}
	manifest := CampaignManifest{SchemaVersion: CampaignSchemaVersion, CampaignID: "fixture-campaign", ConfigSHA256: configDigest, RequestedCount: 1, Instances: []CampaignInstance{{InstanceID: "fixture__repo-1", Repository: "owner/repository", BaseCommit: "1111111111111111111111111111111111111111"}}}
	manifestPath := filepath.Join(root, "manifest.json")
	if err := WriteJSONAtomic(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	return workspace, configPath, manifestPath, filepath.Join(root, "campaign"), config, manifest
}

func readJSONTest(t *testing.T, path string, destination any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		t.Fatal(err)
	}
}
