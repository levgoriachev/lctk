package containerruntime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecutablePathIsInstallationOwned(t *testing.T) {
	t.Setenv(ExecutableOverride, "")
	home := t.TempDir()
	t.Setenv("LCTK_HOME", home)

	path, err := ExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	name := "podman"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	want := filepath.Join(home, "runtime", Provider, Version, "bin", name)
	if path != want {
		t.Fatalf("client path = %q, want %q", path, want)
	}
}

func TestExecutableOverrideIsAbsolute(t *testing.T) {
	t.Setenv(ExecutableOverride, filepath.Join("fixtures", "podman"))
	path, err := ExecutablePath()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || !strings.HasSuffix(path, filepath.Join("fixtures", "podman")) {
		t.Fatalf("override path = %q", path)
	}
}

func TestCommandFailsClosedWhenClientIsMissing(t *testing.T) {
	t.Setenv(ExecutableOverride, filepath.Join(t.TempDir(), "absent-podman"))
	_, err := Command(t.Context(), "info")
	if err == nil || !strings.Contains(err.Error(), ErrClientMissing.Error()) {
		t.Fatalf("Command error = %v", err)
	}
}

func TestCommandPinsTheSelectedRuntimeDataAndPrivateConfigHomes(t *testing.T) {
	client := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(client, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(t.TempDir(), "runtime-data")
	t.Setenv(ExecutableOverride, client)
	installHome := t.TempDir()
	t.Setenv("LCTK_HOME", installHome)
	t.Setenv("LCTK_RUNTIME_DATA_HOME", dataHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "ambient-data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "ambient-config"))

	command, err := Command(t.Context(), "info")
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{
		"XDG_DATA_HOME":   dataHome,
		"XDG_CONFIG_HOME": filepath.Join(installHome, "runtime", Provider, "config"),
	}
	counts := map[string]int{}
	for _, entry := range command.Env {
		name, value, found := strings.Cut(entry, "=")
		for expectedName, expectedValue := range wanted {
			if found && strings.EqualFold(name, expectedName) {
				counts[expectedName]++
				if value != expectedValue {
					t.Errorf("%s = %q, want %q", expectedName, value, expectedValue)
				}
			}
		}
	}
	for name := range wanted {
		if counts[name] != 1 {
			t.Errorf("%s entries = %d, want 1", name, counts[name])
		}
	}
}
