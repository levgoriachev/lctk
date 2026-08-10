package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
)

// Bounds for host-side reads. The span bound exists so one call cannot flood a
// context window; the byte bound matches the service's own outline ceiling.
const (
	defaultSpanLines = 200
	maxSpanLines     = 2000
	maxReadBytes     = 4 << 20
)

type readSpanInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// registerReadSpanTool completes the find-then-show loop inside the surface:
// search names a line, read_span shows it, and neither call leaves the project.
// It reads the saved working tree on the host, so it needs no index and no
// running service, and a file saved a moment ago is read as saved.
func (g *Gateway) registerReadSpanTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "read_span",
		Description: "Read exact lines of one project file. Use after exact_search or find_symbol " +
			"to see the code around a hit. Returns numbered lines from the saved working tree.",
		InputSchema: schemaObject([]string{"path"}, map[string]*jsonschema.Schema{
			"path":       schemaString("Project-relative file path."),
			"start_line": schemaInt("First line, 1-based. Defaults to 1."),
			"end_line":   schemaInt(fmt.Sprintf("Last line, inclusive. Defaults to start_line+%d-1; at most %d lines per call.", defaultSpanLines, maxSpanLines)),
		}),
	}, func(_ context.Context, input readSpanInput, _ *toolTimer) (string, error) {
		lines, total, err := g.readProjectLines(resolved, input.Path)
		if err != nil {
			return "", err
		}

		start := input.StartLine
		if start <= 0 {
			start = 1
		}
		end := input.EndLine
		if end <= 0 {
			end = start + defaultSpanLines - 1
		}
		if end < start {
			return "", &searchToolError{
				code:    codeintel.CodeInvalidPath,
				message: fmt.Sprintf("end_line %d is before start_line %d.", end, start),
				action:  "Ask for a forward range.",
			}
		}
		clamped := false
		if end-start+1 > maxSpanLines {
			end = start + maxSpanLines - 1
			clamped = true
		}
		if start > total {
			return fmt.Sprintf("no lines: %s has %d lines, requested start %d", input.Path, total, start), nil
		}
		if end > total {
			end = total
		}

		numbered := make([]codetext.Line, 0, end-start+1)
		for number := start; number <= end; number++ {
			numbered = append(numbered, codetext.Line{Number: number, Text: lines[number-1]})
		}
		text := codetext.Span(numbered)
		if clamped {
			text += fmt.Sprintf("\n[span clamped to %d lines — continue from line %d]", maxSpanLines, end+1)
		} else if end < total && input.EndLine > 0 && input.EndLine > total {
			text += fmt.Sprintf("\n[file ends at line %d]", total)
		}
		return text, nil
	})
}

// readProjectLines reads one file from the project root with the span bounds
// applied, returning its lines and their count.
func (g *Gateway) readProjectLines(resolved serveContext, relative string) ([]string, int, error) {
	content, err := g.readProjectFile(resolved, relative)
	if err != nil {
		return nil, 0, err
	}
	lines := splitLines(content)
	return lines, len(lines), nil
}

// readProjectFile reads one project file from the host working tree, confined
// to the registered root. The gateway may do this because the route's scope IS
// this project; what it must never do is read through the root's boundary.
func (g *Gateway) readProjectFile(resolved serveContext, relative string) ([]byte, error) {
	invalid := func(message string) error {
		return &searchToolError{code: codeintel.CodeInvalidPath, message: message,
			action: "Use a project-relative path exactly as search results report it."}
	}
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(relative)))
	if cleaned == "" || cleaned == "." {
		return nil, invalid("A file path is required.")
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "../") {
		return nil, invalid("The path escapes the project root: " + relative)
	}
	if len(cleaned) > 1 && cleaned[1] == ':' {
		return nil, invalid("The path escapes the project root: " + relative)
	}

	root := resolved.project.Path
	full := filepath.Join(root, filepath.FromSlash(cleaned))

	// The registered root and the target are both resolved before comparison,
	// so a symlink inside the tree cannot carry the read outside it.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, &searchToolError{code: codeintel.CodeServiceOffline,
			message: "The project root could not be resolved on disk.",
			action:  "Check the registered path with lctk project status."}
	}
	resolvedFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &searchToolError{code: codeintel.CodeFileNotFound,
				message: "No file exists at " + cleaned + ".",
				action:  "Check the path with exact_search or file_outline."}
		}
		return nil, invalid("The path could not be resolved: " + relative)
	}
	if resolvedFull != resolvedRoot && !strings.HasPrefix(resolvedFull, resolvedRoot+string(filepath.Separator)) {
		return nil, invalid("The path resolves outside the project root: " + relative)
	}

	info, err := os.Stat(resolvedFull)
	if err != nil || info.IsDir() {
		return nil, invalid("The path is not a readable file: " + cleaned)
	}
	if info.Size() > maxReadBytes {
		return nil, &searchToolError{code: codeintel.CodeFileTooLarge,
			message: fmt.Sprintf("%s is %d bytes; the read ceiling is %d.", cleaned, info.Size(), maxReadBytes),
			action:  "Read a bounded span of a smaller file, or use file_outline."}
	}

	content, err := os.ReadFile(resolvedFull)
	if err != nil {
		return nil, invalid("The file could not be read: " + cleaned)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, &searchToolError{code: codeintel.CodeInvalidPath,
			message: cleaned + " is a binary file.",
			action:  "Only text files can be read as lines."}
	}
	return content, nil
}

// contextLinesFor reads the surrounding lines of search matches from the host
// working tree. The flush before the search already brought the index up to
// date, and the disk itself is the freshest witness there is.
func (g *Gateway) contextLinesFor(resolved serveContext, matches []codeintel.Match, contextLines int) []codetext.MatchBlock {
	blocks := make([]codetext.MatchBlock, 0, len(matches))
	fileLines := map[string][]string{}
	for _, match := range matches {
		block := codetext.MatchBlock{
			Path:  match.Path,
			Match: codetext.Line{Number: match.Line, Text: strings.TrimRight(match.Preview, "\r\n")},
		}
		if contextLines > 0 {
			lines, ok := fileLines[match.Path]
			if !ok {
				content, err := g.readProjectFile(resolved, match.Path)
				if err == nil {
					lines = splitLines(content)
				}
				fileLines[match.Path] = lines
			}
			if lines != nil && match.Line >= 1 && match.Line <= len(lines) {
				for number := max(1, match.Line-contextLines); number < match.Line; number++ {
					block.Before = append(block.Before, codetext.Line{Number: number, Text: lines[number-1]})
				}
				for number := match.Line + 1; number <= min(len(lines), match.Line+contextLines); number++ {
					block.After = append(block.After, codetext.Line{Number: number, Text: lines[number-1]})
				}
			}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// splitLines splits file content into lines without treating a trailing
// newline as an extra empty line.
func splitLines(content []byte) []string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{}
	}
	return strings.Split(text, "\n")
}
