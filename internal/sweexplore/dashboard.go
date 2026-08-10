package sweexplore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/inference"
)

// DashboardSnapshot is one read-only, source-validated view of campaign
// evidence. Completion counts come only from receipts whose referenced
// artifacts still match their immutable SHA-256 values.
type DashboardSnapshot struct {
	ObservedAt             string                         `json:"observed_at"`
	CampaignID             string                         `json:"campaign_id"`
	RequestedInstances     int                            `json:"requested_instances"`
	PreparedInstances      int                            `json:"prepared_instances"`
	CompletedInstances     int                            `json:"completed_instances"`
	CompletedArms          int                            `json:"completed_arms"`
	TargetArms             int                            `json:"target_arms"`
	CompletedPairs         int                            `json:"completed_pairs"`
	TargetPairs            int                            `json:"target_pairs"`
	FailedAttempts         int                            `json:"failed_attempts"`
	FirstIncompleteOrdinal int                            `json:"first_incomplete_ordinal,omitempty"`
	Instances              []DashboardInstance            `json:"instances"`
	Activity               DashboardActivity              `json:"activity"`
	Preparation            DashboardPreparationEvidence   `json:"preparation"`
	LastFailure            *CampaignFailure               `json:"last_failure,omitempty"`
	Providers              map[Provider]ProviderAggregate `json:"providers"`
}

// DashboardInstance is the compact per-instance progress row displayed below
// the campaign-level summary. Ordinal is one-based to match the manifest.
type DashboardInstance struct {
	Ordinal        int    `json:"ordinal"`
	InstanceID     string `json:"instance_id"`
	Repository     string `json:"repository"`
	Prepared       bool   `json:"prepared"`
	CompletedArms  int    `json:"completed_arms"`
	CompletedPairs int    `json:"completed_pairs"`
	Status         string `json:"status"`
}

// DashboardActivity exposes the newest durable evidence independently of
// process liveness. Index activity uses semantic counters and inference
// telemetry; agent activity uses the growing raw trace size and modification
// time until a receipt is published.
type DashboardActivity struct {
	InstanceID           string                  `json:"instance_id,omitempty"`
	Repository           string                  `json:"repository,omitempty"`
	Stage                string                  `json:"stage,omitempty"`
	ArmID                string                  `json:"arm_id,omitempty"`
	UpdatedAt            string                  `json:"updated_at,omitempty"`
	NotReadyReason       string                  `json:"not_ready_reason,omitempty"`
	ChunksCompleted      int                     `json:"chunks_completed,omitempty"`
	ChunksTotal          int                     `json:"chunks_total,omitempty"`
	CompletionPercent    float64                 `json:"completion_percent,omitempty"`
	ChunksPerSecond      float64                 `json:"chunks_per_second,omitempty"`
	ETASeconds           int64                   `json:"eta_seconds,omitempty"`
	SemanticStalled      bool                    `json:"semantic_stalled"`
	SemanticStallSeconds int64                   `json:"semantic_stall_seconds,omitempty"`
	GPUTelemetry         *inference.GPUTelemetry `json:"gpu_telemetry,omitempty"`
	TraceBytes           int64                   `json:"trace_bytes,omitempty"`
}

// DashboardPreparationEvidence summarizes the most informative successful
// preparation receipt. The longest recorded series is selected so a later
// one-sample warm preflight cannot hide the actual GPU indexing run.
type DashboardPreparationEvidence struct {
	InstanceID             string  `json:"instance_id,omitempty"`
	Samples                int     `json:"samples,omitempty"`
	ProgressSamples        int     `json:"progress_samples,omitempty"`
	CommittedChunks        int     `json:"committed_chunks,omitempty"`
	DurationMS             int64   `json:"duration_ms,omitempty"`
	PeakGPUUtilization     int     `json:"peak_gpu_utilization_percent,omitempty"`
	GPUActiveSamples       int     `json:"gpu_active_samples,omitempty"`
	PeakMemoryUsedMiB      int     `json:"peak_memory_used_mib,omitempty"`
	PeakPowerWatts         float64 `json:"peak_power_watts,omitempty"`
	PeakTemperatureCelsius int     `json:"peak_temperature_celsius,omitempty"`
}

