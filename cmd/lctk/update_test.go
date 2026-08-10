package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/daemonstate"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/projectpath"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/updateflow"
)

type updateInstallerStub struct {
	installed bool
	verified  bool
	rolled    bool
	verifyErr error
}

func (s *updateInstallerStub) Inspect(manifest releasebundle.Manifest) (installation.Plan, releasebundle.Artifact, error) {
	return installation.Plan{CurrentVersion: "1.0.0", TargetVersion: manifest.Version, Ready: true}, releasebundle.Artifact{}, nil
}

func (s *updateInstallerStub) Install(context.Context, releasebundle.Manifest) (installation.Activation, error) {
	s.installed = true
	return installation.Activation{ActiveVersion: "1.1.0"}, nil
}

func (s *updateInstallerStub) VerifyRollback() (installation.Activation, error) {
	s.verified = true
	if s.verifyErr != nil {
		return installation.Activation{}, s.verifyErr
	}
	return installation.Activation{ActiveVersion: "1.1.0", PreviousVersion: "1.0.0"}, nil
}

func (s *updateInstallerStub) Rollback() (installation.Activation, error) {
	s.rolled = true
	return installation.Activation{ActiveVersion: "1.0.0"}, nil
}

type updateStackStub struct {
	states           map[string]projectstack.State
	distribution     inference.Distribution
	failStart        string
	imageInstalled   bool
	archiveInstalled bool
	stopped          []string
	started          []string
	restored         []string
}

func (s *updateStackStub) RuntimeAvailable(context.Context) error { return nil }
func (s *updateStackStub) InstallImage(context.Context, string, string) error {
	s.imageInstalled = true
	return nil
}
func (s *updateStackStub) InstallImageArchive(context.Context, string, string, releasebundle.Artifact) error {
	s.archiveInstalled = true
	return nil
}
func (s *updateStackStub) Status(_ context.Context, project projectregistry.Project) (projectstack.Status, error) {
	return projectstack.Status{ProjectID: project.ID, State: s.states[project.ID]}, nil
}
func (s *updateStackStub) Start(_ context.Context, project projectregistry.Project, _ time.Duration) (projectstack.Status, error) {
	s.started = append(s.started, project.ID)
	if project.ID == s.failStart {
		return projectstack.Status{ProjectID: project.ID, State: projectstack.StateError}, errors.New("health failed")
	}
	return projectstack.Status{ProjectID: project.ID, State: projectstack.StateRunning}, nil
}
func (s *updateStackStub) Stop(_ context.Context, project projectregistry.Project) (projectstack.Status, error) {
	s.stopped = append(s.stopped, project.ID)
	return projectstack.Status{ProjectID: project.ID, State: projectstack.StateStopped}, nil
}
func (s *updateStackStub) RestoreSchemaRollback(_ context.Context, project projectregistry.Project, _ string) error {
	s.restored = append(s.restored, project.ID)
	return nil
}

func TestUpdatePlanIsReadOnly(t *testing.T) {
	current := &updateStackStub{states: map[string]projectstack.State{}}
	target := &updateStackStub{states: map[string]projectstack.State{}}
	installer := &updateInstallerStub{}
	restoreUpdateFactories(t, projectregistry.New(), current, target, installer)

	var output bytes.Buffer
	if err := runUpdate(t.Context(), []string{"--manifest", "release.json", "--plan", "--json"}, &output); err != nil {
		t.Fatalf("runUpdate plan: %v", err)
	}
	if installer.installed || target.imageInstalled || len(current.stopped) != 0 {
		t.Fatalf("read-only plan mutated state: installer=%+v current=%+v target=%+v", installer, current, target)
	}
	for _, wanted := range []string{`"signature_valid": true`, `"writes": false`, `"target_version": "1.1.0"`} {
		if !bytes.Contains(output.Bytes(), []byte(wanted)) {
			t.Errorf("plan omits %s: %s", wanted, output.String())
		}
	}
}

