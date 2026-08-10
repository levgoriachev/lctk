package adminsession

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	store, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

// signIn performs the exchange and returns a request already carrying the
// session, so each test can start from "an administrator is signed in".
func signIn(t *testing.T, store *Store) (*http.Request, string) {
	t.Helper()
	token, csrf, err := store.Exchange(store.Code())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4444/admin/projects", nil)
	request.Header.Set(HeaderSession, token)
	return request, csrf
}

func TestASessionIsEstablishedByExchangingTheCode(t *testing.T) {
	store, _ := newStore(t)
	request, csrf := signIn(t, store)

	got, err := store.Authenticate(request)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got != csrf {
		t.Fatal("the session returned a different CSRF token than the exchange did")
	}
}

// Spending the code on first use prevents a stale owner-only document from
// establishing a second native administrator session unnoticed.
func TestAnExchangeCodeIsGoodExactlyOnce(t *testing.T) {
	store, _ := newStore(t)
	code := store.Code()

	if _, _, err := store.Exchange(code); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if _, _, err := store.Exchange(code); !errors.Is(err, ErrBadCode) {
		t.Fatalf("second exchange with the same code returned %v, want it refused", err)
	}
	if store.Code() == code {
		t.Fatal("the spent code is still the current one")
	}
}

func TestAnEmptyOrWrongCodeIsRefused(t *testing.T) {
	store, _ := newStore(t)
	for _, code := range []string{"", "wrong", store.Code() + "x"} {
		if _, _, err := store.Exchange(code); !errors.Is(err, ErrBadCode) {
			t.Errorf("Exchange(%q) = %v, want it refused", code, err)
		}
	}
}

func TestARequestWithoutASessionIsRefused(t *testing.T) {
	store, _ := newStore(t)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4444/admin/projects", nil)

	if _, err := store.Authenticate(request); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Authenticate without a session header = %v, want %v", err, ErrNoSession)
	}
}

// The DNS-rebinding defence. A page on an attacker's domain can make that
// hostname resolve to 127.0.0.1; the Host header still carries the attacker's
// name, and that is what refuses the request.
func TestARequestNamingAnotherHostIsRefusedBeforeTheCredential(t *testing.T) {
	store, _ := newStore(t)
	request, _ := signIn(t, store)
	request.Host = "lctk.attacker.example"

	if _, err := store.Authenticate(request); !errors.Is(err, ErrForeignHost) {
		t.Fatalf("Authenticate with a foreign Host = %v, want %v", err, ErrForeignHost)
	}
}

func TestLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:4444":         true,
		"127.0.0.1":              true,
		"localhost:4444":         true,
		"LOCALHOST":              true,
		"[::1]:4444":             true,
		"127.9.9.9:4444":         true,
		"192.168.1.10:4444":      false,
		"lctk.attacker.example":  false,
		"attacker.localhost:444": false,
		"":                       false,
	}
	for host, want := range cases {
		if got := LoopbackHost(host); got != want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// A state-changing request needs the header a cross-origin page cannot produce.
func TestAStateChangingRequestMustEchoTheCsrfToken(t *testing.T) {
	store, _ := newStore(t)
	request, csrf := signIn(t, store)

	if err := store.Authorize(request); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Authorize without the header = %v, want it refused", err)
	}

	request.Header.Set(HeaderCSRF, "guessed")
	if err := store.Authorize(request); !errors.Is(err, ErrNoSession) {
		t.Fatal("Authorize accepted a wrong CSRF token")
	}

	request.Header.Set(HeaderCSRF, csrf)
	if err := store.Authorize(request); err != nil {
		t.Fatalf("Authorize with the right token: %v", err)
	}
}

func TestASessionExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	now := time.Unix(1700000000, 0).UTC()
	store, err := New(Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	request, _ := signIn(t, store)
	if _, err := store.Authenticate(request); err != nil {
		t.Fatalf("a fresh session was refused: %v", err)
	}

	now = now.Add(SessionLifetime + time.Minute)
	if _, err := store.Authenticate(request); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an expired session was accepted: %v", err)
	}
}

func TestRevokeEndsASessionAtOnce(t *testing.T) {
	store, _ := newStore(t)
	token, _, err := store.Exchange(store.Code())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4444/admin/projects", nil)
	request.Header.Set(HeaderSession, token)

	store.Revoke(token)
	if _, err := store.Authenticate(request); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a revoked session was accepted: %v", err)
	}
}

// A code left behind by a stopped daemon would sign someone in to the next one.
func TestTheCodeIsRemovedWhenTheDaemonStops(t *testing.T) {
	store, path := newStore(t)

	if _, err := ReadCode(path); err != nil {
		t.Fatalf("ReadCode while running: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ReadCode(path); err == nil {
		t.Fatal("the exchange code survived the daemon that issued it")
	}
}

// The CLI is another process, so the code has to travel through the file.
func TestTheCliReadsTheCurrentCode(t *testing.T) {
	store, path := newStore(t)

	read, err := ReadCode(path)
	if err != nil {
		t.Fatalf("ReadCode: %v", err)
	}
	if read != store.Code() {
		t.Fatal("the stored code is not the current one")
	}

	if _, _, err := store.Exchange(read); err != nil {
		t.Fatal(err)
	}
	rotated, err := ReadCode(path)
	if err != nil {
		t.Fatalf("ReadCode after an exchange: %v", err)
	}
	if rotated == read {
		t.Fatal("the stored code was not replaced after being spent")
	}
	if rotated != store.Code() {
		t.Fatal("the stored code and the live one disagree")
	}
}
