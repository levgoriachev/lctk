//go:build windows

package lctkhome

import (
	"path/filepath"
	"testing"
)

func TestDefaultRuntimeDataUsesADedicatedLocalAppDataDirectory(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LocalAppData", localAppData)
	got, err := defaultRuntimeDataDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(localAppData, "lctk-runtime-data")
	if got != want {
		t.Fatalf("default runtime data=%q want=%q", got, want)
	}
}
