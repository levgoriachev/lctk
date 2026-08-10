// Package installation owns versioned host-core activation. The stable launcher
// reads one atomic document; update never replaces a running executable in place.
package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/diskspace"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

const (
	ActivationFile = "installation.json"
	SchemaVersion  = 1
	diskMargin     = 64 << 20
)

// RequiredBytes includes a second copy for verification/rollback plus the fixed
// activation safety margin used by bootstrap and update preflight.
func RequiredBytes(downloadBytes int64) int64 {
	if downloadBytes < 0 {
		return 0
	}
	return downloadBytes*2 + diskMargin
}

// Activation is the single host activation boundary. PreviousExecutable is the
// verified rollback target, never a guess derived from directory contents.
type Activation struct {
	SchemaVersion      int    `json:"schema_version"`
	ActiveVersion      string `json:"active_version"`
	ActiveExecutable   string `json:"active_executable"`
	ActiveSHA256       string `json:"active_sha256"`
	ActiveBytes        int64  `json:"active_bytes"`
	PreviousVersion    string `json:"previous_version,omitempty"`
	PreviousExecutable string `json:"previous_executable,omitempty"`
	PreviousSHA256     string `json:"previous_sha256,omitempty"`
	PreviousBytes      int64  `json:"previous_bytes,omitempty"`
	ActivatedAt        string `json:"activated_at"`
}

// Plan is read-only evidence for one host-core update.
type Plan struct {
	CurrentVersion string `json:"current_version,omitempty"`
	TargetVersion  string `json:"target_version"`
	Artifact       string `json:"artifact"`
	DownloadBytes  int64  `json:"download_bytes"`
	RequiredBytes  int64  `json:"required_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	Writes         bool   `json:"writes"`
	Ready          bool   `json:"ready"`
}

// Manager installs only beneath Home and exposes subprocess execution for
// deterministic rollback tests.
type Manager struct {
	Home      string
	Client    *http.Client
	Run       func(context.Context, string, ...string) ([]byte, error)
	Available func(string) (uint64, error)
}

// NewManager creates a production manager without creating its home.
func NewManager(home string) *Manager {
	return &Manager{
		Home:      home,
		Client:    http.DefaultClient,
		Run:       runExecutable,
		Available: diskspace.Available,
	}
}

// Inspect performs every host-core preflight without writing.
func (m *Manager) Inspect(manifest releasebundle.Manifest) (Plan, releasebundle.Artifact, error) {
	artifact, err := manifest.HostCore()
	if err != nil {
		return Plan{}, releasebundle.Artifact{}, err
	}
	if m.Home == "" {
		return Plan{}, releasebundle.Artifact{}, errors.New("installation home is empty")
	}
	activation, err := Load(m.Home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Plan{}, releasebundle.Artifact{}, err
	}
	if m.Client == nil || m.Run == nil || m.Available == nil {
		return Plan{}, releasebundle.Artifact{}, errors.New("installation manager is incomplete")
	}
	available, err := m.Available(m.Home)
	if err != nil {
		return Plan{}, releasebundle.Artifact{}, err
	}
	target := filepath.Join(m.Home, "versions", manifest.Version, coreName())
	download := artifact.Bytes
	if verifyFile(target, artifact.Bytes, artifact.SHA256) == nil {
		download = 0
	}
	required := RequiredBytes(download)
	plan := Plan{CurrentVersion: activation.ActiveVersion, TargetVersion: manifest.Version,
		Artifact: artifact.Name, DownloadBytes: download, RequiredBytes: required,
		AvailableBytes: available, Ready: available >= uint64(required)}
	return plan, artifact, nil
}

// Install downloads to the target version directory, verifies and executes the
// candidate, then atomically activates it. The prior activation remains named in
// the document and on disk for explicit or automatic rollback.
func (m *Manager) Install(ctx context.Context, manifest releasebundle.Manifest) (Activation, error) {
	plan, artifact, err := m.Inspect(manifest)
	if err != nil {
		return Activation{}, err
	}
	if !plan.Ready {
		return Activation{}, fmt.Errorf("update requires %d bytes with safety margin; only %d are available", plan.RequiredBytes, plan.AvailableBytes)
	}
	current, loadErr := Load(m.Home)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return Activation{}, loadErr
	}
	targetDir := filepath.Join(m.Home, "versions", manifest.Version)
	target := filepath.Join(targetDir, coreName())
	if loadErr == nil && current.ActiveVersion == manifest.Version {
		active, err := Resolve(m.Home, current.ActiveExecutable)
		if err != nil {
			return Activation{}, err
		}
		if current.ActiveBytes != artifact.Bytes || current.ActiveSHA256 != artifact.SHA256 || !samePath(active, target) {
			return Activation{}, errors.New("installed release uses a different immutable host-core identity for the same version")
		}
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return Activation{}, fmt.Errorf("create target version directory: %w", err)
	}
	if verifyFile(target, artifact.Bytes, artifact.SHA256) != nil {
		if err := m.download(ctx, artifact, target); err != nil {
			return Activation{}, err
		}
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return Activation{}, fmt.Errorf("make candidate executable: %w", err)
	}
	output, err := m.Run(ctx, target, "version", "--json")
	if err != nil {
		return Activation{}, fmt.Errorf("candidate host self-test failed: %w", err)
	}
	var info struct {
		Version string `json:"version"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if err := json.Unmarshal(output, &info); err != nil || info.Version != manifest.Version || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		return Activation{}, errors.New("candidate host self-test returned an incompatible identity")
	}
	// Repairing the exact same immutable release restores missing bytes without
	// overwriting its previous-version rollback pointer with a self-reference.
	if loadErr == nil && current.ActiveVersion == manifest.Version {
		return current, nil
	}
	relative, err := filepath.Rel(m.Home, target)
	if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return Activation{}, errors.New("candidate executable escaped the installation home")
	}
	next := Activation{
		SchemaVersion:      SchemaVersion,
		ActiveVersion:      manifest.Version,
		ActiveExecutable:   filepath.ToSlash(relative),
		ActiveSHA256:       artifact.SHA256,
		ActiveBytes:        artifact.Bytes,
		PreviousVersion:    current.ActiveVersion,
		PreviousExecutable: current.ActiveExecutable,
		PreviousSHA256:     current.ActiveSHA256,
		PreviousBytes:      current.ActiveBytes,
		ActivatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeActivation(m.Home, next); err != nil {
		return Activation{}, err
	}
	return next, nil
}

