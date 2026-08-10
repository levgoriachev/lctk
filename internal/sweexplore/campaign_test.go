package sweexplore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteJSONAtomicRefusesToReplaceEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := WriteJSONAtomic(path, map[string]int{"value": 1}); err != nil {
		t.Fatal(err)
	}
	digest, err := FileSHA256(path)
	if err != nil || len(digest) != sha256.Size*2 {
		t.Fatalf("digest = %q, %v", digest, err)
	}
	if err := WriteJSONAtomic(path, map[string]int{"value": 2}); err == nil {
		t.Fatal("immutable evidence was replaced")
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), `"value": 1`) {
		t.Fatalf("evidence changed: %s, %v", body, err)
	}
}

func TestCampaignSelectionIsDeterministicAndCoversEveryRepository(t *testing.T) {
	candidates := []CampaignInstance{
		{InstanceID: "a-1", Repository: "owner/a"}, {InstanceID: "a-2", Repository: "owner/a"},
		{InstanceID: "b-1", Repository: "owner/b"}, {InstanceID: "b-2", Repository: "owner/b"},
		{InstanceID: "c-1", Repository: "owner/c"},
	}
	first, err := selectCampaignInstances(candidates, 4, "seed")
	if err != nil {
		t.Fatal(err)
	}
	second, err := selectCampaignInstances(candidates, 4, "seed")
	if err != nil {
		t.Fatal(err)
	}
	if !equalCampaignInstances(first, second) {
		t.Fatalf("selection changed: %+v then %+v", first, second)
	}
	repositories := map[string]bool{}
	for _, instance := range first {
		repositories[instance.Repository] = true
	}
	if len(repositories) != 3 {
		t.Fatalf("repository coverage = %v", repositories)
	}
	if _, err := selectCampaignInstances(candidates, 2, "seed"); err == nil {
		t.Fatal("sample smaller than repository count was accepted")
	}
}

