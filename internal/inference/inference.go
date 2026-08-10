// Package inference manages the one installation-wide embedding service.
//
// The service is stateless compute. Project text and vectors never enter its
// container filesystem: each project owns those in its isolated state volume.
package inference

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/machinetunnel"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
)

const (
	ContainerName          = "lctk-inference"
	CandidateContainerName = "lctk-inference-candidate"
	RollbackContainerName  = "lctk-inference-rollback"
	ContainerPort          = 8080
	ModelName              = "nomic-embed-text-v1.5.Q4_K_M.gguf"
	ModelAlias             = ModelName
	ModelSHA256            = "d4e388894e09cf3816e8b0896d81d265b55e7a9fff9ab03fe8bf4ef5e11295ac"
	ModelBytes             = int64(84106624)
	ModelURL               = "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5-GGUF/resolve/0188c9bf409793f810680a5a431e7b899c46104c/" + ModelName
	Image                  = "ghcr.io/ggml-org/llama.cpp:server@sha256:14ab06c571008509adcedf635301edfa98071b1b8345269921d31ea4d519ae47"
	Dimensions             = 768
	// ConfigRevision forces replacement when runtime arguments change while the
	// immutable image and model remain the same.
	ConfigRevision = "9"

	// ProjectEndpoint is reached after the inference container joins the one
	// requesting project's isolated network. No Windows host port is involved.
	ProjectEndpoint = "http://lctk-inference:8080/v1/embeddings"
	// RuntimeNetwork is Podman's rootful default network. The shared inference
	// container remains attached to it while also serving isolated projects.
	RuntimeNetwork = "podman"
	// RecoveryTimeout bounds cleanup and rollback work after the caller's
	// context is cancelled. Recovery must continue independently, but it must
	// never leave setup or update blocked indefinitely on a failed runtime.
	RecoveryTimeout = 2 * time.Minute
	// HealthTimeout keeps readiness polling responsive. SelfTestTimeout is
	// separate because the first real embedding can initialize CUDA kernels and
	// must not inherit the health probe's deliberately short deadline.
	HealthTimeout   = 2 * time.Second
	SelfTestTimeout = 30 * time.Second
)

var (
	ErrImageMissing = errors.New("embedding inference image is not installed")
	ErrModelMissing = errors.New("embedding model is not installed")
	ErrModelInvalid = errors.New("embedding model digest does not match")
	ErrNotReady     = errors.New("embedding inference service is not ready")
	ErrSwapFailed   = errors.New("embedding inference activation transaction failed")
)

// Runner executes OCI runtime operations and makes lifecycle behavior testable.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout string, stderr string, err error)
}

type tunnel interface {
	Ensure(context.Context, string, string) (string, error)
}

// Manager owns the shared container lifecycle. Configuration fields are private
// so production always uses pinned identities; tests use NewManagerForTest.
type Manager struct {
	runner         Runner
	image          string
	modelPath      string
	modelBytes     int64
	modelSHA       string
	downloadURL    string
	distribution   Distribution
	evidencePath   string
	address        string
	healthClient   *http.Client
	selfTestClient *http.Client
	tunnel         tunnel
}

// Status is the observable shared inference state.
type Status struct {
	Ready           bool               `json:"ready"`
	Container       string             `json:"container"`
	Image           string             `json:"image"`
	Model           string             `json:"model"`
	ModelPath       string             `json:"model_path"`
	Endpoint        string             `json:"endpoint"`
	Distribution    Distribution       `json:"distribution"`
	Backend         string             `json:"backend"`
	GPU             *nvidiainstall.GPU `json:"gpu,omitempty"`
	GPUTelemetry    *GPUTelemetry      `json:"gpu_telemetry,omitempty"`
	OffloadedLayers string             `json:"offloaded_layers,omitempty"`
	Detail          string             `json:"detail,omitempty"`
}

// GPUTelemetry is a live, point-in-time device sample taken from the same CUDA
// container whose immutable identity and complete layer offload are verified.
// It proves active computation separately from process liveness.
type GPUTelemetry struct {
	SampledAt          string  `json:"sampled_at"`
	UtilizationPercent int     `json:"utilization_percent"`
	MemoryUsedMiB      int     `json:"memory_used_mib"`
	MemoryTotalMiB     int     `json:"memory_total_mib"`
	PowerWatts         float64 `json:"power_watts"`
	TemperatureCelsius int     `json:"temperature_celsius"`
}

