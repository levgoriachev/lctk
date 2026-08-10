package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

func callFileOutline(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	return callText(t, session, "file_outline", arguments)
}

// TestFileOutlineAnswersWithAHeaderAndDeclarationLines pins the text shape: a
// one-line header, then indented "kind name  A-B  signature" lines a caller can
// jump from.
func TestFileOutlineAnswersWithAHeaderAndDeclarationLines(t *testing.T) {
	_, _, session := searchFixture(t)

	text, result := callFileOutline(t, session, map[string]any{"path": "internal/a.go"})
	if result.IsError {
		t.Fatalf("file_outline refused: %s", resultText(result))
	}
	want := "internal/a.go — go, 9 lines\nfunction Needle  7-9  func Needle() {"
	if text != want {
		t.Errorf("outline = %q, want %q", text, want)
	}
}

func TestFileOutlineSendsOnlyThePathAndNothingThatCouldRedirectScope(t *testing.T) {
	_, service, session := searchFixture(t)

	text, result := callFileOutline(t, session, map[string]any{
		"path":            "internal/a.go",
		"project_id":      "beta-bbbbbbbb",
		"repository_root": "/somewhere/else",
	})
	if result.IsError {
		t.Fatalf("scope-like arguments caused a refusal: %s", resultText(result))
	}
	if !strings.Contains(text, "internal/a.go") {
		t.Errorf("the routed project did not answer:\n%s", text)
	}
	if len(service.outlineRequests) != 1 || service.outlineRequests[0] != "internal/a.go" {
		t.Errorf("the service was asked about %v", service.outlineRequests)
	}
}

// TestFileOutlineIsBoundedByDefault pins the ADR-0031 bounds: depth and entry
// limits with one explicit widening footer, so an 85,000-character outline can
// never answer a 30-line question again.
func TestFileOutlineIsBoundedByDefault(t *testing.T) {
	_, service, session := searchFixture(t)
	service.outlineResponse = `{"path":"internal/big.go","language":"go","bytes":900,"lines":90,` +
		`"digest":"big1","schema_version":1,"symbols":[` +
		`{"name":"Alpha","kind":"function","start_line":1,"end_line":10,"depth":0},` +
		`{"name":"nested","kind":"function","start_line":2,"end_line":4,"depth":1},` +
		`{"name":"deep","kind":"variable","start_line":3,"end_line":3,"depth":2},` +
		`{"name":"Beta","kind":"function","start_line":12,"end_line":20,"depth":0},` +
		`{"name":"Gamma","kind":"function","start_line":22,"end_line":30,"depth":0}],` +
		`"syntax":{"reported":true,"valid":true}}`

	// Default depth is 2, so the depth-2 declaration is omitted; max_symbols 3
	// then drops the last top-level one too. The footer counts both.
	text, result := callFileOutline(t, session, map[string]any{"path": "internal/big.go", "max_symbols": 3})
	if result.IsError {
		t.Fatalf("file_outline refused: %s", resultText(result))
	}
	want := "internal/big.go — go, 90 lines\n" +
		"function Alpha  1-10\n" +
		"  function nested  2-4\n" +
		"function Beta  12-20\n" +
		"[2 more declarations omitted — raise max_depth or max_symbols]"
	if text != want {
		t.Errorf("outline = %q, want %q", text, want)
	}

	// Widening is explicit: deeper and wider bounds show everything, and the
	// footer disappears when nothing was omitted.
	text, _ = callFileOutline(t, session, map[string]any{"path": "internal/big.go", "max_depth": 3})
	if !strings.Contains(text, "    variable deep  3-3") {
		t.Errorf("raising max_depth did not widen the outline:\n%s", text)
	}
	if strings.Contains(text, "omitted") {
		t.Errorf("a complete outline still claims omissions:\n%s", text)
	}
}

