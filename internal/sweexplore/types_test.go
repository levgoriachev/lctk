package sweexplore

import (
	"path/filepath"
	"testing"
)

func validTestConfig() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Benchmark:     BenchmarkConfig{ExploreDatasetPath: "explore", IssueDatasetPath: "issues", OfficialRoot: "official"},
		Workspace:     WorkspaceConfig{Root: "workspace", ProjectID: "project", LCTKExecutable: "lctk", ExpectedLCTKVersion: "1.0", FreshnessTimeoutSeconds: 60},
		Run:           RunConfig{TopK: 5, TimeoutSeconds: 600},
		Arms: []ArmConfig{
			{ID: "codex-native", Provider: ProviderCodex, Mode: ModeNative, Executable: "codex", Model: "model", Effort: "high"},
			{ID: "codex-lctk", Provider: ProviderCodex, Mode: ModeLCTK, Executable: "codex", Model: "model", Effort: "high", MCPConfigPath: "mcp.json", MCPServerName: "lctk", TreatmentTools: []string{"project_info", "exact_search"}},
			{ID: "claude-native", Provider: ProviderClaude, Mode: ModeNative, Executable: "claude", Model: "model", Effort: "high"},
			{ID: "claude-lctk", Provider: ProviderClaude, Mode: ModeLCTK, Executable: "claude", Model: "model", Effort: "high", MCPConfigPath: "mcp.json", MCPServerName: "lctk", TreatmentTools: []string{"project_info", "exact_search"}, ExcludedTools: []string{"run_command"}},
		},
	}
}

func TestConfigRequiresMatchedFourArmCampaign(t *testing.T) {
	config := validTestConfig()
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

func TestConfigSchemaVersionIsTwoAndExclusive(t *testing.T) {
	if SchemaVersion != 2 {
		t.Fatalf("benchmark schema version = %d, want 2", SchemaVersion)
	}
	config := validTestConfig()
	config.SchemaVersion = 1
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("schema version 1 was accepted by a version-2 harness")
	}
}

func TestConfigValidatesPromptMode(t *testing.T) {
	config := validTestConfig()
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if config.Run.PromptMode != PromptModeFreeChoice {
		t.Fatalf("empty prompt mode normalized to %q, want %q", config.Run.PromptMode, PromptModeFreeChoice)
	}
	config = validTestConfig()
	config.Run.PromptMode = PromptModeMandated
	if err := config.Validate(t.TempDir()); err != nil {
		t.Fatalf("mandated prompt mode was rejected: %v", err)
	}
	config = validTestConfig()
	config.Run.PromptMode = "optional"
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("unknown prompt mode was accepted")
	}
}

func TestConfigRequiresTreatmentToolsOnlyOnTreatmentArms(t *testing.T) {
	config := validTestConfig()
	config.Arms[1].TreatmentTools = nil
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("treatment arm without treatment_tools was accepted")
	}
	config = validTestConfig()
	config.Arms[1].TreatmentTools = []string{"project_info", " "}
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("blank treatment tool name was accepted")
	}
	config = validTestConfig()
	config.Arms[0].TreatmentTools = []string{"project_info"}
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("native arm with treatment_tools was accepted")
	}
	config = validTestConfig()
	config.Arms[2].ExcludedTools = []string{"run_command"}
	if err := config.Validate(t.TempDir()); err == nil {
		t.Fatal("native arm with excluded_tools was accepted")
	}
}

func TestCounterbalancedPairIsStableAndContainsBothArms(t *testing.T) {
	native := ArmConfig{ID: "native", Mode: ModeNative}
	treatment := ArmConfig{ID: "lctk", Mode: ModeLCTK}
	first := CounterbalancedPair("instance", ProviderCodex, native, treatment)
	second := CounterbalancedPair("instance", ProviderCodex, native, treatment)
	if first[0].ID != second[0].ID || first[1].ID != second[1].ID || first[0].ID == first[1].ID {
		t.Fatalf("counterbalance = %+v then %+v", first, second)
	}
}
