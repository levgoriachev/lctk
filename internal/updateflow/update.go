// Package updateflow owns the signed in-place update transaction shared by the
// CLI and the native Windows setup. Keeping one coordinator prevents the GUI
// installer from bypassing project health gates or host-core rollback.
package updateflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

const (
	// CurrentProjectSchema is the persistent project schema understood by this
	// host generation and checked before any candidate component is installed.
	CurrentProjectSchema = 2
	// DefaultStartWait is the bounded candidate health gate used for every
	// project that was running immediately before an update or rollback.
	DefaultStartWait = 90 * time.Second
	// RecoveryTimeout keeps rollback independent from a cancelled forward
	// transaction while still bounding every recovery path.
	RecoveryTimeout = 10 * time.Minute
)

// Project records the only lifecycle fact an update needs to preserve.
type Project struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
}

// Plan is read-only evidence for one complete signed update transaction.
type Plan struct {
	CurrentVersion        string                 `json:"current_version"`
	TargetVersion         string                 `json:"target_version"`
	ManifestSource        string                 `json:"manifest_source"`
	SignatureValid        bool                   `json:"signature_valid"`
	Writes                bool                   `json:"writes"`
	Ready                 bool                   `json:"ready"`
	Host                  installation.Plan      `json:"host"`
	Projects              []Project              `json:"projects"`
	Applied               bool                   `json:"applied"`
	RolledBack            bool                   `json:"rolled_back"`
	InferenceDistribution inference.Distribution `json:"inference_distribution"`
	GPU                   *nvidiainstall.GPU     `json:"gpu,omitempty"`
}

// Stack is the version-pinned project runtime boundary used during update.
type Stack interface {
	RuntimeAvailable(context.Context) error
	InstallImage(context.Context, string, string) error
	InstallImageArchive(context.Context, string, string, releasebundle.Artifact) error
	Status(context.Context, projectregistry.Project) (projectstack.Status, error)
	Start(context.Context, projectregistry.Project, time.Duration) (projectstack.Status, error)
	Stop(context.Context, projectregistry.Project) (projectstack.Status, error)
	RestoreSchemaRollback(context.Context, projectregistry.Project, string) error
}

// Installer is the atomic versioned host-core activation boundary.
type Installer interface {
	Inspect(releasebundle.Manifest) (installation.Plan, releasebundle.Artifact, error)
	Install(context.Context, releasebundle.Manifest) (installation.Activation, error)
	VerifyRollback() (installation.Activation, error)
	Rollback() (installation.Activation, error)
}

// Inference is the pinned installation-wide backend boundary staged before any
// project stop or host activation during an update.
type Inference interface {
	ImageAvailable(context.Context) bool
	ModelAvailable() bool
	PullImage(context.Context) error
	InstallModel(context.Context, *http.Client) error
	Ensure(context.Context, time.Duration) (inference.Status, error)
	SelfTest(context.Context) error
}

type NVIDIA interface {
	Inspect(context.Context, releasebundle.Manifest) (nvidiainstall.Plan, error)
	Ensure(context.Context, releasebundle.Manifest) (nvidiainstall.Status, error)
}

// Manager injects every mutable boundary so the same transaction can serve
// the CLI, native setup, and deterministic unit tests.
type Manager struct {
	Home           string
	CurrentVersion string
	ManifestSource string
	Installer      Installer
	LoadRegistry   func() (*projectregistry.Registry, error)
	NewStack       func(string, inference.Distribution) Stack
	ProjectSchema  int
	CandidateWait  time.Duration
	Distribution   inference.Distribution
	NewInference   func(inference.Distribution) (Inference, error)
	NVIDIA         NVIDIA
	ProbeNVIDIA    func(context.Context) (nvidiainstall.GPU, error)
	LoadSelection  func() (inference.Selection, error)
	SaveSelection  func(inference.Selection) error
}

