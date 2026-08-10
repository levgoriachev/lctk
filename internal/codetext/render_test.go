package codetext

import (
	"strings"
	"testing"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

func TestMatchesRendersGrepLinesWithContextBlocks(t *testing.T) {
	plain := Matches([]MatchBlock{
		{Path: "a/b.go", Match: Line{Number: 10, Text: "func Alpha() {"}},
		{Path: "a/c.go", Match: Line{Number: 3, Text: "Alpha()"}},
	})
	if plain != "a/b.go:10: func Alpha() {\na/c.go:3: Alpha()" {
		t.Fatalf("plain matches = %q", plain)
	}

	withContext := Matches([]MatchBlock{
		{
			Path:   "a/b.go",
			Before: []Line{{Number: 9, Text: "// doc"}},
			Match:  Line{Number: 10, Text: "func Alpha() {"},
			After:  []Line{{Number: 11, Text: "\treturn"}},
		},
		{Path: "a/c.go", Match: Line{Number: 3, Text: "Alpha()"}},
	})
	expected := strings.Join([]string{
		"a/b.go-9- // doc",
		"a/b.go:10: func Alpha() {",
		"a/b.go-11- \treturn",
		"--",
		"a/c.go:3: Alpha()",
	}, "\n")
	if withContext != expected {
		t.Fatalf("context matches = %q", withContext)
	}
}

func TestFilesAndCountsDeduplicateInFirstAppearanceOrder(t *testing.T) {
	matches := []codeintel.Match{
		{Path: "z.go", Line: 1}, {Path: "a.go", Line: 2}, {Path: "z.go", Line: 9},
	}
	if files := FilesWithMatches(matches); files != "z.go\na.go" {
		t.Fatalf("files = %q", files)
	}
	if counts := MatchCounts(matches); counts != "z.go: 2\na.go: 1\ntotal: 3" {
		t.Fatalf("counts = %q", counts)
	}
}

func TestSpanNumbersLinesLikeCatN(t *testing.T) {
	span := Span([]Line{{Number: 41, Text: "alpha"}, {Number: 42, Text: ""}})
	if span != "    41\talpha\n    42\t" {
		t.Fatalf("span = %q", span)
	}
}

func TestOutlineBoundsDepthAndCount(t *testing.T) {
	outline := codeintel.Outline{Symbols: []codeintel.Symbol{
		{Name: "HTML", Kind: "class", StartLine: 342, EndLine: 456, Depth: 0, Signature: "class HTML(core.BaseReader):"},
		{Name: "write", Kind: "method", StartLine: 360, EndLine: 456, Depth: 1},
		{Name: "inner", Kind: "function", StartLine: 361, EndLine: 380, Depth: 2},
	}}

	text, omitted := Outline(outline, OutlineOptions{})
	if omitted != 1 || strings.Contains(text, "inner") {
		t.Fatalf("default depth kept nested declaration: omitted=%d text=%q", omitted, text)
	}
	if !strings.Contains(text, "class HTML  342-456  class HTML(core.BaseReader):") ||
		!strings.Contains(text, "  method write  360-456") {
		t.Fatalf("outline text = %q", text)
	}

	text, omitted = Outline(outline, OutlineOptions{MaxDepth: 3, MaxSymbols: 1})
	if omitted != 2 || strings.Contains(text, "write") {
		t.Fatalf("symbol bound not applied: omitted=%d text=%q", omitted, text)
	}
}

func TestSymbolSectionsSplitDeclarationsFromReferences(t *testing.T) {
	located := codeintel.Located{
		Name: "merge_attrs",
		Files: []codeintel.FileOccurrences{
			{Path: "core/merge.py", Occurrences: []codeintel.Occurrence{
				{Line: 495, Declaration: true, Kind: "function", Preview: "def merge_attrs(variable_attrs, combine_attrs):"},
				{Line: 640, Preview: "attrs = merge_attrs(...)"},
			}},
			{Path: "core/concat.py", Occurrences: []codeintel.Occurrence{
				{Line: 12, Preview: "merge_attrs(a, b)"},
			}},
		},
	}
	text, omitted := SymbolSections(located, 50)
	if omitted != 0 {
		t.Fatalf("omitted = %d", omitted)
	}
	expected := strings.Join([]string{
		"definitions (1):",
		"core/merge.py:495: def merge_attrs(variable_attrs, combine_attrs):  [function]",
		"",
		"references (2):",
		"core/merge.py:640: attrs = merge_attrs(...)",
		"core/concat.py:12: merge_attrs(a, b)",
	}, "\n")
	if text != expected {
		t.Fatalf("sections = %q", text)
	}

	_, omitted = SymbolSections(located, 2)
	if omitted != 1 {
		t.Fatalf("bounded omitted = %d", omitted)
	}
}

func TestSemanticMatchesKeepOneBoundedLinePerHit(t *testing.T) {
	text := SemanticMatches([]codeintel.SemanticMatch{
		{Path: "io/ascii/html.py", StartLine: 342, EndLine: 456, Preview: "def write(self, table):\n    lots more"},
	}, 10)
	if text != "io/ascii/html.py:342-456: def write…" {
		t.Fatalf("semantic = %q", text)
	}
}

func TestMemoryRendering(t *testing.T) {
	record := codeintel.MemoryRecord{
		Key: "release-gate", Kind: "decision", Content: "Ship only after parity.\nSecond line.",
		Confidence: 0.9, Revision: 3, ReviewDue: true,
	}
	text := MemoryRecordText(record)
	if !strings.HasPrefix(text, "key: release-gate (decision, revision 3, confidence 0.90)") ||
		!strings.Contains(text, "labels: review due") ||
		!strings.HasSuffix(text, "Second line.") {
		t.Fatalf("record = %q", text)
	}
	list := MemoryList([]codeintel.MemoryMatch{{Record: record}})
	if list != "release-gate (decision, revision 3, confidence 0.90): Ship only after parity." {
		t.Fatalf("list = %q", list)
	}
}