// samePath applies Windows' case-insensitive path identity while retaining the
// ordinary exact comparison used by the other supported build hosts.
func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

// Adopt copies the currently executing packaged core into the versioned store
// and creates the initial activation boundary. Bootstrap uses it after all
// component self-tests pass; it never changes an existing different activation.
func (m *Manager) Adopt(executable, version string) (Activation, error) {
	if executable == "" || version == "" {
		return Activation{}, errors.New("packaged host core path or version is empty")
	}
	if current, err := Load(m.Home); err == nil {
		if current.ActiveVersion != version {
			return Activation{}, errors.New("bootstrap cannot replace an existing activation; use lctk update")
		}
		return current, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Activation{}, err
	}
	size, digest, err := fileIdentity(executable)
	if err != nil {
		return Activation{}, fmt.Errorf("identify packaged host core: %w", err)
	}
	available, err := m.Available(m.Home)
	if err != nil {
		return Activation{}, err
	}
	if required := RequiredBytes(size); available < uint64(required) {
		return Activation{}, fmt.Errorf("bootstrap requires %d bytes with safety margin; only %d are available", required, available)
	}
	targetDir := filepath.Join(m.Home, "versions", version)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return Activation{}, fmt.Errorf("create initial host version directory: %w", err)
	}
	target := filepath.Join(targetDir, coreName())
	if err := copyVerifiedFile(executable, target, size, digest); err != nil {
		return Activation{}, err
	}
	relative, err := filepath.Rel(m.Home, target)
	if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		return Activation{}, errors.New("initial host core escaped the installation home")
	}
	activation := Activation{
		SchemaVersion:    SchemaVersion,
		ActiveVersion:    version,
		ActiveExecutable: filepath.ToSlash(relative),
		ActiveSHA256:     digest,
		ActiveBytes:      size,
		ActivatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeActivation(m.Home, activation); err != nil {
		return Activation{}, err
	}
	return activation, nil
}

func (m *Manager) download(ctx context.Context, artifact releasebundle.Artifact, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return fmt.Errorf("create host-core download request: %w", err)
	}
	response, err := m.Client.Do(request)
	if err != nil {
		return fmt.Errorf("download host core: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download host core returned %s", response.Status)
	}
	temporary := target + ".download"
	if _, err := os.Stat(temporary); err == nil {
		return errors.New("an unfinished host-core download exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect unfinished host-core download: %w", err)
	}
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create host-core download: %w", err)
	}
	defer os.Remove(temporary)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, artifact.Bytes+1))
	if copyErr != nil {
		file.Close()
		return fmt.Errorf("write host-core download: %w", copyErr)
	}
	if written != artifact.Bytes || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		file.Close()
		return errors.New("downloaded host core does not match the signed size and digest")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush host-core download: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close host-core download: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("activate downloaded host core: %w", err)
	}
	return nil
}

// VerifyRollback performs the read-only rollback preflight. Callers that also
// restore persistent project state use it before touching any database, while
// Rollback repeats the verification immediately before host activation to
// close the tampering window between plan and commit.
func (m *Manager) VerifyRollback() (Activation, error) {
	current, err := Load(m.Home)
	if err != nil {
		return Activation{}, err
	}
	if current.PreviousExecutable == "" {
		return Activation{}, errors.New("no previous host core is available")
	}
	previous, err := Resolve(m.Home, current.PreviousExecutable)
	if err != nil {
		return Activation{}, err
	}
	if err := verifyFile(previous, current.PreviousBytes, current.PreviousSHA256); err != nil {
		return Activation{}, fmt.Errorf("previous host core is unavailable or invalid: %w", err)
	}
	return current, nil
}