// ModelPath returns the installation-owned model location without creating it.
func ModelPath() (string, error) {
	home, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "models", ModelName), nil
}

// NewManager constructs the production manager from immutable component ids.
func NewManager(runner Runner) (*Manager, error) {
	selection, err := LoadSelection()
	if err != nil {
		return nil, err
	}
	return NewManagerForDistribution(runner, selection.Distribution)
}

// NewManagerForDistribution binds one explicit owner selection to its pinned
// image and runtime contract. No automatic capability-based fallback exists.
func NewManagerForDistribution(runner Runner, distribution Distribution) (*Manager, error) {
	if !distribution.Valid() {
		return nil, fmt.Errorf("unsupported inference distribution %q", distribution)
	}
	path, err := ModelPath()
	if err != nil {
		return nil, err
	}
	image := Image
	if distribution == DistributionNVIDIAGPU {
		image = nvidiainstall.Image
	}
	home, err := lctkhome.Dir()
	if err != nil {
		return nil, err
	}
	return &Manager{runner: runner, image: image, distribution: distribution, modelPath: path, modelBytes: ModelBytes,
		modelSHA: ModelSHA256, downloadURL: ModelURL, evidencePath: filepath.Join(home, BackendEvidenceFileName),
		healthClient: &http.Client{Timeout: HealthTimeout}, selfTestClient: &http.Client{Timeout: SelfTestTimeout},
		tunnel: machinetunnel.Default}, nil
}

// NewManagerForTest supplies isolated endpoints and artifacts without weakening
// production constants.
func NewManagerForTest(runner Runner, image, modelPath, address string) *Manager {
	size, digest := fileIdentity(modelPath)
	return &Manager{runner: runner, image: image, modelPath: modelPath,
		distribution: DistributionCPU,
		modelBytes:   size, modelSHA: digest, downloadURL: ModelURL, address: address,
		healthClient: &http.Client{Timeout: HealthTimeout}, selfTestClient: &http.Client{Timeout: SelfTestTimeout}}
}

// VerifyModel streams the whole file through SHA-256 and checks size first. A
// filename or successful download is never accepted as model identity.
func VerifyModel(path string) error {
	return verifyFile(path, ModelBytes, ModelSHA256)
}

func verifyFile(path string, wantedBytes int64, wantedSHA string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrModelMissing, path)
		}
		return fmt.Errorf("inspect embedding model %q: %w", path, err)
	}
	if info.Size() != wantedBytes {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrModelInvalid, info.Size(), wantedBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open embedding model %q: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash embedding model %q: %w", path, err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantedSHA {
		return fmt.Errorf("%w: got %s, want %s", ErrModelInvalid, got, wantedSHA)
	}
	return nil
}

// Ensure activates the selected service only after image, model, candidate
// health, real embeddings, and distribution-specific backend evidence pass.
// Existing project-network attachments move in one rollback-capable transaction.
func (m *Manager) Ensure(ctx context.Context, wait time.Duration) (Status, error) {
	status := m.baseStatus()
	if m.runner == nil {
		return status, errors.New("embedding inference runner is not configured")
	}
	expectedImageID, detail, err := m.imageID(ctx)
	if err != nil {
		status.Detail = detail
		return status, fmt.Errorf("%w: %s", ErrImageMissing, status.Detail)
	}
	if err := verifyFile(m.modelPath, m.modelBytes, m.modelSHA); err != nil {
		status.Detail = err.Error()
		return status, err
	}

	current, err := m.inspectIdentity(ctx, ContainerName)
	if err != nil {
		status.Detail = err.Error()
		return status, err
	}
	if current.exists && current.running && current.imageID == expectedImageID &&
		current.configRevision == ConfigRevision && current.distribution == m.distribution {
		if err := m.waitReady(ctx, ContainerName, wait); err != nil {
			status.Detail = err.Error()
			return status, err
		}
		if err := m.SelfTest(ctx); err != nil {
			status.Detail = err.Error()
			return status, err
		}
		if err := m.attachBackendEvidence(ctx, ContainerName, &status); err != nil {
			status.Detail = err.Error()
			return status, err
		}
		status.Ready = true
		return status, nil
	}
	return m.activateCandidate(ctx, wait, current)
}

