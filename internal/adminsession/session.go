// Package adminsession authenticates the local administrator.
//
// The admin surface is a different thing from a project endpoint and is
// deliberately built as one. A project OAuth token opens a project's tools; it
// must never open the administrator's. Nothing here reads a project token, and
// internal/gateway never reads an admin session, so the separation is structural
// rather than a check somebody has to remember.
//
// The native administrator reads a one-time exchange code from the owner-only
// LCTK home, spends it directly, and keeps the returned session and CSRF tokens
// only in process memory. The Host check still refuses DNS rebinding before any
// credential is consulted, and the two independent headers keep an ambient web
// page from producing an authorized state-changing request.
package adminsession

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

// FileName holds the exchange code between the daemon and the installed native
// administrator process.
const FileName = "admin.json"

// HeaderSession carries the native process's in-memory administrator session.
const HeaderSession = "X-LCTK-Session"

// HeaderCSRF carries the token a state-changing request must echo.
const HeaderCSRF = "X-LCTK-Admin"

// SessionLifetime bounds how long a native administrator window stays signed
// in. It covers a working day without creating a durable credential.
const SessionLifetime = 12 * time.Hour

// Errors a caller can act on.
var (
	ErrNoSession   = errors.New("no admin session")
	ErrBadCode     = errors.New("the exchange code is not valid")
	ErrForeignHost = errors.New("the request does not name a loopback host")
)

// Store holds the daemon's admin credentials in memory, with the exchange code
// mirrored to the LCTK home so the native client can read it.
type Store struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	code     string
	sessions map[string]session
}

type session struct {
	csrf    string
	expires time.Time
}

// document is what the CLI reads.
type document struct {
	SchemaVersion int    `json:"schema_version"`
	Code          string `json:"code"`
	IssuedAt      string `json:"issued_at"`
}

// Options configures a store.
type Options struct {
	// Path is the exchange-code document. Empty means the per-user default.
	Path string
	Now  func() time.Time
}

// Path returns the exchange-code document without creating anything.
func Path() (string, error) {
	dir, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// New issues a fresh exchange code and writes it where the CLI can read it.
//
// The code is new on every daemon start. A code left behind by a previous run
// would let anything that captured it sign in to this one, and the CLI is always
// able to read the current one, so nothing is gained by keeping the old.
func New(options Options) (*Store, error) {
	path := options.Path
	if path == "" {
		resolved, err := Path()
		if err != nil {
			return nil, err
		}
		path = resolved
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	store := &Store{
		path:     path,
		now:      options.Now,
		code:     secret(),
		sessions: map[string]session{},
	}
	if err := store.write(); err != nil {
		return nil, err
	}
	return store, nil
}

// Code is the current exchange code. It is returned to the installed native
// client and is never logged or placed in a URL.
func (s *Store) Code() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

// Exchange spends a code and returns a session token and its CSRF token.
//
// The comparison is constant-time, and success replaces the code. A code is good
// for exactly one session, so a code that leaks after being used is worth
// nothing, and a second use is a signal rather than an unnoticed duplicate.
func (s *Store) Exchange(code string) (token, csrf string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.code)) != 1 {
		return "", "", ErrBadCode
	}

	s.code = secret()
	token, csrf = secret(), secret()
	s.sessions[token] = session{csrf: csrf, expires: s.now().Add(SessionLifetime)}
	s.sweep()

	if err := s.writeLocked(); err != nil {
		return "", "", err
	}
	return token, csrf, nil
}

// Authenticate checks a request and returns its CSRF token.
//
// Every request is checked, not only the state-changing ones: reading the
// project list is itself something a stranger should not be able to do.
func (s *Store) Authenticate(r *http.Request) (string, error) {
	if !LoopbackHost(r.Host) {
		return "", ErrForeignHost
	}

	token := strings.TrimSpace(r.Header.Get(HeaderSession))
	if token == "" {
		return "", ErrNoSession
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	found, ok := s.sessions[token]
	if !ok || s.now().After(found.expires) {
		delete(s.sessions, token)
		return "", ErrNoSession
	}
	return found.csrf, nil
}

// Authorize is Authenticate plus the CSRF check a state-changing request needs.
//
// The header is the part a cross-origin page cannot produce. It cannot read the
// token, because reading it would require a response the same-origin policy will
// not hand over.
func (s *Store) Authorize(r *http.Request) error {
	csrf, err := s.Authenticate(r)
	if err != nil {
		return err
	}
	presented := r.Header.Get(HeaderCSRF)
	if subtle.ConstantTimeCompare([]byte(presented), []byte(csrf)) != 1 {
		return ErrNoSession
	}
	return nil
}

// Token returns the presented native session token for explicit sign-out.
func Token(r *http.Request) string { return strings.TrimSpace(r.Header.Get(HeaderSession)) }

// LoopbackHost reports whether a Host header names this machine.
//
// This is the defence against DNS rebinding: a page on an attacker's domain can
// make its hostname resolve to 127.0.0.1, and the browser will then treat
// requests to it as same-origin with the attacker's page. The Host header still
// carries the attacker's name, so checking it refuses the request before any
// credential is consulted.
func LoopbackHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	address := net.ParseIP(name)
	return address != nil && address.IsLoopback()
}

// Revoke ends a session.
func (s *Store) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// Close removes the exchange code from disk. A stopped daemon leaves nothing
// behind that would sign anyone in to the next one.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]session{}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) sweep() {
	now := s.now()
	for token, found := range s.sessions {
		if now.After(found.expires) {
			delete(s.sessions, token)
		}
	}
}

func (s *Store) write() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked()
}

func (s *Store) writeLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %q: %w", dir, err)
	}

	encoded, err := json.MarshalIndent(document{
		SchemaVersion: 1,
		Code:          s.code,
		IssuedAt:      s.now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the admin session document: %w", err)
	}

	temp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary admin session document: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)

	if _, err := temp.Write(append(encoded, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("write the temporary admin session document: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close the temporary admin session document: %w", err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("restrict the temporary admin session document: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace the admin session document: %w", err)
	}
	return nil
}

// ReadCode reads the current exchange code, for a CLI in another process.
func ReadCode(path string) (string, error) {
	if path == "" {
		resolved, err := Path()
		if err != nil {
			return "", err
		}
		path = resolved
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no admin session is available; start the daemon with lctk daemon")
		}
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	var stored document
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", fmt.Errorf("%q is not valid JSON: %w", path, err)
	}
	if stored.Code == "" {
		return "", fmt.Errorf("%q holds no exchange code", path)
	}
	return stored.Code, nil
}

func secret() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail on either target platform; if it ever did,
		// continuing with a predictable credential would be far worse than not
		// starting at all.
		panic("lctk: the system random source is unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
