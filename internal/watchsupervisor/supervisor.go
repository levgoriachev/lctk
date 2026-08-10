// Package watchsupervisor owns one filesystem watcher per running project.
//
// Watching is not free: it costs a native handle per directory, and a machine
// with a dozen registered projects would spend them on projects nobody is using.
// So a watcher is started when a project is running, woken when a client actually
// talks to that project, and released when the project stops or goes quiet. Every
// one of those transitions is a period during which nothing was observed, and
// each is recorded in the project's change journal as a gap rather than glossed
// over.
//
// The supervisor deliberately does not update any index. It produces a trustworthy
// record of what changed and how complete that record is; acting on it is the next
// slice's work. Splitting it that way keeps the honesty rule in one place.
package watchsupervisor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/lev-goryachev/lctk/internal/changejournal"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/fswatch"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
	"github.com/lev-goryachev/lctk/internal/projectmanifest"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
	"github.com/lev-goryachev/lctk/internal/projectstack"
)

// DefaultSweepInterval is how often the supervisor re-checks which projects are
// running.
//
// It is deliberately slow. Each sweep asks the container runtime about every
// registered project, and responsiveness does not come from polling: a client
// talking to a project wakes its watcher immediately.
const DefaultSweepInterval = 60 * time.Second

// Reasons the supervisor records, alongside the watcher's and the journal's.
const (
	// ReasonSuspended marks a watcher released because the project stopped or
	// went quiet. What happens next is unobserved by construction.
	ReasonSuspended = "observation_suspended"
	// ReasonWatchSetIncomplete marks a project with more directories than the
	// service will describe, so the watcher observes only part of it.
	ReasonWatchSetIncomplete = "watch_set_incomplete"
)

// Options configures a supervisor. Every dependency is injectable so the
// supervisor can be exercised without a container runtime.
type Options struct {
	Registry func() (*projectregistry.Registry, error)
	Status   func(ctx context.Context, project projectregistry.Project) (projectstack.Status, error)
	Settings func() (hostsettings.Settings, error)
	Manifest func(projectRoot string) (projectmanifest.Result, error)
	// NewClient builds the adapter to a project's code-intelligence service,
	// which is what supplies the watch set.
	NewClient func(address string) *codeintel.Client
	// Journal opens the persistent record for a project.
	Journal func(projectID string) (*changejournal.Journal, error)

	SweepInterval time.Duration
	Logger        *slog.Logger
	Now           func() time.Time
}

// View is what a status surface reports about one project's observation.
type View struct {
	ProjectID string `json:"project_id"`
	// Watching says a native watcher is currently registered. When false, the
	// pending counts describe a record nobody is adding to.
	Watching bool `json:"watching"`
	// Directories is how many native watches are held.
	Directories int `json:"directories"`
	Pending     int `json:"pending"`
	// Indexing says an index update is in flight, which is what distinguishes a
	// project that is catching up from one that is simply behind.
	Indexing   bool   `json:"indexing"`
	Sequence   uint64 `json:"sequence"`
	Checkpoint uint64 `json:"checkpoint"`
	// Generation is the index generation the checkpoint describes.
	Generation uint64 `json:"generation"`
	// Gap is present when the record is incomplete and the consumer must
	// reconcile rather than apply the pending list.
	Gap         *changejournal.Gap `json:"gap,omitempty"`
	LastEventAt time.Time          `json:"last_event_at,omitzero"`
	SettledAt   time.Time          `json:"settled_at,omitzero"`
	// AppliedAt is when the index was last brought up to date from the journal.
	AppliedAt time.Time `json:"applied_at,omitzero"`
	// LastError is why the most recent attempt to update the index failed, empty
	// when the last attempt succeeded. It is reported rather than only logged,
	// because an index that has stopped advancing looks exactly like one with
	// nothing to do.
	LastError       string  `json:"last_error,omitempty"`
	DebounceSeconds float64 `json:"debounce_seconds"`
}

// Supervisor manages the per-project watchers.
type Supervisor struct {
	options Options

	mu       sync.Mutex
	workers  map[string]*worker
	starting map[string]struct{}
	closed   bool
}

