package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/containerruntime"
	"github.com/lev-goryachev/lctk/internal/nvidiainstall"
)

type runnerCall struct {
	args   []string
	stdout string
	stderr string
	err    error
}

type scriptedRunner struct {
	t     *testing.T
	calls []runnerCall
	seen  [][]string
}

type tunnelStub struct {
	address string
	remote  string
}

func (s *tunnelStub) Ensure(_ context.Context, _ string, remote string) (string, error) {
	s.remote = remote
	return s.address, nil
}

func (r *scriptedRunner) Run(_ context.Context, args ...string) (string, string, error) {
	r.t.Helper()
	r.seen = append(r.seen, append([]string(nil), args...))
	if len(r.calls) == 0 {
		r.t.Fatalf("unexpected runtime call: %v", args)
	}
	call := r.calls[0]
	r.calls = r.calls[1:]
	if call.args != nil && !reflect.DeepEqual(args, call.args) {
		r.t.Fatalf("runtime args = %v, want %v", args, call.args)
	}
	return call.stdout, call.stderr, call.err
}

func TestEnsureReusesThePinnedHealthyContainer(t *testing.T) {
	model := writeTestModel(t)
	health := healthyServer(t)
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{args: []string{"image", "inspect", "image@sha256:test", "--format", "{{.Id}}"}, stdout: "sha256:test\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{.State.Status}}|{{.Image}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`}, stdout: "running|sha256:test|" + ConfigRevision + "|cpu\n"},
	}}
	manager := NewManagerForTest(runner, "image@sha256:test", model, health.URL)
	status, err := manager.Ensure(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !status.Ready || len(runner.calls) != 0 {
		t.Fatalf("status = %+v, remaining calls = %d", status, len(runner.calls))
	}
}

func TestSelfTestUsesItsLongerEmbeddingClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": make([]float64, Dimensions)}},
		})
	}))
	t.Cleanup(server.Close)
	manager := NewManagerForTest(nil, "image@sha256:test", writeTestModel(t), server.URL)
	manager.healthClient = &http.Client{Timeout: time.Millisecond}
	manager.selfTestClient = &http.Client{Timeout: 100 * time.Millisecond}
	if err := manager.SelfTest(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthRejectsHTTP200WhileModelIsStillLoading(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "loading model"})
	}))
	t.Cleanup(server.Close)
	manager := NewManagerForTest(nil, "image@sha256:test", writeTestModel(t), server.URL)
	if err := manager.health(t.Context()); err == nil || !strings.Contains(err.Error(), "loading model") {
		t.Fatalf("health error = %v, want loading state", err)
	}
}

func TestProductionHealthUsesThePrivateContainerAddressThroughMachineTunnel(t *testing.T) {
	model := writeTestModel(t)
	health := healthyServer(t)
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "sha256:test\n"},
		{stdout: "running|sha256:test|" + ConfigRevision + "|cpu\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{(index .NetworkSettings.Networks "podman").IPAddress}}`}, stdout: "10.88.0.2\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{(index .NetworkSettings.Networks "podman").IPAddress}}`}, stdout: "10.88.0.2\n"},
	}}
	manager := NewManagerForTest(runner, "image@sha256:test", model, health.URL)
	manager.address = ""
	tunnel := &tunnelStub{address: strings.TrimPrefix(health.URL, "http://")}
	manager.tunnel = tunnel
	status, err := manager.Ensure(t.Context(), time.Second)
	if err != nil || !status.Ready {
		t.Fatalf("Ensure status=%+v err=%v", status, err)
	}
	if tunnel.remote != "10.88.0.2:8080" {
		t.Fatalf("machine tunnel remote=%q", tunnel.remote)
	}
}

func TestStatusUsesTheImmutableImageIDInsteadOfNormalizedReferenceText(t *testing.T) {
	model := writeTestModel(t)
	health := healthyServer(t)
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{args: []string{"image", "inspect", "registry/image:tag@sha256:test", "--format", "{{.Id}}"}, stdout: "sha256:local-id\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{.State.Status}}|{{.Image}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`}, stdout: "running|sha256:local-id|" + ConfigRevision + "|cpu\n"},
	}}
	manager := NewManagerForTest(runner, "registry/image:tag@sha256:test", model, health.URL)
	status, err := manager.Status(t.Context())
	if err != nil || !status.Ready {
		t.Fatalf("Status = %+v, %v; want the matching immutable image ID to be ready", status, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("remaining runtime calls = %d", len(runner.calls))
	}
}

