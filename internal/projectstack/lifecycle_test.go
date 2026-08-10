package projectstack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/inference"
	"github.com/lev-goryachev/lctk/internal/releasebundle"
)

// fakeRunner answers container-runtime commands from a script, so lifecycle logic
// is testable without a managed WSL machine.
type fakeRunner struct {
	// responses maps a matched substring of the joined arguments to a reply.
	responses []fakeResponse
	calls     [][]string
}

type fakeResponse struct {
	match  string
	stdout string
	stderr string
	err    error
}

type inferenceReadyStub struct{}

func (inferenceReadyStub) Ensure(context.Context, time.Duration) (inference.Status, error) {
	return inference.Status{Ready: true}, nil
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for _, response := range f.responses {
		if strings.Contains(joined, response.match) {
			return response.stdout, response.stderr, response.err
		}
	}
	return "", "", nil
}

// callWith returns the first recorded call containing every fragment.
func (f *fakeRunner) callWith(fragments ...string) []string {
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(joined, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return call
		}
	}
	return nil
}

func healthyRunner() *fakeRunner {
	return &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"linux"}}`},
		{match: "image inspect", stdout: "sha256:abc\n"},
		{match: "inspect", stdout: "running healthy\n"},
	}}
}

func TestStateRetryableOnlyWhileStarting(t *testing.T) {
	if !StateStarting.Retryable() {
		t.Error("starting should be retryable so a caller waits rather than failing")
	}
	for _, state := range []State{StateStopped, StateRunning, StateError, StateUnknown} {
		if state.Retryable() {
			t.Errorf("%s should not be retryable", state)
		}
	}
}

func TestRuntimeUnavailableIsDistinguishable(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stderr: "the lctk-runtime machine is not running", err: errors.New("exit status 1")},
	}}
	manager := NewManagerWithRunner(runner)

	err := manager.RuntimeAvailable(context.Background())
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("got %v, want ErrRuntimeUnavailable", err)
	}
	// A caller must be able to tell "managed runtime is stopped" from "this project broke".
	if errors.Is(err, ErrImageMissing) || errors.Is(err, ErrInvalidProject) {
		t.Error("runtime failure was conflated with a project failure")
	}

	if _, err := manager.Start(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")), 0); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Errorf("Start: got %v, want ErrRuntimeUnavailable", err)
	}
}

// TestNonLinuxRuntimeIsRejectedEarly covers a connection accidentally pointed
// at a provider that cannot satisfy ADR-0011's Linux boundary.
func TestNonLinuxRuntimeIsRejectedEarly(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"windows"}}`},
	}}
	manager := NewManagerWithRunner(runner)

	err := manager.RuntimeAvailable(context.Background())
	if !errors.Is(err, ErrLinuxContainersRequired) {
		t.Fatalf("got %v, want ErrLinuxContainersRequired", err)
	}
	// The two conditions must stay distinguishable: one means the machine is
	// unavailable, the other means the selected connection is wrong.
	if errors.Is(err, ErrRuntimeUnavailable) {
		t.Error("Windows container mode was conflated with an unreachable runtime")
	}
	if !strings.Contains(err.Error(), "windows") {
		t.Errorf("error should identify the incompatible host: %v", err)
	}

	// The check must run before anything tries to pull or build an image.
	if _, err := manager.Start(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")), 0); !errors.Is(err, ErrLinuxContainersRequired) {
		t.Errorf("Start: got %v, want ErrLinuxContainersRequired", err)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "build") || strings.Contains(joined, "run") {
			t.Errorf("work was attempted despite an unusable runtime: %v", call)
		}
	}
}

func TestRuntimeWithoutAnOSFieldIsRejected(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{}}`},
	}}
	if err := NewManagerWithRunner(runner).RuntimeAvailable(context.Background()); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Errorf("got %v, want ErrRuntimeUnavailable", err)
	}
}

func TestMissingImageIsDistinguishable(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"linux"}}`},
		{match: "image inspect", stderr: "No such image", err: errors.New("exit status 1")},
	}}
	manager := NewManagerWithRunner(runner)

	_, err := manager.Start(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")), 0)
	if !errors.Is(err, ErrImageMissing) {
		t.Fatalf("got %v, want ErrImageMissing", err)
	}
	if !strings.Contains(err.Error(), ImageRepository) {
		t.Errorf("error should name the image: %v", err)
	}
}

