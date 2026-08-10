package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
)

// findSymbolInput is the merged definition/reference lookup request.
type findSymbolInput struct {
	Name       string `json:"name"`
	Include    string `json:"include,omitempty"`
	MaxFiles   int    `json:"max_files,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

// Bounds for find_symbol. Files bound the parse work, results bound the answer.
const (
	defaultSymbolResults = 50
	maxSymbolResults     = 500
)

// registerSymbolTool adds find_symbol, the one name-lookup tool.
//
// It replaces find_definition, find_references, and callers_find: in every
// measured trace the split surface taxed the model with a routing decision
// (which of three name tools?) without once changing what came back. Matching
// stays syntax-verified name matching — occurrences inside comments and
// strings are never reported, which is what this answers over a text search.
func (g *Gateway) registerSymbolTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "find_symbol",
		Description: "Find where an identifier is declared and used — parser-verified, so comments " +
			"and strings never match (unlike grep). Returns definitions first, then references, " +
			"as path:line: lines. Matching is by name across files; nothing is type-resolved.",
		InputSchema: schemaObject([]string{"name"}, map[string]*jsonschema.Schema{
			"name":        schemaString("The identifier, exactly as written in code."),
			"include":     schemaString("all (default), definitions, or references."),
			"max_files":   schemaInt("Maximum files to parse. Defaults to 200, which is also the ceiling."),
			"max_results": schemaInt(fmt.Sprintf("Maximum locations to return. Defaults to %d, maximum %d.", defaultSymbolResults, maxSymbolResults)),
		}),
	}, func(ctx context.Context, input findSymbolInput, timer *toolTimer) (string, error) {
		if resolved.status.ServiceAddress == "" {
			return "", errSearchUnsupported()
		}
		declarationsOnly := false
		switch input.Include {
		case "", "all", "references":
		case "definitions":
			declarationsOnly = true
		default:
			return "", &searchToolError{
				code:    codeintel.CodeInvalidPattern,
				message: "include must be all, definitions, or references.",
				action:  "Retry with one of those values.",
			}
		}

		// The index chooses which files are parsed, so the same flush a search
		// performs applies here: a file the index has not caught up with is a
		// file this lookup never opens.
		g.timedFlush(ctx, resolved, timer)

		located, err := backendTimed(g, timer, func() (codeintel.Located, error) {
			return g.searchClient(resolved).Locate(ctx, input.Name, declarationsOnly, input.MaxFiles)
		})
		if err != nil {
			return "", asSearchToolError(err)
		}

		if input.Include == "references" {
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

		maxResults := input.MaxResults
		if maxResults <= 0 {
			maxResults = defaultSymbolResults
		}
		if maxResults > maxSymbolResults {
			maxResults = maxSymbolResults
		}
		body, omitted := codetext.SymbolSections(located, maxResults)
		if body == "" {
			body = "no occurrences of " + input.Name
		}

		var footers []string
		if located.Declarations > 1 {
			footers = append(footers, fmt.Sprintf("[%d declarations share this name — matching is by name, verify which one a use refers to]", located.Declarations))
		}
		if omitted > 0 {
			footers = append(footers, fmt.Sprintf("[%d more locations omitted — raise max_results]", omitted))
		}
		if located.FilesTruncated {
			footers = append(footers, fmt.Sprintf("[only %d files examined — absence of a reference proves nothing; raise max_files]", located.FilesConsidered))
		}
		if located.SkippedUnsupported > 0 || located.SkippedUnreadable > 0 {
			footers = append(footers, fmt.Sprintf("[skipped: %d unsupported-language, %d unreadable files]", located.SkippedUnsupported, located.SkippedUnreadable))
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