type containerIdentity struct {
	exists         bool
	running        bool
	imageID        string
	configRevision string
	distribution   Distribution
}

type networkAttachment struct {
	name string
}

// inspectIdentity distinguishes absence from every other Podman failure and
// compares the actual image ID rather than normalized reference text.
func (m *Manager) inspectIdentity(ctx context.Context, name string) (containerIdentity, error) {
	format := `{{.State.Status}}|{{.Image}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`
	stdout, stderr, err := m.runner.Run(ctx, "inspect", name, "--format", format)
	if err != nil {
		if isNoSuchContainer(stdout, stderr) {
			return containerIdentity{}, nil
		}
		return containerIdentity{}, fmt.Errorf("inspect embedding inference container %s: %s", name, firstLine(stderr, err))
	}
	fields := strings.Split(strings.TrimSpace(stdout), "|")
	if len(fields) != 4 || strings.TrimSpace(fields[1]) == "" {
		return containerIdentity{}, fmt.Errorf("inspect embedding inference container %s returned a malformed identity", name)
	}
	return containerIdentity{
		exists: true, running: fields[0] == "running", imageID: fields[1],
		configRevision: fields[2], distribution: Distribution(fields[3]),
	}, nil
}

// activateCandidate proves the replacement before moving the final name. The
// old container remains a rollback target until every saved project attachment
// and final-name health check succeeds.
func (m *Manager) activateCandidate(ctx context.Context, wait time.Duration, current containerIdentity) (status Status, activationErr error) {
	status = m.baseStatus()
	if err := m.removeOwnedCandidate(ctx); err != nil {
		return status, err
	}
	rollback, err := m.inspectIdentity(ctx, RollbackContainerName)
	if err != nil {
		return status, err
	}
	if rollback.exists {
		return status, fmt.Errorf("%w: stale rollback container %s requires repair", ErrSwapFailed, RollbackContainerName)
	}
	if err := m.startContainer(ctx, CandidateContainerName); err != nil {
		return status, err
	}
	candidateName := CandidateContainerName
	defer func() {
		if candidateName == "" {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
		defer cancel()
		if _, stderr, err := m.runner.Run(cleanupCtx, "rm", "--force", candidateName); err != nil && activationErr == nil {
			activationErr = fmt.Errorf("remove failed inference candidate %s: %s", candidateName, firstLine(stderr, err))
		}
	}()
	if err := m.waitReady(ctx, CandidateContainerName, wait); err != nil {
		return status, err
	}
	if err := m.selfTestFor(ctx, CandidateContainerName); err != nil {
		return status, err
	}
	if err := m.attachBackendEvidence(ctx, CandidateContainerName, &status); err != nil {
		return status, err
	}

	attachments := []networkAttachment{}
	oldRenamed := false
	candidatePromoted := false
	if current.exists {
		attachments, err = m.projectAttachments(ctx, ContainerName)
		if err != nil {
			return status, err
		}
		if _, stderr, err := m.runner.Run(ctx, "rename", ContainerName, RollbackContainerName); err != nil {
			return status, fmt.Errorf("%w: preserve previous inference container: %s", ErrSwapFailed, firstLine(stderr, err))
		}
		oldRenamed = true
		for _, attachment := range attachments {
			if _, stderr, err := m.runner.Run(ctx, "network", "disconnect", attachment.name, RollbackContainerName); err != nil {
				return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted,
					fmt.Errorf("disconnect previous inference from %s: %s", attachment.name, firstLine(stderr, err)))
			}
		}
	}
	if _, stderr, err := m.runner.Run(ctx, "rename", CandidateContainerName, ContainerName); err != nil {
		return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted,
			fmt.Errorf("promote inference candidate: %s", firstLine(stderr, err)))
	}
	// From this point rollback owns cleanup of the promoted final name. The
	// candidate-name defer must not remove a restored old final container.
	candidateName = ""
	candidatePromoted = true
	for _, attachment := range attachments {
		if _, stderr, err := m.runner.Run(ctx, "network", "connect", "--alias", ContainerName, attachment.name, ContainerName); err != nil {
			return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted,
				fmt.Errorf("connect inference candidate to %s: %s", attachment.name, firstLine(stderr, err)))
		}
	}
	if err := m.waitReady(ctx, ContainerName, wait); err != nil {
		return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted, err)
	}
	if err := m.selfTestFor(ctx, ContainerName); err != nil {
		return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted, err)
	}
	if err := m.attachBackendEvidence(ctx, ContainerName, &status); err != nil {
		return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted, err)
	}
	if oldRenamed {
		if _, stderr, err := m.runner.Run(ctx, "rm", "--force", RollbackContainerName); err != nil {
			return status, m.restoreAfterSwapFailure(ctx, attachments, oldRenamed, candidatePromoted,
				fmt.Errorf("remove committed inference rollback container: %s", firstLine(stderr, err)))
		}
	}
	status.Container = ContainerName
	status.Ready = true
	return status, nil
}

