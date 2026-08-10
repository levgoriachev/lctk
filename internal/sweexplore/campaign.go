package sweexplore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// OfficialScoreRecord is the complete normalized output of the commit-pinned
// upstream evaluator. It is stored separately from the agent result so scoring
// can never silently mutate the original prediction.
type OfficialScoreRecord struct {
	InstanceID string   `json:"instance_id"`
	Explorer   string   `json:"explorer"`
	Regions    []Region `json:"regions"`
	Metrics    Metrics  `json:"metrics"`
	NumRegions int      `json:"num_regions"`
}

// ArtifactReference binds an immutable campaign artifact to its SHA-256.
type ArtifactReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ArmReceipt is published only after the raw trace, normalized result,
// official score, and local parity score all agree.
type ArmReceipt struct {
	SchemaVersion int                      `json:"schema_version"`
	CampaignID    string                   `json:"campaign_id"`
	ConfigSHA256  string                   `json:"config_sha256"`
	InstanceID    string                   `json:"instance_id"`
	ArmID         string                   `json:"arm_id"`
	Provider      Provider                 `json:"provider"`
	Mode          Mode                     `json:"mode"`
	CompletedAt   string                   `json:"completed_at"`
	RawTrace      ArtifactReference        `json:"raw_trace"`
	Result        ArtifactReference        `json:"result"`
	OfficialScore ArtifactReference        `json:"official_score"`
	ImportedFrom  *ReceiptImportProvenance `json:"imported_from,omitempty"`
}

// ReceiptImportProvenance makes native-control reuse an explicit re-attested
// operation instead of silently treating evidence from another product
// identity as if it had been produced by the current campaign.
type ReceiptImportProvenance struct {
	SourceCampaignID    string            `json:"source_campaign_id"`
	SourceConfigSHA256  string            `json:"source_config_sha256"`
	SourceRoot          string            `json:"source_root"`
	SourceReceipt       ArtifactReference `json:"source_receipt"`
	SourceRawTrace      ArtifactReference `json:"source_raw_trace"`
	SourceResult        ArtifactReference `json:"source_result"`
	SourceOfficialScore ArtifactReference `json:"source_official_score"`
	ImportedAt          string            `json:"imported_at"`
}

// PairReceipt proves that both arms use the same client and actual model. It
// references arm receipts instead of duplicating mutable aggregate data.
type PairReceipt struct {
	SchemaVersion    int               `json:"schema_version"`
	CampaignID       string            `json:"campaign_id"`
	InstanceID       string            `json:"instance_id"`
	Provider         Provider          `json:"provider"`
	CompletedAt      string            `json:"completed_at"`
	NativeReceipt    ArtifactReference `json:"native_receipt"`
	TreatmentReceipt ArtifactReference `json:"treatment_receipt"`
}

// PrepareRecord retains indexing time and the exact freshness generations. It
// is telemetry only and is never added to measured agent latency.
type PrepareRecord struct {
	SchemaVersion    int                         `json:"schema_version"`
	CampaignID       string                      `json:"campaign_id"`
	InstanceID       string                      `json:"instance_id"`
	Repository       string                      `json:"repository"`
	BaseCommit       string                      `json:"base_commit"`
	StartedAt        string                      `json:"started_at"`
	BaselineMS       int64                       `json:"baseline_settlement_ms"`
	MaterializeMS    int64                       `json:"materialize_ms"`
	FreshnessMS      int64                       `json:"freshness_ms"`
	Freshness        FreshnessProof              `json:"freshness"`
	Telemetry        ArtifactReference           `json:"telemetry"`
	TelemetrySummary PreparationTelemetrySummary `json:"telemetry_summary"`
}

// CampaignFailure keeps failed attempts auditable while fail-fast stops the
// campaign. A later resume creates a new attempt and never erases this record.
type CampaignFailure struct {
	SchemaVersion int    `json:"schema_version"`
	CampaignID    string `json:"campaign_id"`
	InstanceID    string `json:"instance_id"`
	ArmID         string `json:"arm_id,omitempty"`
	Stage         string `json:"stage"`
	FailedAt      string `json:"failed_at"`
	Error         string `json:"error"`
}

