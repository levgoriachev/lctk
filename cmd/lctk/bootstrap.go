package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/diskspace"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type bootstrapInference interface {
	ImageAvailable(context.Context) bool
	ModelAvailable() bool
	PullImage(context.Context) error
	InstallModel(context.Context, *http.Client) error
	Ensure(context.Context, time.Duration) (inference.Status, error)
	SelfTest(context.Context) error
}

var newBootstrapInference = func(distribution inference.Distribution) (bootstrapInference, error) {
	return inference.NewManagerForDistribution(containerruntime.Runner{}, distribution)
}

type bootstrapNVIDIA interface {
	Inspect(context.Context, releasebundle.Manifest) (nvidiainstall.Plan, error)
	Ensure(context.Context, releasebundle.Manifest) (nvidiainstall.Status, error)
}

var (
	newBootstrapNVIDIA       = func() bootstrapNVIDIA { return nvidiainstall.NewManager() }
	probeBootstrapNVIDIAHost = nvidiainstall.ProbeHost
)

var (
	newBootstrapVerifier  = releasebundle.ProductionVerifier
	loadBootstrapManifest = func(ctx context.Context, source string, verifier releasebundle.Verifier) (releasebundle.Manifest, error) {
		return releasebundle.Load(ctx, source, http.DefaultClient, verifier)
	}
)

type bootstrapComponent struct {
	Name          string `json:"name"`
	Identity      string `json:"identity"`
	Installed     bool   `json:"installed"`
	DownloadBytes int64  `json:"download_bytes,omitempty"`
}

type bootstrapPlan struct {
	Version           string               `json:"version"`
	OS                string               `json:"os"`
	Arch              string               `json:"arch"`
	Writes            bool                 `json:"writes"`
	Ready             bool                 `json:"ready"`
	Components        []bootstrapComponent `json:"components"`
	DiskBytes         int64                `json:"disk_bytes"`
	AvailableBytes    uint64               `json:"available_bytes"`
	Applied           bool                 `json:"applied"`
	SelfTest          bool                 `json:"self_test"`
	RecommendedAction string               `json:"recommended_action,omitempty"`
}

