package setupflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/desktopinstall"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/installation"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
	"github.com/lev-goryachev/lctk/internal/runtimeinstall"
	"github.com/lev-goryachev/lctk/internal/updateflow"
	"github.com/lev-goryachev/lctk/internal/windowssetup"
)

type runtimeStub struct{ calls *[]string }

func inferenceInstalled(context.Context, inference.Distribution) (bool, bool, error) {
	return true, true, nil
}

func (s runtimeStub) Inspect(releasebundle.Manifest) (runtimeinstall.Plan, error) {
	return runtimeinstall.Plan{Ready: true, DownloadBytes: 20}, nil
}
func (s runtimeStub) Install(context.Context, releasebundle.Manifest) error {
	*s.calls = append(*s.calls, "runtime")
	return nil
}

type coreStub struct {
	home  string
	calls *[]string
}

func (s coreStub) Inspect(releasebundle.Manifest) (installation.Plan, releasebundle.Artifact, error) {
	return installation.Plan{Ready: true, DownloadBytes: 10}, releasebundle.Artifact{}, nil
}
func (s coreStub) Install(context.Context, releasebundle.Manifest) (installation.Activation, error) {
	*s.calls = append(*s.calls, "core")
	path := filepath.Join(s.home, "versions", "1.0.0", "lctk-core.exe")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return installation.Activation{}, err
	}
	if err := os.WriteFile(path, []byte("core"), 0o700); err != nil {
		return installation.Activation{}, err
	}
	return installation.NewManager(s.home).Adopt(path, "1.0.0")
}

type desktopStub struct{ calls *[]string }

func (s desktopStub) Inspect(releasebundle.Manifest) (desktopinstall.Plan, error) {
	return desktopinstall.Plan{DownloadBytes: 5}, nil
}

type failingDesktopStub struct{ calls *[]string }

func (s failingDesktopStub) Inspect(releasebundle.Manifest) (desktopinstall.Plan, error) {
	return desktopinstall.Plan{}, nil
}

type nvidiaStub struct{ downloadBytes int64 }

func (s nvidiaStub) Inspect(context.Context, releasebundle.Manifest) (nvidiainstall.Plan, error) {
	return nvidiainstall.Plan{DownloadBytes: s.downloadBytes}, nil
}
func (nvidiaStub) Ensure(context.Context, releasebundle.Manifest) (nvidiainstall.Status, error) {
	return nvidiainstall.Status{Ready: true}, nil
}
func (s failingDesktopStub) Install(context.Context, releasebundle.Manifest) (string, error) {
	*s.calls = append(*s.calls, "desktop")
	return "", errors.New("desktop activation failed")
}

type updateStub struct {
	calls    *[]string
	plan     updateflow.Plan
	home     string
	target   string
	applyErr error
	rolled   bool
}

func (s *updateStub) Inspect(context.Context, releasebundle.Manifest) (updateflow.Plan, error) {
	return s.plan, nil
}
func (s *updateStub) Apply(context.Context, releasebundle.Manifest) (updateflow.Plan, error) {
	*s.calls = append(*s.calls, "update")
	if s.applyErr == nil && s.home != "" {
		path := filepath.Join(s.home, "candidate", "lctk-core")
		if runtime.GOOS == "windows" {
			path += ".exe"
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return updateflow.Plan{}, err
		}
		if err := os.WriteFile(path, []byte("candidate"), 0o700); err != nil {
			return updateflow.Plan{}, err
		}
		if _, err := installation.NewManager(s.home).Adopt(path, s.target); err != nil {
			return updateflow.Plan{}, err
		}
	}
	return s.plan, s.applyErr
}
func (s *updateStub) Rollback(context.Context) (updateflow.Plan, error) {
	s.rolled = true
	*s.calls = append(*s.calls, "rollback")
	return updateflow.Plan{RolledBack: true}, nil
}

type upgradeCoreStub struct{}

