//go:build !windows

package lctkhome

import "os"

func loadSavedLocations() (Locations, error) { return Locations{}, nil }
func savePlatformLocations(Locations) error  { return nil }
func clearPlatformLocations() error          { return os.ErrNotExist }
func defaultRuntimeDataDir() (string, error) { return Dir() }
