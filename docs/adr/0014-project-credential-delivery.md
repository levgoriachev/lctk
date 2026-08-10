# ADR-0014: Project credential delivery to a local client

- Status: superseded by [ADR-0026](0026-owner-approved-oauth-for-project-mcp.md)
- Date: 2026-08-01
- Deciders: project maintainers

## Context

[ADR-0012](0012-codex-integration-contract.md) closed the Codex protocol question and left one problem behind, named there as the item that blocks the end-to-end slice: how a per-project grant token reaches the environment the editor inherits.

The [measured results](../spikes/codex-compatibility-results.md) fix the constraints:

- an inline `bearer_token` is rejected outright for a Streamable HTTP server, and there is no key-helper or credential-command mechanism, so the only secret-free paths are an environment variable or OAuth;
- both `bearer_token_env_var` and `env_http_headers` carry variable **names**, so no generated file has to contain a secret;
- the variable is read by the Codex process, so it must be present in the environment that process inherits;
- the extension exposes no setting for `CODEX_HOME` or for MCP environment variables, so a per-workspace credential cannot be injected through extension settings;
- a newly created user-level variable is not visible to an already-running editor;
- `codex doctor --json` detects a missing variable and reports the specific remediation, so the failure is diagnosable rather than silent.

Two LCTK commitments bound the answer. [`docs/security.md`](../security.md) requires that one project's key does not open another project, a property Slice 1.3 verified on the wire. And LCTK is a local-first tool run by the machine owner, which makes an operating-system-wide change a heavier act than an application-scoped one.

## Decision

A project grant token reaches a client through an environment variable that LCTK places in a process it starts, and never through a persistent change LCTK makes to the operating system on the user's behalf.

### The variable is per project

Each project keeps its own token in its own variable, named `LCTK_TOKEN_<PROJECT_ID>` with the identifier upper-cased and non-alphanumeric characters replaced. Two projects never share a variable and never share a token.

This is deliberate cost. A single shared variable would make setup a one-time act, but it would also mean that a client working in one project can reach another project's route successfully. That is precisely the "mixing results from different projects" boundary in [`docs/security.md`](../security.md), and it is the property Slice 1.3 verified by refusing a foreign token with `AUTH_FORBIDDEN`. Convenience does not buy the right to withdraw a verified guarantee.

### The primary mechanism is a launched process

`lctk codex launch PROJECT` starts the editor with the project's token already in the child environment. The credential exists only in the memory of a process LCTK started, is never written to a file outside the owner-only LCTK home, and requires no operating-system state at all. The mechanism is identical on Windows, macOS, and Linux, which no persistent-environment mechanism is.

This also resolves the staleness problem rather than working around it: an editor started by LCTK inherits the current token by construction, so the "variable created after the editor started" failure cannot occur on this path.

### LCTK prints the persistent fallback, and does not apply it

An operator who wants the variable to survive independently of how the editor is started can set it persistently. LCTK prints the exact platform-specific command and the exact value under `--reveal`, and stops there. It does not write to the Windows user environment, a shell profile, or a launch agent.

The reason is a boundary, not a limitation of effort. Editing the user's persistent environment is a change to the machine that outlives LCTK, is invisible at the point of failure, and is not what the user asked for when they registered a folder. Printing keeps the act explicit, attributable, and trivially reversible by the person who performed it.

### Generated client configuration is previewed before it is written

`lctk codex config PROJECT` prints the configuration entry by default and writes only under `--apply`. Writes go into the user's real `CODEX_HOME/config.toml`, inside a marker-delimited region LCTK owns, leaving every other byte of the file untouched, after taking a backup. LCTK refuses to overwrite a same-named entry it did not generate unless told to.

This follows from [ADR-0012](0012-codex-integration-contract.md): the file is shared with the ChatGPT desktop application and the CLI, LCTK is not its only writer, and one malformed key aborts the whole load and silently removes every configured server. A tool that quietly rewrites that file is a tool that can silently disconnect everything else the user runs.

