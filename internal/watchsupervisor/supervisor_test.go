package watchsupervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/changejournal"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/lctkhome"
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

// The debounce floor is 200ms, so a test that waits for a settle waits about
// that long. Budgets here are generous multiples of it; nothing asserts on speed.
const (
	testDebounce = 200 * time.Millisecond
	testBudget   = 10 * time.Second
)

// clock is a hand-advanced time source, so the idle policy can be exercised
// without a test that sleeps for its duration.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeService stands in for a project's code-intelligence container. It records
// what actually crossed the adapter, so a test asserts on the request the index
// received rather than only on what the journal believes.
type fakeService struct {
	server *httptest.Server

	mu         sync.Mutex
	truncated  bool
	failIndex  bool
	generation uint64
	applies    [][]codeintel.Change
	reconciles int
}

type indexRequestBody struct {
	Mode    string             `json:"mode"`
	Changes []codeintel.Change `json:"changes"`
}

func newFakeService(t *testing.T, directories []string) *fakeService {
	t.Helper()
	service := &fakeService{generation: 1}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /watchset", func(w http.ResponseWriter, _ *http.Request) {
		service.mu.Lock()
		truncated := service.truncated
		service.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"directories": directories,
			"count":       len(directories),
			"truncated":   truncated,
			"limit":       100,
		})
	})
	mux.HandleFunc("POST /index", func(w http.ResponseWriter, r *http.Request) {
		var body indexRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)

		service.mu.Lock()
		if service.failIndex {
			service.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":"INTERNAL_ERROR","message":"no","retryable":true}}`)
			return
		}
		if body.Mode == "apply" {
			service.applies = append(service.applies, body.Changes)
		} else {
			service.reconciles++
		}
		service.generation++
		generation := service.generation
		service.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"generation": generation,
			"file_count": 7,
			"applied":    len(body.Changes),
			"full_build": body.Mode == "full",
			"indexed_at": "2026-08-01T12:00:00Z",
		})
	})

	service.server = httptest.NewServer(mux)
	t.Cleanup(service.server.Close)
	return service
}

func (f *fakeService) appliedPaths() [][]codeintel.Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]codeintel.Change{}, f.applies...)
}

func (f *fakeService) reconcileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconciles
}

func (f *fakeService) setFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failIndex = failing
}

func (f *fakeService) address() string {
	return strings.TrimPrefix(f.server.URL, "http://")
}

