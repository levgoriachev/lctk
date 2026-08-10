//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const adminWindowClassName = "LCTKNativeAdminWindow"

type installedAdminTarget struct {
	Window     uintptr
	Process    windows.Handle
	ProcessID  uint32
	Executable string
}

var (
	findInstalledAdminTargets = enumerateInstalledAdminTargets
	postInstalledAdminClose   = func(window uintptr) error {
		result, _, err := procPostMessageW.Call(window, wmClose, 0, 0)
		if result == 0 {
			if err == nil || errors.Is(err, windows.ERROR_SUCCESS) {
				return errors.New("PostMessageW returned false")
			}
			return err
		}
		return nil
	}
	waitInstalledAdminProcess = windows.WaitForSingleObject
	closeInstalledAdminHandle = windows.CloseHandle
)

// releaseInstalledAdminExecutable closes only the native Admin window whose
// process is the exact setup executable in the accepted installation root. A
// running Admin window has no unsaved state, but its mapped executable prevents
// Windows from atomically activating the verified replacement during update.
func releaseInstalledAdminExecutable(installDir string) error {
	expected := filepath.Clean(filepath.Join(installDir, "bin", "lctk-setup.exe"))
	targets, err := findInstalledAdminTargets()
	if err != nil {
		return err
	}
	for _, target := range targets {
		defer closeInstalledAdminHandle(target.Process)
	}
	for _, target := range targets {
		if !strings.EqualFold(filepath.Clean(target.Executable), expected) {
			continue
		}
		if err := postInstalledAdminClose(target.Window); err != nil {
			return fmt.Errorf("close installed LCTK Admin process %d: %w", target.ProcessID, err)
		}
		result, err := waitInstalledAdminProcess(target.Process, 10_000)
		if err != nil {
			return fmt.Errorf("wait for installed LCTK Admin process %d: %w", target.ProcessID, err)
		}
		if result != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("installed LCTK Admin process %d did not close within 10 seconds", target.ProcessID)
		}
	}
	return nil
}

// enumerateInstalledAdminTargets verifies class, process handle, and full image
// path before any close message is allowed. A same-named process elsewhere is
// returned for exact-path filtering and is never mutated.
func enumerateInstalledAdminTargets() ([]installedAdminTarget, error) {
	var targets []installedAdminTarget
	var enumerationErr error
	callback := windows.NewCallback(func(window uintptr, _ uintptr) uintptr {
		className := make([]uint16, 256)
		copied, err := windows.GetClassName(windows.HWND(window), &className[0], int32(len(className)))
		if err != nil || windows.UTF16ToString(className[:copied]) != adminWindowClassName {
			return 1
		}
		var processID uint32
		if _, err := windows.GetWindowThreadProcessId(windows.HWND(window), &processID); err != nil {
			enumerationErr = fmt.Errorf("identify installed LCTK Admin window: %w", err)
			return 0
		}
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, processID)
		if err != nil {
			enumerationErr = fmt.Errorf("open installed LCTK Admin process %d: %w", processID, err)
			return 0
		}
		buffer := make([]uint16, 32768)
		size := uint32(len(buffer))
		if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
			_ = windows.CloseHandle(process)
			enumerationErr = fmt.Errorf("identify installed LCTK Admin process %d: %w", processID, err)
			return 0
		}
		targets = append(targets, installedAdminTarget{Window: window, Process: process, ProcessID: processID,
			Executable: windows.UTF16ToString(buffer[:size])})
		return 1
	})
	if err := windows.EnumWindows(callback, unsafe.Pointer(nil)); err != nil && !errors.Is(err, windows.ERROR_SUCCESS) {
		for _, target := range targets {
			_ = windows.CloseHandle(target.Process)
		}
		return nil, fmt.Errorf("enumerate LCTK Admin windows: %w", err)
	}
	if enumerationErr != nil {
		for _, target := range targets {
			_ = windows.CloseHandle(target.Process)
		}
		return nil, enumerationErr
	}
	return targets, nil
}
