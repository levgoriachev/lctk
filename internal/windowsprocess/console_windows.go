//go:build windows

// Package windowsprocess applies the Windows process attributes required by
// LCTK's native graphical surfaces.
package windowsprocess

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsole prevents a console executable used as an implementation detail
// from flashing a terminal window. Existing process attributes are preserved
// because callers may already carry security or lifecycle flags.
func HideConsole(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.HideWindow = true
	command.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
