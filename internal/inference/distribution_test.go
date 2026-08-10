package inference

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSelectionMissingDefaultsToCPU(t *testing.T) {
	selection, err := LoadSelectionFrom(filepath.Join(t.TempDir(), SelectionFileName))
	if err != nil {
		t.Fatal(err)
	}
	if selection != DefaultSelection {
		t.Fatalf("selection=%+v want %+v", selection, DefaultSelection)
	}
}

func TestSelectionRoundTripReplacesTheCompleteDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), SelectionFileName)
	if err := SaveSelectionTo(path, DefaultSelection); err != nil {
		t.Fatal(err)
	}
	wanted := Selection{SchemaVersion: SelectionSchemaVersion, Distribution: DistributionNVIDIAGPU}
	if err := SaveSelectionTo(path, wanted); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSelectionFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != wanted {
		t.Fatalf("selection=%+v want %+v", got, wanted)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows inherits the owner ACL from the private LCTK home and does not
	// expose that ACL through os.FileMode; POSIX targets expose the exact mode.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("selection permissions=%o want owner-only", info.Mode().Perm())
	}
}

func TestSelectionRejectsUnknownNewerAndExtendedDocuments(t *testing.T) {
	for name, document := range map[string]string{
		"unknown distribution": `{"schema_version":1,"distribution":"automatic"}`,
		"newer schema":         `{"schema_version":2,"distribution":"cpu"}`,
		"unknown field":        `{"schema_version":1,"distribution":"cpu","ready":true}`,
		"trailing value":       `{"schema_version":1,"distribution":"cpu"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), SelectionFileName)
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSelectionFrom(path); err == nil {
				t.Fatal("invalid inference selection was accepted")
			}
		})
	}
}

func TestSaveSelectionRejectsInvalidValueBeforeWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), SelectionFileName)
	err := SaveSelectionTo(path, Selection{SchemaVersion: SelectionSchemaVersion, Distribution: "automatic"})
	if err == nil {
		t.Fatal("invalid selection was written")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("invalid save created %q: %v", path, statErr)
	}
}
