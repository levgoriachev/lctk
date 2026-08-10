//go:build windows

package main

import (
	"fmt"
	"os/exec"

	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// startBackgroundDaemon detaches the sign-in daemon without showing a console
// window; the daemon records its own verified process identity for uninstall.
func startBackgroundDaemon(executable string) error {
	command := exec.Command(executable, "daemon")
	windowsprocess.HideConsole(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start LCTK daemon: %w", err)
	}
	return command.Process.Release()
}
