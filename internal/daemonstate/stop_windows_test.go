//go:build windows

package daemonstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStopRemovesAStaleStateEvenWhenOpenProcessReturnsAccessDenied(t *testing.T) {
	home := writeDaemonState(t, 17248)
	restoreDaemonProcessSeams(t)
	openDaemonProcess = func(uint32, bool, uint32) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}
	daemonProcessExists = func(uint32) (bool, error) { return false, nil }
	if err := Stop(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale daemon state remains: %v", err)
	}
}

func TestStopRequestsAWaitableProcessHandle(t *testing.T) {
	home := writeDaemonState(t, 17248)
	restoreDaemonProcessSeams(t)
	var requestedAccess uint32
	openDaemonProcess = func(access uint32, _ bool, _ uint32) (windows.Handle, error) {
		requestedAccess = access
		return 0, windows.ERROR_ACCESS_DENIED
	}
	daemonProcessExists = func(uint32) (bool, error) { return false, nil }
	if err := Stop(home); err != nil {
		t.Fatal(err)
	}
	if requestedAccess&windows.SYNCHRONIZE == 0 {
		t.Fatalf("OpenProcess access %#x omits SYNCHRONIZE", requestedAccess)
	}
}

func TestStopDoesNotGuessWhenAnInaccessiblePIDStillExists(t *testing.T) {
	home := writeDaemonState(t, 17248)
	restoreDaemonProcessSeams(t)
	openDaemonProcess = func(uint32, bool, uint32) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}
	daemonProcessExists = func(uint32) (bool, error) { return true, nil }
	err := Stop(home)
	if err == nil || !strings.Contains(err.Error(), "recorded LCTK daemon process 17248") {
		t.Fatalf("Stop error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, FileName)); err != nil {
		t.Fatalf("live daemon state was removed: %v", err)
	}
}

func TestProcessExistsFindsTheCurrentProcess(t *testing.T) {
	found, err := processExists(uint32(os.Getpid()))
	if err != nil || !found {
		t.Fatalf("processExists(current) = %t, %v", found, err)
	}
}

func writeDaemonState(t *testing.T, pid int) string {
	t.Helper()
	home := t.TempDir()
	executable := filepath.Join(home, "versions", "1.0.0", "lctk-core.exe")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid":` + fmt.Sprint(pid) + `,"executable":` + strconv.Quote(executable) + `}`)
	if err := os.WriteFile(filepath.Join(home, FileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func restoreDaemonProcessSeams(t *testing.T) {
	t.Helper()
	oldOpen := openDaemonProcess
	oldExists := daemonProcessExists
	t.Cleanup(func() {
		openDaemonProcess = oldOpen
		daemonProcessExists = oldExists
	})
}
