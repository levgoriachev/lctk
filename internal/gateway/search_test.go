package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeService stands in for a project's code-intelligence container. It records
// what the gateway sent, so a test can assert what actually crosses the adapter
// boundary rather than only what comes back.
type fakeService struct {
	server   *httptest.Server
	requests []map[string]any
	response string
	status   int
	// outlineRequests records the paths asked about, so a test can assert what
	// crossed the boundary rather than only what came back.
	outlineRequests []string
	outlineResponse string
	outlineStatus   int
	// outlineLanguages is what /status advertises. Empty stands for a project whose
	// container predates the symbol layer, which is a case the gateway has to
	// handle rather than assume away.
	outlineLanguages []string
	// locateRequests records what a symbol lookup sent, so a test can assert what
	// crossed the boundary rather than only what came back.
	locateRequests []map[string]any
	locateResponse string
	locateStatus   int
	// semanticRequests records conceptual queries independently from exact search.
	semanticRequests []map[string]any
	semanticResponse string
	semanticStatus   int
	semanticReady    bool
}

func newFakeService(t *testing.T) *fakeService {
	t.Helper()
	service := &fakeService{
		status:           http.StatusOK,
		outlineStatus:    http.StatusOK,
		locateStatus:     http.StatusOK,
		semanticStatus:   http.StatusOK,
		outlineLanguages: []string{"go"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /locate", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		service.locateRequests = append(service.locateRequests, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(service.locateStatus)
		if service.locateResponse != "" {
			_, _ = io.WriteString(w, service.locateResponse)
			return
		}
		_, _ = io.WriteString(w, `{"name":"Needle","generation":4,`+
			`"indexed_at":"2026-08-01T11:00:00Z","files":[`+
			`{"path":"internal/a.go","language":"go","digest":"abc","parsed":true,"syntax_reported":true,`+
			`"declarations":1,"occurrences":[`+
			`{"line":7,"column":6,"start_byte":40,"end_byte":46,"declaration":true,"kind":"function",`+
			`"preview":"func Needle() {}"},`+
			`{"line":12,"column":9,"start_byte":90,"end_byte":96,"container":"Other",`+
			`"preview":"\tNeedle()"}]}],`+
			`"occurrences":2,"declarations":1,"files_considered":3,"skipped_unsupported":1}`)
	})
	mux.HandleFunc("POST /outline", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		service.outlineRequests = append(service.outlineRequests, body.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(service.outlineStatus)
		if service.outlineResponse != "" {
			_, _ = io.WriteString(w, service.outlineResponse)
			return
		}
		_, _ = io.WriteString(w, `{"path":"internal/a.go","language":"go","bytes":120,"lines":9,`+
			`"digest":"abc123","schema_version":1,`+
			`"symbols":[{"name":"Needle","kind":"function","start_line":7,"end_line":9,`+
			`"start_byte":40,"end_byte":118,"depth":0,"signature":"func Needle() {"}],`+
			`"syntax":{"reported":true,"valid":true}}`)
	})
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		service.requests = append(service.requests, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(service.status)
		if service.response != "" {
			_, _ = io.WriteString(w, service.response)
			return
		}
		_, _ = io.WriteString(w, `{"matches":[{"path":"internal/a.go","line":7,"column":3,`+
			`"preview":"func Needle() {}","match":"Needle"}],`+
			`"total":1,"truncated":false,"generation":4,`+
			`"indexed_at":"2026-08-01T11:00:00Z","file_count":42}`)
	})
	mux.HandleFunc("POST /semantic/search", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		service.semanticRequests = append(service.semanticRequests, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(service.semanticStatus)
		if service.semanticResponse != "" {
			_, _ = io.WriteString(w, service.semanticResponse)
			return
		}
		_, _ = io.WriteString(w, `{"matches":[{"path":"internal/retry.go","language":"go",`+
			`"chunk_precision":"syntax","anchor":"function:Retry","start_line":7,"end_line":12,`+
			`"preview":"func Retry() { backoff() }","vector_score":0.91,"lexical_score":2,`+
			`"hybrid_score":0.032,"vector_rank":1,"lexical_rank":1}],"total":1,"truncated":false,`+
			`"generation":4,"exact_generation":4,"freshness":"fresh",`+
			`"model":"nomic-test","dimensions":768}`)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		languages, _ := json.Marshal(service.outlineLanguages)
		semantic := ""
		if service.semanticReady {
			semantic = `,"semantic":{"ready":true,"indexing":false,"generation":4,` +
				`"file_count":42,"chunk_count":80,"model":"nomic-test","dimensions":768,"freshness":"fresh"}`
		}
		_, _ = io.WriteString(w, `{"ready":true,"indexing":false,"generation":4,`+
			`"file_count":42,"indexed_at":"2026-08-01T11:00:00Z",`+
			`"outline_languages":`+string(languages)+semantic+`}`)
	})

	service.server = httptest.NewServer(mux)
	t.Cleanup(service.server.Close)
	return service
}