// ConfidenceInterval is a deterministic paired bootstrap interval.
type ConfidenceInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// ArmAggregate retains all official means and every resource counter exposed
// by the clients. Provider-specific values remain explicitly labeled.
type ArmAggregate struct {
	Runs               int                `json:"runs"`
	MeanMetrics        map[string]float64 `json:"mean_metrics"`
	TotalElapsedMS     int64              `json:"total_elapsed_ms"`
	TotalUsage         Usage              `json:"total_usage"`
	TotalProviderStats ProviderMetrics    `json:"total_provider_metrics"`
	TotalLCTKCalls     int                `json:"total_lctk_tool_calls"`
}

// ProviderAggregate keeps clients separate as ADR-0030 requires.
type ProviderAggregate struct {
	CompletedPairs       int                           `json:"completed_pairs"`
	Native               ArmAggregate                  `json:"native"`
	LCTK                 ArmAggregate                  `json:"lctk"`
	MeanPairedDelta      map[string]float64            `json:"mean_paired_delta"`
	PrimaryBootstrap95CI map[string]ConfidenceInterval `json:"primary_bootstrap_95_ci"`
}

// CampaignReport is reproducibly rebuilt from immutable receipts and primary
// artifacts. It is a convenience view, never the sole copy of any measurement.
type CampaignReport struct {
	SchemaVersion      int                            `json:"schema_version"`
	CampaignID         string                         `json:"campaign_id"`
	GeneratedAt        string                         `json:"generated_at"`
	RequestedInstances int                            `json:"requested_instances"`
	FailedAttempts     int                            `json:"failed_attempts"`
	Providers          map[Provider]ProviderAggregate `json:"providers"`
}

type campaignOperations struct {
	prepare       func(context.Context, Config, CampaignInstance, string, string) (PrepareRecord, error)
	runArm        func(context.Context, Config, ArmConfig, Instance, string) (Result, error)
	officialScore func(context.Context, string, Config, string) (string, error)
}

// RunCampaign executes sequentially, resumes only from validated receipts, and
// stops at the first invalid state or failed paid arm.
func RunCampaign(ctx context.Context, config Config, manifest CampaignManifest, configPath, manifestPath, outputRoot, python string, maxInstances int, prepareOnly bool) (CampaignReport, error) {
	operations := campaignOperations{prepare: prepareCampaignInstance, runArm: RunArm, officialScore: OfficialScore}
	return runCampaign(ctx, config, manifest, configPath, manifestPath, outputRoot, python, maxInstances, prepareOnly, operations)
}

func runCampaign(ctx context.Context, config Config, manifest CampaignManifest, configPath, manifestPath, outputRoot, python string, maxInstances int, prepareOnly bool, operations campaignOperations) (CampaignReport, error) {
	if python == "" {
		return CampaignReport{}, errors.New("Python executable is required for official scoring")
	}
	if maxInstances < 0 || maxInstances > len(manifest.Instances) {
		return CampaignReport{}, fmt.Errorf("maximum instances must be between 0 and %d", len(manifest.Instances))
	}
	absoluteOutput, err := filepath.Abs(outputRoot)
	if err != nil {
		return CampaignReport{}, fmt.Errorf("resolve campaign root: %w", err)
	}
	outputRoot = absoluteOutput
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return CampaignReport{}, fmt.Errorf("create campaign root: %w", err)
	}
	if err := ensureCampaignSnapshot(configPath, filepath.Join(outputRoot, "config.json")); err != nil {
		return CampaignReport{}, err
	}
	if err := ensureCampaignSnapshot(manifestPath, filepath.Join(outputRoot, "manifest.json")); err != nil {
		return CampaignReport{}, err
	}
	selectedInstances := manifest.Instances
	if maxInstances > 0 {
		selectedInstances = selectedInstances[:maxInstances]
	}
	for _, selected := range selectedInstances {
		instance, err := LoadInstance(config.Benchmark, selected.InstanceID)
		if err != nil {
			return CampaignReport{}, err
		}
		needsPreparation, err := campaignInstanceNeedsPreparation(config, manifest, instance, outputRoot)
		if err != nil {
			report, _ := buildCampaignReport(manifest, config, outputRoot)
			return report, err
		}
		if needsPreparation {
			prepareRoot := filepath.Join(outputRoot, "instances", selected.InstanceID, "prepare-attempts", uniqueArtifactName("prepare", ""))
			if err := os.MkdirAll(prepareRoot, 0o755); err != nil {
				return CampaignReport{}, err
			}
			prepare, err := operations.prepare(ctx, config, selected, outputRoot, prepareRoot)
			if err != nil {
				_ = writeCampaignFailure(outputRoot, CampaignFailure{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, InstanceID: selected.InstanceID, Stage: "prepare", FailedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: err.Error()})
				report, _ := buildCampaignReport(manifest, config, outputRoot)
				return report, err
			}
			prepare.CampaignID = manifest.CampaignID
			preparePath := filepath.Join(prepareRoot, "prepare.json")
			if err := WriteJSONAtomic(preparePath, prepare); err != nil {
				report, _ := buildCampaignReport(manifest, config, outputRoot)
				return report, err
			}
		}
		if prepareOnly {
			continue
		}
		for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
			if _, err := runCampaignPair(ctx, config, manifest, instance, provider, outputRoot, python, operations); err != nil {
				report, _ := buildCampaignReport(manifest, config, outputRoot)
				return report, err
			}
			report, err := buildCampaignReport(manifest, config, outputRoot)
			if err != nil {
				return CampaignReport{}, err
			}
			reportPath := filepath.Join(outputRoot, "reports", uniqueArtifactName("progress", ".json"))
			if err := WriteJSONAtomic(reportPath, report); err != nil {
				return report, err
			}
		}
	}
	return buildCampaignReport(manifest, config, outputRoot)
}

