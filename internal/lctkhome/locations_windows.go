//go:build windows

package lctkhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const registryPath = `Software\LCTK`

func loadSavedLocations() (Locations, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, registryPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return Locations{}, nil
	}
	if err != nil {
		return Locations{}, fmt.Errorf("open LCTK location registry: %w", err)
	}
	defer key.Close()
	installDir, _, installErr := key.GetStringValue("InstallDir")
	runtimeDataDir, _, runtimeErr := key.GetStringValue("RuntimeDataDir")
	if installErr != nil && !errors.Is(installErr, registry.ErrNotExist) {
		return Locations{}, fmt.Errorf("read LCTK installation directory: %w", installErr)
	}
	if runtimeErr != nil && !errors.Is(runtimeErr, registry.ErrNotExist) {
		return Locations{}, fmt.Errorf("read LCTK runtime data directory: %w", runtimeErr)
	}
	return Locations{InstallDir: installDir, RuntimeDataDir: runtimeDataDir}, nil
}

func savePlatformLocations(locations Locations) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, registryPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetStringValue("InstallDir", locations.InstallDir); err != nil {
		return err
	}
	return key.SetStringValue("RuntimeDataDir", locations.RuntimeDataDir)
}

func clearPlatformLocations() error {
	err := registry.DeleteKey(registry.CURRENT_USER, registryPath)
	if errors.Is(err, registry.ErrNotExist) {
		return os.ErrNotExist
	}
	return err
}

func defaultRuntimeDataDir() (string, error) {
	if localAppData := os.Getenv("LocalAppData"); localAppData != "" {
		return filepath.Join(localAppData, "lctk-runtime-data"), nil
	}
	profile := os.Getenv("USERPROFILE")
	if profile == "" {
		var err error
		profile, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve Podman data home: %w", err)
		}
	}
	// Keep the private Podman data outside its ambient shared default. The user
	// may still choose another absolute location before the machine exists.
	return filepath.Join(profile, "AppData", "Local", "lctk-runtime-data"), nil
}
