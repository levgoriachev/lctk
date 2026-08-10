package projectstack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/machinetunnel"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/verifieddownload"
)

// State is the observable lifecycle state of a project stack.
//
// The vocabulary comes from docs/project-lifecycle.md. It is deliberately coarse:
// an agent consuming this needs to know whether to retry, wait, or give up, not
// the internal details of the container runtime.
type State string

const (
	// StateStopped means the project is registered and nothing is running.
	StateStopped State = "stopped"
	// StateStarting means containers exist but health has not been confirmed.
	StateStarting State = "starting"
	// StateRunning means the service is up and reporting healthy.
	StateRunning State = "running"
	// StateError means containers exist in a failed or unexpected condition.
	StateError State = "error"
	// StateUnknown means the runtime could not be queried at all.
	StateUnknown State = "unknown"
)

// Retryable reports whether waiting and asking again is worthwhile. It exists so
// a typed error can carry the same advice to a calling agent.
func (s State) Retryable() bool { return s == StateStarting }

// Errors a caller is expected to distinguish.
var (
	// ErrRuntimeUnavailable reports that the container runtime cannot be reached.
	// It is separate from a project failure so a caller does not blame the
	// project for the managed WSL runtime being unavailable.
	ErrRuntimeUnavailable = errors.New("container runtime is unavailable")
	// ErrLinuxContainersRequired reports a reachable runtime that does not expose
	// the Linux machine required by the selected backends.
	//
	// LCTK project stacks are Linux containers by design: ADR-0011 requires a
	// Linux boundary for the search backend. Without this check the failure
	// surfaces much later as an opaque image-manifest error.
	ErrLinuxContainersRequired = errors.New("container runtime must be able to run Linux containers")
	// ErrImageMissing reports that the reusable image has not been built.
	ErrImageMissing = errors.New("reusable code-intel image is not available")
)

// Status is the runtime view of one project stack.
type Status struct {
	ProjectID string `json:"project_id"`
	State     State  `json:"state"`
	Health    string `json:"health,omitempty"`
	Container string `json:"container,omitempty"`
	Image     string `json:"image"`
	Network   string `json:"network"`
	Volume    string `json:"volume"`
	// ServiceAddress is the process-owned loopback tunnel for the project's
	// code-intel service, empty when the project is not running. Its dynamic port
	// changes across daemon restarts and must be read rather than remembered.
	ServiceAddress string `json:"service_address,omitempty"`
	// Detail explains an error or transitional state in one line.
	Detail string `json:"detail,omitempty"`
}

// parseInspect splits the delimited inspect output, tolerating the older
// whitespace-separated form so a container started by a previous build is still
// readable rather than reported as unparseable.
func parseInspect(stdout string) []string {
	trimmed := strings.TrimSpace(stdout)
	if strings.Contains(trimmed, "|") {
		return strings.Split(trimmed, "|")
	}
	return strings.Fields(trimmed)
}

// Runner executes container-runtime commands. It is an interface so lifecycle
// logic is verified without mutating a real managed machine.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout string, stderr string, err error)
}

type inferenceLifecycle interface {
	Ensure(context.Context, time.Duration) (inference.Status, error)
}

type serviceTunnel interface {
	Ensure(context.Context, string, string) (string, error)
	Close(string)
}

// Manager drives project stacks.
type Manager struct {
	runner Runner
	// inference is shared stateless compute and is ensured before any project
	// receives its endpoint. It is nil only in narrow lifecycle unit tests that
	// intentionally exercise container lifecycle behavior in isolation.
	sharedInference inferenceLifecycle
	inferenceErr    error
	tunnel          serviceTunnel
	// settings resolves the machine's background-load policy. It is a function so
	// a change takes effect on the next start without restarting the daemon, and
	// so a test can drive a policy without writing a file.
	settings func() (hostsettings.Settings, error)
	// version selects the code-intel tag. Production defaults to the host build;
	// update sets a candidate explicitly before its health gate.
	version string
	// imageClient and loadImageArchive form the authenticated local-RC image
	// path. Production releases keep using immutable registry pulls.
	imageClient      *http.Client
	loadImageArchive func(context.Context, io.Reader) (string, string, error)
}

