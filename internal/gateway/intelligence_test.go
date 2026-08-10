package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/gitinfo"
)

// intelligenceService stands in for the graph and memory endpoints, recording
// every request so a test can assert what crossed the adapter boundary.
func intelligenceService(t *testing.T) (map[string][]map[string]any, string) {
	t.Helper()
	requests := map[string][]map[string]any{}
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			_, _ = io.WriteString(w, `{"ready":true,"generation":7,"graph":{"ready":true,"generation":7,"precision":"name_match","freshness":"fresh"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests[r.URL.Path] = append(requests[r.URL.Path], body)
		switch r.URL.Path {
		case "/graph/callers":
			_, _ = io.WriteString(w, `{"name":"Work","direction":"callers","matches":[{"path":"main.go","caller":"Run","callee":"Work","line":3,"column":2}],"total":1,"generation":7,"precision":"name_match"}`)
		case "/graph/callees":
			_, _ = io.WriteString(w, `{"name":"Run","direction":"callees","matches":[{"path":"main.go","caller":"Run","callee":"Work","line":3,"column":2}],"total":1,"generation":7,"precision":"name_match"}`)
		case "/graph/dependency-path":
			_, _ = io.WriteString(w, `{"from":"main.go","to":"dep.go","path":["main.go","dep.go"],"found":true,"max_depth":32,"generation":7,"precision":"name_match"}`)
		case "/graph/impact":
			_, _ = io.WriteString(w, `{"target":"Work","files":["main.go"],"calls":[],"total":1,"generation":7,"precision":"name_match"}`)
		case "/graph/repository-map":
			_, _ = io.WriteString(w, `{"map":"main.go\n  function Run\n","characters":25,"max_chars":512,"file_count":2,"node_count":2,"generation":7,"precision":"name_match"}`)
		case "/memory/get":
			_, _ = io.WriteString(w, memoryJSON("current"))
		case "/memory/search":
			_, _ = io.WriteString(w, `{"matches":[{"record":`+memoryJSON("old")+`}],"total":1,"modes":["semantic","lexical"],"model":"test","dimensions":16}`)
		case "/memory/put":
			commit, _ := body["source_commit"].(string)
			_, _ = io.WriteString(w, memoryJSON(commit))
		case "/memory/delete":
			_, _ = io.WriteString(w, `{"deleted":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(service.Close)
	return requests, strings.TrimPrefix(service.URL, "http://")
}

// intelligenceFixture wires the stand-in service into a fresh, watched project
// so answers carry no staleness framing.
func intelligenceFixture(t *testing.T, extended bool) (map[string][]map[string]any, *mcp.ClientSession) {
	t.Helper()
	requests, address := intelligenceService(t)
	f := newConfiguredFixture(t, fixtureConfig{requireRunning: true, extendedTools: extended}, "alpha-aaaaaaaa")
	f.service["alpha-aaaaaaaa"] = address
	f.git = &fakeGit{status: gitinfo.Status{Repository: true, Branch: "main", Commit: "current"}}
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true}
	return requests, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
}

// TestDefaultSurfaceServesMapAndMemory drives the five default intelligence
// actions through a real protocol client and pins their text answers.
func TestDefaultSurfaceServesMapAndMemory(t *testing.T) {
	requests, session := intelligenceFixture(t, false)

	tests := []struct {
		name      string
		arguments map[string]any
		path      string
		want      []string
		exact     string
	}{
		{
			name: "repository_map", arguments: map[string]any{"max_chars": 512},
			path:  "/graph/repository-map",
			exact: "main.go\n  function Run",
		},
		{
			name: "memory_get", arguments: map[string]any{"key": "architecture/retry"},
			path: "/memory/get",
			want: []string{"key: architecture/retry (decision, revision 1, confidence 0.90)", "Retry."},
		},
		{
			name: "memory_search", arguments: map[string]any{"query": "retry"},
			path: "/memory/search",
			want: []string{
				"architecture/retry (decision, revision 1, confidence 0.90): Retry.",
				// The searched record was written against another commit, and the
				// answer says so by key.
				"[written against an older commit: architecture/retry]",
			},
		},
		{
			name: "memory_put", arguments: map[string]any{
				"key": "architecture/retry", "kind": "decision", "content": "Retry.", "confidence": 0.9,
			},
			path:  "/memory/put",
			exact: "stored architecture/retry at revision 1",
		},
		{
			name: "memory_delete", arguments: map[string]any{"key": "architecture/retry", "expected_revision": 1},
			path:  "/memory/delete",
			exact: "deleted architecture/retry",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, result := callText(t, session, test.name, test.arguments)
			if result.IsError {
				t.Fatalf("%s refused: %s", test.name, resultText(result))
			}
			if test.exact != "" && text != test.exact {
				t.Errorf("%s = %q, want %q", test.name, text, test.exact)
			}
			for _, want := range test.want {
				if !strings.Contains(text, want) {
					t.Errorf("%s = %q, want it to contain %q", test.name, text, want)
				}
			}
			if len(requests[test.path]) != 1 {
				t.Fatalf("requests to %s = %d", test.path, len(requests[test.path]))
			}
		})
	}

	// A record written without a commit is stamped with the project's current
	// one, so staleness stays detectable later.
	if requests["/memory/put"][0]["source_commit"] != "current" {
		t.Fatalf("memory_put source_commit = %v, want current", requests["/memory/put"][0]["source_commit"])
	}

	// The record memory_get returned matches the current commit, so no
	// staleness framing may appear (ADR-0031: status only when it changes the
	// next action).
	text, _ := callText(t, session, "memory_get", map[string]any{"key": "architecture/retry"})
	if strings.Contains(text, "older commit") {
		t.Errorf("a current record was framed as stale:\n%s", text)
	}
}

