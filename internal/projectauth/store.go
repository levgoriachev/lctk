// Package projectauth owns OAuth clients, owner approval requests, and the
// project-bound credentials accepted by the MCP gateway.
package projectauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

const (
	// ScopeProject is the least-privilege scope required by every project route.
	ScopeProject = "lctk:project"
	// FileName deliberately differs from the superseded recoverable grant file.
	FileName = "oauth.json"
	// LegacyFileName is removed when the new authority opens successfully so an
	// obsolete recoverable bearer token is not left behind.
	LegacyFileName = "grants.json"
	// SchemaVersion starts a new, intentionally incompatible credential format.
	SchemaVersion = 1
	accessTTL     = 15 * time.Minute
	refreshTTL    = 30 * 24 * time.Hour
	requestTTL    = 5 * time.Minute
	codeTTL       = time.Minute
)

var (
	ErrClientNotFound    = errors.New("OAuth client not found")
	ErrRequestNotFound   = errors.New("authorization request not found")
	ErrRequestNotPending = errors.New("authorization request is not pending")
	ErrInvalidGrant      = errors.New("invalid OAuth grant")
	ErrTokenNotFound     = errors.New("access token not found")
	ErrTokenExpired      = errors.New("access token expired")
	ErrWrongProject      = errors.New("token is bound to another project")
)

// Client is a dynamically registered public OAuth client. No client secret is
// issued because desktop MCP clients cannot keep one confidential.
type Client struct {
	ID                      string    `json:"client_id"`
	Name                    string    `json:"client_name"`
	RedirectURIs            []string  `json:"redirect_uris"`
	GrantTypes              []string  `json:"grant_types"`
	ResponseTypes           []string  `json:"response_types"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method"`
	CreatedAt               time.Time `json:"created_at"`
}

// Registration is the accepted RFC 7591 subset for a local public client.
type Registration struct {
	Name                    string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// AccessToken is a persisted hash and expiry, never the bearer value.
type AccessToken struct {
	Hash      string    `json:"hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Authorization is one owner-approved client connection. It is the revocable
// unit shown by the native administrator.
type Authorization struct {
	ID               string        `json:"id"`
	ClientID         string        `json:"client_id"`
	ClientName       string        `json:"client_name"`
	ProjectID        string        `json:"project_id"`
	Resource         string        `json:"resource"`
	Scopes           []string      `json:"scopes"`
	IssuedAt         time.Time     `json:"issued_at"`
	AccessTokens     []AccessToken `json:"access_tokens"`
	RefreshTokenHash string        `json:"refresh_token_hash"`
	RefreshExpiresAt time.Time     `json:"refresh_expires_at"`
	Revoked          bool          `json:"revoked,omitempty"`
}

// Request is an authorization decision awaiting the machine owner.
type Request struct {
	ID            string    `json:"id"`
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	ProjectID     string    `json:"project_id"`
	Resource      string    `json:"resource"`
	RedirectURI   string    `json:"redirect_uri"`
	Scopes        []string  `json:"scopes"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Status        string    `json:"status"`
	state         string
	codeChallenge string
	code          string
	codeExpiresAt time.Time
	consumed      bool
}

// BeginRequest contains the fully validated OAuth authorization parameters.
type BeginRequest struct {
	ClientID      string
	ProjectID     string
	Resource      string
	RedirectURI   string
	Scopes        []string
	State         string
	CodeChallenge string
}

// TokenPair contains new bearer values returned exactly once to the OAuth
// client. Store persists only their hashes.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    time.Duration
	Scopes       []string
}

type document struct {
	SchemaVersion  int             `json:"schema_version"`
	Clients        []Client        `json:"clients"`
	Authorizations []Authorization `json:"authorizations"`
}

// Store is the daemon-owned authorization authority. All mutations are locked
// and persisted atomically before success is reported.
type Store struct {
	mu             sync.Mutex
	path           string
	clients        []Client
	authorizations []Authorization
	requests       map[string]*Request
}

// Open loads the production OAuth store from the owner-only LCTK home.
func Open() (*Store, error) {
	dir, err := lctkhome.EnsureDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, FileName))
}

// OpenAt loads a store at an explicit path for isolated tests.
func OpenAt(path string) (*Store, error) {
	store := &Store{path: path, requests: make(map[string]*Request)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := removeLegacyStore(path); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read OAuth store %q: %w", path, err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OAuth store %q is not valid JSON: %w", path, err)
	}
	if doc.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("OAuth store %q has unsupported schema version %d", path, doc.SchemaVersion)
	}
	store.clients, store.authorizations = doc.Clients, doc.Authorizations
	if store.clients == nil {
		store.clients = []Client{}
	}
	if store.authorizations == nil {
		store.authorizations = []Authorization{}
	}
	if err := store.validateLocked(); err != nil {
		return nil, fmt.Errorf("OAuth store %q: %w", path, err)
	}
	if err := removeLegacyStore(path); err != nil {
		return nil, err
	}
	return store, nil
}

