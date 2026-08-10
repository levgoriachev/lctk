package sweexplore

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestScoreMatchesSyntheticExpectedValues(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"core.go", "optional.go", "noise.go"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("1\n2\n3\n4\n5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	truth := GroundTruth{
		ReadCoreFiles:          []string{"core.go"},
		ReadCoreRegions:        []Region{{Path: "core.go", Start: 2, End: 4}},
		ReadOptionalFilesMap:   map[string][]string{"model": {"optional.go"}},
		ReadOptionalRegionsMap: map[string][]Region{"model": {{Path: "optional.go", Start: 1, End: 2}}},
		MainFiles:              []string{"core.go"},
	}
	predictions := []Region{{Path: "core.go", Start: 2, End: 3}, {Path: "optional.go", Start: 1, End: 1}, {Path: "noise.go", Start: 1, End: 1}}
	metrics, err := Score(root, predictions, truth)
	if err != nil {
		t.Fatal(err)
	}
	assertNear(t, metrics.Precision, 0.5)
	assertNear(t, metrics.Recall, 2.0/3.0)
	assertNear(t, metrics.F1Score, 4.0/7.0)
	assertNear(t, metrics.HitFileRate, 1)
	assertNear(t, metrics.NoiseFileRate, 1.0/3.0)
	assertNear(t, metrics.HitRegionRate, 1)
	assertNear(t, metrics.NoiseRegionRate, 1.0/3.0)
	assertNear(t, metrics.WeightedCoreCoverage, 2.0/3.0)
	assertNear(t, metrics.ContextEfficiency, 0.75)
	assertNear(t, metrics.OptionalCoverage, 0.5)
	assertNear(t, metrics.RecallAt100, 2.0/3.0)
	assertNear(t, metrics.FirstUsefulHit, 1)
}

func TestScoreCountsPositiveLinesBeyondEOFAsOfficialNoise(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "core.go"), []byte("1\n2\n3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	truth := GroundTruth{ReadCoreFiles: []string{"core.go"}, ReadCoreRegions: []Region{{Path: "core.go", Start: 2, End: 3}}, MainFiles: []string{"core.go"}}
	metrics, err := Score(root, []Region{{Path: "core.go", Start: 2, End: 5}}, truth)
	if err != nil {
		t.Fatal(err)
	}
	assertNear(t, metrics.Precision, 0.5)
	assertNear(t, metrics.Recall, 1)
	assertNear(t, metrics.ContextEfficiency, 0.5)
}

func assertNear(t *testing.T, actual, expected float64) {
	t.Helper()
	if math.Abs(actual-expected) > 1e-12 {
		t.Fatalf("got %.16f, want %.16f", actual, expected)
	}
}