// New builds a supervisor, filling in production defaults.
func New(options Options) *Supervisor {
	if options.Registry == nil {
		options.Registry = projectregistry.Load
	}
	if options.Status == nil {
		manager := projectstack.NewManager()
		options.Status = manager.Status
	}
	if options.Settings == nil {
		options.Settings = hostsettings.Load
	}
	if options.Manifest == nil {
		options.Manifest = projectmanifest.Load
	}
	if options.NewClient == nil {
		options.NewClient = codeintel.New
	}
	if options.Journal == nil {
		options.Journal = func(projectID string) (*changejournal.Journal, error) {
			return changejournal.Open(projectID, changejournal.Options{})
		}
	}
	if options.SweepInterval <= 0 {
		options.SweepInterval = DefaultSweepInterval
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Supervisor{
		options:  options,
		workers:  map[string]*worker{},
		starting: map[string]struct{}{},
	}
}

// Run sweeps until the context is cancelled, then releases every watcher.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.Close()

	ticker := time.NewTicker(s.options.SweepInterval)
	defer ticker.Stop()

	s.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep starts watchers for running projects and releases the rest.
func (s *Supervisor) Sweep(ctx context.Context) {
	registry, err := s.options.Registry()
	if err != nil {
		s.options.Logger.Warn("watch sweep could not read the registry", slog.String("error", err.Error()))
		return
	}

	live := map[string]struct{}{}
	for _, project := range registry.List() {
		status, err := s.options.Status(ctx, project)
		if err != nil {
			// The container runtime did not answer. That is not evidence the
			// project stopped, and releasing a watcher over it would cost a
			// reconciliation on the next sweep. An existing watcher is kept; a
			// missing one is not started, because there is nothing to ask for a
			// watch set.
			if s.watching(project.ID) {
				live[project.ID] = struct{}{}
			}
			continue
		}
		if status.State != projectstack.StateRunning || status.ServiceAddress == "" {
			continue
		}
		live[project.ID] = struct{}{}
		s.ensure(ctx, project, status)
	}

	for _, id := range s.workerIDs() {
		if _, running := live[id]; !running {
			s.release(id, "the project is no longer running")
		}
	}
	s.reapIdle()
}

// Wake is called when a client uses a project. It keeps the watcher alive and
// starts one if the project has been quiet long enough to have released it.
//
// It never blocks the caller: starting a watcher means asking the project service
// for its directory list, which is an HTTP round trip and a whole-tree walk, and
// an MCP request must not wait for either.
func (s *Supervisor) Wake(project projectregistry.Project, status projectstack.Status) {
	if status.State != projectstack.StateRunning || status.ServiceAddress == "" {
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if existing, ok := s.workers[project.ID]; ok {
		s.mu.Unlock()
		existing.touch(s.options.Now())
		// A client talking to the project is the earliest evidence available that
		// it came back on a new port, and waiting for the next sweep would leave
		// its index behind for up to a minute after somebody asked about it.
		s.retarget(existing, project.ID, status.ServiceAddress)
		return
	}
	if _, pending := s.starting[project.ID]; pending {
		s.mu.Unlock()
		return
	}
	s.starting[project.ID] = struct{}{}
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), codeintel.DefaultWatchSetTimeout)
		defer cancel()
		s.start(ctx, project, status)
	}()
}

// Flush brings a project's index up to date now, rather than at the end of its
// debounce window, and waits for the caller's context.
//
// It is what lets a search see an edit made a moment ago. A project with nothing
// pending returns immediately, and a project with no watcher returns immediately
// too, because there is no record to apply.
func (s *Supervisor) Flush(ctx context.Context, projectID string) {
	s.mu.Lock()
	w, ok := s.workers[projectID]
	s.mu.Unlock()
	if !ok {
		return
	}
	w.flush(ctx)
}

// View reports the observation state for a project.
func (s *Supervisor) View(projectID string) (View, bool) {
	s.mu.Lock()
	w, ok := s.workers[projectID]
	s.mu.Unlock()
	if !ok {
		return View{}, false
	}
	return w.view(), true
}

// Close releases every watcher.
func (s *Supervisor) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	workers := make([]*worker, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	s.workers = map[string]*worker{}
	s.mu.Unlock()

	for _, w := range workers {
		w.stop(ReasonSuspended, "the daemon is shutting down")
	}
}

func (s *Supervisor) ensure(ctx context.Context, project projectregistry.Project, status projectstack.Status) {
	s.mu.Lock()
	existing, running := s.workers[project.ID]
	_, pending := s.starting[project.ID]
	if running || pending || s.closed {
		s.mu.Unlock()
		if running {
			s.retarget(existing, project.ID, status.ServiceAddress)
		}
		return
	}
	s.starting[project.ID] = struct{}{}
	s.mu.Unlock()

	s.start(ctx, project, status)
}