// Rollback atomically selects the previous verified core.
func (m *Manager) Rollback() (Activation, error) {
	current, err := m.VerifyRollback()
	if err != nil {
		return Activation{}, err
	}
	next := Activation{
		SchemaVersion:      SchemaVersion,
		ActiveVersion:      current.PreviousVersion,
		ActiveExecutable:   current.PreviousExecutable,
		ActiveSHA256:       current.PreviousSHA256,
		ActiveBytes:        current.PreviousBytes,
		PreviousVersion:    current.ActiveVersion,
		PreviousExecutable: current.ActiveExecutable,
		PreviousSHA256:     current.ActiveSHA256,
		PreviousBytes:      current.ActiveBytes,
		ActivatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeActivation(m.Home, next); err != nil {
		return Activation{}, err
	}
	return next, nil
}

// Load reads the one activation boundary without creating anything.
func Load(home string) (Activation, error) {
	encoded, err := os.ReadFile(filepath.Join(home, ActivationFile))
	if err != nil {
		return Activation{}, err
	}
	var activation Activation
	if err := json.Unmarshal(encoded, &activation); err != nil {
		return Activation{}, fmt.Errorf("decode installation activation: %w", err)
	}
	if activation.SchemaVersion != SchemaVersion || activation.ActiveVersion == "" ||
		activation.ActiveBytes <= 0 || !validDigest(activation.ActiveSHA256) {
		return Activation{}, errors.New("installation activation is invalid")
	}
	if _, err := Resolve(home, activation.ActiveExecutable); err != nil {
		return Activation{}, err
	}
	previousFields := activation.PreviousVersion != "" || activation.PreviousExecutable != "" ||
		activation.PreviousSHA256 != "" || activation.PreviousBytes != 0
	if previousFields && (activation.PreviousVersion == "" || activation.PreviousExecutable == "" ||
		activation.PreviousBytes <= 0 || !validDigest(activation.PreviousSHA256)) {
		return Activation{}, errors.New("previous installation activation is incomplete")
	}
	if activation.PreviousExecutable != "" {
		if _, err := Resolve(home, activation.PreviousExecutable); err != nil {
			return Activation{}, err
		}
	}
	return activation, nil
}

// ActiveExecutable resolves and verifies the selected core before the stable
// launcher executes it. A corrupted active binary is never run.
func ActiveExecutable(home string) (string, Activation, error) {
	activation, err := Load(home)
	if err != nil {
		return "", Activation{}, err
	}
	executable, err := Resolve(home, activation.ActiveExecutable)
	if err != nil {
		return "", Activation{}, err
	}
	if err := verifyFile(executable, activation.ActiveBytes, activation.ActiveSHA256); err != nil {
		return "", Activation{}, fmt.Errorf("active host core is unavailable or invalid: %w", err)
	}
	return executable, activation, nil
}

// VerifyExecutable checks a packaged executable against identity embedded in
// the stable launcher before any installation activation exists.
func VerifyExecutable(path string, size int64, digest string) error {
	return verifyFile(path, size, digest)
}

// Resolve converts only a validated activation-relative path into a host path.
func Resolve(home, relative string) (string, error) {
	if home == "" || relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("activation executable must be relative to a non-empty installation home")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("activation executable escapes installation home")
	}
	return filepath.Join(home, clean), nil
}

func writeActivation(home string, activation Activation) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create installation home: %w", err)
	}
	encoded, err := json.MarshalIndent(activation, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installation activation: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary := filepath.Join(home, "."+ActivationFile+".tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary installation activation: %w", err)
	}
	defer os.Remove(temporary)
	if _, err := file.Write(encoded); err != nil {
		file.Close()
		return fmt.Errorf("write temporary installation activation: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("flush temporary installation activation: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary installation activation: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(home, ActivationFile)); err != nil {
		return fmt.Errorf("activate installation: %w", err)
	}
	return nil
}

func verifyFile(path string, size int64, digest string) error {
	if size <= 0 || !validDigest(digest) {
		return errors.New("installed host core identity is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != size {
		return errors.New("installed host core size differs")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("installed host core digest differs")
	}
	return nil
}

func fileIdentity(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func copyVerifiedFile(source, target string, size int64, digest string) error {
	if verifyFile(target, size, digest) == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := target + ".adopt"
	output, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create adopted host core: %w", err)
	}
	defer os.Remove(temporary)
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("copy adopted host core: %w", err)
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return fmt.Errorf("flush adopted host core: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close adopted host core: %w", err)
	}
	if err := verifyFile(temporary, size, digest); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("activate adopted host core: %w", err)
	}
	return nil
}

func validDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && digest == strings.ToLower(digest)
}

func coreName() string {
	if runtime.GOOS == "windows" {
		return "lctk-core.exe"
	}
	return "lctk-core"
}

func runExecutable(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	windowsprocess.HideConsole(command)
	return command.Output()
}