// NewManager returns the production update coordinator for an already verified
// manifest. Signature loading remains with the calling CLI or setup surface.
func NewManager(home, currentVersion, manifestSource string) *Manager {
	return &Manager{
		Home: home, CurrentVersion: currentVersion, ManifestSource: manifestSource,
		Installer: installation.NewManager(home), LoadRegistry: projectregistry.Load,
		NewStack: func(version string, distribution inference.Distribution) Stack {
			return projectstack.NewManager().WithVersion(version).WithInferenceDistribution(distribution)
		},
		ProjectSchema: CurrentProjectSchema, CandidateWait: DefaultStartWait,
		NewInference: func(distribution inference.Distribution) (Inference, error) {
			return inference.NewManagerForDistribution(containerruntime.Runner{}, distribution)
		},
		NVIDIA: nvidiainstall.NewManager(), ProbeNVIDIA: nvidiainstall.ProbeHost,
		LoadSelection: inference.LoadSelection, SaveSelection: inference.SaveSelection,
	}
}

// Inspect validates compatibility, inventories projects, and calculates host
// disk requirements without writing state or pulling a candidate image.
func (m *Manager) Inspect(ctx context.Context, manifest releasebundle.Manifest) (Plan, error) {
	if err := m.validate(manifest); err != nil {
		return Plan{}, err
	}
	hostPlan, _, err := m.Installer.Inspect(manifest)
	if err != nil {
		return Plan{}, err
	}
	registry, err := m.LoadRegistry()
	if err != nil {
		return Plan{}, err
	}
	distribution, err := m.resolveDistribution()
	if err != nil {
		return Plan{}, err
	}
	currentStack := m.NewStack(m.CurrentVersion, distribution)
	if err := currentStack.RuntimeAvailable(ctx); err != nil {
		return Plan{}, err
	}
	var gpu *nvidiainstall.GPU
	if distribution == inference.DistributionNVIDIAGPU {
		found, err := m.ProbeNVIDIA(ctx)
		if err != nil {
			return Plan{}, err
		}
		gpu = &found
		if _, err := m.NVIDIA.Inspect(ctx, manifest); err != nil {
			return Plan{}, err
		}
	}
	projects, _, err := InventoryProjects(ctx, currentStack, registry.List())
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		CurrentVersion: m.CurrentVersion, TargetVersion: manifest.Version,
		ManifestSource: m.ManifestSource, SignatureValid: true, Writes: false,
		Ready: hostPlan.Ready, Host: hostPlan, Projects: projects,
		InferenceDistribution: distribution, GPU: gpu,
	}, nil
}

// Apply repeats the read-only gates, migrates only previously running projects,
// and activates the verified host core last. Every migrated project is restored
// in reverse order when a candidate health gate or host activation fails.
func (m *Manager) Apply(ctx context.Context, manifest releasebundle.Manifest) (Plan, error) {
	plan, err := m.Inspect(ctx, manifest)
	if err != nil {
		return Plan{}, err
	}
	if !plan.Ready {
		return plan, fmt.Errorf("update requires %d bytes; only %d are available", plan.Host.RequiredBytes, plan.Host.AvailableBytes)
	}
	registry, err := m.LoadRegistry()
	if err != nil {
		return plan, err
	}
	currentStack := m.NewStack(m.CurrentVersion, plan.InferenceDistribution)
	_, running, err := InventoryProjects(ctx, currentStack, registry.List())
	if err != nil {
		return plan, err
	}
	targetStack := m.NewStack(manifest.Version, plan.InferenceDistribution)
	restoreInference, err := m.activateInference(ctx, manifest, plan.InferenceDistribution)
	if err != nil {
		return plan, err
	}
	fail := func(cause error) (Plan, error) {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
		defer cancel()
		return plan, errors.Join(cause, restoreInference(recoveryCtx))
	}
	archive, archiveErr := manifest.ArtifactFor("code-image-archive", "linux", "amd64")
	if archiveErr == nil {
		err = targetStack.InstallImageArchive(ctx, manifest.CodeImage.Reference, manifest.Version, archive)
	} else {
		err = targetStack.InstallImage(ctx, manifest.CodeImage.Reference, manifest.Version)
	}
	if err != nil {
		return fail(err)
	}
	migrated, err := SwitchRunningProjects(ctx, currentStack, targetStack, running, m.CandidateWait)
	if err != nil {
		return fail(errors.Join(err, RestoreProjects(ctx, targetStack, currentStack, migrated, m.CurrentVersion, m.CandidateWait)))
	}
	if _, err := m.Installer.Install(ctx, manifest); err != nil {
		return fail(errors.Join(err, RestoreProjects(ctx, targetStack, currentStack, migrated, m.CurrentVersion, m.CandidateWait)))
	}
	plan.Writes, plan.Applied, plan.Ready = true, true, true
	return plan, nil
}