// retarget points an existing worker at the project's current service address.
//
// A project restarted while the daemon is running comes back on a different
// service address. Without this the worker keeps posting to the old one, and
// because a failed drain deliberately advances nothing, the index would stay
// behind for as long as the daemon lived — reported honestly the whole time, and
// never recovering. The journal and the watcher are kept: the host went on
// observing throughout, so what was pending is still pending and is now
// applicable.
func (s *Supervisor) retarget(w *worker, projectID, address string) {
	// Compared before a client is built, because this runs on the request path as
	// well as the sweep and the address is usually unchanged.
	if address == "" || w.targetAddress() == address {
		return
	}
	if !w.retarget(address, s.options.NewClient(address)) {
		return
	}
	s.options.Logger.Info("the project service moved",
		slog.String("project_id", projectID),
		slog.String("address", address))
	w.drainAsync()
}

// start opens the journal, asks the service what to watch, and begins observing.
func (s *Supervisor) start(ctx context.Context, project projectregistry.Project, status projectstack.Status) {
	defer func() {
		s.mu.Lock()
		delete(s.starting, project.ID)
		s.mu.Unlock()
	}()

	logger := s.options.Logger.With(slog.String("project_id", project.ID))

	set, err := s.options.NewClient(status.ServiceAddress).WatchSet(ctx)
	if err != nil {
		// Not an error worth failing over. The project may still be building its
		// first index; the next sweep tries again.
		logger.Debug("watch set unavailable", slog.String("error", err.Error()))
		return
	}

	journal, err := s.options.Journal(project.ID)
	if err != nil {
		logger.Warn("change journal unavailable", slog.String("error", err.Error()))
		return
	}

	watch := s.resolveWatch(project)
	if set.Truncated {
		journal.MarkGap(ReasonWatchSetIncomplete,
			"the project has more directories than the service will describe", s.options.Now())
	}

	watcher, err := fswatch.Start(fswatch.Options{
		Root:           project.Path,
		Directories:    set.Directories,
		MaxDirectories: watch.MaxWatchedDirectories,
		Now:            s.options.Now,
		Logger:         logger,
	})
	if err != nil {
		logger.Warn("watcher could not be started", slog.String("error", err.Error()))
		return
	}

	w := &worker{
		projectID: project.ID,
		journal:   journal,
		watcher:   watcher,
		watch:     watch,
		logger:    logger,
		now:       s.options.Now,
		done:      make(chan struct{}),
		finished:  make(chan struct{}),
		lastUse:   s.options.Now(),
		index:     s.options.NewClient(status.ServiceAddress),
		address:   status.ServiceAddress,
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = watcher.Close()
		return
	}
	if _, raced := s.workers[project.ID]; raced {
		s.mu.Unlock()
		_ = watcher.Close()
		return
	}
	s.workers[project.ID] = w
	s.mu.Unlock()

	go w.run()
	logger.Info("watching project",
		slog.Int("directories", watcher.Watched()),
		slog.Duration("debounce", watch.Debounce()))
}

// resolveWatch layers the project's proposal on the machine policy.
func (s *Supervisor) resolveWatch(project projectregistry.Project) hostsettings.Watch {
	settings, err := s.options.Settings()
	if err != nil {
		s.options.Logger.Warn("host settings could not be read; using defaults",
			slog.String("error", err.Error()))
		settings = hostsettings.Defaults
	}
	watch := settings.Watch

	manifest, err := s.options.Manifest(project.Path)
	if err != nil {
		// A manifest that does not parse is the registration path's problem to
		// report. Here it only means the project has no proposal.
		return watch
	}
	return watch.WithProjectDebounce(manifest.Manifest.Index.DebounceMS)
}

func (s *Supervisor) watching(projectID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.workers[projectID]
	return ok
}

func (s *Supervisor) workerIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.workers))
	for id := range s.workers {
		ids = append(ids, id)
	}
	return ids
}

func (s *Supervisor) release(projectID, detail string) {
	s.mu.Lock()
	w, ok := s.workers[projectID]
	delete(s.workers, projectID)
	s.mu.Unlock()
	if ok {
		w.stop(ReasonSuspended, detail)
	}
}

// reapIdle releases watchers for projects nobody has used and nothing has
// touched.
func (s *Supervisor) reapIdle() {
	now := s.options.Now()
	for _, id := range s.workerIDs() {
		s.mu.Lock()
		w, ok := s.workers[id]
		s.mu.Unlock()
		if ok && w.idleSince(now) {
			s.release(id, "the project has been idle")
		}
	}
}