// campaignInstanceNeedsPreparation validates every existing arm receipt before
// touching the shared workspace. A fully paid instance can reconstruct a
// missing pair receipt and progress report from immutable evidence alone, so a
// resume must not spend hours indexing it again.
func campaignInstanceNeedsPreparation(config Config, manifest CampaignManifest, instance Instance, outputRoot string) (bool, error) {
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		native, treatment, err := config.Pair(provider)
		if err != nil {
			return false, err
		}
		for _, arm := range []ArmConfig{native, treatment} {
			receiptPath := filepath.Join(outputRoot, "instances", instance.InstanceID, "receipts", arm.ID+".json")
			if _, _, err := loadArmReceipt(outputRoot, receiptPath, manifest, instance, arm); err != nil {
				if os.IsNotExist(rootCause(err)) {
					return true, nil
				}
				return false, err
			}
		}
	}
	return false, nil
}

func runCampaignPair(ctx context.Context, config Config, manifest CampaignManifest, instance Instance, provider Provider, outputRoot, python string, operations campaignOperations) (PairReceipt, error) {
	native, treatment, err := config.Pair(provider)
	if err != nil {
		return PairReceipt{}, err
	}
	order := CounterbalancedPair(instance.InstanceID, provider, native, treatment)
	results := make(map[Mode]Result, 2)
	receipts := make(map[Mode]ArtifactReference, 2)
	for _, arm := range order {
		receiptPath := filepath.Join(outputRoot, "instances", instance.InstanceID, "receipts", arm.ID+".json")
		receipt, result, loadErr := loadArmReceipt(outputRoot, receiptPath, manifest, instance, arm)
		if loadErr == nil {
			results[arm.Mode] = result
			receipts[arm.Mode] = artifactReference(outputRoot, receiptPath)
			continue
		}
		if !os.IsNotExist(rootCause(loadErr)) {
			return PairReceipt{}, loadErr
		}
		attemptRoot := filepath.Join(outputRoot, "instances", instance.InstanceID, "attempts", arm.ID, uniqueArtifactName("attempt", ""))
		if err := os.MkdirAll(attemptRoot, 0o755); err != nil {
			return PairReceipt{}, err
		}
		result, runErr := operations.runArm(ctx, config, arm, instance, attemptRoot)
		if runErr != nil {
			failure := CampaignFailure{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, InstanceID: instance.InstanceID, ArmID: arm.ID, Stage: "agent", FailedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: runErr.Error()}
			_ = WriteJSONAtomic(filepath.Join(attemptRoot, "failure.json"), failure)
			return PairReceipt{}, fmt.Errorf("campaign arm %q: %w", arm.ID, runErr)
		}
		if len(result.ActualModels) != 0 && (len(result.ActualModels) != 1 || result.ActualModels[0] != arm.Model) {
			failure := CampaignFailure{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, InstanceID: instance.InstanceID, ArmID: arm.ID, Stage: "model-identity", FailedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: fmt.Sprintf("actual models %q differ from requested model %q", result.ActualModels, arm.Model)}
			_ = WriteJSONAtomic(filepath.Join(attemptRoot, "failure.json"), failure)
			return PairReceipt{}, fmt.Errorf("arm %q actual model differs from requested model", arm.ID)
		}
		resultPath := filepath.Join(attemptRoot, arm.ID+".result.json")
		if err := WriteJSONAtomic(resultPath, result); err != nil {
			return PairReceipt{}, err
		}
		officialBody, scoreErr := operations.officialScore(ctx, python, config, resultPath)
		if scoreErr != nil {
			failure := CampaignFailure{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, InstanceID: instance.InstanceID, ArmID: arm.ID, Stage: "official-score", FailedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: scoreErr.Error()}
			_ = WriteJSONAtomic(filepath.Join(attemptRoot, "failure.json"), failure)
			return PairReceipt{}, scoreErr
		}
		var official OfficialScoreRecord
		if err := json.Unmarshal([]byte(officialBody), &official); err != nil {
			return PairReceipt{}, fmt.Errorf("decode official score for arm %q: %w", arm.ID, err)
		}
		local, err := Score(config.Workspace.Root, result.Regions, instance.GroundTruth)
		if err != nil {
			return PairReceipt{}, err
		}
		if !metricsEqual(local, official.Metrics, 1e-12) || official.InstanceID != instance.InstanceID || official.Explorer != arm.ID || official.NumRegions != len(result.Regions) || !equalRegions(official.Regions, result.Regions) {
			return PairReceipt{}, fmt.Errorf("official score parity failed for arm %q", arm.ID)
		}
		scorePath := filepath.Join(attemptRoot, arm.ID+".official-score.json")
		if err := WriteJSONAtomic(scorePath, official); err != nil {
			return PairReceipt{}, err
		}
		receipt = ArmReceipt{
			SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, ConfigSHA256: manifest.ConfigSHA256,
			InstanceID: instance.InstanceID, ArmID: arm.ID, Provider: provider, Mode: arm.Mode,
			CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), RawTrace: artifactReference(outputRoot, filepath.FromSlash(result.RawTracePath)),
			Result: artifactReference(outputRoot, resultPath), OfficialScore: artifactReference(outputRoot, scorePath),
		}
		if receipt.RawTrace.SHA256 == "" || receipt.Result.SHA256 == "" || receipt.OfficialScore.SHA256 == "" {
			return PairReceipt{}, fmt.Errorf("hash completed artifacts for arm %q", arm.ID)
		}
		if err := WriteJSONAtomic(receiptPath, receipt); err != nil {
			return PairReceipt{}, err
		}
		results[arm.Mode] = result
		receipts[arm.Mode] = artifactReference(outputRoot, receiptPath)
	}
	if receipts[ModeNative].SHA256 == "" || receipts[ModeLCTK].SHA256 == "" {
		return PairReceipt{}, fmt.Errorf("provider %q pair has incomplete arm receipt references", provider)
	}
	if results[ModeNative].ClientVersion != results[ModeLCTK].ClientVersion {
		return PairReceipt{}, fmt.Errorf("provider %q client changed between paired arms: %q != %q", provider, results[ModeNative].ClientVersion, results[ModeLCTK].ClientVersion)
	}
	if len(results[ModeNative].ActualModels) != 0 && len(results[ModeLCTK].ActualModels) != 0 && !equalStrings(results[ModeNative].ActualModels, results[ModeLCTK].ActualModels) {
		return PairReceipt{}, fmt.Errorf("provider %q actual models changed between paired arms", provider)
	}
	pairPath := filepath.Join(outputRoot, "instances", instance.InstanceID, "receipts", string(provider)+".pair.json")
	if existing, loadErr := loadPairReceipt(outputRoot, pairPath, manifest, instance.InstanceID, provider, receipts); loadErr == nil {
		return existing, nil
	} else if !os.IsNotExist(rootCause(loadErr)) {
		return PairReceipt{}, loadErr
	}
	pair := PairReceipt{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, InstanceID: instance.InstanceID, Provider: provider, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), NativeReceipt: receipts[ModeNative], TreatmentReceipt: receipts[ModeLCTK]}
	if err := WriteJSONAtomic(pairPath, pair); err != nil {
		return PairReceipt{}, err
	}
	return pair, nil
}

