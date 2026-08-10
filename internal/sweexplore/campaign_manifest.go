package sweexplore

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const CampaignSchemaVersion = 1

var safeArtifactID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var fullGitCommit = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// CampaignManifest is the immutable sampling and executable identity contract
// for one paid campaign. It deliberately contains no benchmark ground truth.
type CampaignManifest struct {
	SchemaVersion   int                `json:"schema_version"`
	CampaignID      string             `json:"campaign_id"`
	Seed            string             `json:"seed"`
	SelectionMethod string             `json:"selection_method"`
	RequestedCount  int                `json:"requested_count"`
	CreatedAt       string             `json:"created_at"`
	ConfigSHA256    string             `json:"config_sha256"`
	HarnessSHA256   string             `json:"harness_sha256"`
	HarnessCommit   string             `json:"harness_commit"`
	ExploreSHA256   string             `json:"explore_dataset_sha256"`
	IssueSHA256     string             `json:"issue_dataset_sha256"`
	Clients         []CampaignClient   `json:"clients"`
	Instances       []CampaignInstance `json:"instances"`
}

// CampaignClient pins the exact executable bytes and semantic configuration
// used by one provider before any paid turn starts.
type CampaignClient struct {
	Provider         Provider `json:"provider"`
	Executable       string   `json:"executable"`
	ExecutableSHA256 string   `json:"executable_sha256"`
	Version          string   `json:"version"`
	Model            string   `json:"model"`
	Effort           string   `json:"effort,omitempty"`
}

// CampaignInstance records only public checkout identity. The issue prompt is
// loaded from the pinned source at execution time and ground truth stays out of
// every agent-facing artifact.
type CampaignInstance struct {
	InstanceID string `json:"instance_id"`
	Repository string `json:"repository"`
	BaseCommit string `json:"base_commit"`
}

// BuildCampaignManifest selects a deterministic repository-stratified sample
// and records all executable identities needed to reject configuration drift.
func BuildCampaignManifest(ctx context.Context, configPath string, config Config, campaignID string, count int, seed, harnessPath, harnessCommit string) (CampaignManifest, error) {
	if !safeArtifactID.MatchString(campaignID) {
		return CampaignManifest{}, errors.New("campaign id must be a safe filesystem identifier")
	}
	if count <= 0 {
		return CampaignManifest{}, errors.New("campaign count must be positive")
	}
	if strings.TrimSpace(seed) == "" {
		return CampaignManifest{}, errors.New("campaign seed is required")
	}
	if !fullGitCommit.MatchString(harnessCommit) {
		return CampaignManifest{}, errors.New("harness commit must be a full 40-character Git commit")
	}
	configDigest, err := FileSHA256(configPath)
	if err != nil {
		return CampaignManifest{}, fmt.Errorf("hash campaign config: %w", err)
	}
	harnessDigest, err := FileSHA256(harnessPath)
	if err != nil {
		return CampaignManifest{}, fmt.Errorf("hash campaign harness: %w", err)
	}
	candidates, err := loadCampaignCandidates(config.Benchmark)
	if err != nil {
		return CampaignManifest{}, err
	}
	selected, err := selectCampaignInstances(candidates, count, seed)
	if err != nil {
		return CampaignManifest{}, err
	}
	clients := make([]CampaignClient, 0, 2)
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		native, _, pairErr := config.Pair(provider)
		if pairErr != nil {
			return CampaignManifest{}, pairErr
		}
		executable, lookErr := exec.LookPath(native.Executable)
		if lookErr != nil {
			return CampaignManifest{}, fmt.Errorf("resolve %s executable: %w", provider, lookErr)
		}
		digest, hashErr := FileSHA256(executable)
		if hashErr != nil {
			return CampaignManifest{}, fmt.Errorf("hash %s executable: %w", provider, hashErr)
		}
		version, versionErr := readClientVersion(ctx, config.Workspace.Root, native)
		if versionErr != nil {
			return CampaignManifest{}, versionErr
		}
		clients = append(clients, CampaignClient{Provider: provider, Executable: executable, ExecutableSHA256: digest, Version: version, Model: native.Model, Effort: native.Effort})
	}
	return CampaignManifest{
		SchemaVersion: CampaignSchemaVersion, CampaignID: campaignID, Seed: seed,
		SelectionMethod: "repository-floor-then-global-sha256-v1", RequestedCount: count,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfigSHA256: configDigest,
		HarnessSHA256: harnessDigest, HarnessCommit: strings.ToLower(harnessCommit),
		ExploreSHA256: strings.ToLower(config.Benchmark.ExploreDatasetSHA256), IssueSHA256: strings.ToLower(config.Benchmark.IssueDatasetSHA256),
		Clients: clients, Instances: selected,
	}, nil
}

