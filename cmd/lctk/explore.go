package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
	"github.com/lev-goryachev/lctk/internal/projectregistry"
)

// exploreTimeout bounds one read-only exploration answer. These commands exist
// for shell-native agents, and an agent's turn is worth seconds, not minutes.
const exploreTimeout = 60 * time.Second

// Bounds mirroring the MCP surface (ADR-0031), so the CLI and the gateway give
// the same answer to the same question.
const (
	exploreDefaultSpan  = 200
	exploreMaxSpan      = 2000
	exploreMaxReadBytes = 4 << 20
	exploreMaxContext   = 10
)

// runSearch serves `lctk search`: the CLI mirror of exact_search.
//
// Codex-class clients load every MCP schema into every request and reach for
// the shell first; a CLI answer costs them nothing per turn (ADR-0031). One
// AGENTS.md line — "search with lctk search PROJECT PATTERN" — is the whole
// integration.
func runSearch(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	regex := flags.Bool("regex", false, "treat the pattern as a regular expression")
	caseSensitive := flags.Bool("case", false, "match case exactly")
	globs := flags.String("glob", "", "comma-separated project-relative globs such as **/*.go")
	languages := flags.String("lang", "", "comma-separated languages such as go,python")
	limit := flags.Int("limit", 0, "maximum matches per page (default 50, maximum 500)")
	cursor := flags.String("cursor", "", "continue from a previous answer's cursor")
	filesOnly := flags.Bool("files", false, "print matched file paths only")
	countOnly := flags.Bool("count", false, "print per-file match counts only")
	contextLines := flags.Int("context", 0, "lines of context around each match, up to 10")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: lctk search [--regex] [--case] [--glob G] [--lang L] [--limit N] [--cursor C] [--files|--count] [--context N] PROJECT PATTERN")
	}

	project, client, ctx, cancel, err := exploreClient(flags.Arg(0))
	if err != nil {
		return err
	}
	defer cancel()

	mode := ""
	if *regex {
		mode = "regex"
	}
	response, err := client.Search(ctx, codeintel.Request{
		Pattern:       flags.Arg(1),
		Mode:          mode,
		CaseSensitive: *caseSensitive,
		PathGlobs:     splitList(*globs),
		Languages:     splitList(*languages),
		Limit:         *limit,
		Cursor:        *cursor,
	})
	if err != nil {
		return err
	}

	switch {
	case *filesOnly:
		fmt.Fprintln(stdout, codetext.FilesWithMatches(response.Matches))
	case *countOnly:
		fmt.Fprintln(stdout, codetext.MatchCounts(response.Matches))
	default:
		if len(response.Matches) == 0 {
			fmt.Fprintln(stdout, "no matches")
			break
		}
		bounded := *contextLines
		if bounded > exploreMaxContext {
			bounded = exploreMaxContext
		}
		blocks := contextBlocks(project.Path, response.Matches, bounded)
		fmt.Fprintln(stdout, codetext.Matches(blocks))
	}
	if response.Truncated {
		footer := fmt.Sprintf("[truncated: %d matches total", response.Total)
		if response.NextCursor != "" {
			footer += fmt.Sprintf("; continue with --cursor %q", response.NextCursor)
		}
		fmt.Fprintln(stdout, footer+"]")
	}
	return nil
}

