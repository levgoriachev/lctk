//go:build windows

package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendedPayloadExtractsOnlyTheCompleteCandidateSet(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "setup.exe")
	file, err := os.Create(executable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("executable-prefix")); err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name := range payloadNames {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := extractPayload(executable, target); err != nil {
		t.Fatal(err)
	}
	for name := range payloadNames {
		content, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(content) != name {
			t.Fatalf("entry %q = %q, %v", name, content, err)
		}
	}
}

func TestPackageHandlerDoesNotExposeNeighboringFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "release-manifest.json"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(packageHandler(directory))
	defer server.Close()
	for path, wanted := range map[string]int{
		"/release-manifest.json": http.StatusOK,
		"/secret.txt":            http.StatusNotFound,
		"/../secret.txt":         http.StatusNotFound,
	} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != wanted {
			t.Errorf("GET %s = %d, want %d", path, response.StatusCode, wanted)
		}
	}
}

func TestSetupArgumentsSelectExactLocalMode(t *testing.T) {
	original := DefaultSetupMode
	t.Cleanup(func() { DefaultSetupMode = original })
	DefaultSetupMode = ""
	arguments, err := setupArguments(`C:\candidate\release-manifest.json`)
	if err != nil || len(arguments) != 2 || arguments[0] != "--manifest" {
		t.Fatalf("installer arguments=%v err=%v", arguments, err)
	}
	DefaultSetupMode = "uninstall"
	arguments, err = setupArguments("")
	if err != nil || len(arguments) != 1 || arguments[0] != "--uninstall" {
		t.Fatalf("uninstaller arguments=%v err=%v", arguments, err)
	}
	DefaultSetupMode = "unexpected"
	if _, err := setupArguments(""); err == nil {
		t.Fatal("unsupported local setup mode was accepted")
	}
}
