//go:build windows

package desktopinstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStartMenuProgramsResolvesAWritablePhysicalDirectory(t *testing.T) {
	programs, err := startMenuPrograms()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(programs) {
		t.Fatalf("Start-menu Programs path is not absolute: %q", programs)
	}
	if info, err := os.Stat(programs); err != nil || !info.IsDir() {
		t.Fatalf("Start-menu Programs path is not a directory: %q, %v", programs, err)
	}
}

func TestStartMenuProgramsFallsBackToExplorerRegistry(t *testing.T) {
	originalKnown, originalRegistry := resolveKnownPrograms, resolveRegistryPrograms
	t.Cleanup(func() { resolveKnownPrograms, resolveRegistryPrograms = originalKnown, originalRegistry })
	expected := filepath.Join(t.TempDir(), "Programs")
	resolveKnownPrograms = func() (string, error) { return "", errors.New("known folder unavailable") }
	resolveRegistryPrograms = func() (string, error) { return expected, nil }
	programs, err := startMenuPrograms()
	if err != nil {
		t.Fatal(err)
	}
	if programs != expected {
		t.Fatalf("programs=%q want=%q", programs, expected)
	}
}

func TestStartMenuProgramsRejectsUnsafeFallback(t *testing.T) {
	originalKnown, originalRegistry := resolveKnownPrograms, resolveRegistryPrograms
	t.Cleanup(func() { resolveKnownPrograms, resolveRegistryPrograms = originalKnown, originalRegistry })
	resolveKnownPrograms = func() (string, error) { return "", errors.New("known folder unavailable") }
	resolveRegistryPrograms = func() (string, error) { return `relative\Programs`, nil }
	if _, err := startMenuPrograms(); err == nil {
		t.Fatal("relative Start-menu fallback was accepted")
	}
}

func TestCreateShortcutHandlesSpacesWithoutACommandShell(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "directory with spaces")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "lctk.exe")
	if err := os.WriteFile(target, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	shortcut := filepath.Join(directory, "LCTK link.lnk")
	if err := createShortcut(target, "--admin", "Open LCTK", shortcut); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(shortcut); err != nil || info.Size() == 0 {
		t.Fatalf("shortcut was not created: info=%v err=%v", info, err)
	}
}
