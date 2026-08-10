package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/auditlog"
	"github.com/lev-goryachev/lctk/internal/commandpolicy"
	"github.com/lev-goryachev/lctk/internal/gitinfo"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/runner"
)

var testNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// fixture is a gateway wired to an isolated registry and OAuth store, so routing,
// authentication, and scope are tested without touching disk or containers.
type fixture struct {
	server           *httptest.Server
	registry         *projectregistry.Registry
	authorizations   *projectauth.Store
	tokens           map[string]string
	authorizationIDs map[string]string
	now              time.Time
	logs             *bytes.Buffer
	state            map[string]projectstack.State
	statusEr         error
	// service is the address the fixture reports as the project's published
	// code-intelligence service, empty unless a test installs a stand-in.
	service map[string]string
	// changes stands in for the host watch supervisor. A project absent from the
	// map is one nothing is watching.
	changes map[string]ChangeState
	// woken records which projects the route asked the host to observe.
	woken []string
	// flushed records which projects a search asked the host to bring up to date.
	flushed []string
	// git stands in for the working-tree reader, nil unless a test installs one.
	git GitReader
	// runner, audit, manifest, and commands stand in for the command surface.
	// They are fields rather than options so a test can install them after the
	// gateway is built, the same way git is.
	runner   CommandRunner
	audit    Auditor
	manifest ManifestLoader
	commands commandpolicy.Set
}

func newFixture(t *testing.T, requireRunning bool, projectIDs ...string) *fixture {
	t.Helper()

	f := &fixture{
		registry:         projectregistry.New(),
		authorizations:   mustAuthorizationStore(t),
		tokens:           map[string]string{},
		authorizationIDs: map[string]string{},
		now:              testNow,
		logs:             &bytes.Buffer{},
		state:            map[string]projectstack.State{},
		service:          map[string]string{},
		changes:          map[string]ChangeState{},
	}

	// The registry is populated directly rather than through Add, so the test
	// controls identifiers and needs no real folders.
	stored := make([]projectregistry.Project, 0, len(projectIDs))
	for _, id := range projectIDs {
		stored = append(stored, projectregistry.Project{
			ID:           id,
			Name:         id,
			Path:         "/work/" + id,
			Key:          "/work/" + id,
			Profile:      projectregistry.ProfileMinimal,
			RegisteredAt: testNow,
		})
		f.state[id] = projectstack.StateRunning
	}
	f.registry = registryWith(t, stored)

	gateway := New(Options{
		// The registry is read per request in production, so the fixture rebuilds
		// it here with whatever approvals a test has installed. Building it once
		// up front would freeze the approvals before the test could set any.
		Registry:       func() (*projectregistry.Registry, error) { return f.registryWithCommands(t), nil },
		Authorizations: func() (*projectauth.Store, error) { return f.authorizations, nil },
		Status: func(_ context.Context, project projectregistry.Project) (projectstack.Status, error) {
			state, ok := f.state[project.ID]
			if !ok {
				state = projectstack.StateStopped
			}
			status := projectstack.Status{ProjectID: project.ID, State: state}
			if state == projectstack.StateRunning {
				status.Health = "healthy"
				status.ServiceAddress = f.service[project.ID]
			}
			return status, f.statusEr
		},
		Logger:         slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:            func() time.Time { return f.now },
		RequireRunning: requireRunning,
		Wake: func(project projectregistry.Project, _ projectstack.Status) {
			f.woken = append(f.woken, project.ID)
		},
		Changes: func(projectID string) (ChangeState, bool) {
			state, ok := f.changes[projectID]
			return state, ok
		},
		Git:      gitProxy{f},
		Runner:   runnerProxy{f},
		Audit:    auditProxy{f},
		Manifest: func(root string) (projectmanifest.Result, error) { return f.loadManifest(root) },
		Flush: func(_ context.Context, projectID string) {
			f.flushed = append(f.flushed, projectID)
			// A real flush applies what is pending, so the stand-in does too.
			if state, ok := f.changes[projectID]; ok {
				state.Pending = 0
				f.changes[projectID] = state
			}
		},
	})

	mux := http.NewServeMux()
	gateway.Register(mux)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	for _, id := range projectIDs {
		f.tokens[id], f.authorizationIDs[id] = f.issueToken(t, id, f.endpoint(id), testNow)
	}
	return f
}

