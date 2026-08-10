package gateway

import (
	"strings"
	"testing"
	"time"
)

// Freshness framing under ADR-0031: a per-call answer carries a staleness note
// only when the answer may actually be behind the disk, and a fresh answer
// carries nothing. These tests pin the note for each watcher state.

// A project the host is not watching must not be reported as fresh. "Nobody
// looked" is not evidence that nothing changed, and an agent told "fresh" will
// not check again.
func TestAnUnwatchedProjectReportsUnknownFreshnessRatherThanFresh(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text := callProjectInfo(t, session, nil)

	if !strings.Contains(text, "index: generation 4, 42 files, indexed 2026-08-01T11:00:00Z — unknown") {
		t.Fatalf("the index state does not carry the unknown verdict:\n%s", text)
	}
	if !strings.Contains(text, "[index freshness unknown — nothing is watching this project's changes]") {
		t.Fatalf("nothing warned that nobody is watching:\n%s", text)
	}

	// The same honesty travels with every index-backed answer.
	searchText, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}
	if !strings.Contains(searchText, "[index freshness unknown — nothing is watching this project's changes]") {
		t.Fatalf("an unwatched search answer carries no warning:\n%s", searchText)
	}
}

func TestAWatchedAndQuietProjectReportsFreshWithoutANote(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true, DebounceSeconds: 3}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text := callProjectInfo(t, session, nil)

	if !strings.Contains(text, "— fresh") {
		t.Fatalf("a watched, quiet project is not reported fresh:\n%s", text)
	}
	if strings.Contains(text, "[index") {
		t.Fatalf("a fresh answer carries a staleness note:\n%s", text)
	}

	// A fresh search answer is the bare result: status that changes nothing is
	// not repeated per call (ADR-0031).
	searchText, _ := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if strings.Contains(searchText, "[index") {
		t.Fatalf("a fresh search answer carries a staleness note:\n%s", searchText)
	}
}

func TestPendingChangesMakeTheIndexStale(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{
		Watching:    true,
		Pending:     2,
		LastEventAt: testNow.Add(-time.Minute),
	}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text := callProjectInfo(t, session, nil)

	if !strings.Contains(text, "— stale") {
		t.Fatalf("unapplied changes did not make the verdict stale:\n%s", text)
	}
	if !strings.Contains(text, "[index behind: 2 changed paths not yet indexed — recent edits may be missing]") {
		t.Fatalf("the pending count did not reach the caller:\n%s", text)
	}
}

func TestAnIndexBeingBroughtUpToDateSaysUpdating(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true, Indexing: true}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text := callProjectInfo(t, session, nil)

	if !strings.Contains(text, "— updating") {
		t.Fatalf("an in-flight update is not reported:\n%s", text)
	}
	if !strings.Contains(text, "[index updating — retry shortly for the newest edits]") {
		t.Fatalf("the note does not say waiting helps:\n%s", text)
	}
}

// A gap means the pending count is a lower bound. The answer has to say so, and
// must not read as current.
func TestAGapIsReportedAsStale(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true, GapReason: "watcher_overflow"}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text := callProjectInfo(t, session, nil)

	if !strings.Contains(text, "— stale") {
		t.Fatalf("an incomplete record was not reported stale:\n%s", text)
	}
	if !strings.Contains(text, "[index behind the working tree — recent edits may be missing]") {
		t.Fatalf("the note does not warn about the gap:\n%s", text)
	}
}

// Using a project is what should make the host observe it. Verifying it here
// keeps the wakeup on the request path rather than on a timer.
func TestAnAcceptedRequestWakesTheHostWatcher(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])

	if len(f.woken) == 0 {
		t.Fatal("an accepted request did not wake the project's watcher")
	}
	for _, id := range f.woken {
		if id != "alpha-aaaaaaaa" {
			t.Fatalf("woke %q, which is not the routed project", id)
		}
	}
}

func TestARefusedRequestDoesNotWakeAnything(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")

	// A credential issued for another project must not even cause the routed
	// project to be observed: refusal happens before the project is in use.
	rawPost(t, f.server.URL+"/projects/alpha-aaaaaaaa/mcp", "not-a-grant")

	if len(f.woken) != 0 {
		t.Fatalf("a refused request woke %v", f.woken)
	}
}

// An agent that writes a file and immediately searches for it is the common
// case. Waiting a moment for the change to reach the index beats telling the
// agent the code it just wrote does not exist.
func TestASearchBringsTheIndexUpToDateFirst(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true, Pending: 1}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}

	if len(f.flushed) == 0 {
		t.Fatal("a search answered from the index without asking for pending changes first")
	}
	for _, id := range f.flushed {
		if id != "alpha-aaaaaaaa" {
			t.Fatalf("flushed %q, which is not the routed project", id)
		}
	}
	// The flush applied everything, so there is nothing left to warn about.
	if strings.Contains(text, "[index") {
		t.Fatalf("a caught-up answer still carries a staleness note:\n%s", text)
	}
}

// When the flush cannot catch up, the result still comes back — with the
// staleness note attached, so the caller can judge it rather than assume it.
func TestASearchThatCouldNotCatchUpSaysSo(t *testing.T) {
	f := newFixture(t, true, "alpha-aaaaaaaa")
	service := newFakeService(t)
	f.service["alpha-aaaaaaaa"] = service.address()
	f.changes["alpha-aaaaaaaa"] = ChangeState{Watching: true, GapReason: "watcher_overflow"}

	session := f.connect(t, "alpha-aaaaaaaa", f.tokens["alpha-aaaaaaaa"])
	text, result := callExactSearch(t, session, map[string]any{"pattern": "Needle"})
	if result.IsError {
		t.Fatalf("exact_search refused: %s", resultText(result))
	}

	if !strings.Contains(text, "[index behind the working tree — recent edits may be missing]") {
		t.Fatalf("a behind answer carries no warning:\n%s", text)
	}
	// A stale answer is still an answer.
	if !strings.Contains(text, "internal/a.go:7:") {
		t.Fatalf("the search returned nothing:\n%s", text)
	}
}

func TestDescribeChangesIsPessimisticWhileIndexing(t *testing.T) {
	state := ChangeState{Watching: true}
	if _, freshness := describeChanges(state, true, true); freshness != freshnessUpdating {
		t.Fatalf("freshness = %q, want %q while a build is in flight", freshness, freshnessUpdating)
	}
	if _, freshness := describeChanges(ChangeState{}, false, true); freshness != freshnessUpdating {
		t.Fatalf("freshness = %q, want %q even with no watcher", freshness, freshnessUpdating)
	}
}
