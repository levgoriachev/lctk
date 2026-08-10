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

func TestFixtureAgentProcess(t *testing.T) {
	mode := os.Getenv("SWE_EXPLORE_FIXTURE_MODE")
	if mode == "" {
		return
	}
	trace := map[string]any{
		"final_message": "RELEVANT_FILES:\n- main.go:1-2\n",
		"usage":         Usage{InputTokens: 10, OutputTokens: 3},
	}
	if mode == string(ModeLCTK) {
		trace["lctk_tool_calls"] = []string{"mcp__lctk__exact_search"}
	}
	if err := json.NewEncoder(os.Stdout).Encode(trace); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func TestRunArmCompletesSyntheticNativeAndTreatmentPipeline(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-m", "fixture")
	commit := runGit(t, root, "rev-parse", "HEAD")
	config := Config{
		SchemaVersion: SchemaVersion,
		Workspace:     WorkspaceConfig{Root: root, ProjectID: "fixture-project", ExpectedLCTKVersion: "fixture"},
		Run:           RunConfig{TopK: 2, TimeoutSeconds: 30},
	}
	instance := Instance{
		InstanceID: "fixture__repo-1", BaseCommit: commit, ProblemStatement: "Find the fixture logic.",
	}
	for _, mode := range []Mode{ModeNative, ModeLCTK} {
		arm := ArmConfig{ID: "fixture-" + string(mode), Provider: ProviderFixture, Mode: mode, Executable: os.Args[0], Model: "fixture"}
		result, err := RunArm(context.Background(), config, arm, instance, filepath.Join(root, "..", "artifacts-"+string(mode)))
		if err != nil {
			t.Fatalf("%s arm: %v", mode, err)
		}
		if len(result.Regions) != 1 || result.Regions[0].Path != "main.go" {
			t.Fatalf("%s result = %+v", mode, result)
		}
		if result.ClientVersion != "fixture" {
			t.Fatalf("%s client version = %q", mode, result.ClientVersion)
		}
		if mode == ModeLCTK && (result.Freshness == nil || len(result.LCTKToolCalls) != 1) {
			t.Fatalf("treatment evidence = %+v", result)
		}
	}
}

func TestMaterializeSwitchesOnlyCleanRepository(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-m", "first")
	first := runGit(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-m", "second")
	if err := Materialize(context.Background(), root, "owner/repository", first); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, root, "rev-parse", "HEAD"); got != first {
		t.Fatalf("HEAD = %s, want %s", got, first)
	}
	if err := os.WriteFile(path, []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(context.Background(), root, "owner/repository", first); err == nil {
		t.Fatal("dirty repository was materialized")
	}
}

func TestPublicGitHubURLRejectsDatasetInjection(t *testing.T) {
	if got, err := publicGitHubURL("pydata/xarray"); err != nil || got != "https://github.com/pydata/xarray.git" {
		t.Fatalf("valid repository = %q, %v", got, err)
	}
	for _, invalid := range []string{"https://example.com/repo", "owner/repo/extra", "owner repo/name", ""} {
		if _, err := publicGitHubURL(invalid); err == nil {
			t.Fatalf("unsafe repository %q was accepted", invalid)
		}
	}
}

func TestCommandArgumentsEnforceProviderIsolation(t *testing.T) {
	root := t.TempDir()
	mcpPath := filepath.Join(root, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"lctk-benchmark":{"type":"http","url":"http://127.0.0.1:4444/projects/project/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := commandArguments(ArmConfig{Provider: ProviderCodex, Mode: ModeNative, Model: "model", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(native, "--ignore-user-config") || !containsArgument(native, "mcp_servers={}") || !containsArgument(native, `web_search="disabled"`) || !containsArgument(native, "features.apps=false") || !containsArgument(native, "memories.use_memories=false") {
		t.Fatalf("Codex native args = %q", native)
	}
	treatment, err := commandArguments(ArmConfig{Provider: ProviderCodex, Mode: ModeLCTK, Model: "model", Effort: "high", MCPConfigPath: mcpPath, MCPServerName: "lctk-benchmark"})
	if err != nil {
		t.Fatal(err)
	}
	if containsArgument(treatment, "mcp_servers={}") || !containsArgument(treatment, `mcp_servers.lctk-benchmark.url="http://127.0.0.1:4444/projects/project/mcp"`) || !containsArgument(treatment, "mcp_servers.lctk-benchmark.required=true") || !containsArgument(treatment, `mcp_servers.lctk-benchmark.default_tools_approval_mode="approve"`) || !containsSubstring(treatment, `mcp_servers.lctk-benchmark.enabled_tools=["project_info"`) || containsSubstring(treatment, "run_command") {
		t.Fatalf("Codex treatment args = %q", treatment)
	}
	claude, err := commandArguments(ArmConfig{Provider: ProviderClaude, Mode: ModeNative, Model: "model", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(claude, `{"mcpServers":{}}`) || !containsArgument(claude, "--strict-mcp-config") || !containsArgument(claude, "Read,Glob,Grep") {
		t.Fatalf("Claude native args = %q", claude)
	}
	claudeTreatment, err := commandArguments(ArmConfig{Provider: ProviderClaude, Mode: ModeLCTK, Model: "model", Effort: "high", MCPConfigPath: mcpPath, MCPServerName: "lctk-benchmark"})
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstring(claudeTreatment, "mcp__lctk-benchmark__*") || !containsArgument(claudeTreatment, "--disallowed-tools") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__run_command") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__project_info") {
		t.Fatalf("Claude treatment args = %q", claudeTreatment)
	}
}

func TestExplorationPromptHasSymmetricLCTKInstruction(t *testing.T) {
	prompt := ExplorationPrompt("Find the bug.", 5)
	if !strings.Contains(prompt, "you MUST call its project_info tool before any other tool") || !strings.Contains(prompt, "output exactly one RELEVANT_FILES block and no other text") {
		t.Fatalf("prompt does not require the treatment tool: %q", prompt)
	}
}

func containsArgument(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, expected string) bool {
	for _, value := range values {
		if strings.Contains(value, expected) {
			return true
		}
	}
	return false
}

func runGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(fmt.Errorf("git %v: %s: %w", args, output, err))
	}
	return strings.TrimSpace(string(output))
}