func prepareCampaignInstance(ctx context.Context, config Config, selected CampaignInstance, outputRoot, attemptRoot string) (PrepareRecord, error) {
	started := time.Now().UTC()
	telemetryPath := filepath.Join(attemptRoot, "preparation-telemetry.jsonl")
	telemetry := newPreparationTelemetryWriter(telemetryPath)
	currentCommit, err := commandOutput(ctx, config.Workspace.Root, "git", "rev-parse", "HEAD")
	if err != nil {
		return PrepareRecord{}, err
	}
	currentCommit = strings.TrimSpace(currentCommit)
	baselineStarted := time.Now()
	baseline, err := WaitForLCTKObserved(ctx, config.Workspace, "baseline", telemetry.Observe)
	if err != nil {
		return PrepareRecord{}, fmt.Errorf("settle baseline LCTK generation before checkout: %w", err)
	}
	baselineMS := time.Since(baselineStarted).Milliseconds()
	if strings.EqualFold(currentCommit, selected.BaseCommit) {
		if err := VerifyRepository(ctx, config.Workspace.Root, selected.BaseCommit); err != nil {
			return PrepareRecord{}, err
		}
		return completedPrepareRecord(outputRoot, telemetryPath, telemetry, PrepareRecord{SchemaVersion: CampaignSchemaVersion, CampaignID: "", InstanceID: selected.InstanceID, Repository: selected.Repository, BaseCommit: selected.BaseCommit, StartedAt: started.Format(time.RFC3339Nano), BaselineMS: baselineMS, Freshness: baseline})
	}
	materializeStarted := time.Now()
	if err := Materialize(ctx, config.Workspace.Root, selected.Repository, selected.BaseCommit); err != nil {
		return PrepareRecord{}, err
	}
	materializeMS := time.Since(materializeStarted).Milliseconds()
	freshnessStarted := time.Now()
	proof, err := WaitForLCTKAfterGenerationObserved(ctx, config.Workspace, baseline.ExactGeneration, "freshness", telemetry.Observe)
	if err != nil {
		return PrepareRecord{}, err
	}
	return completedPrepareRecord(outputRoot, telemetryPath, telemetry, PrepareRecord{SchemaVersion: CampaignSchemaVersion, CampaignID: "", InstanceID: selected.InstanceID, Repository: selected.Repository, BaseCommit: selected.BaseCommit, StartedAt: started.Format(time.RFC3339Nano), BaselineMS: baselineMS, MaterializeMS: materializeMS, FreshnessMS: time.Since(freshnessStarted).Milliseconds(), Freshness: proof})
}

