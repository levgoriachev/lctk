package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/buildinfo"
	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
)

// schemaCache keeps resolved input schemas across the per-request servers the
// stateless transport builds.
var schemaCache = mcp.NewSchemaCache()

// newProjectServer builds an MCP server bound to one resolved project. The
// closure captures the project, so no tool can reach another one.
func (g *Gateway) newProjectServer(resolved serveContext) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "lctk-project-" + resolved.project.ID,
		Version: buildinfo.Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		SchemaCache:  schemaCache,
	})

	g.registerProjectInfoTool(server, resolved)
	g.registerExactSearchTool(server, resolved)
	g.registerReadSpanTool(server, resolved)
	g.registerOutlineTool(server, resolved)
	g.registerSymbolTool(server, resolved)
	g.registerIntelligenceTools(server, resolved)
	if g.options.ExtendedTools {
		g.registerSemanticTool(server, resolved)
		g.registerGraphTools(server, resolved)
	}
	g.registerGitTools(server, resolved)
	g.registerRunTool(server, resolved)
	return server
}

type projectInfoInput struct{}

// registerProjectInfoTool serves the one-time orientation call. It is the only
// answer that names the host root and the full index state; per-call answers
// stay quiet unless something needs the caller's attention (ADR-0031).
func (g *Gateway) registerProjectInfoTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "project_info",
		Description: "Orient in this project: root path, tool capabilities, index freshness, " +
			"approved commands. Call once per session before exploring.",
		InputSchema: schemaObject(nil, map[string]*jsonschema.Schema{}),
	}, func(ctx context.Context, _ projectInfoInput, timer *toolTimer) (string, error) {
		var lines []string
		lines = append(lines, fmt.Sprintf("project: %s (%s)", resolved.project.Name, resolved.project.ID))
		lines = append(lines, "root: "+resolved.project.Path+" (all tool paths are relative to it)")
		state := string(resolved.status.State)
		if resolved.status.Health != "" {
			state += " (" + resolved.status.Health + ")"
		}
		lines = append(lines, "state: "+state)

		capabilities := []string{"project_info", "exact_search", "read_span"}
		var status codeintel.Status
		haveStatus := false
		if resolved.status.ServiceAddress != "" {
			// Best effort: a project whose service is still starting should still
			// answer project_info.
			if probed, err := backendTimed(g, timer, func() (codeintel.Status, error) {
				return g.searchClient(resolved).Status(ctx)
			}); err == nil {
				status = probed
				haveStatus = true
			}
		}
		if haveStatus && len(status.OutlineLanguages) > 0 {
			capabilities = append(capabilities, "file_outline", "find_symbol")
		}
		if haveStatus && status.Graph != nil {
			capabilities = append(capabilities, "repository_map", "memory")
		}
		if g.options.ExtendedTools {
			if haveStatus && status.Semantic != nil {
				capabilities = append(capabilities, "code_search_semantic")
			}
			if haveStatus && status.Graph != nil {
				capabilities = append(capabilities, "callers_find", "callees_find", "dependency_path", "impact_analyze")
			}
		}
		source := g.sourceOf(ctx, resolved)
		if source != nil {
			capabilities = append(capabilities, "git_status", "git_diff")
		}
		commands := g.runnableNames(resolved)
		if len(commands) > 0 {
			capabilities = append(capabilities, "run_command")
		}
		lines = append(lines, "capabilities: "+strings.Join(capabilities, ", "))

		var changeState ChangeState
		watching := false
		if g.options.Changes != nil {
			changeState, watching = g.options.Changes(resolved.project.ID)
		}
		if haveStatus {
			changes, freshness := describeChanges(changeState, watching, status.Indexing)
			index := fmt.Sprintf("index: generation %d, %d files", status.Generation, status.FileCount)
			if status.IndexedAt != "" {
				index += ", indexed " + status.IndexedAt
			}
			index += " — " + freshness
			if !status.Ready && status.Reason != "" {
				index += " (" + status.Reason + ")"
			}
			lines = append(lines, index)
			if note := stalenessNote(changes, freshness); note != "" {
				lines = append(lines, note)
			}
			if len(status.OutlineLanguages) > 0 {
				lines = append(lines, "outline languages: "+strings.Join(status.OutlineLanguages, ", "))
			}
		} else {
			_, freshness := describeChanges(changeState, watching, false)
			lines = append(lines, "index: unavailable — "+freshness)
		}

		if source != nil {
			git := "source: "
			switch {
			case source.Unborn:
				git += "repository without a commit yet"
			case source.Detached:
				git += "detached at " + source.ShortCommit
			default:
				git += "branch " + source.Branch + " @ " + source.ShortCommit
			}
			if source.Dirty {
				git += fmt.Sprintf(", %d changed files", source.ChangedFiles)
			} else {
				git += ", clean"
			}
			lines = append(lines, git)
		}
		if len(commands) > 0 {
			lines = append(lines, "commands: "+strings.Join(commands, ", "))
		}
		lines = append(lines, "server: lctk "+buildinfo.Version+" at "+g.options.Now().UTC().Format("2006-01-02T15:04:05Z"))
		return strings.Join(lines, "\n"), nil
	})
}

