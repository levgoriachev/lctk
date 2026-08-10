//go:build windows

package main

import (
	"fmt"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"golang.org/x/sys/windows"
)

const (
	messageBoxYes = 6
	messageBoxNo  = 7
)

func showError(message string) {
	text, _ := windows.UTF16PtrFromString(message)
	title, _ := windows.UTF16PtrFromString("LCTK Setup")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
}

func showInfo(message string) {
	text, _ := windows.UTF16PtrFromString(message)
	title, _ := windows.UTF16PtrFromString("LCTK Setup")
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONINFORMATION)
}

// confirmUninstall uses three explicit outcomes: Yes preserves project state
// archives, No removes all LCTK data, and Cancel changes nothing.
func confirmUninstall(locations lctkhome.Locations) (bool, bool) {
	message, _ := windows.UTF16PtrFromString(fmt.Sprintf(
		"Remove LCTK and its managed runtime?\n\nProgram and host state:\n%s\n\nManaged WSL disk, images, volumes, indexes and memory:\n%s\n\nYes: export project state archives, then remove the program and runtime.\nNo: remove all LCTK program and runtime data.\nCancel: keep the installation.",
		locations.InstallDir, locations.RuntimeDataDir))
	title, _ := windows.UTF16PtrFromString("Uninstall LCTK")
	result, _ := windows.MessageBox(0, message, title, windows.MB_YESNOCANCEL|windows.MB_ICONWARNING)
	switch result {
	case messageBoxYes:
		return true, true
	case messageBoxNo:
		return false, true
	default:
		return false, false
	}
}
