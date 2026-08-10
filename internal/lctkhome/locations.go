package lctkhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Locations is the durable Windows installation layout selected in setup.
// InstallDir owns executables and host state. RuntimeDataDir is passed to
// Podman as XDG_DATA_HOME, so the WSL virtual disk and container data follow
// the user's storage choice without links or an ambient Podman installation.
type Locations struct {
	InstallDir     string
	RuntimeDataDir string
}

// CurrentLocations resolves the saved or platform-default setup choices.
func CurrentLocations() (Locations, error) {
	installDir, err := Dir()
	if err != nil {
		return Locations{}, err
	}
	runtimeDataDir, err := RuntimeDataDir()
	if err != nil {
		return Locations{}, err
	}
	return NormalizeLocations(installDir, runtimeDataDir)
}

// NormalizeLocations rejects ambiguous roots before setup can write anything.
// A drive root is never a valid product boundary because uninstall must be able
// to target the selected installation without risking unrelated user data.
func NormalizeLocations(installDir, runtimeDataDir string) (Locations, error) {
	installDir, err := normalizeLocation("installation", installDir)
	if err != nil {
		return Locations{}, err
	}
	runtimeDataDir, err = normalizeLocation("runtime data", runtimeDataDir)
	if err != nil {
		return Locations{}, err
	}
	return Locations{InstallDir: installDir, RuntimeDataDir: runtimeDataDir}, nil
}

func normalizeLocation(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s directory is empty", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s directory: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute)
	if volume != "" && strings.EqualFold(absolute, volume+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s directory cannot be a drive root", label)
	}
	if absolute == string(os.PathSeparator) || absolute == "." {
		return "", fmt.Errorf("%s directory cannot be a filesystem root", label)
	}
	return absolute, nil
}

// ConfigureProcess applies an already validated layout to this process and
// every child it starts, including the downloaded host core and private Podman.
func ConfigureProcess(locations Locations) error {
	normalized, err := NormalizeLocations(locations.InstallDir, locations.RuntimeDataDir)
	if err != nil {
		return err
	}
	if err := os.Setenv(EnvOverride, normalized.InstallDir); err != nil {
		return fmt.Errorf("set %s: %w", EnvOverride, err)
	}
	if err := os.Setenv(RuntimeDataEnvOverride, normalized.RuntimeDataDir); err != nil {
		return fmt.Errorf("set %s: %w", RuntimeDataEnvOverride, err)
	}
	return nil
}

// SaveLocations persists the layout only after setup has presented and the
// user has accepted the complete plan.
func SaveLocations(locations Locations) error {
	normalized, err := NormalizeLocations(locations.InstallDir, locations.RuntimeDataDir)
	if err != nil {
		return err
	}
	if err := savePlatformLocations(normalized); err != nil {
		return fmt.Errorf("save LCTK installation locations: %w", err)
	}
	return ConfigureProcess(normalized)
}

// ClearLocations removes the Windows discovery pointer after uninstall has
// successfully detached the product and its managed runtime.
func ClearLocations() error {
	if err := clearPlatformLocations(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear LCTK installation locations: %w", err)
	}
	return nil
}
