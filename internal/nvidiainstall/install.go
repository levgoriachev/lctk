package nvidiainstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/verifieddownload"
)

const missingPackageMarker = "lctk-package-missing"

// MachineRunner executes fixed root commands inside only the LCTK-owned Podman
// machine. Input is separate so verified package bytes can cross the remote
// boundary without an untrusted machine-side path or package repository.
type MachineRunner interface {
	Run(context.Context, ...string) (stdout string, stderr string, err error)
	RunInput(context.Context, io.Reader, ...string) (stdout string, stderr string, err error)
}

// Plan is the read-only CDI state presented before setup writes anything.
type Plan struct {
	ArtifactPath  string `json:"artifact_path"`
	DownloadBytes int64  `json:"download_bytes"`
	Installed     bool   `json:"installed"`
	WSLReady      bool   `json:"wsl_ready"`
	CDIReady      bool   `json:"cdi_ready"`
	Detail        string `json:"detail,omitempty"`
}

// Status is measured package and CDI evidence for Admin and inference gates.
type Status struct {
	InstalledNEVRA string `json:"installed_nevra"`
	CDIDevice      string `json:"cdi_device"`
	WSLReady       bool   `json:"wsl_ready"`
	Ready          bool   `json:"ready"`
}

// Manager owns only the pinned CDI package lifecycle. It does not select an
// inference distribution and cannot mutate a host or non-LCTK Podman runtime.
type Manager struct {
	runner   MachineRunner
	client   *http.Client
	verify   func(string, int64, string) error
	download func(context.Context, *http.Client, releasebundle.Artifact, string) error
}

// NewManager binds production to the private machine transport and the normal
// HTTPS client; tests inject both dependencies through NewManagerForTest.
func NewManager() *Manager {
	return &Manager{
		runner: machineTransport{}, client: http.DefaultClient,
		verify: verifieddownload.Verify, download: verifieddownload.Download,
	}
}

// NewManagerForTest supplies path-bounded transport and HTTP behavior.
func NewManagerForTest(runner MachineRunner, client *http.Client) *Manager {
	if client == nil {
		client = http.DefaultClient
	}
	return &Manager{
		runner: runner, client: client,
		verify: verifieddownload.Verify, download: verifieddownload.Download,
	}
}

// ArtifactPath returns the retained, installation-owned offline RPM path.
func ArtifactPath() (string, error) {
	home, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "runtime", "nvidia", ToolkitVersion, "downloads", ToolkitFileName), nil
}

// ValidateManifest binds a signed artifact to the identities compiled into the
// consumer. A valid signature alone cannot substitute a different package.
func ValidateManifest(manifest releasebundle.Manifest) (releasebundle.Artifact, error) {
	artifact, err := manifest.ArtifactFor(ToolkitArtifactKind, "linux", "amd64")
	if err != nil {
		return releasebundle.Artifact{}, fail(FailurePackageInvalid, "%v", err)
	}
	// The signed release may mirror the exact RPM on GitHub or expose it from
	// an authenticated local RC. Transport location is deliberately not part of
	// the package identity; signed name, size, and SHA-256 authenticate the bytes.
	if artifact.Name != ToolkitFileName || artifact.Bytes != ToolkitBytes ||
		artifact.SHA256 != ToolkitSHA256 {
		return releasebundle.Artifact{}, fail(FailurePackageInvalid,
			"signed NVIDIA package identity does not match %s", ToolkitNEVRA)
	}
	image := manifest.NVIDIAGPUInferenceImage
	if image.Reference != Image || image.Digest != ImageDigest || image.CompressedBytes != ImageCompressedBytes ||
		image.UnpackedBytes != ImageUnpackedBytes {
		return releasebundle.Artifact{}, fail(FailureCUDAImageInvalid,
			"signed CUDA image identity does not match the pinned llama.cpp distribution")
	}
	return artifact, nil
}

// Inspect performs no download, package installation, or CDI generation. It
// reports exact cached bytes and, when a machine is already available, its
// measured WSL/package/CDI state.
func (m *Manager) Inspect(ctx context.Context, manifest releasebundle.Manifest) (Plan, error) {
	if m == nil || m.verify == nil {
		return Plan{}, errors.New("NVIDIA installer is not configured")
	}
	artifact, err := ValidateManifest(manifest)
	if err != nil {
		return Plan{}, err
	}
	path, err := ArtifactPath()
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{ArtifactPath: path}
	if m.verify(path, artifact.Bytes, artifact.SHA256) != nil {
		plan.DownloadBytes = artifact.Bytes
	}
	if m.runner == nil {
		plan.Detail = "The managed Podman machine is not available for NVIDIA inspection."
		return plan, nil
	}
	status, statusErr := m.Status(ctx)
	if statusErr != nil {
		plan.Detail = statusErr.Error()
		return plan, nil
	}
	plan.Installed = status.InstalledNEVRA == ToolkitNEVRA
	plan.WSLReady = status.WSLReady
	plan.CDIReady = status.CDIDevice == CDIDevice
	return plan, nil
}

