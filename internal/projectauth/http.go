package projectauth

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	brandassets "github.com/lev-goryachev/lctk/assets/brand"
	"github.com/lev-goryachev/lctk/internal/adminsession"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

// HTTPServer exposes the standards-based OAuth surface used by MCP clients.
type HTTPServer struct {
	store    *Store
	registry func() (*projectregistry.Registry, error)
	now      func() time.Time
}

// NewHTTPServer constructs the loopback OAuth authorization server.
func NewHTTPServer(store *Store, registry func() (*projectregistry.Registry, error), now func() time.Time) *HTTPServer {
	if registry == nil {
		registry = projectregistry.Load
	}
	if now == nil {
		now = time.Now
	}
	return &HTTPServer{store: store, registry: registry, now: now}
}

// Register attaches discovery, registration, authorization, and token routes.
func (s *HTTPServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/projects/{project_id}/mcp", s.loopback(s.protectedResourceMetadata))
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.loopback(s.authorizationServerMetadata))
	mux.HandleFunc("POST /oauth/register", s.loopback(s.registerClient))
	mux.HandleFunc("GET /oauth/authorize", s.loopback(s.authorize))
	mux.HandleFunc("GET /oauth/requests/{id}", s.loopback(s.authorizationStatus))
	mux.HandleFunc("POST /oauth/token", s.loopback(s.token))
	mux.HandleFunc("GET /favicon.ico", s.loopback(s.favicon))
}

// ResourceURL is the exact RFC 8707 audience for one route.
func ResourceURL(host, projectID string) string {
	return "http://" + host + "/projects/" + url.PathEscape(projectID) + "/mcp"
}

// MetadataURL is the RFC 9728 document advertised by an unauthenticated route.
func MetadataURL(host, projectID string) string {
	return "http://" + host + "/.well-known/oauth-protected-resource/projects/" + url.PathEscape(projectID) + "/mcp"
}

func (s *HTTPServer) loopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !adminsession.LoopbackHost(r.Host) {
			oauthError(w, http.StatusForbidden, "access_denied", "LCTK OAuth is available only through a loopback host")
			return
		}
		next(w, r)
	}
}

func (s *HTTPServer) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"resource":                 ResourceURL(r.Host, r.PathValue("project_id")),
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{ScopeProject},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *HTTPServer) authorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{ScopeProject},
	})
}

func (s *HTTPServer) registerClient(w http.ResponseWriter, r *http.Request) {
	var input Registration
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(&input); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration JSON is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration must contain one JSON object")
		return
	}
	client, err := s.store.RegisterClient(input, s.now())
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	writeOAuthJSON(w, http.StatusCreated, map[string]any{"client_id": client.ID, "client_name": client.Name, "redirect_uris": client.RedirectURIs, "grant_types": client.GrantTypes, "response_types": client.ResponseTypes, "token_endpoint_auth_method": client.TokenEndpointAuthMethod, "client_id_issued_at": client.CreatedAt.Unix()})
}

func (s *HTTPServer) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	redirectURI := query.Get("redirect_uri")
	if _, err := s.store.ValidateClientRedirect(query.Get("client_id"), redirectURI); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client", err.Error())
		return
	}
	redirectError := func(code, description string) {
		target, err := redirectWith(redirectURI, query.Get("state"), "", code, description)
		if err != nil {
			oauthError(w, http.StatusBadRequest, code, description)
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
	}
	if query.Get("response_type") != "code" {
		redirectError("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if query.Get("state") == "" {
		redirectError("invalid_request", "state is required")
		return
	}
	if query.Get("code_challenge_method") != "S256" {
		redirectError("invalid_request", "code_challenge_method must be S256")
		return
	}
	resource := query.Get("resource")
	projectID, err := projectFromResource(resource, r.Host)
	if err != nil {
		redirectError("invalid_target", err.Error())
		return
	}
	registry, err := s.registry()
	if err != nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "project registry is unavailable")
		return
	}
	if !registryContainsExact(registry, projectID) {
		redirectError("invalid_target", "the requested project resource is not registered")
		return
	}
	request, err := s.store.Begin(BeginRequest{ClientID: query.Get("client_id"), ProjectID: projectID, Resource: resource, RedirectURI: redirectURI, Scopes: strings.Fields(query.Get("scope")), State: query.Get("state"), CodeChallenge: query.Get("code_challenge")}, s.now())
	if err != nil {
		redirectError("invalid_request", err.Error())
		return
	}
	http.Redirect(w, r, "/oauth/requests/"+url.PathEscape(request.ID), http.StatusFound)
}

