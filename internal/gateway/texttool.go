package gateway

import (
	"context"
	"time"

	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInstructions is the routing guidance a connected client sees once per
// session. Claude-class clients defer tool schemas and surface only names plus
// these instructions, and Codex-class clients truncate long instruction text,
// so the routing rules come first and stay short (ADR-0031).
const serverInstructions = "Indexed code intelligence for exactly this project; results are plain text " +
	"path:line lines. Route: repository_map to orient; exact_search for any string or regex " +
	"(indexed, faster than scanning); find_symbol for where a name is declared and used; " +
	"file_outline for a file's structure; read_span to read exact lines. " +
	"Paths are project-relative. The index follows saves automatically; answers say so when it is behind. " +
	"Scope is fixed by the endpoint: arguments naming another project are ignored."

// textResult wraps one plain-text answer.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// toolTimer collects the phase timings of one tool call so fixed costs are
// ranked by measurement rather than by suspicion (ADR-0031).
type toolTimer struct {
	flush   time.Duration
	backend time.Duration
}

// addTextTool registers a tool whose answer is one plain-text block: no output
// schema is published and nothing is double-encoded. The input schema is
// hand-written and deliberately tolerant: unknown arguments are ignored, not
// refused, because a model that pays for a failed call learns nothing useful.
func addTextTool[In any](g *Gateway, server *mcp.Server, resolved serveContext, tool *mcp.Tool,
	handler func(ctx context.Context, input In, timer *toolTimer) (string, error),
) {
	name := tool.Name
	mcp.AddTool(server, tool, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		timer := &toolTimer{}
		started := g.options.Now()
		text, err := handler(ctx, input, timer)
		total := g.options.Now().Sub(started)
		logger := g.options.Logger.With(
			slog.String("request_id", resolved.requestID),
			slog.String("project_id", resolved.project.ID),
			slog.String("tool", name),
			slog.Int64("ms_total", total.Milliseconds()),
			slog.Int64("ms_flush", timer.flush.Milliseconds()),
			slog.Int64("ms_backend", timer.backend.Milliseconds()),
		)
		if err != nil {
			logger.Warn("tool call refused", slog.String("error", err.Error()))
			return nil, nil, err
		}
		logger.Info("tool call served", slog.Int("chars", len(text)))
		return textResult(text), nil, nil
	})
}

// schemaObject builds a flat object schema. AdditionalProperties is left open
// on purpose: an extra argument a model copied from a native tool is dropped,
// not refused.
func schemaObject(required []string, properties map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: properties, Required: required}
}

func schemaString(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description}
}

func schemaInt(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: description}
}

func schemaBool(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: description}
}

func schemaStrings(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: description, Items: &jsonschema.Schema{Type: "string"}}
}

// timedFlush is the pre-answer index flush every index-backed tool performs,
// measured so its real cost is visible in the call log.
func (g *Gateway) timedFlush(ctx context.Context, resolved serveContext, timer *toolTimer) {
	if g.options.Flush == nil {
		return
	}
	started := g.options.Now()
	flushCtx, cancel := context.WithTimeout(ctx, searchFlushBudget)
	g.options.Flush(flushCtx, resolved.project.ID)
	cancel()
	timer.flush += g.options.Now().Sub(started)
}
