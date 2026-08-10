package uninstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/projectpath"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

type machineStub struct{ calls [][]string }

func (m *machineStub) Run(_ context.Context, args ...string) (string, string, error) {
	m.calls = append(m.calls, append([]string(nil), args...))
	return "", "", nil
}

type failingMachineStub struct{ err error }

func (m failingMachineStub) Run(context.Context, ...string) (string, string, error) {
	return "", "", m.err
}

func TestPartialUninstallContinuesOnlyAfterSystemConfirmsTheMachineIsAbsent(t *testing.T) {
	home := t.TempDir()
	verified := false
	manager := &Manager{
		Home: home, Registry: func() (*projectregistry.Registry, error) { return projectregistry.New(), nil },
		Machine:       failingMachineStub{err: containerruntime.ErrClientMissing},
		MachineAbsent: func(context.Context) (bool, error) { verified = true; return true, nil },
		StopDaemon:    func(string) error { return nil }, Unregister: func() error { return nil },
		Export:      func(context.Context, string, string) error { return nil },
		RuntimeData: func() (string, error) { return filepath.Join(home, "runtime-data"), nil },
		UserHome:    func() (string, error) { return filepath.Join(home, "user"), nil },
		Cleanup:     func(string, string) error { return nil }, Remove: func(string) error { return nil },
	}
	if _, err := manager.Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("system WSL absence verification was skipped")
	}
}

func TestCleanupContinuesAfterIndependentUnregisterFailure(t *testing.T) {
	home := t.TempDir()
	unregisterErr := errors.New("Start menu unavailable")
	var removed string
	manager := &Manager{
		Home: home, Registry: func() (*projectregistry.Registry, error) { return projectregistry.New(), nil }, Machine: &machineStub{},
		StopDaemon: func(string) error { return nil }, Unregister: func() error { return unregisterErr }, Export: func(context.Context, string, string) error { return nil },
		RuntimeData: func() (string, error) { return filepath.Join(home, "runtime-data"), nil }, UserHome: func() (string, error) { return filepath.Join(home, "user"), nil },
		Cleanup: func(string, string) error { return nil }, Remove: func(path string) error { removed = path; return nil },
	}
	if _, err := manager.Run(t.Context(), false); !errors.Is(err, unregisterErr) {
		t.Fatalf("error=%v want=%v", err, unregisterErr)
	}
	if removed != home {
		t.Fatalf("home cleanup did not run after unregister failure: removed=%q", removed)
	}
}

func TestPreservingUninstallExportsProjectStateBeforeRemovingRuntime(t *testing.T) {
	home := t.TempDir()
	projectDir := filepath.Join(t.TempDir(), "alpha")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	canonical, err := projectpath.Resolve(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := projectregistry.New()
	project, err := registry.Add(canonical, projectregistry.ProfileFull, false)
	if err != nil {
		t.Fatal(err)
	}
	machine := &machineStub{}
	var exported, removed []string
	manager := &Manager{
		Home: home, Registry: func() (*projectregistry.Registry, error) { return registry, nil }, Machine: machine,
		StopDaemon: func(string) error { return nil }, Unregister: func() error { return nil },
		RuntimeData: func() (string, error) { return filepath.Join(home, "runtime-data"), nil },
		UserHome:    func() (string, error) { return filepath.Join(home, "user"), nil },
		Cleanup:     func(string, string) error { return nil },
		Export:      func(_ context.Context, volume, target string) error { exported = []string{volume, target}; return nil },
		Remove:      func(path string) error { removed = append(removed, path); return nil },
	}
	backup, err := manager.Run(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" || len(exported) != 2 || !strings.Contains(exported[0], project.ID) || exported[1] != filepath.Join(backup, project.ID+".tar") {
		t.Fatalf("backup=%q exported=%v", backup, exported)
	}
	if len(machine.calls) != 1 || strings.Join(machine.calls[0], " ") != "rm --force lctk-runtime" {
		t.Fatalf("machine=%v", machine.calls)
	}
	if len(removed) == 0 || removed[0] == home {
		t.Fatalf("preserving uninstall removed the whole home: %v", removed)
	}
}

func TestDestructiveUninstallRequiresTheExplicitFalseChoice(t *testing.T) {
	home := t.TempDir()
	machine := &machineStub{}
	var removed string
	manager := &Manager{Home: home, Registry: func() (*projectregistry.Registry, error) { return projectregistry.New(), nil }, Machine: machine,
		StopDaemon: func(string) error { return nil }, Unregister: func() error { return nil }, Export: func(context.Context, string, string) error { return nil },
		RuntimeData: func() (string, error) { return filepath.Join(home, "runtime-data"), nil }, UserHome: func() (string, error) { return filepath.Join(home, "user"), nil },
		Cleanup: func(string, string) error { return nil },
		Remove:  func(path string) error { removed = path; return nil }}
	if _, err := manager.Run(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if removed != home {
		t.Fatalf("removed=%q want=%q", removed, home)
	}
}
