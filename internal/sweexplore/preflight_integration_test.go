package sweexplore

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestInstalledLCTKPreflight is opt-in because it requires the real private
// runtime. Acceptance enables it with explicit paths and identities; ordinary
// unit tests remain hermetic.
func TestInstalledLCTKPreflight(t *testing.T) {
	if os.Getenv("LCTK_BENCHMARK_INTEGRATION") != "1" {
		t.Skip("installed LCTK integration is not enabled")
	}
	workspace := WorkspaceConfig{
		Root: os.Getenv("LCTK_BENCHMARK_ROOT"), ProjectID: os.Getenv("LCTK_BENCHMARK_PROJECT"),
		LCTKExecutable: os.Getenv("LCTK_BENCHMARK_EXECUTABLE"), ExpectedLCTKVersion: os.Getenv("LCTK_BENCHMARK_VERSION"),
		FreshnessTimeoutSeconds: 30,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	proof, err := WaitForLCTK(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if proof.ExactGeneration == 0 || proof.ExactGeneration != proof.SemanticGeneration || proof.ExactGeneration != proof.GraphGeneration || proof.ExactGeneration != proof.WatcherGeneration {
		t.Fatalf("freshness proof = %+v", proof)
	}
}

func TestFreshnessAdvanceRequiresStrictlyNewerGeneration(t *testing.T) {
	workspace := WorkspaceConfig{FreshnessTimeoutSeconds: 1}
	if _, err := WaitForLCTKAfterGeneration(context.Background(), workspace, 0); err == nil {
		t.Fatal("zero baseline generation was accepted")
	}
}

func TestFreshnessAdvanceRejectsOldAndEqualGenerations(t *testing.T) {
	tests := []struct {
		name       string
		generation uint64
		eligible   bool
	}{
		{name: "older", generation: 6, eligible: false},
		{name: "equal", generation: 7, eligible: false},
		{name: "newer", generation: 8, eligible: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proof := FreshnessProof{ExactGeneration: test.generation}
			if actual := generationIsEligible(proof, 7, true); actual != test.eligible {
				t.Fatalf("generation %d eligibility = %t, want %t", test.generation, actual, test.eligible)
			}
		})
	}
	if !generationIsEligible(FreshnessProof{}, 7, false) {
		t.Fatal("ordinary preflight unexpectedly required a generation advance")
	}
}