func TestInstallImageArchiveVerifiesTransportAndOCIDigest(t *testing.T) {
	body := []byte("verified container image archive")
	digest := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	expectedOCI := strings.Repeat("a", 64)
	runner := &fakeRunner{responses: []fakeResponse{{
		match: "image inspect", stdout: "sha256:" + expectedOCI + "\n",
	}}}
	manager := NewManagerWithRunner(runner)
	manager.imageClient = server.Client()
	manager.loadImageArchive = func(_ context.Context, input io.Reader) (string, string, error) {
		loaded, err := io.ReadAll(input)
		if err != nil || string(loaded) != string(body) {
			t.Fatalf("loaded archive = %q, err = %v", loaded, err)
		}
		return "Loaded image", "", nil
	}
	t.Setenv("LCTK_HOME", t.TempDir())
	artifact := releasebundle.Artifact{
		Name: "lctk-code-intel.tar", Kind: "code-image-archive", OS: "linux", Arch: "amd64",
		URL: server.URL, Bytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	if err := manager.InstallImageArchive(t.Context(), "localhost/lctk/code-intel@sha256:"+expectedOCI, "0.1.6", artifact); err != nil {
		t.Fatalf("InstallImageArchive: %v", err)
	}
	if runner.callWith("image", "inspect", ImageRepository+":0.1.6") == nil {
		t.Fatal("loaded product tag was not verified")
	}
}

func TestInstallImageArchiveRejectsLoadedDigestMismatch(t *testing.T) {
	body := []byte("verified container image archive")
	digest := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	runner := &fakeRunner{responses: []fakeResponse{{match: "image inspect", stdout: "sha256:" + strings.Repeat("b", 64)}}}
	manager := NewManagerWithRunner(runner)
	manager.imageClient = server.Client()
	manager.loadImageArchive = func(context.Context, io.Reader) (string, string, error) { return "", "", nil }
	t.Setenv("LCTK_HOME", t.TempDir())
	artifact := releasebundle.Artifact{Name: "lctk-code-intel.tar", Kind: "code-image-archive", OS: "linux", Arch: "amd64", URL: server.URL,
		Bytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}
	err := manager.InstallImageArchive(t.Context(), "localhost/lctk/code-intel@sha256:"+strings.Repeat("a", 64), "0.1.6", artifact)
	if err == nil || !strings.Contains(err.Error(), "differs from signed") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestInspectMapsRuntimeStateToLifecycleState(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string
		stderr    string
		err       error
		want      State
		wantError bool
	}{
		{name: "healthy", stdout: "running healthy\n", want: StateRunning},
		{name: "health starting", stdout: "running starting\n", want: StateStarting},
		{name: "no healthcheck", stdout: "running\n", want: StateStarting},
		{name: "unhealthy", stdout: "running unhealthy\n", want: StateError},
		{name: "created", stdout: "created\n", want: StateStarting},
		{name: "restarting", stdout: "restarting\n", want: StateStarting},
		{name: "exited", stdout: "exited\n", want: StateStopped},
		{name: "dead", stdout: "dead\n", want: StateStopped},
		{name: "paused", stdout: "paused\n", want: StateStopped},
		{
			name:   "absent container is simply stopped",
			stderr: "Error: No such object: lctk-alpha-abcd1234-code-intel",
			err:    errors.New("exit status 1"),
			want:   StateStopped,
		},
		{
			name:      "unexplained failure is unknown",
			stderr:    "something went badly wrong",
			err:       errors.New("exit status 1"),
			want:      StateUnknown,
			wantError: true,
		},
		{name: "unrecognized state", stdout: "teleporting\n", want: StateUnknown},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &fakeRunner{responses: []fakeResponse{
				{match: "info", stdout: `{"host":{"os":"linux"}}`},
				{match: "inspect", stdout: testCase.stdout, stderr: testCase.stderr, err: testCase.err},
			}}
			manager := NewManagerWithRunner(runner)

			status, err := manager.Status(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")))
			if testCase.wantError && err == nil {
				t.Error("expected an error")
			}
			if !testCase.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if status.State != testCase.want {
				t.Errorf("state = %q, want %q", status.State, testCase.want)
			}
			// Every status must carry the resource names so a caller can act on
			// it without a second lookup.
			if status.Image == "" || status.Network == "" || status.Volume == "" {
				t.Errorf("status is missing resource names: %+v", status)
			}
			if status.State != StateRunning && status.State != StateStopped && status.Detail == "" {
				t.Errorf("state %q has no explanation", status.State)
			}
		})
	}
}

