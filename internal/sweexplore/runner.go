package sweexplore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeMCPName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// benchmarkLCTKTools is the complete treatment surface. Command execution,
// Git inspection, and persistent-memory tools are deliberately absent because
// the benchmark isolates code intelligence rather than the whole product.
var benchmarkLCTKTools = []string{
	"project_info",
	"exact_search",
	"file_outline",
	"find_definition",
	"find_references",
	"code_search_semantic",
	"callers_find",
	"callees_find",
	"dependency_path",
	"impact_analyze",
	"repository_map",
}

var excludedLCTKTools = []string{
	"run_command",
	"git_status",
	"git_diff",
	"memory_get",
	"memory_search",
	"memory_put",
	"memory_delete",
}

// RunArm performs all unmeasured checks, then measures exactly one fresh agent
// process and writes its raw and normalized artifacts.
func RunArm(ctx context.Context, config Config, arm ArmConfig, instance Instance, outputDir string) (Result, error) {
	if err := VerifyRepository(ctx, config.Workspace.Root, instance.BaseCommit); err != nil {
		return Result{}, err
	}
	clientVersion, err := readClientVersion(ctx, config.Workspace.Root, arm)
	if err != nil {
		return Result{}, err
	}
	var freshness *FreshnessProof
	if arm.Mode == ModeLCTK {
		if arm.Provider == ProviderFixture {
			freshness = &FreshnessProof{ProjectID: config.Workspace.ProjectID, Version: config.Workspace.ExpectedLCTKVersion,
				ExactGeneration: 1, SemanticGeneration: 1, GraphGeneration: 1, WatcherGeneration: 1,
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		} else {
			proof, err := WaitForLCTK(ctx, config.Workspace)
			if err != nil {
				return Result{}, err
			}
			freshness = &proof
		}
	}
	args, err := commandArguments(arm)
	if err != nil {
		return Result{}, err
	}
	prompt := ExplorationPrompt(instance.ProblemStatement, config.Run.TopK)
	measured, cancel := context.WithTimeout(ctx, time.Duration(config.Run.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(measured, arm.Executable, args...)
	command.Dir = config.Workspace.Root
	command.Stdin = strings.NewReader(prompt)
	// CLAUDECODE prevents nested Claude processes; removing only that marker does
	// not alter credentials, provider settings, or campaign-relevant environment.
	command.Env = withoutEnvironment(os.Environ(), "CLAUDECODE")
	if arm.Provider == ProviderFixture {
		command.Env = append(command.Env, "SWE_EXPLORE_FIXTURE_MODE="+string(arm.Mode))
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now().UTC()
	runErr := command.Run()
	elapsed := time.Since(started)
	if measured.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("arm %q timed out after %s", arm.ID, elapsed.Round(time.Millisecond))
	}
	exitCode := 0
	if runErr != nil {
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return Result{}, fmt.Errorf("start arm %q: %w", arm.ID, runErr)
		}
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create artifact directory: %w", err)
	}
	rawPath := filepath.Join(outputDir, arm.ID+".trace.jsonl")
	if err := WriteFileAtomic(rawPath, stdout.Bytes(), 0o600); err != nil {
		return Result{}, fmt.Errorf("write raw trace: %w", err)
	}
	if exitCode != 0 {
		return Result{}, fmt.Errorf("arm %q exited %d: %s", arm.ID, exitCode, strings.TrimSpace(stderr.String()))
	}
	parsed, err := ParseTrace(arm.Provider, stdout.Bytes())
	if err != nil {
		return Result{}, err
	}
	if arm.Mode == ModeNative && len(parsed.LCTKToolCalls) != 0 {
		return Result{}, fmt.Errorf("native arm %q called LCTK", arm.ID)
	}
	if arm.Mode == ModeLCTK && len(parsed.LCTKToolCalls) == 0 {
		return Result{}, fmt.Errorf("LCTK arm %q produced no LCTK tool-call evidence", arm.ID)
	}
	regions, err := ParseRegions(parsed.FinalMessage, config.Workspace.Root, config.Run.TopK)
	if err != nil {
		return Result{}, err
	}
	if err := VerifyRepository(ctx, config.Workspace.Root, instance.BaseCommit); err != nil {
		return Result{}, fmt.Errorf("agent mutated benchmark repository: %w", err)
	}
	return Result{
		SchemaVersion: SchemaVersion, InstanceID: instance.InstanceID, ArmID: arm.ID,
		Provider: arm.Provider, Mode: arm.Mode, Model: arm.Model, ClientVersion: clientVersion, BaseCommit: instance.BaseCommit,
		StartedAt: started.Format(time.RFC3339Nano), ElapsedMS: elapsed.Milliseconds(), ExitCode: exitCode,
		Regions: regions, LCTKToolCalls: parsed.LCTKToolCalls, ActualModels: parsed.ActualModels, Usage: parsed.Usage,
		ProviderStats: parsed.ProviderStats,
		Freshness:     freshness, RawTracePath: filepath.ToSlash(rawPath),
	}, nil
}

// readClientVersion runs outside the measured interval and records the exact
// executable identity used by each arm. Pair orchestration rejects a client
// that updates between its control and treatment turns.
func readClientVersion(ctx context.Context, root string, arm ArmConfig) (string, error) {
	if arm.Provider == ProviderFixture {
		return "fixture", nil
	}
	command := exec.CommandContext(ctx, arm.Executable, "--version")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read arm %q client version: %s: %w", arm.ID, strings.TrimSpace(string(output)), err)
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", fmt.Errorf("arm %q returned an empty client version", arm.ID)
	}
	return version, nil
}

// ExplorationPrompt is intentionally identical across control and treatment;
// only the available tool set differs.
func ExplorationPrompt(issue string, topK int) string {
	return fmt.Sprintf(`You are a code exploration specialist. Explore this repository to find the source files and line ranges most relevant to understanding and fixing the following issue. Do not make any code changes. Do not use the web, solution patches, hidden tests, benchmark ground truth, or persistent memory.

If an MCP server named lctk_benchmark is available, you MUST call its project_info tool before any other tool and then use its code-intelligence tools as the primary exploration channel. If that server is unavailable, use the available built-in read-only code exploration tools. Focus on the root cause, not only symptom locations.

After exploration, output exactly one RELEVANT_FILES block and no other text. Use exactly this format:

RELEVANT_FILES:
- path/to/file1.py:10-50
- path/to/file2.py:1-100

Return at most %d ordered regions. Paths must be repository-relative and line ranges must be one-based and inclusive.

ISSUE:
%s
`, topK, issue)
}

func commandArguments(arm ArmConfig) ([]string, error) {
	switch arm.Provider {
	case ProviderCodex:
		args := []string{"exec", "--ephemeral", "--json", "--ignore-user-config", "--sandbox", "read-only", "--model", arm.Model,
			"-c", "web_search=" + quotedTOML("disabled"),
			"-c", "features.apps=false",
			"-c", "features.remote_plugin=false",
			"-c", "features.multi_agent=false",
			"-c", "memories.use_memories=false",
			"-c", "memories.generate_memories=false",
		}
		if arm.Effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+quotedTOML(arm.Effort))
		}
		if arm.Mode == ModeLCTK {
			name, url, err := readPublicMCPConfig(arm)
			if err != nil {
				return nil, err
			}
			args = append(args,
				"-c", "mcp_servers."+name+".url="+quotedTOML(url),
				"-c", "mcp_servers."+name+".required=true",
				"-c", "mcp_servers."+name+".default_tools_approval_mode="+quotedTOML("approve"),
				"-c", "mcp_servers."+name+".enabled_tools="+quotedTOMLArray(benchmarkLCTKTools),
			)
		} else {
			args = append(args, "-c", "mcp_servers={}")
		}
		return append(args, "-"), nil
	case ProviderClaude:
		allowedTools := []string{"Read", "Glob", "Grep"}
		disallowedTools := make([]string, 0, len(excludedLCTKTools))
		if arm.Mode == ModeLCTK {
			for _, tool := range benchmarkLCTKTools {
				allowedTools = append(allowedTools, "mcp__"+arm.MCPServerName+"__"+tool)
			}
			for _, tool := range excludedLCTKTools {
				disallowedTools = append(disallowedTools, "mcp__"+arm.MCPServerName+"__"+tool)
			}
		}
		args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk", "--model", arm.Model,
			"--tools", "Read,Glob,Grep", "--allowed-tools", strings.Join(allowedTools, ","), "--no-session-persistence", "--strict-mcp-config"}
		if len(disallowedTools) != 0 {
			args = append(args, "--disallowed-tools", strings.Join(disallowedTools, ","))
		}
		if arm.Effort != "" {
			args = append(args, "--effort", arm.Effort)
		}
		if arm.Mode == ModeNative {
			args = append(args, "--mcp-config", `{"mcpServers":{}}`)
		} else {
			if _, _, err := readPublicMCPConfig(arm); err != nil {
				return nil, err
			}
			args = append(args, "--mcp-config", arm.MCPConfigPath)
		}
		return args, nil
	case ProviderFixture:
		return []string{"-test.run=TestFixtureAgentProcess", "--"}, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", arm.Provider)
	}
}

