//go:build windows

package uninstall

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lev-goryachev/lctk/internal/windowsprocess"
	"golang.org/x/sys/windows"
)

const deferredRemovalScript = `$ErrorActionPreference = 'SilentlyContinue'
$root = [Environment]::GetEnvironmentVariable('LCTK_REMOVE_ROOT', 'Process')
$parent = [int][Environment]::GetEnvironmentVariable('LCTK_REMOVE_PARENT', 'Process')
Wait-Process -Id $parent
for ($attempt = 0; $attempt -lt 40; $attempt++) {
    [System.IO.Directory]::Delete($root, $true)
    if (-not [System.IO.Directory]::Exists($root)) { exit 0 }
    Start-Sleep -Milliseconds 250
}
exit 1`

func scheduleRemoval(path string) error {
	removeErr := os.RemoveAll(path)
	if removeErr == nil {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running uninstaller: %w", err)
	}
	inside, err := containsPath(path, self)
	if err != nil {
		return err
	}
	if inside {
		return startDeferredRemoval(path, os.Getpid())
	}
	return removeErr
}

// containsPath proves that the only expected Windows sharing violation is the
// running uninstaller itself. Other deletion failures remain synchronous and
// visible instead of being hidden behind a detached retry.
func containsPath(root, candidate string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, fmt.Errorf("resolve removal root: %w", err)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false, fmt.Errorf("resolve running uninstaller: %w", err)
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, fmt.Errorf("compare removal paths: %w", err)
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

// startDeferredRemoval uses the signed-in Windows PowerShell already present
// on every supported host. It waits for this GUI process to close, removes the
// final locked directory, shows no console, and creates no script file.
func startDeferredRemoval(path string, parent int) error {
	command, err := deferredRemovalCommand(path, parent)
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start deferred uninstall cleanup: %w", err)
	}
	return nil
}

func deferredRemovalCommand(path string, parent int) (*exec.Cmd, error) {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return nil, fmt.Errorf("SystemRoot is unavailable for deferred uninstall cleanup")
	}
	powerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Stat(powerShell)
	if err != nil {
		return nil, fmt.Errorf("locate Windows PowerShell for deferred uninstall cleanup: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("Windows PowerShell cleanup path is a directory: %s", powerShell)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(stringsToUTF16LE(deferredRemovalScript)))
	command := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encoded)
	environment := command.Environ()
	filtered := environment[:0]
	for _, entry := range environment {
		name := strings.SplitN(entry, "=", 2)[0]
		if !strings.EqualFold(name, "LCTK_REMOVE_ROOT") && !strings.EqualFold(name, "LCTK_REMOVE_PARENT") {
			filtered = append(filtered, entry)
		}
	}
	command.Env = append(filtered, "LCTK_REMOVE_ROOT="+path, "LCTK_REMOVE_PARENT="+strconv.Itoa(parent))
	command.Stdin, command.Stdout, command.Stderr = nil, nil, nil
	windowsprocess.HideConsole(command)
	return command, nil
}

func stringsToUTF16LE(value string) string {
	encoded := windows.StringToUTF16(value)
	bytes := make([]byte, 0, len(encoded)*2)
	for _, unit := range encoded[:len(encoded)-1] {
		bytes = append(bytes, byte(unit), byte(unit>>8))
	}
	return string(bytes)
}
