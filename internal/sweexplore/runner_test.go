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
	if mode == string(ModeLCTK) && os.Getenv("SWE_EXPLORE_FIXTURE_SKIP_ADOPTION") == "" {
		trace["lctk_tool_calls"] = []string{"mcp__lctk__exact_search"}
	}
	if os.Getenv("SWE_EXPLORE_FIXTURE_MCP_CONTAMINATION") != "" {
		trace["mcp_tool_calls"] = 1
	}
	// The fixture agent runs with the benchmark checkout as its directory, so
	// this writes into the repository exactly as a misbehaving client would.
	if target := os.Getenv("SWE_EXPLORE_FIXTURE_MUTATION"); target != "" {
		if err := os.WriteFile(target, []byte("mutated by the agent\n"), 0o600); err != nil {
			panic(err)
		}
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
		if result.PromptMode != PromptModeFreeChoice {
			t.Fatalf("%s prompt mode = %q", mode, result.PromptMode)
		}
		if mode == ModeLCTK && (result.Freshness == nil || len(result.LCTKToolCalls) != 1 || !result.TreatmentAdopted) {
			t.Fatalf("treatment evidence = %+v", result)
		}
	}
}

func TestRunArmTreatsZeroTreatmentCallsAsAdoptionDataOnlyInFreeChoice(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-m", "fixture")
	commit := runGit(t, root, "rev-parse", "HEAD")
	instance := Instance{InstanceID: "fixture__repo-1", BaseCommit: commit, ProblemStatement: "Find the fixture logic."}
	arm := ArmConfig{ID: "fixture-lctk", Provider: ProviderFixture, Mode: ModeLCTK, Executable: os.Args[0], Model: "fixture"}
	t.Setenv("SWE_EXPLORE_FIXTURE_SKIP_ADOPTION", "1")
	config := Config{
		SchemaVersion: SchemaVersion,
		Workspace:     WorkspaceConfig{Root: root, ProjectID: "fixture-project", ExpectedLCTKVersion: "fixture"},
		Run:           RunConfig{TopK: 2, TimeoutSeconds: 30, PromptMode: PromptModeFreeChoice},
	}
	result, err := RunArm(context.Background(), config, arm, instance, filepath.Join(root, "..", "artifacts-free-choice"))
	if err != nil {
		t.Fatalf("free-choice zero-adoption treatment run failed: %v", err)
	}
	if result.TreatmentAdopted || len(result.LCTKToolCalls) != 0 || result.PromptMode != PromptModeFreeChoice {
		t.Fatalf("free-choice zero-adoption result = %+v", result)
	}
	config.Run.PromptMode = PromptModeMandated
	if _, err := RunArm(context.Background(), config, arm, instance, filepath.Join(root, "..", "artifacts-mandated")); err == nil || !strings.Contains(err.Error(), "tool-call evidence") {
		t.Fatalf("mandated zero-adoption treatment run must be fatal, got %v", err)
	}
}

