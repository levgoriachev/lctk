// Package setupflow coordinates the signed host, private runtime, desktop, and
// bootstrap transactions behind the one-click Windows setup UI.
package setupflow

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/daemonstate"
	"github.com/lev-goryachev/lctk/internal/desktopinstall"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/runtimeinstall"
	"github.com/lev-goryachev/lctk/internal/updateflow"
	"github.com/lev-goryachev/lctk/internal/windowsprocess"
	"github.com/lev-goryachev/lctk/internal/windowssetup"
)

// ErrRebootRequired is a successful prerequisite mutation that cannot continue
// until Windows restarts.
var ErrRebootRequired = errors.New("Windows must restart before LCTK setup can continue")

// Action identifies the exact setup transaction presented to the user. The
// installer never silently turns an older package into a downgrade.
type Action string

const (
	ActionInstall Action = "install"
	ActionUpgrade Action = "upgrade"
	ActionRepair  Action = "repair"
)

// Plan is the complete read-only setup decision shown by setup.exe.
type Plan struct {
	Action                    Action                 `json:"action"`
	CurrentVersion            string                 `json:"current_version,omitempty"`
	Version                   string                 `json:"version"`
	Host                      windowssetup.Status    `json:"host"`
	Runtime                   runtimeinstall.Plan    `json:"runtime"`
	Core                      installation.Plan      `json:"core"`
	Desktop                   desktopinstall.Plan    `json:"desktop"`
	InferenceDistribution     inference.Distribution `json:"inference_distribution"`
	GPU                       *nvidiainstall.GPU     `json:"gpu,omitempty"`
	NVIDIA                    nvidiainstall.Plan     `json:"nvidia,omitempty"`
	Upgrade                   updateflow.Plan        `json:"upgrade,omitempty"`
	DownloadBytes             int64                  `json:"download_bytes"`
	RuntimeDataRequiredBytes  int64                  `json:"runtime_data_required_bytes"`
	RuntimeDataAvailableBytes uint64                 `json:"runtime_data_available_bytes"`
	InferenceImageInstalled   bool                   `json:"inference_image_installed"`
	InferenceModelInstalled   bool                   `json:"inference_model_installed"`
	InferenceDownloadBytes    int64                  `json:"inference_download_bytes"`
	InferenceRuntimeBytes     int64                  `json:"inference_runtime_required_bytes"`
	Ready                     bool                   `json:"ready"`
	Writes                    bool                   `json:"writes"`
}

// Progress is one durable user-facing setup phase.
type Progress func(phase, detail string)

type runtimeInstaller interface {
	Inspect(releasebundle.Manifest) (runtimeinstall.Plan, error)
	Install(context.Context, releasebundle.Manifest) error
}

type coreInstaller interface {
	Inspect(releasebundle.Manifest) (installation.Plan, releasebundle.Artifact, error)
	Install(context.Context, releasebundle.Manifest) (installation.Activation, error)
}

type desktopInstaller interface {
	Inspect(releasebundle.Manifest) (desktopinstall.Plan, error)
	Install(context.Context, releasebundle.Manifest) (string, error)
}

type nvidiaInstaller interface {
	Inspect(context.Context, releasebundle.Manifest) (nvidiainstall.Plan, error)
	Ensure(context.Context, releasebundle.Manifest) (nvidiainstall.Status, error)
}

// updateCoordinator is the same project and host transaction used by the CLI.
// Setup supplies an already signature-verified manifest.
type updateCoordinator interface {
	Inspect(context.Context, releasebundle.Manifest) (updateflow.Plan, error)
	Apply(context.Context, releasebundle.Manifest) (updateflow.Plan, error)
	Rollback(context.Context) (updateflow.Plan, error)
}

// Manager wires the production installers while keeping the coordinator
// independently testable.
type Manager struct {
	Home             string
	ManifestSource   string
	Runtime          runtimeInstaller
	Core             coreInstaller
	Desktop          desktopInstaller
	NVIDIA           nvidiaInstaller
	Distribution     inference.Distribution
	ProbeHost        func(context.Context) (windowssetup.Status, error)
	ProbeNVIDIA      func(context.Context) (nvidiainstall.GPU, error)
	InspectInference func(context.Context, inference.Distribution) (imageInstalled bool, modelInstalled bool, err error)
	EnableWSL        func(context.Context) (bool, error)
	RegisterResume   func() error
	Run              func(context.Context, string, ...string) ([]byte, error)
	NewUpdate        func(string) updateCoordinator
	StopDaemon       func(string) error
	StartDaemon      func(string) error
	Progress         Progress
}