func TestCampaignPublishesHashedReceiptsAndResumesWithoutRerun(t *testing.T) {
	root := t.TempDir()
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
	config := Config{
		SchemaVersion: SchemaVersion,
		Benchmark:     BenchmarkConfig{ExploreDatasetPath: explorePath, ExploreDatasetSHA256: testSHA256(exploreBody), IssueDatasetPath: issuePath, IssueDatasetSHA256: testSHA256(issueBody)},
		Workspace:     WorkspaceConfig{Root: workspace}, Run: RunConfig{TopK: 1, TimeoutSeconds: 30},
		Arms: []ArmConfig{
			{ID: "codex-native", Provider: ProviderCodex, Mode: ModeNative, Model: "codex-model"},
			{ID: "codex-lctk", Provider: ProviderCodex, Mode: ModeLCTK, Model: "codex-model"},
			{ID: "claude-native", Provider: ProviderClaude, Mode: ModeNative, Model: "claude-model"},
			{ID: "claude-lctk", Provider: ProviderClaude, Mode: ModeLCTK, Model: "claude-model"},
		},
	}
	manifest := CampaignManifest{SchemaVersion: CampaignSchemaVersion, CampaignID: "fixture-campaign", ConfigSHA256: configDigest, RequestedCount: 1, Instances: []CampaignInstance{{InstanceID: "fixture__repo-1", Repository: "owner/repository", BaseCommit: "1111111111111111111111111111111111111111"}}}
	manifestPath := filepath.Join(root, "manifest-source.json")
	if err := WriteJSONAtomic(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	outputRoot := filepath.Join(root, "campaign")
	runs := 0
	prepares := 0
	operations := fixtureCampaignOperations(t, &runs)
	fixturePrepare := operations.prepare
	operations.prepare = func(ctx context.Context, config Config, selected CampaignInstance, outputRoot, attemptRoot string) (PrepareRecord, error) {
		prepares++
		return fixturePrepare(ctx, config, selected, outputRoot, attemptRoot)
	}
	report, err := runCampaign(context.Background(), config, manifest, configPath, manifestPath, outputRoot, "python", 0, false, operations)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 4 || prepares != 1 || report.Providers[ProviderCodex].CompletedPairs != 1 || report.Providers[ProviderClaude].CompletedPairs != 1 {
		t.Fatalf("runs = %d, prepares = %d, report = %+v", runs, prepares, report)
	}
	if len(report.Providers[ProviderCodex].Native.MeanMetrics) != 17 || len(report.Providers[ProviderCodex].PrimaryBootstrap95CI) != 4 {
		t.Fatalf("incomplete metric report: %+v", report.Providers[ProviderCodex])
	}
	operations.runArm = func(context.Context, Config, ArmConfig, Instance, string) (Result, error) {
		return Result{}, errors.New("completed arm was rerun")
	}
	report, err = runCampaign(context.Background(), config, manifest, configPath, manifestPath, outputRoot, "python", 0, false, operations)
	if err != nil || runs != 4 || prepares != 1 || report.Providers[ProviderClaude].CompletedPairs != 1 {
		t.Fatalf("resume repeated work: runs=%d prepares=%d report=%+v err=%v", runs, prepares, report, err)
	}
	receiptPath := filepath.Join(outputRoot, "instances", "fixture__repo-1", "receipts", "codex-native.json")
	var receipt ArmReceipt
	receiptBody, _ := os.ReadFile(receiptPath)
	if err := json.Unmarshal(receiptBody, &receipt); err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(outputRoot, filepath.FromSlash(receipt.RawTrace.Path))
	if err := os.WriteFile(rawPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCampaign(context.Background(), config, manifest, configPath, manifestPath, outputRoot, "python", 0, false, operations); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered trace was accepted: %v", err)
	}
	if prepares != 1 {
		t.Fatalf("tampered receipt triggered workspace preparation: prepares=%d", prepares)
	}
}

func fixtureCampaignOperations(t *testing.T, runs *int) campaignOperations {
	t.Helper()
	return campaignOperations{
		prepare: func(_ context.Context, _ Config, selected CampaignInstance, _, _ string) (PrepareRecord, error) {
			return PrepareRecord{InstanceID: selected.InstanceID, Repository: selected.Repository, BaseCommit: selected.BaseCommit, Freshness: FreshnessProof{ExactGeneration: 1, SemanticGeneration: 1, GraphGeneration: 1}}, nil
		},
		runArm: func(_ context.Context, _ Config, arm ArmConfig, instance Instance, output string) (Result, error) {
			*runs++
			rawPath := filepath.Join(output, arm.ID+".trace.jsonl")
			if err := WriteFileAtomic(rawPath, []byte(`{"trace":true}`+"\n"), 0o600); err != nil {
				return Result{}, err
			}
			actual := []string(nil)
			if arm.Provider == ProviderClaude {
				actual = []string{arm.Model}
			}
			calls := []string(nil)
			if arm.Mode == ModeLCTK {
				calls = []string{"mcp__lctk__project_info"}
			}
			return Result{SchemaVersion: SchemaVersion, InstanceID: instance.InstanceID, ArmID: arm.ID, Provider: arm.Provider, Mode: arm.Mode, Model: arm.Model, ClientVersion: "fixture-client", BaseCommit: instance.BaseCommit, ElapsedMS: 100, Regions: []Region{{Path: "main.go", Start: 1, End: 2}}, LCTKToolCalls: calls, ActualModels: actual, Usage: Usage{InputTokens: 10, OutputTokens: 2}, ProviderStats: ProviderMetrics{ReportedCostUSD: 0.01}, RawTracePath: filepath.ToSlash(rawPath)}, nil
		},
		officialScore: func(_ context.Context, _ string, config Config, resultPath string) (string, error) {
			body, err := os.ReadFile(resultPath)
			if err != nil {
				return "", err
			}
			var result Result
			if err := json.Unmarshal(body, &result); err != nil {
				return "", err
			}
			metrics, err := Score(config.Workspace.Root, result.Regions, GroundTruth{ReadCoreFiles: []string{"main.go"}, ReadCoreRegions: []Region{{Path: "main.go", Start: 1, End: 2}}, MainFiles: []string{"main.go"}})
			if err != nil {
				return "", err
			}
			score, err := json.Marshal(OfficialScoreRecord{InstanceID: result.InstanceID, Explorer: result.ArmID, Regions: result.Regions, Metrics: metrics, NumRegions: len(result.Regions)})
			return string(score), err
		},
	}
}

func testSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func equalCampaignInstances(left, right []CampaignInstance) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
