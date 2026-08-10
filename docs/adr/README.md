# Architecture Decision Records

ADRs record significant decisions together with their context, alternatives, and consequences.

## Statuses

- `proposed` — under discussion;
- `accepted` — accepted;
- `rejected` — rejected;
- `superseded` — replaced by a newer ADR.

## Index

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-route-bound-project-scope.md) | Project scope is bound to the route and server-side context | accepted |
| [0002](0002-host-daemon-and-container-runtime.md) | The host daemon manages lifecycle while services remain containerized | superseded by ADR-0009 |
| [0003](0003-reusable-images-and-project-stacks.md) | Reusable images and separate project stacks | accepted |
| [0004](0004-stable-aggregated-tool-api.md) | Stable aggregated MCP tool API | accepted |
| [0005](0005-host-watcher-and-incremental-indexing.md) | Host watcher and incremental indexing | accepted |
| [0006](0006-go-language-and-toolchain.md) | Go language and toolchain for LCTK-owned code | accepted |
| [0007](0007-unified-versioning.md) | Unified product versioning | accepted |
| [0008](0008-platform-and-ci-baseline.md) | Platform targets and hosted CI baseline | accepted |
| [0009](0009-embedded-go-gateway-and-project-runtime.md) | LCTK-owned Go gateway embedded in the host daemon with containerized project services | accepted |
| [0010](0010-codebase-memory-mcp-reference-only.md) | Codebase Memory MCP is reference-only prior art | accepted |
| [0011](0011-zoekt-exact-search-backend.md) | Zoekt exact-search backend behind an LCTK adapter | accepted |
| [0012](0012-codex-integration-contract.md) | Codex integration contract measured against the real client | accepted |
| [0013](0013-registry-persistence.md) | Versioned JSON document for the local project registry | accepted |
| [0014](0014-project-credential-delivery.md) | Project credential delivery to a local client | accepted |
| [0015](0015-change-observation-is-complete-or-declared-incomplete.md) | Change observation is complete or declared incomplete | accepted |
| [0016](0016-admin-surface-and-local-session.md) | The admin surface and its local session | accepted |
| [0017](0017-command-policy-and-the-runner.md) | A repository proposes commands, the owner approves them, a client runs them by name | accepted |
| [0018](0018-the-index-describes-the-disk.md) | The index describes the disk, and a write that changed nothing costs nothing | accepted |
| [0019](0019-tree-sitter-symbol-layer.md) | Tree-sitter is the symbol layer, and diagnostics stop at syntax | accepted |
| [0020](0020-shared-embedding-and-project-semantic-store.md) | Shared local embedding inference and an isolated SQLite semantic store per project | accepted |
| [0021](0021-derived-code-graph-and-explicit-project-memory.md) | A derived name-matched code graph and explicit reviewed project memory | accepted |
| [0022](0022-transactional-bootstrap-update-and-release-evidence.md) | Transactional bootstrap, update rollback, and evidence-gated releases | accepted |
| [0023](0023-managed-podman-wsl-runtime-and-windows-installer.md) | Managed headless Podman on WSL2 and a one-click Windows installer | accepted |
| [0024](0024-native-windows-setup-and-user-selected-storage.md) | Native Windows setup and user-selected installation and runtime-data locations | accepted |
| [0025](0025-native-windows-admin-and-complete-uninstall.md) | Native Windows administrator window and complete product uninstall | accepted |
| [0026](0026-owner-approved-oauth-for-project-mcp.md) | Owner-approved OAuth for project MCP clients | accepted |
| [0027](0027-native-setup-in-place-upgrade-and-repair.md) | Native setup in-place upgrade and repair | accepted |
| [0028](0028-authenticated-local-code-image-artifacts.md) | Authenticated local code-image artifacts | accepted |
| [0029](0029-selectable-cpu-and-nvidia-gpu-inference.md) | Selectable CPU and NVIDIA GPU inference distributions | accepted |
| [0030](0030-warm-index-code-exploration-benchmark.md) | Warm-index code-exploration benchmark | accepted |

Create a new ADR by copying [`template.md`](template.md). A number is not reused even if the ADR is later rejected.
