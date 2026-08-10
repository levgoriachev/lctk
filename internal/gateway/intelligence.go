package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
)

type repositoryMapInput struct {
	MaxChars int `json:"max_chars,omitempty"`
}

type graphNameInput struct {
	Name   string `json:"name"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type dependencyInput struct {
	From     string `json:"from"`
	To       string `json:"to"`
	MaxDepth int    `json:"max_depth,omitempty"`
}

type impactInput struct {
	Target string `json:"target"`
	Limit  int    `json:"limit,omitempty"`
}

type memoryGetInput struct {
	Key string `json:"key"`
}
type memorySearchInput struct {
	Query string `json:"query,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type memoryPutInput struct {
	Key              string   `json:"key"`
	Kind             string   `json:"kind"`
	Content          string   `json:"content"`
	Confidence       float64  `json:"confidence"`
	Provenance       []string `json:"provenance,omitempty"`
	SourceCommit     string   `json:"source_commit,omitempty"`
	ExpectedRevision *int     `json:"expected_revision,omitempty"`
	Reviewed         bool     `json:"reviewed,omitempty"`
}
type memoryDeleteInput struct {
	Key              string `json:"key"`
	ExpectedRevision int    `json:"expected_revision"`
}

// registerIntelligenceTools adds the default-surface intelligence tools: the
// budgeted repository map and explicit project memory.
func (g *Gateway) registerIntelligenceTools(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "repository_map",
		Description: "An importance-ranked map of the project's files and declarations inside an " +
			"exact character budget — the fastest way to orient in an unfamiliar codebase.",
		InputSchema: schemaObject(nil, map[string]*jsonschema.Schema{
			"max_chars": schemaInt("Character budget. Defaults to 12000, range 256 to 100000."),
		}),
	}, func(ctx context.Context, input repositoryMapInput, timer *toolTimer) (string, error) {
		if resolved.status.ServiceAddress == "" {
			return "", errSearchUnsupported()
		}
		g.timedFlush(ctx, resolved, timer)
		result, err := backendTimed(g, timer, func() (codeintel.MapResponse, error) {
			return g.searchClient(resolved).RepositoryMap(ctx, codeintel.MapRequest{MaxChars: input.MaxChars})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		text := strings.TrimSuffix(result.Map, "\n")
		if result.Truncated {
			text += fmt.Sprintf("\n[truncated at %d characters — raise max_chars for more]", result.MaxChars)
		}
		if note := g.watcherNote(resolved); note != "" {
			text += "\n" + note
		}
		return text, nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "memory_get",
		Description: "Read one explicit project-memory record by key. Memory holds only what was " +
			"deliberately written; nothing is recorded automatically.",
		InputSchema: schemaObject([]string{"key"}, map[string]*jsonschema.Schema{
			"key": schemaString("Stable lowercase memory key."),
		}),
	}, func(ctx context.Context, input memoryGetInput, timer *toolTimer) (string, error) {
		record, err := backendTimed(g, timer, func() (codeintel.MemoryRecord, error) {
			return g.searchClient(resolved).MemoryGet(ctx, codeintel.MemoryGetRequest{Key: input.Key})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		text := codetext.MemoryRecordText(record)
		if note := g.memoryStalenessNote(ctx, resolved, record); note != "" {
			text += "\n" + note
		}
		return text, nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "memory_search",
		Description: "Search explicit project memory; an empty query lists every record. " +
			"Records carry kind, confidence, and revision labels.",
		InputSchema: schemaObject(nil, map[string]*jsonschema.Schema{
			"query": schemaString("Concept or text to find. Empty lists records."),
			"kind":  schemaString("Optional decision, convention, fact, or note filter."),
			"limit": schemaInt("Maximum records. Defaults to 50, maximum 200."),
		}),
	}, func(ctx context.Context, input memorySearchInput, timer *toolTimer) (string, error) {
		result, err := backendTimed(g, timer, func() (codeintel.MemorySearchResponse, error) {
			return g.searchClient(resolved).MemorySearch(ctx, codeintel.MemorySearchRequest{Query: input.Query, Kind: input.Kind, Limit: input.Limit})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		if len(result.Matches) == 0 {
			return "no memory records", nil
		}
		text := codetext.MemoryList(result.Matches)
		current := g.currentCommit(ctx, resolved)
		var stale []string
		for _, match := range result.Matches {
			if current != "" && match.Record.SourceCommit != "" && match.Record.SourceCommit != current {
				stale = append(stale, match.Record.Key)
			}
		}
		if len(stale) > 0 {
			sort.Strings(stale)
			text += "\n[written against an older commit: " + strings.Join(stale, ", ") + "]"
		}
		if result.Truncated {
			text += fmt.Sprintf("\n[truncated: %d records total]", result.Total)
		}
		return text, nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "memory_put",
		Description: "Create or update one explicit project-memory record. Updating an existing key " +
			"requires the revision memory_get returned, so concurrent edits cannot silently collide.",
		InputSchema: schemaObject([]string{"key", "kind", "content", "confidence"}, map[string]*jsonschema.Schema{
			"key":               schemaString("Stable lowercase memory key."),
			"kind":              schemaString("decision, convention, fact, or note."),
			"content":           schemaString("The project knowledge to record."),
			"confidence":        {Type: "number", Description: "Confidence from 0 through 1."},
			"provenance":        schemaStrings("Project-relative evidence paths."),
			"source_commit":     schemaString("Source commit; defaults to the current project commit."),
			"expected_revision": schemaInt("Required to update an existing key; omit to create."),
			"reviewed":          schemaBool("Mark the written revision reviewed now."),
		}),
	}, func(ctx context.Context, input memoryPutInput, timer *toolTimer) (string, error) {
		commit := input.SourceCommit
		if commit == "" {
			commit = g.currentCommit(ctx, resolved)
		}
		record, err := backendTimed(g, timer, func() (codeintel.MemoryRecord, error) {
			return g.searchClient(resolved).MemoryPut(ctx, codeintel.MemoryPutRequest{
				Key: input.Key, Kind: input.Kind, Content: input.Content, Confidence: input.Confidence,
				Provenance: input.Provenance, SourceCommit: commit,
				ExpectedRevision: input.ExpectedRevision, Reviewed: input.Reviewed,
			})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		return fmt.Sprintf("stored %s at revision %d", record.Key, record.Revision), nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "memory_delete",
		Description: "Delete one project-memory key at the revision the caller last read. " +
			"A concurrent change is refused rather than overwritten.",
		InputSchema: schemaObject([]string{"key", "expected_revision"}, map[string]*jsonschema.Schema{
			"key":               schemaString("Existing memory key."),
			"expected_revision": schemaInt("Revision returned by memory_get."),
		}),
	}, func(ctx context.Context, input memoryDeleteInput, timer *toolTimer) (string, error) {
		if _, err := backendTimed(g, timer, func() (struct{}, error) {
			return struct{}{}, g.searchClient(resolved).MemoryDelete(ctx, codeintel.MemoryDeleteRequest{Key: input.Key, ExpectedRevision: input.ExpectedRevision})
		}); err != nil {
			return "", asSearchToolError(err)
		}
		return "deleted " + input.Key, nil
	})
}

// registerGraphTools adds the extended graph-traversal tools. They stay off the
// default surface: in every measured trace their questions were answered by
// find_symbol or exact_search at a fraction of the cost, and a tool that is
// rarely the right choice taxes every call's tool-selection (ADR-0031).
func (g *Gateway) registerGraphTools(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "callers_find",
		Description: "Call sites whose callee matches a name, from the saved code graph. " +
			"Name-matched, not type-resolved.",
		InputSchema: schemaObject([]string{"name"}, map[string]*jsonschema.Schema{
			"name":   schemaString("Exact callee name."),
			"limit":  schemaInt("Maximum results per page. Defaults to 50, maximum 200."),
			"cursor": schemaString("Continue a previous page."),
		}),
	}, func(ctx context.Context, input graphNameInput, timer *toolTimer) (string, error) {
		g.timedFlush(ctx, resolved, timer)
		result, err := backendTimed(g, timer, func() (codeintel.GraphMatches, error) {
			return g.searchClient(resolved).Callers(ctx, codeintel.GraphRequest{Name: input.Name, Limit: input.Limit, Cursor: input.Cursor})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		return g.graphMatchesText(resolved, result), nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "callees_find",
		Description: "Calls made inside declarations with an exact name, from the saved code graph. " +
			"Name-matched, not type-resolved.",
		InputSchema: schemaObject([]string{"name"}, map[string]*jsonschema.Schema{
			"name":   schemaString("Exact declaration name."),
			"limit":  schemaInt("Maximum results per page. Defaults to 50, maximum 200."),
			"cursor": schemaString("Continue a previous page."),
		}),
	}, func(ctx context.Context, input graphNameInput, timer *toolTimer) (string, error) {
		g.timedFlush(ctx, resolved, timer)
		result, err := backendTimed(g, timer, func() (codeintel.GraphMatches, error) {
			return g.searchClient(resolved).Callees(ctx, codeintel.GraphRequest{Name: input.Name, Limit: input.Limit, Cursor: input.Cursor})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		return g.graphMatchesText(resolved, result), nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "dependency_path",
		Description: "Shortest import path between two project files through saved import syntax. " +
			"No package-manager resolution is implied.",
		InputSchema: schemaObject([]string{"from", "to"}, map[string]*jsonschema.Schema{
			"from":      schemaString("Project-relative source file."),
			"to":        schemaString("Project-relative destination file."),
			"max_depth": schemaInt("Maximum import edges to traverse. Defaults to 32, the ceiling."),
		}),
	}, func(ctx context.Context, input dependencyInput, timer *toolTimer) (string, error) {
		g.timedFlush(ctx, resolved, timer)
		result, err := backendTimed(g, timer, func() (codeintel.DependencyResponse, error) {
			return g.searchClient(resolved).DependencyPath(ctx, codeintel.DependencyRequest{From: input.From, To: input.To, MaxDepth: input.MaxDepth})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		if !result.Found {
			return fmt.Sprintf("no import path from %s to %s within %d edges", result.From, result.To, result.MaxDepth), nil
		}
		text := strings.Join(result.Path, "\n -> ")
		if note := g.watcherNote(resolved); note != "" {
			text += "\n" + note
		}
		return text, nil
	})

	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "impact_analyze",
		Description: "Direct reverse-import and name-matched caller evidence for a file or " +
			"declaration — who would notice if this changed. Not transitive, not type-resolved.",
		InputSchema: schemaObject([]string{"target"}, map[string]*jsonschema.Schema{
			"target": schemaString("Project-relative file or exact declaration name."),
			"limit":  schemaInt("Maximum combined evidence. Defaults to 50, maximum 200."),
		}),
	}, func(ctx context.Context, input impactInput, timer *toolTimer) (string, error) {
		g.timedFlush(ctx, resolved, timer)
		result, err := backendTimed(g, timer, func() (codeintel.ImpactResponse, error) {
			return g.searchClient(resolved).Impact(ctx, codeintel.ImpactRequest{Target: input.Target, Limit: input.Limit})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		var sections []string
		if len(result.Files) > 0 {
			sections = append(sections, fmt.Sprintf("importers (%d):\n%s", len(result.Files), strings.Join(result.Files, "\n")))
		}
		if len(result.Calls) > 0 {
			sections = append(sections, fmt.Sprintf("callers (%d):\n%s", len(result.Calls),
				codetext.CallSites(codeintel.GraphMatches{Direction: "callers", Matches: result.Calls})))
		}
		if len(sections) == 0 {
			return "no direct importers or callers found for " + result.Target, nil
		}
		text := strings.Join(sections, "\n\n")
		if result.Truncated {
			text += fmt.Sprintf("\n[truncated: %d total]", result.Total)
		}
		if result.Ambiguous {
			text += "\n[the name matches more than one declaration — evidence is name-matched]"
		}
		if note := g.watcherNote(resolved); note != "" {
			text += "\n" + note
		}
		return text, nil
	})
}

// graphMatchesText renders caller/callee evidence with its bounds and caveats.
func (g *Gateway) graphMatchesText(resolved serveContext, result codeintel.GraphMatches) string {
	if len(result.Matches) == 0 {
		return "no " + result.Direction + " found for " + result.Name
	}
	text := codetext.CallSites(result)
	if result.Truncated {
		text += fmt.Sprintf("\n[truncated: %d total; continue with cursor %q]", result.Total, result.NextCursor)
	}
	if result.Ambiguous {
		text += fmt.Sprintf("\n[%d declarations share this name — evidence is name-matched]", result.Declarations)
	}
	if note := g.watcherNote(resolved); note != "" {
		text += "\n" + note
	}
	return text
}

// watcherNote is the per-call staleness framing derived from the host watcher.
func (g *Gateway) watcherNote(resolved serveContext) string {
	if g.options.Changes == nil {
		return stalenessNote(nil, freshnessUnknown)
	}
	state, watching := g.options.Changes(resolved.project.ID)
	changes, freshness := describeChanges(state, watching, false)
	return stalenessNote(changes, freshness)
}

func (g *Gateway) currentCommit(ctx context.Context, resolved serveContext) string {
	source := g.sourceOf(ctx, resolved)
	if source == nil {
		return ""
	}
	return source.Commit
}

// memoryStalenessNote says when a record was written against another commit.
func (g *Gateway) memoryStalenessNote(ctx context.Context, resolved serveContext, record codeintel.MemoryRecord) string {
	current := g.currentCommit(ctx, resolved)
	if current != "" && record.SourceCommit != "" && record.SourceCommit != current {
		return "[written against an older commit — verify before relying on it]"
	}
	return ""
}
