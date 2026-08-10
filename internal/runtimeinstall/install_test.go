package runtimeinstall

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type fakeMachine struct {
	calls  [][]string
	exists bool
}

func (m *fakeMachine) Run(_ context.Context, args ...string) (string, string, error) {
	m.calls = append(m.calls, append([]string(nil), args...))
	if args[0] == "inspect" && !m.exists {
		return "", "machine does not exist", os.ErrNotExist
	}
	if args[0] == "init" {
		m.exists = true
	}
	return "", "", nil
}

func TestInstallVerifiesExtractsAndInitializesTheNamedMachine(t *testing.T) {
	archive := podmanArchive(t)
	machineImage := []byte("immutable WSL image")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/client":
			_, _ = writer.Write(archive)
		case "/machine":
			_, _ = writer.Write(machineImage)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	home := t.TempDir()
	machine := &fakeMachine{}
	manager := NewManager(home)
	manager.TargetOS, manager.TargetArch = "windows", "amd64"
	manager.Machine = machine
	manager.Available = func(string) (uint64, error) { return 8 << 30, nil }
	manifest := runtimeManifest(server.URL, archive, machineImage)

	plan, err := manager.Inspect(manifest)
	if err != nil || !plan.Ready || plan.DownloadBytes != int64(len(archive)+len(machineImage)) {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if err := manager.Install(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(home, "runtime", "podman", containerruntime.Version, "bin", "podman.exe")
	if body, err := os.ReadFile(client); err != nil || string(body) != "podman" {
		t.Fatalf("client=%q err=%v", body, err)
	}
	if len(machine.calls) != 4 || machine.calls[0][0] != "stop" || machine.calls[2][0] != "init" || machine.calls[3][0] != "start" {
		t.Fatalf("machine calls = %v", machine.calls)
	}
	if !contains(machine.calls[2], containerruntime.MachineName) || !contains(machine.calls[2], "--rootful") {
		t.Fatalf("machine init is not bounded to LCTK: %v", machine.calls[2])
	}
	fixedTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(client, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(t.Context(), manifest); err != nil {
		t.Fatalf("idempotent repair: %v", err)
	}
	if info, err := os.Stat(client); err != nil || !info.ModTime().Equal(fixedTime) {
		t.Fatalf("verified client was rewritten: time=%v err=%v", info.ModTime(), err)
	}
	if len(machine.calls) != 6 || machine.calls[4][0] != "inspect" || machine.calls[5][0] != "start" {
		t.Fatalf("repair recreated the managed machine: %v", machine.calls)
	}

	second, err := manager.Inspect(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range second.Components {
		if !component.Installed {
			t.Fatalf("component not recognized after install: %+v", component)
		}
	}
}

func TestInstallStopsTheManagedMachineBeforeRepairingALockedHelper(t *testing.T) {
	archive := podmanArchive(t)
	machineImage := []byte("immutable WSL image")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/client" {
			_, _ = writer.Write(archive)
			return
		}
		_, _ = writer.Write(machineImage)
	}))
	defer server.Close()
	home := t.TempDir()
	machine := &fakeMachine{}
	manager := NewManager(home)
	manager.TargetOS, manager.TargetArch = "windows", "amd64"
	manager.Machine = machine
	manager.Available = func(string) (uint64, error) { return 8 << 30, nil }
	manifest := runtimeManifest(server.URL, archive, machineImage)
	if err := manager.Install(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(home, "runtime", "podman", containerruntime.Version, "bin", "win-sshproxy.exe")
	if err := os.WriteFile(helper, []byte("damaged"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(helper); err != nil || string(body) != "ssh" {
		t.Fatalf("repaired helper=%q err=%v", body, err)
	}
	if len(machine.calls) != 7 || machine.calls[4][0] != "stop" || machine.calls[5][0] != "inspect" || machine.calls[6][0] != "start" {
		t.Fatalf("repair did not stop and restart the managed machine: %v", machine.calls)
	}
}

func TestInstallRejectsWrongArtifactIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("wrong")) }))
	defer server.Close()
	manager := NewManager(t.TempDir())
	manager.TargetOS, manager.TargetArch = "windows", "amd64"
	manager.Machine = &fakeMachine{}
	manager.Available = func(string) (uint64, error) { return 8 << 30, nil }
	manifest := runtimeManifest(server.URL, []byte("expected client"), []byte("expected machine"))
	if err := manager.Install(t.Context(), manifest); err == nil || !strings.Contains(err.Error(), "identity differs") {
		t.Fatalf("wrong download identity produced %v", err)
	}
}

func TestInstallClientRejectsUnsafeArchivePaths(t *testing.T) {
	// These names cover both portable ZIP traversal syntax and the Windows path
	// separator ambiguity that has historically enabled archive extraction to
	// escape its intended destination.
	for _, name := range []string{
		"podman-5.8.2/usr/bin/../../../../podman.exe",
		`podman-5.8.2\usr\bin\podman.exe`,
	} {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := zip.NewWriter(&buffer)
			entry, err := writer.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte("untrusted")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			archivePath := filepath.Join(t.TempDir(), "podman.zip")
			if err := os.WriteFile(archivePath, buffer.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			manager := NewManager(t.TempDir())
			if err := manager.installClient(t.Context(), archivePath); err == nil || !strings.Contains(err.Error(), "unsafe path") {
				t.Fatalf("unsafe archive path produced %v", err)
			}
		})
	}
}

func podmanArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"podman-5.8.2/usr/bin/podman.exe":       "podman",
		"podman-5.8.2/usr/bin/gvproxy.exe":      "proxy",
		"podman-5.8.2/usr/bin/win-sshproxy.exe": "ssh",
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func runtimeManifest(base string, client, machine []byte) releasebundle.Manifest {
	return releasebundle.Manifest{Version: "1.0.0", Artifacts: []releasebundle.Artifact{
		artifact("client.zip", "podman-client", "windows", "amd64", base+"/client", client),
		artifact("machine.tar.zst", "podman-machine", "linux", "amd64", base+"/machine", machine),
	}}
}

func artifact(name, kind, targetOS, arch, url string, body []byte) releasebundle.Artifact {
	digest := sha256.Sum256(body)
	return releasebundle.Artifact{Name: name, Kind: kind, OS: targetOS, Arch: arch, URL: url, Bytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