func TestRunArmRejectsAnyMCPCallOnNativeArm(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "main.go")
	runGit(t, root, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-m", "fixture")
	commit := runGit(t, root, "rev-parse", "HEAD")
	instance := Instance{InstanceID: "fixture__repo-1", BaseCommit: commit, ProblemStatement: "Find the fixture logic."}
	arm := ArmConfig{ID: "fixture-native", Provider: ProviderFixture, Mode: ModeNative, Executable: os.Args[0], Model: "fixture"}
	config := Config{
		SchemaVersion: SchemaVersion,
		Workspace:     WorkspaceConfig{Root: root, ProjectID: "fixture-project", ExpectedLCTKVersion: "fixture"},
		Run:           RunConfig{TopK: 2, TimeoutSeconds: 30},
	}
	t.Setenv("SWE_EXPLORE_FIXTURE_MCP_CONTAMINATION", "1")
	if _, err := RunArm(context.Background(), config, arm, instance, filepath.Join(root, "..", "artifacts-contaminated")); err == nil || !strings.Contains(err.Error(), "called an MCP tool") {
		t.Fatalf("contaminated native arm must be fatal, got %v", err)
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
	if err := Materialize(context.Background(), root, "owner/repository", first, nil); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, root, "rev-parse", "HEAD"); got != first {
		t.Fatalf("HEAD = %s, want %s", got, first)
	}
	if err := os.WriteFile(path, []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(context.Background(), root, "owner/repository", first, nil); err == nil {
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
	treatmentTools := []string{"project_info", "exact_search", "read_span"}
	excludedTools := []string{"run_command", "memory_put"}
	treatment, err := commandArguments(ArmConfig{Provider: ProviderCodex, Mode: ModeLCTK, Model: "model", Effort: "high", MCPConfigPath: mcpPath, MCPServerName: "lctk-benchmark", TreatmentTools: treatmentTools, ExcludedTools: excludedTools})
	if err != nil {
		t.Fatal(err)
	}
	if containsArgument(treatment, "mcp_servers={}") || !containsArgument(treatment, `mcp_servers.lctk-benchmark.url="http://127.0.0.1:4444/projects/project/mcp"`) || !containsArgument(treatment, "mcp_servers.lctk-benchmark.required=true") || !containsArgument(treatment, `mcp_servers.lctk-benchmark.default_tools_approval_mode="approve"`) || !containsArgument(treatment, `mcp_servers.lctk-benchmark.enabled_tools=["project_info","exact_search","read_span"]`) || containsSubstring(treatment, "run_command") {
		t.Fatalf("Codex treatment args = %q", treatment)
	}
	if _, err := commandArguments(ArmConfig{Provider: ProviderCodex, Mode: ModeLCTK, Model: "model", MCPConfigPath: mcpPath, MCPServerName: "lctk-benchmark"}); err == nil {
		t.Fatal("treatment arm without treatment_tools was accepted")
	}
	claude, err := commandArguments(ArmConfig{Provider: ProviderClaude, Mode: ModeNative, Model: "model", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(claude, `{"mcpServers":{}}`) || !containsArgument(claude, "--strict-mcp-config") || !containsArgument(claude, "Read,Glob,Grep") {
		t.Fatalf("Claude native args = %q", claude)
	}
	claudeTreatment, err := commandArguments(ArmConfig{Provider: ProviderClaude, Mode: ModeLCTK, Model: "model", Effort: "high", MCPConfigPath: mcpPath, MCPServerName: "lctk-benchmark", TreatmentTools: treatmentTools, ExcludedTools: excludedTools})
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstring(claudeTreatment, "mcp__lctk-benchmark__*") || !containsArgument(claudeTreatment, "--disallowed-tools") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__run_command") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__memory_put") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__project_info") || !containsSubstring(claudeTreatment, "mcp__lctk-benchmark__read_span") {
		t.Fatalf("Claude treatment args = %q", claudeTreatment)
	}
}

func TestExplorationPromptFreeChoiceNeverMentionsMCP(t *testing.T) {
	prompt := ExplorationPrompt("Find the bug.", 5, PromptModeFreeChoice, "lctk-benchmark")
	if strings.Contains(prompt, "MCP") || strings.Contains(prompt, "lctk") {
		t.Fatalf("free-choice prompt mentions the treatment server: %q", prompt)
	}
	if !strings.Contains(prompt, "output exactly one RELEVANT_FILES block and no other text") || !strings.Contains(prompt, "Focus on the root cause, not only symptom locations.") {
		t.Fatalf("free-choice prompt lost shared instructions: %q", prompt)
	}
	if prompt != ExplorationPrompt("Find the bug.", 5, PromptModeFreeChoice, "") {
		t.Fatal("free-choice prompt differs across arms")
	}
}

func TestExplorationPromptMandatedNamesConfiguredServer(t *testing.T) {
	prompt := ExplorationPrompt("Find the bug.", 5, PromptModeMandated, "serena_control")
	mandate := "If an MCP server named serena_control is available, you MUST call its project_info tool before any other tool and then use its code-intelligence tools as the primary exploration channel. If that server is unavailable, use the available built-in read-only code exploration tools. Focus on the root cause, not only symptom locations."
	if !strings.Contains(prompt, mandate) {
		t.Fatalf("mandated prompt lost the exact mandate sentence: %q", prompt)
	}
	if !strings.Contains(ExplorationPrompt("Find the bug.", 5, PromptModeMandated, ""), "If an MCP server named lctk_benchmark is available") {
		t.Fatal("mandated prompt without a configured server lost the ADR-0030 fallback name")
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