// restoreAfterSwapFailure removes only the promoted candidate, reconnects the
// saved old container while its unique name prevents alias collision, and then
// restores the public name. All rollback errors are retained for diagnosis.
func (m *Manager) restoreAfterSwapFailure(ctx context.Context, attachments []networkAttachment, oldRenamed, candidatePromoted bool, cause error) error {
	errorsFound := []error{fmt.Errorf("%w: %v", ErrSwapFailed, cause)}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecoveryTimeout)
	defer cancel()
	if candidatePromoted {
		if _, stderr, err := m.runner.Run(rollbackCtx, "rm", "--force", ContainerName); err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("remove failed inference candidate: %s", firstLine(stderr, err)))
		}
	}
	if oldRenamed {
		for _, attachment := range attachments {
			if _, stderr, err := m.runner.Run(rollbackCtx, "network", "connect", "--alias", ContainerName, attachment.name, RollbackContainerName); err != nil && !isAlreadyConnected(stderr) {
				errorsFound = append(errorsFound, fmt.Errorf("restore inference network %s: %s", attachment.name, firstLine(stderr, err)))
			}
		}
		if _, stderr, err := m.runner.Run(rollbackCtx, "rename", RollbackContainerName, ContainerName); err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("restore previous inference name: %s", firstLine(stderr, err)))
		}
	}
	return errors.Join(errorsFound...)
}

func (m *Manager) startContainer(ctx context.Context, name string) error {
	runtimeModelPath, err := containerruntime.HostPath(m.modelPath)
	if err != nil {
		return fmt.Errorf("prepare embedding model mount: %w", err)
	}
	mount := "type=bind,source=" + runtimeModelPath + ",target=/models/" + ModelName + ",readonly"
	args := []string{
		"run", "--detach", "--name", name, "--restart", "unless-stopped",
		"--label", "tech.lctk.managed=true", "--label", "tech.lctk.component=inference",
		"--label", "tech.lctk.inference-config=" + ConfigRevision,
		"--label", "tech.lctk.inference-distribution=" + string(m.distribution),
	}
	if m.distribution == DistributionNVIDIAGPU {
		// Trace verbosity is required for exact layer-offload evidence. Keep that
		// high-volume diagnostic stream in a container-scoped bounded log instead
		// of the installation-wide system journal.
		args = append(args, "--device", nvidiainstall.CDIDevice,
			"--log-driver", "k8s-file", "--log-opt", "max-size=32mb")
	}
	batchSize := "4096"
	if m.distribution == DistributionNVIDIAGPU {
		// The pinned CUDA image accepts the full 8192-token physical batch on the
		// supported Pascal floor. This keeps the embedding tensor work resident
		// enough to saturate the GPU instead of feeding it in CPU-bound bursts.
		batchSize = "8192"
	}
	args = append(args,
		"--mount", mount, m.image,
		"--model", "/models/"+ModelName, "--alias", ModelAlias,
		"--embedding", "--pooling", "mean", "--host", "0.0.0.0",
		"--parallel", "8", "--port", fmt.Sprint(ContainerPort), "--ctx-size", "32768",
		"--batch-size", batchSize, "--ubatch-size", batchSize,
	)
	if m.distribution == DistributionNVIDIAGPU {
		// b10257 emits its measured CUDA device and exact offloaded-layer count
		// only at trace level. It does not log embedding input text; diagnostics
		// read only the fixed two-minute startup window.
		args = append(args, "--n-gpu-layers", "99", "--log-verbosity", "4")
	}
	if _, stderr, err := m.runner.Run(ctx, args...); err != nil {
		return fmt.Errorf("start %s embedding inference candidate: %s", m.distribution, firstLine(stderr, err))
	}
	return nil
}

