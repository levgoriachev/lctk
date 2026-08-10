package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

func TestInspectSelectionRejectsUnsafeLocationsBeforeBuildingAPlan(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	_, _, err := inspectSelection(context.Background(), setupRequest{}, lctkhome.Locations{
		InstallDir: root, RuntimeDataDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("inspectSelection accepted a drive root as the installation directory")
	}
}

func TestSetupModesAreMutuallyExclusiveBeforeAnyMutation(t *testing.T) {
	err := run(t.Context(), []string{"--admin", "--uninstall"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("run error = %v", err)
	}
}

func TestCustomAdminAddressRequiresAdminMode(t *testing.T) {
	err := run(t.Context(), []string{"--listen", "127.0.0.1:4455"})
	if err == nil || !strings.Contains(err.Error(), "only together with --admin") {
		t.Fatalf("run error = %v", err)
	}
}