func (s *fakeService) address() string {
	return strings.TrimPrefix(s.server.URL, "http://")
}

func callExactSearch(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	return callText(t, session, "exact_search", arguments)
}

// searchFixture wires a watched, fresh project with a fake service, so answers
// carry no staleness framing and assertions can be exact.
func searchFixture(t *testing.T) (*fixture, *fakeService, *mcp.ClientSession) {
	t.Helper()
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true}
	return f, service, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
}

// TestExactSearchAnswersInGrepForm pins the ADR-0031 response contract: one
// grep-shaped line per hit, project-relative paths, and nothing else when the
// index is fresh — no envelope, no provenance, no duplicate encoding.
func TestExactSearchAnswersInGrepForm(t *testing.T) {
	_, _, session := searchFixture(t)

	text, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}
	if text != "internal/a.go:7: func Needle() {}" {
		t.Errorf("answer = %q, want the bare grep line", text)
	}
	if strings.HasPrefix(text, "/") {
		t.Error("a result path is absolute")
	}
}

func TestExactSearchScopeIgnoresModelSuppliedArguments(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa", "beta-bbbbbbbb")
	alpha := newFakeService(t)
	beta := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = alpha.address()
	f.service["beta-bbbbbbbb"] = beta.address()

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text, result := callExactSearch(t, session, map[string]any{
		"pattern":         "Needle",
		"project_id":      "beta-bbbbbbbb",
		"repository_root": "/etc",
		"path":            "/etc/passwd",
	})

	// Unknown arguments are dropped, not refused: the call still answers.
	if result.IsError {
		t.Fatalf("scope-like arguments caused a refusal: %s", resultText(result))
	}
	if !strings.Contains(text, "internal/a.go:7:") {
		t.Errorf("the routed project did not answer:\n%s", text)
	}
	if len(beta.requests) != 0 {
		t.Errorf("the other project's service was contacted: %+v", beta.requests)
	}
	if len(alpha.requests) != 1 {
		t.Fatalf("the routed service received %d requests", len(alpha.requests))
	}
	// The scope-like arguments must not cross the adapter boundary at all: the
	// backend has no business seeing a project name it could act on.
	for _, forbidden := range []string{"project_id", "repository_root", "path"} {
		if _, present := alpha.requests[0][forbidden]; present {
			t.Errorf("%q was forwarded to the backend: %+v", forbidden, alpha.requests[0])
		}
	}
}

func TestExactSearchForwardsTheQueryFaithfully(t *testing.T) {
	_, service, session := searchFixture(t)

	callExactSearch(t, session, map[string]any{
		"pattern":        `func \w+\(`,
		"mode":           "regex",
		"case_sensitive": true,
		"path_globs":     []string{"**/*.go"},
		"languages":      []string{"Go"},
		"limit":          25,
		"cursor":         "abc",
	})

	if len(service.requests) != 1 {
		t.Fatalf("requests = %d", len(service.requests))
	}
	sent := service.requests[0]
	if sent["pattern"] != `func \w+\(` || sent["mode"] != "regex" || sent["case_sensitive"] != true {
		t.Errorf("the query was altered in transit: %+v", sent)
	}
	if sent["cursor"] != "abc" || sent["limit"] != float64(25) {
		t.Errorf("pagination was altered in transit: %+v", sent)
	}
}