// Rollback verifies the previous host before touching project state, restores
// all registered schema bundles, restarts only previously running projects,
// and changes host activation last.
func (m *Manager) Rollback(ctx context.Context) (Plan, error) {
	current, err := m.Installer.VerifyRollback()
	if err != nil {
		return Plan{}, err
	}
	rollbackDistribution, restoreInference, err := m.activateRollbackInference(ctx, current.PreviousVersion)
	if err != nil {
		return Plan{}, err
	}
	fail := func(cause error) (Plan, error) {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
		defer cancel()
		return Plan{}, errors.Join(cause, restoreInference(recoveryCtx))
	}
	registry, err := m.LoadRegistry()
	if err != nil {
		return fail(err)
	}
	currentStack := m.NewStack(current.ActiveVersion, rollbackDistribution)
	previousStack := m.NewStack(current.PreviousVersion, rollbackDistribution)
	projects := registry.List()
	_, running, err := InventoryProjects(ctx, currentStack, projects)
	if err != nil {
		return fail(err)
	}
	if err := RollbackRegisteredProjects(ctx, currentStack, previousStack, projects, running, current.PreviousVersion, m.CandidateWait); err != nil {
		return fail(err)
	}
	activation, err := m.Installer.Rollback()
	if err != nil {
		return fail(err)
	}
	return Plan{CurrentVersion: current.ActiveVersion, TargetVersion: activation.ActiveVersion,
		Writes: true, Ready: true, Applied: true, RolledBack: true}, nil
}

// activateRollbackInference gives a pre-0.1.12 host the CPU service it knows
// how to maintain while retaining the owner's newer selection document inertly.
// If later rollback work fails, the current selected backend is restored.
func (m *Manager) activateRollbackInference(ctx context.Context, previousVersion string) (inference.Distribution, func(context.Context) error, error) {
	noRestore := func(context.Context) error { return nil }
	if m.NewInference == nil || m.LoadSelection == nil {
		return "", nil, errors.New("rollback inference coordinator is incomplete")
	}
	selection, err := m.LoadSelection()
	if err != nil {
		return "", nil, err
	}
	if releasebundle.VersionAtLeast(previousVersion, "0.1.12") {
		return selection.Distribution, noRestore, nil
	}
	cpu, err := m.NewInference(inference.DistributionCPU)
	if err != nil {
		return "", nil, err
	}
	if !cpu.ImageAvailable(ctx) {
		return "", nil, errors.New("verified CPU inference image is unavailable for rollback")
	}
	if _, err := cpu.Ensure(ctx, 2*time.Minute); err != nil {
		return "", nil, err
	}
	if err := cpu.SelfTest(ctx); err != nil {
		return "", nil, err
	}
	return inference.DistributionCPU, func(rollbackCtx context.Context) error {
		if selection.Distribution == inference.DistributionCPU {
			return nil
		}
		selected, err := m.NewInference(selection.Distribution)
		if err != nil {
			return err
		}
		if _, err := selected.Ensure(rollbackCtx, 2*time.Minute); err != nil {
			return err
		}
		return selected.SelfTest(rollbackCtx)
	}, nil
}