// NewManager returns the complete production setup transaction.
func NewManager(home, manifestSource string) *Manager {
	manager := &Manager{
		Home: home, ManifestSource: manifestSource,
		Runtime: runtimeinstall.NewManager(home), Core: installation.NewManager(home), Desktop: desktopinstall.NewManager(home),
		NVIDIA: nvidiainstall.NewManager(), Distribution: inference.DistributionCPU,
		ProbeHost: windowssetup.Probe, EnableWSL: windowssetup.EnableWSL, RegisterResume: windowssetup.RegisterResume,
		ProbeNVIDIA: nvidiainstall.ProbeHost,
		Run:         run, StopDaemon: daemonstate.Stop, StartDaemon: daemonstate.Start,
	}
	manager.InspectInference = func(ctx context.Context, distribution inference.Distribution) (bool, bool, error) {
		shared, err := inference.NewManagerForDistribution(containerruntime.Runner{}, distribution)
		if err != nil {
			return false, false, err
		}
		return shared.ImageAvailable(ctx), shared.ModelAvailable(), nil
	}
	manager.NewUpdate = func(currentVersion string) updateCoordinator {
		update := updateflow.NewManager(home, currentVersion, manifestSource)
		update.Distribution = manager.Distribution
		return update
	}
	return manager
}

// DecideAction compares strict numeric product versions. Re-running the exact
// same immutable release is an explicit repair; an older package fails closed.
func DecideAction(currentVersion, targetVersion string) (Action, error) {
	if currentVersion == "" {
		return ActionInstall, nil
	}
	if currentVersion == targetVersion {
		return ActionRepair, nil
	}
	if releasebundle.VersionAtLeast(targetVersion, currentVersion) {
		return ActionUpgrade, nil
	}
	if releasebundle.VersionAtLeast(currentVersion, targetVersion) {
		return "", fmt.Errorf("setup %s cannot downgrade installed LCTK %s; use verified rollback instead", targetVersion, currentVersion)
	}
	return "", fmt.Errorf("installed LCTK version %q is invalid", currentVersion)
}

// Inspect completes every non-mutating prerequisite and component decision.
func (m *Manager) Inspect(ctx context.Context, manifest releasebundle.Manifest) (Plan, error) {
	if !m.Distribution.Valid() {
		return Plan{}, fmt.Errorf("setup selected unsupported inference distribution %q", m.Distribution)
	}
	host, err := m.ProbeHost(ctx)
	if err != nil {
		return Plan{Version: manifest.Version, Host: host}, err
	}
	runtimePlan, err := m.Runtime.Inspect(manifest)
	if err != nil {
		return Plan{}, err
	}
	corePlan, _, err := m.Core.Inspect(manifest)
	if err != nil {
		return Plan{}, err
	}
	desktopPlan, err := m.Desktop.Inspect(manifest)
	if err != nil {
		return Plan{}, err
	}
	if m.InspectInference == nil {
		return Plan{}, errors.New("setup inference inspection is incomplete")
	}
	imageInstalled, modelInstalled, err := m.InspectInference(ctx, m.Distribution)
	if err != nil {
		return Plan{}, err
	}
	selectedImage := manifest.InferenceImage
	if m.Distribution == inference.DistributionNVIDIAGPU {
		selectedImage = manifest.NVIDIAGPUInferenceImage
	}
	inferenceDownloadBytes := int64(0)
	inferenceRuntimeBytes := int64(0)
	if !imageInstalled {
		inferenceDownloadBytes += selectedImage.CompressedBytes
		inferenceRuntimeBytes = selectedImage.CompressedBytes + selectedImage.UnpackedBytes
	}
	modelDownloadBytes := int64(0)
	if !modelInstalled {
		modelDownloadBytes = manifest.EmbeddingModel.Bytes
		inferenceDownloadBytes += modelDownloadBytes
	}
	var gpu *nvidiainstall.GPU
	var nvidiaPlan nvidiainstall.Plan
	if m.Distribution == inference.DistributionNVIDIAGPU {
		if m.ProbeNVIDIA == nil || m.NVIDIA == nil {
			return Plan{}, errors.New("setup NVIDIA validation is incomplete")
		}
		found, err := m.ProbeNVIDIA(ctx)
		if err != nil {
			return Plan{Version: manifest.Version, Host: host, InferenceDistribution: m.Distribution}, err
		}
		gpu = &found
		nvidiaPlan, err = m.NVIDIA.Inspect(ctx, manifest)
		if err != nil {
			return Plan{}, err
		}
	}
	action, err := DecideAction(corePlan.CurrentVersion, manifest.Version)
	if err != nil {
		return Plan{}, err
	}
	var upgradePlan updateflow.Plan
	if action == ActionUpgrade {
		if m.NewUpdate == nil {
			return Plan{}, errors.New("setup update coordinator is missing")
		}
		upgradePlan, err = m.NewUpdate(corePlan.CurrentVersion).Inspect(ctx, manifest)
		if err != nil {
			return Plan{}, err
		}
	}
	ready := host.Supported && !host.RequiresEnablement && runtimePlan.Ready && corePlan.Ready
	// The model installer retains the verified file only after a temporary
	// download has completed, so reserve both copies in the installation home.
	if modelDownloadBytes > 0 && corePlan.AvailableBytes < uint64(corePlan.RequiredBytes+modelDownloadBytes*2) {
		ready = false
	}
	if action == ActionUpgrade {
		ready = ready && upgradePlan.Ready
	}
	return Plan{
		Action: action, CurrentVersion: corePlan.CurrentVersion, Version: manifest.Version,
		Host: host, Runtime: runtimePlan, Core: corePlan, Desktop: desktopPlan, Upgrade: upgradePlan,
		InferenceDistribution: m.Distribution, GPU: gpu, NVIDIA: nvidiaPlan,
		InferenceImageInstalled: imageInstalled, InferenceModelInstalled: modelInstalled,
		InferenceDownloadBytes: inferenceDownloadBytes, InferenceRuntimeBytes: inferenceRuntimeBytes,
		DownloadBytes: runtimePlan.DownloadBytes + corePlan.DownloadBytes + desktopPlan.DownloadBytes + nvidiaPlan.DownloadBytes + inferenceDownloadBytes,
		Ready:         ready,
	}, nil
}

