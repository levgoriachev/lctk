// Package projectstack generates and drives each isolated project runtime.
//
// Each registered project gets its own container, network, and volume, per
// [ADR-0003]. The source is mounted read-only into the code-intel boundary, so a
// project cannot write to its own working tree through this path.
//
// Generated configuration is derived state: it is rewritten from the registry
// whenever a project starts, and it is never a source of truth for project
// identity or for the host path.
//
// [ADR-0003]: ../../docs/adr/0003-reusable-images-and-project-stacks.md
package projectstack

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

// ErrInvalidProject reports a registry record that cannot produce a safe,
// deterministic managed-runtime identity or mount plan.
var ErrInvalidProject = errors.New("project cannot be used as a container runtime")

const (
	// ServiceName is the single service in a Slice 1.2 stack.
	ServiceName = "code-intel"
	// WorkspaceMount is where the project source appears inside the container.
	WorkspaceMount = "/workspace"
	// StateMount is where per-project persistent state lives inside the container.
	StateMount = "/var/lib/lctk"
	// ServicePort is the port the code-intel service listens on inside the
	// container. It is fixed because each project has its own network namespace;
	// the host reaches its private address through a process-owned dynamic
	// loopback tunnel, so two projects can never contend for one number.
	ServicePort = 8080
	// ImageRepository is the reusable image every project shares.
	ImageRepository = "lctk/code-intel"
	// resourcePrefix keeps every OCI resource LCTK creates identifiable and
	// distinguishable from unrelated resources on the managed machine.
	resourcePrefix = "lctk"
)

// resourceNamePattern is the common subset accepted for Podman resources and
// installation-owned directory names.
var resourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Names holds every managed OCI resource name derived from a project identifier.
//
// The names are a pure function of the identifier, so they are stable across
// restarts and reinstalls and can be recomputed rather than stored.
type Names struct {
	ProjectID     string
	Network       string
	Volume        string
	ContainerName string
	Image         string
}

// DeriveNames computes the resource names for a project identifier.
//
// ADR-0003 left Compose resource naming to be specified; this is that
// specification. The image tag follows the unified product version from
// [ADR-0007], so an upgraded LCTK asks for a matching image rather than silently
// reusing an older one.
//
// [ADR-0007]: ../../docs/adr/0007-unified-versioning.md
func DeriveNames(projectID string) (Names, error) {
	return DeriveNamesForVersion(projectID, buildinfo.Version)
}

// DeriveNamesForVersion selects an explicit immutable product image while
// preserving every stable project resource name. Update uses it to health-check
// a candidate before activating the matching host core.
func DeriveNamesForVersion(projectID, version string) (Names, error) {
	if projectID == "" {
		return Names{}, fmt.Errorf("%w: project id is empty", ErrInvalidProject)
	}
	if !resourceNamePattern.MatchString(projectID) {
		return Names{}, fmt.Errorf("%w: project id %q is not usable as a runtime resource name",
			ErrInvalidProject, projectID)
	}
	if version == "" {
		return Names{}, fmt.Errorf("%w: product version is empty", ErrInvalidProject)
	}

	base := resourcePrefix + "-" + projectID
	return Names{
		ProjectID:     projectID,
		Network:       base + "-net",
		Volume:        base + "-state",
		ContainerName: base + "-" + ServiceName,
		Image:         ImageRepository + ":" + version,
	}, nil
}

// StackDir is the per-project directory holding generated configuration.
//
// It lives under the LCTK home rather than in the repository, because generated
// configuration must not be committable and must not be editable by whoever
// controls the project's source.
func StackDir(projectID string) (string, error) {
	home, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	if projectID == "" || !resourceNamePattern.MatchString(projectID) {
		return "", fmt.Errorf("%w: project id %q cannot be used as a directory name",
			ErrInvalidProject, projectID)
	}
	return filepath.Join(home, "projects", projectID), nil
}