// Ensure downloads or reuses the signed RPM, proves WSL GPU projection,
// installs exactly one NEVRA, generates CDI, and verifies the advertised device.
func (m *Manager) Ensure(ctx context.Context, manifest releasebundle.Manifest) (Status, error) {
	if m == nil || m.runner == nil || m.client == nil || m.verify == nil || m.download == nil {
		return Status{}, errors.New("NVIDIA machine runner is not configured")
	}
	artifact, err := ValidateManifest(manifest)
	if err != nil {
		return Status{}, err
	}
	path, err := ArtifactPath()
	if err != nil {
		return Status{}, err
	}
	if m.verify(path, artifact.Bytes, artifact.SHA256) != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Status{}, fmt.Errorf("create NVIDIA package cache: %w", err)
		}
		if err := m.download(ctx, m.client, artifact, path); err != nil {
			return Status{}, fail(FailurePackageInvalid, "download pinned NVIDIA CDI package: %v", err)
		}
	}
	if err := m.probeWSL(ctx); err != nil {
		return Status{}, err
	}
	installed, err := m.installedNEVRA(ctx)
	if err != nil {
		return Status{}, err
	}
	if installed != "" && installed != ToolkitNEVRA {
		return Status{}, fail(FailurePackageInvalid,
			"managed machine contains %s, expected %s; remove the incompatible package through LCTK repair",
			installed, ToolkitNEVRA)
	}
	if installed == "" {
		file, err := os.Open(path)
		if err != nil {
			return Status{}, fail(FailurePackageInvalid, "open verified NVIDIA CDI package: %v", err)
		}
		_, stderr, installErr := m.runner.RunInput(ctx, file, "sh", "-ceu", installPackageScript)
		closeErr := file.Close()
		if installErr != nil {
			return Status{}, fail(FailurePackageInvalid, "install pinned NVIDIA CDI package: %s", firstLine(stderr, installErr))
		}
		if closeErr != nil {
			return Status{}, fail(FailurePackageInvalid, "close verified NVIDIA CDI package: %v", closeErr)
		}
	}
	if err := m.GenerateCDI(ctx); err != nil {
		return Status{}, err
	}
	return m.Status(ctx)
}

// Status verifies actual WSL projection, exact RPM ownership, and generated CDI
// identity. It never repairs or generates state and is safe for Admin polling.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	if m == nil || m.runner == nil {
		return Status{}, errors.New("NVIDIA machine runner is not configured")
	}
	if err := m.probeWSL(ctx); err != nil {
		return Status{}, err
	}
	installed, err := m.installedNEVRA(ctx)
	if err != nil {
		return Status{}, err
	}
	if installed != ToolkitNEVRA {
		if installed == "" {
			return Status{WSLReady: true}, fail(FailurePackageInvalid, "NVIDIA CDI package is not installed")
		}
		return Status{WSLReady: true, InstalledNEVRA: installed}, fail(FailurePackageInvalid,
			"installed NVIDIA CDI package is %s, expected %s", installed, ToolkitNEVRA)
	}
	stdout, stderr, err := m.runner.Run(ctx, "rpm", "-V", ToolkitName)
	if err != nil || strings.TrimSpace(stdout) != "" {
		return Status{WSLReady: true, InstalledNEVRA: installed}, fail(FailurePackageInvalid,
			"installed NVIDIA CDI package verification failed: %s", firstLine(stdout+"\n"+stderr, err))
	}
	stdout, stderr, err = m.runner.Run(ctx, "nvidia-ctk", "cdi", "list")
	if err != nil {
		return Status{WSLReady: true, InstalledNEVRA: installed}, fail(FailureCDIUnavailable,
			"list NVIDIA CDI devices: %s", firstLine(stderr, err))
	}
	if !containsLine(stdout, CDIDevice) {
		return Status{WSLReady: true, InstalledNEVRA: installed}, fail(FailureCDIUnavailable,
			"NVIDIA CDI does not expose %s; regenerate CDI through LCTK repair", CDIDevice)
	}
	return Status{WSLReady: true, InstalledNEVRA: installed, CDIDevice: CDIDevice, Ready: true}, nil
}

// GenerateCDI is the one explicit repair mutation performed after package
// installation and before Status. It uses only the pinned nvidia-ctk binary.
func (m *Manager) GenerateCDI(ctx context.Context) error {
	_, stderr, err := m.runner.Run(ctx, "sh", "-ceu",
		"install -d -m 0755 /etc/cdi; nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml")
	if err != nil {
		return fail(FailureCDIUnavailable, "generate NVIDIA CDI specification: %s", firstLine(stderr, err))
	}
	return nil
}

func (m *Manager) probeWSL(ctx context.Context) error {
	_, stderr, err := m.runner.Run(ctx, "sh", "-ceu", wslProbeScript)
	if err != nil {
		return fail(FailureWSLGPUUnavailable,
			"managed WSL does not expose /dev/dxg and libcuda.so.1: %s", firstLine(stderr, err))
	}
	return nil
}

func (m *Manager) installedNEVRA(ctx context.Context) (string, error) {
	stdout, stderr, err := m.runner.Run(ctx, "sh", "-ceu", installedPackageScript)
	if err != nil {
		return "", fail(FailurePackageInvalid, "inspect NVIDIA CDI package: %s", firstLine(stderr, err))
	}
	value := strings.TrimSpace(stdout)
	if value == missingPackageMarker {
		return "", nil
	}
	if value == "" || strings.Contains(value, "\n") {
		return "", fail(FailurePackageInvalid, "installed NVIDIA CDI package identity is malformed")
	}
	return value, nil
}

func containsLine(output, wanted string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == wanted {
			return true
		}
	}
	return false
}

const (
	wslProbeScript = `test -c /dev/dxg
test -r /usr/lib/wsl/lib/libcuda.so.1`
	installedPackageScript = `if rpm -q nvidia-container-toolkit-base >/dev/null 2>&1; then
  rpm -q --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}\n' nvidia-container-toolkit-base
else
  printf 'lctk-package-missing\n'
fi`
	installPackageScript = `umask 077
package_file="$(mktemp /var/tmp/lctk-nvidia-toolkit-XXXXXX.rpm)"
trap 'rm -f "$package_file"' EXIT
cat >"$package_file"
rpm -Uvh --replacepkgs "$package_file"`
)
