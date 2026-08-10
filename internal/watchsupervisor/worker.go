package watchsupervisor

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lev-goryachev/lctk/internal/changejournal"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/fswatch"
	"github.com/lev-goryachev/lctk/internal/hostsettings"
)

// worker observes one project, keeps its journal, and applies it to the index.
type worker struct {
	projectID string
	journal   *changejournal.Journal
	watcher   *fswatch.Watcher
	watch     hostsettings.Watch
	logger    *slog.Logger
	now       func() time.Time

	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}

	// drainMu serializes index updates. Two concurrent drains would read the same
	// pending list and apply it twice, and the second would commit a checkpoint
	// covering work the first had already retracted.
	drainMu sync.Mutex
	// draining says an index update is in flight. It is reported rather than kept
	// private, because "behind and catching up" and "behind and not" call for
	// different things from a caller: one is worth waiting for.
	draining atomic.Bool
	// stopped refuses new index updates once the worker is being released.
	stopped atomic.Bool

	mu      sync.Mutex
	lastUse time.Time
	// index and address are the service this worker applies to. They are mutable
	// because a project restarted while the daemon runs comes back on a different
	// service address, and the watcher and the journal outlive that.
	index     indexClient
	address   string
	settledAt time.Time
	appliedAt time.Time
	lastError string
}

// target returns the service the worker should apply to now.
func (w *worker) target() indexClient {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.index
}

// targetAddress returns where that service is, for a caller deciding whether
// anything needs to change.
func (w *worker) targetAddress() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.address
}

// retarget points the worker at a different address, keeping everything else.
//
// Nothing is recorded as a gap. The host never stopped observing — what failed
// was applying the record, not producing it — so the pending list is still
// complete and is simply applicable again. Marking a gap here would turn a
// recoverable lag into a full reconciliation for no reason.
func (w *worker) retarget(address string, client indexClient) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if address == "" || address == w.address {
		return false
	}
	w.address, w.index = address, client
	// The failure that made this necessary is no longer what is happening, and
	// leaving it in the view would report a stale reason for a lag about to clear.
	w.lastError = ""
	return true
}

// indexClient is the part of the code-intelligence adapter a worker drives. It is
// an interface so the drain can be tested without a container.
type indexClient interface {
	Apply(ctx context.Context, changes []codeintel.Change) (codeintel.IndexResult, error)
	Reconcile(ctx context.Context, full bool) (codeintel.IndexResult, error)
}

// run applies the debounce policy to the event stream.
//
// Two timers, not one. The debounce timer restarts on every change, which is what
// collapses a save-format-save burst into a single update. The ceiling timer does
// not restart, which is what stops a project somebody is typing in continuously
// from deferring its update forever.
func (w *worker) run() {
	defer close(w.finished)

	// Persist before waiting for anything. The journal opened with a gap covering
	// the period nobody was watching, and that is a fact about the project whether
	// or not a change ever follows it. Without this, a quiet project has no
	// document on disk and a status command reports "never observed" about a
	// project that is being observed right now.
	w.settle()

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()

	ceiling := time.NewTimer(time.Hour)
	ceiling.Stop()
	defer ceiling.Stop()
	ceilingArmed := false

	arm := func() {
		debounce.Reset(w.watch.Debounce())
		if !ceilingArmed {
			ceiling.Reset(w.watch.SettleCeiling())
			ceilingArmed = true
		}
	}

	for {
		select {
		case <-w.done:
			// The worker is going away, so the journal is written but no index
			// update is started: a drain would outlive the thing that owns it.
			w.persist()
			return

		case event, ok := <-w.watcher.Events():
			if !ok {
				w.persist()
				return
			}
			w.journal.Record(event)
			w.touch(event.At)
			arm()

		case gap, ok := <-w.watcher.Gaps():
			if !ok {
				w.persist()
				return
			}
			w.journal.MarkGap(gap.Reason, gap.Detail, gap.At)
			w.logger.Warn("change record is incomplete",
				slog.String("reason", gap.Reason),
				slog.String("detail", gap.Detail))
			// A gap is settled on the same timer as a change rather than at once.
			// It usually arrives in the middle of the burst that caused it, and
			// persisting once at the end of the burst is both cheaper and just as
			// timely for a consumer that only acts when the batch settles.
			arm()

		case <-debounce.C:
			ceiling.Stop()
			ceilingArmed = false
			w.settle()

		case <-ceiling.C:
			debounce.Stop()
			ceilingArmed = false
			w.settle()
		}
	}
}

