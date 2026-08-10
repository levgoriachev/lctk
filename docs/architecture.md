# LCTK architecture

## Status

Architecture baseline. Go is selected for LCTK-owned code, and the shared MCP gateway is an LCTK-owned component embedded in the host daemon. The persistent exact-search engine is Zoekt behind an LCTK adapter under [ADR-0011](adr/0011-zoekt-exact-search-backend.md), and the local registry is a versioned JSON document under [ADR-0013](adr/0013-registry-persistence.md). The change journal is a versioned per-project document in the LCTK home under [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md). Stage 5 adds one installation-wide stateless llama.cpp embedding service and one transactional semantic SQLite database in each project's existing state volume under [ADR-0020](adr/0020-shared-embedding-and-project-semantic-store.md). Stage 6 adds the derived syntax/name-match graph and explicit reviewed project memory to that isolated database under [ADR-0021](adr/0021-derived-code-graph-and-explicit-project-memory.md). Codebase Memory MCP remains reference-only prior art under [ADR-0010](adr/0010-codebase-memory-mcp-reference-only.md); it is not an LCTK core, backend, wrapper, or production dependency.

## Current implementation

The current implementation includes the Windows one-click product path:

- one manifest-verifying unsigned Windows setup executable, a stable launcher, a versioned Go host core, and a sign-in daemon;
- a standard-library HTTP daemon with `GET /health`;
- the official MCP Go SDK Streamable HTTP handler at `/mcp` with temporary tool `foundation_info`;
- an `fsnotify` basic event-delivery proof;
- a read-only identity probe for LCTK's private Podman client and explicit `lctk-runtime-root` connection;
- the local project registry from Slice 1.1: canonical host paths, stable project identities, `lctk project add/status/remove`, and manifest parsing, none of which start a service;
- the per-project runtime from Slice 1.2 and ADR-0023: a deterministic JSON plan, explicit Podman operations, a reusable versioned image, an isolated network and persistent volume, a read-only source mount, lifecycle commands, and typed health;
- the project-scoped MCP endpoint from Slice 1.3: `/projects/{project_id}/mcp` inside the host daemon, owner-approved per-project OAuth, the `project_info` tool, typed lifecycle and authorization errors, and request-correlated local logs;
- the client integration amended by ADR-0026: IDE-owned Streamable HTTP configuration, OAuth discovery, dynamic public-client registration, S256 PKCE, native owner approval, short-lived access tokens, and rotating refresh tokens;
- persistent exact search from Slice 1.5: a per-project search service in the project container, a staged generation store published atomically, the project's own ignore rules honoured, and the `exact_search` tool behind a stable host-side adapter;
- the host change journal from Slice 2.1: a native filesystem watcher per running project, normalized project-relative events, a configurable debounce, a persistent per-project journal that is either complete since its checkpoint or explicitly incomplete, and freshness reported through `project_info`;
- the constrained runner from Slice 3.2: `run_command` executing only what the machine owner approved, one container per run with the project mounted writable and everything else denied, and an append-only audit record;
- Git awareness from Slice 3.1: `git_status` and `git_diff` on the project route, read-only and route-scoped, plus the branch, commit, and dirty state in `project_info`;
- resource policy and the admin surface from Slice 2.3 and ADR-0025: background-load modes that change what a project costs, disk reporting with a refusal to start on a nearly full volume, and a native Windows administrator over an API a project credential cannot reach;
- incremental indexing from Slice 2.2: a settled batch applied to the index automatically, a gap reconciled instead of applied, a removed directory retracting everything beneath it, bulk changes rebuilt rather than applied, and a search that flushes pending changes before answering so an edit made a moment ago is already searchable.
- the syntax and symbol layer from Stage 4: one Tree-sitter engine in the project service for Go, Python, Rust, C, C++, JavaScript, TypeScript, and TSX; live file outlines; bounded name-matched definition and reference lookup; per-language syntax verdicts; and parse concurrency governed by the existing resource mode.
- persistent semantic intelligence from Stage 5: AST-aware chunks, transactional per-project SQLite state, hybrid lexical/vector ranking, and one shared pinned local embedding process with explicit model, generation, freshness, and failure evidence.
- the Stage 6 graph, repository map, and explicit memory layer: derived name-match calls and imports committed with semantic generations, deterministic bounded graph tools and maps, and revision-checked reviewed knowledge with provenance and Git awareness.
- Stage 7 installation and release hardening plus ADR-0023 and ADR-0024: a digest-verifying stable launcher, schema-2 signed manifests, plan-first native Windows setup with user-selected storage, pinned Podman/WSL artifacts, WSL prerequisite and reboot continuation, sign-in/Start-menu registration, transactional update and rollback, and fail-closed Windows-only publication gates.