// TestFileOutlineWarnsWhenTheFileDoesNotParse: the parse verdict is a leading
// warning line only when there is something to warn about.
func TestFileOutlineWarnsWhenTheFileDoesNotParse(t *testing.T) {
	_, service, session := searchFixture(t)
	service.outlineResponse = `{"path":"internal/broken.go","language":"go","bytes":50,"lines":5,` +
		`"digest":"bad1","schema_version":1,"symbols":[],` +
		`"syntax":{"reported":true,"valid":false,"errors":2}}`

	text, result := callFileOutline(t, session, map[string]any{"path": "internal/broken.go"})
	if result.IsError {
		t.Fatalf("file_outline refused: %s", resultText(result))
	}
	if !strings.Contains(text, "warning: the file does not parse cleanly (2 broken regions)") {
		t.Errorf("no parse warning:\n%s", text)
	}
	if !strings.Contains(text, "no declarations") {
		t.Errorf("an empty outline does not say so:\n%s", text)
	}
}

// A project whose container predates the symbol layer reports no languages. The
// capability must not be advertised then: a caller that trusts project_info
// would otherwise learn the truth from a refusal.
func TestFileOutlineIsAdvertisedOnlyWhenTheServiceCanActuallyOutline(t *testing.T) {
	f, service, session := searchFixture(t)

	info := callProjectInfo(t, session, nil)
	capabilities := capabilitiesLine(t, info)
	if !strings.Contains(capabilities, "file_outline") {
		t.Errorf("capabilities = %q, want file_outline", capabilities)
	}
	if !strings.Contains(info, "outline languages: go") {
		t.Errorf("outline languages missing:\n%s", info)
	}

	service.outlineLanguages = nil
	session = f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	info = callProjectInfo(t, session, nil)
	capabilities = capabilitiesLine(t, info)
	for _, absent := range []string{"file_outline", "find_symbol"} {
		if strings.Contains(capabilities, absent) {
			t.Errorf("capabilities = %q; a service with no grammars must advertise no %s", capabilities, absent)
		}
	}
	if strings.Contains(info, "outline languages:") {
		t.Errorf("outline languages reported with none available:\n%s", info)
	}
}

func TestAFileOutlineRefusalCarriesWhatToDoInstead(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
		want    []string
	}{
		{
			// An empty outline would read as "this file declares nothing", so the
			// refusal has to be explicit and has to name a tool that does work.
			name:    "an unsupported language",
			code:    codeintel.CodeLanguageUnsupported,
			message: "Outlines are not available for \".md\"; this build understands go.",
			want:    []string{"LANGUAGE_UNSUPPORTED", "exact_search", "project_info"},
		},
		{
			// A file plainly present in the checkout can be absent here, so the
			// advice names the reason rather than inviting the same request again.
			name:    "a path the project does not hold",
			code:    codeintel.CodeFileNotFound,
			message: "There is no such file in this project: internal/gone.go",
			want:    []string{"FILE_NOT_FOUND", "ignore rules"},
		},
		{
			name:    "an escaping path",
			code:    codeintel.CodeInvalidPath,
			message: "the path must stay inside the project: \"../outside.go\"",
			want:    []string{"INVALID_PATH", "Correct the request"},
		},
		{
			name:    "a file too costly to parse",
			code:    codeintel.CodeParseIncomplete,
			message: "The file was not fully parsed within 5s.",
			want:    []string{"PARSE_INCOMPLETE", "search within it"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, true, "alpha-aaaaaaaa")
			service := newFakeService(t)
			service.outlineStatus = http.StatusBadRequest
			service.outlineResponse = `{"error":{"code":"` + c.code +
				`","message":` + quote(c.message) + `,"retryable":false}}`
			f.service["alpha-aaaaaaaa"] = service.address()

			session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
			_, result := callFileOutline(t, session, map[string]any{"path": "x"})
			if result == nil || !result.IsError {
				t.Fatal("the refusal did not reach the caller as an error")
			}
			text := resultText(result)
			for _, want := range c.want {
				if !strings.Contains(text, want) {
					t.Errorf("the refusal reads %q, which does not mention %q", text, want)
				}
			}
		})
	}
}

func quote(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}