func runBootstrap(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	planOnly := flags.Bool("plan", false, "inspect prerequisites and planned writes without changing state")
	proceed := flags.Bool("yes", false, "apply the displayed bootstrap plan")
	asJSON := flags.Bool("json", false, "write the plan and result as JSON")
	manifestSource := flags.String("manifest", "", "signed release manifest HTTPS URL or local file")
	distributionValue := flags.String("inference-distribution", "", "explicit cpu or nvidia_gpu distribution")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: lctk bootstrap [--manifest SOURCE] [--inference-distribution cpu|nvidia_gpu] [--plan] [--yes] [--json]")
	}
	previousSelection, err := inference.LoadSelection()
	if err != nil {
		return err
	}
	distribution := previousSelection.Distribution
	if *distributionValue != "" {
		distribution = inference.Distribution(*distributionValue)
	}
	if !distribution.Valid() {
		return fmt.Errorf("unsupported inference distribution %q", distribution)
	}

	stack := newStackManager()
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := stack.RuntimeAvailable(probeCtx); err != nil {
		return err
	}
	shared, err := newBootstrapInference(distribution)
	if err != nil {
		return err
	}
	codeImage := projectstack.ImageRepository + ":" + buildinfo.Version
	codeInstalled := stack.ImageAvailable(probeCtx, codeImage) == nil
	inferenceInstalled := shared.ImageAvailable(probeCtx)
	modelInstalled := shared.ModelAvailable()
	var release releasebundle.Manifest
	var nvidiaPlan nvidiainstall.Plan
	manifestRequired := !strings.HasSuffix(buildinfo.Version, "-dev")
	if *manifestSource != "" || releasebundle.DefaultManifestURL != "" || manifestRequired {
		verifier, verifyErr := newBootstrapVerifier()
		if verifyErr != nil {
			return verifyErr
		}
		release, err = loadBootstrapManifest(ctx, *manifestSource, verifier)
		if err != nil {
			return err
		}
		if release.Version != buildinfo.Version || release.InferenceImage.Reference != inference.Image ||
			release.EmbeddingModel.SHA256 != inference.ModelSHA256 || release.EmbeddingModel.Bytes != inference.ModelBytes {
			return errors.New("signed bootstrap manifest does not match this host build")
		}
		if distribution == inference.DistributionNVIDIAGPU {
			if _, err := nvidiainstall.ValidateManifest(release); err != nil {
				return err
			}
			if _, err := probeBootstrapNVIDIAHost(probeCtx); err != nil {
				return err
			}
			nvidiaPlan, err = newBootstrapNVIDIA().Inspect(probeCtx, release)
			if err != nil {
				return err
			}
		}
		codeInstalled, err = stack.ImageMatches(probeCtx, codeImage, release.CodeImage.Reference)
		if err != nil {
			return err
		}
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate packaged host core: %w", err)
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect packaged host core: %w", err)
	}
	hostInstalled := false
	if activation, loadErr := installation.Load(home); loadErr == nil {
		hostInstalled = activation.ActiveVersion == buildinfo.Version
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	available, err := diskspace.Available(home)
	if err != nil {
		return err
	}
	downloadBytes := missingBytes(modelInstalled, inference.ModelBytes) + missingBytes(hostInstalled, executableInfo.Size())
	if release.Version != "" {
		downloadBytes += missingBytes(codeInstalled, release.CodeImage.CompressedBytes)
		selectedImageBytes := release.InferenceImage.CompressedBytes
		if distribution == inference.DistributionNVIDIAGPU {
			selectedImageBytes = release.NVIDIAGPUInferenceImage.CompressedBytes
			downloadBytes += nvidiaPlan.DownloadBytes
		}
		downloadBytes += missingBytes(inferenceInstalled, selectedImageBytes)
	}
	requiredBytes := installation.RequiredBytes(downloadBytes)
	if release.Version != "" && distribution == inference.DistributionNVIDIAGPU && !inferenceInstalled {
		requiredBytes += release.NVIDIAGPUInferenceImage.UnpackedBytes
	}
	var codeImageBytes, inferenceImageBytes int64
	codeIdentity := codeImage
	if release.Version != "" {
		codeImageBytes = missingBytes(codeInstalled, release.CodeImage.CompressedBytes)
		selectedImageBytes := release.InferenceImage.CompressedBytes
		if distribution == inference.DistributionNVIDIAGPU {
			selectedImageBytes = release.NVIDIAGPUInferenceImage.CompressedBytes
		}
		inferenceImageBytes = missingBytes(inferenceInstalled, selectedImageBytes)
		codeIdentity = release.CodeImage.Reference
	}
	plan := bootstrapPlan{
		Version: buildinfo.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Writes: false, Ready: codeInstalled && available >= uint64(requiredBytes),
		Components: []bootstrapComponent{
			{Name: "host-core", Identity: buildinfo.Version, Installed: hostInstalled,
				DownloadBytes: missingBytes(hostInstalled, executableInfo.Size())},
			{Name: "code-intel", Identity: codeIdentity, Installed: codeInstalled, DownloadBytes: codeImageBytes},
			{Name: "embedding-inference", Identity: selectedInferenceImage(distribution), Installed: inferenceInstalled, DownloadBytes: inferenceImageBytes},
			{Name: "embedding-model", Identity: inference.ModelSHA256, Installed: modelInstalled,
				DownloadBytes: missingBytes(modelInstalled, inference.ModelBytes)},
		},
		DiskBytes: requiredBytes, AvailableBytes: available,
	}
	if !codeInstalled {
		if release.Version == "" {
			plan.RecommendedAction = "Install the matching code-intel image; in a source checkout run lctk image build."
		}
	}
	if available < uint64(requiredBytes) {
		plan.RecommendedAction = fmt.Sprintf("Free disk space: bootstrap requires %d bytes and only %d are available.", requiredBytes, available)
	}
	if *planOnly || !*proceed {
		if *asJSON {
			return writeJSON(stdout, plan)
		}
		writeBootstrapPlan(stdout, plan)
		if !*planOnly {
			fmt.Fprintln(stdout, "No changes applied. Review the plan, then run lctk bootstrap --yes.")
		}
		return nil
	}
	if !codeInstalled && release.Version == "" {
		if *asJSON {
			_ = writeJSON(stdout, plan)
		}
		return errors.New(plan.RecommendedAction)
	}
	if distribution == inference.DistributionNVIDIAGPU && release.Version == "" {
		return errors.New("NVIDIA GPU bootstrap requires a signed release manifest")
	}
	if available < uint64(requiredBytes) {
		if *asJSON {
			_ = writeJSON(stdout, plan)
		}
		return errors.New(plan.RecommendedAction)
	}
	applyCtx, applyCancel := context.WithTimeout(ctx, 20*time.Minute)
	defer applyCancel()
	if !codeInstalled {
		if err := stack.InstallImage(applyCtx, release.CodeImage.Reference, buildinfo.Version); err != nil {
			return err
		}
	}
	if distribution == inference.DistributionNVIDIAGPU {
		if _, err := newBootstrapNVIDIA().Ensure(applyCtx, release); err != nil {
			return err
		}
	}
	if !inferenceInstalled {
		if err := shared.PullImage(applyCtx); err != nil {
			return err
		}
	}
	if !modelInstalled {
		if err := shared.InstallModel(applyCtx, nil); err != nil {
			return err
		}
	}
	if _, err := shared.Ensure(applyCtx, 2*time.Minute); err != nil {
		return err
	}
	if err := shared.SelfTest(applyCtx); err != nil {
		return err
	}
	installer := installation.NewManager(home)
	if _, err := installer.Adopt(executable, buildinfo.Version); err != nil {
		return errors.Join(err, restoreInferenceDistribution(applyCtx, previousSelection.Distribution, distribution))
	}
	if err := inference.SaveSelection(inference.Selection{
		SchemaVersion: inference.SelectionSchemaVersion, Distribution: distribution,
	}); err != nil {
		return errors.Join(err, restoreInferenceDistribution(applyCtx, previousSelection.Distribution, distribution))
	}
	plan.Writes = true
	plan.Ready = true
	plan.Applied = true
	plan.SelfTest = true
	for index := range plan.Components {
		plan.Components[index].Installed = true
		plan.Components[index].DownloadBytes = 0
	}
	plan.DiskBytes = 0
	plan.RecommendedAction = ""
	if *asJSON {
		return writeJSON(stdout, plan)
	}
	fmt.Fprintln(stdout, "Bootstrap complete; the pinned local embedding path passed its functional self-test.")
	return nil
}

