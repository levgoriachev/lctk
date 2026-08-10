package nvidiainstall

import "testing"

func TestParseHostProbeAcceptsGTX1070(t *testing.T) {
	gpu, err := ParseHostProbe("NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1\n")
	if err != nil {
		t.Fatal(err)
	}
	if gpu.Name != "NVIDIA GeForce GTX 1070" || gpu.DriverVersion != "582.53" ||
		gpu.VRAMMiB != 8192 || gpu.ComputeMajor != 6 || gpu.ComputeMinor != 1 {
		t.Fatalf("unexpected GPU: %+v", gpu)
	}
}

func TestParseHostProbeRejectsEveryCompatibilityAmbiguity(t *testing.T) {
	for name, test := range map[string]struct {
		output string
		code   FailureCode
	}{
		"missing":        {"", FailureAdapterMissing},
		"multiple":       {"GPU A, 582.53, 8192, 6.1\nGPU B, 582.53, 8192, 8.6", FailureAdapterMissing},
		"old driver":     {"GPU, 572.60, 8192, 6.1", FailureDriverUnsupported},
		"bad driver":     {"GPU, current, 8192, 6.1", FailureDriverUnsupported},
		"pre-pascal":     {"GPU, 582.53, 8192, 5.2", FailureComputeUnsupported},
		"bad capability": {"GPU, 582.53, 8192, unknown", FailureComputeUnsupported},
		"bad memory":     {"GPU, 582.53, none, 6.1", FailureAdapterMissing},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHostProbe(test.output)
			if !IsCode(err, test.code) {
				t.Fatalf("error=%v want code %s", err, test.code)
			}
		})
	}
}

func TestParseHostProbeAcceptsMinimumVersions(t *testing.T) {
	if _, err := ParseHostProbe("GPU, 572.61, 1, 6.0"); err != nil {
		t.Fatal(err)
	}
}