func completedPrepareRecord(outputRoot, telemetryPath string, telemetry *preparationTelemetryWriter, record PrepareRecord) (PrepareRecord, error) {
	record.Telemetry = artifactReference(outputRoot, telemetryPath)
	if record.Telemetry.SHA256 == "" {
		return PrepareRecord{}, errors.New("hash durable preparation telemetry")
	}
	record.TelemetrySummary = telemetry.Summary()
	return record, nil
}

func ensureCampaignSnapshot(source, destination string) error {
	sourceDigest, err := FileSHA256(source)
	if err != nil {
		return err
	}
	if destinationDigest, destinationErr := FileSHA256(destination); destinationErr == nil {
		if destinationDigest != sourceDigest {
			return fmt.Errorf("campaign snapshot %s differs from source", destination)
		}
		return nil
	} else if !os.IsNotExist(destinationErr) {
		return destinationErr
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return WriteFileAtomic(destination, body, 0o600)
}

func loadArmReceipt(root, path string, manifest CampaignManifest, instance Instance, arm ArmConfig) (ArmReceipt, Result, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return ArmReceipt{}, Result{}, err
	}
	var receipt ArmReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return ArmReceipt{}, Result{}, err
	}
	if receipt.SchemaVersion != CampaignSchemaVersion || receipt.CampaignID != manifest.CampaignID || receipt.ConfigSHA256 != manifest.ConfigSHA256 || receipt.InstanceID != instance.InstanceID || receipt.ArmID != arm.ID || receipt.Provider != arm.Provider || receipt.Mode != arm.Mode {
		return ArmReceipt{}, Result{}, fmt.Errorf("arm receipt %s has mismatched identity", path)
	}
	for _, artifact := range []ArtifactReference{receipt.RawTrace, receipt.Result, receipt.OfficialScore} {
		if err := verifyArtifactReference(root, artifact); err != nil {
			return ArmReceipt{}, Result{}, err
		}
	}
	resultBody, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(receipt.Result.Path)))
	if err != nil {
		return ArmReceipt{}, Result{}, err
	}
	var result Result
	if err := json.Unmarshal(resultBody, &result); err != nil {
		return ArmReceipt{}, Result{}, err
	}
	if result.InstanceID != instance.InstanceID || result.ArmID != arm.ID || result.Provider != arm.Provider || result.Mode != arm.Mode || result.Model != arm.Model || result.BaseCommit != instance.BaseCommit {
		return ArmReceipt{}, Result{}, fmt.Errorf("result referenced by %s has mismatched identity", path)
	}
	scoreBody, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(receipt.OfficialScore.Path)))
	if err != nil {
		return ArmReceipt{}, Result{}, err
	}
	var score OfficialScoreRecord
	if err := json.Unmarshal(scoreBody, &score); err != nil {
		return ArmReceipt{}, Result{}, err
	}
	if score.InstanceID != instance.InstanceID || score.Explorer != arm.ID || score.NumRegions != len(result.Regions) || !equalRegions(score.Regions, result.Regions) {
		return ArmReceipt{}, Result{}, fmt.Errorf("official score referenced by %s has mismatched identity", path)
	}
	return receipt, result, nil
}

