package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

func callFindSymbol(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	return callText(t, session, "find_symbol", arguments)
}

// TestFindSymbolAnswersDefinitionsThenReferences pins the merged lookup's text
// shape: sectioned grep lines, definitions first, with the partial-coverage
// footer that keeps a bounded answer from reading as a complete one.
func TestFindSymbolAnswersDefinitionsThenReferences(t *testing.T) {
	f, service, session := searchFixture(t)

	text, result := callFindSymbol(t, session, map[string]any{"name": "Needle"})
	if result.IsError {
		t.Fatalf("find_symbol refused: %s", resultText(result))
	}
	want := "definitions (1):\n" +
		"internal/a.go:7: func Needle() {}  [function]\n" +
		"\n" +
		"references (1):\n" +
		"internal/a.go:12: Needle()\n" +
		"[skipped: 1 unsupported-language, 0 unreadable files]"
	if text != want {
		t.Errorf("answer = %q, want %q", text, want)
	}
	if len(service.locateRequests) != 1 {
		t.Fatalf("locate requests = %d", len(service.locateRequests))
	}
	// The lookup parses what the index offers, so the same pre-answer flush a
	// search performs must have happened before the backend was asked.
	if len(f.flushed) == 0 {
		t.Fatal("find_symbol answered without flushing pending changes first")
	}
}

func TestFindSymbolAsksOnlyForDeclarationsWhenToldTo(t *testing.T) {
	_, service, session := searchFixture(t)

	callFindSymbol(t, session, map[string]any{"name": "Needle", "include": "definitions"})

	if len(service.locateRequests) != 1 {
		t.Fatalf("requests = %d", len(service.locateRequests))
	}
	sent := service.locateRequests[0]
	if sent["name"] != "Needle" {
		t.Errorf("name = %v", sent["name"])
	}
	// The narrowing has to actually be on the wire: otherwise definitions-only
	// is the full lookup with a different description.
	if sent["declarations_only"] != true {
		t.Errorf("declarations_only = %v, want true: %+v", sent["declarations_only"], sent)
	}
}

func TestFindSymbolCanAnswerReferencesOnly(t *testing.T) {
	_, _, session := searchFixture(t)

	text, result := callFindSymbol(t, session, map[string]any{"name": "Needle", "include": "references"})
	if result.IsError {
		t.Fatalf("find_symbol refused: %s", resultText(result))
	}
	if strings.Contains(text, "definitions") {
		t.Errorf("a references-only answer still lists definitions:\n%s", text)
	}
	if !strings.Contains(text, "references (1):\ninternal/a.go:12: Needle()") {
		t.Errorf("the references section is missing:\n%s", text)
	}
}

func TestFindSymbolRefusesAnUnknownIncludeValue(t *testing.T) {
	_, service, session := searchFixture(t)

	_, result := callFindSymbol(t, session, map[string]any{"name": "Needle", "include": "callers"})
	if !result.IsError {
		t.Fatal("an unknown include value was accepted")
	}
	text := resultText(result)
	if !strings.Contains(text, "INVALID_PATTERN") || !strings.Contains(text, "definitions, or references") {
		t.Errorf("the refusal does not name the valid values: %q", text)
	}
	if len(service.locateRequests) != 0 {
		t.Errorf("an invalid request still reached the backend: %+v", service.locateRequests)
	}
}

func TestASymbolLookupSendsNothingThatCouldRedirectScope(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa", "beta-bbbbbbbb")
	alpha := newFakeService(t)
	beta := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = alpha.address()
	f.service["beta-bbbbbbbb"] = beta.address()

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text, result := callFindSymbol(t, session, map[string]any{
		"name":       "Needle",
		"project_id": "beta-bbbbbbbb",
	})
	if result.IsError {
		t.Fatalf("a scope-like argument caused a refusal: %s", resultText(result))
	}
	if !strings.Contains(text, "internal/a.go:7:") {
		t.Errorf("the routed project did not answer:\n%s", text)
	}
	if len(beta.locateRequests) != 0 {
		t.Errorf("the other project's service was contacted: %+v", beta.locateRequests)
	}
	for _, forbidden := range []string{"project_id", "repository_root", "path"} {
		if _, present := alpha.locateRequests[0][forbidden]; present {
			t.Errorf("%q reached the backend: %+v", forbidden, alpha.locateRequests[0])
		}
	}
}

