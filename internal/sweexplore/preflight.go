package sweexplore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/changejournal"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/machinetunnel"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

// NotReadyError marks an honest transitional freshness state that can be
// retried within the configured unmeasured settlement window.
type NotReadyError struct {
	detail string
}

// Error returns the current observable reason the project is not yet eligible.
func (err *NotReadyError) Error() string { return err.detail }

// VerifyRepository proves that an agent sees the declared pristine snapshot.
// Both a wrong commit and any tracked or untracked change are fatal because
// either changes what the benchmark asks the agent to explore.
func VerifyRepository(ctx context.Context, root, expectedCommit string) error {
	head, err := commandOutput(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(expectedCommit)) {
		return fmt.Errorf("repository HEAD is %s, want %s", strings.TrimSpace(head), expectedCommit)
	}
	status, err := commandOutput(ctx, root, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("benchmark repository is not clean")
	}
	return nil
}

// PreflightLCTK proves the complete warm-index state without reading or copying
// any OAuth credential. It opens only LCTK's private runtime tunnel and closes
// that process-local listener before returning.
func PreflightLCTK(ctx context.Context, workspace WorkspaceConfig) (FreshnessProof, error) {
	versionOutput, err := commandOutput(ctx, workspace.Root, workspace.LCTKExecutable, "version", "--json")
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("read installed LCTK version: %w", err)
	}
	var version struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(versionOutput), &version); err != nil {
		return FreshnessProof{}, fmt.Errorf("decode installed LCTK version: %w", err)
	}
	if version.Version != workspace.ExpectedLCTKVersion {
		return FreshnessProof{}, fmt.Errorf("installed LCTK version is %q, want %q", version.Version, workspace.ExpectedLCTKVersion)
	}
	registry, err := projectregistry.Load()
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("load LCTK project registry: %w", err)
	}
	project, err := registry.Resolve(workspace.ProjectID)
	if err != nil {
		return FreshnessProof{}, err
	}
	if !samePath(project.Path, workspace.Root) {
		return FreshnessProof{}, fmt.Errorf("project %q is registered for %q, not %q", project.ID, project.Path, workspace.Root)
	}
	manager := projectstack.NewManager()
	status, err := manager.Status(ctx, project)
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("inspect project stack: %w", err)
	}
	defer machinetunnel.Default.Close(project.ID)
	if status.State != projectstack.StateRunning || status.Health != "healthy" || status.ServiceAddress == "" {
		return FreshnessProof{}, fmt.Errorf("project stack is %s/%s: %s", status.State, status.Health, status.Detail)
	}
	client := codeintel.New(status.ServiceAddress)
	index, err := client.Status(ctx)
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("read code-intelligence status: %w", err)
	}
	if !index.Ready || index.Indexing || index.Semantic == nil || index.Graph == nil {
		return FreshnessProof{}, &NotReadyError{detail: "exact, semantic, and graph indexes are not all ready"}
	}
	if !index.Semantic.Ready || index.Semantic.Indexing || !index.Graph.Ready || index.Semantic.Freshness != "fresh" || index.Graph.Freshness != "fresh" {
		return FreshnessProof{}, &NotReadyError{detail: "semantic or graph index is not fresh and ready"}
	}
	if index.Generation == 0 || index.Generation != index.Semantic.Generation || index.Generation != index.Graph.Generation {
		return FreshnessProof{}, &NotReadyError{detail: fmt.Sprintf("index generations differ: exact=%d semantic=%d graph=%d", index.Generation, index.Semantic.Generation, index.Graph.Generation)}
	}
	memory, err := client.MemorySearch(ctx, codeintel.MemorySearchRequest{Limit: 1})
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("verify empty project memory: %w", err)
	}
	if memory.Total != 0 || len(memory.Matches) != 0 {
		return FreshnessProof{}, fmt.Errorf("project memory contains %d records", memory.Total)
	}
	journalPath, err := changejournal.PathFor(project.ID)
	if err != nil {
		return FreshnessProof{}, err
	}
	body, err := os.ReadFile(journalPath)
	if err != nil {
		return FreshnessProof{}, fmt.Errorf("read watcher journal: %w", err)
	}
	var journal changejournal.Snapshot
	if err := json.Unmarshal(body, &journal); err != nil {
		return FreshnessProof{}, fmt.Errorf("decode watcher journal: %w", err)
	}
	if journal.Gap != nil || len(journal.Pending) != 0 || journal.Sequence != journal.Checkpoint {
		return FreshnessProof{}, &NotReadyError{detail: fmt.Sprintf("watcher is not settled: sequence=%d checkpoint=%d pending=%d gap=%v", journal.Sequence, journal.Checkpoint, len(journal.Pending), journal.Gap != nil)}
	}
	if journal.Generation != index.Generation {
		return FreshnessProof{}, &NotReadyError{detail: fmt.Sprintf("watcher generation %d differs from index generation %d", journal.Generation, index.Generation)}
	}
	return FreshnessProof{
		ProjectID: project.ID, Version: version.Version,
		ExactGeneration: index.Generation, SemanticGeneration: index.Semantic.Generation,
		GraphGeneration: index.Graph.Generation, WatcherGeneration: journal.Generation,
		WatcherSequence: journal.Sequence, WatcherCheckpoint: journal.Checkpoint,
		WatcherPending: len(journal.Pending), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// WaitForLCTK retries only explicitly transitional freshness states. Structural
// errors such as a wrong registration, version, or non-empty memory fail on the
// first observation.
func WaitForLCTK(ctx context.Context, workspace WorkspaceConfig) (FreshnessProof, error) {
	return waitForLCTK(ctx, workspace, 0, false, "", nil)
}

// WaitForLCTKObserved preserves every preparation observation after each
// two-second retry delay while applying the same strict warm-index eligibility
// contract as WaitForLCTK. Probe duration is intentionally not hidden.
func WaitForLCTKObserved(ctx context.Context, workspace WorkspaceConfig, phase string, observer func(PreparationSample) error) (FreshnessProof, error) {
	if observer == nil {
		return FreshnessProof{}, errors.New("preparation observer is required")
	}
	return waitForLCTK(ctx, workspace, 0, false, phase, observer)
}

// WaitForLCTKAfterGeneration prevents a checkout race in which the watcher has
// not observed a Git switch yet and the service still reports the preceding
// checkout as fresh. A changed checkout is eligible only after every index and
// the watcher converge on a strictly newer generation.
func WaitForLCTKAfterGeneration(ctx context.Context, workspace WorkspaceConfig, previousGeneration uint64) (FreshnessProof, error) {
	if previousGeneration == 0 {
		return FreshnessProof{}, errors.New("previous LCTK generation must be positive")
	}
	return waitForLCTK(ctx, workspace, previousGeneration, true, "", nil)
}

// WaitForLCTKAfterGenerationObserved records the post-checkout index and GPU
// time series without weakening the generation-advance barrier.
func WaitForLCTKAfterGenerationObserved(ctx context.Context, workspace WorkspaceConfig, previousGeneration uint64, phase string, observer func(PreparationSample) error) (FreshnessProof, error) {
	if previousGeneration == 0 {
		return FreshnessProof{}, errors.New("previous LCTK generation must be positive")
	}
	if observer == nil {
		return FreshnessProof{}, errors.New("preparation observer is required")
	}
	return waitForLCTK(ctx, workspace, previousGeneration, true, phase, observer)
}

func waitForLCTK(ctx context.Context, workspace WorkspaceConfig, previousGeneration uint64, requireAdvance bool, phase string, observer func(PreparationSample) error) (FreshnessProof, error) {
	deadline := time.Duration(workspace.FreshnessTimeoutSeconds) * time.Second
	bounded, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var last error
	for {
		proof, err := PreflightLCTK(bounded, workspace)
		if observer != nil {
			sample, sampleErr := observePreparation(bounded, workspace, phase, err)
			if sampleErr != nil {
				return FreshnessProof{}, fmt.Errorf("collect preparation telemetry: %w", sampleErr)
			}
			if sampleErr := observer(sample); sampleErr != nil {
				return FreshnessProof{}, fmt.Errorf("persist preparation telemetry: %w", sampleErr)
			}
		}
		if err == nil && generationIsEligible(proof, previousGeneration, requireAdvance) {
			return proof, nil
		}
		if err == nil {
			err = &NotReadyError{detail: fmt.Sprintf("watcher has not advanced beyond generation %d", previousGeneration)}
		}
		var pending *NotReadyError
		if !errors.As(err, &pending) {
			return FreshnessProof{}, err
		}
		last = err
		select {
		case <-bounded.Done():
			return FreshnessProof{}, fmt.Errorf("LCTK did not become fresh within %s: %w", deadline, last)
		case <-time.After(2 * time.Second):
		}
	}
}

// observePreparation reads project progress and live inference state from their
// authoritative APIs. The preflight error is retained as the current not-ready
// reason; it is never substituted for the progress counters.
func observePreparation(ctx context.Context, workspace WorkspaceConfig, phase string, preflightErr error) (PreparationSample, error) {
	registry, err := projectregistry.Load()
	if err != nil {
		return PreparationSample{}, err
	}
	project, err := registry.Resolve(workspace.ProjectID)
	if err != nil {
		return PreparationSample{}, err
	}
	manager := projectstack.NewManager()
	stack, err := manager.Status(ctx, project)
	if err != nil {
		return PreparationSample{}, err
	}
	defer machinetunnel.Default.Close(project.ID)
	if stack.ServiceAddress == "" {
		return PreparationSample{}, fmt.Errorf("project stack has no service address: %s/%s", stack.State, stack.Health)
	}
	index, err := codeintel.New(stack.ServiceAddress).Status(ctx)
	if err != nil {
		return PreparationSample{}, err
	}
	inferenceManager, err := inference.NewRuntimeManager()
	if err != nil {
		return PreparationSample{}, err
	}
	inferenceStatus, err := inferenceManager.Status(ctx)
	if err != nil {
		return PreparationSample{}, err
	}
	reason := ""
	if preflightErr != nil {
		reason = preflightErr.Error()
	}
	return newPreparationSample(phase, index, inferenceStatus, reason, time.Now().UTC()), nil
}

// generationIsEligible centralizes the checkout barrier: ordinary preflight
// accepts any complete proof, while post-checkout preflight requires a proof
// from a generation created after the recorded baseline.
func generationIsEligible(proof FreshnessProof, previousGeneration uint64, requireAdvance bool) bool {
	return !requireAdvance || proof.ExactGeneration > previousGeneration
}

func commandOutput(ctx context.Context, directory, executable string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s: %s: %w", executable, strings.TrimSpace(string(output)), err)
	}
	return string(output), nil
}

func samePath(left, right string) bool {
	return strings.EqualFold(strings.TrimRight(left, `\/`), strings.TrimRight(right, `\/`))
}