type exactSearchInput struct {
	Pattern       string   `json:"pattern"`
	Mode          string   `json:"mode,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	PathGlobs     []string `json:"path_globs,omitempty"`
	Languages     []string `json:"languages,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Cursor        string   `json:"cursor,omitempty"`
	OutputMode    string   `json:"output_mode,omitempty"`
	ContextLines  int      `json:"context_lines,omitempty"`
}

// maxContextLines bounds per-match context so one broad search cannot flood a
// context window.
const maxContextLines = 10

func (g *Gateway) registerExactSearchTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "exact_search",
		Description: "Indexed search over the saved working tree — use for any literal string or regex; " +
			"faster than scanning files. Returns path:line: text matches. Files saved a moment " +
			"ago are included.",
		InputSchema: schemaObject([]string{"pattern"}, map[string]*jsonschema.Schema{
			"pattern":        schemaString("Text or regular expression to find."),
			"mode":           schemaString("literal (default) or regex."),
			"case_sensitive": schemaBool("Match case exactly. Defaults to false."),
			"path_globs":     schemaStrings("Project-relative globs such as **/*.go."),
			"languages":      schemaStrings("Restrict to languages such as Go or TypeScript."),
			"limit":          schemaInt("Maximum matches per page. Defaults to 50, maximum 500."),
			"cursor":         schemaString("Continue from a previous answer's cursor."),
			"output_mode":    schemaString("content (default), files_with_matches, or count."),
			"context_lines":  schemaInt(fmt.Sprintf("Lines of context around each match, up to %d. Defaults to 0.", maxContextLines)),
		}),
	}, func(ctx context.Context, input exactSearchInput, timer *toolTimer) (string, error) {
		if resolved.status.ServiceAddress == "" {
			return "", errSearchUnsupported()
		}
		g.timedFlush(ctx, resolved, timer)

		response, err := backendTimed(g, timer, func() (codeintel.Response, error) {
			return g.searchClient(resolved).Search(ctx, codeintel.Request{
				Pattern:       input.Pattern,
				Mode:          input.Mode,
				CaseSensitive: input.CaseSensitive,
				PathGlobs:     input.PathGlobs,
				Languages:     input.Languages,
				Limit:         input.Limit,
				Cursor:        input.Cursor,
			})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}

		var body string
		switch input.OutputMode {
		case "", "content":
			contextLines := input.ContextLines
			if contextLines > maxContextLines {
				contextLines = maxContextLines
			}
			body = codetext.Matches(g.contextLinesFor(resolved, response.Matches, contextLines))
		case "files_with_matches":
			body = codetext.FilesWithMatches(response.Matches)
		case "count":
			body = codetext.MatchCounts(response.Matches)
		default:
			return "", &searchToolError{
				code:    codeintel.CodeInvalidPattern,
				message: "output_mode must be content, files_with_matches, or count.",
				action:  "Retry with one of those values.",
			}
		}

		var footers []string
		if len(response.Matches) == 0 {
			body = "no matches"
			if (input.Mode == "" || input.Mode == "literal") && looksLikeRegex(input.Pattern) {
				body += ` (pattern contains regex syntax but mode is literal — retry with mode:"regex"?)`
			}
		}
		if response.Truncated {
			footer := fmt.Sprintf("[truncated: %d matches total", response.Total)
			if response.NextCursor != "" {
				footer += `; continue with cursor "` + response.NextCursor + `"`
			}
			footers = append(footers, footer+"]")
		}
		if g.options.Changes != nil {
			state, watching := g.options.Changes(resolved.project.ID)
			changes, freshness := describeChanges(state, watching, false)
			if note := stalenessNote(changes, freshness); note != "" {
				footers = append(footers, note)
			}
		}
		if len(footers) > 0 {
			body += "\n" + strings.Join(footers, "\n")
		}
		return body, nil
	})
}