func TestStartWritesRuntimePlanAndPinsEveryResource(t *testing.T) {
	isolate(t)
	runner := healthyRunner()
	manager := NewManagerWithRunner(runner)
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))

	status, err := manager.Start(context.Background(), project, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateRunning {
		t.Errorf("state = %q, want running", status.State)
	}

	planPath, err := RuntimePlanPath(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("runtime plan was not persisted: %v", err)
	}
	run := runner.callWith("run", "--name", "lctk-alpha-abcd1234-code-intel")
	if run == nil {
		t.Fatalf("no pinned project run was made; calls: %v", runner.calls)
	}
	joined := strings.Join(run, " ")
	for _, identity := range []string{"lctk-alpha-abcd1234-net", "lctk-alpha-abcd1234-state", project.ID} {
		if !strings.Contains(joined, identity) {
			t.Errorf("project run omits %q: %v", identity, run)
		}
	}
}

func TestStartConnectsInferenceOnlyToTheProjectNetwork(t *testing.T) {
	isolate(t)
	runner := healthyRunner()
	manager := NewManagerWithRunner(runner).WithInference(inferenceReadyStub{})
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))
	if _, err := manager.Start(t.Context(), project, 0); err != nil {
		t.Fatal(err)
	}
	connect := runner.callWith("network", "connect", "--alias", inference.ContainerName, "lctk-alpha-abcd1234-net")
	if connect == nil || strings.Contains(strings.Join(connect, " "), "lctk-beta") {
		t.Fatalf("inference was not bounded to the project's network: %v", runner.calls)
	}
}

// TestStopPreservesTheProjectVolume guards the remove-versus-purge distinction: a
// stop that deleted volumes would destroy indexes and memory.
func TestStopPreservesTheProjectVolume(t *testing.T) {
	isolate(t)
	runner := healthyRunner()
	manager := NewManagerWithRunner(runner)
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))

	status, err := manager.Stop(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateStopped {
		t.Errorf("state = %q, want stopped", status.State)
	}

	remove := runner.callWith("rm", "--force", "lctk-alpha-abcd1234-code-intel")
	if remove == nil {
		t.Fatalf("no container removal was made; calls: %v", runner.calls)
	}
	if disconnect := runner.callWith("network", "disconnect", "lctk-alpha-abcd1234-net", inference.ContainerName); disconnect == nil {
		t.Fatalf("stop left inference attached to the project network: %v", runner.calls)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "volume rm") || strings.Contains(joined, "--volumes") {
			t.Fatalf("stop asked Podman to delete project state: %v", call)
		}
	}
}

func TestRestartStopsThenStarts(t *testing.T) {
	isolate(t)
	runner := healthyRunner()
	manager := NewManagerWithRunner(runner)
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))

	if _, err := manager.Restart(context.Background(), project, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	var order []string
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "rm --force"):
			order = append(order, "down")
		case strings.Contains(joined, "run --detach"):
			order = append(order, "up")
		}
	}
	if len(order) != 2 || order[0] != "down" || order[1] != "up" {
		t.Errorf("restart order = %v, want down then up", order)
	}
}

