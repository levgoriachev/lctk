// Package projectregistration owns the atomic local registration side effects
// shared by the CLI and the Admin UI.
package projectregistration

import (
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectpath"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

// Result contains the authoritative registry record and safe manifest details
// needed by either user surface.
type Result struct {
	Project  projectregistry.Project
	Manifest projectmanifest.Result
}

// Register resolves the operator-supplied path, reads only safe repository
// declarations, and persists the project. Client access requires a later
// owner-approved OAuth request and is never implied by registration.
func Register(path string, explicitProfile projectregistry.Profile) (Result, error) {
	canonical, err := projectpath.Resolve(path)
	if err != nil {
		return Result{}, err
	}
	manifest, err := projectmanifest.Load(canonical.Display)
	if err != nil {
		return Result{}, err
	}
	profile := explicitProfile
	if profile == "" {
		profile = projectregistry.Profile(manifest.Manifest.Profile)
	}
	registry, err := projectregistry.Load()
	if err != nil {
		return Result{}, err
	}
	project, err := registry.Add(canonical, profile, manifest.TrackedPresent)
	if err != nil {
		return Result{}, err
	}
	if err := registry.Save(); err != nil {
		return Result{}, err
	}
	return Result{Project: project, Manifest: manifest}, nil
}
