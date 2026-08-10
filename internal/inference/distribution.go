package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

const (
	// SelectionFileName is deliberately separate from settings.json. The
	// verified 0.1.11 rollback target must continue to understand host settings.
	SelectionFileName = "inference.json"
	// SelectionSchemaVersion versions only the inference distribution choice.
	SelectionSchemaVersion = 1
)

// Distribution is the owner-selected local inference implementation. It is an
// enum rather than a capability flag because CPU and NVIDIA GPU are complete,
// independently verifiable distributions with no silent fallback between them.
type Distribution string

const (
	DistributionCPU       Distribution = "cpu"
	DistributionNVIDIAGPU Distribution = "nvidia_gpu"
)

// Valid reports whether the value names one supported inference distribution.
func (d Distribution) Valid() bool {
	return d == DistributionCPU || d == DistributionNVIDIAGPU
}

// Selection is the complete owner-controlled persistence document. Runtime
// readiness and GPU diagnostics are intentionally absent because they must be
// measured from the active backend rather than trusted from configuration.
type Selection struct {
	SchemaVersion int          `json:"schema_version"`
	Distribution  Distribution `json:"distribution"`
}

// DefaultSelection preserves the CPU behavior of installations created before
// inference.json existed and keeps CPU explicit for new installations.
var DefaultSelection = Selection{
	SchemaVersion: SelectionSchemaVersion,
	Distribution:  DistributionCPU,
}

// SelectionPath returns the owner-controlled document path without creating it.
func SelectionPath() (string, error) {
	home, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, SelectionFileName), nil
}

// LoadSelection reads the current choice. Absence means CPU for compatibility;
// malformed, unknown, or newer documents fail instead of changing backends.
func LoadSelection() (Selection, error) {
	path, err := SelectionPath()
	if err != nil {
		return DefaultSelection, err
	}
	return LoadSelectionFrom(path)
}

// LoadSelectionFrom is the path-bounded form used by tests and setup staging.
func LoadSelectionFrom(path string) (Selection, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultSelection, nil
		}
		return DefaultSelection, fmt.Errorf("read inference selection %q: %w", path, err)
	}

	var selection Selection
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selection); err != nil {
		return DefaultSelection, fmt.Errorf("inference selection %q is not valid: %w", path, err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return DefaultSelection, fmt.Errorf("inference selection %q is not valid: %w", path, err)
	}
	if err := selection.Validate(); err != nil {
		return DefaultSelection, fmt.Errorf("inference selection %q: %w", path, err)
	}
	return selection, nil
}

// Validate rejects ambiguous selection state before a runtime transaction uses
// it to choose an image, device injection, package, or readiness contract.
func (s Selection) Validate() error {
	if s.SchemaVersion != SelectionSchemaVersion {
		return fmt.Errorf("schema version %d is unsupported; expected %d", s.SchemaVersion, SelectionSchemaVersion)
	}
	if !s.Distribution.Valid() {
		return fmt.Errorf("distribution %q is unsupported; expected cpu or nvidia_gpu", s.Distribution)
	}
	return nil
}

// SaveSelection commits an already-proven distribution with an owner-only,
// flushed, atomic replacement. Callers must not invoke it before activation and
// the real embedding self-test have completed successfully.
func SaveSelection(selection Selection) error {
	path, err := SelectionPath()
	if err != nil {
		return err
	}
	return SaveSelectionTo(path, selection)
}

// SaveSelectionTo is the path-bounded form used by tests and setup staging.
func SaveSelectionTo(path string, selection Selection) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inference selection: %w", err)
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create inference selection directory %q: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, SelectionFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary inference selection in %q: %w", dir, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary inference selection: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("flush temporary inference selection: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary inference selection: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("restrict temporary inference selection: %w", err)
	}
	if err := replaceStateFile(tempPath, path); err != nil {
		return fmt.Errorf("replace inference selection %q: %w", path, err)
	}
	stored, err := LoadSelectionFrom(path)
	if err != nil {
		return fmt.Errorf("verify saved inference selection: %w", err)
	}
	if stored != selection {
		return errors.New("saved inference selection does not match the accepted value")
	}
	return nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("document contains more than one JSON value")
	}
	return err
}
