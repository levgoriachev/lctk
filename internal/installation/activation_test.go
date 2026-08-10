package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

func TestInstallActivatesVerifiedVersionsAndRollback(t *testing.T) {
	home := t.TempDir()
	first := []byte("first signed host core")
	second := []byte("second signed host core")
	server := newArtifactServer(t, map[string][]byte{"/first": first, "/second": second})
	manager := newTestManager(home, server.Client())

	active, err := manager.Install(context.Background(), hostManifest("1.0.0", server.URL+"/first", first))
	if err != nil {
		t.Fatalf("Install first: %v", err)
	}
	if active.ActiveVersion != "1.0.0" || active.PreviousVersion != "" {
		t.Fatalf("first activation = %+v", active)
	}

	active, err = manager.Install(context.Background(), hostManifest("1.1.0", server.URL+"/second", second))
	if err != nil {
		t.Fatalf("Install second: %v", err)
	}
	if active.ActiveVersion != "1.1.0" || active.PreviousVersion != "1.0.0" {
		t.Fatalf("second activation = %+v", active)
	}

	rolledBack, err := manager.Rollback()
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack.ActiveVersion != "1.0.0" || rolledBack.PreviousVersion != "1.1.0" {
		t.Fatalf("rollback activation = %+v", rolledBack)
	}
}

func TestInstallRepairsTheSameImmutableVersionWithoutDestroyingRollback(t *testing.T) {
	home := t.TempDir()
	first := []byte("first signed host core")
	second := []byte("second signed host core")
	server := newArtifactServer(t, map[string][]byte{"/first": first, "/second": second})
	manager := newTestManager(home, server.Client())
	if _, err := manager.Install(t.Context(), hostManifest("1.0.0", server.URL+"/first", first)); err != nil {
		t.Fatal(err)
	}
	active, err := manager.Install(t.Context(), hostManifest("1.1.0", server.URL+"/second", second))
	if err != nil {
		t.Fatal(err)
	}
	target, err := Resolve(home, active.ActiveExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("corrupt"), 0o700); err != nil {
		t.Fatal(err)
	}
	repaired, err := manager.Install(t.Context(), hostManifest("1.1.0", server.URL+"/second", second))
	if err != nil {
		t.Fatal(err)
	}
	if repaired.PreviousVersion != "1.0.0" || repaired.PreviousExecutable != active.PreviousExecutable {
		t.Fatalf("same-version repair destroyed rollback: before=%+v after=%+v", active, repaired)
	}
	if err := verifyFile(target, int64(len(second)), digestOf(second)); err != nil {
		t.Fatalf("same-version repair did not restore the active core: %v", err)
	}
}

func TestInstallRejectsDifferentBytesUnderTheSameVersion(t *testing.T) {
	home := t.TempDir()
	first := []byte("first identity")
	rebuilt := []byte("different rebuild")
	server := newArtifactServer(t, map[string][]byte{"/first": first, "/rebuilt": rebuilt})
	manager := newTestManager(home, server.Client())
	if _, err := manager.Install(t.Context(), hostManifest("1.0.0", server.URL+"/first", first)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(t.Context(), hostManifest("1.0.0", server.URL+"/rebuilt", rebuilt)); err == nil {
		t.Fatal("Install accepted different host-core bytes under an installed version")
	}
}

func TestInstallFailsBeforeDownloadWhenDiskIsLow(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested = true
	}))
	t.Cleanup(server.Close)
	manager := newTestManager(t.TempDir(), server.Client())
	manager.Available = func(string) (uint64, error) { return 1, nil }

	_, err := manager.Install(context.Background(), hostManifest("1.0.0", server.URL, []byte("core")))
	if err == nil || !strings.Contains(err.Error(), "safety margin") {
		t.Fatalf("Install error = %v, want disk refusal", err)
	}
	if requested {
		t.Fatal("host core was downloaded after the disk preflight failed")
	}
}

func TestInstallRejectsDigestMismatchAndCorruptRollback(t *testing.T) {
	home := t.TempDir()
	server := newArtifactServer(t, map[string][]byte{"/first": []byte("wrong bytes")})
	manager := newTestManager(home, server.Client())
	manifest := hostManifest("1.0.0", server.URL+"/first", []byte("expected bytes"))
	if _, err := manager.Install(context.Background(), manifest); err == nil {
		t.Fatal("Install accepted bytes that differed from the signed identity")
	}
	if _, err := os.Stat(filepath.Join(home, ActivationFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("activation exists after failed install: %v", err)
	}

	first := []byte("first")
	second := []byte("second")
	server.Config.Handler = http.HandlerFunc(func(output http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/first":
			_, _ = output.Write(first)
		case "/second":
			_, _ = output.Write(second)
		default:
			http.NotFound(output, request)
		}
	})
	if _, err := manager.Install(context.Background(), hostManifest("1.0.0", server.URL+"/first", first)); err != nil {
		t.Fatalf("Install first valid version: %v", err)
	}
	if _, err := manager.Install(context.Background(), hostManifest("1.1.0", server.URL+"/second", second)); err != nil {
		t.Fatalf("Install second valid version: %v", err)
	}
	activation, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := Resolve(home, activation.PreviousExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyRollback(); err == nil {
		t.Fatal("rollback preflight accepted a corrupt previous executable")
	}
	if _, err := manager.Rollback(); err == nil {
		t.Fatal("Rollback activated a corrupt previous executable")
	}
	stillActive, err := Load(home)
	if err != nil || stillActive.ActiveVersion != "1.1.0" {
		t.Fatalf("activation changed after refused rollback: %+v, %v", stillActive, err)
	}
}

func TestLoadRejectsEscapingAndIncompleteActivation(t *testing.T) {
	home := t.TempDir()
	document := Activation{
		SchemaVersion:    SchemaVersion,
		ActiveVersion:    "1.0.0",
		ActiveExecutable: "../outside",
		ActiveSHA256:     strings.Repeat("0", 64),
		ActiveBytes:      1,
	}
	encoded, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(home, ActivationFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home); err == nil {
		t.Fatal("Load accepted an executable outside the installation home")
	}
}

func newTestManager(home string, client *http.Client) *Manager {
	manager := NewManager(home)
	manager.Client = client
	manager.Available = func(string) (uint64, error) { return 1 << 40, nil }
	manager.Run = func(_ context.Context, executable string, _ ...string) ([]byte, error) {
		return json.Marshal(map[string]string{
			"version": filepath.Base(filepath.Dir(executable)),
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
		})
	}
	return manager
}

func hostManifest(version, url string, content []byte) releasebundle.Manifest {
	identity := digestOf(content)
	return releasebundle.Manifest{
		Version: version,
		Artifacts: []releasebundle.Artifact{{
			Name: "lctk-core", Kind: "host-core", OS: runtime.GOOS, Arch: runtime.GOARCH,
			URL: url, Bytes: int64(len(content)), SHA256: identity,
		}},
	}
}

func digestOf(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func newArtifactServer(t *testing.T, artifacts map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(output http.ResponseWriter, request *http.Request) {
		content, found := artifacts[request.URL.Path]
		if !found {
			http.NotFound(output, request)
			return
		}
		_, _ = output.Write(content)
	}))
	t.Cleanup(server.Close)
	return server
}
