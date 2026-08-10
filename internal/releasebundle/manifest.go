// Package releasebundle verifies the signed document that binds every install
// and update component to one product version.
package releasebundle

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strings"
	"time"
)

const SchemaVersion = 2

// TrustedKeyID and TrustedPublicKey are injected into official binaries at
// build time. Development binaries intentionally have no release trust root and
// therefore cannot apply an official update accidentally.
var (
	TrustedKeyID     = ""
	TrustedPublicKey = ""
)

// Envelope keeps the exact signed payload bytes. Re-marshalling a JSON object
// before verification would make whitespace and map ordering part of security.
type Envelope struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Manifest is the compatibility and immutable-component contract for one
// release. URLs locate bytes; sizes and SHA-256 digests decide their identity.
type Manifest struct {
	SchemaVersion           int        `json:"schema_version"`
	Version                 string     `json:"version"`
	Commit                  string     `json:"commit"`
	PublishedAt             string     `json:"published_at"`
	MinimumHostVersion      string     `json:"minimum_host_version"`
	ProjectSchemaFrom       int        `json:"project_schema_from"`
	ProjectSchemaTo         int        `json:"project_schema_to"`
	Artifacts               []Artifact `json:"artifacts"`
	CodeImage               Image      `json:"code_image"`
	InferenceImage          Image      `json:"inference_image"`
	NVIDIAGPUInferenceImage Image      `json:"nvidia_gpu_inference_image,omitempty"`
	EmbeddingModel          Model      `json:"embedding_model"`
	MigrationNotesURL       string     `json:"migration_notes_url"`
	RollbackInstructions    string     `json:"rollback_instructions"`
}