func TestProductionSelfTestReusesThePrivateContainerMachineTunnel(t *testing.T) {
	model := writeTestModel(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("self-test path=%q", request.URL.Path)
		}
		vector := make([]float64, Dimensions)
		if err := json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": vector}}}); err != nil {
			t.Errorf("encode self-test response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	format := `{{(index .NetworkSettings.Networks "podman").IPAddress}}`
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{args: []string{"inspect", ContainerName, "--format", format}, stdout: "10.88.0.2\n"},
	}}
	manager := NewManagerForTest(runner, "image@sha256:test", model, "")
	tunnel := &tunnelStub{address: strings.TrimPrefix(server.URL, "http://")}
	manager.tunnel = tunnel
	if err := manager.SelfTest(t.Context()); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if tunnel.remote != "10.88.0.2:8080" {
		t.Fatalf("machine tunnel remote=%q", tunnel.remote)
	}
}

func TestEnsureStartsOneLoopbackOnlyStatelessContainer(t *testing.T) {
	model := writeTestModel(t)
	health := healthyServer(t)
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "sha256:test\n"},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stdout: "candidate-id\n"},
		{args: []string{"rename", CandidateContainerName, ContainerName}},
	}}
	manager := NewManagerForTest(runner, "image@sha256:test", model, health.URL)
	runtimeModel, err := containerruntime.HostPath(model)
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Ensure(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !status.Ready || len(runner.seen) != 6 {
		t.Fatalf("status = %+v, calls = %v", status, runner.seen)
	}
	run := strings.Join(runner.seen[4], " ")
	for _, required := range []string{
		"run --detach --name " + CandidateContainerName,
		"source=" + runtimeModel,
		"target=/models/" + ModelName + ",readonly",
		"--embedding --pooling mean",
		"--parallel 8",
		"--batch-size 4096 --ubatch-size 4096",
	} {
		if !strings.Contains(run, required) {
			t.Errorf("run args are missing %q: %s", required, run)
		}
	}
	if strings.Contains(run, "--publish") {
		t.Errorf("inference run exposes a WSL port instead of using the authenticated machine tunnel: %s", run)
	}
}

