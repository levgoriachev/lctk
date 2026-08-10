package sweexplore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInstanceJoinsPinnedInputsAndRejectsDuplicates(t *testing.T) {
	root := t.TempDir()
	explorePath := filepath.Join(root, "explore.jsonl")
	issuePath := filepath.Join(root, "issues.jsonl")
	explore := `{"instance_id":"org__repo-1","ground_truth":{"read_core_files":["main.go"],"read_core_regions":[{"path":"main.go","start":1,"end":2}]}}` + "\n"
	issue := `{"instance_id":"org__repo-1","repo":"org/repo","base_commit":"0123456789012345678901234567890123456789","problem_statement":"bug"}` + "\n"
	writeTestFile(t, explorePath, explore)
	writeTestFile(t, issuePath, issue)
	config := BenchmarkConfig{
		ExploreDatasetPath: explorePath, ExploreDatasetSHA256: digest(explore),
		IssueDatasetPath: issuePath, IssueDatasetSHA256: digest(issue),
	}
	instance, err := LoadInstance(config, "org__repo-1")
	if err != nil {
		t.Fatal(err)
	}
	if instance.BaseCommit == "" || instance.GroundTruth.ReadCoreFiles[0] != "main.go" {
		t.Fatalf("instance = %+v", instance)
	}
	writeTestFile(t, issuePath, issue+issue)
	config.IssueDatasetSHA256 = digest(issue + issue)
	if _, err := LoadInstance(config, "org__repo-1"); err == nil {
		t.Fatal("duplicate issue record was accepted")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func digest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
