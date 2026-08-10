# ADR-0020: Shared local embedding inference and an isolated semantic store per project

- Status: accepted
- Date: 2026-08-04
- Deciders: project maintainers

## Context

Stage 5 needs semantic retrieval without a paid API, while preserving the existing resource and isolation contracts. A model process per project wastes memory, a shared vector database weakens project-state isolation, and a semantic index that cannot state which disk generation it describes would contradict ADR-0015 and ADR-0018.

The Tree-sitter layer already provides declaration byte extents for eight source-language variants. Other indexed text still needs a bounded, explicit fallback so documentation and configuration are not invisible to semantic search.

## Decision

Run one CPU-only embedding inference service per LCTK installation. It is shared stateless compute, managed by the host daemon, and exposes only an embedding endpoint to project services. The first pinned implementation is `llama.cpp` serving the official Apache-2.0 `nomic-ai/nomic-embed-text-v1.5-GGUF` Q4_K_M model. Model and image identities include immutable digests and are installed only after checksum verification.

Each project keeps its own semantic state in its existing persistent volume. The first vector adapter stores normalized little-endian `float32` vectors beside metadata in one transactional SQLite database and performs exact cosine ranking in LCTK-owned Go code. SQLite is provided through the cross-platform `ncruces/go-sqlite3` adapter. No project service can name or open another project's store.

Supported source files are chunked on Tree-sitter declaration extents. Oversized declarations are split on line boundaries without crossing the declaration. Other indexable text uses bounded overlapping line chunks and reports `chunk_precision: text`. The chunker bounds the complete embedding input, including the model task prefix, project-relative path, structural anchor, separators, and content, below the pinned runtime's context limit. Stable chunk identity is derived from project-relative path, structural anchor, and ordinal; a separate content digest decides whether embedding work is required. A changed chunking contract has an explicit revision and atomically rebuilds all derived semantic chunks before publishing that revision; explicit project memory remains intact.

`code_search_semantic` embeds the query locally and returns bounded project-relative matches with chunk precision, vector and lexical scores, the hybrid score, embedding model identity, index generation, freshness, and source state. Hybrid ranking is deterministic reciprocal-rank fusion over semantic and exact candidates. A vector-only answer is never silently presented as complete when inference or semantic state is unavailable.

Incremental indexing deletes chunks removed from a changed file, reuses unchanged content digests, embeds only new or changed chunks, and commits metadata and vectors atomically. A gap or failed batch leaves the previously published generation intact and reports it as stale or incomplete.

The host watcher places no fixed wall-clock deadline around a whole repository reconcile. A full semantic rebuild is transactional and may legitimately take longer than an edit-sized drain on constrained hardware; canceling the aggregate operation would discard every prepared vector and make the next retry restart from zero. Individual embedding requests remain independently bounded and fail fast, and a stopped project service terminates the in-flight reconcile.

## Alternatives considered

- **One model process in every project stack.** Rejected because model memory scales with active projects although inference is stateless.
- **A shared vector server.** Rejected because collection names and credentials would become an additional cross-project isolation boundary.
- **Cloud embeddings.** Rejected because core semantic search must work offline after installation.
- **A hash or bag-of-words pseudo-embedding.** Rejected because it is lexical search represented as vectors, not semantic retrieval.
- **`sqlite-vec` as the first adapter.** Rejected after build measurement. Its published cgo binding fails on the pinned Alpine/GCC toolchain, while its published WASM bundle has an incompatible host-function ABI with the Go runtime version declared by that bundle. Carrying a fork or false platform compile definition would make an experimental dependency part of the release boundary. The owned exact adapter is smaller and replaceable after scale evidence.
- **An unstable ANN index in the first adapter.** Rejected. Exact ranking establishes the correctness baseline; the adapter boundary permits replacement after measured scale evidence.

## Consequences

### Positive

- Model memory is paid once, while every project's persistent state remains isolated.
- Chunks reuse the byte boundaries Stage 4 already verified.
- SQLite transactions give one publication boundary for metadata and vectors.
- The public MCP contract does not expose `llama.cpp`, GGUF, SQLite, or the vector table layout.

### Negative

- Semantic readiness depends on a second managed runtime component.
- The initial vector adapter performs exact nearest-neighbour search; its one-million-file cost must be measured before release.
- The selected model is English-focused. Supporting another model is an adapter and compatibility decision, not an automatic download.

### Follow-up

- Measure retrieval quality, cold start, indexing throughput, memory, disk, and query latency on the accepted corpus and stress suite.
- Treat a model or embedding-dimension change as a semantic schema migration requiring a rebuild and rollback path.