func (m *Manager) resolveDistribution() (inference.Distribution, error) {
	if m.Distribution != "" {
		if !m.Distribution.Valid() {
			return "", fmt.Errorf("update selected unsupported inference distribution %q", m.Distribution)
		}
		return m.Distribution, nil
	}
	selection, err := m.LoadSelection()
	if err != nil {
		return "", err
	}
	return selection.Distribution, nil
}

// activateInference stages and proves the selected backend before a project is
// stopped. Its returned closure restores both runtime and persisted selection
// if any later project or host gate fails.
func (m *Manager) activateInference(ctx context.Context, manifest releasebundle.Manifest, target inference.Distribution) (func(context.Context) error, error) {
	previous, err := m.LoadSelection()
	if err != nil {
		return nil, err
	}
	restore := func(rollbackCtx context.Context) error {
		if previous.Distribution == target {
			return nil
		}
		manager, err := m.NewInference(previous.Distribution)
		if err != nil {
			return fmt.Errorf("restore previous inference distribution: %w", err)
		}
		if _, err := manager.Ensure(rollbackCtx, 2*time.Minute); err != nil {
			return fmt.Errorf("restore previous inference distribution: %w", err)
		}
		if err := manager.SelfTest(rollbackCtx); err != nil {
			return fmt.Errorf("self-test restored inference distribution: %w", err)
		}
		if err := m.SaveSelection(previous); err != nil {
			return fmt.Errorf("restore inference selection: %w", err)
		}
		return nil
	}
	if target == inference.DistributionNVIDIAGPU {
		if _, err := m.NVIDIA.Ensure(ctx, manifest); err != nil {
			return nil, err
		}
	}
	manager, err := m.NewInference(target)
	if err != nil {
		return nil, err
	}
	if !manager.ImageAvailable(ctx) {
		if err := manager.PullImage(ctx); err != nil {
			return nil, err
		}
	}
	if !manager.ModelAvailable() {
		if err := manager.InstallModel(ctx, nil); err != nil {
			return nil, err
		}
	}
	if _, err := manager.Ensure(ctx, 2*time.Minute); err != nil {
		return nil, err
	}
	if err := manager.SelfTest(ctx); err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
		defer cancel()
		return nil, errors.Join(err, restore(recoveryCtx))
	}
	selection := inference.Selection{SchemaVersion: inference.SelectionSchemaVersion, Distribution: target}
	if err := m.SaveSelection(selection); err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
		defer cancel()
		return nil, errors.Join(err, restore(recoveryCtx))
	}
	return restore, nil
}

// validate rejects stale, incompatible, or incomplete coordinator inputs before
// any runtime inventory can reach Podman.
func (m *Manager) validate(manifest releasebundle.Manifest) error {
	if m.Home == "" || m.CurrentVersion == "" || m.Installer == nil || m.LoadRegistry == nil || m.NewStack == nil ||
		m.NewInference == nil || m.LoadSelection == nil || m.SaveSelection == nil ||
		m.ProjectSchema < 1 || m.CandidateWait <= 0 {
		return errors.New("update coordinator is incomplete")
	}
	distribution, err := m.resolveDistribution()
	if err != nil {
		return err
	}
	if distribution == inference.DistributionNVIDIAGPU {
		if m.NVIDIA == nil || m.ProbeNVIDIA == nil {
			return errors.New("update NVIDIA validation is incomplete")
		}
		if _, err := nvidiainstall.ValidateManifest(manifest); err != nil {
			return err
		}
	}
	if !releasebundle.VersionAtLeast(m.CurrentVersion, manifest.MinimumHostVersion) {
		return fmt.Errorf("release %s requires host %s or newer", manifest.Version, manifest.MinimumHostVersion)
	}
	if manifest.Version == m.CurrentVersion || !releasebundle.VersionAtLeast(manifest.Version, m.CurrentVersion) {
		return fmt.Errorf("release %s is not newer than active host %s; use explicit rollback for downgrades", manifest.Version, m.CurrentVersion)
	}
	if m.ProjectSchema < manifest.ProjectSchemaFrom || m.ProjectSchema > manifest.ProjectSchemaTo {
		return fmt.Errorf("release %s does not accept project schema %d", manifest.Version, m.ProjectSchema)
	}
	if manifest.InferenceImage.Reference != inference.Image || manifest.EmbeddingModel.SHA256 != inference.ModelSHA256 || manifest.EmbeddingModel.Bytes != inference.ModelBytes {
		return errors.New("release manifest inference or model identity differs from this compatibility generation")
	}
	return nil
}

