package sweexplore

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// LoadInstance verifies both immutable inputs and performs an exact one-to-one
// join. It never exposes ground truth to the agent prompt.
func LoadInstance(config BenchmarkConfig, instanceID string) (Instance, error) {
	if strings.TrimSpace(instanceID) == "" {
		return Instance{}, fmt.Errorf("instance id is required")
	}
	if err := VerifySHA256(config.ExploreDatasetPath, config.ExploreDatasetSHA256); err != nil {
		return Instance{}, fmt.Errorf("verify SWE-Explore dataset: %w", err)
	}
	if err := VerifySHA256(config.IssueDatasetPath, config.IssueDatasetSHA256); err != nil {
		return Instance{}, fmt.Errorf("verify issue dataset: %w", err)
	}
	explore, err := findUniqueJSONL[ExploreRecord](config.ExploreDatasetPath, instanceID, func(record ExploreRecord) string {
		return record.InstanceID
	})
	if err != nil {
		return Instance{}, fmt.Errorf("load SWE-Explore record: %w", err)
	}
	issue, err := findUniqueJSONL[IssueRecord](config.IssueDatasetPath, instanceID, func(record IssueRecord) string {
		return record.InstanceID
	})
	if err != nil {
		return Instance{}, fmt.Errorf("load issue record: %w", err)
	}
	if strings.TrimSpace(issue.Repository) == "" || strings.TrimSpace(issue.BaseCommit) == "" || strings.TrimSpace(issue.ProblemStatement) == "" {
		return Instance{}, fmt.Errorf("issue record %q is incomplete", instanceID)
	}
	return Instance{
		InstanceID: explore.InstanceID, Repository: issue.Repository,
		BaseCommit: issue.BaseCommit, ProblemStatement: issue.ProblemStatement,
		GroundTruth: explore.GroundTruth,
	}, nil
}

// VerifySHA256 requires a lowercase or uppercase 64-character digest. Empty
// digests are rejected because an unpinned campaign cannot be reproduced.
func VerifySHA256(path, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("expected SHA-256 for %s must contain 64 hexadecimal characters", path)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("expected SHA-256 for %s is invalid: %w", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", path, actual, expected)
	}
	return nil
}

func findUniqueJSONL[T any](path, instanceID string, identify func(T) string) (T, error) {
	var zero T
	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	// SWE-bench problem statements and patches can exceed Scanner's small
	// default token. The bounded 32 MiB record cap remains well above the pinned
	// public rows while failing on a corrupt one-line file.
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	found := false
	var result T
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var record T
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return zero, fmt.Errorf("decode %s line %d: %w", path, line, err)
		}
		if identify(record) != instanceID {
			continue
		}
		if found {
			return zero, fmt.Errorf("instance %q occurs more than once in %s", instanceID, path)
		}
		result, found = record, true
	}
	if err := scanner.Err(); err != nil {
		return zero, fmt.Errorf("scan %s: %w", path, err)
	}
	if !found {
		return zero, fmt.Errorf("instance %q is absent from %s", instanceID, path)
	}
	return result, nil
}