func mustAuthorizationStore(t *testing.T) *projectauth.Store {
	t.Helper()
	store, err := projectauth.OpenAt(filepath.Join(t.TempDir(), projectauth.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// issueToken drives the same registration, approval, PKCE, and exchange used
// by a real client while keeping gateway tests independent of a browser.
func (f *fixture) issueToken(t *testing.T, projectID, resource string, now time.Time) (string, string) {
	t.Helper()
	redirect := "http://127.0.0.1:39001/callback"
	client, err := f.authorizations.RegisterClient(projectauth.Registration{Name: "test-client", RedirectURIs: []string{redirect}}, now)
	if err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	request, err := f.authorizations.Begin(projectauth.BeginRequest{ClientID: client.ID, ProjectID: projectID, Resource: resource, RedirectURI: redirect, Scopes: []string{projectauth.ScopeProject}, CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:])}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.authorizations.Approve(request.ID, now); err != nil {
		t.Fatal(err)
	}
	_, code, _, err := f.authorizations.RequestState(request.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := f.authorizations.ExchangeCode(code, client.ID, redirect, resource, verifier, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, authorization := range f.authorizations.List() {
		if authorization.ClientID == client.ID {
			return pair.AccessToken, authorization.ID
		}
	}
	t.Fatal("OAuth exchange persisted no authorization for its client")
	return "", ""
}

// registryWith builds a registry containing the given records.
//
// The records are written to an isolated LCTK home and loaded back, because the
// registry's contents are deliberately not settable from outside the package.
// That also exercises the real load path.
func registryWith(t *testing.T, projects []projectregistry.Project) *projectregistry.Registry {
	t.Helper()
	home := t.TempDir()
	t.Setenv(lctkhome.EnvOverride, home)

	document := struct {
		SchemaVersion int                       `json:"schema_version"`
		Projects      []projectregistry.Project `json:"projects"`
	}{SchemaVersion: projectregistry.SchemaVersion, Projects: projects}

	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, projectregistry.FileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := projectregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func (f *fixture) endpoint(projectID string) string {
	return f.server.URL + "/projects/" + projectID + "/mcp"
}

// connect opens a real MCP client session against a project endpoint.
func (f *fixture) connect(t *testing.T, projectID, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "lctk-gateway-test", Version: "0.1.0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: f.endpoint(projectID)}
	if token != "" {
		transport.HTTPClient = &http.Client{Transport: bearerRoundTripper{token: token}}
	}
	session, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect to %s: %v", projectID, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

type bearerRoundTripper struct{ token string }

func (b bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(request)
}

// rawPost issues a bare JSON-RPC initialize, for cases where the client library
// would refuse before the response can be inspected.
func rawPost(t *testing.T, url, token string) (*http.Response, TypedError) {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"raw","version":"0"}}}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope errorEnvelope
	_ = json.Unmarshal(raw, &envelope)
	return response, envelope.Error
}

func callProjectInfo(t *testing.T, session *mcp.ClientSession, arguments map[string]any) projectInfoOutput {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "project_info",
		Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("project_info failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("project_info returned an error result: %+v", result.Content)
	}

	var output projectInfoOutput
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatalf("project_info output is not the expected shape: %v", err)
	}
	return output
}

func TestProjectInfoAnswersFromTheRoute(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	output := callProjectInfo(t, session, nil)
	if output.ProjectID != "alpha-aaaaaaaa" {
		t.Errorf("project_id = %q", output.ProjectID)
	}
	if output.ScopeSource != "route_and_registry" {
		t.Errorf("scope_source = %q", output.ScopeSource)
	}
	if output.State != string(projectstack.StateRunning) {
		t.Errorf("state = %q", output.State)
	}
	// The host path must not leak to the client; the project sees its own mount.
	if output.Root != projectstack.WorkspaceMount {
		t.Errorf("root = %q, want the in-container workspace", output.Root)
	}
	if strings.Contains(output.Root, "/work/") {
		t.Errorf("the host path leaked to the client: %q", output.Root)
	}
}

// TestModelSuppliedProjectIDCannotChangeScope is the roadmap's required check.
func TestModelSuppliedProjectIDCannotChangeScope(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa", "beta-bbbbbbbb")
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	for _, arguments := range []map[string]any{
		{"project_id": "beta-bbbbbbbb"},
		{"repository_root": "/work/beta-bbbbbbbb"},
		{"path": "../beta-bbbbbbbb"},
		{"project_id": "beta-bbbbbbbb", "repository_root": "/work/beta-bbbbbbbb"},
	} {
		output := callProjectInfo(t, session, arguments)
		if output.ProjectID != "alpha-aaaaaaaa" {
			t.Errorf("arguments %v changed the scope to %q", arguments, output.ProjectID)
		}
	}
}

// TestCredentialAndRouteMustAgree is the roadmap's required check that one
// project's key does not open another.
func TestCredentialAndRouteMustAgree(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa", "beta-bbbbbbbb")

	// Alpha's token on beta's route.
	response, typed := rawPost(t, f.endpoint("beta-bbbbbbbb"), f.tokens["alpha-aaaaaaaa"])
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", response.StatusCode)
	}
	if typed.Code != CodeAuthForbidden {
		t.Errorf("code = %q, want %s", typed.Code, CodeAuthForbidden)
	}
	if typed.Retryable {
		t.Error("a scope mismatch is not retryable")
	}
	if typed.RequestID == "" {
		t.Error("no request id was reported")
	}
	if typed.ProjectID != "beta-bbbbbbbb" {
		t.Errorf("project_id = %q, want the routed project", typed.ProjectID)
	}

	// Each token still works on its own route.
	for _, id := range []string{"alpha-aaaaaaaa", "beta-bbbbbbbb"} {
		output := callProjectInfo(t, f.connect(t, id, f.tokens[id]), nil)
		if output.ProjectID != id {
			t.Errorf("%s: got %q", id, output.ProjectID)
		}
	}
}

func TestMissingAndUnknownCredentials(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	response, typed := rawPost(t, f.endpoint("alpha-aaaaaaaa"), "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing credential: status = %d, want 401", response.StatusCode)
	}
	if typed.Code != CodeAuthRequired {
		t.Errorf("missing credential: code = %q", typed.Code)
	}
	if response.Header.Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate challenge was sent")
	}

	response, typed = rawPost(t, f.endpoint("alpha-aaaaaaaa"), "not-a-real-token")
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown credential: status = %d, want 401", response.StatusCode)
	}
	if typed.Code != CodeAuthRequired {
		t.Errorf("unknown credential: code = %q", typed.Code)
	}
}

