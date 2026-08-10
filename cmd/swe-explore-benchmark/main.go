// Command swe-explore-benchmark validates and runs the warm-index experiment
// defined by ADR-0030. It is development tooling, not part of the installed
// LCTK product surface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/sweexplore"
)

const usage = `Usage:
  swe-explore-benchmark validate --config FILE
  swe-explore-benchmark prepare --config FILE --instance ID
  swe-explore-benchmark preflight --config FILE --instance ID --arm ID
  swe-explore-benchmark run --config FILE --instance ID --arm ID --output FILE
  swe-explore-benchmark pair --config FILE --instance ID --provider codex|claude --output-dir DIR
  swe-explore-benchmark score --config FILE --result FILE
  swe-explore-benchmark official-score --config FILE --result FILE --python EXE
  swe-explore-benchmark manifest --config FILE --campaign-id ID --count N --seed TEXT --harness-commit SHA --output FILE
  swe-explore-benchmark campaign --config FILE --manifest FILE --output-dir DIR --python EXE [--max-instances N] [--prepare-only]
  swe-explore-benchmark campaign-import-native --config FILE --manifest FILE --source-config FILE --source-manifest FILE --source-harness FILE --source-dir DIR --output-dir DIR --instance ID --python EXE
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "validate":
		return validateCommand(ctx, args[1:], stdout)
	case "preflight":
		return preflightCommand(ctx, args[1:], stdout)
	case "prepare":
		return prepareCommand(ctx, args[1:], stdout)
	case "run":
		return runCommand(ctx, args[1:], stdout)
	case "pair":
		return pairCommand(ctx, args[1:], stdout)
	case "score":
		return scoreCommand(args[1:], stdout)
	case "official-score":
		return officialScoreCommand(ctx, args[1:], stdout)
	case "manifest":
		return manifestCommand(ctx, args[1:], stdout)
	case "campaign":
		return campaignCommand(ctx, args[1:], stdout)
	case "campaign-import-native":
		return campaignImportNativeCommand(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func manifestCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	campaignID := flags.String("campaign-id", "", "immutable campaign identifier")
	count := flags.Int("count", 0, "repository-stratified instance count")
	seed := flags.String("seed", "", "deterministic selection seed")
	harnessCommit := flags.String("harness-commit", "", "full repository commit containing the harness")
	outputPath := flags.String("output", "", "immutable manifest path")
	if err := flags.Parse(args); err != nil || *configPath == "" || *campaignID == "" || *count <= 0 || *seed == "" || *harnessCommit == "" || *outputPath == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark manifest --config FILE --campaign-id ID --count N --seed TEXT --harness-commit SHA --output FILE")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	currentCommit, err := gitOutput(ctx, ".", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect harness source commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(currentCommit), *harnessCommit) {
		return fmt.Errorf("harness source is at %s, want declared commit %s", strings.TrimSpace(currentCommit), *harnessCommit)
	}
	harness, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve benchmark executable: %w", err)
	}
	manifest, err := sweexplore.BuildCampaignManifest(ctx, *configPath, config, *campaignID, *count, *seed, harness, *harnessCommit)
	if err != nil {
		return err
	}
	if err := sweexplore.WriteJSONAtomic(*outputPath, manifest); err != nil {
		return err
	}
	return writeJSON(stdout, manifest)
}

func campaignCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("campaign", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	manifestPath := flags.String("manifest", "", "immutable campaign manifest")
	outputDir := flags.String("output-dir", "", "campaign artifact root")
	python := flags.String("python", "python", "Python 3.12+ executable")
	maxInstances := flags.Int("max-instances", 0, "run only the first N immutable manifest instances; zero means all")
	prepareOnly := flags.Bool("prepare-only", false, "materialize and verify indexes without invoking paid clients")
	if err := flags.Parse(args); err != nil || *configPath == "" || *manifestPath == "" || *outputDir == "" || *python == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark campaign --config FILE --manifest FILE --output-dir DIR --python EXE [--max-instances N] [--prepare-only]")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	harness, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve benchmark executable: %w", err)
	}
	manifest, err := sweexplore.LoadCampaignManifest(ctx, *manifestPath, *configPath, harness, config)
	if err != nil {
		return err
	}
	currentCommit, err := gitOutput(ctx, ".", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect harness source commit: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(currentCommit), manifest.HarnessCommit) {
		return fmt.Errorf("harness source is at %s, manifest pins %s", strings.TrimSpace(currentCommit), manifest.HarnessCommit)
	}
	report, err := sweexplore.RunCampaign(ctx, config, manifest, *configPath, *manifestPath, *outputDir, *python, *maxInstances, *prepareOnly)
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func campaignImportNativeCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("campaign-import-native", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "target campaign configuration")
	manifestPath := flags.String("manifest", "", "target immutable campaign manifest")
	sourceConfigPath := flags.String("source-config", "", "source campaign configuration")
	sourceManifestPath := flags.String("source-manifest", "", "source immutable campaign manifest")
	sourceHarness := flags.String("source-harness", "", "source campaign harness executable")
	sourceDir := flags.String("source-dir", "", "source campaign artifact root")
	outputDir := flags.String("output-dir", "", "target campaign artifact root")
	instanceID := flags.String("instance", "", "exact checked-out instance to re-attest")
	python := flags.String("python", "python", "Python 3.12+ executable")
	if err := flags.Parse(args); err != nil || *configPath == "" || *manifestPath == "" || *sourceConfigPath == "" || *sourceManifestPath == "" || *sourceHarness == "" || *sourceDir == "" || *outputDir == "" || *instanceID == "" || *python == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark campaign-import-native --config FILE --manifest FILE --source-config FILE --source-manifest FILE --source-harness FILE --source-dir DIR --output-dir DIR --instance ID --python EXE")
	}
	targetConfig, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	sourceConfig, err := sweexplore.LoadConfig(*sourceConfigPath)
	if err != nil {
		return err
	}
	harness, err := os.Executable()
	if err != nil {
		return err
	}
	targetManifest, err := sweexplore.LoadCampaignManifest(ctx, *manifestPath, *configPath, harness, targetConfig)
	if err != nil {
		return err
	}
	sourceManifest, err := sweexplore.LoadCampaignManifest(ctx, *sourceManifestPath, *sourceConfigPath, *sourceHarness, sourceConfig)
	if err != nil {
		return err
	}
	report, err := sweexplore.ImportNativeReceipts(ctx, targetConfig, targetManifest, sourceConfig, sourceManifest, *sourceDir, *outputDir, *instanceID, *python)
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func pairCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	instanceID := flags.String("instance", "", "exact benchmark instance")
	providerName := flags.String("provider", "", "codex or claude")
	outputDir := flags.String("output-dir", "", "pair artifact directory")
	if err := flags.Parse(args); err != nil || *configPath == "" || *instanceID == "" || *providerName == "" || *outputDir == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark pair --config FILE --instance ID --provider codex|claude --output-dir DIR")
	}
	provider := sweexplore.Provider(*providerName)
	if provider != sweexplore.ProviderCodex && provider != sweexplore.ProviderClaude {
		return fmt.Errorf("unsupported pair provider %q", provider)
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	native, treatment, err := config.Pair(provider)
	if err != nil {
		return err
	}
	instance, err := sweexplore.LoadInstance(config.Benchmark, *instanceID)
	if err != nil {
		return err
	}
	order := sweexplore.CounterbalancedPair(instance.InstanceID, provider, native, treatment)
	results := make([]sweexplore.Result, 0, 2)
	for _, arm := range order {
		resultPath := filepath.Join(*outputDir, arm.ID+".result.json")
		result, err := sweexplore.RunArm(ctx, config, arm, instance, *outputDir)
		if err != nil {
			return fmt.Errorf("pair arm %q: %w", arm.ID, err)
		}
		if err := writeJSONFile(resultPath, result); err != nil {
			return err
		}
		results = append(results, result)
	}
	if results[0].ClientVersion != results[1].ClientVersion {
		return fmt.Errorf("provider %q client changed between paired arms: %q != %q", provider, results[0].ClientVersion, results[1].ClientVersion)
	}
	if len(results[0].ActualModels) != 0 && len(results[1].ActualModels) != 0 && !slices.Equal(results[0].ActualModels, results[1].ActualModels) {
		return fmt.Errorf("provider %q actual models changed between paired arms: %q != %q", provider, results[0].ActualModels, results[1].ActualModels)
	}
	return writeJSON(stdout, map[string]any{"instance_id": instance.InstanceID, "provider": provider, "results": results})
}

func officialScoreCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("official-score", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	resultPath := flags.String("result", "", "normalized result JSON")
	python := flags.String("python", "python", "Python 3.12+ executable")
	if err := flags.Parse(args); err != nil || *configPath == "" || *resultPath == "" || *python == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark official-score --config FILE --result FILE --python EXE")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	output, err := sweexplore.OfficialScore(ctx, *python, config, *resultPath)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, output)
	return err
}

func prepareCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	instanceID := flags.String("instance", "", "exact benchmark instance")
	if err := flags.Parse(args); err != nil || *configPath == "" || *instanceID == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark prepare --config FILE --instance ID")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	instance, err := sweexplore.LoadInstance(config.Benchmark, *instanceID)
	if err != nil {
		return err
	}
	if err := sweexplore.Materialize(ctx, config.Workspace.Root, instance.Repository, instance.BaseCommit); err != nil {
		return err
	}
	proof, err := sweexplore.WaitForLCTK(ctx, config.Workspace)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"instance_id": *instanceID, "base_commit": instance.BaseCommit, "freshness": proof})
}

func validateCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark validate --config FILE")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := sweexplore.VerifySHA256(config.Benchmark.ExploreDatasetPath, config.Benchmark.ExploreDatasetSHA256); err != nil {
		return err
	}
	if err := sweexplore.VerifySHA256(config.Benchmark.IssueDatasetPath, config.Benchmark.IssueDatasetSHA256); err != nil {
		return err
	}
	commit, err := gitOutput(ctx, config.Benchmark.OfficialRoot, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("inspect official evaluator: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(commit), config.Benchmark.OfficialCommit) {
		return fmt.Errorf("official evaluator is at %s, want %s", strings.TrimSpace(commit), config.Benchmark.OfficialCommit)
	}
	for _, arm := range config.Arms {
		if _, err := exec.LookPath(arm.Executable); err != nil {
			return fmt.Errorf("arm %q executable: %w", arm.ID, err)
		}
	}
	return writeJSON(stdout, map[string]any{"valid": true, "arms": len(config.Arms), "schema_version": sweexplore.SchemaVersion})
}

func preflightCommand(ctx context.Context, args []string, stdout io.Writer) error {
	config, arm, instance, err := loadSelection(args, "preflight")
	if err != nil {
		return err
	}
	if err := sweexplore.VerifyRepository(ctx, config.Workspace.Root, instance.BaseCommit); err != nil {
		return err
	}
	response := map[string]any{"instance_id": instance.InstanceID, "arm_id": arm.ID, "repository_commit": instance.BaseCommit}
	if arm.Mode == sweexplore.ModeLCTK {
		proof, err := sweexplore.WaitForLCTK(ctx, config.Workspace)
		if err != nil {
			return err
		}
		response["freshness"] = proof
	}
	return writeJSON(stdout, response)
}

func runCommand(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	instanceID := flags.String("instance", "", "exact benchmark instance")
	armID := flags.String("arm", "", "exact configured arm")
	outputPath := flags.String("output", "", "normalized result JSON")
	if err := flags.Parse(args); err != nil || *configPath == "" || *instanceID == "" || *armID == "" || *outputPath == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark run --config FILE --instance ID --arm ID --output FILE")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	arm, err := config.Arm(*armID)
	if err != nil {
		return err
	}
	instance, err := sweexplore.LoadInstance(config.Benchmark, *instanceID)
	if err != nil {
		return err
	}
	result, err := sweexplore.RunArm(ctx, config, arm, instance, filepath.Dir(*outputPath))
	if err != nil {
		return err
	}
	if err := writeJSONFile(*outputPath, result); err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func scoreCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("score", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	resultPath := flags.String("result", "", "normalized result JSON")
	if err := flags.Parse(args); err != nil || *configPath == "" || *resultPath == "" || flags.NArg() != 0 {
		return errors.New("usage: swe-explore-benchmark score --config FILE --result FILE")
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(*resultPath)
	if err != nil {
		return err
	}
	var result sweexplore.Result
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	instance, err := sweexplore.LoadInstance(config.Benchmark, result.InstanceID)
	if err != nil {
		return err
	}
	if result.SchemaVersion != sweexplore.SchemaVersion || result.BaseCommit != instance.BaseCommit || strings.TrimSpace(result.ClientVersion) == "" {
		return errors.New("result schema, client version, or base commit does not match the pinned instance")
	}
	metrics, err := sweexplore.Score(config.Workspace.Root, result.Regions, instance.GroundTruth)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"instance_id": result.InstanceID, "explorer": result.ArmID,
		"regions": result.Regions, "metrics": metrics, "num_regions": len(result.Regions),
	})
}

func loadSelection(args []string, command string) (sweexplore.Config, sweexplore.ArmConfig, sweexplore.Instance, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "campaign configuration")
	instanceID := flags.String("instance", "", "exact benchmark instance")
	armID := flags.String("arm", "", "exact configured arm")
	if err := flags.Parse(args); err != nil || *configPath == "" || *instanceID == "" || *armID == "" || flags.NArg() != 0 {
		return sweexplore.Config{}, sweexplore.ArmConfig{}, sweexplore.Instance{}, fmt.Errorf("usage: swe-explore-benchmark %s --config FILE --instance ID --arm ID", command)
	}
	config, err := sweexplore.LoadConfig(*configPath)
	if err != nil {
		return sweexplore.Config{}, sweexplore.ArmConfig{}, sweexplore.Instance{}, err
	}
	arm, err := config.Arm(*armID)
	if err != nil {
		return sweexplore.Config{}, sweexplore.ArmConfig{}, sweexplore.Instance{}, err
	}
	instance, err := sweexplore.LoadInstance(config.Benchmark, *instanceID)
	return config, arm, instance, err
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, "git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
	return string(output), nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeJSONFile(path string, value any) error {
	if err := sweexplore.WriteJSONAtomic(path, value); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}
