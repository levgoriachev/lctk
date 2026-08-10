package nvidiainstall

import (
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

// GPU is the exact physical adapter evidence reported by Windows before setup
// mutates the private runtime. LCTK 0.1.12 intentionally supports one adapter;
// ambiguous multi-GPU output is rejected rather than represented incorrectly.
type GPU struct {
	Name              string `json:"name"`
	DriverVersion     string `json:"driver_version"`
	VRAMMiB           int    `json:"vram_mib"`
	ComputeCapability string `json:"compute_capability"`
	ComputeMajor      int    `json:"-"`
	ComputeMinor      int    `json:"-"`
}

// ParseHostProbe validates the strict CSV requested from the trusted Windows
// nvidia-smi executable and applies the pinned CUDA and WSL compatibility floor.
func ParseHostProbe(output string) (GPU, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return GPU{}, fail(FailureAdapterMissing, "NVIDIA adapter output is malformed: %v", err)
	}
	if len(records) == 0 || (len(records) == 1 && len(records[0]) == 1 && strings.TrimSpace(records[0][0]) == "") {
		return GPU{}, fail(FailureAdapterMissing, "Windows did not report an NVIDIA adapter; select CPU or install a supported NVIDIA adapter")
	}
	if len(records) != 1 {
		return GPU{}, fail(FailureAdapterMissing, "LCTK 0.1.12 requires exactly one NVIDIA adapter, but Windows reported %d", len(records))
	}
	record := records[0]
	if len(record) != 4 {
		return GPU{}, fail(FailureAdapterMissing, "NVIDIA adapter output has %d fields; expected 4", len(record))
	}
	for index := range record {
		record[index] = strings.TrimSpace(record[index])
		if record[index] == "" {
			return GPU{}, fail(FailureAdapterMissing, "NVIDIA adapter output contains an empty field")
		}
	}
	vram, err := strconv.Atoi(record[2])
	if err != nil || vram <= 0 {
		return GPU{}, fail(FailureAdapterMissing, "NVIDIA adapter VRAM %q is invalid", record[2])
	}
	driverMajor, driverMinor, err := parseVersion(record[1])
	if err != nil {
		return GPU{}, fail(FailureDriverUnsupported, "NVIDIA driver version %q is invalid", record[1])
	}
	if lessVersion(driverMajor, driverMinor, MinimumDriverMajor, MinimumDriverMinor) {
		return GPU{}, fail(FailureDriverUnsupported,
			"NVIDIA driver %s is older than CUDA 12.8 minimum %d.%d; update the Windows NVIDIA driver and retry",
			record[1], MinimumDriverMajor, MinimumDriverMinor)
	}
	computeMajor, computeMinor, err := parseVersion(record[3])
	if err != nil {
		return GPU{}, fail(FailureComputeUnsupported, "NVIDIA compute capability %q is invalid", record[3])
	}
	if lessVersion(computeMajor, computeMinor, MinimumComputeMajor, MinimumComputeMinor) {
		return GPU{}, fail(FailureComputeUnsupported,
			"NVIDIA compute capability %s predates WSL-supported Pascal %d.%d; select CPU or use a supported adapter",
			record[3], MinimumComputeMajor, MinimumComputeMinor)
	}
	return GPU{
		Name: record[0], DriverVersion: record[1], VRAMMiB: vram,
		ComputeCapability: record[3], ComputeMajor: computeMajor, ComputeMinor: computeMinor,
	}, nil
}

func parseVersion(value string) (int, int, error) {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected major.minor")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("invalid major")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("invalid minor")
	}
	for _, part := range parts[2:] {
		if _, err := strconv.Atoi(part); err != nil {
			return 0, 0, fmt.Errorf("invalid suffix")
		}
	}
	return major, minor, nil
}

func lessVersion(gotMajor, gotMinor, wantMajor, wantMinor int) bool {
	return gotMajor < wantMajor || (gotMajor == wantMajor && gotMinor < wantMinor)
}

func firstLine(stderr string, err error) string {
	if line, _, found := strings.Cut(strings.TrimSpace(stderr), "\n"); found && strings.TrimSpace(line) != "" {
		return strings.TrimSpace(line)
	}
	if line := strings.TrimSpace(stderr); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}