// TestExactSearchOutputModes pins the grep-familiar -l and -c shapes.
func TestExactSearchOutputModes(t *testing.T) {
	_, service, session := searchFixture(t)
	service.response = `{"matches":[` +
		`{"path":"internal/a.go","line":7,"column":3,"preview":"func Needle() {}","match":"Needle"},` +
		`{"path":"internal/a.go","line":9,"column":3,"preview":"Needle()","match":"Needle"},` +
		`{"path":"internal/b.go","line":2,"column":1,"preview":"Needle()","match":"Needle"}],` +
		`"total":3,"truncated":false,"generation":4,"indexed_at":"2026-08-01T11:00:00Z","file_count":42}`

	files, result := callExactSearch(t, session, map[string]any{"pattern": "Needle", "output_mode": "files_with_matches"})
	if result.IsError {
		t.Fatalf("files_with_matches refused: %s", resultText(result))
	}
	if files != "internal/a.go\ninternal/b.go" {
		t.Errorf("files_with_matches = %q", files)
	}

	counts, result := callExactSearch(t, session, map[string]any{"pattern": "Needle", "output_mode": "count"})
	if result.IsError {
		t.Fatalf("count refused: %s", resultText(result))
	}
	if counts != "internal/a.go: 2\ninternal/b.go: 1\ntotal: 3" {
		t.Errorf("count = %q", counts)
	}

	_, result = callExactSearch(t, session, map[string]any{"pattern": "Needle", "output_mode": "sideways"})
	if !result.IsError {
		t.Fatal("an unknown output_mode was accepted")
	}
	text := resultText(result)
	if !strings.Contains(text, "INVALID_PATTERN") || !strings.Contains(text, "files_with_matches") {
		t.Errorf("the refusal does not name the valid modes: %q", text)
	}
}

// TestExactSearchTruncationFooter: truncation is stated only when it happened,
// with the cursor that continues the page.
func TestExactSearchTruncationFooter(t *testing.T) {
	_, service, session := searchFixture(t)
	service.response = `{"matches":[{"path":"internal/a.go","line":7,"column":3,` +
		`"preview":"func Needle() {}","match":"Needle"}],` +
		`"total":120,"truncated":true,"next_cursor":"abc","generation":4,` +
		`"indexed_at":"2026-08-01T11:00:00Z","file_count":42}`

	text, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}
	want := "internal/a.go:7: func Needle() {}\n[truncated: 120 matches total; continue with cursor \"abc\"]"
	if text != want {
		t.Errorf("answer = %q, want %q", text, want)
	}
}

// TestExactSearchEmptyAnswerAndRegexHint: an empty result is one short line, and
// a literal pattern that looks like an escaped regex says the one useful thing.
func TestExactSearchEmptyAnswerAndRegexHint(t *testing.T) {
	_, service, session := searchFixture(t)
	service.response = `{"matches":[],"total":0,"truncated":false,"generation":4,` +
		`"indexed_at":"2026-08-01T11:00:00Z","file_count":42}`

	text, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}
	if text != "no matches" {
		t.Errorf("empty answer = %q, want a single short line", text)
	}

	text, _ = callExactSearch(t, session, map[string]any{"pattern": `func \w+\(`})
	want := `no matches (pattern contains regex syntax but mode is literal — retry with mode:"regex"?)`
	if text != want {
		t.Errorf("hinted answer = %q, want %q", text, want)
	}

	// The hint belongs to literal mode only: the same pattern searched as a
	// regex that found nothing is just an empty result.
	text, _ = callExactSearch(t, session, map[string]any{"pattern": `func \w+\(`, "mode": "regex"})
	if text != "no matches" {
		t.Errorf("regex-mode empty answer = %q", text)
	}
}

// TestExactSearchContextLinesComeFromTheHostTree: the -C-style context is read
// from the saved working tree on the host, after the flush, and is bounded.
func TestExactSearchContextLinesComeFromTheHostTree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "one\ntwo\nthree\nfour\nfive\n"
	if err := os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newConfiguredFixture(t, fixtureConfig{
		requireRunning: true,
		roots:          map[string]string{"alpha-aaaaaaaa": root},
	}, "alpha-aaaaaaaa")
	service := newFakeService(t)
	service.response = `{"matches":[{"path":"pkg/a.go","line":3,"column":1,` +
		`"preview":"three","match":"three"}],"total":1,"truncated":false,` +
		`"generation":4,"indexed_at":"2026-08-01T11:00:00Z","file_count":1}`
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true}
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	text, result := callExactSearch(t, session, map[string]any{"pattern": "three", "context_lines": 1})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}
	want := "pkg/a.go-2- two\npkg/a.go:3: three\npkg/a.go-4- four"
	if text != want {
		t.Errorf("answer = %q, want %q", text, want)
	}

	// An oversized request is clamped, not refused: the whole small file is the
	// most context it can produce.
	text, _ = callExactSearch(t, session, map[string]any{"pattern": "three", "context_lines": 99})
	for _, line := range []string{"pkg/a.go-1- one", "pkg/a.go-2- two", "pkg/a.go-4- four", "pkg/a.go-5- five"} {
		if !strings.Contains(text, line) {
			t.Errorf("clamped context is missing %q:\n%s", line, text)
		}
	}
}