// Install applies the accepted plan in dependency order and runs the existing
// signed bootstrap self-test before exposing the desktop launcher.
func (m *Manager) Install(ctx context.Context, manifest releasebundle.Manifest) (installErr error) {
	plan, err := m.Inspect(ctx, manifest)
	if err != nil {
		return err
	}
	if plan.Host.RequiresEnablement {
		m.report("windows", "Enabling WSL2 prerequisites")
		reboot, err := m.EnableWSL(ctx)
		if err != nil {
			return err
		}
		if reboot {
			if err := m.RegisterResume(); err != nil {
				return err
			}
			return ErrRebootRequired
		}
	}
	if !plan.Runtime.Ready || !plan.Core.Ready || plan.Action == ActionUpgrade && !plan.Upgrade.Ready {
		return errors.New("setup plan does not have enough disk space")
	}
	if m.StopDaemon == nil || m.StartDaemon == nil {
		return errors.New("setup daemon lifecycle is incomplete")
	}
	daemonStopped := false
	if plan.Action != ActionInstall {
		m.report("daemon", "Stopping the installed LCTK background service")
		if err := m.StopDaemon(m.Home); err != nil {
			return err
		}
		daemonStopped = true
	}
	var updater updateCoordinator
	upgradeApplied := false
	defer func() {
		if installErr == nil {
			return
		}
		if upgradeApplied && updater != nil {
			// Cancellation of the forward transaction must not cancel recovery.
			// Rollback gets its own bounded context so a timed-out download or
			// health gate cannot strand migrated projects or host activation.
			rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
			_, rollbackErr := updater.Rollback(rollbackCtx)
			cancelRollback()
			installErr = errors.Join(installErr, rollbackErr)
		}
		if daemonStopped {
			installErr = errors.Join(installErr, m.StartDaemon(m.Home))
		}
	}()
	if plan.Action == ActionUpgrade {
		m.report("update", fmt.Sprintf("Updating LCTK %s to %s and checking running projects", plan.CurrentVersion, plan.Version))
		updater = m.NewUpdate(plan.CurrentVersion)
		if _, err := updater.Apply(ctx, manifest); err != nil {
			return err
		}
		upgradeApplied = true
	}
	m.report("runtime", "Installing the verified private Podman runtime")
	if err := m.Runtime.Install(ctx, manifest); err != nil {
		return err
	}
	if m.Distribution == inference.DistributionNVIDIAGPU {
		m.report("nvidia", "Installing and verifying NVIDIA WSL CDI support")
		if _, err := m.NVIDIA.Ensure(ctx, manifest); err != nil {
			return err
		}
	}
	if plan.Action != ActionUpgrade {
		m.report("core", "Installing and verifying the LCTK host core")
		if _, err := m.Core.Install(ctx, manifest); err != nil {
			return err
		}
	}
	executable, _, err := installation.ActiveExecutable(m.Home)
	if err != nil {
		return err
	}
	m.report("components", "Installing container images and the embedding model")
	bootstrapCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	output, err := m.Run(bootstrapCtx, executable, "bootstrap", "--manifest", m.ManifestSource,
		"--inference-distribution", string(m.Distribution), "--yes", "--json")
	if err != nil {
		return fmt.Errorf("bootstrap signed runtime components: %s: %w", string(output), err)
	}
	m.report("desktop", "Registering LCTK at sign-in and in the Start menu")
	if _, err := m.Desktop.Install(ctx, manifest); err != nil {
		return err
	}
	if err := m.StartDaemon(m.Home); err != nil {
		return err
	}
	daemonStopped = false
	m.report("complete", "LCTK is installed and ready")
	return nil
}

func (m *Manager) report(phase, detail string) {
	if m.Progress != nil {
		m.Progress(phase, detail)
	}
}

func run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	windowsprocess.HideConsole(command)
	return command.CombinedOutput()
}
