# Implementation plan: agent-first tool surface (ADR-0031)

- Status: implemented 2026-08-10 (slices A-G; semantic rebuild and the next
  paid campaign remain follow-ups by design)
- Date: 2026-08-10
- Decision record: [ADR-0031](../adr/0031-agent-first-tool-surface.md)

Slices land in order; each slice keeps `go build ./...`, `go vet ./...`, and the affected package tests green. The benchmark harness changes are last so the new surface and the new measurement land together but stay reviewable.

## Slice A — response contract (internal/gateway)

1. Re-register every tool without a typed output: handlers return one plain-text content block. This removes every published `outputSchema` and every `structuredContent` duplicate.
2. Text formats:
   - `exact_search`: one `path:line: text` line per match; a trailing `[truncated — N total, cursor: …]` line only when truncated; a `[stale: …]` line only when freshness is not `fresh`; empty result is `no matches`. `output_mode` accepts `content` (default), `files_with_matches`, `count`.
   - `file_outline`: indented `kind name  lines A-B` tree, bounded (see Slice C); parse errors reported as a leading line only when the file does not parse cleanly.
   - `repository_map`: existing budgeted text, minus the JSON envelope.
   - symbol/graph answers: `path:line: declaration` lines with an `ambiguous (N names matched)` header only when relevant.
3. Envelope rules: no `project_id`, `scope_source`, `root`, or provenance in per-call responses. Freshness/provenance appear only when they would change the caller's next action. `project_info` keeps the full picture (host root, capabilities, index state) as the one-time orientation call.
4. Input schemas: hand-written, flat, no declared-but-ignored parameters, unknown arguments tolerated. A literal pattern that looks like an escaped regex and matches nothing answers `no matches (pattern contains regex syntax — retry with mode:"regex"?)`.
5. Server `instructions` published at `mcp.NewServer` (routing guidance; critical content in the first 512 characters). Tool descriptions rewritten as when-to-use guidance.
6. Rewrite gateway tests against the text contract.

## Slice B — path contract and read primitives (internal/gateway)

1. No response mentions `/workspace`. `project_info` states the host root once; all other paths are project-relative.
2. New `read_span` tool: project-relative path plus 1-based inclusive line range, served from the host working tree with path confinement, numbered output, default and maximum span sizes, clear over-EOF behavior.
3. `context_lines` parameter on `exact_search` (default 0, bounded): context read host-side from the saved working tree after the pre-answer flush, grep `-C`-style output.

## Slice C — bounds and surface shape (internal/gateway)

1. `file_outline`: default depth and entry bounds with explicit widening parameters; oversized outlines end with one `[truncated — N declarations total]` line.
2. `find_symbol`: merged definition/reference/caller lookup (name → declarations, then references bounded per file and in total, callers included when the graph capability is present).
3. Default exploration surface: `project_info`, `exact_search`, `read_span`, `file_outline`, `repository_map`, `find_symbol`. `code_search_semantic`, `callers_find`, `callees_find`, `dependency_path`, `impact_analyze` register only when the owner enables the extended surface (settings toggle); memory, git, and run tools keep their existing capability gates.
4. Per-call phase timing (resolve, status probe, flush, backend call) recorded through the existing logging path so fixed costs are ranked by measurement.

## Slice D — Go import resolution (images/code-intel)

`resolveImport` learns Go module-path resolution: read the module path from `go.mod` at index time, map `module/path/pkg` imports to repository directories, drop the base-name fallback that manufactures false edges. Tests cover Go, and the existing JS/Python cases stay green.

## Slice E — CLI mirror (cmd/lctk)

`lctk search`, `lctk read`, `lctk outline`, `lctk map`, `lctk symbol` — read-only, resolving the project like existing CLI commands and printing the same text formats as the MCP tools, so Codex-class shell-native agents get the primitives at zero schema cost. A documented AGENTS.md snippet ships with the commands.

## Slice F — benchmark v2 (internal/sweexplore)

Change sites enumerated from reconnaissance; config `schema_version` bumps to 2.

1. **Free choice is the default.** `run.prompt_mode`: `free_choice` (default) or `mandated`. Free-choice prompt contains no MCP sentence at all — arms differ only in server availability. `mandated` reproduces the ADR-0030 sentence with the server name taken from arm configuration (runner.go:165 hardcode removed; ExplorationPrompt gains the arm/mode).
2. **Zero-treatment-calls is data.** The runner.go:120-122 fatal check applies only in `mandated` mode; in free choice the result records adoption. Native-arm contamination checks (runner.go:117-119, campaign_import.go:93) survive unchanged.
3. **Treatment is generic.** Tool-call evidence detection (parser.go:245-252) is parameterized by the configured server name instead of the `lctk` substring; the treatment tool allowlist moves to configuration. A competitor control (e.g. Serena) is the same harness with a different treatment server in its own campaign.
4. **Telemetry gets inference.** Per-pair deltas and deterministic bootstrap CIs (campaign.go:580-590, 654-678) extend beyond the four quality metrics to elapsed ms, billed token categories, turns, and tool calls. `ArmAggregate` gains adoption counters and native-tool-call totals; the parser counts native tool use (Codex `command_execution` items, Claude non-MCP `tool_use` blocks).
5. **Parser fixes.** Codex `cache_write_input_tokens` is read (currently dropped, so Codex cache-creation is silently zero); Codex model identity is documented as manifest-pinned because the trace carries none.
6. **Reporting.** Per-arm failure classes (timeout, invalid output, zero-adoption in mandated mode) replace the single global failure count; docs/benchmarks pages updated for both modes; single-run doc annotated as mandated-mode evidence.

## Slice G — docs and verification

README tool list, docs/indexing.md, docs/benchmarks/swe-explore.md, ADR index. Full `go build`, `go vet`, `go test ./...`; drive the new surface by hand through an in-memory MCP client transcript checked into the test suite; open the PR.

## Explicitly out of scope for this branch

- Semantic-search rebuild (embedding model, lexical scoring, reranking) — gated on a retrieval-quality corpus per ADR-0031.
- The next paid campaign itself — corpus selection (large post-cutoff repositories) is a separate decision after this branch merges.