func (s *HTTPServer) authorizationStatus(w http.ResponseWriter, r *http.Request) {
	request, code, state, err := s.store.RequestState(r.PathValue("id"), s.now())
	if err != nil {
		http.Error(w, "This authorization request does not exist or has expired.", http.StatusNotFound)
		return
	}
	switch request.Status {
	case "approved":
		target, err := redirectWith(request.RedirectURI, state, code, "", "")
		if err != nil {
			http.Error(w, "The registered callback is invalid.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	case "denied":
		target, err := redirectWith(request.RedirectURI, state, "", "access_denied", "The connection was denied in LCTK.")
		if err != nil {
			http.Error(w, "The registered callback is invalid.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	case "expired":
		target, err := redirectWith(request.RedirectURI, state, "", "access_denied", "The LCTK approval request expired.")
		if err != nil {
			http.Error(w, "The registered callback is invalid.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Refresh", "2")
	_ = waitingPage.Execute(w, waitingPageData{Logo: waitingPageLogo, Client: request.ClientName, Project: request.ProjectID, Callback: request.RedirectURI})
}

// favicon serves the canonical embedded browser icon without introducing a
// filesystem or remote-network dependency into the local OAuth boundary.
func (s *HTTPServer) favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(brandassets.FaviconICO)
}

func (s *HTTPServer) token(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "public clients must use token_endpoint_auth_method none")
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/x-www-form-urlencoded" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token requests must use application/x-www-form-urlencoded")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token request form is invalid")
		return
	}
	var pair TokenPair
	var err error
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		pair, err = s.store.ExchangeCode(r.Form.Get("code"), r.Form.Get("client_id"), r.Form.Get("redirect_uri"), r.Form.Get("resource"), r.Form.Get("code_verifier"), s.now())
	case "refresh_token":
		pair, err = s.store.Refresh(r.Form.Get("refresh_token"), r.Form.Get("client_id"), r.Form.Get("resource"), strings.Fields(r.Form.Get("scope")), s.now())
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
		return
	}
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the OAuth grant is invalid, expired, or already used")
		return
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{"access_token": pair.AccessToken, "refresh_token": pair.RefreshToken, "token_type": "Bearer", "expires_in": int(pair.ExpiresIn.Seconds()), "scope": strings.Join(pair.Scopes, " ")})
}

func registryContainsExact(registry *projectregistry.Registry, projectID string) bool {
	for _, project := range registry.List() {
		if project.ID == projectID {
			return true
		}
	}
	return false
}

func projectFromResource(raw, host string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host != host || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("resource must be the exact loopback MCP URL")
	}
	const prefix, suffix = "/projects/", "/mcp"
	if !strings.HasPrefix(parsed.Path, prefix) || !strings.HasSuffix(parsed.Path, suffix) {
		return "", errors.New("resource must name a project MCP route")
	}
	id, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(parsed.Path, prefix), suffix))
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", errors.New("resource has an invalid project identifier")
	}
	return id, nil
}

func redirectWith(raw, state, code, oauthCode, description string) (string, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	query := target.Query()
	if state != "" {
		query.Set("state", state)
	}
	if code != "" {
		query.Set("code", code)
	}
	if oauthCode != "" {
		query.Set("error", oauthCode)
	}
	if description != "" {
		query.Set("error_description", description)
	}
	target.RawQuery = query.Encode()
	return target.String(), nil
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeOAuthJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeOAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// waitingPageData preserves html/template escaping for every request-derived
// value while permitting only the separately trusted static brand SVG.
type waitingPageData struct {
	Logo     template.HTML
	Client   string
	Project  string
	Callback string
}

// waitingPageLogo is trusted because it is compiled from the reviewed static
// repository asset, never from a request, registry value, or remote response.
var waitingPageLogo = template.HTML(brandassets.LockupOnLightSVG)

var waitingPage = template.Must(template.New("waiting").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Approve in LCTK</title><link rel="icon" href="/favicon.ico" sizes="any">
<style>:root{color-scheme:light}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:32px 20px;background:#f7f7f4;color:#31363d;font-family:Segoe UI,Arial,sans-serif}main{width:min(680px,100%)}.brand{margin-bottom:42px}.brand svg{display:block;width:180px;height:auto}h1{margin:0 0 12px;font-size:32px;line-height:1.18;letter-spacing:-.02em}.lead{margin:0 0 28px;color:#596169;font-size:17px;line-height:1.55}.card{overflow:hidden;border:1px solid #d7dcde;border-radius:14px;background:#fff;box-shadow:0 18px 45px rgba(49,54,61,.08)}.row{display:grid;grid-template-columns:110px minmax(0,1fr);gap:18px;padding:17px 20px;border-top:1px solid #e7eaeb}.row:first-child{border-top:0}.label{color:#687178;font-size:13px;font-weight:700;letter-spacing:.07em;text-transform:uppercase}.value{min-width:0;font-weight:600;overflow-wrap:anywhere}code{font:13px/1.5 Consolas,monospace;color:#0b7184}.instruction{margin:26px 0 0;padding-left:16px;border-left:4px solid #0b8197;line-height:1.6}.muted{margin:22px 0 0;color:#737b81;font-size:13px;line-height:1.5}@media(max-width:520px){.row{grid-template-columns:1fr;gap:6px}.brand{margin-bottom:32px}h1{font-size:28px}}</style></head>
<body><main><div class="brand">{{.Logo}}</div><h1>Approve this connection in LCTK</h1><p class="lead">A coding client is requesting access to one exact local project.</p><section class="card" aria-label="Connection request"><div class="row"><span class="label">Client</span><span class="value">{{.Client}}</span></div><div class="row"><span class="label">Project</span><span class="value">{{.Project}}</span></div><div class="row"><span class="label">Callback</span><code class="value">{{.Callback}}</code></div></section><p class="instruction">Open the native LCTK window and select <strong>Approve</strong> or <strong>Deny</strong>. This page will continue automatically.</p><p class="muted">No LCTK administration is available in this browser page.</p></main></body></html>`))