func (upgradeCoreStub) Inspect(manifest releasebundle.Manifest) (installation.Plan, releasebundle.Artifact, error) {
	return installation.Plan{CurrentVersion: "1.0.0", TargetVersion: manifest.Version, Ready: true}, releasebundle.Artifact{}, nil
}
func (upgradeCoreStub) Install(context.Context, releasebundle.Manifest) (installation.Activation, error) {
	return installation.Activation{}, errors.New("setup bypassed the shared update transaction")
}
func (s desktopStub) Install(context.Context, releasebundle.Manifest) (string, error) {
	*s.calls = append(*s.calls, "desktop")
	return "lctk.exe", nil
}

func TestInstallAppliesTheAcceptedDependencyOrder(t *testing.T) {
	home := t.TempDir()
	var calls []string
	manager := &Manager{
		Distribution: inference.DistributionCPU,
		Home:         home, ManifestSource: "https://example/manifest",
		Runtime: runtimeStub{&calls}, Core: coreStub{home, &calls}, Desktop: desktopStub{&calls},
		ProbeHost: func(context.Context) (windowssetup.Status, error) {
			return windowssetup.Status{Supported: true, WSLReady: true}, nil
		},
		InspectInference: inferenceInstalled,
		Run: func(_ context.Context, executable string, args ...string) ([]byte, error) {
			calls = append(calls, "bootstrap")
			// ActiveExecutable returns the native release filename because setup is
			// exercised on every supported CI host, even though the desktop setup
			// itself is released only for Windows.
			coreName := "lctk-core"
			if runtime.GOOS == "windows" {
				coreName += ".exe"
			}
			if filepath.Base(executable) != coreName || !contains(args, "--yes") {
				t.Fatalf("bootstrap=%s %v", executable, args)
			}
			return []byte(`{"ready":true}`), nil
		},
		StopDaemon:  func(string) error { calls = append(calls, "stop-daemon"); return nil },
		StartDaemon: func(string) error { calls = append(calls, "start-daemon"); return nil },
	}
	if err := manager.Install(t.Context(), releasebundle.Manifest{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"runtime", "core", "bootstrap", "desktop", "start-daemon"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestInspectIncludesMissingGPUImageModelAndUnpackedRuntimeStorage(t *testing.T) {
	var calls []string
	manager := &Manager{
		Distribution: inference.DistributionNVIDIAGPU,
		Runtime:      runtimeStub{&calls}, Core: coreStub{t.TempDir(), &calls}, Desktop: desktopStub{&calls},
		NVIDIA: nvidiaStub{downloadBytes: 7},
		ProbeHost: func(context.Context) (windowssetup.Status, error) {
			return windowssetup.Status{Supported: true, WSLReady: true}, nil
		},
		ProbeNVIDIA: func(context.Context) (nvidiainstall.GPU, error) {
			return nvidiainstall.GPU{Name: "NVIDIA test GPU"}, nil
		},
		InspectInference: func(context.Context, inference.Distribution) (bool, bool, error) {
			return false, false, nil
		},
	}
	manifest := releasebundle.Manifest{
		Version:                 "1.0.0",
		NVIDIAGPUInferenceImage: releasebundle.Image{CompressedBytes: 2_586_107_421, UnpackedBytes: 4_360_073_216},
		EmbeddingModel:          releasebundle.Model{Bytes: inference.ModelBytes},
	}
	plan, err := manager.Inspect(t.Context(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	wantDownload := int64(20 + 10 + 5 + 7 + 2_586_107_421 + inference.ModelBytes)
	if plan.InferenceImageInstalled || plan.InferenceModelInstalled || plan.InferenceDownloadBytes != 2_586_107_421+inference.ModelBytes ||
		plan.InferenceRuntimeBytes != 2_586_107_421+4_360_073_216 || plan.DownloadBytes != wantDownload {
		t.Fatalf("incomplete GPU plan: %+v", plan)
	}
}

func TestDecideActionAllowsInstallUpgradeAndRepairButRejectsDowngrade(t *testing.T) {
	for _, test := range []struct {
		current string
		target  string
		want    Action
	}{
		{"", "1.0.0", ActionInstall},
		{"1.0.0", "1.1.0", ActionUpgrade},
		{"1.1.0", "1.1.0", ActionRepair},
	} {
		got, err := DecideAction(test.current, test.target)
		if err != nil || got != test.want {
			t.Fatalf("DecideAction(%q, %q) = %q, %v; want %q", test.current, test.target, got, err, test.want)
		}
	}
	if _, err := DecideAction("1.1.0", "1.0.0"); err == nil {
		t.Fatal("DecideAction accepted a downgrade")
	}
}

func TestUpgradeUsesTheSharedTransactionAndRestartsTheDaemon(t *testing.T) {
	home := t.TempDir()
	var calls []string
	updater := &updateStub{calls: &calls, home: home, target: "1.1.0", plan: updateflow.Plan{
		CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Ready: true,
		Host: installation.Plan{CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Ready: true},
	}}
	manager := &Manager{
		Distribution: inference.DistributionCPU,
		Home:         home, ManifestSource: "https://example/manifest",
		Runtime: runtimeStub{&calls}, Core: upgradeCoreStub{}, Desktop: desktopStub{&calls},
		ProbeHost: func(context.Context) (windowssetup.Status, error) {
			return windowssetup.Status{Supported: true, WSLReady: true}, nil
		},
		InspectInference: inferenceInstalled,
		NewUpdate:        func(string) updateCoordinator { return updater },
		StopDaemon:       func(string) error { calls = append(calls, "stop-daemon"); return nil },
		StartDaemon:      func(string) error { calls = append(calls, "start-daemon"); return nil },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			calls = append(calls, "bootstrap")
			return []byte(`{"ready":true}`), nil
		},
	}
	if err := manager.Install(t.Context(), releasebundle.Manifest{Version: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"stop-daemon", "update", "runtime", "bootstrap", "desktop", "start-daemon"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestUpgradeRollsBackAndRestartsThePreviousDaemonAfterALaterFailure(t *testing.T) {
	home := t.TempDir()
	var calls []string
	updater := &updateStub{calls: &calls, home: home, target: "1.1.0", plan: updateflow.Plan{
		CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Ready: true,
		Host: installation.Plan{CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Ready: true},
	}}
	manager := &Manager{
		Distribution: inference.DistributionCPU,
		Home:         home, ManifestSource: "https://example/manifest",
		Runtime: runtimeStub{&calls}, Core: upgradeCoreStub{}, Desktop: failingDesktopStub{&calls},
		ProbeHost: func(context.Context) (windowssetup.Status, error) {
			return windowssetup.Status{Supported: true, WSLReady: true}, nil
		},
		InspectInference: inferenceInstalled,
		NewUpdate:        func(string) updateCoordinator { return updater },
		StopDaemon:       func(string) error { calls = append(calls, "stop-daemon"); return nil },
		StartDaemon:      func(string) error { calls = append(calls, "start-daemon"); return nil },
		Run: func(context.Context, string, ...string) ([]byte, error) {
			calls = append(calls, "bootstrap")
			return []byte(`{"ready":true}`), nil
		},
	}
	err := manager.Install(t.Context(), releasebundle.Manifest{Version: "1.1.0"})
	if err == nil || !strings.Contains(err.Error(), "desktop activation failed") {
		t.Fatalf("Install error = %v", err)
	}
	want := []string{"stop-daemon", "update", "runtime", "bootstrap", "desktop", "rollback", "start-daemon"}
	if !reflect.DeepEqual(calls, want) || !updater.rolled {
		t.Fatalf("calls=%v rolled=%t want=%v", calls, updater.rolled, want)
	}
}

func TestInstallStopsForARequiredReboot(t *testing.T) {
	var enabled, resumed bool
	manager := &Manager{
		Distribution: inference.DistributionCPU,
		Runtime:      runtimeStub{new([]string)}, Core: coreStub{t.TempDir(), new([]string)}, Desktop: desktopStub{new([]string)},
		ProbeHost: func(context.Context) (windowssetup.Status, error) {
			return windowssetup.Status{Supported: true, RequiresEnablement: true}, nil
		},
		InspectInference: inferenceInstalled,
		EnableWSL:        func(context.Context) (bool, error) { enabled = true; return true, nil },
		RegisterResume:   func() error { resumed = true; return nil },
	}
	if err := manager.Install(t.Context(), releasebundle.Manifest{}); !errors.Is(err, ErrRebootRequired) {
		t.Fatalf("err=%v", err)
	}
	if !enabled || !resumed {
		t.Fatalf("enabled=%t resumed=%t", enabled, resumed)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
