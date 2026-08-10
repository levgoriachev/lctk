//go:build windows

package daemonstate

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// start launches the stable installation-owned launcher without a console.
// The launcher resolves the atomically selected host core before the daemon
// records its own process identity for later update or uninstall operations.
func start(home string) error {
	launcher := filepath.Join(home, "bin", "lctk.exe")
	command := exec.Command(launcher, "daemon")
	windowsprocess.HideConsole(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start LCTK daemon: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach LCTK daemon: %w", err)
	}
	return nil
}