// TestGraphToolsAreNotOnTheDefaultSurface: the default catalog stops at the five
// exploration tools plus memory; a graph call must fail loudly, not quietly
// serve a tool the catalog does not advertise.
func TestGraphToolsAreNotOnTheDefaultSurface(t *testing.T) {
	_, session := intelligenceFixture(t, false)

	for _, name := range []string{"callers_find", "callees_find", "dependency_path", "impact_analyze", "code_search_semantic"} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{
			"name": "Work", "from": "main.go", "to": "dep.go", "target": "Work", "query": "work",
		}})
		if err == nil && (result == nil || !result.IsError) {
			t.Errorf("%s answered on the default surface", name)
		}
	}
}

// TestExtendedSurfaceServesGraphTools drives the four graph tools through MCP
// with ExtendedTools on, pinning their grep-shaped answers.
func TestExtendedSurfaceServesGraphTools(t *testing.T) {
	requests, session := intelligenceFixture(t, true)

	tests := []struct {
		name      string
		arguments map[string]any
		path      string
		want      string
	}{
		// The scope-like argument on the first call is dropped, not refused.
		{"callers_find", map[string]any{"name": "Work", "project_id": "wrong"}, "/graph/callers", "main.go:3: Run"},
		{"callees_find", map[string]any{"name": "Run"}, "/graph/callees", "main.go:3: Work"},
		{"dependency_path", map[string]any{"from": "main.go", "to": "dep.go"}, "/graph/dependency-path", "main.go\n -> dep.go"},
		{"impact_analyze", map[string]any{"target": "Work"}, "/graph/impact", "importers (1):\nmain.go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			text, result := callText(t, session, test.name, test.arguments)
			if result.IsError {
				t.Fatalf("%s refused: %s", test.name, resultText(result))
			}
			if text != test.want {
				t.Errorf("%s = %q, want %q", test.name, text, test.want)
			}
			if len(requests[test.path]) != 1 {
				t.Fatalf("requests to %s = %d", test.path, len(requests[test.path]))
			}
			if _, present := requests[test.path][0]["project_id"]; present {
				t.Errorf("a scope-like argument crossed the adapter boundary: %+v", requests[test.path][0])
			}
		})
	}
}

// TestMemoryGetSaysWhenTheRecordIsFromAnotherCommit: the one staleness line a
// memory answer may carry appears exactly when the commits differ.
func TestMemoryGetSaysWhenTheRecordIsFromAnotherCommit(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/memory/get" {
			_, _ = io.WriteString(w, memoryJSON("old"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(service.Close)

	f := newFixture(t, true, "alpha-aaaaaaaa")
	f.service["alpha-aaaaaaaa"] = strings.TrimPrefix(service.URL, "http://")
	f.git = &fakeGit{status: gitinfo.Status{Repository: true, Branch: "main", Commit: "current"}}
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, result := callText(t, session, "memory_get", map[string]any{"key": "architecture/retry"})
	if result.IsError {
		t.Fatalf("memory_get refused: %s", resultText(result))
	}
	if !strings.Contains(text, "[written against an older commit — verify before relying on it]") {
		t.Errorf("a stale record was not framed as such:\n%s", text)
	}
}

func memoryJSON(commit string) string {
	return `{"key":"architecture/retry","kind":"decision","content":"Retry.","confidence":0.9,` +
		`"provenance":["docs/adr/retry.md"],"source_commit":"` + commit + `","revision":1,` +
		`"created_at":"2026-08-04T00:00:00Z","updated_at":"2026-08-04T00:00:00Z","review_due":false,"low_confidence":false}`
}
