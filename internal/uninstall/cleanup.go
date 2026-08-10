package uninstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
)

// cleanupManagedRuntimeResidue removes only paths whose names are bound to
// LCTK's one managed machine. Podman's shared parents are removed only when
// empty, so an unrelated Podman installation remains outside our authority.
func cleanupManagedRuntimeResidue(runtimeData, userHome string) error {
	if runtimeData == "" || userHome == "" {
		return errors.New("runtime data or user home directory is empty")
	}
	machineData := filepath.Join(runtimeData, "containers", "podman", "machine", "wsl")
	exact := []string{
		filepath.Join(machineData, containerruntime.MachineName),
		filepath.Join(machineData, "wsldist", containerruntime.MachineName),
	}
	cacheMatches, err := filepath.Glob(filepath.Join(machineData, containerruntime.MachineName+"-*"))
	if err != nil {
		return fmt.Errorf("resolve managed Podman machine cache: %w", err)
	}
	exact = append(exact, cacheMatches...)
	config := filepath.Join(userHome, ".config", "containers", "podman", "machine", "wsl")
	for _, suffix := range []string{".json", ".ign", ".lock"} {
		exact = append(exact, filepath.Join(config, containerruntime.MachineName+suffix))
	}
	for _, path := range exact {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove managed runtime residue %q: %w", path, err)
		}
	}
	containersRoot := filepath.Join(runtimeData, "containers")
	orphaned, err := onlyKnownPodmanScaffold(containersRoot)
	if err != nil {
		return err
	}
	if orphaned {
		if err := os.RemoveAll(containersRoot); err != nil {
			return fmt.Errorf("remove orphaned managed Podman scaffold %q: %w", containersRoot, err)
		}
		return removeDirectoryIfEmpty(runtimeData)
	}
	// These directories are all Podman-owned containers. Removing an empty one
	// is cleanup; a non-empty one belongs to another machine and is preserved.
	for _, path := range []string{
		filepath.Join(machineData, "wsldist"), machineData,
		filepath.Join(runtimeData, "containers", "podman", "machine"),
		filepath.Join(runtimeData, "containers", "podman"),
		config,
		filepath.Dir(config), filepath.Dir(filepath.Dir(config)),
	} {
		if err := removeDirectoryIfEmpty(path); err != nil {
			return err
		}
	}
	return removeDirectoryIfEmpty(runtimeData)
}

// onlyKnownPodmanScaffold identifies the exact empty Windows client residue
// left after the sole LCTK WSL machine is removed. Any unknown path proves the
// data root is shared and prevents broad deletion.
func onlyKnownPodmanScaffold(root string) (bool, error) {
	allowedDirectories := map[string]bool{
		".": true, "podman": true, "podman/machine": true,
		"podman/machine/wsl": true, "podman/machine/wsl/cache": true,
		"podman/machine/wsl/wsldist": true,
		"podman/machine/hyperv":      true, "podman/machine/hyperv/cache": true,
		"storage": true, "storage/containers": true,
	}
	allowedFiles := map[string]bool{
		"podman/machine/machine": true, "podman/machine/machine.pub": true,
		"podman/machine/port-alloc.dat": true, "podman/machine/port-alloc.lck": true,
	}
	found := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		found = true
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if allowedDirectories[relative] {
				return nil
			}
			return errUnknownScaffold
		}
		if allowedFiles[relative] {
			return nil
		}
		return errUnknownScaffold
	})
	if errors.Is(err, errUnknownScaffold) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect managed Podman scaffold %q: %w", root, err)
	}
	return found, nil
}

var errUnknownScaffold = errors.New("Podman scaffold contains an unknown path")

func removeDirectoryIfEmpty(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect cleanup directory %q: %w", path, err)
	}
	if len(entries) != 0 {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove empty cleanup directory %q: %w", path, err)
	}
	return nil
}