func readPublicMCPConfig(arm ArmConfig) (string, string, error) {
	if !safeMCPName.MatchString(arm.MCPServerName) {
		return "", "", fmt.Errorf("MCP server name %q is unsafe", arm.MCPServerName)
	}
	body, err := os.ReadFile(arm.MCPConfigPath)
	if err != nil {
		return "", "", fmt.Errorf("read MCP config: %w", err)
	}
	var document struct {
		Servers map[string]struct {
			Type    string            `json:"type,omitempty"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers,omitempty"`
		} `json:"mcpServers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return "", "", fmt.Errorf("decode MCP config: %w", err)
	}
	if len(document.Servers) != 1 {
		return "", "", errors.New("MCP config must contain exactly one server")
	}
	server, ok := document.Servers[arm.MCPServerName]
	if !ok {
		return "", "", fmt.Errorf("MCP config does not contain %q", arm.MCPServerName)
	}
	if len(server.Headers) != 0 {
		return "", "", errors.New("MCP config must not contain headers or copied credentials")
	}
	if !strings.HasPrefix(server.URL, "http://127.0.0.1:") {
		return "", "", errors.New("MCP server URL must use the loopback HTTP endpoint")
	}
	return arm.MCPServerName, server.URL, nil
}

func quotedTOML(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func quotedTOMLArray(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quotedTOML(value))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

func withoutEnvironment(values []string, key string) []string {
	prefix := strings.ToUpper(key) + "="
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if !strings.HasPrefix(strings.ToUpper(value), prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}
