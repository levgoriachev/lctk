package sweexplore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRegionsRequiresExactValidBlock(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	regions, err := ParseRegions("RELEVANT_FILES:\n- main.go:2-3\n", root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 || regions[0] != (Region{Path: "main.go", Start: 2, End: 3}) {
		t.Fatalf("regions = %+v", regions)
	}
	for _, invalid := range []string{
		"main.go:2-3",
		"RELEVANT_FILES:\n- ../main.go:1-1\n",
		"RELEVANT_FILES:\n- main.go:1-1\n- main.go:2-2\n",
	} {
		if _, err := ParseRegions(invalid, root, 1); err == nil {
			t.Fatalf("ParseRegions(%q) succeeded", invalid)
		}
	}
}

func TestParseRegionsPreservesOfficialOutOfFileNoise(t *testing.T) {
	regions, err := ParseRegions("RELEVANT_FILES:\n- missing.go:458-480\n", t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := Region{Path: "missing.go", Start: 458, End: 480}
	if len(regions) != 1 || regions[0] != want {
		t.Fatalf("regions = %+v, want %+v", regions, want)
	}
}

func TestParseProviderTraces(t *testing.T) {
	codex := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"lctk_benchmark","tool":"project_info","result":null,"error":{"message":"user cancelled MCP tool call"},"status":"failed"}}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"lctk_benchmark","tool":"exact_search","result":{"content":[]},"error":null,"status":"completed"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"RELEVANT_FILES:\n- main.go:1-1"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":2}}`,
	}, "\n")
	parsed, err := ParseTrace(ProviderCodex, []byte(codex))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.LCTKToolCalls) != 1 || parsed.Usage.InputTokens != 10 || parsed.Usage.ReasoningOutputTokens != 2 {
		t.Fatalf("Codex trace = %+v", parsed)
	}
	claude := strings.Join([]string{
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"tool_use","name":"mcp__lctk-self__repository_map"}]}}`,
		`{"type":"result","result":"RELEVANT_FILES:\n- main.go:1-1","total_cost_usd":0.125,"duration_ms":900,"duration_api_ms":700,"num_turns":2,"usage":{"input_tokens":12,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"output_tokens":4}}`,
	}, "\n")
	parsed, err = ParseTrace(ProviderClaude, []byte(claude))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.LCTKToolCalls) != 1 || len(parsed.ActualModels) != 1 || parsed.ActualModels[0] != "claude-sonnet-4-6" || parsed.Usage.CachedInputTokens != 5 || parsed.Usage.CacheCreationInputTokens != 6 || parsed.Usage.OutputTokens != 4 || parsed.ProviderStats.ReportedCostUSD != 0.125 || parsed.ProviderStats.APIDurationMS != 700 || parsed.ProviderStats.Turns != 2 {
		t.Fatalf("Claude trace = %+v", parsed)
	}
}