func selectedInferenceImage(distribution inference.Distribution) string {
	if distribution == inference.DistributionNVIDIAGPU {
		return nvidiainstall.Image
	}
	return inference.Image
}

func restoreInferenceDistribution(ctx context.Context, previous, attempted inference.Distribution) error {
	if previous == attempted {
		return nil
	}
	manager, err := newBootstrapInference(previous)
	if err != nil {
		return fmt.Errorf("restore previous inference distribution: %w", err)
	}
	if _, err := manager.Ensure(context.WithoutCancel(ctx), 2*time.Minute); err != nil {
		return fmt.Errorf("restore previous inference distribution: %w", err)
	}
	return nil
}

func missingBytes(installed bool, size int64) int64 {
	if installed {
		return 0
	}
	return size
}

func writeBootstrapPlan(output io.Writer, plan bootstrapPlan) {
	fmt.Fprintf(output, "LCTK %s bootstrap plan for %s/%s (writes: %t)\n", plan.Version, plan.OS, plan.Arch, plan.Writes)
	for _, component := range plan.Components {
		fmt.Fprintf(output, "  %s: installed=%t identity=%s", component.Name, component.Installed, component.Identity)
		if component.DownloadBytes > 0 {
			fmt.Fprintf(output, " download=%d bytes", component.DownloadBytes)
		}
		fmt.Fprintln(output)
	}
	if plan.RecommendedAction != "" {
		fmt.Fprintln(output, "  action:", plan.RecommendedAction)
	}
}