// settle persists the journal and starts bringing the index up to date.
func (w *worker) settle() {
	w.persist()
	w.drainAsync()
}

// persist writes the journal and records when the batch became quiet.
func (w *worker) persist() {
	if err := w.journal.Save(); err != nil {
		w.logger.Warn("change journal could not be written", slog.String("error", err.Error()))
		return
	}
	w.mu.Lock()
	w.settledAt = w.now()
	w.mu.Unlock()
}

// drainAsync brings the index up to date without holding up the event loop.
//
// A rebuild can take minutes, and the goroutine reading native events must not be
// one of the things waiting for it: a blocked reader is how the kernel buffer
// overflows. If a drain is already running, this one is dropped rather than
// queued — the running drain will pick up whatever arrived, and if it does not,
// the next settle will.
func (w *worker) drainAsync() {
	if w.stopped.Load() {
		return
	}
	go func() {
		if !w.drainMu.TryLock() {
			return
		}
		defer w.drainMu.Unlock()
		if w.stopped.Load() {
			return
		}
		w.drain()
	}()
}

// flush brings the index up to date and waits, bounded by the caller's context.
//
// It is what makes a search see an edit that has not waited out its debounce
// window. The caller's deadline bounds the wait, not the work: giving up on
// waiting must not cancel a rebuild halfway, or a caller with a short deadline
// could stop the index from ever advancing.
func (w *worker) flush(ctx context.Context) {
	if w.stopped.Load() {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.drainMu.Lock()
		defer w.drainMu.Unlock()
		if w.stopped.Load() {
			return
		}
		w.drain()
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// drain applies what has been observed to the project index.
//
// The choice between applying and reconciling is the journal's to make, not this
// function's: an incomplete record cannot be applied, because the pending list is
// then a lower bound and applying it would leave the index wrong while the
// checkpoint claimed otherwise.
func (w *worker) drain() {
	snapshot := w.journal.Snapshot()
	if snapshot.Gap == nil && len(snapshot.Pending) == 0 {
		return
	}
	// The mark comes from the snapshot being acted on. Asking the journal
	// separately would let a change observed in between be committed without ever
	// being applied.
	mark := snapshot.Mark()

	w.draining.Store(true)
	defer w.draining.Store(false)

	// A drain may escalate to a repository-wide semantic rebuild whose honest
	// duration depends on the project and hardware. It therefore has no aggregate
	// host deadline: canceling it discards the semantic store's transactional work
	// and makes every retry restart from zero. Individual remote operations retain
	// their own fail-fast timeouts, while a stopped service terminates the request.
	ctx := context.Background()

	var (
		result codeintel.IndexResult
		err    error
	)
	// Read the target once. A retarget mid-drain leaves this attempt talking to
	// the address it started with, which fails and is retried against the new one
	// rather than switching services halfway through a batch.
	index := w.target()
	if snapshot.Gap != nil {
		result, err = index.Reconcile(ctx, false)
	} else {
		result, err = index.Apply(ctx, indexChanges(snapshot.Pending))
	}
	if err != nil {
		// The checkpoint is not advanced, so nothing is lost: the same batch is
		// tried again at the next settle or flush.
		w.mu.Lock()
		w.lastError = err.Error()
		w.mu.Unlock()
		w.logger.Warn("index could not be brought up to date", slog.String("error", err.Error()))
		return
	}

	if snapshot.Gap != nil {
		w.journal.Reconciled(mark, result.Generation)
	} else {
		w.journal.Commit(mark.Sequence, result.Generation)
	}

	w.mu.Lock()
	w.appliedAt = w.now()
	w.lastError = ""
	w.mu.Unlock()

	w.logger.Info("index brought up to date",
		slog.Uint64("generation", result.Generation),
		slog.Int("applied", result.Applied),
		// Reported even when zero: a batch of saves that changed nothing is the
		// interesting case, and it is invisible otherwise.
		slog.Int("unchanged", result.Unchanged),
		slog.Int("files", result.FileCount),
		slog.Bool("full_build", result.FullBuild),
		slog.Bool("reconciled", snapshot.Gap != nil))
	w.persist()
}

// indexChanges translates observations into index operations.
func indexChanges(entries []changejournal.Entry) []codeintel.Change {
	changes := make([]codeintel.Change, 0, len(entries))
	for _, entry := range entries {
		switch {
		case entry.Kind == fswatch.Removed:
			// A removed directory takes its files with it, and only the service
			// can name them: by now the directory is gone.
			changes = append(changes, codeintel.Change{
				Path: entry.Path, Deleted: true, Subtree: entry.Directory,
			})
		case entry.Directory:
			// A created directory has no content of its own. Whatever is inside it
			// arrives as its own entry, including the files the watcher found
			// already there when it adopted the directory.
		default:
			changes = append(changes, codeintel.Change{Path: entry.Path})
		}
	}
	return changes
}

// touch records recent activity, which is what keeps a busy project's watcher
// from being reaped as idle.
func (w *worker) touch(at time.Time) {
	if at.IsZero() {
		at = w.now()
	}
	w.mu.Lock()
	if at.After(w.lastUse) {
		w.lastUse = at
	}
	w.mu.Unlock()
}

func (w *worker) idleSince(now time.Time) bool {
	w.mu.Lock()
	last := w.lastUse
	w.mu.Unlock()
	return now.Sub(last) > w.watch.IdleStop()
}

// stop releases the watcher and records that observation has ended.
//
// The gap is written after the watcher is closed, so the record says the project
// stopped being observed from that moment rather than from some point before it.
func (w *worker) stop(reason, detail string) {
	w.stopOnce.Do(func() {
		close(w.done)
		<-w.finished

		// Releasing a watcher has to mean the work has actually stopped. An index
		// update runs on its own goroutine, so without this a drain would still be
		// writing the journal and committing checkpoints for a worker that no
		// longer exists. The flag is set first so nothing new starts, then the lock
		// waits out whatever is already running.
		w.stopped.Store(true)
		w.drainMu.Lock()
		w.drainMu.Unlock() //nolint:staticcheck // waiting out an in-flight drain, not guarding a section

		_ = w.watcher.Close()

		w.journal.MarkGap(reason, detail, w.now())
		if err := w.journal.Save(); err != nil {
			w.logger.Warn("change journal could not be written on shutdown",
				slog.String("error", err.Error()))
		}
		w.logger.Info("released project watcher", slog.String("detail", detail))
	})
}

func (w *worker) view() View {
	snapshot := w.journal.Snapshot()

	w.mu.Lock()
	settledAt, appliedAt, lastError := w.settledAt, w.appliedAt, w.lastError
	w.mu.Unlock()

	return View{
		ProjectID:       w.projectID,
		Watching:        true,
		Directories:     w.watcher.Watched(),
		Pending:         len(snapshot.Pending),
		Indexing:        w.draining.Load(),
		Sequence:        snapshot.Sequence,
		Checkpoint:      snapshot.Checkpoint,
		Generation:      snapshot.Generation,
		AppliedAt:       appliedAt,
		LastError:       lastError,
		Gap:             snapshot.Gap,
		LastEventAt:     snapshot.LastEventAt,
		SettledAt:       settledAt,
		DebounceSeconds: w.watch.Debounce().Seconds(),
	}
}