func loadPairReceipt(root, path string, manifest CampaignManifest, instanceID string, provider Provider, receipts map[Mode]ArtifactReference) (PairReceipt, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return PairReceipt{}, err
	}
	var pair PairReceipt
	if err := json.Unmarshal(body, &pair); err != nil {
		return PairReceipt{}, err
	}
	if pair.SchemaVersion != CampaignSchemaVersion || pair.CampaignID != manifest.CampaignID || pair.InstanceID != instanceID || pair.Provider != provider || pair.NativeReceipt != receipts[ModeNative] || pair.TreatmentReceipt != receipts[ModeLCTK] {
		return PairReceipt{}, fmt.Errorf("pair receipt %s has mismatched identity", path)
	}
	return pair, nil
}

func verifyArtifactReference(root string, reference ArtifactReference) error {
	if reference.Path == "" || reference.SHA256 == "" {
		return errors.New("artifact reference is incomplete")
	}
	path := filepath.Join(root, filepath.FromSlash(reference.Path))
	digest, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if digest != reference.SHA256 {
		return fmt.Errorf("artifact digest mismatch for %s: got %s, want %s", reference.Path, digest, reference.SHA256)
	}
	return nil
}

func artifactReference(root, path string) ArtifactReference {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ArtifactReference{}
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ArtifactReference{}
	}
	digest, err := FileSHA256(absolute)
	if err != nil {
		return ArtifactReference{}
	}
	return ArtifactReference{Path: filepath.ToSlash(relative), SHA256: digest}
}