func TestWaitForHealthTimesOutWithTheLastObservedState(t *testing.T) {
	isolate(t)
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"linux"}}`},
		{match: "image inspect", stdout: "sha256:abc\n"},
		// Never becomes healthy.
		{match: "inspect", stdout: "running starting\n"},
	}}
	manager := NewManagerWithRunner(runner)

	status, err := manager.Start(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")), 400*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	// The caller must learn that it was still starting, which is retryable, not
	// that it failed outright.
	if status.State != StateStarting {
		t.Errorf("state = %q, want starting", status.State)
	}
	if !status.State.Retryable() {
		t.Error("a timeout while starting should be retryable")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("error should explain the timeout: %v", err)
	}
}

func TestWaitForHealthFailsFastOnUnhealthy(t *testing.T) {
	isolate(t)
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"linux"}}`},
		{match: "image inspect", stdout: "sha256:abc\n"},
		{match: "inspect", stdout: "running unhealthy\n"},
	}}
	manager := NewManagerWithRunner(runner)

	started := time.Now()
	status, err := manager.Start(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")), 10*time.Second)
	if err == nil {
		t.Fatal("expected a failure")
	}
	if status.State != StateError {
		t.Errorf("state = %q, want error", status.State)
	}
	// An unhealthy container is terminal, so waiting out the budget would waste
	// the caller's time.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("waited %s before reporting a terminal failure", elapsed)
	}
}

func TestBuildImageReportsTheFailure(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "build", stderr: "failed to solve: no such file", err: errors.New("exit status 1")},
	}}
	manager := NewManagerWithRunner(runner)

	err := manager.BuildImage(context.Background(), "lctk/code-intel:test", "missing-dir")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should carry the runtime message: %v", err)
	}
}

func TestImageMatchesRequiresTheTagAndSignedDigestToShareAnIdentity(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "image inspect lctk/code-intel:1.0.0", stdout: "sha256:expected\n"},
		{match: "image inspect ghcr.io/example/code@sha256:abc", stdout: "sha256:expected\n"},
	}}
	manager := NewManagerWithRunner(runner)

	matches, err := manager.ImageMatches(t.Context(), "lctk/code-intel:1.0.0", "ghcr.io/example/code@sha256:abc")
	if err != nil || !matches {
		t.Fatalf("ImageMatches = %t, %v; want true", matches, err)
	}

	runner.responses[1].stdout = "sha256:foreign\n"
	matches, err = manager.ImageMatches(t.Context(), "lctk/code-intel:1.0.0", "ghcr.io/example/code@sha256:abc")
	if err != nil || matches {
		t.Fatalf("ImageMatches = %t, %v; want false for a mutable foreign tag", matches, err)
	}
}

func TestImageMatchesTreatsOnlyARealMissingImageAsAbsent(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{
			match: "image inspect",
			stderr: `Error: unable to inspect "lctk/code-intel:0.1.1": failed to find image ` +
				`lctk/code-intel:0.1.1: lctk/code-intel:0.1.1: image not known`,
			err: errors.New("exit status 1"),
		},
	}}
	manager := NewManagerWithRunner(runner)
	if matches, err := manager.ImageMatches(t.Context(), "missing", "also-missing"); err != nil || matches {
		t.Fatalf("missing ImageMatches = %t, %v", matches, err)
	}

	runner.responses[0].stderr = "permission denied"
	if _, err := manager.ImageMatches(t.Context(), "forbidden", "unused"); err == nil {
		t.Fatal("ImageMatches hid a non-missing runtime inspection failure")
	}
}

func TestSchemaRollbackKeepsAnActiveDatabaseUntilAtomicReplacement(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{{match: "run"}}}
	manager := NewManagerWithRunner(runner)
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))

	if err := manager.RestoreSchemaRollback(t.Context(), project, "1.0.0"); err != nil {
		t.Fatalf("RestoreSchemaRollback: %v", err)
	}
	call := runner.callWith("run", "--rm")
	if call == nil {
		t.Fatalf("no isolated rollback helper was run: %v", runner.calls)
	}
	script := call[len(call)-1]
	if !strings.Contains(script, `ln -f "$db" "$failed"`) ||
		!strings.Contains(script, `mv -f "$rollback" "$db"`) {
		t.Fatalf("rollback does not preserve then atomically replace the database:\n%s", script)
	}
	if strings.Contains(script, `mv "$db" "$failed"`) {
		t.Fatalf("rollback removes the active database before commit:\n%s", script)
	}
}

func TestIsNoSuchContainer(t *testing.T) {
	if !isNoSuchContainer("", "Error: No such object: abc") {
		t.Error("no such object was not recognized")
	}
	if !isNoSuchContainer("", "no such container: abc") {
		t.Error("no such container was not recognized")
	}
	if isNoSuchContainer("", "permission denied") {
		t.Error("an unrelated error was treated as a missing container")
	}
}
