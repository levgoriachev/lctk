package nvidiainstall

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type machineStub struct {
	calls          []string
	packageQueries int
	installed      string
	input          []byte
}

func (s *machineStub) Run(_ context.Context, args ...string) (string, string, error) {
	call := strings.Join(args, " ")
	s.calls = append(s.calls, call)
	switch {
	case call == strings.Join([]string{"sh", "-ceu", wslProbeScript}, " "):
		return "", "", nil
	case call == strings.Join([]string{"sh", "-ceu", installedPackageScript}, " "):
		s.packageQueries++
		if s.installed != "" {
			return s.installed + "\n", "", nil
		}
		if s.packageQueries == 1 && len(s.input) == 0 {
			return missingPackageMarker + "\n", "", nil
		}
		return ToolkitNEVRA + "\n", "", nil
	case call == strings.Join([]string{"sh", "-ceu", "install -d -m 0755 /etc/cdi; nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"}, " "):
		return "", "", nil
	case call == "rpm -V "+ToolkitName:
		return "", "", nil
	case call == "nvidia-ctk cdi list":
		return CDIDevice + "\n", "", nil
	default:
		return "", "unexpected command", os.ErrInvalid
	}
}

func (s *machineStub) RunInput(_ context.Context, input io.Reader, args ...string) (string, string, error) {
	call := strings.Join(args, " ")
	s.calls = append(s.calls, call)
	if call != strings.Join([]string{"sh", "-ceu", installPackageScript}, " ") {
		return "", "unexpected input command", os.ErrInvalid
	}
	bytes, err := io.ReadAll(input)
	if err != nil {
		return "", "", err
	}
	s.input = bytes
	return "", "", nil
}

func TestEnsureInstallsVerifiedPackageAndProvesCDI(t *testing.T) {
	t.Setenv(lctkhome.EnvOverride, t.TempDir())
	body := []byte("verified pinned RPM")
	runner := &machineStub{}
	manager := NewManagerForTest(runner, nil)
	manager.verify = func(path string, _ int64, _ string) error {
		_, err := os.Stat(path)
		return err
	}
	manager.download = func(_ context.Context, _ *http.Client, _ releasebundle.Artifact, target string) error {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	}
	status, err := manager.Ensure(t.Context(), testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || string(runner.input) != string(body) {
		t.Fatalf("status=%+v streamed=%q", status, runner.input)
	}
}

func TestStatusAcceptsOnlyExactPackageAndCDIDevice(t *testing.T) {
	runner := &machineStub{installed: ToolkitNEVRA}
	status, err := NewManagerForTest(runner, nil).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.InstalledNEVRA != ToolkitNEVRA || status.CDIDevice != CDIDevice || !status.WSLReady {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestEnsureRejectsIncompatibleInstalledPackageBeforeStreaming(t *testing.T) {
	t.Setenv(lctkhome.EnvOverride, t.TempDir())
	runner := &machineStub{installed: ToolkitName + "-2.0.0-1.x86_64"}
	manager := NewManagerForTest(runner, nil)
	manager.verify = func(string, int64, string) error { return nil }
	_, err := manager.Ensure(t.Context(), testManifest())
	if !IsCode(err, FailurePackageInvalid) {
		t.Fatalf("error=%v want package failure", err)
	}
	if len(runner.input) != 0 {
		t.Fatal("incompatible package was overwritten")
	}
}

func TestValidateManifestRejectsAnyGPUIdentityDrift(t *testing.T) {
	manifest := testManifest()
	manifest.NVIDIAGPUInferenceImage.Digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := ValidateManifest(manifest); !IsCode(err, FailureCUDAImageInvalid) {
		t.Fatalf("error=%v want CUDA image failure", err)
	}
	manifest = testManifest()
	manifest.Artifacts[0].Bytes++
	if _, err := ValidateManifest(manifest); !IsCode(err, FailurePackageInvalid) {
		t.Fatalf("error=%v want package failure", err)
	}
}

func TestValidateManifestAcceptsSignedMirrorURL(t *testing.T) {
	manifest := testManifest()
	manifest.Artifacts[0].URL = "http://127.0.0.1:4466/" + ToolkitFileName
	artifact, err := ValidateManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.URL != manifest.Artifacts[0].URL {
		t.Fatalf("validated URL = %q, want signed mirror URL", artifact.URL)
	}
}

func testManifest() releasebundle.Manifest {
	return releasebundle.Manifest{
		Version: "0.1.12",
		Artifacts: []releasebundle.Artifact{{
			Name: ToolkitFileName, Kind: ToolkitArtifactKind, OS: "linux", Arch: "amd64",
			URL: ToolkitURL, Bytes: ToolkitBytes, SHA256: ToolkitSHA256,
		}},
		NVIDIAGPUInferenceImage: releasebundle.Image{
			Name: "llama.cpp-server-cuda", Reference: Image, Digest: ImageDigest,
			CompressedBytes: ImageCompressedBytes, UnpackedBytes: ImageUnpackedBytes,
			Platforms: []string{"linux/amd64", "linux/arm64"},
		},
	}
}
