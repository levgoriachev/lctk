package projectstack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

// RuntimePlan is the complete deterministic container contract for one project.
// It replaces generated Compose input: LCTK owns both this persisted audit view
// and the exact Podman arguments derived from it.
type RuntimePlan struct {
	SchemaVersion   int               `json:"schema_version"`
	ProjectID       string            `json:"project_id"`
	Image           string            `json:"image"`
	Container       string            `json:"container"`
	Network         string            `json:"network"`
	Volume          string            `json:"volume"`
	WorkspaceSource string            `json:"workspace_source"`
	WorkspaceTarget string            `json:"workspace_target"`
	StateTarget     string            `json:"state_target"`
	ServicePort     int               `json:"service_port"`
	CPUs            float64           `json:"cpus,omitempty"`
	MemoryMB        int               `json:"memory_mb,omitempty"`
	Environment     []string          `json:"environment"`
	Labels          map[string]string `json:"labels"`
	Health          HealthPlan        `json:"health"`
}

// HealthPlan is the fail-fast service readiness contract passed to Podman.
type HealthPlan struct {
	Command     string `json:"command"`
	Interval    string `json:"interval"`
	Timeout     string `json:"timeout"`
	Retries     int    `json:"retries"`
	StartPeriod string `json:"start_period"`
}

// BuildRuntimePlan validates one authoritative registry record and derives the
// runtime view for the active product version.
func BuildRuntimePlan(project projectregistry.Project, budget hostsettings.Budget) (RuntimePlan, error) {
	return BuildRuntimePlanForVersion(project, budget, buildinfo.Version)
}

// BuildRuntimePlanForVersion selects a candidate image without changing stable
// project resource names. Transactional update uses this before activation.
func BuildRuntimePlanForVersion(project projectregistry.Project, budget hostsettings.Budget, version string) (RuntimePlan, error) {
	names, err := DeriveNamesForVersion(project.ID, version)
	if err != nil {
		return RuntimePlan{}, err
	}
	if project.Path == "" || !filepath.IsAbs(project.Path) {
		return RuntimePlan{}, fmt.Errorf("%w: host path %q is not absolute", ErrInvalidProject, project.Path)
	}
	runtimePath, err := containerruntime.HostPath(project.Path)
	if err != nil {
		return RuntimePlan{}, fmt.Errorf("prepare project %s runtime mount: %w", project.ID, err)
	}
	return RuntimePlan{
		SchemaVersion:   1,
		ProjectID:       project.ID,
		Image:           names.Image,
		Container:       names.ContainerName,
		Network:         names.Network,
		Volume:          names.Volume,
		WorkspaceSource: runtimePath,
		WorkspaceTarget: WorkspaceMount,
		StateTarget:     StateMount,
		ServicePort:     ServicePort,
		CPUs:            budget.CPUs,
		MemoryMB:        budget.MemoryLimitMB,
		Environment:     runtimeEnvironment(project, budget),
		Labels: map[string]string{
			"tech.lctk.managed":    "true",
			"tech.lctk.project-id": project.ID,
			"tech.lctk.version":    version,
		},
		Health: HealthPlan{
			Command:     "test -f " + StateMount + "/ready",
			Interval:    "5s",
			Timeout:     "3s",
			Retries:     6,
			StartPeriod: "2s",
		},
	}, nil
}

// Arguments renders the complete Podman run call. Map-backed labels are emitted
// in a fixed order so the plan produces byte-identical command evidence.
func (p RuntimePlan) Arguments() []string {
	args := []string{
		"run", "--detach", "--replace", "--init", "--restart", "no",
		"--name", p.Container,
		"--network", p.Network,
		"--volume", p.WorkspaceSource + ":" + p.WorkspaceTarget + ":ro",
		"--volume", p.Volume + ":" + p.StateTarget,
		"--health-cmd", p.Health.Command,
		"--health-interval", p.Health.Interval,
		"--health-timeout", p.Health.Timeout,
		"--health-retries", strconv.Itoa(p.Health.Retries),
		"--health-start-period", p.Health.StartPeriod,
	}
	if p.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(p.CPUs, 'f', -1, 64))
	}
	if p.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(p.MemoryMB)+"m")
	}
	for _, value := range p.Environment {
		args = append(args, "--env", value)
	}
	for _, key := range []string{"tech.lctk.managed", "tech.lctk.project-id", "tech.lctk.version"} {
		args = append(args, "--label", key+"="+p.Labels[key])
	}
	return append(args, p.Image)
}

// RenderRuntimePlan returns stable indented JSON for diagnostics and recovery.
func RenderRuntimePlan(plan RuntimePlan) ([]byte, error) {
	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render runtime plan for %s: %w", plan.ProjectID, err)
	}
	return append(body, '\n'), nil
}

// WriteRuntimePlan atomically persists derived runtime state below LCTK_HOME.
func WriteRuntimePlan(plan RuntimePlan) (string, error) {
	body, err := RenderRuntimePlan(plan)
	if err != nil {
		return "", err
	}
	dir, err := StackDir(plan.ProjectID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create stack directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, "runtime.json")
	temporary, err := os.CreateTemp(dir, "runtime.*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary runtime plan in %q: %w", dir, err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary runtime plan: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("flush temporary runtime plan: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary runtime plan: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return "", fmt.Errorf("restrict temporary runtime plan permissions: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("replace runtime plan %q: %w", path, err)
	}
	keep = true
	return path, nil
}

// RuntimePlanPath returns the deterministic derived-state path without writing.
func RuntimePlanPath(projectID string) (string, error) {
	dir, err := StackDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "runtime.json"), nil
}

// runtimeEnvironment is the complete service environment. Slice order is part
// of the deterministic plan and therefore intentionally explicit.
func runtimeEnvironment(project projectregistry.Project, budget hostsettings.Budget) []string {
	values := []string{
		"LCTK_PROJECT_ID=" + project.ID,
		"LCTK_PROJECT_PROFILE=" + string(project.Profile),
		"LCTK_WORKSPACE=" + WorkspaceMount,
		"LCTK_STATE_DIR=" + StateMount,
		"LCTK_EMBEDDING_URL=" + inference.ProjectEndpoint,
		"LCTK_EMBEDDING_MODEL=" + inference.ModelAlias,
		"LCTK_EMBEDDING_DIMENSIONS=" + strconv.Itoa(inference.Dimensions),
	}
	if budget.IndexParallelism > 0 {
		values = append(values, "LCTK_INDEX_PARALLELISM="+strconv.Itoa(budget.IndexParallelism))
	}
	return values
}