// LoadCampaignManifest validates immutable inputs and every joined instance
// before a campaign can spend model budget.
func LoadCampaignManifest(ctx context.Context, path, configPath, harnessPath string, config Config) (CampaignManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return CampaignManifest{}, fmt.Errorf("read campaign manifest: %w", err)
	}
	var manifest CampaignManifest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return CampaignManifest{}, fmt.Errorf("decode campaign manifest: %w", err)
	}
	if manifest.SchemaVersion != CampaignSchemaVersion || !safeArtifactID.MatchString(manifest.CampaignID) || manifest.RequestedCount <= 0 || manifest.RequestedCount != len(manifest.Instances) || manifest.SelectionMethod != "repository-floor-then-global-sha256-v1" || strings.TrimSpace(manifest.Seed) == "" || !fullGitCommit.MatchString(manifest.HarnessCommit) {
		return CampaignManifest{}, errors.New("campaign manifest header or instance count is invalid")
	}
	configDigest, err := FileSHA256(configPath)
	if err != nil || configDigest != manifest.ConfigSHA256 {
		return CampaignManifest{}, fmt.Errorf("campaign config digest differs from manifest: got %q, want %q", configDigest, manifest.ConfigSHA256)
	}
	harnessDigest, err := FileSHA256(harnessPath)
	if err != nil || harnessDigest != manifest.HarnessSHA256 {
		return CampaignManifest{}, fmt.Errorf("campaign harness digest differs from manifest: got %q, want %q", harnessDigest, manifest.HarnessSHA256)
	}
	if !strings.EqualFold(config.Benchmark.ExploreDatasetSHA256, manifest.ExploreSHA256) || !strings.EqualFold(config.Benchmark.IssueDatasetSHA256, manifest.IssueSHA256) {
		return CampaignManifest{}, errors.New("campaign dataset digests differ from manifest")
	}
	officialCommit, err := commandOutput(ctx, config.Benchmark.OfficialRoot, "git", "rev-parse", "HEAD")
	if err != nil {
		return CampaignManifest{}, fmt.Errorf("inspect official evaluator commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(officialCommit), config.Benchmark.OfficialCommit) {
		return CampaignManifest{}, fmt.Errorf("official evaluator is at %s, want %s", strings.TrimSpace(officialCommit), config.Benchmark.OfficialCommit)
	}
	if len(manifest.Clients) != 2 {
		return CampaignManifest{}, errors.New("campaign manifest must pin exactly two clients")
	}
	clientPins := make(map[Provider]CampaignClient, len(manifest.Clients))
	for _, client := range manifest.Clients {
		if _, duplicate := clientPins[client.Provider]; duplicate {
			return CampaignManifest{}, fmt.Errorf("duplicate client pin for provider %q", client.Provider)
		}
		clientPins[client.Provider] = client
	}
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		native, _, pairErr := config.Pair(provider)
		if pairErr != nil {
			return CampaignManifest{}, pairErr
		}
		pin, ok := clientPins[provider]
		if !ok || pin.Model != native.Model || pin.Effort != native.Effort {
			return CampaignManifest{}, fmt.Errorf("provider %q model or effort differs from manifest", provider)
		}
		executable, lookErr := exec.LookPath(native.Executable)
		if lookErr != nil {
			return CampaignManifest{}, lookErr
		}
		digest, hashErr := FileSHA256(executable)
		if hashErr != nil || digest != pin.ExecutableSHA256 {
			return CampaignManifest{}, fmt.Errorf("provider %q executable digest differs from manifest", provider)
		}
		version, versionErr := readClientVersion(ctx, config.Workspace.Root, native)
		if versionErr != nil || version != pin.Version {
			return CampaignManifest{}, fmt.Errorf("provider %q client version differs from manifest: got %q, want %q", provider, version, pin.Version)
		}
	}
	candidates, err := loadCampaignCandidates(config.Benchmark)
	if err != nil {
		return CampaignManifest{}, err
	}
	byID := make(map[string]CampaignInstance, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.InstanceID] = candidate
	}
	seen := make(map[string]struct{}, len(manifest.Instances))
	for _, instance := range manifest.Instances {
		if !safeArtifactID.MatchString(instance.InstanceID) {
			return CampaignManifest{}, fmt.Errorf("unsafe campaign instance id %q", instance.InstanceID)
		}
		if _, duplicate := seen[instance.InstanceID]; duplicate {
			return CampaignManifest{}, fmt.Errorf("duplicate campaign instance %q", instance.InstanceID)
		}
		seen[instance.InstanceID] = struct{}{}
		if candidate, ok := byID[instance.InstanceID]; !ok || candidate != instance {
			return CampaignManifest{}, fmt.Errorf("campaign instance %q differs from pinned datasets", instance.InstanceID)
		}
	}
	expectedSelection, err := selectCampaignInstances(candidates, manifest.RequestedCount, manifest.Seed)
	if err != nil {
		return CampaignManifest{}, err
	}
	if !equalCampaignInstanceSlices(expectedSelection, manifest.Instances) {
		return CampaignManifest{}, errors.New("campaign instance selection differs from the declared seed and method")
	}
	return manifest, nil
}

