//go:build windows

package nvidiainstall

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lev-goryachev/lctk/internal/windowsprocess"
)

// ProbeHost executes only the trusted driver utility beneath System32. PATH is
// not consulted because setup decisions must not be influenced by a repository
// or another NVIDIA utility installed elsewhere on the machine.
func ProbeHost(ctx context.Context) (GPU, error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return GPU{}, fail(FailureAdapterMissing, "Windows SystemRoot is unavailable; cannot locate the trusted NVIDIA driver utility")
	}
	path := filepath.Join(systemRoot, "System32", "nvidia-smi.exe")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return GPU{}, fail(FailureAdapterMissing, "Windows NVIDIA driver utility is unavailable at %s", path)
	}
	command := exec.CommandContext(ctx, path,
		"--query-gpu=name,driver_version,memory.total,compute_cap",
		"--format=csv,noheader,nounits")
	windowsprocess.HideConsole(command)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return GPU{}, fail(FailureAdapterMissing, "Windows NVIDIA probe failed: %s", firstLine(stderr.String(), err))
	}
	gpu, err := ParseHostProbe(stdout.String())
	if err != nil {
		return GPU{}, err
	}
	if gpu.Name == "" {
		return GPU{}, fmt.Errorf("NVIDIA probe returned an empty adapter identity")
	}
	return gpu, nil
}