// LoadDashboardSnapshot verifies the immutable launch identity before reading
// campaign evidence. It never creates, repairs, or modifies an artifact.
func LoadDashboardSnapshot(configPath, manifestPath, harnessPath, outputRoot string) (DashboardSnapshot, error) {
	config, err := LoadConfig(configPath)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	manifest, err := readDashboardManifest(manifestPath)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	configDigest, err := FileSHA256(configPath)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("hash dashboard campaign config: %w", err)
	}
	if configDigest != manifest.ConfigSHA256 {
		return DashboardSnapshot{}, fmt.Errorf("dashboard config digest is %s, manifest pins %s", configDigest, manifest.ConfigSHA256)
	}
	harnessDigest, err := FileSHA256(harnessPath)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("hash dashboard campaign harness: %w", err)
	}
	if harnessDigest != manifest.HarnessSHA256 {
		return DashboardSnapshot{}, fmt.Errorf("dashboard harness digest is %s, manifest pins %s", harnessDigest, manifest.HarnessSHA256)
	}
	return BuildDashboardSnapshot(config, manifest, outputRoot)
}

// BuildDashboardSnapshot reconstructs progress and results from primary
// receipts. It is exported so the native UI and focused tests share exactly
// the same metric model.
func BuildDashboardSnapshot(config Config, manifest CampaignManifest, outputRoot string) (DashboardSnapshot, error) {
	if manifest.SchemaVersion != CampaignSchemaVersion || manifest.CampaignID == "" || len(manifest.Instances) != manifest.RequestedCount {
		return DashboardSnapshot{}, errors.New("dashboard manifest identity is incomplete")
	}
	absoluteRoot, err := filepath.Abs(outputRoot)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("resolve dashboard campaign root: %w", err)
	}
	report, err := buildCampaignReport(manifest, config, absoluteRoot)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("rebuild dashboard aggregate: %w", err)
	}
	snapshot := DashboardSnapshot{
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), CampaignID: manifest.CampaignID,
		RequestedInstances: len(manifest.Instances), TargetArms: len(manifest.Instances) * len(config.Arms),
		TargetPairs: len(manifest.Instances) * 2, FailedAttempts: report.FailedAttempts,
		Providers: report.Providers, Instances: make([]DashboardInstance, 0, len(manifest.Instances)),
	}
	for ordinal, selected := range manifest.Instances {
		instance := Instance{InstanceID: selected.InstanceID, Repository: selected.Repository, BaseCommit: selected.BaseCommit}
		row, rowErr := buildDashboardInstance(config, manifest, absoluteRoot, instance, ordinal+1)
		if rowErr != nil {
			return DashboardSnapshot{}, rowErr
		}
		snapshot.Instances = append(snapshot.Instances, row)
		snapshot.CompletedArms += row.CompletedArms
		snapshot.CompletedPairs += row.CompletedPairs
		if row.Prepared {
			snapshot.PreparedInstances++
		}
		if row.Status == "complete" {
			snapshot.CompletedInstances++
		} else if snapshot.FirstIncompleteOrdinal == 0 {
			snapshot.FirstIncompleteOrdinal = row.Ordinal
		}
	}
	activity, activityErr := latestDashboardActivity(absoluteRoot, manifest)
	if activityErr != nil {
		return DashboardSnapshot{}, activityErr
	}
	snapshot.Activity = activity
	snapshot.Preparation, err = dashboardPreparationEvidence(absoluteRoot)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	snapshot.LastFailure, err = latestDashboardFailure(absoluteRoot)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	return snapshot, nil
}

