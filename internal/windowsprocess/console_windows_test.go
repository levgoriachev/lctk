//go:build windows

package windowsprocess

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHideConsoleCreatesRequiredWindowsAttributes(t *testing.T) {
	command := exec.Command("cmd.exe", "/c", "exit", "0")
	HideConsole(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatal("console window is not hidden")
	}
	if command.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("creation flags=%#x", command.SysProcAttr.CreationFlags)
	}
}

func TestHideConsolePreservesExistingCreationFlags(t *testing.T) {
	command := exec.Command("cmd.exe", "/c", "exit", "0")
	HideConsole(command)
	command.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	HideConsole(command)
	if command.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatalf("existing creation flags were replaced: %#x", command.SysProcAttr.CreationFlags)
	}
}
