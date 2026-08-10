// Package containerruntime owns the private Podman transport used by LCTK.
//
// The executable and connection are selected by absolute, installation-owned
// identity. LCTK never consults PATH or Podman's default connection because
// either would let unrelated user configuration change the runtime boundary
// accepted in ADR-0023.
package containerruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

const (
	// Provider is the bounded OCI implementation selected by ADR-0023.
	Provider = "podman"
	// Version is the portable client and machine-OS compatibility line shipped
	// by one LCTK release. Podman does not support a host/machine version skew.
	Version = "5.8.2"
	// MachineName is the only WSL-backed Podman machine LCTK owns.
	MachineName = "lctk-runtime"
	// ConnectionName selects the root connection created for MachineName. The
	// explicit connection prevents a user's Podman default from affecting LCTK.
	ConnectionName = MachineName + "-root"
	// ExecutableOverride relocates the private client for isolated tests and
	// source-development proofs. Production setup never writes this variable.
	ExecutableOverride = "LCTK_PODMAN_PATH"
)

// ErrClientMissing distinguishes an incomplete installation from a stopped or
// unhealthy managed machine.
var ErrClientMissing = errors.New("the managed Podman client is not installed")

// MachineExists reports whether the one LCTK-owned WSL machine already exists
// at the currently selected runtime-data location. Setup uses this read-only
// gate to prevent an implicit and destructive storage migration.
func MachineExists(ctx context.Context) (bool, error) {
	command, err := MachineCommand(ctx, "inspect", MachineName)
	if err != nil {
		if errors.Is(err, ErrClientMissing) {
			return false, nil
		}
		return false, err
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return true, nil
	}
	message := strings.ToLower(string(output) + " " + err.Error())
	if strings.Contains(message, "does not exist") || strings.Contains(message, "no such") {
		return false, nil
	}
	return false, fmt.Errorf("inspect managed Podman machine: %s: %w", strings.TrimSpace(string(output)), err)
}

// ExecutablePath resolves the private client without creating any directory.
func ExecutablePath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(ExecutableOverride)); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", ExecutableOverride, err)
		}
		return absolute, nil
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	name := "podman"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, "runtime", Provider, Version, "bin", name), nil
}

// Command builds one OCI command against LCTK's explicit machine connection.
func Command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := ExecutablePath()
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		if statErr == nil {
			statErr = errors.New("path is a directory")
		}
		return nil, fmt.Errorf("%w at %q: %v", ErrClientMissing, path, statErr)
	}
	selected := []string{"--connection", ConnectionName}
	selected = append(selected, args...)
	command := exec.CommandContext(ctx, path, selected...)
	windowsprocess.HideConsole(command)
	if err := attachRuntimeDataHome(command); err != nil {
		return nil, err
	}
	return command, nil
}

// MachineCommand builds one local machine-lifecycle command. Machine commands
// cannot use --connection because they create and start that connection.
func MachineCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := ExecutablePath()
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		if statErr == nil {
			statErr = errors.New("path is a directory")
		}
		return nil, fmt.Errorf("%w at %q: %v", ErrClientMissing, path, statErr)
	}
	selected := []string{"machine"}
	selected = append(selected, args...)
	command := exec.CommandContext(ctx, path, selected...)
	windowsprocess.HideConsole(command)
	if err := attachRuntimeDataHome(command); err != nil {
		return nil, err
	}
	return command, nil
}

// attachRuntimeDataHome gives every private Podman process the same explicit
// WSL storage root selected in setup and an installation-owned configuration
// root. Rebuilding both values prevents ambient Podman configuration from
// changing LCTK's machine identity and lets uninstall remove the configuration
// together with the installation home.
func attachRuntimeDataHome(command *exec.Cmd) error {
	dataHome, err := lctkhome.RuntimeDataDir()
	if err != nil {
		return err
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	configHome := filepath.Join(home, "runtime", Provider, "config")
	environment := command.Environ()
	filtered := environment[:0]
	for _, entry := range environment {
		name := strings.SplitN(entry, "=", 2)[0]
		if !strings.EqualFold(name, "XDG_DATA_HOME") && !strings.EqualFold(name, "XDG_CONFIG_HOME") {
			filtered = append(filtered, entry)
		}
	}
	command.Env = append(filtered, "XDG_DATA_HOME="+dataHome, "XDG_CONFIG_HOME="+configHome)
	return nil
}

// Runner executes OCI commands through the private client.
type Runner struct{}

// Run captures one command without allowing inherited stdin or an interactive
// prompt. Installation and authentication failures must fail fast instead.
func (Runner) Run(ctx context.Context, args ...string) (string, string, error) {
	command, err := Command(ctx, args...)
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}

// Load streams one already verified container image archive into LCTK's private Podman
// connection. Streaming through stdin is required for a remote Podman client:
// an --input path would be interpreted inside the managed Linux machine rather
// than on the Windows host that downloaded and authenticated the artifact.
func Load(ctx context.Context, input io.Reader) (string, string, error) {
	if input == nil {
		return "", "", errors.New("OCI image archive input is nil")
	}
	command, err := Command(ctx, "load")
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	command.Stdin = input
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}