func TestUpdateRestoresEveryMigratedProjectBeforeHostActivationOnHealthFailure(t *testing.T) {
	registry := testUpdateRegistry(t, 2)
	projects := registry.List()
	current := &updateStackStub{states: map[string]projectstack.State{
		projects[0].ID: projectstack.StateRunning,
		projects[1].ID: projectstack.StateRunning,
	}}
	target := &updateStackStub{states: map[string]projectstack.State{}, failStart: projects[1].ID}
	installer := &updateInstallerStub{}
	restoreUpdateFactories(t, registry, current, target, installer)

	err := runUpdate(t.Context(), []string{"--manifest", "release.json", "--yes"}, &bytes.Buffer{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("health gate failed")) {
		t.Fatalf("runUpdate error = %v", err)
	}
	if installer.installed {
		t.Fatal("host core activated after a project health failure")
	}
	if !target.archiveInstalled || target.imageInstalled {
		t.Fatalf("signed local image archive path was not selected: target=%+v", target)
	}
	if len(target.restored) != 2 || len(current.started) != 2 {
		t.Fatalf("rollback incomplete: target=%+v current=%+v", target, current)
	}
}

func TestRollbackRestoresStoppedProjectsWithoutStartingThem(t *testing.T) {
	registry := testUpdateRegistry(t, 2)
	projects := registry.List()
	current := &updateStackStub{states: map[string]projectstack.State{}}
	previous := &updateStackStub{states: map[string]projectstack.State{}}
	if err := rollbackRegisteredProjects(t.Context(), current, previous, projects,
		[]projectregistry.Project{projects[0]}, "1.0.0"); err != nil {
		t.Fatalf("rollbackRegisteredProjects: %v", err)
	}
	if len(current.restored) != 2 {
		t.Fatalf("restored projects = %v, want both registered projects", current.restored)
	}
	if len(previous.started) != 1 || previous.started[0] != projects[0].ID {
		t.Fatalf("started projects = %v, want only previously running project", previous.started)
	}
}

func TestRollbackRefusesProjectChangesBeforePreviousCoreVerification(t *testing.T) {
	registry := testUpdateRegistry(t, 1)
	project := registry.List()[0]
	current := &updateStackStub{states: map[string]projectstack.State{project.ID: projectstack.StateRunning}}
	previous := &updateStackStub{states: map[string]projectstack.State{}}
	installer := &updateInstallerStub{verifyErr: errors.New("previous host core is corrupt")}
	restoreUpdateFactories(t, registry, current, previous, installer)

	err := runUpdateRollback(t.Context(), []string{"--json"}, &bytes.Buffer{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("corrupt")) {
		t.Fatalf("runUpdateRollback error = %v", err)
	}
	if !installer.verified || installer.rolled || len(current.stopped) != 0 || len(current.restored) != 0 || len(previous.started) != 0 {
		t.Fatalf("rollback mutated state before core verification: installer=%+v current=%+v previous=%+v", installer, current, previous)
	}
}

func TestRollbackRestartsTheDaemonThroughTheActiveLauncher(t *testing.T) {
	registry := testUpdateRegistry(t, 0)
	current := &updateStackStub{states: map[string]projectstack.State{}}
	previous := &updateStackStub{states: map[string]projectstack.State{}}
	installer := &updateInstallerStub{}
	restoreUpdateFactories(t, registry, current, previous, installer)
	events := []string{}
	loadUpdateDaemon = func(string) (daemonstate.State, error) {
		return daemonstate.State{PID: 42, Executable: "installed-core"}, nil
	}
	stopUpdateDaemon = func(string) error {
		events = append(events, "stop")
		return nil
	}
	startUpdateDaemon = func(string) error {
		events = append(events, "start")
		if !installer.rolled {
			t.Fatal("daemon restarted before host rollback activation")
		}
		return nil
	}

	if err := runUpdateRollback(t.Context(), []string{"--json"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runUpdateRollback: %v", err)
	}
	if len(events) != 2 || events[0] != "stop" || events[1] != "start" {
		t.Fatalf("daemon events = %v, want stop then start", events)
	}
	if current.distribution != inference.DistributionCPU {
		t.Fatalf("previous-version stack distribution = %q, want CPU", current.distribution)
	}
}

func restoreUpdateFactories(t *testing.T, registry *projectregistry.Registry, current, target *updateStackStub, installer *updateInstallerStub) {
	t.Helper()
	oldVersion := buildinfo.Version
	oldVerifier := newUpdateVerifier
	oldLoader := loadUpdateManifest
	oldStack := newUpdateStack
	oldInstaller := newUpdateInstaller
	oldRegistry := loadUpdateRegistry
	oldInference := newUpdateInference
	oldLoadDaemon := loadUpdateDaemon
	oldStopDaemon := stopUpdateDaemon
	oldStartDaemon := startUpdateDaemon
	buildinfo.Version = "1.0.0"
	newUpdateVerifier = func() (releasebundle.Verifier, error) { return releasebundle.Verifier{}, nil }
	loadUpdateManifest = func(context.Context, string, releasebundle.Verifier) (releasebundle.Manifest, error) {
		return testUpdateManifest(), nil
	}
	newUpdateStack = func(version string, distribution inference.Distribution) updateStack {
		if version == "1.1.0" {
			target.distribution = distribution
			return target
		}
		current.distribution = distribution
		return current
	}
	newUpdateInstaller = func(string) updateInstaller { return installer }
	loadUpdateRegistry = func() (*projectregistry.Registry, error) { return registry, nil }
	shared := &bootstrapInferenceStub{image: true, model: true}
	newUpdateInference = func(inference.Distribution) (updateflow.Inference, error) { return shared, nil }
	loadUpdateDaemon = func(string) (daemonstate.State, error) { return daemonstate.State{}, os.ErrNotExist }
	stopUpdateDaemon = func(string) error { return nil }
	startUpdateDaemon = func(string) error { return nil }
	t.Setenv("LCTK_HOME", t.TempDir())
	t.Cleanup(func() {
		buildinfo.Version = oldVersion
		newUpdateVerifier = oldVerifier
		loadUpdateManifest = oldLoader
		newUpdateStack = oldStack
		newUpdateInstaller = oldInstaller
		loadUpdateRegistry = oldRegistry
		newUpdateInference = oldInference
		loadUpdateDaemon = oldLoadDaemon
		stopUpdateDaemon = oldStopDaemon
		startUpdateDaemon = oldStartDaemon
	})
}

func testUpdateManifest() releasebundle.Manifest {
	return releasebundle.Manifest{
		Version: "1.1.0", MinimumHostVersion: "1.0.0", ProjectSchemaFrom: 1, ProjectSchemaTo: 2,
		CodeImage:      releasebundle.Image{Reference: "registry/code@sha256:digest"},
		InferenceImage: releasebundle.Image{Reference: inference.Image},
		EmbeddingModel: releasebundle.Model{Bytes: inference.ModelBytes, SHA256: inference.ModelSHA256},
		Artifacts: []releasebundle.Artifact{
			{Kind: "host-core", OS: runtime.GOOS, Arch: runtime.GOARCH},
			{Kind: "code-image-archive", OS: "linux", Arch: "amd64"},
		},
	}
}

func testUpdateRegistry(t *testing.T, count int) *projectregistry.Registry {
	t.Helper()
	registry := projectregistry.New()
	for index := 0; index < count; index++ {
		path := filepath.Join(t.TempDir(), string(rune('a'+index)))
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		canonical, err := projectpath.Resolve(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Add(canonical, projectregistry.ProfileFull, false); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}