func TestNVIDIACandidateRequiresDeviceOffloadAndMeasuredBackend(t *testing.T) {
	model := writeTestModel(t)
	server := healthyServer(t)
	identity := "0123456789abcdef|sha256:gpu|2026-08-07 00:07:42.409325154 +0200 CEST|" + ConfigRevision + "|nvidia_gpu\n"
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "sha256:gpu\n"},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stdout: "candidate\n"},
		{args: []string{"exec", CandidateContainerName, "nvidia-smi", "--query-gpu=name,driver_version,memory.total,compute_cap,utilization.gpu,memory.used,power.draw,temperature.gpu", "--format=csv,noheader,nounits"}, stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 94, 4480, 132.5, 82\n"},
		{args: []string{"inspect", CandidateContainerName, "--format", `{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`}, stdout: identity},
		{args: []string{"logs", "--since", "2026-08-07T00:07:42.409325154+02:00", "--until", "2026-08-07T00:09:42.409325154+02:00", CandidateContainerName}, stderr: "ggml_cuda_init: found 1 CUDA devices\nload_tensors: offloaded 13/13 layers to GPU\nCUDA0 compute buffer"},
		{args: []string{"rename", CandidateContainerName, ContainerName}},
		{args: []string{"exec", ContainerName, "nvidia-smi", "--query-gpu=name,driver_version,memory.total,compute_cap,utilization.gpu,memory.used,power.draw,temperature.gpu", "--format=csv,noheader,nounits"}, stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 94, 4480, 132.5, 82\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{index .Config.Labels "tech.lctk.inference-config"}}|{{index .Config.Labels "tech.lctk.inference-distribution"}}`}, stdout: identity},
	}}
	manager := NewManagerForTest(runner, nvidiainstall.Image, model, server.URL)
	manager.distribution = DistributionNVIDIAGPU
	manager.evidencePath = filepath.Join(t.TempDir(), BackendEvidenceFileName)
	status, err := manager.Ensure(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.Backend != "cuda" || status.GPU == nil || status.GPUTelemetry == nil || status.GPUTelemetry.UtilizationPercent != 94 || status.OffloadedLayers != "13/13" {
		t.Fatalf("unexpected GPU status: %+v", status)
	}
	run := strings.Join(runner.seen[4], " ")
	for _, wanted := range []string{
		"--device " + nvidiainstall.CDIDevice,
		"--log-driver k8s-file --log-opt max-size=32mb",
		"--n-gpu-layers 99",
		"tech.lctk.inference-distribution=nvidia_gpu",
		"--batch-size 8192 --ubatch-size 8192",
	} {
		if !strings.Contains(run, wanted) {
			t.Errorf("GPU candidate args omit %q: %s", wanted, run)
		}
	}
}

func TestNVIDIABackendRejectsPartialLayerOffload(t *testing.T) {
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 0, 4096, 52.0, 74\n"},
		{stdout: "0123456789abcdef|sha256:gpu|2026-08-07 00:07:42.409325154 +0200 CEST|" + ConfigRevision + "|nvidia_gpu\n"},
		{stdout: "CUDA0\nload_tensors: offloaded 12/13 layers to GPU\n"},
	}}
	manager := NewManagerForTest(runner, nvidiainstall.Image, writeTestModel(t), "http://127.0.0.1:1")
	manager.distribution = DistributionNVIDIAGPU
	err := manager.attachBackendEvidence(t.Context(), CandidateContainerName, &Status{})
	if !nvidiainstall.IsCode(err, nvidiainstall.FailureCUDAOffloadMissing) {
		t.Fatalf("partial offload error = %v, want CUDA offload failure", err)
	}
}

func TestNVIDIAEvidenceIsReusedOnlyForTheSameContainerStart(t *testing.T) {
	firstIdentity := "0123456789abcdef|sha256:gpu|2026-08-07 00:07:42.409325154 +0200 CEST|" + ConfigRevision + "|nvidia_gpu\n"
	restartedIdentity := "0123456789abcdef|sha256:gpu|2026-08-07 01:07:42.409325154 +0200 CEST|" + ConfigRevision + "|nvidia_gpu\n"
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 0, 4096, 52.0, 74\n"},
		{stdout: firstIdentity},
		{stderr: "CUDA0\nload_tensors: offloaded 13/13 layers to GPU\n"},
		{stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 91, 4480, 129.0, 81\n"},
		{stdout: firstIdentity},
		{stdout: "NVIDIA GeForce GTX 1070, 582.53, 8192, 6.1, 0, 4096, 52.0, 74\n"},
		{stdout: restartedIdentity},
		{stdout: "CUDA0\nload_tensors: offloaded 12/13 layers to GPU\n"},
	}}
	manager := NewManagerForTest(runner, nvidiainstall.Image, writeTestModel(t), "http://127.0.0.1:1")
	manager.distribution = DistributionNVIDIAGPU
	manager.evidencePath = filepath.Join(t.TempDir(), BackendEvidenceFileName)

	for attempt := 0; attempt < 2; attempt++ {
		status := Status{}
		if err := manager.attachBackendEvidence(t.Context(), ContainerName, &status); err != nil {
			t.Fatalf("same-start evidence attempt %d: %v", attempt, err)
		}
		if status.OffloadedLayers != "13/13" {
			t.Fatalf("same-start status = %+v", status)
		}
	}
	err := manager.attachBackendEvidence(t.Context(), ContainerName, &Status{})
	if !nvidiainstall.IsCode(err, nvidiainstall.FailureCUDAOffloadMissing) {
		t.Fatalf("restarted container error = %v, want fresh offload failure", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unconsumed runtime calls: %+v", runner.calls)
	}
}

func TestNVIDIAEvidenceRetainsCandidateAndRollbackRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), BackendEvidenceFileName)
	identities := make([]backendEvidence, 0, backendEvidenceLimit+1)
	for index := 0; index < backendEvidenceLimit+1; index++ {
		identity, err := parseBackendEvidenceIdentity("container-" + string(rune('a'+index)) +
			"|sha256:gpu|2026-08-07 0" + string(rune('0'+index)) + ":07:42.409325154 +0200 CEST|" + ConfigRevision + "|nvidia_gpu")
		if err != nil {
			t.Fatal(err)
		}
		identity.OffloadedLayers = "13/13"
		if err := saveBackendEvidence(path, identity); err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
	}
	document, found, err := loadBackendEvidenceDocument(path)
	if err != nil || !found {
		t.Fatalf("load evidence document: found=%t err=%v", found, err)
	}
	if len(document.Records) != backendEvidenceLimit {
		t.Fatalf("records=%d want=%d", len(document.Records), backendEvidenceLimit)
	}
	if _, found, err := loadBackendEvidence(path, identities[0]); err != nil || found {
		t.Fatalf("oldest record survived bound: found=%t err=%v", found, err)
	}
	for _, identity := range identities[1:] {
		if _, found, err := loadBackendEvidence(path, identity); err != nil || !found {
			t.Fatalf("rollback-window record missing: found=%t err=%v", found, err)
		}
	}
}

func TestCandidateSwapPreservesProjectAliasAndDeletesRollbackOnlyAfterProof(t *testing.T) {
	model := writeTestModel(t)
	server := healthyServer(t)
	topology := `{"podman":{"Aliases":["0123456789ab"]},"lctk-project-net":{"Aliases":["lctk-inference","abcdef012345"]}}`
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "sha256:new\n"},
		{stdout: "running|sha256:old|6|cpu\n"},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stdout: "candidate\n"},
		{args: []string{"inspect", ContainerName, "--format", `{{json .NetworkSettings.Networks}}`}, stdout: topology},
		{args: []string{"rename", ContainerName, RollbackContainerName}},
		{args: []string{"network", "disconnect", "lctk-project-net", RollbackContainerName}},
		{args: []string{"rename", CandidateContainerName, ContainerName}},
		{args: []string{"network", "connect", "--alias", ContainerName, "lctk-project-net", ContainerName}},
		{args: []string{"rm", "--force", RollbackContainerName}},
	}}
	manager := NewManagerForTest(runner, "image@sha256:new", model, server.URL)
	status, err := manager.Ensure(t.Context(), time.Second)
	if err != nil || !status.Ready {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unconsumed runtime calls: %+v", runner.calls)
	}
}

func TestCandidateSwapFailureRestoresOldNameAndAlias(t *testing.T) {
	model := writeTestModel(t)
	server := healthyServer(t)
	topology := `{"podman":{"Aliases":["0123456789ab"]},"lctk-project-net":{"Aliases":["lctk-inference","abcdef012345"]}}`
	runner := &scriptedRunner{t: t, calls: []runnerCall{
		{stdout: "sha256:new\n"},
		{stdout: "running|sha256:old|6|cpu\n"},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stderr: "No such container", err: errors.New("exit 1")},
		{stdout: "candidate\n"},
		{stdout: topology},
		{},
		{},
		{},
		{stderr: "forced network error", err: errors.New("exit 125")},
		{args: []string{"rm", "--force", ContainerName}},
		{args: []string{"network", "connect", "--alias", ContainerName, "lctk-project-net", RollbackContainerName}},
		{args: []string{"rename", RollbackContainerName, ContainerName}},
	}}
	manager := NewManagerForTest(runner, "image@sha256:new", model, server.URL)
	_, err := manager.Ensure(t.Context(), time.Second)
	if !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("error=%v want ErrSwapFailed", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("rollback omitted calls: %+v", runner.calls)
	}
}

func TestVerifyModelRejectsAnUnpinnedFile(t *testing.T) {
	path := writeTestModel(t)
	err := VerifyModel(path)
	if !errors.Is(err, ErrModelInvalid) {
		t.Fatalf("VerifyModel error = %v, want ErrModelInvalid", err)
	}
}

func TestCancelledInstallStartsNoDownloadOrRuntimeOperation(t *testing.T) {
	runner := &scriptedRunner{t: t}
	manager := NewManagerForTest(runner, "image@sha256:test", filepath.Join(t.TempDir(), "missing.gguf"), "http://127.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.PullImage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PullImage error = %v, want context cancellation", err)
	}
	if err := manager.InstallModel(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("InstallModel error = %v, want context cancellation", err)
	}
	if len(runner.seen) != 0 {
		t.Fatalf("cancelled install executed runtime calls: %v", runner.seen)
	}
}

func writeTestModel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("test model bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func healthyServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v1/embeddings":
			vector := make([]float64, Dimensions)
			if err := json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": vector}}}); err != nil {
				t.Errorf("encode self-test response: %v", err)
			}
		default:
			t.Errorf("unexpected inference path = %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
