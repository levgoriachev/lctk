# Project lifecycle

## Entities

- **Registration** — a local record associating `project_id` with a canonical host path and policy.
- **Desired state** — the desired project state (`running` or `stopped`).
- **Runtime state** — the actual state of containers and services.
- **Index state** — the separate state of each persistent index.
- **Manifest** — a safe project declaration that may be tracked in Git.

## Commands

Minimal management API and CLI:

```text
lctk project add <path> [--profile full]
lctk project start <project>
lctk project stop <project>
lctk project restart <project>
lctk project status <project>
lctk project reindex <project> [--service <name>]
lctk project logs <project>
lctk project doctor <project>
lctk project remove <project>
lctk project purge <project>
```

The coding MCP endpoint does not receive these administrative operations automatically.

## Registration

`project add` must:

1. accept a native host path;
2. canonicalize and resolve the path using host OS facilities;
3. verify that the path exists on a local Windows drive addressable through the managed WSL2 mount;
4. show the actual mount to the user;
5. create a stable local `project_id`;
6. read and validate the safe fields in `.mcp-project.yaml`, if it exists;
7. store the host path, grants, and local overrides outside Git;
8. detect languages and estimate required components and resources;
9. show the download, disk, and runtime plan;
10. create reproducible project runtime configuration;
11. create separate networks and volumes;
12. start the stack according to the selected machine runtime mode;
13. begin layered initial indexing;
14. register the route and health state with the gateway;
15. generate local Codex MCP configuration without committing secrets;
16. show machine-readable and human-readable status.

The repository manifest never determines the authoritative host path.

## Managed runtime resource naming

[ADR-0003](adr/0003-reusable-images-and-project-stacks.md) left resource naming to be specified. Every name is a pure function of `project_id`, so names are stable across restarts and reinstalls and are recomputed rather than stored:

| Resource | Name |
|---|---|
| Network | `lctk-{project_id}-net` |
| Volume | `lctk-{project_id}-state` |
| Container | `lctk-{project_id}-code-intel` |
| Image | `lctk/code-intel:{product_version}` |

The image is shared by every project and its tag follows the unified product version from [ADR-0007](adr/0007-unified-versioning.md), so an upgraded LCTK requests a matching image instead of silently reusing an older one.

The deterministic runtime plan lives under the per-user LCTK home at `projects/{project_id}/runtime.json`, never inside the repository. It is derived state, rewritten from the registry on every start, and is not a source of truth. LCTK translates a verified local-drive path to its WSL mount and issues explicit Podman network, volume, and container operations; Compose is not part of the installed product. The service has no published WSL port. Its private container address is exposed to the host only through an authenticated process-owned SSH tunnel on Windows loopback. The shared inference container joins this one project network under `lctk-inference`; no project container joins another project's network.

The source is mounted read-only into `code-intel` at `/workspace`. Per-project state lives in the named volume at `/var/lib/lctk`. A writable source mount belongs to the future runner boundary, not to `code-intel`.

## Runtime states

Proposed state machine:

```text
registered/stopped
       │ start or on-demand activity
       ▼
    starting ───── failure ───► error
       │ health ready
       ▼
     running
       │ idle timeout or stop
       ▼
    stopping ────────────────► stopped

remove: registration removed, persistent data retained by explicit choice
purge: registration and selected persistent data explicitly deleted
```

Indexes have their own states and are not reduced to the stack state:

```text
missing | building | ready | updating | stale | corrupt | failed
```

`running` does not mean that every index is fully ready. MCP status and tool responses must show the readiness and freshness of the specific capability.

## Stop/start and persistence

`project stop`:

- shuts down watcher consumers, LSP processes, and workers gracefully;
- stops project containers;
- preserves the registry and project volumes;
- does not delete indexes or memory;
- leaves the shared daemon and gateway available.

A request to a stopped project in a manual or disabled-autostart scenario returns the typed error `PROJECT_STOPPED`, not an empty result or an incidental transport error.

`project start`:

1. checks volume integrity and schema versions;
2. obtains the current commit, branch, and dirty state;
3. reconciles the filesystem with the index manifest and change journal;
4. performs incremental catch-up;
5. rebuilds only a corrupt or incompatible index;
6. updates gateway health.

A full rebuild after every start is prohibited as the normal path.

## Machine runtime modes

### Always-on

Project stacks with the desired state `running` are kept running. The precise recovery policy after user sign-in or a daemon restart still needs to be formalized.

### On-demand

The stack starts in response to:

- an MCP call to the project;
- a new meaningful source-file change that requires indexing;
- an explicitly started project operation.

The first MCP call may remain open during startup, subject to a separate startup timeout. The client receives progress or status.

The stack stops after a user-configured `N` minutes following the last meaningful activity and completion of required processing. The value of `N` is user-configurable.

Meaningful activity:

- MCP tool call;
- a new change to an indexed file;
- an active runner process;
- processing a batch caused by a user change;
- an explicitly started reindex, build, or test operation.

The following do not constitute activity by themselves:

- the existence of an old uncommitted diff;
- health checks;
- internal logging;
- an idle open transport with no requests.

The behavior of excluded and generated paths requires a precise policy so that noisy build output does not keep the stack running indefinitely.

## Remove and purge

The operations are explicitly distinct:

- `stop` — stop compute and preserve everything;
- `remove` — remove registration and runtime resources; persistent data is not deleted implicitly;
- `purge` — delete the registration, indexes, memory, and volumes after separate confirmation.

Before a destructive action, the CLI and Admin UI must list the resources to be deleted and their approximate size.

## Typed failures

Minimum lifecycle codes:

```text
PROJECT_NOT_FOUND
PROJECT_STOPPED
PROJECT_STARTING
PROJECT_STOPPING
SERVICE_UNAVAILABLE
INDEX_BUILDING
INDEX_STALE
INDEX_CORRUPT
AUTH_REQUIRED
AUTH_FORBIDDEN
RESOURCE_LIMIT
INTERNAL_ERROR
```

Each error contains `code`, a clear `message`, `retryable`, `recommended_action`, `project_id`, `service`, and `request_id`.
