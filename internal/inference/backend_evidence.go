package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// BackendEvidenceFileName is derived host state, deliberately separate from
	// the owner's distribution choice in inference.json.
	BackendEvidenceFileName = "inference-evidence.json"
	backendEvidenceSchema   = 1
	backendEvidenceLimit    = 4
)

// backendEvidenceDocument retains the small rollback window as well as the
// current container proof. A candidate may be proven before a later network or
// cleanup gate restores the preceding container, so one replace-in-place record
// would break transactional rollback.
type backendEvidenceDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Records       []backendEvidence `json:"records"`
}

// backendEvidence binds one measured full CUDA offload to the exact container
// process that produced it. A container restart changes StartedAt and therefore
// requires fresh startup-log proof before LCTK can report the backend ready.
type backendEvidence struct {
	SchemaVersion   int          `json:"schema_version"`
	ContainerID     string       `json:"container_id"`
	ImageID         string       `json:"image_id"`
	ConfigRevision  string       `json:"config_revision"`
	Distribution    Distribution `json:"distribution"`
	StartedAt       time.Time    `json:"started_at"`
	OffloadedLayers string       `json:"offloaded_layers"`
}

// parseBackendEvidenceIdentity validates all runtime-owned identity fields
// before either persisted or newly captured evidence may authorize readiness.
func parseBackendEvidenceIdentity(raw string) (backendEvidence, error) {
	fields := strings.Split(strings.TrimSpace(raw), "|")
	if len(fields) != 5 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return backendEvidence{}, errors.New("llama.cpp CUDA evidence identity is malformed")
	}
	started, err := time.Parse(podmanStartedAtLayout, fields[2])
	if err != nil {
		return backendEvidence{}, errors.New("llama.cpp CUDA startup time is malformed")
	}
	evidence := backendEvidence{
		SchemaVersion: backendEvidenceSchema, ContainerID: fields[0], ImageID: fields[1],
		StartedAt: started, ConfigRevision: fields[3], Distribution: Distribution(fields[4]),
	}
	if evidence.ConfigRevision != ConfigRevision || evidence.Distribution != DistributionNVIDIAGPU {
		return backendEvidence{}, errors.New("llama.cpp CUDA evidence identity does not match the active GPU contract")
	}
	return evidence, nil
}

// matches accepts only the same immutable image, runtime contract, container,
// and process start. Names are excluded because candidate promotion renames the
// already-proven container without changing its identity.
func (e backendEvidence) matches(identity backendEvidence) bool {
	return e.valid() && identity.validIdentity() && e.ContainerID == identity.ContainerID &&
		e.ImageID == identity.ImageID && e.ConfigRevision == identity.ConfigRevision &&
		e.Distribution == identity.Distribution && e.StartedAt.Equal(identity.StartedAt) &&
		validCompleteOffload(e.OffloadedLayers)
}

// validIdentity checks the fields obtained from Podman without requiring a
// measurement result that exists only after startup logs have been parsed.
func (e backendEvidence) validIdentity() bool {
	return e.SchemaVersion == backendEvidenceSchema && e.ContainerID != "" && e.ImageID != "" &&
		e.ConfigRevision != "" && e.Distribution == DistributionNVIDIAGPU && !e.StartedAt.IsZero()
}

// valid checks one complete persisted record before it may be reused or kept
// in the bounded rollback document.
func (e backendEvidence) valid() bool {
	return e.validIdentity() && validCompleteOffload(e.OffloadedLayers)
}

// validCompleteOffload applies the same strict N/N rule used for fresh logs to
// a compact persisted layer count.
func validCompleteOffload(value string) bool {
	match := offloadPattern.FindStringSubmatch("offloaded " + value + " layers to GPU")
	return len(match) == 3 && match[1] == match[2]
}

// loadBackendEvidenceDocument rejects partial, extended, or oversized state;
// absence remains normal before the first GPU container created by this core.
func loadBackendEvidenceDocument(path string) (backendEvidenceDocument, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return backendEvidenceDocument{SchemaVersion: backendEvidenceSchema}, false, nil
		}
		return backendEvidenceDocument{}, false, err
	}
	var document backendEvidenceDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return backendEvidenceDocument{}, false, err
	}
	if err := requireJSONEnd(decoder); err != nil {
		return backendEvidenceDocument{}, false, err
	}
	if document.SchemaVersion != backendEvidenceSchema || len(document.Records) == 0 || len(document.Records) > backendEvidenceLimit {
		return backendEvidenceDocument{}, false, errors.New("persisted CUDA evidence document is incomplete or unsupported")
	}
	for _, evidence := range document.Records {
		if !evidence.valid() {
			return backendEvidenceDocument{}, false, errors.New("persisted CUDA evidence is incomplete or unsupported")
		}
	}
	return document, true, nil
}

// loadBackendEvidence finds the newest proof for one exact running process.
func loadBackendEvidence(path string, identity backendEvidence) (backendEvidence, bool, error) {
	document, found, err := loadBackendEvidenceDocument(path)
	if err != nil || !found {
		return backendEvidence{}, false, err
	}
	for index := len(document.Records) - 1; index >= 0; index-- {
		if document.Records[index].matches(identity) {
			return document.Records[index], true, nil
		}
	}
	return backendEvidence{}, false, nil
}

// saveBackendEvidence atomically publishes only an already-measured result.
// Immediate readback prevents a truncated audit record from surviving a gate.
func saveBackendEvidence(path string, evidence backendEvidence) error {
	if !evidence.valid() {
		return errors.New("refuse to persist incomplete CUDA evidence")
	}
	document, found, err := loadBackendEvidenceDocument(path)
	if err != nil {
		return err
	}
	if !found {
		document = backendEvidenceDocument{SchemaVersion: backendEvidenceSchema}
	}
	filtered := make([]backendEvidence, 0, len(document.Records)+1)
	for _, existing := range document.Records {
		if !existing.matches(evidence) {
			filtered = append(filtered, existing)
		}
	}
	filtered = append(filtered, evidence)
	if len(filtered) > backendEvidenceLimit {
		filtered = filtered[len(filtered)-backendEvidenceLimit:]
	}
	document.Records = filtered
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, BackendEvidenceFileName+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	if err := replaceStateFile(temporaryPath, path); err != nil {
		return fmt.Errorf("replace CUDA evidence: %w", err)
	}
	stored, found, err := loadBackendEvidence(path, evidence)
	if err != nil || !found || !stored.matches(evidence) || stored.OffloadedLayers != evidence.OffloadedLayers {
		return errors.Join(errors.New("saved CUDA evidence does not match the verified result"), err)
	}
	return nil
}
