# LCTK documentation

This folder is the source of truth for current requirements and accepted architecture decisions. English is the canonical language for repository documentation, source-code comments, user-facing text, schemas, and examples. Translations may be added later, but they must not replace or contradict the canonical English documentation.

## Decision statuses

The documentation uses three categories:

- **Accepted** — the decision has been agreed upon and must be followed by the implementation.
- **Working assumption** — a reasonable current position that may be changed through an ADR.
- **Open question** — no decision has been made yet.

Significant architecture decisions are recorded in ADRs. Changing an accepted decision does not erase history: the old ADR is marked `superseded`, and a new one is then added.

## Navigation

| Document | Purpose |
|---|---|
| [`product.md`](product.md) | Product goals, users, capabilities, and boundaries |
| [`brand.md`](brand.md) | Accepted logo meaning, colors, variants, spacing, and distribution assets |
| [`architecture.md`](architecture.md) | Control plane, project stacks, MCP routing, and component boundaries |
| [`project-lifecycle.md`](project-lifecycle.md) | Add/start/stop/restart/remove operations and project states |
| [`indexing.md`](indexing.md) | Watcher, incremental indexing, freshness, and persistence |
| [`symbols.md`](symbols.md) | What a file declares, whether it parses, and how precise either answer is |
| [`security.md`](security.md) | Trusted-local model, project isolation, and owner-approved client OAuth |
| [`compatibility.md`](compatibility.md) | Platform targets, current evidence, and certification gaps |
| [`development.md`](development.md) | Go prerequisites, local checks, and foundation commands |
| [`versioning.md`](versioning.md) | Product, schema, image, and compatibility version rules |
| [`releasing.md`](releasing.md) | Signed release workflow, installation, update, and rollback |
| [`stage7-client-verification.md`](stage7-client-verification.md) | Complete MCP catalog evidence through Codex and an independent SDK client |
| [`stress.md`](stress.md) | Parameterized semantic and exact-search measurements through one million files |
| [`benchmarks/swe-explore.md`](benchmarks/swe-explore.md) | Warm-index SWE-Explore A/B measurement contract for Codex and Claude Code |
| [`benchmarks/swe-explore-single-run.md`](benchmarks/swe-explore-single-run.md) | Accepted single-instance pipeline and official-scorer readiness evidence |
| [`roadmap.md`](roadmap.md) | Small, verifiable vertical slices |
| [`spikes/gateway-evaluation.md`](spikes/gateway-evaluation.md) | Slice 0.2 gateway test and scoring contract |
| [`spikes/gateway-evaluation-results.md`](spikes/gateway-evaluation-results.md) | Slice 0.2 measurements and accepted gateway recommendation |
| [`spikes/search-backend-evaluation.md`](spikes/search-backend-evaluation.md) | Slice 0.3 persistent search backend test contract |
| [`spikes/search-backend-evaluation-results.md`](spikes/search-backend-evaluation-results.md) | Slice 0.3 measurements and accepted Zoekt recommendation |
| [`spikes/codex-compatibility.md`](spikes/codex-compatibility.md) | Slice 0.4 Codex verification contract |
| [`spikes/codex-compatibility-results.md`](spikes/codex-compatibility-results.md) | Slice 0.4 measurements and accepted Codex integration contract |
| [`spikes/codex-end-to-end-results.md`](spikes/codex-end-to-end-results.md) | Slice 1.4 end-to-end measurements against the real client, daemon, and containers |
| [`spikes/codebase-memory-mcp-assessment.md`](spikes/codebase-memory-mcp-assessment.md) | Comparative assessment of Codebase Memory MCP and the accepted reference-only disposition |
| [`spikes/symbol-backend-evaluation.md`](spikes/symbol-backend-evaluation.md) | Slice 4.1 symbol engine test contract |
| [`spikes/symbol-backend-evaluation-results.md`](spikes/symbol-backend-evaluation-results.md) | Slice 4.1 measurements and the accepted Tree-sitter recommendation |
| [`open-questions.md`](open-questions.md) | Unresolved product and technical questions |
| [`adr/`](adr/README.md) | Architecture decision log |

## Update rules

1. A newly agreed requirement is first recorded in the relevant topic document.
2. If the requirement changes a system boundary or long-term contract, an ADR is created.
3. Assumptions are not recorded as accepted decisions.
4. An implementation is not considered complete until the corresponding acceptance criterion has been verified.
5. The documentation must not claim that an integration works until a test has confirmed it.