type fileOutlineInput struct {
	Path       string `json:"path"`
	MaxDepth   int    `json:"max_depth,omitempty"`
	MaxSymbols int    `json:"max_symbols,omitempty"`
}

// registerOutlineTool adds the bounded file outline.
func (g *Gateway) registerOutlineTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "file_outline",
		Description: "List a file's declarations with their line ranges — the fast way to find " +
			"where to read in a big file, and to check that an edited file still parses. " +
			"Bounded by default; raise max_depth or max_symbols to widen.",
		InputSchema: schemaObject([]string{"path"}, map[string]*jsonschema.Schema{
			"path":        schemaString("Project-relative file path."),
			"max_depth":   schemaInt(fmt.Sprintf("Nesting depth to show. Defaults to %d.", codetext.DefaultOutlineDepth)),
			"max_symbols": schemaInt(fmt.Sprintf("Maximum declarations to show. Defaults to %d.", codetext.DefaultOutlineSymbols)),
		}),
	}, func(ctx context.Context, input fileOutlineInput, timer *toolTimer) (string, error) {
		if resolved.status.ServiceAddress == "" {
			return "", errSearchUnsupported()
		}
		outline, err := backendTimed(g, timer, func() (codeintel.Outline, error) {
			return g.searchClient(resolved).Outline(ctx, input.Path)
		})
		if err != nil {
			return "", asSearchToolError(err)
		}

		header := fmt.Sprintf("%s — %s, %d lines", outline.Path, outline.Language, outline.Lines)
		body, omitted := codetext.Outline(outline, codetext.OutlineOptions{
			MaxDepth: input.MaxDepth, MaxSymbols: input.MaxSymbols,
		})
		text := header
		if outline.Syntax.Reported && !outline.Syntax.Valid {
			text += fmt.Sprintf("\nwarning: the file does not parse cleanly (%d broken regions) — declarations may be incomplete", outline.Syntax.Errors)
		}
		if body != "" {
			text += "\n" + body
		} else {
			text += "\nno declarations"
		}
		if omitted > 0 {
			text += fmt.Sprintf("\n[%d more declarations omitted — raise max_depth or max_symbols]", omitted)
		}
		return text, nil
	})
}

// stalenessNote is the only freshness framing a per-call answer carries, and
// only when the answer may actually be behind the disk (ADR-0031).
func stalenessNote(changes *changeInfo, freshness string) string {
	switch freshness {
	case freshnessFresh:
		return ""
	case freshnessUpdating:
		return "[index updating — retry shortly for the newest edits]"
	case freshnessStale:
		if changes != nil && changes.Pending > 0 {
			return fmt.Sprintf("[index behind: %d changed paths not yet indexed — recent edits may be missing]", changes.Pending)
		}
		return "[index behind the working tree — recent edits may be missing]"
	default:
		return "[index freshness unknown — nothing is watching this project's changes]"
	}
}

// looksLikeRegex reports whether a literal pattern was probably meant as a
// regular expression, so a zero-hit answer can say the useful thing.
func looksLikeRegex(pattern string) bool {
	return strings.ContainsAny(pattern, `\[](){}|^$*+?`)
}

// errSearchUnsupported is the shared refusal for a project without a running
// code-intelligence service.
func errSearchUnsupported() error {
	return &searchToolError{
		code:    codeintel.CodeSearchUnsupported,
		message: "This project does not expose a code-intelligence service.",
		action:  "Restart it with lctk project restart so it is recreated with the current stack definition.",
	}
}

// backendTimed measures one backend call into the tool timer.
func backendTimed[T any](g *Gateway, timer *toolTimer, call func() (T, error)) (T, error) {
	started := g.options.Now()
	value, err := call()
	timer.backend += g.options.Now().Sub(started)
	return value, err
}
