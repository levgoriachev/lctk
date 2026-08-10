package releasebundle

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestVerifierRejectsTamperingAndAcceptsCompleteManifest(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validManifest()
	payload, _ := json.Marshal(manifest)
	envelope := Envelope{KeyID: "test", Algorithm: "ed25519", Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))}
	document, _ := json.Marshal(envelope)
	verified, err := (Verifier{KeyID: "test", PublicKey: public}).Verify(document)
	if err != nil || verified.Version != manifest.Version {
		t.Fatalf("verified=%+v err=%v", verified, err)
	}
	envelope.Payload = base64.StdEncoding.EncodeToString(append(payload, ' '))
	tampered, _ := json.Marshal(envelope)
	if _, err := (Verifier{KeyID: "test", PublicKey: public}).Verify(tampered); err == nil {
		t.Fatal("tampered payload verified")
	}
}

func TestArtifactURLAllowsOnlyHTTPSOrNumericLoopbackHTTP(t *testing.T) {
	for value, wanted := range map[string]bool{
		"https://example.com/file":      true,
		"http://127.0.0.1:4466/file":    true,
		"http://[::1]:4466/file":        true,
		"http://localhost:4466/file":    false,
		"http://example.com/file":       false,
		"file:///C:/package/file":       false,
		"https://user@example.com/file": false,
	} {
		if got := validArtifactURL(value); got != wanted {
			t.Errorf("validArtifactURL(%q) = %t, want %t", value, got, wanted)
		}
	}
}

func TestManifestRejectsMutableImageAndDuplicateHost(t *testing.T) {
	manifest := validManifest()
	manifest.CodeImage.Reference = "ghcr.io/example/code:latest"
	if err := manifest.Validate(); err == nil {
		t.Fatal("mutable image accepted")
	}
	manifest = validManifest()
	manifest.Artifacts = append(manifest.Artifacts, manifest.Artifacts[0])
	if err := manifest.Validate(); err == nil {
		t.Fatal("duplicate host artifact accepted")
	}
}

func TestWindowsManifestRequiresTheCompleteOneClickSet(t *testing.T) {
	manifest := validManifest()
	manifest.Artifacts = manifest.Artifacts[:len(manifest.Artifacts)-1]
	if err := manifest.Validate(); err == nil {
		t.Fatal("Windows release without its machine image was accepted")
	}
}

func TestCurrentWindowsManifestRequiresNVIDIADistributionArtifacts(t *testing.T) {
	manifest := validManifest()
	manifest.NVIDIAGPUInferenceImage = Image{}
	if err := manifest.Validate(); err == nil {
		t.Fatal("current Windows release without its NVIDIA GPU image was accepted")
	}
	manifest = validManifest()
	manifest.Artifacts = manifest.Artifacts[:len(manifest.Artifacts)-1]
	if err := manifest.Validate(); err == nil {
		t.Fatal("current Windows release without its NVIDIA CDI package was accepted")
	}
}

func TestHistoricalWindowsManifestRemainsReadableBySchemaTwo(t *testing.T) {
	manifest := validManifest()
	manifest.Version = "0.1.11"
	manifest.NVIDIAGPUInferenceImage = Image{}
	manifest.Artifacts = manifest.Artifacts[:len(manifest.Artifacts)-1]
	if err := manifest.Validate(); err != nil {
		t.Fatalf("historical manifest rejected: %v", err)
	}
}

func TestVersionAtLeastUsesNumericComponents(t *testing.T) {
	for _, test := range []struct {
		current, minimum string
		want             bool
	}{
		{"1.10.0", "1.9.9", true},
		{"2.0.0", "1.99.99", true},
		{"1.0.0", "1.0.0", true},
		{"0.9.9", "1.0.0", false},
		{"1.0.0-dev", "1.0.0", false},
	} {
		if got := VersionAtLeast(test.current, test.minimum); got != test.want {
			t.Errorf("VersionAtLeast(%q, %q) = %t, want %t", test.current, test.minimum, got, test.want)
		}
	}
}

func validManifest() Manifest {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	return Manifest{SchemaVersion: SchemaVersion, Version: "1.0.0", Commit: "0123456789abcdef", PublishedAt: "2026-08-04T00:00:00Z", MinimumHostVersion: "0.1.0", ProjectSchemaFrom: 1, ProjectSchemaTo: 2,
		Artifacts: []Artifact{
			{Name: "lctk-core", Kind: "host-core", OS: "windows", Arch: "amd64", URL: "https://example/core", Bytes: 42, SHA256: hash},
			{Name: "lctk.exe", Kind: "host-launcher", OS: "windows", Arch: "amd64", URL: "https://example/launcher", Bytes: 42, SHA256: hash},
			{Name: "setup.exe", Kind: "installer", OS: "windows", Arch: "amd64", URL: "https://example/setup", Bytes: 42, SHA256: hash},
			{Name: "podman.zip", Kind: "podman-client", OS: "windows", Arch: "amd64", URL: "https://example/client", Bytes: 42, SHA256: hash},
			{Name: "machine.tar.zst", Kind: "podman-machine", OS: "linux", Arch: "amd64", URL: "https://example/machine", Bytes: 42, SHA256: hash},
			{Name: "nvidia-container-toolkit-base.rpm", Kind: "nvidia-container-toolkit-base", OS: "linux", Arch: "amd64", URL: "https://example/nvidia.rpm", Bytes: 42, SHA256: hash},
		},
		CodeImage:               Image{Name: "code", Reference: "ghcr.io/example/code@sha256:" + hash, Digest: "sha256:" + hash, CompressedBytes: 42, Platforms: []string{"linux/amd64", "linux/arm64"}},
		InferenceImage:          Image{Name: "inference", Reference: "ghcr.io/example/inference@sha256:" + hash, Digest: "sha256:" + hash, CompressedBytes: 42, Platforms: []string{"linux/amd64", "linux/arm64"}},
		NVIDIAGPUInferenceImage: Image{Name: "inference-cuda", Reference: "ghcr.io/example/inference-cuda@sha256:" + hash, Digest: "sha256:" + hash, CompressedBytes: 84, UnpackedBytes: 168, Platforms: []string{"linux/amd64", "linux/arm64"}},
		EmbeddingModel:          Model{Name: "model", URL: "https://example/model", Bytes: 42, SHA256: hash, License: "Apache-2.0"}, MigrationNotesURL: "https://example/migration", RollbackInstructions: "lctk update rollback"}
}