// runRead serves `lctk read`: numbered lines straight from the working tree.
func runRead(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("read", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from := flags.Int("from", 1, "first line, 1-based")
	to := flags.Int("to", 0, "last line, inclusive (default: first 200 lines)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: lctk read [--from N] [--to N] PROJECT PATH")
	}

	registry, err := projectregistry.Load()
	if err != nil {
		return err
	}
	project, err := registry.Resolve(flags.Arg(0))
	if err != nil {
		return err
	}

	lines, err := readConfinedLines(project.Path, flags.Arg(1))
	if err != nil {
		return err
	}
	start := *from
	if start <= 0 {
		start = 1
	}
	end := *to
	if end <= 0 {
		end = start + exploreDefaultSpan - 1
	}
	if end < start {
		return fmt.Errorf("--to %d is before --from %d", end, start)
	}
	clamped := false
	if end-start+1 > exploreMaxSpan {
		end = start + exploreMaxSpan - 1
		clamped = true
	}
	if start > len(lines) {
		fmt.Fprintf(stdout, "no lines: %s has %d lines, requested start %d\n", flags.Arg(1), len(lines), start)
		return nil
	}
	if end > len(lines) {
		end = len(lines)
	}
	numbered := make([]codetext.Line, 0, end-start+1)
	for number := start; number <= end; number++ {
		numbered = append(numbered, codetext.Line{Number: number, Text: lines[number-1]})
	}
	fmt.Fprintln(stdout, codetext.Span(numbered))
	if clamped {
		fmt.Fprintf(stdout, "[span clamped to %d lines — continue from line %d]\n", exploreMaxSpan, end+1)
	}
	return nil
}

// runOutline serves `lctk outline`: the CLI mirror of file_outline.
func runOutline(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("outline", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	depth := flags.Int("depth", 0, "nesting depth to show (default 2)")
	symbols := flags.Int("symbols", 0, "maximum declarations to show (default 200)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: lctk outline [--depth N] [--symbols N] PROJECT PATH")
	}

	_, client, ctx, cancel, err := exploreClient(flags.Arg(0))
	if err != nil {
		return err
	}
	defer cancel()

	outline, err := client.Outline(ctx, flags.Arg(1))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s — %s, %d lines\n", outline.Path, outline.Language, outline.Lines)
	if outline.Syntax.Reported && !outline.Syntax.Valid {
		fmt.Fprintf(stdout, "warning: the file does not parse cleanly (%d broken regions)\n", outline.Syntax.Errors)
	}
	body, omitted := codetext.Outline(outline, codetext.OutlineOptions{MaxDepth: *depth, MaxSymbols: *symbols})
	if body == "" {
		fmt.Fprintln(stdout, "no declarations")
	} else {
		fmt.Fprintln(stdout, body)
	}
	if omitted > 0 {
		fmt.Fprintf(stdout, "[%d more declarations omitted — raise --depth or --symbols]\n", omitted)
	}
	return nil
}

// runMap serves `lctk map`: the CLI mirror of repository_map.
func runMap(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("map", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	chars := flags.Int("chars", 0, "character budget (default 12000, range 256 to 100000)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: lctk map [--chars N] PROJECT")
	}

	_, client, ctx, cancel, err := exploreClient(flags.Arg(0))
	if err != nil {
		return err
	}
	defer cancel()

	result, err := client.RepositoryMap(ctx, codeintel.MapRequest{MaxChars: *chars})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, strings.TrimSuffix(result.Map, "\n"))
	if result.Truncated {
		fmt.Fprintf(stdout, "[truncated at %d characters — raise --chars for more]\n", result.MaxChars)
	}
	return nil
}

// runSymbol serves `lctk symbol`: the CLI mirror of find_symbol.
func runSymbol(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("symbol", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	include := flags.String("include", "all", "all, definitions, or references")
	maxFiles := flags.Int("max-files", 0, "maximum files to parse (default 200)")
	maxResults := flags.Int("max-results", 0, "maximum locations to print (default 50)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: lctk symbol [--include all|definitions|references] [--max-files N] [--max-results N] PROJECT NAME")
	}

	_, client, ctx, cancel, err := exploreClient(flags.Arg(0))
	if err != nil {
		return err
	}
	defer cancel()

	located, err := client.Locate(ctx, flags.Arg(1), *include == "definitions", *maxFiles)
	if err != nil {
		return err
	}
	if *include == "references" {
		for index := range located.Files {
			var uses []codeintel.Occurrence
			for _, occurrence := range located.Files[index].Occurrences {
				if !occurrence.Declaration {
					uses = append(uses, occurrence)
				}
			}
			located.Files[index].Occurrences = uses
		}
	}
	body, omitted := codetext.SymbolSections(located, *maxResults)
	if body == "" {
		fmt.Fprintf(stdout, "no occurrences of %s\n", flags.Arg(1))
		return nil
	}
	fmt.Fprintln(stdout, body)
	if located.Declarations > 1 {
		fmt.Fprintf(stdout, "[%d declarations share this name — matching is by name]\n", located.Declarations)
	}
	if omitted > 0 {
		fmt.Fprintf(stdout, "[%d more locations omitted — raise --max-results]\n", omitted)
	}
	if located.FilesTruncated {
		fmt.Fprintf(stdout, "[only %d files examined — raise --max-files]\n", located.FilesConsidered)
	}
	return nil
}