`lctk project reindex` remains for explicit catch-up and for recovering a corrupt index, but it is no longer how the index keeps up with editing. The legacy `/mcp` endpoint remains foundation compatibility evidence only and is not project-scoped.

Git runs on the host rather than in the project container. The container mounts the source read-only and Git wants to refresh its index when asked for status, and the host is where the user's Git configuration lives, so an answer computed elsewhere would be about a different repository than the one they see. A project registered below a repository root is scoped to its own subtree, so a project endpoint cannot report a sibling directory's changes.

The watcher derives what it observes from the project's own service rather than reading ignore files itself, so the exclusion policy has exactly one implementation. On this repository that is 42 watched directories out of 56,077 on disk.

The search engine runs only inside the project container. It is built as a separate Go module, which keeps it out of the portable host executable by construction rather than by convention, as [ADR-0011](adr/0011-zoekt-exact-search-backend.md) requires. The host reaches it on a loopback port the container runtime assigns, so no port allocation is coordinated and no project service is reachable from the network.

## Principles

1. **The IDE is a client, not infrastructure.** VS Code presents the conversation, code, and diffs; heavy indexes live outside the extension.
2. **Scope is assigned by the server.** The authoritative project is determined by the endpoint, credential, and registry, not by model arguments.
3. **One set of artifacts, separate project state.** Images and binaries are reused; mounts, indexes, memory, networks, and volumes belong to a single project.
4. **Stable public tool API.** The client does not know the names of internal search, LSP, vector, or graph engines.
5. **Modular monolith before microservices.** A component becomes a separate service only when a genuine operational or dependency boundary exists.
6. **Persistent and incremental by default.** A restart must not imply a full rebuild.
7. **Local-first.** Core intelligence works after installation without an external backend.
8. **LCTK owns the product boundary.** External libraries and specialized tools may remain replaceable implementation details, but the project registry, grants, public MCP API, lifecycle, policy, orchestration, freshness model, and control plane are LCTK-owned. Complete external code-intelligence products do not become a second core behind an LCTK façade.

## Logical architecture

```text
VS Code + Codex                  Other explicitly granted clients
       │                                      │
       └──────────── Streamable HTTP MCP ─────┘
                              │
              127.0.0.1 embedded LCTK gateway
                    auth, routing, tool policy
                              │ project_id
             ┌────────────────┴────────────────┐
             │                                 │
       Project A stack                   Project B stack
       code-intel + runner               code-intel + runner
       isolated indexes                  isolated indexes
       isolated memory                   isolated memory
             │                                 │
       bind mount A                      bind mount B

Host-side LCTK daemon
├── local registry and secrets
├── shared MCP gateway and grant enforcement
├── host path canonicalization
├── managed Podman/WSL lifecycle
├── filesystem watcher and change journal
├── resource planning
├── local Admin UI
└── CLI API for lctk
```

## Host-side daemon

A small per-user daemon is installed on Windows and starts at sign-in. It is the only LCTK component that:

- registers arbitrary host paths;
- canonicalizes paths using host OS facilities;
- manages its private Podman client and the `lctk-runtime` WSL machine;
- stores the local registry, registered OAuth clients, and hashed project authorizations;
- serves the shared project-scoped MCP gateway;
- watches for file changes;
- serves the Admin API and `lctk` CLI;
- starts on-demand stacks and enforces the idle policy.

The embedded gateway does not expose the daemon's runtime client or administrative handlers to coding MCP requests. Project services do not receive a runtime socket. Coding MCP tools do not control the daemon or Podman directly.

## Shared control plane

The shared control plane is an LCTK-owned Go component inside the host daemon, as accepted in [ADR-0009](adr/0009-embedded-go-gateway-and-project-runtime.md). It continuously provides a stable localhost endpoint. Its responsibilities are:

