# ADR-0031: Agent-first tool surface

- Status: accepted
- Date: 2026-08-10
- Deciders: project maintainers

## Context

The first paid SWE-Explore measurements ([ADR-0030](0030-warm-index-code-exploration-benchmark.md), [single-run evidence](../benchmarks/swe-explore-single-run.md)) showed every LCTK treatment arm losing to its native control on wall-clock time, tokens, cost, and final-answer quality. A full audit of the traces, the tool surface, and the external literature attributed the loss to four mechanisms, in descending order of measured cost:

1. **Response packaging.** Every MCP response is emitted twice (a JSON text block plus an identical `structuredContent` copy, a measured 2.15-2.3x wire inflation), wrapped in an invariant envelope (`project_id`, `scope_source`, `root`, unconditional provenance) that costs 290-713 characters even for an empty result, and published alongside inlined output schemas that are 65% of the `tools/list` payload. The match format costs ~1.8x a grep-equivalent line for the same hit.
2. **Missing primitives and a wrong path contract.** The exploration surface can find but not show: there is no span-read tool, so in one measured run an agent issued 27 MCP calls without ever reading a file body, reconstructing content through 16 consecutive `exact_search` sweeps. Every response advertises the container-internal root `/workspace`, which poisoned the clients' native `Read`/`Grep` calls in both measured Claude treatment runs.
3. **Descriptions written as epistemic contracts.** No tool description says when to use the tool or how it beats the client's built-in search; 40-50% of the description text is negation and internal vocabulary. The MCP server publishes no `instructions`. Claude Code defers MCP tool schemas by default and surfaces only names and server instructions, so the absent instructions were the entire discoverability budget, spent on nothing.
4. **Unbounded and duplicative outputs.** `file_outline` returned an 85,929-character outline of one file to answer a question about a 30-line function in a different file; `find_references` bounds files but not occurrences; `code_search_semantic` returned 32-63 KB payloads whose useful content a subsequent `exact_search` reproduced at roughly 1/20th the cost, with the current embedding model ranking wrong-module chunks above the target.

External evidence (Claude Code's removal of RAG in favor of agentic grep, the SWE-agent ACI ablations on succinct search output, Anthropic and OpenAI MCP guidance, Cursor's semantic-search study, the corrected JetBrains IDE-tools study) consistently identifies interface economics — few tools, compelling names and instructions, succinct bounded output the client's own tools can act on — as the lever that decides whether a code-intelligence tool helps or taxes a frontier agent. The target clients differ structurally: Codex CLI loads every MCP schema into every request and is shell-native, while Claude Code defers schemas and routes discovery through names and server instructions.

The benchmark harness itself also measured the wrong contract: it mandated LCTK as the primary exploration channel (so voluntary adoption was unobservable and a zero-LCTK run was a fatal error), and it computed paired inference only over final-answer quality metrics while the product promise — fewer seconds, steps, and tokens — was recorded as raw totals with no statistics.

## Decision

The MCP tool surface is redesigned around the agent's economics rather than the index's epistemology.

**Response contract.** Tools return one plain-text block, formatted for direct agent consumption (search hits as `path:line: text` lines), with no `structuredContent` duplicate and no published output schema. Status metadata appears only when it changes a correct next action: freshness and provenance are stated only when the index is not fresh, truncation only when results were truncated. An empty result is a single short line. Machine-stable data (paths, line numbers) keeps a fixed lexical shape instead of a JSON schema.

**Path contract.** Every path in every response is project-relative, and no response names a container-internal mount. The one-time `project_info` answer states the host root so a client can address files with its own tools; per-response envelopes do not repeat it.

**Primitives.** A `read_span` tool (path plus line range, bounded, numbered plain text) completes the find-then-show loop inside the surface. Search results accept a `context_lines` option. `file_outline` is bounded by default with explicit widening. Reference lookups bound occurrences, not only files.

