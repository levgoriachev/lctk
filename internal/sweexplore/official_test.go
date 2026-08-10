package sweexplore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOfficialScorerMatchesLocalParityScorer(t *testing.T) {
	officialRoot, err := filepath.Abs(filepath.Join("..", "..", ".artifacts", "swe-explore-research"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(officialRoot, "eval.py")); err != nil {
		t.Skip("pinned upstream evaluator is not present")
	}
	commitCommand := exec.Command("git", "rev-parse", "HEAD")
	commitCommand.Dir = officialRoot
	commitBody, err := commitCommand.Output()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("1\n2\n3\n4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	datasetPath := filepath.Join(root, "bench.jsonl")
	dataset := `{"instance_id":"fixture__repo-1","ground_truth":{"read_core_files":["main.py"],"read_core_regions":[{"path":"main.py","start":2,"end":4}],"read_optional_files_map":{},"read_optional_regions_map":{},"main_files":["main.py"]}}` + "\n"
	writeTestFile(t, datasetPath, dataset)
	resultPath := filepath.Join(root, "result.json")
	result := Result{SchemaVersion: SchemaVersion, InstanceID: "fixture__repo-1", ArmID: "fixture", Regions: []Region{{Path: "main.py", Start: 2, End: 3}}}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{
		Benchmark: BenchmarkConfig{ExploreDatasetPath: datasetPath, OfficialRoot: officialRoot, OfficialCommit: string(bytes.TrimSpace(commitBody))},
		Workspace: WorkspaceConfig{Root: root},
	}
	officialBody, err := OfficialScore(context.Background(), "python", config, resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var official struct {
		Metrics Metrics `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(officialBody), &official); err != nil {
		t.Fatal(err)
	}
	local, err := Score(root, result.Regions, GroundTruth{
		ReadCoreFiles: []string{"main.py"}, ReadCoreRegions: []Region{{Path: "main.py", Start: 2, End: 4}}, MainFiles: []string{"main.py"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if official.Metrics != local {
		t.Fatalf("official = %+v\nlocal = %+v", official.Metrics, local)
	}
}