func (m *Manager) removeOwnedCandidate(ctx context.Context) error {
	format := `{{index .Config.Labels "tech.lctk.managed"}}|{{index .Config.Labels "tech.lctk.component"}}`
	stdout, stderr, err := m.runner.Run(ctx, "inspect", CandidateContainerName, "--format", format)
	if err != nil {
		if isNoSuchContainer(stdout, stderr) {
			return nil
		}
		return fmt.Errorf("inspect stale inference candidate: %s", firstLine(stderr, err))
	}
	if strings.TrimSpace(stdout) != "true|inference" {
		return fmt.Errorf("container %s exists but is not owned by LCTK", CandidateContainerName)
	}
	if _, stderr, err := m.runner.Run(ctx, "rm", "--force", CandidateContainerName); err != nil {
		return fmt.Errorf("remove stale inference candidate: %s", firstLine(stderr, err))
	}
	return nil
}

func (m *Manager) projectAttachments(ctx context.Context, name string) ([]networkAttachment, error) {
	stdout, stderr, err := m.runner.Run(ctx, "inspect", name, "--format", `{{json .NetworkSettings.Networks}}`)
	if err != nil {
		return nil, fmt.Errorf("inspect inference network topology: %s", firstLine(stderr, err))
	}
	var networks map[string]struct {
		Aliases []string `json:"Aliases"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &networks); err != nil {
		return nil, fmt.Errorf("decode inference network topology: %w", err)
	}
	if _, ok := networks[RuntimeNetwork]; !ok {
		return nil, fmt.Errorf("inference container is detached from runtime network %s", RuntimeNetwork)
	}
	attachments := make([]networkAttachment, 0, len(networks)-1)
	for name, network := range networks {
		if name == RuntimeNetwork {
			continue
		}
		if name == "" || !validProjectAliases(network.Aliases) {
			return nil, fmt.Errorf("inference network %q has unexpected aliases %v", name, network.Aliases)
		}
		attachments = append(attachments, networkAttachment{name: name})
	}
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].name < attachments[j].name })
	return attachments, nil
}

func validProjectAliases(aliases []string) bool {
	foundPublic := false
	for _, alias := range aliases {
		switch {
		case alias == ContainerName:
			foundPublic = true
		case isShortContainerID(alias):
		default:
			return false
		}
	}
	return foundPublic
}

func isShortContainerID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

var offloadPattern = regexp.MustCompile(`(?i)offloaded\s+([1-9][0-9]*)/([1-9][0-9]*)\s+layers\s+to\s+gpu`)

const podmanStartedAtLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

func (m *Manager) attachBackendEvidence(ctx context.Context, name string, status *Status) error {
	if m.distribution == DistributionCPU {
		status.Backend = "cpu"
		return nil
	}
	stdout, stderr, err := m.runner.Run(ctx, "exec", name, "nvidia-smi",
		"--query-gpu=name,driver_version,memory.total,compute_cap,utilization.gpu,memory.used,power.draw,temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDADeviceMissing,
			Detail: "CUDA container cannot query the NVIDIA adapter: " + firstLine(stderr, err)}
	}
	gpu, telemetry, err := parseGPUStatusProbe(stdout, time.Now().UTC())
	if err != nil {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDADeviceMissing, Detail: err.Error()}
	}
	identityRaw, identityStderr, err := m.runner.Run(ctx, "inspect", name, "--format",
		`{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`)
	if err != nil {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
			Detail: "inspect llama.cpp CUDA evidence identity: " + firstLine(identityStderr, err)}
	}
	identity, err := parseBackendEvidenceIdentity(identityRaw)
	if err != nil {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
			Detail: err.Error()}
	}
	if m.evidencePath != "" {
		stored, found, loadErr := loadBackendEvidence(m.evidencePath, identity)
		if loadErr != nil {
			return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
				Detail: "read persisted llama.cpp CUDA evidence: " + loadErr.Error()}
		}
		if found && stored.matches(identity) {
			status.Backend = "cuda"
			status.GPU = &gpu
			status.GPUTelemetry = &telemetry
			status.OffloadedLayers = stored.OffloadedLayers
			return nil
		}
	}
	// The exact offload evidence is emitted during startup. A fixed time window
	// stays available after later project traffic without reading unbounded logs.
	logs, logStderr, err := m.runner.Run(ctx, "logs", "--since", identity.StartedAt.Format(time.RFC3339Nano),
		"--until", identity.StartedAt.Add(2*time.Minute).Format(time.RFC3339Nano), name)
	if err != nil {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
			Detail: "read llama.cpp CUDA diagnostics: " + firstLine(logStderr, err)}
	}
	// Podman preserves the container's stdout and stderr channels. llama.cpp
	// writes structured startup diagnostics to stderr on the pinned image even
	// when `podman logs` succeeds, so both successful streams are evidence.
	logs += "\n" + logStderr
	if len(logs) > 1<<20 {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
			Detail: "llama.cpp CUDA startup diagnostics exceed the bounded evidence limit"}
	}
	match := offloadPattern.FindStringSubmatch(logs)
	if len(match) != 3 || match[1] != match[2] || !strings.Contains(strings.ToLower(logs), "cuda") {
		return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
			Detail: "llama.cpp did not report CUDA initialization and offloaded model layers"}
	}
	evidence := identity
	evidence.OffloadedLayers = match[1] + "/" + match[2]
	if m.evidencePath != "" {
		if err := saveBackendEvidence(m.evidencePath, evidence); err != nil {
			return &nvidiainstall.Failure{Code: nvidiainstall.FailureCUDAOffloadMissing,
				Detail: "persist verified llama.cpp CUDA evidence: " + err.Error()}
		}
	}
	status.Backend = "cuda"
	status.GPU = &gpu
	status.GPUTelemetry = &telemetry
	status.OffloadedLayers = evidence.OffloadedLayers
	return nil
}

// parseGPUStatusProbe validates both immutable adapter identity and every live
// measurement. Missing or non-numeric telemetry is a hard observability
// failure because a zero/unknown sample must never be presented as proof of
// CUDA work.
func parseGPUStatusProbe(output string, sampledAt time.Time) (nvidiainstall.GPU, GPUTelemetry, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil || len(records) != 1 || len(records[0]) != 8 {
		return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA runtime telemetry is malformed")
	}
	record := records[0]
	for index := range record {
		record[index] = strings.TrimSpace(record[index])
		if record[index] == "" {
			return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA runtime telemetry contains an empty field")
		}
	}
	gpu, err := nvidiainstall.ParseHostProbe(strings.Join(record[:4], ","))
	if err != nil {
		return nvidiainstall.GPU{}, GPUTelemetry{}, err
	}
	utilization, err := strconv.Atoi(record[4])
	if err != nil || utilization < 0 || utilization > 100 {
		return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA GPU utilization %q is invalid", record[4])
	}
	memoryUsed, err := strconv.Atoi(record[5])
	if err != nil || memoryUsed < 0 || memoryUsed > gpu.VRAMMiB {
		return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA GPU memory use %q is invalid", record[5])
	}
	power, err := strconv.ParseFloat(record[6], 64)
	if err != nil || power < 0 {
		return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA GPU power draw %q is invalid", record[6])
	}
	temperature, err := strconv.Atoi(record[7])
	if err != nil || temperature < 0 || temperature > 120 {
		return nvidiainstall.GPU{}, GPUTelemetry{}, fmt.Errorf("NVIDIA GPU temperature %q is invalid", record[7])
	}
	return gpu, GPUTelemetry{SampledAt: sampledAt.Format(time.RFC3339Nano), UtilizationPercent: utilization,
		MemoryUsedMiB: memoryUsed, MemoryTotalMiB: gpu.VRAMMiB, PowerWatts: power, TemperatureCelsius: temperature}, nil
}

func isNoSuchContainer(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + " " + stderr)
	return strings.Contains(combined, "no such object") || strings.Contains(combined, "no such container")
}

func isAlreadyConnected(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "already connected")
}

// Status checks installed identities, container state, and the live endpoint
// without changing any of them.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	status := m.baseStatus()
	expectedImageID, detail, err := m.imageID(ctx)
	if err != nil {
		status.Detail = detail
		return status, fmt.Errorf("%w: %s", ErrImageMissing, status.Detail)
	}
	if err := verifyFile(m.modelPath, m.modelBytes, m.modelSHA); err != nil {
		status.Detail = err.Error()
		return status, err
	}
	identity, err := m.inspectIdentity(ctx, ContainerName)
	if err != nil {
		status.Detail = err.Error()
		return status, err
	}
	if !identity.exists || !identity.running || identity.imageID != expectedImageID ||
		identity.configRevision != ConfigRevision || identity.distribution != m.distribution {
		status.Detail = "The pinned embedding container is not running."
		return status, ErrNotReady
	}
	if err := m.health(ctx); err != nil {
		status.Detail = err.Error()
		return status, err
	}
	if err := m.attachBackendEvidence(ctx, ContainerName, &status); err != nil {
		status.Detail = err.Error()
		return status, err
	}
	status.Ready = true
	return status, nil
}

// imageID resolves the pinned manifest reference to the local immutable image
// identity used by Podman container inspection. A missing or empty identity is
// terminal because lifecycle code cannot safely decide whether to reuse it.
func (m *Manager) imageID(ctx context.Context) (string, string, error) {
	stdout, stderr, err := m.runner.Run(ctx, "image", "inspect", m.image, "--format", "{{.Id}}")
	if err != nil {
		return "", firstLine(stderr, err), err
	}
	id := strings.TrimSpace(stdout)
	if id == "" {
		return "", "the pinned embedding image has an empty immutable ID", errors.New("empty embedding image ID")
	}
	return id, "", nil
}

func fileIdentity(path string) (int64, string) {
	file, err := os.Open(path)
	if err != nil {
		return ModelBytes, ModelSHA256
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return ModelBytes, ModelSHA256
	}
	return size, hex.EncodeToString(hash.Sum(nil))
}

func (m *Manager) baseStatus() Status {
	endpoint := ProjectEndpoint
	if m.address != "" {
		endpoint = m.address + "/v1/embeddings"
	}
	return Status{Container: ContainerName, Image: m.image, Model: ModelAlias,
		ModelPath: m.modelPath, Endpoint: endpoint, Distribution: m.distribution}
}

func (m *Manager) health(ctx context.Context) error {
	return m.healthFor(ctx, ContainerName)
}

func (m *Manager) healthFor(ctx context.Context, name string) error {
	address, err := m.serviceAddressFor(ctx, name)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/health", nil)
	if err != nil {
		return err
	}
	response, err := m.healthClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("embedding health returned %s", response.Status)
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&health); err != nil {
		return fmt.Errorf("decode embedding health: %w", err)
	}
	if health.Status != "ok" {
		return fmt.Errorf("embedding health is %q, want ok", health.Status)
	}
	return nil
}

func (m *Manager) waitReady(ctx context.Context, name string, wait time.Duration) error {
	if wait <= 0 {
		wait = 90 * time.Second
	}
	deadline := time.Now().Add(wait)
	var detail string
	for {
		if err := m.healthFor(ctx, name); err == nil {
			return nil
		} else {
			detail = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s", ErrNotReady, detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// serviceAddress resolves one Windows-reachable base URL. Production always
// derives it from the exact private container and the authenticated machine
// tunnel; tests may supply an isolated HTTP endpoint directly.
func (m *Manager) serviceAddress(ctx context.Context) (string, error) {
	return m.serviceAddressFor(ctx, ContainerName)
}

func (m *Manager) serviceAddressFor(ctx context.Context, name string) (string, error) {
	if m.tunnel == nil {
		if strings.TrimSpace(m.address) == "" {
			return "", errors.New("embedding service endpoint is not configured")
		}
		return m.address, nil
	}
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, RuntimeNetwork)
	stdout, stderr, err := m.runner.Run(ctx, "inspect", name, "--format", format)
	if err != nil {
		return "", fmt.Errorf("inspect embedding container address: %s", firstLine(stderr, err))
	}
	containerAddress := strings.TrimSpace(stdout)
	if containerAddress == "" {
		return "", fmt.Errorf("embedding container has no address on runtime network %s", RuntimeNetwork)
	}
	remote := net.JoinHostPort(containerAddress, fmt.Sprint(ContainerPort))
	local, err := m.tunnel.Ensure(ctx, "inference-"+name, remote)
	if err != nil {
		return "", fmt.Errorf("open embedding machine tunnel: %w", err)
	}
	return "http://" + local, nil
}

func firstLine(stderr string, err error) string {
	if line := strings.TrimSpace(strings.SplitN(stderr, "\n", 2)[0]); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}
