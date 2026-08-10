package codeintel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(strings.TrimPrefix(server.URL, "http://"))
}

func typedError(t *testing.T, err error) *Error {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error is not typed: %v", err)
	}
	return typed
}

func TestSearchTranslatesTheServiceResponseIntoTheStableShape(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"matches":[{"path":"a.go","line":1,"column":2,"preview":"p","match":"m"}],`+
			`"total":9,"truncated":true,"next_cursor":"c2","generation":7,`+
			`"indexed_at":"2026-08-01T10:00:00Z","file_count":12}`)
	}))

	response, err := client.Search(context.Background(), Request{Pattern: "m"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Matches) != 1 || response.Matches[0].Path != "a.go" {
		t.Errorf("matches = %+v", response.Matches)
	}
	if response.Total != 9 || !response.Truncated || response.NextCursor != "c2" {
		t.Errorf("response = %+v", response)
	}
	// Provenance is assembled here rather than trusted from the service, so the
	// backend name and schema version cannot drift with whatever answered.
	if response.Provenance.Backend != Backend || response.Provenance.SchemaVersion != SchemaVersion {
		t.Errorf("provenance = %+v", response.Provenance)
	}
	if response.Provenance.IndexGeneration != 7 || response.Provenance.FileCount != 12 {
		t.Errorf("provenance = %+v", response.Provenance)
	}
}

func TestSearchNeverReturnsANilMatchSlice(t *testing.T) {
	// A caller serializing this into a tool result should see an empty list, not
	// a null, because null reads to a model as "no answer" rather than "no
	// matches".
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"matches":null,"total":0,"generation":1,"file_count":0}`)
	}))

	response, err := client.Search(context.Background(), Request{Pattern: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Matches == nil {
		t.Error("matches is nil")
	}
}

func TestServiceErrorsKeepTheirCodeAndGainAnAction(t *testing.T) {
	cases := []struct {
		code       string
		status     int
		retryable  bool
		wantAction string
	}{
		{code: CodeIndexNotReady, status: http.StatusServiceUnavailable, retryable: true, wantAction: "retry"},
		{code: CodeIndexCorrupt, status: http.StatusInternalServerError, wantAction: "reindex"},
		{code: CodeInvalidPattern, status: http.StatusBadRequest, wantAction: "Correct"},
		// Not "correct the request": nothing about the request was wrong when it was
		// made, the index moved underneath it, and starting over is the fix.
		{code: CodeInvalidCursor, status: http.StatusBadRequest, wantAction: "again"},
		{code: CodeLimitExceeded, status: http.StatusBadRequest, wantAction: "Correct"},
	}

	for _, testCase := range cases {
		t.Run(testCase.code, func(t *testing.T) {
			body := `{"error":{"code":"` + testCase.code + `","message":"m","retryable":` +
				boolLiteral(testCase.retryable) + `}}`
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, body)
			}))

			_, err := client.Search(context.Background(), Request{Pattern: "x"})
			typed := typedError(t, err)
			if typed.Code != testCase.code {
				t.Errorf("code = %q, want %q", typed.Code, testCase.code)
			}
			if typed.Retryable != testCase.retryable {
				t.Errorf("retryable = %v, want %v", typed.Retryable, testCase.retryable)
			}
			if !strings.Contains(strings.ToLower(typed.Action), strings.ToLower(testCase.wantAction)) {
				t.Errorf("action = %q, want it to mention %q", typed.Action, testCase.wantAction)
			}
		})
	}
}

func boolLiteral(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestAnUnstructuredFailureIsStillTyped(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream exploded")
	}))

	_, err := client.Search(context.Background(), Request{Pattern: "x"})
	typed := typedError(t, err)
	if typed.Code != CodeInternalError {
		t.Errorf("code = %q", typed.Code)
	}
	if !typed.Retryable {
		t.Error("a 5xx with no envelope should be reported as retryable")
	}
}

func TestAnUnreachableServiceIsDistinctFromAnAbsentOne(t *testing.T) {
	// Nothing listening: the project is running but its service is not answering.
	offline := New("127.0.0.1:1")
	_, err := offline.Search(context.Background(), Request{Pattern: "x"})
	typed := typedError(t, err)
	if typed.Code != CodeServiceOffline {
		t.Errorf("code = %q, want %q", typed.Code, CodeServiceOffline)
	}
	if !typed.Retryable {
		t.Error("an unreachable service should be retryable")
	}

	// No address at all: the container predates service discovery, which a
	// restart fixes and a retry does not.
	absent := New("")
	_, err = absent.Search(context.Background(), Request{Pattern: "x"})
	typed = typedError(t, err)
	if typed.Code != CodeSearchUnsupported {
		t.Errorf("code = %q, want %q", typed.Code, CodeSearchUnsupported)
	}
	if typed.Retryable {
		t.Error("an absent service must not be reported as retryable")
	}
	if !strings.Contains(strings.ToLower(typed.Action), "restart") {
		t.Errorf("action = %q", typed.Action)
	}
}

// blockingService returns a handler that answers nothing until the test ends.
//
// The release channel is closed by a cleanup registered after the server's own,
// so it runs first; a handler parked on the request context instead would keep
// the test server's Close waiting, because a client-side timeout does not
// promptly cancel the server-side request on every platform.
func blockingService(t *testing.T) *Client {
	t.Helper()
	release := make(chan struct{})
	client := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release) })
	return client
}

