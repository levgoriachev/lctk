package projectauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/gateway"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectauth"
	"github.com/lev-goryachev/lctk/internal/projectregistration"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

func TestOwnerApprovedOAuthRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv(lctkhome.EnvOverride, home)
	projectDir := filepath.Join(t.TempDir(), "oauth-project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	registered, err := projectregistration.Register(projectDir, projectregistry.ProfileMinimal)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(home, projectauth.LegacyFileName)
	if err := os.WriteFile(legacyPath, []byte(`{"obsolete_recoverable_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := projectauth.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy grant store was not removed: %v", err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	gateway.New(gateway.Options{Registry: projectregistry.Load, Authorizations: func() (*projectauth.Store, error) { return store, nil }, Status: func(context.Context, projectregistry.Project) (projectstack.Status, error) {
		return projectstack.Status{State: projectstack.StateRunning, Health: "healthy"}, nil
	}, Now: func() time.Time { return now }}).Register(mux)
	projectauth.NewHTTPServer(store, projectregistry.Load, func() time.Time { return now }).Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resource := server.URL + "/projects/" + registered.Project.ID + "/mcp"

	probe := postInitialize(t, client, resource, "")
	if probe.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initial MCP status = %d", probe.StatusCode)
	}
	challenge := probe.Header.Get("WWW-Authenticate")
	probe.Body.Close()
	if !strings.Contains(challenge, "resource_metadata=\"") || !strings.Contains(challenge, `scope="lctk:project"`) {
		t.Fatalf("OAuth challenge = %q", challenge)
	}

	metadataURL := server.URL + "/.well-known/oauth-protected-resource/projects/" + registered.Project.ID + "/mcp"
	var protected map[string]any
	getJSON(t, client, metadataURL, &protected)
	if protected["resource"] != resource {
		t.Fatalf("protected resource = %v, want %s", protected["resource"], resource)
	}
	var provider map[string]any
	getJSON(t, client, server.URL+"/.well-known/oauth-authorization-server", &provider)
	if provider["registration_endpoint"] != server.URL+"/oauth/register" {
		t.Fatalf("registration endpoint = %v", provider["registration_endpoint"])
	}

	redirectURI := "http://127.0.0.1:39001/callback"
	registrationBody := `{"client_name":"Codex integration test","redirect_uris":["` + redirectURI + `"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none","software_id":"codex-test"}`
	registrationResponse, err := client.Post(server.URL+"/oauth/register", "application/json", strings.NewReader(registrationBody))
	if err != nil {
		t.Fatal(err)
	}
	if registrationResponse.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(registrationResponse.Body)
		t.Fatalf("registration = %d: %s", registrationResponse.StatusCode, raw)
	}
	var registeredClient projectauth.Client
	if err := json.NewDecoder(registrationResponse.Body).Decode(&registeredClient); err != nil {
		t.Fatal(err)
	}
	registrationResponse.Body.Close()
	unsafeQuery := url.Values{"response_type": {"code"}, "client_id": {registeredClient.ID}, "redirect_uri": {"https://attacker.invalid/callback"}}
	unsafeResponse, err := client.Get(server.URL + "/oauth/authorize?" + unsafeQuery.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if unsafeResponse.StatusCode != http.StatusBadRequest || unsafeResponse.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect produced %d to %q", unsafeResponse.StatusCode, unsafeResponse.Header.Get("Location"))
	}
	unsafeResponse.Body.Close()

	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{"response_type": {"code"}, "client_id": {registeredClient.ID}, "redirect_uri": {redirectURI}, "scope": {projectauth.ScopeProject}, "state": {"codex-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {resource}}
	authorizeResponse, err := client.Get(server.URL + "/oauth/authorize?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if authorizeResponse.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %d", authorizeResponse.StatusCode)
	}
	waitingURL := server.URL + authorizeResponse.Header.Get("Location")
	authorizeResponse.Body.Close()

	waiting, err := client.Get(waitingURL)
	if err != nil {
		t.Fatal(err)
	}
	waitingBody, _ := io.ReadAll(waiting.Body)
	waiting.Body.Close()
	if waiting.StatusCode != http.StatusOK || !strings.Contains(string(waitingBody), "Approve this connection in LCTK") || !strings.Contains(string(waitingBody), "<svg") || strings.Contains(string(waitingBody), "/admin/api/") {
		t.Fatalf("waiting page = %d: %s", waiting.StatusCode, waitingBody)
	}
	if policy := waiting.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "default-src 'none'") || !strings.Contains(policy, "img-src 'self'") {
		t.Fatalf("waiting page content security policy = %q", policy)
	}
	favicon, err := client.Get(server.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	faviconBody, _ := io.ReadAll(favicon.Body)
	favicon.Body.Close()
	if favicon.StatusCode != http.StatusOK || favicon.Header.Get("Content-Type") != "image/x-icon" || len(faviconBody) < 6 || faviconBody[2] != 1 || faviconBody[4] != 3 {
		t.Fatalf("favicon = %d %q %x", favicon.StatusCode, favicon.Header.Get("Content-Type"), faviconBody)
	}
	pending := store.Pending(now)
	if len(pending) != 1 || pending[0].ClientName != "Codex integration test" || pending[0].ProjectID != registered.Project.ID {
		t.Fatalf("pending = %+v", pending)
	}
	if _, err := store.Approve(pending[0].ID, now); err != nil {
		t.Fatal(err)
	}

	callbackResponse, err := client.Get(waitingURL)
	if err != nil {
		t.Fatal(err)
	}
	if callbackResponse.StatusCode != http.StatusFound {
		t.Fatalf("approved poll = %d", callbackResponse.StatusCode)
	}
	callback, err := url.Parse(callbackResponse.Header.Get("Location"))
	callbackResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if callback.Query().Get("state") != "codex-state" || callback.Query().Get("code") == "" {
		t.Fatalf("callback = %s", callback)
	}

	tokenForm := url.Values{"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")}, "client_id": {registeredClient.ID}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}, "resource": {resource}}
	tokenResponse, err := client.PostForm(server.URL+"/oauth/token", tokenForm)
	if err != nil {
		t.Fatal(err)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if tokenResponse.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(tokenResponse.Body)
		t.Fatalf("token = %d: %s", tokenResponse.StatusCode, raw)
	}
	if err := json.NewDecoder(tokenResponse.Body).Decode(&tokens); err != nil {
		t.Fatal(err)
	}
	tokenResponse.Body.Close()
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.ExpiresIn != 900 {
		t.Fatalf("tokens = %+v", tokens)
	}
	reusedCode, err := client.PostForm(server.URL+"/oauth/token", tokenForm)
	if err != nil {
		t.Fatal(err)
	}
	if reusedCode.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused authorization code = %d", reusedCode.StatusCode)
	}
	reusedCode.Body.Close()

	authorized := postInitialize(t, client, resource, tokens.AccessToken)
	if authorized.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(authorized.Body)
		t.Fatalf("authorized MCP = %d: %s", authorized.StatusCode, raw)
	}
	authorized.Body.Close()
	rawStore, err := os.ReadFile(filepath.Join(home, projectauth.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawStore), tokens.AccessToken) || strings.Contains(string(rawStore), tokens.RefreshToken) {
		t.Fatal("OAuth store contains a recoverable bearer token")
	}

	refreshForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}, "client_id": {registeredClient.ID}, "resource": {resource}}
	refreshed, err := client.PostForm(server.URL+"/oauth/token", refreshForm)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(refreshed.Body)
		t.Fatalf("refresh = %d: %s", refreshed.StatusCode, raw)
	}
	refreshed.Body.Close()
	reused, err := client.PostForm(server.URL+"/oauth/token", refreshForm)
	if err != nil {
		t.Fatal(err)
	}
	if reused.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused refresh token = %d", reused.StatusCode)
	}
	reused.Body.Close()

	authorizations := store.List()
	if len(authorizations) != 1 {
		t.Fatalf("authorizations = %d", len(authorizations))
	}
	if _, err := store.Revoke(authorizations[0].ID); err != nil {
		t.Fatal(err)
	}
	revoked := postInitialize(t, client, resource, tokens.AccessToken)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked access token = %d", revoked.StatusCode)
	}
	revoked.Body.Close()
}

func postInitialize(t *testing.T, client *http.Client, endpoint, token string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func getJSON(t *testing.T, client *http.Client, endpoint string, output any) {
	t.Helper()
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", endpoint, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		t.Fatal(err)
	}
}
