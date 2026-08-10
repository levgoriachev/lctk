// Package nvidiainstall owns the immutable NVIDIA WSL/CDI distribution and
// every compatibility gate required before LCTK may report GPU inference.
package nvidiainstall

import (
	"errors"
	"fmt"
)

const (
	// Image matches the exact llama.cpp b10257 source generation used by the
	// CPU image while adding the pinned CUDA 12.8.1 runtime.
	Image                = "ghcr.io/ggml-org/llama.cpp:server-cuda-b10257@sha256:37dd122824e58af9ec861955242abdeeade5a1dcf0ad768bf2b37f903c2805c6"
	ImageDigest          = "sha256:37dd122824e58af9ec861955242abdeeade5a1dcf0ad768bf2b37f903c2805c6"
	ImageCompressedBytes = int64(2586107421)
	ImageUnpackedBytes   = int64(4360073216)

	ToolkitVersion      = "1.19.1"
	ToolkitRelease      = "1"
	ToolkitArch         = "x86_64"
	ToolkitName         = "nvidia-container-toolkit-base"
	ToolkitNEVRA        = ToolkitName + "-" + ToolkitVersion + "-" + ToolkitRelease + "." + ToolkitArch
	ToolkitFileName     = ToolkitNEVRA + ".rpm"
	ToolkitURL          = "https://nvidia.github.io/libnvidia-container/stable/rpm/x86_64/" + ToolkitFileName
	ToolkitBytes        = int64(6190068)
	ToolkitSHA256       = "b12de77bdffd3df13cea4589a1b04a133b1ffcb250b860f7349420eed37aeb5d"
	ToolkitArtifactKind = "nvidia-container-toolkit-base"
	CDIDevice           = "nvidia.com/gpu=all"
	MinimumDriverMajor  = 572
	MinimumDriverMinor  = 61
	MinimumComputeMajor = 6
	MinimumComputeMinor = 0
)

// FailureCode is stable machine-readable setup and Admin state. Human detail
// remains actionable, but callers never have to classify arbitrary stderr.
type FailureCode string

const (
	FailureAdapterMissing     FailureCode = "nvidia_adapter_missing"
	FailureComputeUnsupported FailureCode = "nvidia_compute_unsupported"
	FailureDriverUnsupported  FailureCode = "nvidia_driver_unsupported"
	FailureWSLGPUUnavailable  FailureCode = "wsl_gpu_unavailable"
	FailurePackageInvalid     FailureCode = "nvidia_package_invalid"
	FailureCDIUnavailable     FailureCode = "nvidia_cdi_unavailable"
	FailureCUDAImageInvalid   FailureCode = "cuda_image_invalid"
	FailureCUDADeviceMissing  FailureCode = "cuda_device_unavailable"
	FailureCUDAOffloadMissing FailureCode = "cuda_offload_missing"
)

// Failure carries one typed gate plus sanitized diagnostic detail.
type Failure struct {
	Code   FailureCode `json:"code"`
	Detail string      `json:"detail"`
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", f.Code, f.Detail)
}

// IsCode lets callers and tests classify a failure without parsing its text.
func IsCode(err error, code FailureCode) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Code == code
}

func fail(code FailureCode, format string, args ...any) error {
	return &Failure{Code: code, Detail: fmt.Sprintf(format, args...)}
}