// Artifact is one host launcher, host core, archive, checksum, or SBOM.
type Artifact struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
	URL    string `json:"url"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Image identifies an OCI manifest by digest; Reference must include @sha256.
type Image struct {
	Name            string `json:"name"`
	Reference       string `json:"reference"`
	Digest          string `json:"digest"`
	CompressedBytes int64  `json:"compressed_bytes"`
	// UnpackedBytes is required for the large CUDA distribution so setup can
	// reserve installation storage before asking Podman to unpack its layers.
	UnpackedBytes int64    `json:"unpacked_bytes,omitempty"`
	Platforms     []string `json:"platforms"`
}

// Model repeats the immutable model contract so bootstrap/update can reject a
// manifest that disagrees with the binary compiled to consume it.
type Model struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	License string `json:"license"`
}

// Verifier is injectable for deterministic tests; production uses the embedded
// build-time trust root only.
type Verifier struct {
	KeyID     string
	PublicKey ed25519.PublicKey
}

// ProductionVerifier resolves the immutable trust root embedded by the release
// workflow. An unsigned development binary fails closed.
func ProductionVerifier() (Verifier, error) {
	if TrustedKeyID == "" || TrustedPublicKey == "" {
		return Verifier{}, errors.New("this development binary has no embedded release trust root")
	}
	decoded, err := base64.StdEncoding.DecodeString(TrustedPublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return Verifier{}, errors.New("the embedded release public key is invalid")
	}
	return Verifier{KeyID: TrustedKeyID, PublicKey: ed25519.PublicKey(decoded)}, nil
}

// Verify authenticates exact payload bytes before decoding or trusting any URL.
func (v Verifier) Verify(document []byte) (Manifest, error) {
	if len(v.PublicKey) != ed25519.PublicKeySize || v.KeyID == "" {
		return Manifest{}, errors.New("release verifier is not configured")
	}
	var envelope Envelope
	if err := json.Unmarshal(document, &envelope); err != nil {
		return Manifest{}, fmt.Errorf("decode release envelope: %w", err)
	}
	if envelope.KeyID != v.KeyID || envelope.Algorithm != "ed25519" {
		return Manifest{}, errors.New("release envelope uses an untrusted key or algorithm")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return Manifest{}, fmt.Errorf("decode release payload: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(v.PublicKey, payload, signature) {
		return Manifest{}, errors.New("release manifest signature is invalid")
	}
	var manifest Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode signed release manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate rejects incomplete release claims even when their signature is valid.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion || !validVersion(m.Version) || len(m.Commit) < 7 {
		return errors.New("release manifest identity or schema is invalid")
	}
	if _, err := time.Parse(time.RFC3339, m.PublishedAt); err != nil {
		return errors.New("release manifest publication time is invalid")
	}
	if !validVersion(m.MinimumHostVersion) || m.ProjectSchemaFrom < 1 || m.ProjectSchemaTo < m.ProjectSchemaFrom {
		return errors.New("release manifest compatibility range is invalid")
	}
	if m.RollbackInstructions == "" || len(m.Artifacts) == 0 || !validHTTPS(m.MigrationNotesURL) {
		return errors.New("release manifest omits rollback instructions or artifacts")
	}
	seen := map[string]bool{}
	for _, artifact := range m.Artifacts {
		key := artifact.Kind + "\x00" + artifact.OS + "\x00" + artifact.Arch
		if seen[key] || artifact.Name == "" || artifact.Kind == "" || !validArtifactURL(artifact.URL) || artifact.Bytes <= 0 || !validSHA256(artifact.SHA256) {
			return fmt.Errorf("release artifact %q is invalid or duplicated", artifact.Name)
		}
		seen[key] = true
	}
	if _, hasWindowsCore := seen["host-core\x00windows\x00amd64"]; hasWindowsCore {
		for _, identity := range []string{
			"host-launcher\x00windows\x00amd64",
			"installer\x00windows\x00amd64",
			"podman-client\x00windows\x00amd64",
			"podman-machine\x00linux\x00amd64",
		} {
			if !seen[identity] {
				return errors.New("Windows release omits a one-click installer component")
			}
		}
		// Selectable GPU inference begins with 0.1.12 while schema 2 remains
		// readable by 0.1.11. The version gate accepts historical signed schema-2
		// manifests but makes omission from every new Windows release terminal.
		if VersionAtLeast(m.Version, "0.1.12") {
			if !seen["nvidia-container-toolkit-base\x00linux\x00amd64"] {
				return errors.New("Windows release omits the NVIDIA CDI package")
			}
			if err := validateImage(m.NVIDIAGPUInferenceImage); err != nil {
				return fmt.Errorf("NVIDIA GPU inference image: %w", err)
			}
			if m.NVIDIAGPUInferenceImage.UnpackedBytes <= 0 ||
				!hasPlatform(m.NVIDIAGPUInferenceImage.Platforms, "linux/amd64") {
				return errors.New("NVIDIA GPU inference image omits linux/amd64 storage evidence")
			}
		}
	}
	if err := validateImage(m.CodeImage); err != nil {
		return fmt.Errorf("code image: %w", err)
	}
	if err := validateImage(m.InferenceImage); err != nil {
		return fmt.Errorf("inference image: %w", err)
	}
	if m.EmbeddingModel.Name == "" || !validHTTPS(m.EmbeddingModel.URL) || m.EmbeddingModel.Bytes <= 0 || !validSHA256(m.EmbeddingModel.SHA256) || m.EmbeddingModel.License == "" {
		return errors.New("embedding model identity is invalid")
	}
	return nil
}

// HostCore returns the one executable matching the binary doing the update.
func (m Manifest) HostCore() (Artifact, error) {
	return m.ArtifactFor("host-core", runtime.GOOS, runtime.GOARCH)
}

// ArtifactFor selects one immutable component for a target identity. Manifest
// validation rejects duplicate identities, so selection is unambiguous.
func (m Manifest) ArtifactFor(kind, targetOS, arch string) (Artifact, error) {
	for _, artifact := range m.Artifacts {
		if artifact.Kind == kind && artifact.OS == targetOS && artifact.Arch == arch {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("release %s has no %s artifact for %s/%s", m.Version, kind, targetOS, arch)
}

func validateImage(image Image) error {
	digest := strings.TrimPrefix(image.Digest, "sha256:")
	if image.Name == "" || image.CompressedBytes <= 0 || !validSHA256(digest) ||
		!strings.HasSuffix(image.Reference, "@sha256:"+digest) || len(image.Platforms) == 0 {
		return errors.New("immutable OCI identity is invalid")
	}
	return nil
}

func hasPlatform(platforms []string, wanted string) bool {
	for _, platform := range platforms {
		if platform == wanted {
			return true
		}
	}
	return false
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

func validVersion(value string) bool {
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

// VersionAtLeast compares the strict numeric release versions accepted by the
// manifest schema. It deliberately has no prerelease compatibility rules.
func VersionAtLeast(current, minimum string) bool {
	currentParts, currentOK := versionParts(current)
	minimumParts, minimumOK := versionParts(minimum)
	if !currentOK || !minimumOK {
		return false
	}
	for index := range currentParts {
		if currentParts[index] != minimumParts[index] {
			return currentParts[index] > minimumParts[index]
		}
	}
	return true
}

func versionParts(value string) ([3]int, bool) {
	var result [3]int
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != len(result) {
		return result, false
	}
	for index, part := range parts {
		if part == "" {
			return result, false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return result, false
			}
			result[index] = result[index]*10 + int(char-'0')
		}
	}
	return result, true
}

func validHTTPS(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

// validArtifactURL permits plain HTTP only on numeric loopback for the local
// one-file release-candidate harness. The signed manifest and the mandatory
// size/SHA-256 check still authenticate every byte; production publication
// continues to generate HTTPS URLs exclusively.
func validArtifactURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