**Surface size.** The default exploration surface is five tools: `exact_search`, `read_span`, `file_outline`, `repository_map`, and `find_symbol` (the merged definition/reference/caller lookup). Semantic search leaves the default surface until a retrieval-quality corpus proves the current embedding stack against `exact_search` on the same queries; graph, memory, git, and run tools remain capability-gated as today. Input schemas stay flat, tolerate unknown arguments instead of rejecting them, and declare no ignored parameters.

**Guidance.** The server publishes MCP `instructions` (routing: map first, exact search for strings, symbol lookup for names, spans to read; critical content within the first 512 characters). Tool descriptions state when to use the tool and what it returns; contract caveats live in documentation, not in the per-call token budget. A literal search that looks like an escaped regex and matches nothing says so in one line.

**Per-client delivery.** Codex-class clients are served by a CLI mirror of the read-only primitives (`lctk search`, `lctk read`, `lctk outline`, `lctk map`, `lctk symbol`) plus a documented AGENTS.md snippet, at zero schema cost; MCP remains the delivery for Claude-class clients where deferred schemas make names and instructions the adoption lever.

**Measurement.** The benchmark harness gains a free-choice mode: the treatment arm exposes the server without mandating its use, a treatment run with zero LCTK calls is recorded as an adoption observation rather than a fatal error, and paired deltas with confidence intervals are computed for elapsed time, tokens, turns, and tool calls alongside the quality metrics. The treatment MCP server name is configuration, enabling competitor-control arms through the identical harness. The gateway logs a per-phase timing breakdown for every tool call so fixed-cost work is ranked by measurement before it is optimized.

The Go import resolver in the derived code graph is fixed to resolve module-path imports, so the graph tools work on Go repositories, including this one.

## Alternatives considered

### Keep the typed structured-output registration and optimize the structs

Rejected. The output schema and the structured duplicate are constant costs charged to every call and every `tools/list`, and no field in them changed an agent's next action in any measured trace. Text output with a stable lexical shape preserves machine readability at a fraction of the cost.

### Keep the mandated-use benchmark and add a second mandated variant

Rejected. A mandate cannot observe non-adoption, which is itself a primary product signal, and it charges ritual calls to every treatment run. Comparability with the earlier mandated campaigns is preserved by keeping the mandated prompt available as a configuration, not as the default.

### Rebuild semantic search first

Rejected for this slice. In every measured trace the semantic tool was the most expensive and least useful call, and the audit's external evidence places its winnable regime (very large unfamiliar repositories, conceptual queries) outside the current corpus. It returns to the surface when a retrieval-quality measurement earns its place; removing it now is one line of surface configuration, not a deletion of the pipeline.

### Serve Codex through MCP with a minimized schema set

Rejected as the primary Codex path. Codex CLI re-sends every schema with every request and its native habit is the shell; a CLI mirror plus an AGENTS.md hint costs nothing per turn and matches trained behavior. The MCP endpoint remains available to Codex users who prefer it.

## Consequences

### Positive

- Every response byte the agent pays for is either the answer or a warning that changes the next action.
- The find-show loop closes inside the surface; the client's native tools also work, because paths are client-consumable.
- The five-tool surface fits both clients' economics, and adoption becomes measurable instead of mandated.
- Competitor arms and telemetry inference make the next campaign able to distinguish implementation failure from concept failure.

### Negative

- Clients that consumed `structuredContent` must switch to parsing the text block; the pre-1.0 surface makes this acceptable now and expensive later.
- Gateway tests asserting structured outputs must be rewritten against the text contract.
- The mandated-mode campaigns' treatment numbers are not directly comparable to free-choice campaigns; comparisons continue within a mode.

### Follow-up

- Re-run the paired benchmark in free-choice mode on a corpus that includes large repositories absent from model training data, with pre-registered success thresholds, before any claim about product value.
- Decide semantic search's future (code-trained embedder plus real lexical scoring plus reranking, or removal) from the retrieval-quality corpus, not from taste.
- Revisit the strategic weighting between search quality and the governance layer (propose/approve/act, scope enforcement, freshness honesty, project memory) once the free-choice campaign reports.
