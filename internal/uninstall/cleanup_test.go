package uninstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeCleanupRemovesOnlyLCTKMachineResidue(t *testing.T) {
	runtimeData := filepath.Join(t.TempDir(), "runtime data")
	userHome := filepath.Join(t.TempDir(), "user")
	machineData := filepath.Join(runtimeData, "containers", "podman", "machine", "wsl")
	config := filepath.Join(userHome, ".config", "containers", "podman", "machine", "wsl")
	paths := []string{
		filepath.Join(machineData, "wsldist", "lctk-runtime", "ext4.vhdx"),
		filepath.Join(machineData, "lctk-runtime", "win-sshproxy.tid"),
		filepath.Join(machineData, "lctk-runtime-amd64"),
		filepath.Join(config, "lctk-runtime.json"),
		filepath.Join(config, "lctk-runtime.ign"),
		filepath.Join(config, "lctk-runtime.lock"),
	}
	for _, path := range []string{
		filepath.Join(runtimeData, "containers", "podman", "machine", "machine"),
		filepath.Join(runtimeData, "containers", "podman", "machine", "machine.pub"),
		filepath.Join(runtimeData, "containers", "podman", "machine", "port-alloc.dat"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("scaffold"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(runtimeData, "containers", "storage", "containers"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	neighbor := filepath.Join(config, "personal-podman.json")
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupManagedRuntimeResidue(runtimeData, userHome); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("managed residue still exists at %q: %v", path, err)
		}
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "keep" {
		t.Fatalf("neighboring Podman configuration was changed: %q, %v", content, err)
	}
	if _, err := os.Stat(runtimeData); !os.IsNotExist(err) {
		t.Fatalf("empty managed runtime root still exists: %v", err)
	}
}

func TestRuntimeCleanupPreservesUnknownContainerData(t *testing.T) {
	runtimeData := filepath.Join(t.TempDir(), "runtime data")
	userHome := filepath.Join(t.TempDir(), "user")
	neighbor := filepath.Join(runtimeData, "containers", "storage", "volumes", "personal", "data")
	if err := os.MkdirAll(filepath.Dir(neighbor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupManagedRuntimeResidue(runtimeData, userHome); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(neighbor); err != nil || string(content) != "keep" {
		t.Fatalf("unknown neighboring runtime data was changed: %q, %v", content, err)
	}
}