// exploreClient resolves a project and connects to its running service.
func exploreClient(reference string) (projectregistry.Project, *codeintel.Client, context.Context, context.CancelFunc, error) {
	registry, err := projectregistry.Load()
	if err != nil {
		return projectregistry.Project{}, nil, nil, nil, err
	}
	project, err := registry.Resolve(reference)
	if err != nil {
		return projectregistry.Project{}, nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), exploreTimeout)
	status, err := newStackManager().Status(ctx, project)
	if err != nil {
		cancel()
		return projectregistry.Project{}, nil, nil, nil, err
	}
	if status.ServiceAddress == "" {
		cancel()
		return projectregistry.Project{}, nil, nil, nil, fmt.Errorf(
			"project %s is not running a code-intelligence service; start it with lctk project start", project.ID)
	}
	return project, newSearchClient(status.ServiceAddress), ctx, cancel, nil
}

// newSearchClient is a seam for tests, mirroring newStackManager.
var newSearchClient = codeintel.New

// contextBlocks reads surrounding lines from the working tree, best effort: a
// file that cannot be read still yields its match line.
func contextBlocks(root string, matches []codeintel.Match, contextLines int) []codetext.MatchBlock {
	blocks := make([]codetext.MatchBlock, 0, len(matches))
	cache := map[string][]string{}
	for _, match := range matches {
		block := codetext.MatchBlock{
			Path:  match.Path,
			Match: codetext.Line{Number: match.Line, Text: strings.TrimRight(match.Preview, "\r\n")},
		}
		if contextLines > 0 {
			lines, ok := cache[match.Path]
			if !ok {
				if read, err := readConfinedLines(root, match.Path); err == nil {
					lines = read
				}
				cache[match.Path] = lines
			}
			if lines != nil && match.Line >= 1 && match.Line <= len(lines) {
				for number := maxInt(1, match.Line-contextLines); number < match.Line; number++ {
					block.Before = append(block.Before, codetext.Line{Number: number, Text: lines[number-1]})
				}
				for number := match.Line + 1; number <= minInt(len(lines), match.Line+contextLines); number++ {
					block.After = append(block.After, codetext.Line{Number: number, Text: lines[number-1]})
				}
			}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// readConfinedLines reads one project file as lines, confined to the root so a
// crafted path cannot read through the project boundary.
func readConfinedLines(root, relative string) ([]string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relative)))
	if cleaned == "" || cleaned == "." {
		return nil, errors.New("a file path is required")
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
		return nil, fmt.Errorf("the path escapes the project root: %s", relative)
	}
	if len(cleaned) > 1 && cleaned[1] == ':' {
		return nil, fmt.Errorf("the path escapes the project root: %s", relative)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("the project root could not be resolved: %w", err)
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(cleaned)))
	if err != nil {
		return nil, err
	}
	if full != resolvedRoot && !strings.HasPrefix(full, resolvedRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("the path resolves outside the project root: %s", relative)
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("the path is not a readable file: %s", cleaned)
	}
	if info.Size() > exploreMaxReadBytes {
		return nil, fmt.Errorf("%s is %d bytes; the read ceiling is %d", cleaned, info.Size(), exploreMaxReadBytes)
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, fmt.Errorf("%s is a binary file", cleaned)
	}
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	return strings.Split(text, "\n"), nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
