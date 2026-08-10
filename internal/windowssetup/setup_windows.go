//go:build windows

package windowssetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// Probe reads host, firmware, privilege, and WSL state without mutating them.
func Probe(ctx context.Context) (Status, error) {
	version := windows.RtlGetVersion()
	status := Status{
		Build:                  version.BuildNumber,
		Architecture:           runtime.GOARCH,
		VirtualizationFirmware: windows.IsProcessorFeaturePresent(windows.PF_VIRT_FIRMWARE_ENABLED),
		Elevated:               windows.Token(0).IsElevated(),
	}
	if status.Build < MinimumBuild || status.Architecture != "amd64" {
		return status, ErrUnsupportedHost
	}
	command := exec.CommandContext(ctx, "wsl.exe", "--status")
	windowsprocess.HideConsole(command)
	status.WSLReady = command.Run() == nil
	return evaluateHost(status)
}

// EnableWSL enables the two Windows optional features required by WSL2. A true
// result means Windows must reboot before setup can safely continue.
func EnableWSL(ctx context.Context) (bool, error) {
	if !windows.Token(0).IsElevated() {
		return false, ErrElevationRequired
	}
	for _, feature := range []string{"Microsoft-Windows-Subsystem-Linux", "VirtualMachinePlatform"} {
		command := exec.CommandContext(ctx, "dism.exe", "/online", "/enable-feature", "/featurename:"+feature, "/all", "/norestart")
		windowsprocess.HideConsole(command)
		output, err := command.CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3010 {
				return false, fmt.Errorf("enable Windows feature %s: %s: %w", feature, strings.TrimSpace(string(output)), err)
			}
		}
	}
	command := exec.CommandContext(ctx, "wsl.exe", "--install", "--no-distribution", "--web-download")
	windowsprocess.HideConsole(command)
	output, err := command.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 3010 {
			return false, fmt.Errorf("install the WSL2 kernel: %s: %w", strings.TrimSpace(string(output)), err)
		}
	}
	return true, nil
}

// RelaunchElevated starts the current setup through the UAC consent UI. The
// accepted unsigned release policy means Windows identifies its publisher as
// unknown; component integrity is enforced separately by the release manifest.
func RelaunchElevated(args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate setup executable: %w", err)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(executable)
	parameters, _ := windows.UTF16PtrFromString(joinArgs(args))
	if err := windows.ShellExecute(0, verb, file, parameters, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("request administrator approval: %w", err)
	}
	return nil
}

// RegisterResume resumes the same setup transaction once after the next login.
func RegisterResume() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\RunOnce`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open setup resume registry: %w", err)
	}
	defer key.Close()
	return key.SetStringValue("LCTK Setup", syscall.EscapeArg(executable)+" --resume")
}

func joinArgs(args []string) string {
	escaped := make([]string, 0, len(args))
	for _, arg := range args {
		escaped = append(escaped, syscall.EscapeArg(arg))
	}
	return strings.Join(escaped, " ")
}
