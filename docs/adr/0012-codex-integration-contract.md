# ADR-0012: Codex integration contract

- Status: accepted; credential delivery amended by [ADR-0026](0026-owner-approved-oauth-for-project-mcp.md)
- Date: 2026-07-31
- Deciders: project maintainers

## Context

Codex is the first client LCTK must serve. Until Slice 0.4 the integration was an assumption: the roadmap listed the configuration path, transport fields, credential mechanism, required server behavior, and reload story as things to be checked, and [`docs/open-questions.md`](../open-questions.md) recorded five unanswered questions about them.

Slice 0.4 replaced the assumption with measurement. The [verification contract](../spikes/codex-compatibility.md) fixed the method and the hard gates, and the [measured results](../spikes/codex-compatibility-results.md) record the outcome against exact artifacts: VS Code extension `openai.chatgpt` `26.727.40816` and the `codex-cli 0.146.0-alpha.9.2` binary bundled inside it. A tracked harness under [`spikes/codex-compatibility/`](../../spikes/codex-compatibility/) drove that real binary against an LCTK-shaped project endpoint. All six hard gates pass.

The measurement changed several working assumptions:

- a project-local `.codex/config.toml` does exist, but only takes effect in a project the user has marked trusted, and it overrides a same-named user-global entry, including its URL;
- an inline `bearer_token` is rejected outright for a Streamable HTTP server, and no key-helper or credential-command mechanism exists, so a token can only come from an environment variable or from OAuth;
- server-initiated streaming is not needed: the client's `GET` stream attempt can be refused and the client proceeds normally;
- reload is a full reconnect with a new session rather than an in-place refresh;
- the local reachability probe used by `codex doctor` is unauthenticated, so a typed `401` still counts as reachable.

Two facts bound this decision. The measured Codex build is an alpha, and the app-server protocol used to drive it is explicitly experimental. The evidence is one run on one Windows host, which [`docs/compatibility.md`](../compatibility.md) classifies as local measured evidence: stronger than documentation, weaker than hosted test evidence.

## Decision

Adopt the measured contract as the basis for LCTK's Codex integration.

### Transport and protocol

LCTK exposes project endpoints as Streamable HTTP at `/projects/{project_id}/mcp` and targets MCP protocol version `2025-06-18`. A stateless JSON endpoint is sufficient; LCTK does not owe Codex a server-initiated event stream, and refusing the `GET` stream is acceptable behavior. LCTK must tolerate repeated full handshakes, and no durable state may live only in an MCP session, because reload discards sessions.

### Credentials

LCTK supplies project credentials as a bearer token referenced by environment-variable name, using `bearer_token_env_var`. LCTK never writes a token value into any generated file. Static and environment-derived HTTP headers may carry additional non-secret context.

### Authoritative project identity

The route remains the only authority on project scope, reaffirming [ADR-0001](0001-route-bound-project-scope.md). A `project_id` supplied by the model, by a tool argument, or by a repository-local Codex file never changes the scope LCTK serves. A credential issued for one project is refused on another project's route with a typed error.

### Generated configuration is derived, never authoritative

Codex configuration is an artifact LCTK generates and may regenerate. It is not a source of truth for project identity, registry contents, or grants. LCTK must assume the file can be edited, shadowed by a repository-local file in a trusted project, or shared with the ChatGPT desktop app and the CLI, and must not depend on a configuration entry name being unique or unmodified. Generated TOML must be escaped correctly, because one malformed key aborts the whole configuration load and silently removes every configured server.

### Local diagnostics

A project route answers an unauthenticated `HEAD` or `GET` with a typed `401` rather than failing to connect, so that the operator's existing Codex diagnostics report the project as reachable. LCTK builds its own `doctor` story on the same observable semantics instead of inventing a different reachability model.

### The experimental protocol stays in the spike

The app-server methods `mcpServerStatus/list`, `mcpServer/tool/call`, and `config/mcpServer/reload` are a verification driver only. No LCTK production code may depend on them.

### Version binding

This contract is bound to the artifacts named in the results document. A materially different Codex version requires re-running the harness before the integration is claimed to work. Slice 1.5 must additionally exercise the extension user interface, which this decision does not cover.

## Alternatives considered

### Treat the published documentation as the contract

Rejected. Documentation was necessary but not sufficient. It omitted the observable consequences that most affect LCTK's design, notably that reload reconnects rather than refreshes and that the reachability probe is unauthenticated. A widely circulated third-party guide was actively wrong about both the trust precondition for project-local configuration and the availability of inline tokens.

### Require session and streaming support before shipping a gateway

Rejected. The measurement shows Codex works against a stateless JSON endpoint that refuses the stream. Building server-initiated streaming into Slice 1.3 would add cost with no client benefit. LCTK may add it later for another client without breaking this contract.

### Write the token into generated configuration

Not available. Codex rejects an inline `bearer_token` for a Streamable HTTP server. This turned out to be favorable: it forces the secret out of every generated file and makes a project-local Codex file safe to commit.

### Generate only a project-local trusted configuration file

Rejected as the sole mechanism. It requires a user-global trust record anyway, it silently does nothing in an untrusted project, and a repository author can shadow it. It remains a candidate as an additional convenience, decided separately.

### Depend on the app-server protocol in production

Rejected. It is labelled experimental, it is the richest surface Codex exposes, and depending on it would tie LCTK's control plane to an interface that may change without notice.

## Consequences

### Positive

- The Codex integration rests on measured behavior, and the measurement is reproducible with one command.
- Slice 1.3's gateway is simpler than planned: no server-initiated streaming is required.
- No LCTK-generated file can leak a project token, which keeps the manifest boundary in [`docs/security.md`](../security.md) intact and makes a committed Codex file safe.
- Route-bound scope is confirmed to hold through a real client rather than only in LCTK's own tests, which strengthens [ADR-0001](0001-route-bound-project-scope.md).
- Three of the five open Codex questions are closed, and the remaining ones are now specific.
- The harness gives LCTK a regression check for future Codex releases.

### Negative

- The contract is bound to an alpha Codex build, so re-measurement is an ongoing cost rather than a one-time task.
- Credential delivery is unsolved and is now the critical path: extension settings cannot inject an environment variable, and a newly created user-level variable is invisible to an already-running editor.
- A repository author can shadow a generated project endpoint in a trusted checkout, which LCTK must detect or tolerate rather than prevent.
- Codex configuration is shared with the ChatGPT desktop app and the CLI, so an LCTK-generated entry is not scoped to one editor and LCTK is not the only writer of that file.
- Evidence covers Windows only; macOS is unverified for this boundary.

### Follow-up

- Decide how a per-project grant token reaches the environment the editor inherits. This blocks Slice 1.5 and deserves its own ADR.
- Decide whether LCTK generates a user-global entry, a trusted project-local file, or both.
- Decide whether a local OAuth path is worth avoiding environment variables in a local-first product.
- Define LCTK's behavior when a repository ships a `.codex/config.toml` that shadows a generated project endpoint.
- Re-run the harness on macOS and on the first non-alpha Codex release that LCTK targets.
- Exercise the extension user interface in Slice 1.5 before any end-to-end Codex claim.
- Measure `startup_timeout_sec` and `tool_timeout_sec` enforcement once a real gateway exists.