func TestExactSearchSurfacesTypedBackendFailures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantCode  string
		wantRetry bool
	}{
		{
			name:      "index still building",
			status:    http.StatusServiceUnavailable,
			body:      `{"error":{"code":"INDEX_NOT_READY","message":"The project index has not been built yet.","retryable":true}}`,
			wantCode:  "INDEX_NOT_READY",
			wantRetry: true,
		},
		{
			name:     "bad pattern",
			status:   http.StatusBadRequest,
			body:     `{"error":{"code":"INVALID_PATTERN","message":"The regular expression is invalid.","retryable":false}}`,
			wantCode: "INVALID_PATTERN",
		},
		{
			name:     "stale cursor",
			status:   http.StatusBadRequest,
			body:     `{"error":{"code":"INVALID_CURSOR","message":"The cursor belongs to another generation.","retryable":false}}`,
			wantCode: "INVALID_CURSOR",
		},
		{
			name:     "corrupt index",
			status:   http.StatusInternalServerError,
			body:     `{"error":{"code":"INDEX_CORRUPT","message":"The persistent index cannot be opened.","retryable":false}}`,
			wantCode: "INDEX_CORRUPT",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, true, "alpha-aaaaaaaa")
			service := newFakeService(t)
			service.status = testCase.status
			service.response = testCase.body
			f.service["alpha-aaaaaaaa"] = service.address()

			session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
			_, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
			if result == nil || !result.IsError {
				t.Fatal("a backend failure was not reported as an error result")
			}

			text := resultText(result)
			if !strings.Contains(text, testCase.wantCode) {
				t.Errorf("the typed code is not visible to the caller: %q", text)
			}
			// A caller deciding whether to try again must be told, not left to
			// infer it from the wording.
			if testCase.wantRetry && !strings.Contains(text, "retryable") {
				t.Errorf("a retryable failure does not say so: %q", text)
			}
		})
	}
}

func TestExactSearchReportsAnUnreachableService(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	// An address nothing is listening on: the project is running but its service
	// is not answering, which is a different condition from a stopped project and
	// deserves a different answer.
	f.service["alpha-aaaaaaaa"] = "127.0.0.1:1"

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	_, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result == nil || !result.IsError {
		t.Fatal("an unreachable service was not reported as an error")
	}
	if text := resultText(result); !strings.Contains(text, "SERVICE_UNAVAILABLE") {
		t.Errorf("error text = %q", text)
	}
}

func TestExactSearchOnAProjectWithoutAPublishedService(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	// No published address at all, which is what a container created from an
	// older stack definition looks like.
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	_, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result == nil || !result.IsError {
		t.Fatal("a project without a service did not report an error")
	}
	text := resultText(result)
	if !strings.Contains(text, "SEARCH_UNAVAILABLE") {
		t.Errorf("error text = %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "restart") {
		t.Errorf("the error does not say how to fix it: %q", text)
	}
}

func TestProjectInfoReportsIndexStateAndOutlineCapability(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	// Without a published service there is no index to describe, and the
	// symbol tools must not be advertised: a caller should learn a capability
	// is absent by asking, not by a failed call.
	withoutService := callProjectInfo(t, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"]), nil)
	if !strings.Contains(withoutService, "index: unavailable") {
		t.Errorf("a project without a service did not say its index is unavailable:\n%s", withoutService)
	}
	capabilities := capabilitiesLine(t, withoutService)
	for _, absent := range []string{"exact_search", "file_outline", "find_symbol", "repository_map"} {
		if strings.Contains(capabilities, absent) {
			t.Errorf("%s was advertised on a project that cannot serve it: %q", absent, capabilities)
		}
	}
	// read_span reads the host working tree, so it stays available either way.
	if !strings.Contains(capabilities, "read_span") {
		t.Errorf("read_span missing from a serviceless project: %q", capabilities)
	}

	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true}
	withService := callProjectInfo(t, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"]), nil)

	capabilities = capabilitiesLine(t, withService)
	for _, want := range []string{"exact_search", "read_span", "file_outline", "find_symbol"} {
		if !strings.Contains(capabilities, want) {
			t.Errorf("capabilities = %q, want %s", capabilities, want)
		}
	}
	if !strings.Contains(withService, "index: generation 4, 42 files, indexed 2026-08-01T11:00:00Z — fresh") {
		t.Errorf("the index state did not reach the caller:\n%s", withService)
	}
	if !strings.Contains(withService, "outline languages: go") {
		t.Errorf("the outline languages did not reach the caller:\n%s", withService)
	}
}