### The failure is diagnosable from both sides

`lctk codex status` reports, without revealing the token, whether the configuration entry exists, whether the variable is present in the environment LCTK itself can see, and whether the endpoint answers. On the wire, an unauthenticated request to a project route returns a typed `AUTH_REQUIRED` whose recommended action names the restart case, because a client that inherited a stale environment cannot otherwise tell that condition apart from a missing grant. Neither surface reveals whether the project exists, so the ordering established in Slice 1.3 is preserved.

### OAuth is deferred, not rejected

A local OAuth authorization server would remove the environment variable entirely and would let a user-started editor authenticate on its own. It is the better long-term answer for a client that supports it, and it is the natural way to serve a browser origin later. It is deferred because it is a substantially larger surface than this slice, and because the launched-process mechanism is sufficient to prove the end-to-end chain now. Nothing in this decision blocks adding it: the grant model, the route-bound scope, and the typed errors are unchanged by how the credential arrives.

## Alternatives considered

### Write the variable into the persistent user environment

Rejected. On Windows it means writing `HKCU\Environment` and broadcasting a settings change; on macOS there is no supported equivalent for a GUI application, so it degrades to editing a shell profile, which only helps an editor started from a shell. The mechanism is therefore inconsistent across the platforms LCTK targets, it still requires an editor restart, and it leaves durable machine state behind for a per-project credential that can be revoked at any time. It remains available to the operator as a printed command.

### One shared token for every local project

Rejected. It makes setup a one-time act and removes the per-project restart, which is a real benefit. It also lets a client scoped to one project reach every other registered project, which is the isolation property `docs/security.md` states and Slice 1.3 verified. The failure mode is not hypothetical for this product: the client is an agent, and an instruction encountered inside one repository is exactly the way a request for another project's contents arises.

### An isolated `CODEX_HOME` owned entirely by LCTK

Rejected. It would remove the merge problem completely, since LCTK would own the whole file. It would also detach the editor from the user's own Codex state, including their authentication and their other configured servers, which is a worse outcome than the merge problem it solves.

### Parse and rewrite the user's `config.toml`

Rejected. Round-tripping TOML through a parser discards comments, ordering, and formatting in a file the user owns and edits by hand. A marker-delimited textual replacement changes only the region LCTK generated.

### Put the token in the configuration file

Not available. Codex rejects an inline `bearer_token` for a Streamable HTTP server. This constraint is favorable and is treated as a property to preserve rather than an obstacle: no LCTK-generated file contains a secret.

## Consequences

### Positive

- The end-to-end chain is reachable now, with no dependency on an unbuilt OAuth surface.
- The credential never exists at rest outside the owner-only LCTK home.
- LCTK makes no persistent change to the user's machine to deliver a credential.
- Per-project isolation survives intact, including through a real client.
- One mechanism covers Windows, macOS, and Linux, so the verification transfers rather than being re-litigated per platform.
- The user's existing Codex configuration cannot be silently damaged by an LCTK write.

### Negative

- An editor the user started themselves does not carry the credential, so the primary path asks the user to start it through LCTK, which is a change to their habit.
- Registering a new project requires a new variable, so a persistently configured operator restarts the editor once per project.
- Revoking and reissuing a grant requires restarting whatever process holds the old value.
- LCTK writes into a file it does not own, and a repository-local Codex file in a trusted project can still shadow the generated entry, which LCTK detects and reports rather than prevents.

### Follow-up

- Decide whether a local OAuth authorization server is worth building for clients that support it, which would also serve the browser-origin flow in `docs/security.md`.
- Decide LCTK's behavior when a repository ships a `.codex/config.toml` that shadows a generated project endpoint, now that detection exists.
- Extend the launched-process mechanism to other clients as they are measured, rather than assuming this contract generalizes.