func buildCampaignReport(manifest CampaignManifest, config Config, root string) (CampaignReport, error) {
	report := CampaignReport{SchemaVersion: CampaignSchemaVersion, CampaignID: manifest.CampaignID, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), RequestedInstances: len(manifest.Instances), FailedAttempts: countCampaignFailures(root), Providers: map[Provider]ProviderAggregate{}}
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		nativeArm, treatmentArm, err := config.Pair(provider)
		if err != nil {
			return CampaignReport{}, err
		}
		var nativeResults, treatmentResults []Result
		var nativeScores, treatmentScores []OfficialScoreRecord
		for _, selected := range manifest.Instances {
			pairPath := filepath.Join(root, "instances", selected.InstanceID, "receipts", string(provider)+".pair.json")
			pairBody, pairErr := os.ReadFile(pairPath)
			if os.IsNotExist(pairErr) {
				continue
			}
			if pairErr != nil {
				return CampaignReport{}, pairErr
			}
			var pair PairReceipt
			if err := json.Unmarshal(pairBody, &pair); err != nil {
				return CampaignReport{}, fmt.Errorf("decode pair receipt %s: %w", pairPath, err)
			}
			if pair.CampaignID != manifest.CampaignID || pair.InstanceID != selected.InstanceID || pair.Provider != provider {
				return CampaignReport{}, fmt.Errorf("pair receipt %s has mismatched identity", pairPath)
			}
			for _, item := range []struct {
				mode Mode
				arm  ArmConfig
				ref  ArtifactReference
			}{{ModeNative, nativeArm, pair.NativeReceipt}, {ModeLCTK, treatmentArm, pair.TreatmentReceipt}} {
				if err := verifyArtifactReference(root, item.ref); err != nil {
					return CampaignReport{}, err
				}
				receiptPath := filepath.Join(root, filepath.FromSlash(item.ref.Path))
				body, err := os.ReadFile(receiptPath)
				if err != nil {
					return CampaignReport{}, err
				}
				var receipt ArmReceipt
				if err := json.Unmarshal(body, &receipt); err != nil {
					return CampaignReport{}, err
				}
				if receipt.CampaignID != manifest.CampaignID || receipt.InstanceID != selected.InstanceID || receipt.Provider != provider || receipt.ArmID != item.arm.ID || receipt.Mode != item.mode || verifyArtifactReference(root, receipt.Result) != nil || verifyArtifactReference(root, receipt.OfficialScore) != nil {
					return CampaignReport{}, fmt.Errorf("invalid arm receipt %s", receiptPath)
				}
				var result Result
				var score OfficialScoreRecord
				resultBody, resultErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(receipt.Result.Path)))
				scoreBody, scoreErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(receipt.OfficialScore.Path)))
				if resultErr != nil || scoreErr != nil {
					return CampaignReport{}, fmt.Errorf("read completed pair artifacts for %s", selected.InstanceID)
				}
				if err := json.Unmarshal(resultBody, &result); err != nil {
					return CampaignReport{}, err
				}
				if err := json.Unmarshal(scoreBody, &score); err != nil {
					return CampaignReport{}, err
				}
				if result.InstanceID != selected.InstanceID || result.ArmID != item.arm.ID || result.Provider != provider || result.Mode != item.mode || score.InstanceID != selected.InstanceID || score.Explorer != item.arm.ID || score.NumRegions != len(result.Regions) || !equalRegions(score.Regions, result.Regions) {
					return CampaignReport{}, fmt.Errorf("completed artifacts for %s/%s have mismatched identity", selected.InstanceID, item.arm.ID)
				}
				if item.mode == ModeNative {
					nativeResults = append(nativeResults, result)
					nativeScores = append(nativeScores, score)
				} else {
					treatmentResults = append(treatmentResults, result)
					treatmentScores = append(treatmentScores, score)
				}
			}
		}
		if len(nativeScores) != len(treatmentScores) || len(nativeResults) != len(nativeScores) || len(treatmentResults) != len(treatmentScores) {
			return CampaignReport{}, fmt.Errorf("provider %q completed pair artifacts are unbalanced", provider)
		}
		pairs := len(nativeScores)
		if pairs == 0 {
			report.Providers[provider] = ProviderAggregate{Native: emptyArmAggregate(), LCTK: emptyArmAggregate(), MeanPairedDelta: map[string]float64{}, PrimaryBootstrap95CI: map[string]ConfidenceInterval{}}
			continue
		}
		deltas := make([]map[string]float64, 0, pairs)
		for index := 0; index < pairs; index++ {
			native := metricsMap(nativeScores[index].Metrics)
			treatment := metricsMap(treatmentScores[index].Metrics)
			delta := make(map[string]float64, len(native))
			for name, value := range native {
				delta[name] = treatment[name] - value
			}
			deltas = append(deltas, delta)
		}
		report.Providers[provider] = ProviderAggregate{CompletedPairs: pairs, Native: aggregateArm(nativeResults[:pairs], nativeScores[:pairs]), LCTK: aggregateArm(treatmentResults[:pairs], treatmentScores[:pairs]), MeanPairedDelta: meanMetricMaps(deltas), PrimaryBootstrap95CI: bootstrapPrimary(manifest.CampaignID, provider, deltas)}
	}
	return report, nil
}