// NewManager returns a manager backed by LCTK's private Podman connection.
func NewManager() *Manager {
	runner := containerruntime.Runner{}
	shared, err := inference.NewManager(runner)
	return &Manager{runner: runner, sharedInference: shared, inferenceErr: err, tunnel: machinetunnel.Default,
		settings: hostsettings.Load, version: buildinfo.Version, imageClient: http.DefaultClient,
		loadImageArchive: containerruntime.Load}
}

// NewManagerWithRunner returns a manager backed by a supplied runner, for tests.
func NewManagerWithRunner(runner Runner) *Manager {
	return &Manager{runner: runner, settings: hostsettings.Load, version: buildinfo.Version}
}

// WithVersion selects the candidate code-intel tag for transactional update.
func (m *Manager) WithVersion(version string) *Manager {
	m.version = version
	return m
}

// WithInferenceDistribution binds the backend already selected by an update
// transaction. Reading inference.json again while switching project versions
// would let a rollback to a pre-selection host reactivate the owner's newer GPU
// choice after the coordinator had deliberately staged CPU compatibility.
func (m *Manager) WithInferenceDistribution(distribution inference.Distribution) *Manager {
	m.sharedInference, m.inferenceErr = inference.NewManagerForDistribution(m.runner, distribution)
	return m
}

func (m *Manager) names(projectID string) (Names, error) {
	return DeriveNamesForVersion(projectID, m.version)
}

// WithInference injects the shared lifecycle for tests that verify ordering.
// Production uses NewManager, which always configures the pinned implementation.
func (m *Manager) WithInference(shared inferenceLifecycle) *Manager {
	m.sharedInference = shared
	m.inferenceErr = nil
	return m
}

// WithSettings replaces the policy source, for tests and for callers that have
// already resolved it.
func (m *Manager) WithSettings(settings func() (hostsettings.Settings, error)) *Manager {
	m.settings = settings
	return m
}

// Budget resolves what this project is allowed to cost: the machine policy, with
// the project's own mode layered on top when it has one.
func (m *Manager) Budget(project projectregistry.Project) hostsettings.Budget {
	load := hostsettings.Defaults
	if m.settings != nil {
		if resolved, err := m.settings(); err == nil {
			load = resolved
		}
	}
	return load.Resources.WithProjectMode(hostsettings.Mode(project.ResourceMode)).Budget()
}

// RuntimeAvailable reports whether LCTK's explicit Podman connection answers
// and is backed by the Linux machine required by code intelligence.
func (m *Manager) RuntimeAvailable(ctx context.Context) error {
	stdout, stderr, err := m.runner.Run(ctx, "info", "--format", "json")
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRuntimeUnavailable, firstLine(stderr, err))
	}
	var identity struct {
		Host struct {
			OS string `json:"os"`
		} `json:"host"`
	}
	if err := json.Unmarshal([]byte(stdout), &identity); err != nil || identity.Host.OS == "" {
		return fmt.Errorf("%w: Podman returned invalid host identity", ErrRuntimeUnavailable)
	}
	if serverOS := strings.ToLower(identity.Host.OS); serverOS != "linux" {
		return fmt.Errorf("%w: the managed runtime reports %q", ErrLinuxContainersRequired, serverOS)
	}
	return nil
}

// ImageAvailable reports whether the reusable image exists locally.
func (m *Manager) ImageAvailable(ctx context.Context, image string) error {
	_, found, err := m.inspectImageID(ctx, image)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrImageMissing, image)
	}
	return nil
}

// ImageMatches proves that the product version tag resolves to the exact
// signed digest selected by a release manifest. Existence of the tag alone is
// insufficient because local tags are mutable Docker state.
func (m *Manager) ImageMatches(ctx context.Context, tag, immutableReference string) (bool, error) {
	tagID, tagFound, err := m.inspectImageID(ctx, tag)
	if err != nil || !tagFound {
		return false, err
	}
	referenceID, referenceFound, err := m.inspectImageID(ctx, immutableReference)
	if err != nil || !referenceFound {
		return false, err
	}
	return tagID == referenceID, nil
}

