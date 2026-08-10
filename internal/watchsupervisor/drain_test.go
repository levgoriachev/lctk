package watchsupervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lev-goryachev/lctk/internal/changejournal"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/fswatch"
)

// deadlineProbeIndex records whether the watcher imposed one aggregate
// deadline on a repository operation. The production adapters keep their own
// per-request bounds; this probe protects the higher-level rebuild contract.
type deadlineProbeIndex struct {
	hadDeadline bool
}

// Apply implements indexClient for completeness. The test below starts from a
// journal gap, so reaching this method would itself expose wrong routing.
func (probe *deadlineProbeIndex) Apply(context.Context, []codeintel.Change) (codeintel.IndexResult, error) {
	return codeintel.IndexResult{}, nil
}

// Reconcile captures the context contract and returns one published generation
// so the journal can close its initial observation gap normally.
func (probe *deadlineProbeIndex) Reconcile(ctx context.Context, _ bool) (codeintel.IndexResult, error) {
	_, probe.hadDeadline = ctx.Deadline()
	return codeintel.IndexResult{Generation: 2, FileCount: 1}, nil
}

func TestRepositoryReconcileHasNoAggregateHostDeadline(t *testing.T) {
	journal, err := changejournal.Open("proj", changejournal.Options{
		Path: filepath.Join(t.TempDir(), "journal.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	probe := &deadlineProbeIndex{}
	worker := &worker{
		journal: journal,
		index:   probe,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:     time.Now,
	}

	worker.drain()

	if probe.hadDeadline {
		t.Fatal("repository reconcile received an aggregate host deadline")
	}
	snapshot := journal.Snapshot()
	if snapshot.Gap != nil || snapshot.Generation != 2 {
		t.Fatalf("journal = %+v, want reconciled generation 2", snapshot)
	}
}

// awaitApply waits for the index to receive a batch matching the predicate.
func (h *harness) awaitApply(t *testing.T, service *fakeService, what string, match func([][]codeintel.Change) bool) {
	t.Helper()
	deadline := time.Now().Add(testBudget)
	for time.Now().Before(deadline) {
		if applied := service.appliedPaths(); match(applied) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the index never received %s within %s; it received %+v",
		what, testBudget, service.appliedPaths())
}

// The record starts incomplete, because the daemon was not watching before it
// started. An incomplete record cannot be applied: the pending list is a lower
// bound, so the only correct first move is to reconcile.
func TestAnIncompleteRecordIsReconciledRatherThanApplied(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)

	h.Sweep(context.Background())

	h.awaitView(t, "the starting gap being closed", func(v View) bool {
		return v.Gap == nil && v.Generation > 0
	})
	if service.reconcileCount() == 0 {
		t.Fatal("the index was never reconciled after a period of no observation")
	}
	if applied := service.appliedPaths(); len(applied) != 0 {
		t.Fatalf("an incomplete record was applied as a batch: %+v", applied)
	}
}

func TestASavedFileReachesTheIndexWithoutBeingAskedTo(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	h.write(t, "main.go", "package main\n")

	h.awaitApply(t, service, "the saved file", func(batches [][]codeintel.Change) bool {
		for _, batch := range batches {
			for _, change := range batch {
				if change.Path == "main.go" && !change.Deleted {
					return true
				}
			}
		}
		return false
	})

	view := h.awaitView(t, "the checkpoint catching up", func(v View) bool {
		return v.Pending == 0 && v.Checkpoint == v.Sequence
	})
	if view.Generation == 0 {
		t.Fatal("the checkpoint records no index generation, so a restart could not tell what it describes")
	}
	if view.LastError != "" {
		t.Fatalf("last error = %q, want none", view.LastError)
	}
}

// A removed directory has to reach the index as a subtree deletion. Sent as an
// ordinary path it would retract one entry the index does not hold and leave
// every file that was inside it searchable.
func TestARemovedDirectoryReachesTheIndexAsASubtreeDeletion(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	h.write(t, "pkg/a.go", "package pkg\n")
	h.awaitApply(t, service, "the new file", func(batches [][]codeintel.Change) bool {
		for _, batch := range batches {
			for _, change := range batch {
				if change.Path == "pkg/a.go" {
					return true
				}
			}
		}
		return false
	})

	if err := os.RemoveAll(filepath.Join(h.root, "pkg")); err != nil {
		t.Fatal(err)
	}
	h.awaitApply(t, service, "the subtree deletion", func(batches [][]codeintel.Change) bool {
		for _, batch := range batches {
			for _, change := range batch {
				if change.Path == "pkg" && change.Deleted && change.Subtree {
					return true
				}
			}
		}
		return false
	})
}

// A created directory carries no content of its own. Sending it would make the
// index read a path that is not a file and discard it, once per directory.
func TestACreatedDirectoryIsNotSentToTheIndex(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	h.write(t, "pkg/a.go", "package pkg\n")
	h.awaitApply(t, service, "the new file", func(batches [][]codeintel.Change) bool {
		for _, batch := range batches {
			for _, change := range batch {
				if change.Path == "pkg/a.go" {
					return true
				}
			}
		}
		return false
	})

	for _, batch := range service.appliedPaths() {
		for _, change := range batch {
			if change.Path == "pkg" && !change.Deleted {
				t.Fatalf("a created directory was sent to the index: %+v", change)
			}
		}
	}
}

// A failed update must leave the work outstanding. Advancing the checkpoint over
// changes the index never received is the one way to lose them silently.
func TestAFailedUpdateDoesNotAdvanceTheCheckpoint(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	service.setFailing(true)
	h.write(t, "main.go", "package main\n")

	view := h.awaitView(t, "the failure being reported", func(v View) bool { return v.LastError != "" })
	if view.Pending == 0 {
		t.Fatal("the change was dropped when the index refused it")
	}
	if view.Checkpoint == view.Sequence {
		t.Fatal("the checkpoint advanced over a change the index never received")
	}

	// Recovery is the next settle, not an operator's intervention.
	service.setFailing(false)
	h.Flush(context.Background(), h.project.ID)

	h.awaitView(t, "the retry succeeding", func(v View) bool {
		return v.Pending == 0 && v.LastError == "" && v.Checkpoint == v.Sequence
	})
}

// Flush is what lets a search see an edit that has not waited out its debounce
// window. Without it the answer would be correct about a generation the caller
// already knows is behind.
func TestFlushAppliesWithoutWaitingOutTheDebounceWindow(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)
	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	// Recorded directly rather than through the filesystem, so the test is about
	// the flush and not about how quickly a native event arrives.
	journal := h.journalFor(t)
	journal.Record(fswatch.Event{Path: "urgent.go", Kind: fswatch.Written, At: h.clock.Now()})

	before := len(service.appliedPaths())
	h.Flush(context.Background(), h.project.ID)

	if len(service.appliedPaths()) <= before {
		t.Fatal("Flush returned without the change reaching the index")
	}
}

func TestFlushingAnUnwatchedProjectIsHarmless(t *testing.T) {
	service := newFakeService(t, []string{"."})
	h := newHarness(t, service, nil)

	h.Flush(context.Background(), h.project.ID)
	h.Flush(context.Background(), "no-such-project")
}

func TestIndexChangesTranslatesEveryObservation(t *testing.T) {
	entries := []changejournal.Entry{
		{Path: "a.go", Kind: fswatch.Written},
		{Path: "b.go", Kind: fswatch.Removed},
		{Path: "pkg", Kind: fswatch.Removed, Directory: true},
		{Path: "pkg", Kind: fswatch.Written, Directory: true},
	}
	got := indexChanges(entries)

	want := []codeintel.Change{
		{Path: "a.go"},
		{Path: "b.go", Deleted: true},
		{Path: "pkg", Deleted: true, Subtree: true},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A project restarted while the daemon is running comes back on a different
// service address. Nothing about the host's observation changed, so the index has
// to catch up on the new address rather than stay behind on the old one forever.
func TestAProjectThatMovedToANewPortIsCaughtUpRatherThanAbandoned(t *testing.T) {
	first := newFakeService(t, []string{"."})
	runtime := &control{}
	h := newHarness(t, first, runtime)

	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	// The project is torn down and its service address stops answering.
	first.server.Close()
	h.write(t, "after-restart.go", "package main\n")

	stalled := h.awaitView(t, "the failed update being reported",
		func(v View) bool { return v.LastError != "" })
	if stalled.Checkpoint >= stalled.Sequence {
		t.Fatalf("the checkpoint advanced past an update that failed: %+v", stalled)
	}

	// It comes back, and the port is the only thing that is different.
	second := newFakeService(t, []string{"."})
	runtime.moveTo(second.address())
	h.Sweep(context.Background())

	h.awaitApply(t, second, "the change observed while the old port was dead",
		func(batches [][]codeintel.Change) bool {
			for _, batch := range batches {
				for _, change := range batch {
					if change.Path == "after-restart.go" && !change.Deleted {
						return true
					}
				}
			}
			return false
		})
	h.awaitView(t, "the lag clearing", func(v View) bool {
		return v.LastError == "" && v.Checkpoint == v.Sequence
	})

	// The journal survived the move, so catching up was an ordinary batch. Had it
	// been discarded, a recoverable lag would have cost a full reconciliation.
	if second.reconcileCount() != 0 {
		t.Errorf("catching up on the new address reconciled %d times, want an applied batch",
			second.reconcileCount())
	}
}

// A client talking to the project is earlier evidence than the next sweep that it
// came back somewhere else, and an agent that just asked about a project should
// not wait a sweep interval for its index to start catching up.
func TestAClientRequestIsEnoughToNoticeTheProjectMoved(t *testing.T) {
	first := newFakeService(t, []string{"."})
	runtime := &control{}
	h := newHarness(t, first, runtime)

	h.Sweep(context.Background())
	h.awaitView(t, "the starting gap being closed", func(v View) bool { return v.Gap == nil })

	first.server.Close()
	h.write(t, "moved.go", "package main\n")
	h.awaitView(t, "the failed update being reported", func(v View) bool { return v.LastError != "" })

	second := newFakeService(t, []string{"."})
	runtime.moveTo(second.address())

	// No sweep. Only a client using the project, which is what Wake reports.
	status := h.status
	status.ServiceAddress = second.address()
	h.Wake(h.project, status)

	h.awaitApply(t, second, "the pending change after a client woke the project",
		func(batches [][]codeintel.Change) bool {
			for _, batch := range batches {
				for _, change := range batch {
					if change.Path == "moved.go" {
						return true
					}
				}
			}
			return false
		})
}
