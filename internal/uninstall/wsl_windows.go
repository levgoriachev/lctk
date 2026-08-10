//go:build windows

package uninstall

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// managedDistributionAbsent uses the operating-system WSL inventory only when
// a partial uninstall has already removed the private Podman client. Absence is
// accepted only after this independent exact-name check succeeds.
func managedDistributionAbsent(ctx context.Context) (bool, error) {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return false, fmt.Errorf("SystemRoot is unavailable for managed WSL verification")
	}
	wsl := filepath.Join(systemRoot, "System32", "wsl.exe")
	info, err := os.Stat(wsl)
	if err != nil {
		return false, fmt.Errorf("locate system WSL client: %w", err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("system WSL client path is a directory: %s", wsl)
	}
	command := exec.CommandContext(ctx, wsl, "--list", "--quiet")
	windowsprocess.HideConsole(command)
	output, err := command.Output()
	if err != nil {
		return false, fmt.Errorf("list WSL distributions during partial uninstall: %w", err)
	}
	for _, name := range strings.Split(normalizeWSLOutput(output), "\n") {
		if strings.EqualFold(strings.TrimSpace(name), containerruntime.MachineName) {
			return false, nil
		}
	}
	return true, nil
}

// normalizeWSLOutput handles both UTF-16LE emitted by Windows console WSL and
// UTF-8 emitted by newer redirected implementations.
func normalizeWSLOutput(output []byte) string {
	if len(output) >= 2 && (output[0] == 0xff && output[1] == 0xfe || hasZeroByte(output)) {
		if len(output)%2 != 0 {
			output = output[:len(output)-1]
		}
		units := make([]uint16, 0, len(output)/2)
		for index := 0; index+1 < len(output); index += 2 {
			unit := binary.LittleEndian.Uint16(output[index : index+2])
			if unit != 0xfeff {
				units = append(units, unit)
			}
		}
		return string(utf16.Decode(units))
	}
	return string(output)
}

func hasZeroByte(output []byte) bool {
	for _, value := range output {
		if value == 0 {
			return true
		}
	}
	return false
}