func dashboardPreparationEvidence(root string) (DashboardPreparationEvidence, error) {
	paths, err := filepath.Glob(filepath.Join(root, "instances", "*", "prepare-attempts", "*", "prepare.json"))
	if err != nil {
		return DashboardPreparationEvidence{}, err
	}
	var selected PrepareRecord
	for _, path := range paths {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return DashboardPreparationEvidence{}, readErr
		}
		var record PrepareRecord
		if decodeErr := json.Unmarshal(body, &record); decodeErr != nil {
			return DashboardPreparationEvidence{}, decodeErr
		}
		if record.TelemetrySummary.Samples > selected.TelemetrySummary.Samples {
			selected = record
		}
	}
	if selected.TelemetrySummary.Samples == 0 {
		return DashboardPreparationEvidence{}, nil
	}
	telemetryPath := filepath.Join(root, filepath.FromSlash(selected.Telemetry.Path))
	file, err := os.Open(telemetryPath)
	if err != nil {
		return DashboardPreparationEvidence{}, err
	}
	defer file.Close()
	evidence := DashboardPreparationEvidence{
		InstanceID: selected.InstanceID, Samples: selected.TelemetrySummary.Samples,
		ProgressSamples:    selected.TelemetrySummary.ProgressSamples,
		DurationMS:         selected.BaselineMS + selected.MaterializeMS + selected.FreshnessMS,
		PeakGPUUtilization: selected.TelemetrySummary.PeakGPUUtilization,
		GPUActiveSamples:   selected.TelemetrySummary.GPUActiveSamples,
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var last PreparationSample
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if err := json.Unmarshal(scanner.Bytes(), &last); err != nil {
			return DashboardPreparationEvidence{}, fmt.Errorf("decode preparation evidence %s: %w", telemetryPath, err)
		}
		if telemetry := last.Inference.GPUTelemetry; telemetry != nil {
			evidence.PeakMemoryUsedMiB = max(evidence.PeakMemoryUsedMiB, telemetry.MemoryUsedMiB)
			evidence.PeakPowerWatts = math.Max(evidence.PeakPowerWatts, telemetry.PowerWatts)
			evidence.PeakTemperatureCelsius = max(evidence.PeakTemperatureCelsius, telemetry.TemperatureCelsius)
		}
	}
	if err := scanner.Err(); err != nil {
		return DashboardPreparationEvidence{}, err
	}
	evidence.CommittedChunks = completedSemanticChunks(last.Semantic)
	return evidence, nil
}

func readDashboardManifest(path string) (CampaignManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return CampaignManifest{}, fmt.Errorf("read dashboard campaign manifest: %w", err)
	}
	var manifest CampaignManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return CampaignManifest{}, fmt.Errorf("decode dashboard campaign manifest: %w", err)
	}
	return manifest, nil
}

func buildDashboardInstance(config Config, manifest CampaignManifest, root string, instance Instance, ordinal int) (DashboardInstance, error) {
	row := DashboardInstance{Ordinal: ordinal, InstanceID: instance.InstanceID, Repository: instance.Repository, Status: "pending"}
	prepared, err := validateDashboardPreparation(root, manifest, instance)
	if err != nil {
		return DashboardInstance{}, err
	}
	row.Prepared = prepared
	receipts := map[Provider]map[Mode]ArtifactReference{}
	for _, arm := range config.Arms {
		path := filepath.Join(root, "instances", instance.InstanceID, "receipts", arm.ID+".json")
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return DashboardInstance{}, statErr
		}
		if _, _, loadErr := loadArmReceipt(root, path, manifest, instance, arm); loadErr != nil {
			return DashboardInstance{}, fmt.Errorf("validate dashboard arm %s: %w", arm.ID, loadErr)
		}
		if receipts[arm.Provider] == nil {
			receipts[arm.Provider] = map[Mode]ArtifactReference{}
		}
		receipts[arm.Provider][arm.Mode] = artifactReference(root, path)
		row.CompletedArms++
	}
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		path := filepath.Join(root, "instances", instance.InstanceID, "receipts", string(provider)+".pair.json")
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return DashboardInstance{}, statErr
		}
		if _, loadErr := loadPairReceipt(root, path, manifest, instance.InstanceID, provider, receipts[provider]); loadErr != nil {
			return DashboardInstance{}, fmt.Errorf("validate dashboard pair %s: %w", provider, loadErr)
		}
		row.CompletedPairs++
	}
	switch {
	case row.CompletedArms == len(config.Arms) && row.CompletedPairs == 2:
		row.Status = "complete"
	case row.CompletedArms > 0 || row.CompletedPairs > 0:
		row.Status = "running"
	case row.Prepared:
		row.Status = "prepared"
	}
	return row, nil
}

func validateDashboardPreparation(root string, manifest CampaignManifest, instance Instance) (bool, error) {
	pattern := filepath.Join(root, "instances", instance.InstanceID, "prepare-attempts", "*", "prepare.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, readErr
		}
		var record PrepareRecord
		if decodeErr := json.Unmarshal(body, &record); decodeErr != nil {
			return false, fmt.Errorf("decode dashboard preparation %s: %w", path, decodeErr)
		}
		if record.CampaignID != manifest.CampaignID || record.InstanceID != instance.InstanceID || record.Repository != instance.Repository || !strings.EqualFold(record.BaseCommit, instance.BaseCommit) {
			return false, fmt.Errorf("preparation receipt %s has mismatched identity", path)
		}
		if verifyErr := verifyArtifactReference(root, record.Telemetry); verifyErr != nil {
			return false, fmt.Errorf("validate preparation receipt %s: %w", path, verifyErr)
		}
	}
	return len(paths) > 0, nil
}