func aggregateArm(results []Result, scores []OfficialScoreRecord) ArmAggregate {
	aggregate := emptyArmAggregate()
	aggregate.Runs = len(results)
	metricRows := make([]map[string]float64, 0, len(scores))
	for index, result := range results {
		aggregate.TotalElapsedMS += result.ElapsedMS
		aggregate.TotalUsage.InputTokens += result.Usage.InputTokens
		aggregate.TotalUsage.CachedInputTokens += result.Usage.CachedInputTokens
		aggregate.TotalUsage.CacheCreationInputTokens += result.Usage.CacheCreationInputTokens
		aggregate.TotalUsage.OutputTokens += result.Usage.OutputTokens
		aggregate.TotalUsage.ReasoningOutputTokens += result.Usage.ReasoningOutputTokens
		aggregate.TotalProviderStats.ReportedCostUSD += result.ProviderStats.ReportedCostUSD
		aggregate.TotalProviderStats.DurationMS += result.ProviderStats.DurationMS
		aggregate.TotalProviderStats.APIDurationMS += result.ProviderStats.APIDurationMS
		aggregate.TotalProviderStats.Turns += result.ProviderStats.Turns
		aggregate.TotalLCTKCalls += len(result.LCTKToolCalls)
		if index < len(scores) {
			metricRows = append(metricRows, metricsMap(scores[index].Metrics))
		}
	}
	aggregate.MeanMetrics = meanMetricMaps(metricRows)
	return aggregate
}

func emptyArmAggregate() ArmAggregate { return ArmAggregate{MeanMetrics: map[string]float64{}} }

func metricsMap(metrics Metrics) map[string]float64 {
	body, _ := json.Marshal(metrics)
	values := map[string]float64{}
	_ = json.Unmarshal(body, &values)
	return values
}

func metricsEqual(left, right Metrics, tolerance float64) bool {
	leftValues, rightValues := metricsMap(left), metricsMap(right)
	for name, value := range leftValues {
		if math.Abs(value-rightValues[name]) > tolerance {
			return false
		}
	}
	return true
}

func meanMetricMaps(rows []map[string]float64) map[string]float64 {
	means := map[string]float64{}
	if len(rows) == 0 {
		return means
	}
	for _, row := range rows {
		for name, value := range row {
			means[name] += value
		}
	}
	for name := range means {
		means[name] /= float64(len(rows))
	}
	return means
}

func bootstrapPrimary(campaignID string, provider Provider, deltas []map[string]float64) map[string]ConfidenceInterval {
	result := map[string]ConfidenceInterval{}
	for _, metric := range []string{"weighted_core_coverage", "context_efficiency", "recall_at_300", "first_useful_hit"} {
		values := make([]float64, len(deltas))
		for index := range deltas {
			values[index] = deltas[index][metric]
		}
		if len(values) == 1 {
			result[metric] = ConfidenceInterval{Lower: values[0], Upper: values[0]}
			continue
		}
		digest := sha256.Sum256([]byte(campaignID + "\x00" + string(provider) + "\x00" + metric))
		random := rand.New(rand.NewSource(int64(binary.LittleEndian.Uint64(digest[:8]))))
		distribution := make([]float64, 10000)
		for sample := range distribution {
			for draw := 0; draw < len(values); draw++ {
				distribution[sample] += values[random.Intn(len(values))]
			}
			distribution[sample] /= float64(len(values))
		}
		sort.Float64s(distribution)
		result[metric] = ConfidenceInterval{Lower: distribution[249], Upper: distribution[9749]}
	}
	return result
}

func countCampaignFailures(root string) int {
	count := 0
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && (entry.Name() == "failure.json" || strings.HasPrefix(entry.Name(), "failure-")) {
			count++
		}
		return nil
	})
	return count
}

func writeCampaignFailure(root string, failure CampaignFailure) error {
	path := filepath.Join(root, "instances", failure.InstanceID, "failures", uniqueArtifactName("failure", ".json"))
	return WriteJSONAtomic(path, failure)
}

func uniqueArtifactName(prefix, suffix string) string {
	return prefix + "-" + time.Now().UTC().Format("20060102T150405.000000000Z") + suffix
}

func equalStrings(left, right []string) bool {
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

func equalRegions(left, right []Region) bool {
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

func rootCause(err error) error {
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	return err
}
