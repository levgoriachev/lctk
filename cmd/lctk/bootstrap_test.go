package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/projectstack"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

type bootstrapRunner struct {
	codeImage   bool
	tagID       string
	referenceID string
}

func (r bootstrapRunner) Run(_ context.Context, args ...string) (string, string, error) {
	if len(args) > 0 && args[0] == "info" {
		return `{"host":{"os":"linux"}}`, "", nil
	}
	if len(args) > 2 && args[0] == "image" && args[1] == "inspect" && r.codeImage {
		if strings.Contains(args[2], "@sha256:") && r.referenceID != "" {
			return r.referenceID + "\n", "", nil
		}
		if r.tagID != "" {
			return r.tagID + "\n", "", nil
		}
		return "sha256:code\n", "", nil
	}
	return "", "missing", context.Canceled
}

type bootstrapInferenceStub struct {
	image, model                       bool
	pulled, installed, ensured, tested bool
}

func (s *bootstrapInferenceStub) ImageAvailable(context.Context) bool { return s.image }
func (s *bootstrapInferenceStub) ModelAvailable() bool                { return s.model }
func (s *bootstrapInferenceStub) PullImage(context.Context) error {
	s.pulled = true
	s.image = true
	return nil
}
func (s *bootstrapInferenceStub) InstallModel(context.Context, *http.Client) error {
	s.installed = true
	s.model = true
	return nil
}
func (s *bootstrapInferenceStub) Ensure(context.Context, time.Duration) (inference.Status, error) {
	s.ensured = true
	return inference.Status{Ready: true}, nil
}
func (s *bootstrapInferenceStub) SelfTest(context.Context) error {
	s.tested = true
	return nil
}

func TestBootstrapWithoutConfirmationIsAReadOnlyPlan(t *testing.T) {
	stub := &bootstrapInferenceStub{}
	restoreBootstrapFactories(t, bootstrapRunner{codeImage: true}, stub)
	var output bytes.Buffer
	if err := runBootstrap(t.Context(), nil, &output); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if stub.pulled || stub.installed || stub.ensured || stub.tested {
		t.Fatalf("read-only plan mutated state: %+v", stub)
	}
	for _, wanted := range []string{"writes: false", "No changes applied", inference.ModelSHA256} {
		if !strings.Contains(output.String(), wanted) {
			t.Errorf("plan is missing %q:\n%s", wanted, output.String())
		}
	}
}

func TestConfirmedBootstrapInstallsAndFunctionallyTestsMissingComponents(t *testing.T) {
	stub := &bootstrapInferenceStub{}
	restoreBootstrapFactories(t, bootstrapRunner{codeImage: true}, stub)
	var output bytes.Buffer
	if err := runBootstrap(t.Context(), []string{"--yes", "--json"}, &output); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if !stub.pulled || !stub.installed || !stub.ensured || !stub.tested {
		t.Fatalf("applied bootstrap did not complete every gate: %+v", stub)
	}
	for _, wanted := range []string{`"applied": true`, `"self_test": true`, `"ready": true`} {
		if !strings.Contains(output.String(), wanted) {
			t.Errorf("result is missing %q:\n%s", wanted, output.String())
		}
	}
}

func TestOfficialBootstrapAlwaysVerifiesManifestAndRejectsAMutableForeignTag(t *testing.T) {
	runner := bootstrapRunner{codeImage: true, tagID: "sha256:foreign", referenceID: "sha256:signed"}
	stub := &bootstrapInferenceStub{image: true, model: true}
	restoreBootstrapFactories(t, runner, stub)
	oldVersion := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = oldVersion })

	loaded := false
	newBootstrapVerifier = func() (releasebundle.Verifier, error) { return releasebundle.Verifier{}, nil }
	loadBootstrapManifest = func(context.Context, string, releasebundle.Verifier) (releasebundle.Manifest, error) {
		loaded = true
		return releasebundle.Manifest{
			Version: "1.0.0",
			CodeImage: releasebundle.Image{
				Reference:       "ghcr.io/example/code@sha256:signed",
				CompressedBytes: 123,
			},
			InferenceImage: releasebundle.Image{Reference: inference.Image},
			EmbeddingModel: releasebundle.Model{SHA256: inference.ModelSHA256, Bytes: inference.ModelBytes},
		}, nil
	}

	var output bytes.Buffer
	if err := runBootstrap(t.Context(), []string{"--plan", "--json"}, &output); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if !loaded {
		t.Fatal("official bootstrap skipped its signed release manifest")
	}
	var plan bootstrapPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	for _, component := range plan.Components {
		if component.Name == "code-intel" {
			if component.Installed || component.DownloadBytes != 123 {
				t.Fatalf("mutable foreign tag was trusted: %+v", component)
			}
			return
		}
	}
	t.Fatal("bootstrap plan omitted code-intel")
}

func restoreBootstrapFactories(t *testing.T, runner projectstack.Runner, shared bootstrapInference) {
	t.Helper()
	t.Setenv("LCTK_HOME", t.TempDir())
	oldStack := newStackManager
	oldInference := newBootstrapInference
	oldVerifier := newBootstrapVerifier
	oldLoader := loadBootstrapManifest
	newStackManager = func() *projectstack.Manager { return projectstack.NewManagerWithRunner(runner) }
	newBootstrapInference = func(inference.Distribution) (bootstrapInference, error) { return shared, nil }
	t.Cleanup(func() {
		newStackManager = oldStack
		newBootstrapInference = oldInference
		newBootstrapVerifier = oldVerifier
		loadBootstrapManifest = oldLoader
	})
}
