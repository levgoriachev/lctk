//go:build !windows

package nvidiainstall

import "context"

// ProbeHost is unsupported outside the Windows setup target selected by
// ADR-0029; Linux containers are validated through the separate WSL/CDI gate.
func ProbeHost(context.Context) (GPU, error) {
	return GPU{}, fail(FailureAdapterMissing, "NVIDIA GPU setup is supported only by the Windows installer")
}