func loadCampaignCandidates(config BenchmarkConfig) ([]CampaignInstance, error) {
	if err := VerifySHA256(config.ExploreDatasetPath, config.ExploreDatasetSHA256); err != nil {
		return nil, fmt.Errorf("verify SWE-Explore dataset: %w", err)
	}
	if err := VerifySHA256(config.IssueDatasetPath, config.IssueDatasetSHA256); err != nil {
		return nil, fmt.Errorf("verify issue dataset: %w", err)
	}
	explore, err := readCampaignJSONL(config.ExploreDatasetPath, func(record ExploreRecord) string { return record.InstanceID })
	if err != nil {
		return nil, err
	}
	issues, err := readCampaignJSONL(config.IssueDatasetPath, func(record IssueRecord) string { return record.InstanceID })
	if err != nil {
		return nil, err
	}
	candidates := make([]CampaignInstance, 0, len(explore))
	for id := range explore {
		issue, ok := issues[id]
		if !ok {
			continue
		}
		if strings.TrimSpace(issue.Repository) == "" || strings.TrimSpace(issue.BaseCommit) == "" || strings.TrimSpace(issue.ProblemStatement) == "" {
			return nil, fmt.Errorf("issue record %q is incomplete", id)
		}
		candidates = append(candidates, CampaignInstance{InstanceID: id, Repository: issue.Repository, BaseCommit: issue.BaseCommit})
	}
	if len(candidates) == 0 {
		return nil, errors.New("pinned datasets have no joined campaign instances")
	}
	return candidates, nil
}

func readCampaignJSONL[T any](path string, identify func(T) string) (map[string]T, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records := map[string]T{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var record T
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode %s line %d: %w", path, line, err)
		}
		id := identify(record)
		if id == "" {
			return nil, fmt.Errorf("%s line %d has no instance id", path, line)
		}
		if _, duplicate := records[id]; duplicate {
			return nil, fmt.Errorf("instance %q occurs more than once in %s", id, path)
		}
		records[id] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return records, nil
}

func selectCampaignInstances(candidates []CampaignInstance, count int, seed string) ([]CampaignInstance, error) {
	if count > len(candidates) {
		return nil, fmt.Errorf("campaign requests %d instances but only %d are joined", count, len(candidates))
	}
	groups := map[string][]CampaignInstance{}
	for _, candidate := range candidates {
		groups[candidate.Repository] = append(groups[candidate.Repository], candidate)
	}
	if count < len(groups) {
		return nil, fmt.Errorf("campaign count %d cannot cover all %d repositories", count, len(groups))
	}
	repositories := make([]string, 0, len(groups))
	for repository := range groups {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	selected := make([]CampaignInstance, 0, count)
	selectedIDs := map[string]struct{}{}
	for _, repository := range repositories {
		group := groups[repository]
		sort.Slice(group, func(left, right int) bool { return campaignInstanceLess(seed, group[left], group[right]) })
		selected = append(selected, group[0])
		selectedIDs[group[0].InstanceID] = struct{}{}
	}
	remaining := make([]CampaignInstance, 0, len(candidates)-len(selected))
	for _, candidate := range candidates {
		if _, already := selectedIDs[candidate.InstanceID]; !already {
			remaining = append(remaining, candidate)
		}
	}
	sort.Slice(remaining, func(left, right int) bool { return campaignInstanceLess(seed, remaining[left], remaining[right]) })
	selected = append(selected, remaining[:count-len(selected)]...)
	sort.Slice(selected, func(left, right int) bool {
		if selected[left].Repository != selected[right].Repository {
			return selected[left].Repository < selected[right].Repository
		}
		return campaignInstanceLess(seed, selected[left], selected[right])
	})
	return selected, nil
}

func campaignRank(seed, instanceID string) string {
	digest := sha256.Sum256([]byte(seed + "\x00" + instanceID))
	return hex.EncodeToString(digest[:])
}

func campaignInstanceLess(seed string, left, right CampaignInstance) bool {
	leftRank, rightRank := campaignRank(seed, left.InstanceID), campaignRank(seed, right.InstanceID)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	return left.InstanceID < right.InstanceID
}

func equalCampaignInstanceSlices(left, right []CampaignInstance) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