func latestDashboardActivity(root string, manifest CampaignManifest) (DashboardActivity, error) {
	telemetryPath, telemetryInfo, err := newestDashboardFile(root, "preparation-telemetry.jsonl")
	if err != nil {
		return DashboardActivity{}, err
	}
	tracePath, traceInfo, err := newestDashboardFile(root, ".trace.jsonl")
	if err != nil {
		return DashboardActivity{}, err
	}
	if telemetryInfo != nil && (traceInfo == nil || !traceInfo.ModTime().After(telemetryInfo.ModTime())) {
		sample, readErr := readLastPreparationSample(telemetryPath)
		if readErr != nil {
			return DashboardActivity{}, readErr
		}
		instanceID := dashboardInstanceFromPath(root, telemetryPath)
		return DashboardActivity{
			InstanceID: instanceID, Repository: dashboardRepository(manifest, instanceID), Stage: "indexing/" + sample.Phase,
			UpdatedAt: sample.ObservedAt, NotReadyReason: sample.NotReadyReason,
			ChunksCompleted: completedSemanticChunks(sample.Semantic), ChunksTotal: sample.Semantic.ChunksTotal,
			CompletionPercent: sample.CompletionPercent, ChunksPerSecond: sample.ChunksPerSecond, ETASeconds: sample.ETASeconds,
			SemanticStalled: sample.Semantic.Stalled, SemanticStallSeconds: sample.Semantic.StallSeconds,
			GPUTelemetry: sample.Inference.GPUTelemetry,
		}, nil
	}
	if traceInfo != nil {
		instanceID := dashboardInstanceFromPath(root, tracePath)
		return DashboardActivity{InstanceID: instanceID, Repository: dashboardRepository(manifest, instanceID), Stage: "agent", ArmID: dashboardArmFromTracePath(tracePath), UpdatedAt: traceInfo.ModTime().UTC().Format(time.RFC3339Nano), TraceBytes: traceInfo.Size()}, nil
	}
	return DashboardActivity{}, nil
}

func newestDashboardFile(root, suffix string) (string, os.FileInfo, error) {
	var selectedPath string
	var selectedInfo os.FileInfo
	err := filepath.WalkDir(filepath.Join(root, "instances"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if selectedInfo == nil || info.ModTime().After(selectedInfo.ModTime()) {
			selectedPath, selectedInfo = path, info
		}
		return nil
	})
	if os.IsNotExist(err) {
		return "", nil, nil
	}
	return selectedPath, selectedInfo, err
}

func readLastPreparationSample(path string) (PreparationSample, error) {
	file, err := os.Open(path)
	if err != nil {
		return PreparationSample{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 2*1024*1024)
	var last string
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			last = scanner.Text()
		}
	}
	if err := scanner.Err(); err != nil {
		return PreparationSample{}, err
	}
	if last == "" {
		return PreparationSample{}, fmt.Errorf("preparation telemetry is empty: %s", path)
	}
	var sample PreparationSample
	if err := json.Unmarshal([]byte(last), &sample); err != nil {
		return PreparationSample{}, fmt.Errorf("decode last preparation sample %s: %w", path, err)
	}
	return sample, nil
}

func latestDashboardFailure(root string) (*CampaignFailure, error) {
	paths, err := filepath.Glob(filepath.Join(root, "instances", "*", "failures", "*.json"))
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	sort.Slice(paths, func(left, right int) bool {
		leftInfo, leftErr := os.Stat(paths[left])
		rightInfo, rightErr := os.Stat(paths[right])
		return leftErr == nil && (rightErr != nil || leftInfo.ModTime().After(rightInfo.ModTime()))
	})
	body, err := os.ReadFile(paths[0])
	if err != nil {
		return nil, err
	}
	var failure CampaignFailure
	if err := json.Unmarshal(body, &failure); err != nil {
		return nil, err
	}
	return &failure, nil
}

func dashboardInstanceFromPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	if len(parts) >= 2 && parts[0] == "instances" {
		return parts[1]
	}
	return ""
}

func dashboardArmFromTracePath(path string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] == "attempts" {
			return parts[index+1]
		}
	}
	return ""
}

func dashboardRepository(manifest CampaignManifest, instanceID string) string {
	for _, instance := range manifest.Instances {
		if instance.InstanceID == instanceID {
			return instance.Repository
		}
	}
	return ""
}