// registryWith builds a registry holding one project without touching real user
// state. It goes through the store rather than constructing the type directly, so
// the test exercises the same document the daemon reads.
func registryWith(t *testing.T, project projectregistry.Project) *projectregistry.Registry {
	t.Helper()
	home := t.TempDir()
	t.Setenv(lctkhome.EnvOverride, home)

	document := struct {
		SchemaVersion int                       `json:"schema_version"`
		Projects      []projectregistry.Project `json:"projects"`
	}{SchemaVersion: projectregistry.SchemaVersion, Projects: []projectregistry.Project{project}}

	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, projectregistry.FileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := projectregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// control lets a test steer what the container runtime appears to say.
type control struct {
	mu      sync.Mutex
	stopped bool
	err     error
	// address overrides the service address the runtime reports, which is how a
	// test reproduces a project restarting onto a different service address.
	address string
}

func (c *control) set(stopped bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped, c.err = stopped, err
}

func (c *control) moveTo(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.address = address
}

func (c *control) readAddress(fallback string) string {
	if c == nil {
		return fallback
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.address == "" {
		return fallback
	}
	return c.address
}

func (c *control) read() (bool, error) {
	if c == nil {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped, c.err
}

type harness struct {
	*Supervisor
	root     string
	project  projectregistry.Project
	status   projectstack.Status
	journals string
	clock    *clock
}

func newHarness(t *testing.T, service *fakeService, runtime *control) *harness {
	t.Helper()

	root := t.TempDir()
	journals := t.TempDir()
	project := projectregistry.Project{ID: "proj", Name: "proj", Path: root, Key: root, Profile: projectregistry.ProfileMinimal}
	status := projectstack.Status{ProjectID: project.ID, State: projectstack.StateRunning, ServiceAddress: service.address()}
	tick := &clock{at: time.Unix(1700000000, 0).UTC()}

	registry := registryWith(t, project)

	supervisor := New(Options{
		Registry: func() (*projectregistry.Registry, error) { return registry, nil },
		Status: func(context.Context, projectregistry.Project) (projectstack.Status, error) {
			stopped, err := runtime.read()
			switch {
			case err != nil:
				return projectstack.Status{ProjectID: project.ID, State: projectstack.StateUnknown}, err
			case stopped:
				return projectstack.Status{ProjectID: project.ID, State: projectstack.StateStopped}, nil
			}
			// Copied rather than mutated: the runtime is asked from more than one
			// goroutine, and the captured status is shared.
			current := status
			current.ServiceAddress = runtime.readAddress(service.address())
			return current, nil
		},
		Settings: func() (hostsettings.Settings, error) {
			return hostsettings.Settings{
				SchemaVersion: hostsettings.SchemaVersion,
				Watch: hostsettings.Watch{
					DebounceMS:            int(testDebounce.Milliseconds()),
					MaxDebounceMS:         1000,
					MaxWatchedDirectories: 100,
					IdleStopSeconds:       60,
				},
			}, nil
		},
		Manifest:  func(string) (projectmanifest.Result, error) { return projectmanifest.Result{}, nil },
		NewClient: codeintel.New,
		Journal: func(projectID string) (*changejournal.Journal, error) {
			return changejournal.Open(projectID, changejournal.Options{
				Path: filepath.Join(journals, projectID+".json"),
				Now:  tick.Now,
			})
		},
		SweepInterval: time.Hour,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           tick.Now,
	})
	t.Cleanup(supervisor.Close)

	return &harness{
		Supervisor: supervisor,
		root:       root,
		project:    project,
		status:     status,
		journals:   journals,
		clock:      tick,
	}
}

func (h *harness) write(t *testing.T, relative, content string) {
	t.Helper()
	path := filepath.Join(h.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// awaitView polls the supervisor's own view rather than a fixed sleep, so the
// test states what it is waiting for instead of guessing how long it takes.
func (h *harness) awaitView(t *testing.T, what string, match func(View) bool) View {
	t.Helper()
	deadline := time.Now().Add(testBudget)
	var last View
	for time.Now().Before(deadline) {
		if view, ok := h.View(h.project.ID); ok {
			last = view
			if match(view) {
				return view
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no view matched %s within %s; last was %+v", what, testBudget, last)
	return View{}
}

// journalFor reaches the live journal of the running worker, so a test can record
// an observation without depending on how fast a native event arrives.
func (h *harness) journalFor(t *testing.T) *changejournal.Journal {
	t.Helper()
	h.Supervisor.mu.Lock()
	defer h.Supervisor.mu.Unlock()
	w, ok := h.Supervisor.workers[h.project.ID]
	if !ok {
		t.Fatal("the project is not being watched")
	}
	return w.journal
}

// storedJournal reads the document on disk, retrying while the writer holds it.
// On Windows the rename that publishes a new journal briefly locks the target,
// and a reader that happens to arrive then gets a sharing violation rather than
// stale content.
func (h *harness) storedJournal(t *testing.T) changejournal.Snapshot {
	t.Helper()
	deadline := time.Now().Add(testBudget)
	var last error
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(h.journals, h.project.ID+".json"))
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var snapshot changejournal.Snapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatalf("decode stored journal: %v", err)
		}
		return snapshot
	}
	t.Fatalf("read stored journal: %v", last)
	return changejournal.Snapshot{}
}

// awaitStored waits for the document on disk, which is what a restart or a status
// command would read. The in-memory view can be ahead of it by one settle.
func (h *harness) awaitStored(t *testing.T, what string, match func(changejournal.Snapshot) bool) changejournal.Snapshot {
	t.Helper()
	deadline := time.Now().Add(testBudget)
	var last changejournal.Snapshot
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(h.journals, h.project.ID+".json"))
		if err == nil {
			var snapshot changejournal.Snapshot
			if json.Unmarshal(raw, &snapshot) == nil {
				last = snapshot
				if match(snapshot) {
					return snapshot
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the stored journal never matched %s within %s; last was %+v", what, testBudget, last)
	return changejournal.Snapshot{}
}

func TestASweepStartsWatchingARunningProject(t *testing.T) {
	// "gone" is in the service's answer but not on disk, which is the ordinary
	// race between the service enumerating and the host registering. It is
	// skipped rather than treated as a failure.
	service := newFakeService(t, []string{".", "internal", "gone"})
	h := newHarness(t, service, nil)
	if err := os.MkdirAll(filepath.Join(h.root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}

	h.Sweep(context.Background())

	view, ok := h.View(h.project.ID)
	if !ok {
		t.Fatal("a running project was not picked up by the sweep")
	}
	if !view.Watching || view.Directories != 2 {
		t.Fatalf("view = %+v, want the two directories that exist to be watched", view)
	}
	if view.DebounceSeconds != testDebounce.Seconds() {
		t.Fatalf("debounce = %v seconds, want %v", view.DebounceSeconds, testDebounce.Seconds())
	}
}

// A watched project must have a document on disk before anything happens to it.
// Without one, a status command reports "never observed" about a project that is
// being observed right now, which is the opposite of the truth. This was found by
// running the daemon against a real repository, not by a fixture.
func TestAWatchedProjectHasAJournalBeforeAnythingChanges(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)

	h.Sweep(context.Background())

	stored := h.awaitStored(t, "a journal written at startup", func(s changejournal.Snapshot) bool {
		return s.ProjectID == h.project.ID
	})
	if len(stored.Pending) != 0 {
		t.Fatalf("stored pending = %+v, want an empty record", stored.Pending)
	}
}

func TestASavedFileIsObservedSequencedAndSettled(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())

	h.write(t, "main.go", "package main\n")

	view := h.awaitView(t, "the saved file being observed", func(v View) bool {
		return v.Sequence > 0 && !v.SettledAt.IsZero() && !v.LastEventAt.IsZero()
	})
	if view.Sequence == 0 {
		t.Fatal("the observation was not sequenced")
	}

	// The pending list empties because the change reaches the index; what the
	// journal must retain across a restart is the checkpoint that says so.
	h.awaitStored(t, "the checkpoint recording the applied change", func(s changejournal.Snapshot) bool {
		return len(s.Pending) == 0 && s.Checkpoint == s.Sequence && s.Sequence > 0
	})
}

// The debounce window exists so an editor's save-then-format-then-save burst
// costs the index one update, not twenty. The assertion is on what the index
// received, because that is the cost the window exists to avoid. Filesystem
// notification APIs may coalesce adjacent writes before LCTK sees them, so the
// journal sequence is deliberately not treated as a count of write calls.
func TestABurstOfSavesCostsTheIndexOneChange(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	for i := 0; i < 20; i++ {
		h.write(t, "main.go", "package main // "+strconv.Itoa(i)+"\n")
	}

	view := h.awaitView(t, "the burst reaching the index", func(v View) bool {
		return v.Sequence > 0 && v.Pending == 0 && v.Checkpoint == v.Sequence
	})
	total := 0
	for _, batch := range service.appliedPaths() {
		total += len(batch)
	}
	if total > 2 {
		t.Fatalf("the index received %d changes for %d saves of one file; the window collapsed nothing",
			total, view.Sequence)
	}
}

func TestAStoppedProjectReleasesItsWatcherAndRecordsAGap(t *testing.T) {
	service := newFakeService(t, []string{"."})
	runtime := &control{}
	h := newHarness(t, service, runtime)

	h.Sweep(context.Background())
	if _, ok := h.View(h.project.ID); !ok {
		t.Fatal("the project was not being watched to begin with")
	}

	runtime.set(true, nil)
	h.Sweep(context.Background())

	if _, ok := h.View(h.project.ID); ok {
		t.Fatal("a stopped project kept its watcher")
	}
	stored := h.storedJournal(t)
	if stored.Gap == nil || stored.Gap.Reason == "" {
		t.Fatalf("stored gap = %+v, want the unobserved period recorded", stored.Gap)
	}
}

// A request to a project is what "on-demand wakeup" means: the client using the
// project is the reason to spend handles on watching it.
func TestARequestWakesAProjectTheSweepHasNotReached(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)

	h.Wake(h.project, h.status)

	h.awaitView(t, "the woken project", func(v View) bool { return v.Watching })
}

func TestWakingATornDownSupervisorDoesNothing(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)

	h.Close()
	h.Wake(h.project, h.status)

	if _, ok := h.View(h.project.ID); ok {
		t.Fatal("a closed supervisor started a watcher")
	}
}

// A project with more directories than the service will describe is watched only
// in part, and the record has to say so rather than look complete.
func TestATruncatedWatchSetIsRecordedAsAGap(t *testing.T) {
	service := newFakeService(t, []string{"."})
	service.mu.Lock()
	service.truncated = true
	service.mu.Unlock()

	h := newHarness(t, service, nil)
	h.Sweep(context.Background())

	view, ok := h.View(h.project.ID)
	if !ok {
		t.Fatal("the project was not watched at all")
	}
	if view.Gap == nil {
		t.Fatal("a partial watch set was reported as a complete record")
	}
	if view.Gap.Reason != changejournal.ReasonStarted && view.Gap.Reason != ReasonWatchSetIncomplete {
		t.Fatalf("gap reason = %q, want the incompleteness recorded", view.Gap.Reason)
	}
}

func TestAnIdleProjectReleasesItsWatcher(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())

	if _, ok := h.View(h.project.ID); !ok {
		t.Fatal("the project was not being watched to begin with")
	}

	h.clock.Advance(2 * time.Hour)
	h.Sweep(context.Background())

	if _, ok := h.View(h.project.ID); ok {
		t.Fatal("a project nobody has used kept its watcher")
	}
}

// The managed runtime not answering is not the same as a project having stopped.
// Releasing a watcher over it would cost a reconciliation on the next sweep for
// a project that never went anywhere.
func TestAnUnreachableRuntimeDoesNotReleaseAWatcher(t *testing.T) {
	service := newFakeService(t, []string{"."})
	runtime := &control{}
	h := newHarness(t, service, runtime)

	h.Sweep(context.Background())
	if _, ok := h.View(h.project.ID); !ok {
		t.Fatal("the project was not being watched to begin with")
	}

	runtime.set(false, errors.New("the container runtime is unavailable"))
	h.Sweep(context.Background())

	if _, ok := h.View(h.project.ID); !ok {
		t.Fatal("a transient runtime failure released the watcher")
	}
}

func TestAProjectWhoseServiceIsUnreachableIsNotWatched(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	service.server.Close()

	h.Sweep(context.Background())

	if _, ok := h.View(h.project.ID); ok {
		t.Fatal("a watcher was started without knowing what to watch")
	}
}