func (m *Manager) inspectImageID(ctx context.Context, image string) (string, bool, error) {
	stdout, stderr, err := m.runner.Run(ctx, "image", "inspect", image, "--format", "{{.Id}}")
	if err != nil {
		if isNoSuchImage(stdout, stderr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect image %s: %s", image, firstLine(stderr, err))
	}
	id := strings.TrimSpace(stdout)
	if id == "" {
		return "", false, fmt.Errorf("inspect image %s returned no identity", image)
	}
	return id, true, nil
}

// InstallImage pulls an immutable OCI reference, then assigns the local unified
// version tag consumed by the project plan. The tag is derived state; the signed digest
// remains the authority checked by the pull.
func (m *Manager) InstallImage(ctx context.Context, immutableReference, version string) error {
	if immutableReference == "" || version == "" {
		return errors.New("code-intel image reference or version is empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, stderr, err := m.runner.Run(ctx, "pull", immutableReference); err != nil {
		return fmt.Errorf("pull code-intel image: %s", firstLine(stderr, err))
	}
	tag := ImageRepository + ":" + version
	if _, stderr, err := m.runner.Run(ctx, "tag", immutableReference, tag); err != nil {
		return fmt.Errorf("tag code-intel image %s: %s", tag, firstLine(stderr, err))
	}
	return nil
}

// InstallImageArchive downloads the optional signed local-RC image artifact,
// streams it into the private Podman machine, and verifies the loaded manifest
// digest before the product version tag can be used. The archive SHA-256 binds
// transport bytes; the manifest digest independently binds runnable image content.
func (m *Manager) InstallImageArchive(ctx context.Context, immutableReference, version string, artifact releasebundle.Artifact) error {
	if immutableReference == "" || version == "" || artifact.Kind != "code-image-archive" || artifact.OS != "linux" || artifact.Arch != "amd64" {
		return errors.New("local code-intel image identity is incomplete")
	}
	if artifact.Name == "" || filepath.Base(artifact.Name) != artifact.Name || strings.ContainsAny(artifact.Name, `/\`) {
		return errors.New("local code-intel image archive name is unsafe")
	}
	separator := strings.LastIndex(immutableReference, "@")
	if separator < 0 {
		return errors.New("local code-intel image reference has no sha256 digest")
	}
	expectedDigest, found := strings.CutPrefix(immutableReference[separator+1:], "sha256:")
	decodedDigest, digestErr := hex.DecodeString(expectedDigest)
	if !found || digestErr != nil || len(decodedDigest) != sha256.Size || expectedDigest != strings.ToLower(expectedDigest) {
		return errors.New("local code-intel image reference has no sha256 digest")
	}
	if m.imageClient == nil || m.loadImageArchive == nil {
		return errors.New("local code-intel image loader is not configured")
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return err
	}
	workspace, err := os.MkdirTemp(home, ".code-image-")
	if err != nil {
		return fmt.Errorf("create local image workspace: %w", err)
	}
	defer os.RemoveAll(workspace)
	archive := filepath.Join(workspace, artifact.Name)
	if err := verifieddownload.Download(ctx, m.imageClient, artifact, archive); err != nil {
		return err
	}
	input, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open verified code-intel image: %w", err)
	}
	_, stderr, loadErr := m.loadImageArchive(ctx, input)
	closeErr := input.Close()
	if loadErr != nil {
		return errors.Join(fmt.Errorf("load verified code-intel image: %s", firstLine(stderr, loadErr)), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close verified code-intel image: %w", closeErr)
	}
	tag := ImageRepository + ":" + version
	stdout, stderr, err := m.runner.Run(ctx, "image", "inspect", tag, "--format", "{{.Digest}}")
	if err != nil {
		return fmt.Errorf("inspect loaded code-intel image %s: %s", tag, firstLine(stderr, err))
	}
	if actual := strings.TrimPrefix(strings.TrimSpace(stdout), "sha256:"); actual != expectedDigest {
		return fmt.Errorf("loaded code-intel image digest sha256:%s differs from signed sha256:%s", actual, expectedDigest)
	}
	return nil
}

// RestoreSchemaRollback swaps the fixed semantic database rollback bundle back
// into place inside one project volume. It is invoked only after the candidate
// container has been stopped; source remains unmounted and the helper has no
// network access.
func (m *Manager) RestoreSchemaRollback(ctx context.Context, project projectregistry.Project, version string) error {
	names, err := DeriveNamesForVersion(project.ID, version)
	if err != nil {
		return err
	}
	script := `set -eu
db=/state/semantic/semantic.db
rollback=${db}.rollback-v1
failed=${db}.failed-update
test -f "$rollback" || exit 0
# A hardlink preserves the displaced candidate without copying it. The final
# rename replaces semantic.db atomically, so a crash sees either the complete
# current database or the complete rollback database and never a missing name.
ln -f "$db" "$failed"
mv -f "$rollback" "$db"`
	args := []string{
		"run", "--rm", "--network", "none", "--entrypoint", "/bin/sh",
		"--mount", "type=volume,source=" + names.Volume + ",target=/state",
		names.Image, "-c", script,
	}
	if _, stderr, err := m.runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("restore schema rollback for %s: %s", project.ID, firstLine(stderr, err))
	}
	return nil
}

// BuildImage builds the reusable image from a build context directory.
//
// The image is shared by every project, so it is built once per product version
// rather than per project.
func (m *Manager) BuildImage(ctx context.Context, image, contextDir string) error {
	_, stderr, err := m.runner.Run(ctx, "build", "--tag", image, contextDir)
	if err != nil {
		return fmt.Errorf("build %s: %s", image, firstLine(stderr, err))
	}
	return nil
}

// Start writes the deterministic runtime plan, creates isolated resources, and
// starts the one project service before waiting for health.
func (m *Manager) Start(ctx context.Context, project projectregistry.Project, wait time.Duration) (Status, error) {
	names, err := m.names(project.ID)
	if err != nil {
		return Status{}, err
	}
	if err := m.RuntimeAvailable(ctx); err != nil {
		return Status{}, err
	}
	if err := m.ImageAvailable(ctx, names.Image); err != nil {
		return Status{}, err
	}
	if m.inferenceErr != nil {
		return Status{}, m.inferenceErr
	}
	if m.sharedInference != nil {
		if _, err := m.sharedInference.Ensure(ctx, wait); err != nil {
			return Status{}, err
		}
	}

	plan, err := BuildRuntimePlanForVersion(project, m.Budget(project), m.version)
	if err != nil {
		return Status{}, err
	}
	if _, err := WriteRuntimePlan(plan); err != nil {
		return Status{}, err
	}
	if err := m.ensureNetwork(ctx, plan); err != nil {
		return Status{}, err
	}
	if m.sharedInference != nil {
		if err := m.ensureInferenceAttachment(ctx, plan); err != nil {
			return Status{}, err
		}
	}
	if err := m.ensureVolume(ctx, plan); err != nil {
		return Status{}, err
	}
	if _, stderr, err := m.runner.Run(ctx, plan.Arguments()...); err != nil {
		return Status{}, fmt.Errorf("start %s: %s", project.ID, firstLine(stderr, err))
	}

	return m.waitForHealth(ctx, names, wait)
}

// Stop takes the stack down while preserving the project volume.
//
// This is the stop of docs/project-lifecycle.md, not purge: containers and the
// network go away, persistent state does not.
func (m *Manager) Stop(ctx context.Context, project projectregistry.Project) (Status, error) {
	names, err := m.names(project.ID)
	if err != nil {
		return Status{}, err
	}
	if err := m.RuntimeAvailable(ctx); err != nil {
		return Status{}, err
	}

	// Removing the service and its private network deliberately leaves the named
	// state volume intact. Purge remains a separate destructive operation.
	if _, stderr, err := m.runner.Run(ctx, "rm", "--force", names.ContainerName); err != nil && !isNoSuchContainer("", stderr) {
		return Status{}, fmt.Errorf("stop %s container: %s", project.ID, firstLine(stderr, err))
	}
	if m.tunnel != nil {
		m.tunnel.Close(project.ID)
	}
	if _, stderr, err := m.runner.Run(ctx, "network", "disconnect", "--force", names.Network, inference.ContainerName); err != nil && !isNotConnected("", stderr) {
		return Status{}, fmt.Errorf("disconnect inference from %s: %s", project.ID, firstLine(stderr, err))
	}
	if _, stderr, err := m.runner.Run(ctx, "network", "rm", names.Network); err != nil && !isNoSuchNetwork("", stderr) {
		return Status{}, fmt.Errorf("stop %s network: %s", project.ID, firstLine(stderr, err))
	}

	return Status{
		ProjectID: project.ID,
		State:     StateStopped,
		Image:     names.Image,
		Network:   names.Network,
		Volume:    names.Volume,
	}, nil
}

// Restart stops and starts the stack, preserving the project volume.
func (m *Manager) Restart(ctx context.Context, project projectregistry.Project, wait time.Duration) (Status, error) {
	if _, err := m.Stop(ctx, project); err != nil {
		return Status{}, err
	}
	return m.Start(ctx, project, wait)
}

// Status reports the current runtime state without changing it.
func (m *Manager) Status(ctx context.Context, project projectregistry.Project) (Status, error) {
	names, err := m.names(project.ID)
	if err != nil {
		return Status{}, err
	}

	status := Status{
		ProjectID: project.ID,
		State:     StateUnknown,
		Image:     names.Image,
		Network:   names.Network,
		Volume:    names.Volume,
	}

	if err := m.RuntimeAvailable(ctx); err != nil {
		status.Detail = err.Error()
		return status, err
	}
	return m.inspect(ctx, names)
}

// inspect derives the lifecycle state from the container's own reported state and
// health. A container that does not exist means stopped, which is the normal
// resting state of a registered project.
func (m *Manager) inspect(ctx context.Context, names Names) (Status, error) {
	status := Status{
		ProjectID: names.ProjectID,
		Image:     names.Image,
		Network:   names.Network,
		Volume:    names.Volume,
		Container: names.ContainerName,
	}

	// One inspect answers three questions, so a search does not cost an extra
	// runtime call to find the service. Fields are separated explicitly rather
	// than by whitespace because health and the private address are both allowed
	// to be empty, and positional parsing would silently shift.
	format := "{{.State.Status}}|" +
		"{{if .State.Health}}{{.State.Health.Status}}{{end}}|" +
		fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, names.Network)
	stdout, stderr, err := m.runner.Run(ctx, "inspect", names.ContainerName, "--format", format)
	if err != nil {
		if isNoSuchContainer(stdout, stderr) {
			status.State = StateStopped
			status.Container = ""
			return status, nil
		}
		status.State = StateUnknown
		status.Detail = firstLine(stderr, err)
		return status, fmt.Errorf("inspect %s: %s", names.ContainerName, status.Detail)
	}

	fields := parseInspect(stdout)
	containerState := ""
	if len(fields) > 0 {
		containerState = fields[0]
	}
	if len(fields) > 1 {
		status.Health = fields[1]
	}
	if len(fields) > 2 && fields[2] != "" {
		remote := net.JoinHostPort(fields[2], strconv.Itoa(ServicePort))
		if m.tunnel == nil {
			status.ServiceAddress = remote
		} else {
			local, tunnelErr := m.tunnel.Ensure(ctx, names.ProjectID, remote)
			if tunnelErr != nil {
				status.State = StateUnknown
				status.Detail = tunnelErr.Error()
				return status, fmt.Errorf("open project %s machine tunnel: %w", names.ProjectID, tunnelErr)
			}
			status.ServiceAddress = local
		}
	}

	switch containerState {
	case "running":
		switch status.Health {
		case "healthy":
			status.State = StateRunning
		case "starting", "":
			status.State = StateStarting
			status.Detail = "container is up, health not yet confirmed"
		case "unhealthy":
			status.State = StateError
			status.Detail = "container reports unhealthy"
		default:
			status.State = StateStarting
			status.Detail = "unrecognized health status " + status.Health
		}
	case "created", "restarting":
		status.State = StateStarting
		status.Detail = "container is " + containerState
	case "exited", "dead", "removing", "paused":
		status.State = StateStopped
		status.Detail = "container is " + containerState
	case "":
		status.State = StateUnknown
		status.Detail = "container state could not be parsed"
	default:
		status.State = StateUnknown
		status.Detail = "unrecognized container state " + containerState
	}
	return status, nil
}

// waitForHealth polls until the stack is running, giving up after the budget.
//
// A timeout returns the last observed status together with a typed error, so a
// caller learns whether it was still starting, which is retryable, or failed.
func (m *Manager) waitForHealth(ctx context.Context, names Names, budget time.Duration) (Status, error) {
	if budget <= 0 {
		return m.inspect(ctx, names)
	}
	deadline := time.Now().Add(budget)

	var status Status
	for {
		var err error
		status, err = m.inspect(ctx, names)
		if err != nil {
			return status, err
		}
		switch status.State {
		case StateRunning:
			return status, nil
		case StateError:
			return status, fmt.Errorf("project %s failed to start: %s", names.ProjectID, status.Detail)
		}
		if !time.Now().Before(deadline) {
			return status, fmt.Errorf("project %s did not become healthy within %s (state %s)",
				names.ProjectID, budget, status.State)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ensureNetwork creates only the deterministically named project network and
// treats every unexpected inspection failure as fatal.
func (m *Manager) ensureNetwork(ctx context.Context, plan RuntimePlan) error {
	if _, stderr, err := m.runner.Run(ctx, "network", "inspect", plan.Network, "--format", "{{.Name}}"); err == nil {
		return nil
	} else if !isNoSuchNetwork("", stderr) {
		return fmt.Errorf("inspect project network %s: %s", plan.Network, firstLine(stderr, err))
	}
	args := []string{"network", "create", "--driver", "bridge",
		"--label", "tech.lctk.managed=true",
		"--label", "tech.lctk.project-id=" + plan.ProjectID,
		plan.Network}
	if _, stderr, err := m.runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("create project network %s: %s", plan.Network, firstLine(stderr, err))
	}
	return nil
}

// ensureInferenceAttachment gives exactly one project network access to the
// shared stateless inference container. The inference container joins each
// isolated network, while project containers never join a network belonging to
// another project and therefore cannot address one another.
func (m *Manager) ensureInferenceAttachment(ctx context.Context, plan RuntimePlan) error {
	args := []string{"network", "connect", "--alias", inference.ContainerName, plan.Network, inference.ContainerName}
	if _, stderr, err := m.runner.Run(ctx, args...); err != nil && !isAlreadyConnected("", stderr) {
		return fmt.Errorf("connect inference to project network %s: %s", plan.Network, firstLine(stderr, err))
	}
	return nil
}

// ensureVolume creates durable project state once and never replaces it during
// ordinary start, restart, update, or runtime recovery.
func (m *Manager) ensureVolume(ctx context.Context, plan RuntimePlan) error {
	if _, stderr, err := m.runner.Run(ctx, "volume", "inspect", plan.Volume, "--format", "{{.Name}}"); err == nil {
		return nil
	} else if !isNoSuchVolume("", stderr) {
		return fmt.Errorf("inspect project volume %s: %s", plan.Volume, firstLine(stderr, err))
	}
	args := []string{"volume", "create",
		"--label", "tech.lctk.managed=true",
		"--label", "tech.lctk.project-id=" + plan.ProjectID,
		plan.Volume}
	if _, stderr, err := m.runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("create project volume %s: %s", plan.Volume, firstLine(stderr, err))
	}
	return nil
}

func isNoSuchContainer(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "no such object") ||
		strings.Contains(combined, "no such container")
}

func isNoSuchImage(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "no such image") ||
		strings.Contains(combined, "no such object") ||
		// Podman 5.8 reports an absent local image as "image not known".
		// This is the expected pre-pull state during bootstrap, not a runtime
		// failure, so the signed image may be fetched through InstallImage.
		strings.Contains(combined, "image not known")
}

func isNoSuchNetwork(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "network not found") ||
		strings.Contains(combined, "no such network")
}

func isAlreadyConnected(stdout, stderr string) bool {
	return strings.Contains(strings.ToLower(stdout+" "+stderr), "already connected")
}

func isNotConnected(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "not connected") || isNoSuchContainer(stdout, stderr) || isNoSuchNetwork(stdout, stderr)
}

func isNoSuchVolume(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "no such volume") ||
		strings.Contains(combined, "volume not found")
}

func firstLine(stderr string, err error) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		if err != nil {
			return err.Error()
		}
		return "no output"
	}
	if index := strings.IndexAny(trimmed, "\r\n"); index > 0 {
		return trimmed[:index]
	}
	return trimmed
}