func TestASlowServiceIsReportedAsRetryable(t *testing.T) {
	client := blockingService(t)
	client.HTTP = &http.Client{Timeout: 50 * time.Millisecond}

	_, err := client.Search(context.Background(), Request{Pattern: "x"})
	typed := typedError(t, err)
	if typed.Code != CodeServiceOffline || !typed.Retryable {
		t.Errorf("error = %+v", typed)
	}
}

func TestACancelledSearchIsNotBlamedOnTheService(t *testing.T) {
	client := blockingService(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Search(ctx, Request{Pattern: "x"})
	typed := typedError(t, err)
	if typed.Code != CodeInternalError || !strings.Contains(typed.Message, "cancelled") {
		t.Errorf("error = %+v", typed)
	}
}

func TestTheRequestCarriesOnlyTheQuery(t *testing.T) {
	var received string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		_, _ = io.WriteString(w, `{"matches":[],"total":0,"generation":1,"file_count":0}`)
	}))

	if _, err := client.Search(context.Background(), Request{
		Pattern: "needle", Mode: "regex", PathGlobs: []string{"**/*.go"}, Limit: 10,
	}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`"pattern":"needle"`, `"mode":"regex"`, `"path_globs":["**/*.go"]`, `"limit":10`} {
		if !strings.Contains(received, want) {
			t.Errorf("request body %q does not contain %q", received, want)
		}
	}
	// Nothing that could redirect scope may appear on the wire to the backend.
	for _, forbidden := range []string{"project_id", "repository_root", "workspace", "root"} {
		if strings.Contains(received, forbidden) {
			t.Errorf("request body carries %q: %s", forbidden, received)
		}
	}
}

func TestStatusAndReindexUseTheServiceContract(t *testing.T) {
	var indexBody string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /index", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		indexBody = string(body)
		_, _ = io.WriteString(w, `{"generation":3,"file_count":5,"applied":5,"full_build":true}`)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ready":true,"generation":3,"file_count":5,"indexed_at":"2026-08-01T10:00:00Z"}`)
	})
	client := newTestClient(t, mux)

	status, err := client.Reindex(context.Background(), true)
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if !strings.Contains(indexBody, `"mode":"full"`) {
		t.Errorf("index request = %s", indexBody)
	}
	if !status.Ready || status.Generation != 3 || status.FileCount != 5 {
		t.Errorf("status = %+v", status)
	}

	status, err = client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.IndexedAt != "2026-08-01T10:00:00Z" {
		t.Errorf("status = %+v", status)
	}
}

func TestOutlineTranslatesTheServiceResponseIntoTheStableShape(t *testing.T) {
	var received string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		_, _ = io.WriteString(w, `{"path":"internal/a.go","language":"go","bytes":120,"lines":9,`+
			`"digest":"abc","schema_version":1,`+
			`"symbols":[{"name":"Needle","kind":"function","start_line":7,"end_line":9,`+
			`"start_byte":40,"end_byte":118,"container":"Outer","depth":1,"signature":"func Needle() {"}],`+
			`"syntax":{"reported":true,"valid":false,"errors":2,"first_error_line":8}}`)
	}))

	outline, err := client.Outline(context.Background(), "internal/a.go")
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	if !strings.Contains(received, `"path":"internal/a.go"`) {
		t.Errorf("request body = %s", received)
	}
	// Nothing that could redirect scope may reach the service.
	for _, forbidden := range []string{"project_id", "repository_root", "workspace", "root"} {
		if strings.Contains(received, forbidden) {
			t.Errorf("request body carries %q: %s", forbidden, received)
		}
	}
	if len(outline.Symbols) != 1 {
		t.Fatalf("symbols = %+v", outline.Symbols)
	}
	symbol := outline.Symbols[0]
	if symbol.Name != "Needle" || symbol.Kind != "function" || symbol.Container != "Outer" || symbol.Depth != 1 {
		t.Errorf("symbol = %+v", symbol)
	}
	if symbol.StartByte != 40 || symbol.EndByte != 118 {
		t.Errorf("the extent did not survive: %+v", symbol)
	}
	if outline.Digest != "abc" || outline.Lines != 9 || outline.SchemaVersion != 1 {
		t.Errorf("outline = %+v", outline)
	}
	// A file that does not parse still reports its declarations, and says where to
	// look. Losing either half would make the tool useless mid-edit.
	if outline.Syntax.Valid || outline.Syntax.Errors != 2 || outline.Syntax.FirstErrorLine != 8 {
		t.Errorf("syntax = %+v", outline.Syntax)
	}
}

func TestOutlineNeverReturnsANilSymbolSlice(t *testing.T) {
	// A null reads to a model as "no answer" rather than "no declarations", which is
	// a different and wrong claim about a file.
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"path":"a.go","language":"go","symbols":null,"syntax":{"reported":true,"valid":true}}`)
	}))

	outline, err := client.Outline(context.Background(), "a.go")
	if err != nil {
		t.Fatal(err)
	}
	if outline.Symbols == nil {
		t.Error("symbols is nil")
	}
}

func TestEveryOutlineFailureCarriesAnActionAndNoneSaysRetry(t *testing.T) {
	// These four are the whole outline failure surface. None of them is fixed by
	// waiting, so none may read as retryable, and each has to point somewhere.
	for _, code := range []string{
		CodeLanguageUnsupported, CodeFileNotFound, CodeFileTooLarge, CodeParseIncomplete,
	} {
		t.Run(code, func(t *testing.T) {
			action := ActionFor(code)
			if action == "" {
				t.Fatal("the code carries no recommended action")
			}
			if strings.Contains(strings.ToLower(action), "retry") {
				t.Errorf("the action tells the caller to retry: %q", action)
			}
		})
	}
}
