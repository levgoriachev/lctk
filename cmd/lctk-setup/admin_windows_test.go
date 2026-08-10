//go:build windows

package main

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/lev-goryachev/lctk/internal/adminclient"
)

func TestIdenticalOAuthPollLeavesRenderedListUntouched(t *testing.T) {
	items := []adminListItem{{ID: "request-a", Text: "Codex"}}
	if !equalAdminListItems(items, append([]adminListItem(nil), items...)) {
		t.Fatal("an identical poll would destructively rebuild the request list")
	}
}

func TestLogSelectionSurvivesAnAppendedRecord(t *testing.T) {
	previous := "first\r\nselected diagnostic\r\n"
	selected := "selected diagnostic"
	start := len(utf16.Encode([]rune(previous[:strings.Index(previous, selected)])))
	end := start + len(utf16.Encode([]rune(selected)))

	gotStart, gotEnd := preservedUTF16Selection(previous, previous+"new record\r\n", start, end)
	if gotStart != start || gotEnd != end {
		t.Fatalf("selection = %d:%d, want %d:%d", gotStart, gotEnd, start, end)
	}
}

func TestRepeatedLogSelectionKeepsItsOriginalOccurrence(t *testing.T) {
	previous := "same\r\nsame"
	start := len(utf16.Encode([]rune("same\r\n")))
	end := start + len(utf16.Encode([]rune("same")))
	gotStart, gotEnd := preservedUTF16Selection(previous, previous+"\r\nnext", start, end)
	if gotStart != start || gotEnd != end {
		t.Fatalf("selection = %d:%d, want %d:%d", gotStart, gotEnd, start, end)
	}
}

func TestRemovedLogSelectionCollapsesInsteadOfSelectingOtherText(t *testing.T) {
	start, end := preservedUTF16Selection("prefix selected", "prefix changed", 7, 15)
	if start != 7 || end != 7 {
		t.Fatalf("selection = %d:%d, want a collapsed caret at 7", start, end)
	}
}

func TestSemanticDiagnosticMakesProgressFailureAndStallExplicit(t *testing.T) {
	progress := semanticDiagnostic(&adminclient.SemanticIndex{Indexing: true, ChunksEmbedded: 64, ChunksReused: 32, ChunksTotal: 128})
	if !strings.Contains(progress, "96/128") {
		t.Fatalf("progress = %q, want embedded plus reused chunks", progress)
	}
	failure := semanticDiagnostic(&adminclient.SemanticIndex{LastError: "identity collision"})
	if !strings.Contains(failure, "failed") || !strings.Contains(failure, "identity collision") {
		t.Fatalf("failure = %q, want explicit error", failure)
	}
	stalled := semanticDiagnostic(&adminclient.SemanticIndex{Indexing: true, Stalled: true, StallSeconds: 181, ChunksEmbedded: 64, ChunksTotal: 128})
	if !strings.Contains(stalled, "STALLED") || !strings.Contains(stalled, "181s") {
		t.Fatalf("stalled = %q, want visible stall", stalled)
	}
}

func TestIndexProgressStatesKeepEveryLayerIndependent(t *testing.T) {
	exact, semantic, graph := indexProgressStates(&adminclient.Index{
		Ready: true, Generation: 3, FileCount: 353,
		Semantic: &adminclient.SemanticIndex{Indexing: true, ChunksEmbedded: 96, ChunksReused: 4, ChunksTotal: 1161},
		Graph:    &adminclient.GraphIndex{Reason: "The derived graph has not been built yet."},
	})
	if exact.Current != 1 || exact.Total != 1 || !strings.Contains(exact.Label, "353 files") {
		t.Fatalf("exact = %+v, want a complete exact generation", exact)
	}
	if semantic.Current != 100 || semantic.Total != 1161 || !strings.Contains(semantic.Label, "100/1161") {
		t.Fatalf("semantic = %+v, want measured chunk progress", semantic)
	}
	if !graph.Indeterminate || !strings.Contains(graph.Label, "total is not reported") {
		t.Fatalf("graph = %+v, want an honest active graph state without a fabricated percent", graph)
	}
}

func TestIndexProgressStatesExposeFailuresWithoutFakeProgress(t *testing.T) {
	_, semantic, graph := indexProgressStates(&adminclient.Index{
		Semantic: &adminclient.SemanticIndex{LastError: "identity collision"},
		Graph:    &adminclient.GraphIndex{Reason: "graph publication failed"},
	})
	if semantic.Current != 0 || semantic.Indeterminate || !strings.Contains(semantic.Label, "identity collision") {
		t.Fatalf("semantic = %+v, want a stopped explicit failure", semantic)
	}
	if graph.Current != 0 || graph.Indeterminate || !strings.Contains(graph.Label, "graph publication failed") {
		t.Fatalf("graph = %+v, want a stopped explicit failure", graph)
	}
}