// TestUnknownProjectRequiresAValidCredentialFirst keeps the endpoint from
// confirming which projects exist to an unauthenticated caller.
func TestUnknownProjectRequiresAValidCredentialFirst(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	// No credential against a project that does not exist: the answer must be
	// about the credential, not about the project.
	response, typed := rawPost(t, f.endpoint("ghost-99999999"), "")
	if response.StatusCode != http.StatusUnauthorized || typed.Code != CodeAuthRequired {
		t.Errorf("status = %d, code = %q; want 401 AUTH_REQUIRED", response.StatusCode, typed.Code)
	}

	// A real credential scoped elsewhere gets the scope answer, still not an
	// existence answer.
	response, typed = rawPost(t, f.endpoint("ghost-99999999"), f.tokens["alpha-aaaaaaaa"])
	if response.StatusCode != http.StatusForbidden || typed.Code != CodeAuthForbidden {
		t.Errorf("status = %d, code = %q; want 403 AUTH_FORBIDDEN", response.StatusCode, typed.Code)
	}
}

// TestProjectNotFoundForAuthorizedButUnregisteredProject covers an OAuth
// authorization that outlived its registration.
func TestProjectNotFoundForAuthorizedButUnregisteredProject(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	token, _ := f.issueToken(t, "gone-11111111", f.endpoint("gone-11111111"), testNow)

	response, typed := rawPost(t, f.endpoint("gone-11111111"), token)
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", response.StatusCode)
	}
	if typed.Code != CodeProjectNotFound {
		t.Errorf("code = %q, want %s", typed.Code, CodeProjectNotFound)
	}
	if typed.RecommendedAction == "" {
		t.Error("no recommended action was given")
	}
}

// TestStoppedAndStartingProjectsReturnTypedErrors is the roadmap's required check
// that a stopped project answers with a typed error rather than empty data.
func TestStoppedAndStartingProjectsReturnTypedErrors(t *testing.T) {
	cases := []struct {
		state     projectstack.State
		wantCode  string
		retryable bool
	}{
		{projectstack.StateStopped, CodeProjectStopped, false},
		{projectstack.StateStarting, CodeProjectStarting, true},
		{projectstack.StateError, CodeServiceUnavailable, false},
		{projectstack.StateUnknown, CodeServiceUnavailable, true},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.state), func(t *testing.T) {
			f := newFixture(t, true, "alpha-aaaaaaaa")
			f.state["alpha-aaaaaaaa"] = testCase.state

			response, typed := rawPost(t, f.endpoint("alpha-aaaaaaaa"), f.tokens["alpha-aaaaaaaa"])
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", response.StatusCode)
			}
			if typed.Code != testCase.wantCode {
				t.Errorf("code = %q, want %s", typed.Code, testCase.wantCode)
			}
			if typed.Retryable != testCase.retryable {
				t.Errorf("retryable = %t, want %t", typed.Retryable, testCase.retryable)
			}
			if typed.ProjectID != "alpha-aaaaaaaa" {
				t.Errorf("project_id = %q", typed.ProjectID)
			}
			if typed.RecommendedAction == "" {
				t.Error("no recommended action was given")
			}
		})
	}
}

