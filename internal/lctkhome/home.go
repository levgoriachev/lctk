// Package lctkhome resolves the per-user directory that holds LCTK host state.
//
// Host state is deliberately outside any repository. The registry binds a
// project_id to an authoritative host path, and per docs/security.md that
// binding, together with credentials and OAuth authorization state, must never live in Git where a
// repository author could influence it.
package lctkhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// EnvOverride names the environment variable that relocates the LCTK home.
// Tests set it so that they never touch real user state.
const EnvOverride = "LCTK_HOME"

// RuntimeDataEnvOverride names the environment variable that relocates the
// Podman WSL data directory. Podman's supported XDG_DATA_HOME contract then
// places the managed VM disk, images, volumes, and project indexes beneath it.
const RuntimeDataEnvOverride = "LCTK_RUNTIME_DATA_HOME"

// Dir returns the LCTK home directory without creating it.
//
// The override wins so that tests and portable installations stay isolated.
// Otherwise the platform convention applies: LocalAppData on Windows,
// Application Support on macOS, and the XDG data directory elsewhere.
func Dir() (string, error) {
	if override := os.Getenv(EnvOverride); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", EnvOverride, err)
		}
		return absolute, nil
	}
	if saved, err := loadSavedLocations(); err != nil {
		return "", err
	} else if saved.InstallDir != "" {
		return saved.InstallDir, nil
	}

	switch runtime.GOOS {
	case "windows":
		if base := os.Getenv("LocalAppData"); base != "" {
			return filepath.Join(base, "lctk"), nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve LCTK home: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "lctk"), nil
	default:
		if base := os.Getenv("XDG_DATA_HOME"); base != "" {
			return filepath.Join(base, "lctk"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve LCTK home: %w", err)
		}
		return filepath.Join(home, ".local", "share", "lctk"), nil
	}

	// Windows without LocalAppData set.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve LCTK home: neither LocalAppData nor a user home directory is available")
	}
	return filepath.Join(home, "AppData", "Local", "lctk"), nil
}

// RuntimeDataDir returns the host directory whose Podman-owned descendants
// contain the managed WSL disk, OCI images, volumes, and project indexes.
func RuntimeDataDir() (string, error) {
	if override := os.Getenv(RuntimeDataEnvOverride); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", RuntimeDataEnvOverride, err)
		}
		return absolute, nil
	}
	if saved, err := loadSavedLocations(); err != nil {
		return "", err
	} else if saved.RuntimeDataDir != "" {
		return saved.RuntimeDataDir, nil
	}
	return defaultRuntimeDataDir()
}

// EnsureDir returns the LCTK home directory, creating it when absent.
//
// The directory is private to the user because it will hold client credentials
// and OAuth authorization state. On Windows the mode is advisory and inherited ACLs apply.
func EnsureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create LCTK home %q: %w", dir, err)
	}
	return dir, nil
}
