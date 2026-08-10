package sweexplore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// NativeImportReport describes re-attested control evidence copied without
// invoking either paid client.
type NativeImportReport struct {
	SourceCampaignID string `json:"source_campaign_id"`
	TargetCampaignID string `json:"target_campaign_id"`
	ImportedReceipts int    `json:"imported_receipts"`
}

// ImportNativeReceipts copies only validated native-control traces, reruns the
// commit-pinned official and local scorers, and publishes new target-campaign
// receipts with explicit provenance. Treatment receipts are never eligible.
func ImportNativeReceipts(ctx context.Context, targetConfig Config, targetManifest CampaignManifest, sourceConfig Config, sourceManifest CampaignManifest, sourceRoot, targetRoot, instanceID, python string) (NativeImportReport, error) {
	report := NativeImportReport{SourceCampaignID: sourceManifest.CampaignID, TargetCampaignID: targetManifest.CampaignID}
	if targetManifest.CampaignID == sourceManifest.CampaignID {
		return report, fmt.Errorf("source and target campaign identities must differ")
	}
	if !safeArtifactID.MatchString(instanceID) {
		return report, fmt.Errorf("exact target instance is required for native receipt import")
	}
	if targetManifest.ExploreSHA256 != sourceManifest.ExploreSHA256 || targetManifest.IssueSHA256 != sourceManifest.IssueSHA256 || targetConfig.Benchmark.OfficialCommit != sourceConfig.Benchmark.OfficialCommit {
		return report, fmt.Errorf("source and target benchmark or official evaluator identity differs")
	}
	absoluteSource, err := filepath.Abs(sourceRoot)
	if err != nil {
		return report, err
	}
	absoluteTarget, err := filepath.Abs(targetRoot)
	if err != nil {
		return report, err
	}
	if samePath(absoluteSource, absoluteTarget) {
		return report, fmt.Errorf("source and target campaign roots must differ")
	}
	if err := os.MkdirAll(absoluteTarget, 0o755); err != nil {
		return report, err
	}
	sourceInstances := make(map[string]CampaignInstance, len(sourceManifest.Instances))
	for _, instance := range sourceManifest.Instances {
		sourceInstances[instance.InstanceID] = instance
	}
	for _, targetSelected := range targetManifest.Instances {
		if targetSelected.InstanceID != instanceID {
			continue
		}
		sourceSelected, exists := sourceInstances[targetSelected.InstanceID]
		if !exists {
			continue
		}
		if sourceSelected != targetSelected {
			return report, fmt.Errorf("instance %q identity differs between campaigns", targetSelected.InstanceID)
		}
		if err := VerifyRepository(ctx, targetConfig.Workspace.Root, targetSelected.BaseCommit); err != nil {
			return report, fmt.Errorf("verify checkout before native re-attestation: %w", err)
		}
		instance, err := LoadInstance(targetConfig.Benchmark, targetSelected.InstanceID)
		if err != nil {
			return report, err
		}
		for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
			sourceArm, _, err := sourceConfig.Pair(provider)
			if err != nil {
				return report, err
			}
			targetArm, _, err := targetConfig.Pair(provider)
			if err != nil {
				return report, err
			}
			if sourceArm.Mode != ModeNative || targetArm.Mode != ModeNative || sourceArm.ID != targetArm.ID || sourceArm.Provider != targetArm.Provider || sourceArm.Model != targetArm.Model || sourceArm.Effort != targetArm.Effort {
				return report, fmt.Errorf("provider %q native arm identity differs between campaigns", provider)
			}
			if err := verifyImportClientIdentity(sourceManifest, targetManifest, provider); err != nil {
				return report, err
			}
			sourceReceiptPath := filepath.Join(absoluteSource, "instances", instance.InstanceID, "receipts", sourceArm.ID+".json")
			sourceReceipt, sourceResult, err := loadArmReceipt(absoluteSource, sourceReceiptPath, sourceManifest, instance, sourceArm)
			if err != nil {
				if os.IsNotExist(rootCause(err)) {
					continue
				}
				return report, fmt.Errorf("validate source native receipt %s: %w", sourceReceiptPath, err)
			}
			if sourceResult.ClientVersion != campaignClient(sourceManifest, provider).Version || len(sourceResult.LCTKToolCalls) != 0 {
				return report, fmt.Errorf("source native result for %s/%s has invalid client or LCTK usage", instance.InstanceID, provider)
			}
			targetReceiptPath := filepath.Join(absoluteTarget, "instances", instance.InstanceID, "receipts", targetArm.ID+".json")
			if _, _, err := loadArmReceipt(absoluteTarget, targetReceiptPath, targetManifest, instance, targetArm); err == nil {
				continue
			} else if !os.IsNotExist(rootCause(err)) {
				return report, err
			}
			attemptRoot := filepath.Join(absoluteTarget, "instances", instance.InstanceID, "attempts", targetArm.ID, uniqueArtifactName("import", ""))
			if err := os.MkdirAll(attemptRoot, 0o755); err != nil {
				return report, err
			}
			sourceRawPath := filepath.Join(absoluteSource, filepath.FromSlash(sourceReceipt.RawTrace.Path))
			rawBody, err := os.ReadFile(sourceRawPath)
			if err != nil {
				return report, err
			}
			targetRawPath := filepath.Join(attemptRoot, targetArm.ID+".trace.jsonl")
			if err := WriteFileAtomic(targetRawPath, rawBody, 0o600); err != nil {
				return report, err
			}
			sourceResult.RawTracePath = filepath.ToSlash(targetRawPath)
			targetResultPath := filepath.Join(attemptRoot, targetArm.ID+".result.json")
			if err := WriteJSONAtomic(targetResultPath, sourceResult); err != nil {
				return report, err
			}
			officialBody, err := OfficialScore(ctx, python, targetConfig, targetResultPath)
			if err != nil {
				return report, err
			}
			var official OfficialScoreRecord
			if err := json.Unmarshal([]byte(officialBody), &official); err != nil {
				return report, err
			}
			local, err := Score(targetConfig.Workspace.Root, sourceResult.Regions, instance.GroundTruth)
			if err != nil {
				return report, err
			}
			if !metricsEqual(local, official.Metrics, 1e-12) || official.InstanceID != instance.InstanceID || official.Explorer != targetArm.ID || official.NumRegions != len(sourceResult.Regions) || !equalRegions(official.Regions, sourceResult.Regions) {
				return report, fmt.Errorf("re-attested score parity failed for %s/%s", instance.InstanceID, provider)
			}
			targetScorePath := filepath.Join(attemptRoot, targetArm.ID+".official-score.json")
			if err := WriteJSONAtomic(targetScorePath, official); err != nil {
				return report, err
			}
			receipt := ArmReceipt{SchemaVersion: CampaignSchemaVersion, CampaignID: targetManifest.CampaignID, ConfigSHA256: targetManifest.ConfigSHA256,
				InstanceID: instance.InstanceID, ArmID: targetArm.ID, Provider: provider, Mode: ModeNative, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
				RawTrace: artifactReference(absoluteTarget, targetRawPath), Result: artifactReference(absoluteTarget, targetResultPath), OfficialScore: artifactReference(absoluteTarget, targetScorePath),
				ImportedFrom: &ReceiptImportProvenance{SourceCampaignID: sourceManifest.CampaignID, SourceConfigSHA256: sourceManifest.ConfigSHA256,
					SourceRoot: absoluteSource, SourceReceipt: artifactReference(absoluteSource, sourceReceiptPath), SourceRawTrace: sourceReceipt.RawTrace,
					SourceResult: sourceReceipt.Result, SourceOfficialScore: sourceReceipt.OfficialScore, ImportedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
			if receipt.RawTrace.SHA256 == "" || receipt.Result.SHA256 == "" || receipt.OfficialScore.SHA256 == "" || receipt.ImportedFrom.SourceReceipt.SHA256 == "" {
				return report, fmt.Errorf("hash imported artifacts for %s/%s", instance.InstanceID, provider)
			}
			if err := WriteJSONAtomic(targetReceiptPath, receipt); err != nil {
				return report, err
			}
			report.ImportedReceipts++
		}
	}
	if report.ImportedReceipts == 0 {
		return report, fmt.Errorf("source campaign has no importable native receipts for instance %q", instanceID)
	}
	return report, nil
}

func verifyImportClientIdentity(source, target CampaignManifest, provider Provider) error {
	sourceClient := campaignClient(source, provider)
	targetClient := campaignClient(target, provider)
	if sourceClient.Provider == "" || targetClient.Provider == "" || sourceClient.ExecutableSHA256 != targetClient.ExecutableSHA256 || sourceClient.Version != targetClient.Version || sourceClient.Model != targetClient.Model || sourceClient.Effort != targetClient.Effort {
		return fmt.Errorf("provider %q executable, version, model, or effort differs between campaigns", provider)
	}
	return nil
}

func campaignClient(manifest CampaignManifest, provider Provider) CampaignClient {
	for _, client := range manifest.Clients {
		if client.Provider == provider {
			return client
		}
	}
	return CampaignClient{}
}