func TestRevokedAndExpiredOAuthTokensAreRefused(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	expired := f.tokens["alpha-aaaaaaaa"]
	f.now = testNow.Add(16 * time.Minute)
	response, typed := rawPost(t, f.endpoint("alpha-aaaaaaaa"), expired)
	if response.StatusCode != http.StatusUnauthorized || typed.Code != CodeAuthRequired {
		t.Errorf("expired: status = %d, code = %q", response.StatusCode, typed.Code)
	}

	f.now = testNow
	revoked, authorizationID := f.issueToken(t, "alpha-aaaaaaaa", f.endpoint("alpha-aaaaaaaa"), testNow)
	if _, err := f.authorizations.Revoke(authorizationID); err != nil {
		t.Fatal(err)
	}
	response, typed = rawPost(t, f.endpoint("alpha-aaaaaaaa"), revoked)
	if response.StatusCode != http.StatusUnauthorized || typed.Code != CodeAuthRequired {
		t.Errorf("revoked: status = %d, code = %q", response.StatusCode, typed.Code)
	}
}

func TestRuntimeFailuresAreTyped(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	f.statusEr = projectstack.ErrRuntimeUnavailable

	response, typed := rawPost(t, f.endpoint("alpha-aaaaaaaa"), f.tokens["alpha-aaaaaaaa"])
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.StatusCode)
	}
	if typed.Code != CodeRuntimeUnavailable {
		t.Errorf("code = %q, want %s", typed.Code, CodeRuntimeUnavailable)
	}
	if !typed.Retryable {
		t.Error("an unavailable runtime is retryable once it is started")
	}

	f.statusEr = projectstack.ErrLinuxContainersRequired
	response, typed = rawPost(t, f.endpoint("alpha-aaaaaaaa"), f.tokens["alpha-aaaaaaaa"])
	if typed.Code != CodeRuntimeUnsuitable {
		t.Errorf("code = %q, want %s", typed.Code, CodeRuntimeUnsuitable)
	}
	if typed.Retryable {
		t.Error("the wrong container mode is not fixed by retrying")
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.StatusCode)
	}
}

func TestLogsCarryRequestAndProjectIdentifiers(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	callProjectInfo(t, session, nil)

	logged := f.logs.String()
	for _, want := range []string{"request_id=req-", "project_id=alpha-aaaaaaaa", "authorization_id=authorization-"} {
		if !strings.Contains(logged, want) {
			t.Errorf("logs are missing %q:\n%s", want, logged)
		}
	}
	// A refusal must be logged too, with its code.
	rawPost(t, f.endpoint("alpha-aaaaaaaa"), "")
	if !strings.Contains(f.logs.String(), "code="+CodeAuthRequired) {
		t.Errorf("refusals are not logged with their code:\n%s", f.logs.String())
	}
	// The credential itself must never reach the log.
	if strings.Contains(logged, f.tokens["alpha-aaaaaaaa"]) {
		t.Error("the OAuth token was written to the log")
	}
}

// TestToolListIsTheDocumentedCatalog pins the tool catalog.
//
// A project endpoint exposes named user actions and nothing else, per ADR-0004.
// The list is asserted exactly so that adding a tool is a deliberate act with a
// test change attached, rather than something that happens quietly.
func TestToolListIsTheDocumentedCatalog(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)

	want := []string{
		"callees_find", "callers_find", "code_search_semantic", "dependency_path", "exact_search",
		"git_diff", "git_status", "impact_analyze", "memory_delete", "memory_get", "memory_put",
		"memory_search", "project_info", "repository_map", "run_command",
	}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	}
}