func removeLegacyStore(path string) error {
	if filepath.Base(path) != FileName {
		return nil
	}
	legacy := filepath.Join(filepath.Dir(path), LegacyFileName)
	if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove superseded recoverable grant store %q: %w", legacy, err)
	}
	return nil
}

// RegisterClient validates and persists one public OAuth client.
func (s *Store) RegisterClient(reg Registration, now time.Time) (Client, error) {
	reg.Name = strings.TrimSpace(reg.Name)
	if reg.Name == "" || len(reg.Name) > 200 {
		return Client{}, errors.New("client_name must contain 1 to 200 characters")
	}
	if len(reg.RedirectURIs) == 0 || len(reg.RedirectURIs) > 10 {
		return Client{}, errors.New("redirect_uris must contain 1 to 10 entries")
	}
	redirects := make([]string, 0, len(reg.RedirectURIs))
	seen := map[string]bool{}
	for _, raw := range reg.RedirectURIs {
		canonical, err := validateRedirectURI(raw)
		if err != nil {
			return Client{}, err
		}
		if !seen[canonical] {
			redirects, seen[canonical] = append(redirects, canonical), true
		}
	}
	if len(reg.GrantTypes) == 0 {
		reg.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(reg.ResponseTypes) == 0 {
		reg.ResponseTypes = []string{"code"}
	}
	if !sameStringSet(reg.GrantTypes, []string{"authorization_code", "refresh_token"}) {
		return Client{}, errors.New("grant_types must contain authorization_code and refresh_token")
	}
	if !sameStringSet(reg.ResponseTypes, []string{"code"}) {
		return Client{}, errors.New("only the code response type is supported")
	}
	authMethod := strings.TrimSpace(reg.TokenEndpointAuthMethod)
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" {
		return Client{}, errors.New("only token_endpoint_auth_method none is supported")
	}
	id, err := randomValue("client-", 18)
	if err != nil {
		return Client{}, err
	}
	client := Client{ID: id, Name: reg.Name, RedirectURIs: redirects, GrantTypes: append([]string(nil), reg.GrantTypes...), ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none", CreatedAt: now.UTC().Truncate(time.Second)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = append(s.clients, client)
	if err := s.saveLocked(); err != nil {
		s.clients = s.clients[:len(s.clients)-1]
		return Client{}, err
	}
	return client, nil
}

// ValidateClientRedirect establishes whether an authorization error may be
// redirected to the supplied URI without creating an open redirect.
func (s *Store) ValidateClientRedirect(clientID, redirectURI string) (Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client, ok := s.clientLocked(clientID)
	if !ok {
		return Client{}, ErrClientNotFound
	}
	if !contains(client.RedirectURIs, redirectURI) {
		return Client{}, errors.New("redirect_uri does not exactly match the registered client")
	}
	return client, nil
}

// Begin creates a short-lived owner approval request after validating the
// client registration and exact redirect URI.
func (s *Store) Begin(input BeginRequest, now time.Time) (Request, error) {
	if input.CodeChallenge == "" {
		return Request{}, errors.New("S256 code_challenge is required")
	}
	if !validS256Challenge(input.CodeChallenge) {
		return Request{}, errors.New("code_challenge is not a valid S256 value")
	}
	if !sameStringSet(input.Scopes, []string{ScopeProject}) {
		return Request{}, errors.New("scope must be exactly lctk:project")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	client, ok := s.clientLocked(input.ClientID)
	if !ok {
		return Request{}, ErrClientNotFound
	}
	if !contains(client.RedirectURIs, input.RedirectURI) {
		return Request{}, errors.New("redirect_uri does not exactly match the registered client")
	}
	id, err := randomValue("request-", 18)
	if err != nil {
		return Request{}, err
	}
	request := &Request{ID: id, ClientID: client.ID, ClientName: client.Name, ProjectID: input.ProjectID, Resource: input.Resource, RedirectURI: input.RedirectURI, Scopes: []string{ScopeProject}, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(requestTTL), Status: "pending", state: input.State, codeChallenge: input.CodeChallenge}
	s.requests[id] = request
	return *request, nil
}

// Pending returns unexpired requests in deterministic creation order.
func (s *Store) Pending(now time.Time) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRequestsLocked(now)
	out := make([]Request, 0)
	for _, request := range s.requests {
		if request.Status == "pending" {
			out = append(out, *request)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Approve binds a single-use authorization code to a pending request.
func (s *Store) Approve(id string, now time.Time) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRequestsLocked(now)
	request, ok := s.requests[id]
	if !ok {
		return Request{}, ErrRequestNotFound
	}
	if request.Status != "pending" {
		return Request{}, ErrRequestNotPending
	}
	code, err := randomValue("code-", 32)
	if err != nil {
		return Request{}, err
	}
	request.Status, request.code, request.codeExpiresAt = "approved", code, now.UTC().Add(codeTTL)
	return *request, nil
}

// Deny marks a pending request as refused by the owner.
func (s *Store) Deny(id string, now time.Time) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRequestsLocked(now)
	request, ok := s.requests[id]
	if !ok {
		return Request{}, ErrRequestNotFound
	}
	if request.Status != "pending" {
		return Request{}, ErrRequestNotPending
	}
	request.Status = "denied"
	return *request, nil
}

// RequestState returns the public browser state plus the redirect result.
func (s *Store) RequestState(id string, now time.Time) (Request, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRequestsLocked(now)
	request, ok := s.requests[id]
	if !ok {
		return Request{}, "", "", ErrRequestNotFound
	}
	return *request, request.code, request.state, nil
}

// ExchangeCode verifies every authorization-code binding and persists a new
// revocable authorization before returning bearer values.
func (s *Store) ExchangeCode(code, clientID, redirectURI, resource, verifier string, now time.Time) (TokenPair, error) {
	if !validVerifier(verifier) {
		return TokenPair{}, ErrInvalidGrant
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireRequestsLocked(now)
	var request *Request
	for _, candidate := range s.requests {
		if secureEqual(candidate.code, code) {
			request = candidate
		}
	}
	if request == nil || request.Status != "approved" || request.consumed || now.After(request.codeExpiresAt) || request.ClientID != clientID || request.RedirectURI != redirectURI || request.Resource != resource || !secureEqual(pkceChallenge(verifier), request.codeChallenge) {
		return TokenPair{}, ErrInvalidGrant
	}
	pair, accessHash, refreshHash, err := newTokenPair(request.Scopes)
	if err != nil {
		return TokenPair{}, err
	}
	id, err := randomValue("authorization-", 12)
	if err != nil {
		return TokenPair{}, err
	}
	authorization := Authorization{ID: id, ClientID: request.ClientID, ClientName: request.ClientName, ProjectID: request.ProjectID, Resource: request.Resource, Scopes: append([]string(nil), request.Scopes...), IssuedAt: now.UTC().Truncate(time.Second), AccessTokens: []AccessToken{{Hash: accessHash, ExpiresAt: now.UTC().Add(accessTTL)}}, RefreshTokenHash: refreshHash, RefreshExpiresAt: now.UTC().Add(refreshTTL)}
	s.authorizations = append(s.authorizations, authorization)
	request.consumed = true
	if err := s.saveLocked(); err != nil {
		s.authorizations = s.authorizations[:len(s.authorizations)-1]
		request.consumed = false
		return TokenPair{}, err
	}
	return pair, nil
}

// Refresh rotates the refresh token and adds a new short-lived access token.
func (s *Store) Refresh(refreshToken, clientID, resource string, scopes []string, now time.Time) (TokenPair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := tokenHash(refreshToken)
	for i := range s.authorizations {
		auth := &s.authorizations[i]
		if !secureEqual(auth.RefreshTokenHash, hash) {
			continue
		}
		if auth.Revoked || now.After(auth.RefreshExpiresAt) || auth.ClientID != clientID || auth.Resource != resource || (len(scopes) > 0 && !sameStringSet(scopes, auth.Scopes)) {
			return TokenPair{}, ErrInvalidGrant
		}
		pair, accessHash, refreshHash, err := newTokenPair(auth.Scopes)
		if err != nil {
			return TokenPair{}, err
		}
		auth.AccessTokens = append(unexpiredTokens(auth.AccessTokens, now), AccessToken{Hash: accessHash, ExpiresAt: now.UTC().Add(accessTTL)})
		oldRefresh := auth.RefreshTokenHash
		auth.RefreshTokenHash = refreshHash
		if err := s.saveLocked(); err != nil {
			auth.RefreshTokenHash = oldRefresh
			auth.AccessTokens = auth.AccessTokens[:len(auth.AccessTokens)-1]
			return TokenPair{}, err
		}
		return pair, nil
	}
	return TokenPair{}, ErrInvalidGrant
}

// ResolveAccessToken validates an opaque access token against the exact route
// project and RFC 8707 resource audience.
func (s *Store) ResolveAccessToken(token, projectID, resource string, now time.Time) (Authorization, error) {
	hash := tokenHash(strings.TrimSpace(token))
	if token == "" {
		return Authorization{}, ErrTokenNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *Authorization
	expired := false
	for i := range s.authorizations {
		for _, access := range s.authorizations[i].AccessTokens {
			if secureEqual(access.Hash, hash) {
				matched = &s.authorizations[i]
				expired = now.After(access.ExpiresAt)
			}
		}
	}
	if matched == nil || matched.Revoked {
		return Authorization{}, ErrTokenNotFound
	}
	if expired {
		return Authorization{}, ErrTokenExpired
	}
	if matched.ProjectID != projectID || matched.Resource != resource {
		return Authorization{}, ErrWrongProject
	}
	return *matched, nil
}

// List returns redacted authorizations for the native administrator.
func (s *Store) List() []Authorization {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Authorization, len(s.authorizations))
	copy(out, s.authorizations)
	for i := range out {
		out[i].AccessTokens = nil
		out[i].RefreshTokenHash = ""
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Revoke invalidates one exact authorization.
func (s *Store) Revoke(id string) (Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.authorizations {
		if s.authorizations[i].ID != id {
			continue
		}
		old := s.authorizations[i].Revoked
		s.authorizations[i].Revoked = true
		if err := s.saveLocked(); err != nil {
			s.authorizations[i].Revoked = old
			return Authorization{}, err
		}
		return s.authorizations[i], nil
	}
	return Authorization{}, errors.New("authorization not found")
}

// RevokeForProject invalidates every client authorization for a removed project.
func (s *Store) RevokeForProject(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.authorizations {
		if s.authorizations[i].ProjectID == projectID && !s.authorizations[i].Revoked {
			s.authorizations[i].Revoked, changed = true, true
		}
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) clientLocked(id string) (Client, bool) {
	for _, client := range s.clients {
		if client.ID == id {
			return client, true
		}
	}
	return Client{}, false
}
func (s *Store) expireRequestsLocked(now time.Time) {
	for id, request := range s.requests {
		if now.After(request.ExpiresAt) && request.Status == "pending" {
			request.Status = "expired"
		}
		if now.After(request.ExpiresAt.Add(10 * time.Minute)) {
			delete(s.requests, id)
		}
	}
}

func (s *Store) saveLocked() error {
	if err := s.validateLocked(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create OAuth store directory: %w", err)
	}
	raw, err := json.MarshalIndent(document{SchemaVersion: SchemaVersion, Clients: s.clients, Authorizations: s.authorizations}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode OAuth store: %w", err)
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary OAuth store: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return fmt.Errorf("replace OAuth store: %w", err)
	}
	return nil
}

func (s *Store) validateLocked() error {
	clientIDs := map[string]bool{}
	for _, client := range s.clients {
		if client.ID == "" || clientIDs[client.ID] {
			return errors.New("OAuth client identifiers must be non-empty and unique")
		}
		clientIDs[client.ID] = true
	}
	authIDs := map[string]bool{}
	for _, auth := range s.authorizations {
		if auth.ID == "" || authIDs[auth.ID] {
			return errors.New("authorization identifiers must be non-empty and unique")
		}
		if !clientIDs[auth.ClientID] {
			return fmt.Errorf("authorization %q names an unknown client", auth.ID)
		}
		if auth.ProjectID == "" || auth.Resource == "" || auth.RefreshTokenHash == "" {
			return fmt.Errorf("authorization %q is incomplete", auth.ID)
		}
		authIDs[auth.ID] = true
	}
	return nil
}

func validateRedirectURI(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("invalid redirect_uri %q", raw)
	}
	if parsed.Scheme == "https" {
		return parsed.String(), nil
	}
	host := parsed.Hostname()
	if parsed.Scheme != "http" || !(strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()) {
		return "", errors.New("redirect_uri must use HTTPS or HTTP on a loopback host")
	}
	return parsed.String(), nil
}

func validVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}

func validS256Challenge(value string) bool {
	if len(value) != 43 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == sha256.Size
}
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func secureEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
func unexpiredTokens(tokens []AccessToken, now time.Time) []AccessToken {
	out := tokens[:0]
	for _, token := range tokens {
		if now.Before(token.ExpiresAt) {
			out = append(out, token)
		}
	}
	return out
}
func randomValue(prefix string, bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate OAuth value: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
func newTokenPair(scopes []string) (TokenPair, string, string, error) {
	access, err := randomValue("access-", 32)
	if err != nil {
		return TokenPair{}, "", "", err
	}
	refresh, err := randomValue("refresh-", 32)
	if err != nil {
		return TokenPair{}, "", "", err
	}
	return TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresIn: accessTTL, Scopes: append([]string(nil), scopes...)}, tokenHash(access), tokenHash(refresh), nil
}
