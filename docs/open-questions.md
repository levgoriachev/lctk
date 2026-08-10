# Open questions

This document contains only decisions that have not yet been made. Once a decision is made, the item is moved to the relevant topic document and, if necessary, to an ADR.

## Codex configuration and credentials

Slice 0.4 measured the schema itself; see the [results](spikes/codex-compatibility-results.md). Resolved for Codex extension `26.727.40816` and bundled `codex-cli 0.146.0-alpha.9.2`:

- project-local configuration is possible at `.codex/config.toml`, but only in a trusted project, and it overrides same-named user-global entries;
- a bearer token cannot be supplied without an environment variable, because an inline `bearer_token` is rejected for a Streamable HTTP server;
- no key helper or credential command exists for MCP servers; the alternatives are an environment variable or OAuth;
- secret-free configuration is committable, because both credential mechanisms reference environment-variable names rather than values;
- `config/mcpServer/reload` is the reload mechanism and does not require restarting the editor.

Decided since, in [ADR-0014](adr/0014-project-credential-delivery.md): the token reaches the client in the environment of a process LCTK starts, per project, with the durable alternative printed rather than applied; and LCTK generates a user-global `mcp_servers` entry, written into a marker-delimited region of the user's own file.

What remains undecided:

- whether LCTK should offer a local OAuth path to avoid environment variables entirely, which would also let a user-started editor authenticate on its own and would serve the browser-origin flow in [`security.md`](security.md);
- whether a trusted project-local file is worth generating in addition to the user-global entry;
- how LCTK reacts when a repository ships its own `.codex/config.toml` that shadows a generated project endpoint, now that LCTK can detect a same-named entry it did not write.

## Daemon management API

A transport and permission model must be selected among the `lctk` CLI and Admin UI, the host daemon, and the container control plane:

- localhost HTTP;
- named pipe/Unix domain socket;
- a combined approach.

The Admin API must not accidentally become a regular project MCP capability.

## Runtime policy details

Open items:

- always-on startup semantics after OS sign-in;
- the default idle timeout `N` for on-demand mode;
- startup timeout and the behavior of a long-held MCP call;
- machine-wide settings or project overrides for individual process policies;
- precise draining and shutdown behavior during an active background operation.

## Watcher/indexing policy

Settled in Slice 2.1 by [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md):

- debounce default: 3 seconds, with a 30-second ceiling on deferral by continuous editing;
- debounce configuration scope: machine default in the host settings file, project proposal in the manifest, clamped by the host to between 200 ms and 60 seconds;
- what happens when observation is incomplete: an explicit gap, never an optimistic answer.

Open items:

- bulk-change thresholds are set at 10,000 pending paths by value, not by measurement;
- which generated or excluded paths count as activity;
- how to handle project configurations that intentionally index generated code;
- the atomic consistency model across multiple backends;
- whether macOS warrants FSEvents, and the cgo dependency it would add to the host binary, in place of one kqueue descriptor per watched directory.

## Resource planning

Settled in Slice 2.3: the background-load mode controls container CPU and index concurrency, is set machine-wide with a per-project override in the registry, and leaves memory uncapped unless asked, because a CPU limit throttles an indexer and a memory limit kills it.

The following must be defined:

- Stage 7 measured index/database growth through one million synthetic files or chunks, but the disk formula for a heterogeneous large real repository remains open;
- RAM and CPU budgets by profile and backend;
- when to warn, prohibit startup, or permit swapping;
- the lifetime of the shared embedding inference process;
- the meaning of `ready`: a warm process or an API capable of starting a worker quickly;
- performance classes for baseline and stress repositories.

## Language adapters

The initial ecosystems have been chosen, but their order has not been accepted:

- TypeScript/JavaScript;
- Python;
- Rust;
- C/C++.

Supported LSP implementations, toolchain versions, runner images, and rules for installing third-party binaries must be selected.

## Manifest schema

It has been accepted that a safe `.mcp-project.yaml` may be stored in Git, while the host path, secrets, and grants may not. Slice 1.1 settled the mechanics:

- the manifest carries an explicit `schema_version`, and a newer version than the build understands is refused;
- the untracked override is `.mcp-project.local.yaml` beside the tracked file, and it wins field by field;
- unknown fields produce warnings rather than failing registration, so a manifest written for a newer LCTK still works;
- a declaration of a path, mount, grant, secret, credential, or capability is rejected at any nesting depth, and the manifest type has no field capable of holding one.

The following must still be formalized:

- the agent-generated command proposal and user confirmation flow, since manifest commands are currently parsed as proposals and never executed;
- environment and secret references, given that the manifest may not contain secrets;
- migration and backward compatibility once a second manifest schema version exists.
