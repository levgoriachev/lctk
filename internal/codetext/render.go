// Package codetext renders code-intelligence answers as compact plain text.
//
// The formats exist for agents: one grep-shaped line per fact, no envelope, and
// framing only where it changes what a reader does next (ADR-0031). The MCP
// gateway and the lctk CLI both render through this package so a tool answer
// and a terminal answer are the same answer.
package codetext

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lev-goryachev/lctk/internal/codeintel"
)

// Line is one numbered source line.
type Line struct {
	Number int
	Text   string
}

// MatchBlock is one search hit with optional surrounding context.
type MatchBlock struct {
	Path   string
	Match  Line
	Before []Line
	After  []Line
}

// Matches renders search hits in grep form: "path:line: text" with optional
// "path-line- text" context lines and "--" separators between blocks.
func Matches(blocks []MatchBlock) string {
	var builder strings.Builder
	withContext := false
	for _, block := range blocks {
		if len(block.Before) > 0 || len(block.After) > 0 {
			withContext = true
			break
		}
	}
	for index, block := range blocks {
		if withContext && index > 0 {
			builder.WriteString("--\n")
		}
		for _, line := range block.Before {
			fmt.Fprintf(&builder, "%s-%d- %s\n", block.Path, line.Number, line.Text)
		}
		fmt.Fprintf(&builder, "%s:%d: %s\n", block.Path, block.Match.Number, block.Match.Text)
		for _, line := range block.After {
			fmt.Fprintf(&builder, "%s-%d- %s\n", block.Path, line.Number, line.Text)
		}
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// FilesWithMatches renders the unique matched paths, one per line, in order of
// first appearance.
func FilesWithMatches(matches []codeintel.Match) string {
	seen := map[string]bool{}
	var paths []string
	for _, match := range matches {
		if !seen[match.Path] {
			seen[match.Path] = true
			paths = append(paths, match.Path)
		}
	}
	return strings.Join(paths, "\n")
}

// MatchCounts renders per-file match counts and a total.
func MatchCounts(matches []codeintel.Match) string {
	counts := map[string]int{}
	var order []string
	for _, match := range matches {
		if counts[match.Path] == 0 {
			order = append(order, match.Path)
		}
		counts[match.Path]++
	}
	var builder strings.Builder
	for _, path := range order {
		fmt.Fprintf(&builder, "%s: %d\n", path, counts[path])
	}
	fmt.Fprintf(&builder, "total: %d", len(matches))
	return builder.String()
}

// Span renders numbered file lines in cat -n form.
func Span(lines []Line) string {
	var builder strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&builder, "%6d\t%s\n", line.Number, line.Text)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// OutlineOptions bound an outline rendering.
type OutlineOptions struct {
	// MaxDepth keeps declarations whose nesting depth is below it. Zero means
	// the default of 2 (top-level declarations and their direct members).
	MaxDepth int
	// MaxSymbols bounds the rendered declarations. Zero means the default of 200.
	MaxSymbols int
}

// DefaultOutlineDepth and DefaultOutlineSymbols are the ADR-0031 outline bounds.
const (
	DefaultOutlineDepth   = 2
	DefaultOutlineSymbols = 200
)

// Outline renders a file outline as an indented declaration tree and reports
// how many declarations the bounds omitted.
func Outline(outline codeintel.Outline, options OutlineOptions) (string, int) {
	maxDepth := options.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultOutlineDepth
	}
	maxSymbols := options.MaxSymbols
	if maxSymbols <= 0 {
		maxSymbols = DefaultOutlineSymbols
	}

	var builder strings.Builder
	rendered := 0
	omitted := 0
	for _, symbol := range outline.Symbols {
		if symbol.Depth >= maxDepth {
			omitted++
			continue
		}
		if rendered >= maxSymbols {
			omitted++
			continue
		}
		rendered++
		indent := strings.Repeat("  ", symbol.Depth)
		line := fmt.Sprintf("%s%s %s  %d-%d", indent, symbol.Kind, symbol.Name, symbol.StartLine, symbol.EndLine)
		if signature := compactSignature(symbol.Signature, symbol.Name); signature != "" {
			line += "  " + signature
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return strings.TrimSuffix(builder.String(), "\n"), omitted
}

// compactSignature keeps a declaration's first line when it says more than the
// name alone, bounded so one declaration cannot dominate the answer.
func compactSignature(signature, name string) string {
	signature = strings.TrimSpace(signature)
	if signature == "" || signature == name {
		return ""
	}
	if utf8.RuneCountInString(signature) > 120 {
		runes := []rune(signature)
		signature = string(runes[:119]) + "…"
	}
	return signature
}

// SymbolSections renders a find_symbol answer: declarations first, then
// references, each in grep form, bounded by maxResults across both sections.
// It reports how many locations the bound omitted.
func SymbolSections(located codeintel.Located, maxResults int) (string, int) {
	if maxResults <= 0 {
		maxResults = 50
	}

	type row struct {
		path       string
		occurrence codeintel.Occurrence
	}
	var declarations, references []row
	for _, file := range located.Files {
		for _, occurrence := range file.Occurrences {
			entry := row{path: file.Path, occurrence: occurrence}
			if occurrence.Declaration {
				declarations = append(declarations, entry)
			} else {
				references = append(references, entry)
			}
		}
	}

	var builder strings.Builder
	remaining := maxResults
	omitted := 0

	writeRows := func(header string, rows []row) {
		if len(rows) == 0 {
			return
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		fmt.Fprintf(&builder, "%s (%d):\n", header, len(rows))
		for _, entry := range rows {
			if remaining <= 0 {
				omitted++
				continue
			}
			remaining--
			text := strings.TrimSpace(entry.occurrence.Preview)
			if text == "" {
				text = located.Name
			}
			line := fmt.Sprintf("%s:%d: %s", entry.path, entry.occurrence.Line, text)
			if entry.occurrence.Declaration && entry.occurrence.Kind != "" {
				line += "  [" + entry.occurrence.Kind + "]"
			}
			builder.WriteString(line)
			builder.WriteString("\n")
		}
	}

	writeRows("definitions", declarations)
	writeRows("references", references)
	return strings.TrimSuffix(builder.String(), "\n"), omitted
}

// CallSites renders caller or callee evidence in grep form.
func CallSites(matches codeintel.GraphMatches) string {
	var builder strings.Builder
	for _, evidence := range matches.Matches {
		context := evidence.Callee
		if matches.Direction == "callers" && evidence.Caller != "" {
			context = evidence.Caller
		}
		fmt.Fprintf(&builder, "%s:%d: %s\n", evidence.Path, evidence.Line, context)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// SemanticMatches renders conceptual search hits as ranged grep lines.
func SemanticMatches(matches []codeintel.SemanticMatch, previewRunes int) string {
	if previewRunes <= 0 {
		previewRunes = 160
	}
	var builder strings.Builder
	for _, match := range matches {
		preview := firstLine(match.Preview)
		if utf8.RuneCountInString(preview) > previewRunes {
			runes := []rune(preview)
			preview = string(runes[:previewRunes-1]) + "…"
		}
		fmt.Fprintf(&builder, "%s:%d-%d: %s\n", match.Path, match.StartLine, match.EndLine, preview)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// MemoryRecordText renders one memory record with its labels.
func MemoryRecordText(record codeintel.MemoryRecord) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "key: %s (%s, revision %d, confidence %.2f)\n", record.Key, record.Kind, record.Revision, record.Confidence)
	if record.SourceCommit != "" {
		fmt.Fprintf(&builder, "source commit: %s\n", record.SourceCommit)
	}
	if len(record.Provenance) > 0 {
		fmt.Fprintf(&builder, "provenance: %s\n", strings.Join(record.Provenance, ", "))
	}
	var labels []string
	if record.ReviewDue {
		labels = append(labels, "review due")
	}
	if record.LowConfidence {
		labels = append(labels, "low confidence")
	}
	if record.ReviewedAt != "" {
		labels = append(labels, "reviewed "+record.ReviewedAt)
	}
	if len(labels) > 0 {
		fmt.Fprintf(&builder, "labels: %s\n", strings.Join(labels, ", "))
	}
	builder.WriteString(record.Content)
	return builder.String()
}

// MemoryList renders memory search matches, one record per line.
func MemoryList(matches []codeintel.MemoryMatch) string {
	var builder strings.Builder
	for _, match := range matches {
		fmt.Fprintf(&builder, "%s (%s, revision %d, confidence %.2f): %s\n",
			match.Record.Key, match.Record.Kind, match.Record.Revision,
			match.Record.Confidence, firstLine(match.Record.Content))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// SortedCopy returns paths sorted without mutating the argument.
func SortedCopy(paths []string) []string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	return sorted
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}