- Streamable HTTP MCP transport;
- OAuth discovery, owner approval, token validation, and revocation;
- extracting `project_id` from server-side route context;
- capability filtering;
- virtual project servers;
- schema and path normalization;
- timeouts and health aggregation;
- typed errors;
- local audit logs and trace context.

Accepted public route:

```text
http://127.0.0.1:4444/projects/{project_id}/mcp
```

The gateway uses the official MCP Go SDK and LCTK-owned registry, grant-validation, health-resolution, error-translation, and upstream-transport interfaces. Compatibility of route, authentication, and tool contracts does not depend on an external gateway product.

## Project runtime

The runtime model is a deterministic container, network, and volume namespace for each registered folder inside one installation-owned Podman WSL machine. Two runtime boundaries are sufficient:

- **code-intel** — read-only source mount, adapters, and persistent indexes;
- **runner** — writable source mount and project-command execution.

Heavyweight or incompatible backends can later be moved into separate services without changing the external MCP API.

Project-specific state:

- bind mount source root;
- exact/symbol/AST/semantic/graph indexes;
- project memory;
- change/index manifests;
- runtime caches;
- Docker network and volumes;
- health and freshness state.

## Code-intel boundary

Client tools describe a user action:

```text
exact_search
code_search_semantic
file_outline
find_definition
find_references
git_status
git_diff
run_command
repository_map
callers_find
callees_find
dependency_path
impact_analyze
memory_get
memory_search
memory_put
memory_delete
```

`code-intel` selects adapters, merges results, and removes duplicates. Every relevant response includes compact:

- normalized project-relative paths;
- provenance;
- freshness/index generation;
- source commit and dirty state, where applicable;
- bounded snippets and a pagination cursor.

## Client authorization

Access to the localhost MCP endpoint is not globally open. A client independently starts standard OAuth and LCTK creates an authorization only after native owner approval. Each authorization binds:

- client name;
- one exact project resource URL;
- the `lctk:project` scope;
- short-lived access-token hashes;
- one rotating refresh-token hash and expiration;
- revocation state.

Registering a project grants nothing. The user adds the displayed URL to Codex or another compatible client, starts that client's OAuth action, and approves or denies the pending request in the native window. LCTK never edits client configuration and never displays or stores recoverable bearer values.

## Admin UI

The minimal native Windows administrator must support:

- add/remove project;
- start/stop/restart;
- index progress and freshness;
- logs/doctor;
- runner network, resource mode, and capability policies;
- pending OAuth approvals and authorized-client revocation.

The Admin API is not exposed through the regular project `code` endpoint.

As amended by [ADR-0025](adr/0025-native-windows-admin-and-complete-uninstall.md) and [ADR-0026](adr/0026-owner-approved-oauth-for-project-mcp.md), `lctk-setup.exe --admin` renders native Win32 controls and calls the daemon's loopback JSON Admin API. It lists projects with their state, exact/semantic/graph index diagnostics, and change record; registers, starts, stops, restarts, and reindexes them; sets a project's resource mode; shows exact IDE connection steps; approves or denies pending OAuth connections; revokes authorized clients; shows runtime diagnostics and recent logs; and opens the registered uninstaller. Polling updates controls only when their content changes and restores list selection, text selection, caret, and scroll by stable identity, so background refresh cannot revoke an operator's in-progress OAuth decision or log inspection. OAuth credentials are never served to it. The independent session boundary originates in [ADR-0016](adr/0016-admin-surface-and-local-session.md); the IDE-owned browser page can only wait for a native decision and never receives an admin session.

## Runtime modes

The user selects one global machine mode:

- `always-on` — selected stacks are kept running;
- `on-demand` — the daemon starts a stack on an MCP request or meaningful project activity and stops it after a configurable idle timeout.

The first on-demand call may remain open until the stack is ready, subject to a separate startup timeout. The client must receive progress or status rather than an ambiguous `connection refused`.

## Installation and release activation

