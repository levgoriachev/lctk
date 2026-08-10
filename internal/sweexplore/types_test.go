package sweexplore

import (
	"path/filepath"
	"testing"
)

func TestConfigRequiresMatchedFourArmCampaign(t *testing.T) {
	config := Config{
		SchemaVersion: SchemaVersion,
		Benchmark:     BenchmarkConfig{ExploreDatasetPath: "explore", IssueDatasetPath: "issues", OfficialRoot: "official"},
		Workspace:     WorkspaceConfig{Root: "workspace", ProjectID: "project", LCTKExecutable: "lctk", ExpectedLCTKVersion: "1.0", FreshnessTimeoutSeconds: 60},
		Run:           RunConfig{TopK: 5, TimeoutSeconds: 600},
		Arms: []ArmConfig{
			{ID: "codex-native", Provider: ProviderCodex, Mode: ModeNative, Executable: "codex", Model: "model", Effort: "high"},
			{ID: "codex-lctk", Provider: ProviderCodex, Mode: ModeLCTK, Executable: "codex", Model: "model", Effort: "high", MCPConfigPath: "mcp.json", MCPServerName: "lctk"},
			{ID: "claude-native", Provider: ProviderClaude, Mode: ModeNative, Executable: "claude", Model: "model", Effort: "high"},
			{ID: "claude-lctk", Provider: ProviderClaude, Mode: ModeLCTK, Executable: "claude", Model: "model", Effort: "high", MCPConfigPath: "mcp.json", MCPServerName: "lctk"},
		},
	}
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(config.Workspace.Root) || !filepath.IsAbs(config.Arms[1].MCPConfigPath) {
		t.Fatalf("relative paths were not resolved: %+v", config)
	}
	config.Arms[1].Model = "different"
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("mismatched pair was accepted")
	}
	config.Arms[1].Model = "model"
	config.Workspace.FreshnessTimeoutSeconds = 43200
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatalf("twelve-hour preparation bound was rejected: %v", err)
	}
	config.Workspace.FreshnessTimeoutSeconds++
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("preparation timeout above twelve hours was accepted")
	}
}

func TestCounterbalancedPairIsStableAndContainsBothArms(t *testing.T) {
	native := ArmConfig{ID: "native", Mode: ModeNative}
	treatment := ArmConfig{ID: "lctk", Mode: ModeLCTK}
	first := CounterbalancedPair("instance", ProviderCodex, native, treatment)
	second := CounterbalancedPair("instance", ProviderCodex, native, treatment)
	if first != second || first[0].ID == first[1].ID {
		t.Fatalf("counterbalance = %+v then %+v", first, second)
	}
}
