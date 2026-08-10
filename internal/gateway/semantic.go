package gateway

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lev-goryachev/lctk/internal/codeintel"
	"github.com/lev-goryachev/lctk/internal/codetext"
)

type semanticSearchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// defaultSemanticLimit is deliberately small: in every measured trace the
// twenty-match default flooded the context with wrong-module chunks that a
// two-line exact_search answered better (ADR-0031). Five ranked ranges are a
// lead, not a payload.
const defaultSemanticLimit = 5

// registerSemanticTool adds conceptual search. It is part of the extended
// surface: it returns to the default one when a retrieval-quality measurement
// earns its place there.
func (g *Gateway) registerSemanticTool(server *mcp.Server, resolved serveContext) {
	addTextTool(g, server, resolved, &mcp.Tool{
		Name: "code_search_semantic",
		Description: "Find code by concept when the identifier is unknown — describe the behavior " +
			"in words. For any known string or name, exact_search and find_symbol are faster " +
			"and more precise.",
		InputSchema: schemaObject([]string{"query"}, map[string]*jsonschema.Schema{
			"query": schemaString("The concept or behavior to find, in words."),
			"limit": schemaInt(fmt.Sprintf("Maximum ranked ranges. Defaults to %d, maximum 100.", defaultSemanticLimit)),
		}),
	}, func(ctx context.Context, input semanticSearchInput, timer *toolTimer) (string, error) {
		if resolved.status.ServiceAddress == "" {
			return "", errSearchUnsupported()
		}
		g.timedFlush(ctx, resolved, timer)
		limit := input.Limit
		if limit == 0 {
			limit = defaultSemanticLimit
		}
		response, err := backendTimed(g, timer, func() (codeintel.SemanticResponse, error) {
			return g.searchClient(resolved).SemanticSearch(ctx, codeintel.SemanticRequest{Query: input.Query, Limit: limit})
		})
		if err != nil {
			return "", asSearchToolError(err)
		}
		if len(response.Matches) == 0 {
			return "no conceptual matches — try exact_search with a distinctive string", nil
		}
		text := codetext.SemanticMatches(response.Matches, 0)
		text += "\n[ranked by meaning, not proof — read the ranges with read_span before relying on them]"
		if note := g.watcherNote(resolved); note != "" {
			text += "\n" + note
		}
		return text, nil
	})
}
