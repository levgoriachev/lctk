// Package uninstall removes the complete Windows product boundary and exports
// project volumes only when the user explicitly chooses preservation.
package uninstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/daemonstate"
	"github.com/lev-goryachev/lctk/internal/desktopinstall"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

type machineRunner interface {
	Run(context.Context, ...string) (string, string, error)
}

// Manager owns every uninstall side effect behind injectable boundaries.
type Manager struct {
	Home     string
	Registry func() (*projectregistry.Registry, error)
	Machine  machineRunner
	// MachineAbsent verifies the system WSL inventory when a partial removal has
	// already deleted the private Podman client needed for machine rm.
	MachineAbsent func(context.Context) (bool, error)
	Export        func(context.Context, string, string) error
	StopDaemon    func(string) error
	Unregister    func() error
	RuntimeData   func() (string, error)
	UserHome      func() (string, error)
	Cleanup       func(string, string) error
	Remove        func(string) error
}

// NewManager returns the production uninstaller.
func NewManager(home string) *Manager {
	return &Manager{Home: home, Registry: projectregistry.Load, Machine: machineCLI{}, Export: exportVolume,
		MachineAbsent: managedDistributionAbsent,
		StopDaemon:    daemonstate.Stop, Unregister: desktopinstall.Unregister,
		RuntimeData: lctkhome.RuntimeDataDir, UserHome: os.UserHomeDir,
		Cleanup: cleanupManagedRuntimeResidue, Remove: scheduleRemoval}
}

// Run stops the daemon, optionally exports each registered project volume,
// removes the managed machine and desktop integration, then schedules deletion
// of locked installer files.
func (m *Manager) Run(ctx context.Context, preserve bool) (string, error) {
	if m.Home == "" || m.Registry == nil || m.Machine == nil || m.Export == nil || m.StopDaemon == nil || m.Unregister == nil ||
		m.RuntimeData == nil || m.UserHome == nil || m.Cleanup == nil || m.Remove == nil {
		return "", errors.New("uninstaller is incomplete")
	}
	if err := m.StopDaemon(m.Home); err != nil {
		return "", fmt.Errorf("stop LCTK daemon: %w", err)
	}
	backup := ""
	if preserve {
		registry, err := m.Registry()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		backup = filepath.Join(m.Home, "preserved", time.Now().UTC().Format("20060102T150405Z"))
		if err := os.MkdirAll(backup, 0o700); err != nil {
			return "", err
		}
		if registry != nil {
			for _, project := range registry.List() {
				names, err := projectstack.DeriveNames(project.ID)
				if err != nil {
					return "", err
				}
				if err := m.Export(ctx, names.Volume, filepath.Join(backup, project.ID+".tar")); err != nil {
					return "", err
				}
			}
		}
	}
	if _, stderr, err := m.Machine.Run(ctx, "rm", "--force", containerruntime.MachineName); err != nil && !bytes.Contains(bytes.ToLower([]byte(stderr)), []byte("does not exist")) {
		if !errors.Is(err, containerruntime.ErrClientMissing) {
			return backup, fmt.Errorf("remove managed Podman machine: %s: %w", stderr, err)
		}
		if m.MachineAbsent == nil {
			return backup, fmt.Errorf("verify partially removed managed Podman machine: %w", err)
		}
		absent, verifyErr := m.MachineAbsent(ctx)
		if verifyErr != nil {
			return backup, verifyErr
		}
		if !absent {
			return backup, errors.New("managed WSL distribution lctk-runtime still exists but its private Podman client is missing")
		}
	}
	runtimeData, err := m.RuntimeData()
	if err != nil {
		return backup, fmt.Errorf("resolve managed runtime data: %w", err)
	}
	userHome, err := m.UserHome()
	if err != nil {
		return backup, fmt.Errorf("resolve user profile for managed runtime cleanup: %w", err)
	}
	// The machine is already stopped and removed at this point, so the remaining
	// cleanup targets are independent. Attempt every target and report their
	// combined errors instead of leaving later registrations untouched.
	var cleanupErrors []error
	if err := m.Cleanup(runtimeData, userHome); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := m.Unregister(); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if preserve {
		for _, name := range []string{"bin", "models", "runtime", "versions", "installation.json", "daemon.json"} {
			if err := m.Remove(filepath.Join(m.Home, name)); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		return backup, errors.Join(cleanupErrors...)
	}
	if err := m.Remove(m.Home); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return "", errors.Join(cleanupErrors...)
}

type machineCLI struct{}

func (machineCLI) Run(ctx context.Context, args ...string) (string, string, error) {
	command, err := containerruntime.MachineCommand(ctx, args...)
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}

func exportVolume(ctx context.Context, volume, target string) error {
	if _, stderr, err := (containerruntime.Runner{}).Run(ctx, "volume", "inspect", volume); err != nil {
		message := bytes.ToLower([]byte(stderr + " " + err.Error()))
		if bytes.Contains(message, []byte("no such volume")) || bytes.Contains(message, []byte("no such object")) {
			return nil
		}
		return fmt.Errorf("inspect volume %s: %s: %w", volume, stderr, err)
	}
	command, err := containerruntime.Command(ctx, "volume", "export", volume)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	command.Stdout = file
	var stderr bytes.Buffer
	command.Stderr = &stderr
	runErr := command.Run()
	closeErr := file.Close()
	if runErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return fmt.Errorf("export volume %s: %s: %w", volume, stderr.String(), runErr)
	}
	return nil
}