// InventoryProjects records which registered projects are running without
// starting or stopping any project.
func InventoryProjects(ctx context.Context, stack Stack, projects []projectregistry.Project) ([]Project, []projectregistry.Project, error) {
	result := make([]Project, 0, len(projects))
	running := make([]projectregistry.Project, 0, len(projects))
	for _, project := range projects {
		status, err := stack.Status(ctx, project)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect project %s before update: %w", project.ID, err)
		}
		isRunning := status.State == projectstack.StateRunning
		result = append(result, Project{ID: project.ID, Running: isRunning})
		if isRunning {
			running = append(running, project)
		}
	}
	return result, running, nil
}

// SwitchRunningProjects stops each old stack and accepts the candidate only
// after its bounded health gate succeeds.
func SwitchRunningProjects(ctx context.Context, current, target Stack, projects []projectregistry.Project, wait time.Duration) ([]projectregistry.Project, error) {
	migrated := make([]projectregistry.Project, 0, len(projects))
	for _, project := range projects {
		if _, err := current.Stop(ctx, project); err != nil {
			return migrated, fmt.Errorf("stop project %s for update: %w", project.ID, err)
		}
		migrated = append(migrated, project)
		if _, err := target.Start(ctx, project, wait); err != nil {
			return migrated, fmt.Errorf("candidate health gate failed for project %s: %w", project.ID, err)
		}
	}
	return migrated, nil
}

// RestoreProjects reverses a failed candidate migration and retains every
// individual failure so recovery never silently stops halfway.
func RestoreProjects(ctx context.Context, candidate, previous Stack, projects []projectregistry.Project, previousVersion string, wait time.Duration) error {
	var failures []error
	for index := len(projects) - 1; index >= 0; index-- {
		project := projects[index]
		if _, err := candidate.Stop(ctx, project); err != nil {
			failures = append(failures, fmt.Errorf("stop failed candidate %s: %w", project.ID, err))
			continue
		}
		if err := candidate.RestoreSchemaRollback(ctx, project, previousVersion); err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err := previous.Start(ctx, project, wait); err != nil {
			failures = append(failures, fmt.Errorf("restore project %s: %w", project.ID, err))
		}
	}
	return errors.Join(failures...)
}

// RollbackRegisteredProjects restores every registered schema bundle but starts
// only the projects that were running before rollback began.
func RollbackRegisteredProjects(ctx context.Context, current, previous Stack, projects, running []projectregistry.Project, previousVersion string, wait time.Duration) error {
	runningIDs := make(map[string]bool, len(running))
	var failures []error
	for _, project := range running {
		runningIDs[project.ID] = true
		if _, err := current.Stop(ctx, project); err != nil {
			failures = append(failures, fmt.Errorf("stop project %s before rollback: %w", project.ID, err))
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	for _, project := range projects {
		if err := current.RestoreSchemaRollback(ctx, project, previousVersion); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	for _, project := range projects {
		if runningIDs[project.ID] {
			if _, err := previous.Start(ctx, project, wait); err != nil {
				failures = append(failures, fmt.Errorf("restart project %s after rollback: %w", project.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}
