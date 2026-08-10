//go:build windows

package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReleaseInstalledAdminExecutableClosesOnlyTheExactInstalledProcess(t *testing.T) {
	restoreInstalledAdminSeams(t)
	installDir := filepath.Join(t.TempDir(), "lctk")
	findInstalledAdminTargets = func() ([]installedAdminTarget, error) {
		return []installedAdminTarget{
			{Window: 11, Process: 21, ProcessID: 31, Executable: filepath.Join(installDir, "bin", "lctk-setup.exe")},
			{Window: 12, Process: 22, ProcessID: 32, Executable: filepath.Join(t.TempDir(), "lctk-setup.exe")},
		}, nil
	}
	var closedWindows []uintptr
	postInstalledAdminClose = func(window uintptr) error { closedWindows = append(closedWindows, window); return nil }
	var waited []windows.Handle
	waitInstalledAdminProcess = func(handle windows.Handle, _ uint32) (uint32, error) {
		waited = append(waited, handle)
		return windows.WAIT_OBJECT_0, nil
	}
	var released []windows.Handle
	closeInstalledAdminHandle = func(handle windows.Handle) error { released = append(released, handle); return nil }
	if err := releaseInstalledAdminExecutable(installDir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closedWindows, []uintptr{11}) || !reflect.DeepEqual(waited, []windows.Handle{21}) {
		t.Fatalf("closed windows=%v waited=%v", closedWindows, waited)
	}
	if !reflect.DeepEqual(released, []windows.Handle{22, 21}) {
		t.Fatalf("released handles=%v", released)
	}
}

func TestReleaseInstalledAdminExecutableFailsWhenTheExactProcessDoesNotExit(t *testing.T) {
	restoreInstalledAdminSeams(t)
	installDir := filepath.Join(t.TempDir(), "lctk")
	findInstalledAdminTargets = func() ([]installedAdminTarget, error) {
		return []installedAdminTarget{{Window: 11, Process: 21, ProcessID: 31,
			Executable: filepath.Join(installDir, "bin", "lctk-setup.exe")}}, nil
	}
	postInstalledAdminClose = func(uintptr) error { return nil }
	waitInstalledAdminProcess = func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
	closeInstalledAdminHandle = func(windows.Handle) error { return nil }
	err := releaseInstalledAdminExecutable(installDir)
	if err == nil || !strings.Contains(err.Error(), "did not close within 10 seconds") {
		t.Fatalf("error=%v", err)
	}
}

func restoreInstalledAdminSeams(t *testing.T) {
	t.Helper()
	oldFind := findInstalledAdminTargets
	oldPost := postInstalledAdminClose
	oldWait := waitInstalledAdminProcess
	oldClose := closeInstalledAdminHandle
	t.Cleanup(func() {
		findInstalledAdminTargets = oldFind
		postInstalledAdminClose = oldPost
		waitInstalledAdminProcess = oldWait
		closeInstalledAdminHandle = oldClose
	})
}