The native Windows setup records the user-selected installation and runtime-data directories before applying its verified plan. The installation directory owns host executables and state. LCTK passes the runtime-data directory to every private Podman process as `XDG_DATA_HOME`, placing the managed WSL disk, OCI images, volumes, indexes, and project memory on the selected volume. An existing managed machine is never moved implicitly. As defined by [ADR-0027](adr/0027-native-setup-in-place-upgrade-and-repair.md), a newer setup preserves both locations and persistent state, applies the shared health-gated update transaction, and repairs the exact same immutable version when rerun; it never silently downgrades. Native setup owns daemon stop/start around the shared coordinator. Explicit CLI update and rollback establish the same boundary themselves: they stop only the installation-recorded daemon, change project, inference, and host activation together, then restart the stable launcher so the daemon resolves the newly active core. If no daemon was running, the command does not create one.

Container services publish no WSL ports. WSL localhost forwarding is shared across distributions and can route a Podman port into Docker Desktop when both are running. LCTK instead reads the exact private container IP from its explicit Podman connection and exposes it only through a process-owned SSH tunnel bound to Windows numeric loopback. The SSH client uses the machine's private identity and accepts only the Ed25519 host key read through the verified Podman machine channel. The shared inference container joins each requesting project's otherwise isolated network, so projects address inference directly without gaining a path to other projects.

As defined by [ADR-0029](adr/0029-selectable-cpu-and-nvidia-gpu-inference.md), setup selects one complete local inference distribution: CPU or NVIDIA GPU. The owner choice is stored separately in `inference.json`; missing state means CPU for pre-0.1.12 compatibility. NVIDIA GPU requires the exact signed CUDA image, a compatible Windows adapter and driver, WSL GPU projection, the pinned `nvidia-container-toolkit-base` RPM installed inside only the private Podman machine, generated CDI device `nvidia.com/gpu=all`, measured complete CUDA layer offload, and a real embedding self-test. The pinned b10257 server emits its physical CUDA device and `offloaded N/N layers to GPU` evidence only at trace verbosity, so LCTK reads the container's fixed first two minutes of startup logs and rejects partial `N/M` offload. It immediately persists the verified result in an owner-only bounded evidence document keyed to the exact container, image, configuration, distribution, and process start; a restart must prove offload again. The trace stream uses a container-scoped 32 MiB log rather than the private machine's shared journal. No failed GPU gate can activate or report CPU as a fallback.

Distribution changes use a candidate container instead of deleting the active shared service. LCTK proves candidate health, embeddings, and backend evidence, records the exact project-network topology, renames the old service to a rollback identity, moves every `lctk-inference` alias, and proves the final name again before removing the rollback container and committing `inference.json`. A failed network or final-name gate restores the preceding container and aliases under a bounded recovery context. Update passes its selected distribution explicitly to every version-pinned project stack; rollback passes CPU to a pre-`0.1.12` stack, so starting a previous project cannot reread and reactivate a newer persisted GPU choice. Cached immutable CPU, CUDA, and CDI components remain available for offline switching; complete uninstall removes them with the installation-owned machine.

The user-facing `lctk` executable is a stable launcher. Host cores live under `versions/<version>/lctk-core`; `installation.json` names the active and previous relative paths together with their exact sizes and SHA-256 digests. The launcher verifies the selected bytes before execution. A freshly extracted official package has no activation document, so its launcher accepts only the sibling core whose post-signing identity was embedded during the release build.

Bootstrap and update share a signed release manifest but have different commit points. Bootstrap verifies the package, selected inference image, model, disk budget, and inference behavior before creating the first activation. Update proves and commits the selected inference distribution before stopping a project, installs the candidate code image, and restarts every previously running project against the target version. If a later project or host gate fails, recovery restores both the preceding inference runtime and its persisted selection. Official manifests pull digest-addressed registry images; under [ADR-0028](adr/0028-authenticated-local-code-image-artifacts.md), a local one-file RC may instead carry a signed image archive whose transport hash and loaded manifest digest are both verified. Only after all project health gates pass does update atomically activate the candidate host core.

A schema migration is never run against the authoritative database in place. The project service checkpoints schema 1, copies it, migrates and integrity-checks the copy, proves that no non-empty WAL sidecar remains, and atomically swaps the copy while retaining the original rollback bundle. Successful application validation creates a hardlink marker to the exact migrated inode; after a process interruption, an unmarked activation is validated again, while a partially restored database completes its atomic rollback before startup continues. A failed project health gate restores migrated projects in reverse order. Explicit rollback also visits stopped projects so their persistent databases cannot remain newer than the reactivated host and image.