func TestAMaxFilesBoundReachesTheService(t *testing.T) {
	// The work is proportional to this, so a caller that sets it must actually get
	// it rather than a silently unbounded lookup.
	_, service, session := searchFixture(t)

	callFindSymbol(t, session, map[string]any{"name": "Needle", "max_files": 5})

	if got := service.locateRequests[0]["max_files"]; got != float64(5) {
		t.Errorf("max_files = %v, want 5", got)
	}
}

// TestATruncatedLookupSaysSoRatherThanImplyingCompleteness: an empty tail must
// never read as an absence of references.
func TestATruncatedLookupSaysSoRatherThanImplyingCompleteness(t *testing.T) {
	_, service, session := searchFixture(t)
	service.locateResponse = `{"name":"err","generation":4,"files":[],` +
		`"occurrences":0,"declarations":0,"files_considered":200,"files_truncated":true}`

	text, result := callFindSymbol(t, session, map[string]any{"name": "err"})
	if result.IsError {
		t.Fatalf("find_symbol refused: %s", resultText(result))
	}
	if !strings.Contains(text, "no occurrences of err") {
		t.Errorf("an empty answer does not say so:\n%s", text)
	}
	if !strings.Contains(text, "[only 200 files examined — absence of a reference proves nothing; raise max_files]") {
		t.Errorf("a truncated lookup does not warn about its coverage:\n%s", text)
	}
}

// TestAnAmbiguousNameIsFlagged: name matching is honest about its precision when
// several declarations share the name.
func TestAnAmbiguousNameIsFlagged(t *testing.T) {
	_, service, session := searchFixture(t)
	service.locateResponse = `{"name":"Run","generation":4,"files":[` +
		`{"path":"a.go","language":"go","occurrences":[` +
		`{"line":3,"declaration":true,"kind":"function","preview":"func Run() {}"}]},` +
		`{"path":"b.go","language":"go","occurrences":[` +
		`{"line":9,"declaration":true,"kind":"method","preview":"func (s S) Run() {}"}]}],` +
		`"occurrences":2,"declarations":2,"files_considered":2}`

	text, result := callFindSymbol(t, session, map[string]any{"name": "Run"})
	if result.IsError {
		t.Fatalf("find_symbol refused: %s", resultText(result))
	}
	if !strings.Contains(text, "[2 declarations share this name — matching is by name, verify which one a use refers to]") {
		t.Errorf("the ambiguity did not reach the caller:\n%s", text)
	}
}

// TestAResultBoundOmitsRatherThanLies: past max_results the answer says how many
// locations were left out and how to widen.
func TestAResultBoundOmitsRatherThanLies(t *testing.T) {
	_, _, session := searchFixture(t)

	text, result := callFindSymbol(t, session, map[string]any{"name": "Needle", "max_results": 1})
	if result.IsError {
		t.Fatalf("find_symbol refused: %s", resultText(result))
	}
	if !strings.Contains(text, "[1 more locations omitted — raise max_results]") {
		t.Errorf("the omission did not reach the caller:\n%s", text)
	}
}

func TestASymbolLookupRefusalCarriesWhatToDo(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	service.locateStatus = http.StatusBadRequest
	service.locateResponse = `{"error":{"code":"` + codeintel.CodeInvalidPattern +
		`","message":"\"Needle|Other\" is not an identifier; a name may hold letters, digits, underscores, and dollars","retryable":false}}`
	f.service["alpha-aaaaaaaa"] = service.address()

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	_, result := callFindSymbol(t, session, map[string]any{"name": "Needle|Other"})
	if result == nil || !result.IsError {
		t.Fatal("the refusal did not reach the caller as an error")
	}
	text := resultText(result)
	for _, want := range []string{"INVALID_PATTERN", "is not an identifier", "Correct the request"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal reads %q, which does not mention %q", text, want)
		}
	}
}

func TestTheSymbolCapabilityIsAdvertisedWithTheOutlineOne(t *testing.T) {
	// Both come from the same engine, so a service that cannot outline cannot
	// answer a lookup either, and advertising one without the other would send a
	// caller to discover that by refusal.
	f, service, session := searchFixture(t)

	capabilities := capabilitiesLine(t, callProjectInfo(t, session, nil))
	for _, tool := range []string{"file_outline", "find_symbol"} {
		if !strings.Contains(capabilities, tool) {
			t.Errorf("capabilities = %q, want %s", capabilities, tool)
		}
	}

	service.outlineLanguages = nil
	session = f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	capabilities = capabilitiesLine(t, callProjectInfo(t, session, nil))
	for _, tool := range []string{"file_outline", "find_symbol"} {
		if strings.Contains(capabilities, tool) {
			t.Errorf("capabilities = %q; a service with no grammars must advertise no %s", capabilities, tool)
		}
	}
}
