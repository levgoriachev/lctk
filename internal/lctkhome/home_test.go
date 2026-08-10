package lctkhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirPrefersTheOverrideAndMakesItAbsolute(t *testing.T) {
	want := t.TempDir()
	t.Setenv(EnvOverride, want)

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}

	t.Setenv(EnvOverride, "relative-home")
	got, err = Dir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Dir() = %q, want an absolute path", got)
	}
}

func TestDirFallsBackToAPlatformLocation(t *testing.T) {
	t.Setenv(EnvOverride, "")

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Dir() = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "lctk" {
		t.Errorf("Dir() = %q, want a path ending in lctk", got)
	}
	// Host state must never resolve into the working directory, where it could be
	// committed to a repository.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, cwd) {
		t.Errorf("Dir() = %q resolves inside the working directory %q", got, cwd)
	}
}

func TestEnsureDirCreatesTheDirectory(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "lctk")
	t.Setenv(EnvOverride, target)

	got, err := EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("EnsureDir() = %q, want %q", got, target)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("EnsureDir did not create a directory")
	}

	// Calling it again must succeed.
	if _, err := EnsureDir(); err != nil {
		t.Errorf("second call failed: %v", err)
	}
}

func TestRuntimeDataDirPrefersTheAbsoluteOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "container-data")
	t.Setenv(RuntimeDataEnvOverride, want)

	got, err := RuntimeDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("RuntimeDataDir() = %q, want %q", got, want)
	}
}

func TestNormalizeLocationsRejectsDriveRoots(t *testing.T) {
	volume := filepath.VolumeName(t.TempDir())
	if volume == "" {
		t.Skip("the host filesystem has no volume root")
	}
	_, err := NormalizeLocations(volume+string(os.PathSeparator), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "drive root") {
		t.Fatalf("NormalizeLocations drive root error = %v", err)
	}
}