func TestGatewayWithoutLifecycleGatingStillScopes(t *testing.T) {
	// With gating disabled a stopped project is still reachable, which is what
	// the registry-only tests rely on, but scope must not weaken.
	f := newFixture(t, false, "alpha-aaaaaaaa", "beta-bbbbbbbb")
	f.state["alpha-aaaaaaaa"] = projectstack.StateStopped

	output := callProjectInfo(t, f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"]), map[string]any{
		"project_id": "beta-bbbbbbbb",
	})
	if output.ProjectID != "alpha-aaaaaaaa" {
		t.Errorf("scope changed to %q", output.ProjectID)
	}
	if output.State != string(projectstack.StateStopped) {
		t.Errorf("state = %q, want the real state to still be reported", output.State)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":    "abc",
		"bearer abc":    "abc",
		"BEARER  abc  ": "abc",
		"Basic abc":     "",
		"abc":           "",
		"":              "",
		"Bearer":        "",
	}
	for header, want := range cases {
		if got := bearerToken(header); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

// TestUnauthenticatedProbeIsTypedAndUniform covers the diagnostic contract in
// ADR-0012: an operator's existing Codex diagnostics probe a route without
// credentials, so the route must answer rather than fail to connect. ADR-0014
// adds that the answer names the stale-environment case, and it must do so
// without revealing whether the project exists.
func TestUnauthenticatedProbeIsTypedAndUniform(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	type probe struct{ method, project string }
	probes := []probe{
		{http.MethodGet, "alpha-aaaaaaaa"},
		{http.MethodHead, "alpha-aaaaaaaa"},
		{http.MethodPost, "alpha-aaaaaaaa"},
		{http.MethodGet, "never-registered"},
		{http.MethodHead, "never-registered"},
	}

	var bodies []string
	for _, p := range probes {
		request, err := http.NewRequestWithContext(t.Context(), p.method, f.endpoint(p.project), nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.project, err)
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}

		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", p.method, p.project, response.StatusCode)
		}
		if response.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: no WWW-Authenticate header", p.method, p.project)
		}
		if p.method == http.MethodHead {
			continue
		}

		var envelope errorEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("%s %s: body is not the typed envelope: %v", p.method, p.project, err)
		}
		if envelope.Error.Code != CodeAuthRequired {
			t.Errorf("%s %s: code = %q", p.method, p.project, envelope.Error.Code)
		}
		if !strings.Contains(envelope.Error.RecommendedAction, "Authenticate") {
			t.Errorf("%s %s: recommended action does not name OAuth authentication: %q",
				p.method, p.project, envelope.Error.RecommendedAction)
		}
		// The project identifier echoes the route, which the caller already
		// knows. Everything else must match between a real and an invented
		// project.
		envelope.Error.ProjectID = ""
		envelope.Error.RequestID = ""
		normalized, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(normalized))
	}

	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("an unauthenticated probe distinguishes a registered project from an invented one:\n%s\n%s",
				bodies[0], bodies[i])
		}
	}
}

// gitProxy defers to whatever reader the fixture holds at call time, so a test
// can install one after the gateway has already been built.
//
// The gateway asks whether a reader exists when it registers tools, which is
// before a test can set the field; the proxy is always present and the fixture
// decides what it does.
type gitProxy struct{ fixture *fixture }

func (p gitProxy) Status(ctx context.Context, root string, options gitinfo.Options) (gitinfo.Status, error) {
	if p.fixture.git == nil {
		return gitinfo.Status{}, gitinfo.ErrGitUnavailable
	}
	return p.fixture.git.Status(ctx, root, options)
}

func (p gitProxy) Diff(ctx context.Context, root string, options gitinfo.DiffOptions) (gitinfo.Diff, error) {
	if p.fixture.git == nil {
		return gitinfo.Diff{}, gitinfo.ErrGitUnavailable
	}
	return p.fixture.git.Diff(ctx, root, options)
}

// The proxies below exist for the same reason gitProxy does: the gateway decides
// which tools to register when it builds a server, which is before a test can
// install a stand-in. The proxy is always present and the fixture decides what it
// does.
type runnerProxy struct{ fixture *fixture }

func (p runnerProxy) Run(ctx context.Context, request runner.Request) (runner.Result, error) {
	if p.fixture.runner == nil {
		return runner.Result{}, runner.ErrRuntimeUnavailable
	}
	return p.fixture.runner.Run(ctx, request)
}

type auditProxy struct{ fixture *fixture }

func (p auditProxy) Append(entry auditlog.Entry) error {
	if p.fixture.audit == nil {
		return nil
	}
	return p.fixture.audit.Append(entry)
}

// loadManifest serves the fixture's proposals, and none when a test installs no
// loader.
func (f *fixture) loadManifest(root string) (projectmanifest.Result, error) {
	if f.manifest == nil {
		return projectmanifest.Result{}, nil
	}
	return f.manifest(root)
}

// registryWithCommands returns the registry with the fixture's approvals applied.
func (f *fixture) registryWithCommands(t *testing.T) *projectregistry.Registry {
	t.Helper()
	if len(f.commands.Approvals) == 0 && f.commands.Image == "" && f.commands.Network == "" {
		return f.registry
	}
	for _, project := range f.registry.List() {
		if err := f.registry.SetCommands(project.ID, f.commands); err != nil {
			t.Fatal(err)
		}
	}
	return f.registry
}
