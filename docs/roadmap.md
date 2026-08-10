# Roadmap

## Delivery policy

Work proceeds in small, verifiable vertical slices. Each slice:

- belongs to the target architecture rather than being a disposable demonstration;
- has an explicit user scenario;
- includes automated tests for the relevant boundary;
- updates the documentation and ADRs;
- is not claimed to work without reproducible verification.

Codebase Memory MCP may be studied as architectural prior art, but it is not a roadmap backend or integration candidate. [ADR-0010](adr/0010-codebase-memory-mcp-reference-only.md) and the [comparative assessment](spikes/codebase-memory-mcp-assessment.md) record that boundary. Advanced graph, semantic, and repository-map work remains LCTK-owned and must be delivered through the same slice and verification policy.

## Stage 0 — Research and contracts

### Slice 0.1: Repository foundation

**Status:** complete as a public pre-alpha repository and executable foundation.

Delivered:

- Apache-2.0 repository, documentation, policy, and ADR baseline;
- Go 1.25 module and one `lctk` executable;
- official MCP Go SDK v1.7.0 Streamable HTTP compatibility tool;
- health endpoint, `fsnotify` watcher proof, and read-only Moby Docker diagnostic;
- automated tests and hosted Windows/macOS CI;
- non-publishing Windows amd64 and Darwin arm64 dry-run archives plus checksums.

Verification: clean-checkout checks run on hosted Windows and macOS runner environments. The workflows provide build, test, and artifact-construction evidence but do not certify Windows 10 22H2, macOS 13, Docker Desktop behavior, or native archive execution on those targets.

The current `/mcp` endpoint and `foundation_info` tool are foundation compatibility evidence only. They do not implement the ADR-0009 project-scoped gateway.

### Slice 0.2: MCP/gateway spike

The reproducible scenario, hard gates, measurements, and scoring rules are defined in the [gateway evaluation contract](spikes/gateway-evaluation.md).

**Status:** complete; the [measured results](spikes/gateway-evaluation-results.md) support the accepted LCTK-owned Go gateway in [ADR-0009](adr/0009-embedded-go-gateway-and-project-runtime.md).

Evaluated ContextForge, Docker MCP Gateway, MCPJungle, and a minimal custom gateway against the same scenario:

- Streamable HTTP;
- virtual `/projects/{id}/mcp` routes;
- server-side context injection;
- dynamic project registration;
- per-client and per-project tool filtering;
- typed errors;
- local Docker deployment;
- license, maintenance, protocol compliance, and overhead.

Outcome: ADR-0009 selects an LCTK-owned Go gateway embedded in the host daemon. Spike code remains evidence and does not become a production dependency.

### Slice 0.3: Search backend spike

**Status:** complete; the [measured results](spikes/search-backend-evaluation-results.md) support the accepted Zoekt backend in [ADR-0011](adr/0011-zoekt-exact-search-backend.md).

The [evaluation contract](spikes/search-backend-evaluation.md) and [measured results](spikes/search-backend-evaluation-results.md) compare Zoekt, Livegrep, and OpenGrok. `ripgrep` is the correctness oracle and diagnostic baseline, not a persistent backend.

Verify:

- incremental indexing;
- Windows and macOS paths through mounted `/workspace`;
- regex, literal, glob, and language filters;
- update/delete/rename;
- persistent restart;
- latency and disk use;
- license and arm64 images.

Outcome: ADR-0011 selects pinned Zoekt behind a narrow LCTK-owned working-tree adapter. Livegrep and OpenGrok fail required incremental/correctness or packaging hard gates. Spike code remains evidence and does not become production code.

### Slice 0.4: Codex compatibility spike

**Status:** complete; the [measured results](spikes/codex-compatibility-results.md) support the accepted integration contract in [ADR-0012](adr/0012-codex-integration-contract.md). All six hard gates of the [verification contract](spikes/codex-compatibility.md) pass against Codex extension `26.727.40816` and bundled `codex-cli 0.146.0-alpha.9.2`.

Using the current official documentation and the actual extension, verify:

- project-local config path/schema;
- Streamable HTTP fields;
- bearer token support;
- environment and key-helper capabilities;
- required server behavior;
- reload and reconnect UX.

Measured against Codex extension `26.727.40816` and bundled `codex-cli 0.146.0-alpha.9.2`:

- project-local `.codex/config.toml` works, but only in a trusted project, and it overrides same-named user-global entries;
- Streamable HTTP is first class, with `url`, `bearer_token_env_var`, `http_headers`, `env_http_headers`, `enabled`, and timeout fields;
- an inline `bearer_token` is rejected, so a token must come from an environment variable or OAuth, and no key-helper mechanism exists;
- `codex doctor --json` gives a local reachability and credential diagnostic with no model turn, and its reachability probe is unauthenticated, so a typed `401` still counts as reachable;
- `config/mcpServer/reload` is the reload mechanism, and it performs a full reconnect with a new session rather than an in-place refresh;
- the client speaks MCP protocol version `2025-06-18`, honors the server-issued session id, and works against a stateless JSON endpoint, so server-initiated streaming is not required;
- a model-supplied `project_id` does not change server-enforced scope, and a token issued for one project is refused on another project's route.

Outcome: a verified integration contract, not an assumption. Credential delivery to the editor environment is the remaining design problem, not protocol compatibility.

## Stage 1 — First end-to-end lifecycle

### Slice 1.1: Local registry

**Status:** complete. Persistence is recorded in [ADR-0013](adr/0013-registry-persistence.md).

Implemented:

- a typed project model and migrations, stored as a versioned JSON document in a per-user LCTK home with atomic replacement, refusal to reset on corruption, and a typed error for a newer schema;
- native path canonicalization: absolute resolution, symlinks, junctions, macOS firmlinks, Windows 8.3 short-name expansion, drive-letter case, and a measured rather than assumed case-sensitivity probe;
- a stable local project ID derived from the canonical path, identical for every alias of one folder and constrained to a charset safe in a route segment and a container name;
- `lctk project add/status/remove`, which start no containers, networks, volumes, or indexing;
- a manifest parser for `.mcp-project.yaml` with an untracked `.mcp-project.local.yaml` override, strict validation, warnings for unknown fields, and rejection of any attempt to declare a path, mount, grant, secret, or capability.

Verified:

- different Windows drives produce distinct identities;
- Windows and macOS path handling through hosted CI on both platforms;
- duplicate, case, and path-alias handling, with `os.SameFile` as the authority and the comparison key as the fallback for an unavailable volume;
- the manifest cannot replace the authoritative host path, both because such a declaration is rejected and because the manifest type has no field able to hold one.

### Slice 1.2: Reproducible project container

**Status:** complete. Resource naming is specified in [project lifecycle](project-lifecycle.md#compose-resource-naming), closing the [ADR-0003](adr/0003-reusable-images-and-project-stacks.md) follow-up.

Implemented:

- deterministic Compose generation, byte-reproducible and stored under the per-user LCTK home rather than in the repository, using long mount syntax so a Windows drive letter is unambiguous;
- a reusable versioned image built from a digest-pinned base, shared by every project, tagged with the product version per [ADR-0007](adr/0007-unified-versioning.md), plus `lctk image build/status`;
- a per-project network and persistent volume, with the source mounted read-only at `/workspace` and project state at `/var/lib/lctk`;
- `lctk project start/stop/restart`, where stop and remove release runtime resources but never delete the project volume;
- typed lifecycle state — `stopped`, `starting`, `running`, `error`, `unknown` — with container health, a one-line explanation, and an explicit `retryable` flag, and with an unreachable container runtime distinguished from a failed project so registry information stays usable while Docker Desktop is closed.

Verified:

- two projects receive separate mounts, networks, and volumes, confirmed in the runtime and by each container being unable to read the other's source;
- stop/start preserves marker state in the project volume, confirmed by a start counter and a creation timestamp surviving a full stop;
- generated configuration is reproducible, byte-identical across a restart;
- the source mount is genuinely read-only from inside the container.

Container-dependent verification runs against real Docker on a developer machine and skips explicitly on hosted runners, which have no usable Linux Docker daemon. It is never simulated. See [compatibility](compatibility.md).

### Slice 1.3: Project-scoped MCP `project_info`

**Status:** complete. This is the first slice an agent can actually connect to.

Implemented:

- the shared localhost gateway inside the host daemon, serving `/projects/{project_id}/mcp`, with the registry and grants read per request so a project registered while the daemon runs becomes reachable without a restart;
- an automatic project grant issued on registration, stored owner-only outside any repository, revoked when its only project is removed, and surfaced through `lctk grant show/list/revoke` with the token withheld unless `--reveal` is given;
- one tool, `project_info`, answering from the route and the server-side registry, and reporting `scope_source` so a caller can verify that its own arguments did not influence the answer;
- typed errors `PROJECT_NOT_FOUND`, `PROJECT_STOPPED`, `PROJECT_STARTING`, `SERVICE_UNAVAILABLE`, `AUTH_REQUIRED`, `AUTH_FORBIDDEN`, `RUNTIME_UNAVAILABLE`, and `RUNTIME_UNSUITABLE`, each carrying `retryable`, `recommended_action`, `project_id`, and `request_id`;
- request, project, grant, and client identifiers in local logs, with the token never written.

Verified:

- the credential and route must agree: a token issued for one project is refused on another with `AUTH_FORBIDDEN`, while each token still works on its own route;
- a model-supplied `project_id`, `repository_root`, or `path` does not change scope;
- a stopped project returns a typed `PROJECT_STOPPED` rather than empty data, and a starting one returns a retryable `PROJECT_STARTING`;
- an unauthenticated caller cannot learn whether a project exists, because the credential is checked before the registry;
- the host path is never exposed to the client; `project_info` reports the in-container workspace.

Confirmed by hand against a running daemon with a real container: `initialize` succeeded on the routed project, a foreign token was refused with 403, a `project_info` call carrying a deliberately wrong `project_id` still answered for the routed project, and a stopped project returned the full typed error envelope with 503.

### Slice 1.4: Actual Codex end-to-end

**Status:** complete. The chain is measured and passing against real components, and against two independent MCP client implementations. The [measured results](spikes/codex-end-to-end-results.md) record both runs and what they do not cover.

The acceptance criterion was widened during the slice. It originally read "verified through the Codex extension in VS Code". What the endpoint owes a client is the MCP protocol, not one editor's presentation of it, so a second unrelated client is stronger evidence than a second look at the same one. The slice is accepted on the protocol boundary being verified against two clients, one of them a real agent session. The Codex extension's own panels remain unclicked, which the results document states.

LCTK generates local Codex configuration and delivers the project grant. The scenario:

```text
register folder
→ start stack
→ connect project endpoint
→ project_info
→ attempt cross-project access and receive refusal/no data
→ stop and receive typed error
→ restart and reconnect
```

Credential delivery is decided in [ADR-0014](adr/0014-project-credential-delivery.md), which closes the item [ADR-0012](adr/0012-codex-integration-contract.md) named as blocking this slice.

Implemented:

- generation of the Codex `mcp_servers` entry for a project endpoint, written into a marker-delimited region of the user's own configuration file, leaving every other byte untouched, previewed by default and written only with `--apply`, backed up before each write, and parsed before it is written so a generated document cannot break the client's whole configuration load;
- refusal to overwrite a same-named entry LCTK did not generate unless forced, and outright refusal to rewrite one written as an inline key;
- credential delivery through a process LCTK starts, so the token exists only in that process's environment, no generated file holds a secret, and LCTK makes no durable change to the machine; the persistent alternative is printed for the operator to run, never applied;
- `lctk codex status/config/env/launch`, with the token withheld unless explicitly revealed;
- a typed `AUTH_REQUIRED` on an unauthenticated probe of any project route, whose recommended action names the case where a client started before its grant existed, worded identically for a project that does not exist.

Verified:

- the credential reaches a started process's environment, and nothing is left behind in LCTK's own;
- generating an entry never writes a token, and re-applying an unchanged entry rewrites nothing;
- writing preserves unrelated servers, tables, and quoted path keys in a file LCTK does not own;
- an unauthenticated `GET`, `HEAD`, or `POST` returns the same typed `401` for a registered project and an invented one.

Measured end to end against the real `lctk` executable, a real daemon, real containers, and the Codex binary the VS Code extension runs: register, start, connect, `project_info` with a deliberately wrong `project_id` argument, `403 AUTH_FORBIDDEN` for a foreign token, a typed `PROJECT_STOPPED` that survives into the client's own error text, project state preserved across a stop, and reconnection after a restart with no configuration change.

Repeated by hand against Claude Code, an unrelated MCP client, on the same live endpoint: handshake and `tools/list`; the credential stored as a variable reference rather than a value and resolved at connect time; a missing variable reported by name; a foreign token refused on another project's route while the correctly paired entry connected in the same check; a typed `PROJECT_STOPPED` for a stopped project; reconnection after a restart; and a real agent session calling `project_info` with a deliberately wrong `project_id` and receiving the routed project, `scope_source: route_and_registry`, and no host path.

This slice originally followed persistent search. It was moved ahead of it so that the client boundary is proven against a real client before the heaviest backend work begins, and so that search lands inside a chain already known to work end to end. The search steps of the original scenario move to Slice 1.5, which re-runs this chain with `exact_search` in it.

### Slice 1.5: Persistent `exact_search`

**Status:** complete. The exclusion policy and the generation store are described in [indexing](indexing.md).

Implemented:

- a per-project search service running in the project's Linux container, built as a separate Go module so the engine cannot enter the portable host executable, which is what [ADR-0011](adr/0011-zoekt-exact-search-backend.md) requires rather than merely asks for;
- a staged generation store: a build writes aside and publishes by replacing one symlink, so a live query sees a whole generation or the previous one and never a half-written index;
- delta builds that hard-link the previous shards, with a bounded delta depth that escalates to a full rebuild, and generation pruning;
- ignore rules from `.gitignore`, then `.lctkignore`, then an untracked `.lctkignore.local`, each honoured in nested directories and each able to re-include what an earlier one excluded, with a short overridable default list and unconditional exclusion of version-control metadata;
- literal and regular-expression search, case sensitivity, path globs, language filters, bounded previews, pagination, and limits;
- an `exact_search` tool on the project route behind [`internal/codeintel`](../internal/codeintel), the stable adapter [ADR-0004](adr/0004-stable-aggregated-tool-api.md) requires, reporting backend, schema version, index generation, index time, and file count as provenance;
- the service exposed through a process-owned SSH tunnel on an ephemeral Windows loopback port, derived from the private container address in the same inspect call that reports lifecycle state, so many projects run at once without shared WSL ports or number coordination;
- `project_info` reporting the search capability and index freshness, and `lctk project reindex` for explicit catch-up and for the documented recovery from a corrupt index.

Verified:

- results come only from the mounted project: a file beside the workspace, and a symbolic link pointing at it, contribute nothing;
- create, modify, delete, and rename, including a rename delivered as one batch;
- a restart answers from the persisted index without rebuilding, and reconciliation catches up changes made while the service was not running;
- a cursor from another index generation is refused rather than silently skipping or repeating results;
- absolute, parent-traversing, and Windows-style paths are refused rather than reinterpreted, in both globs and change batches;
- a corrupt index is a typed error rather than an empty result, and recovers with a rebuild;
- delta escalation loses nothing, and pruning never breaks the published generation;
- a targeted update applies the same ignore rules as a full build, so an ignored file cannot be added by one and dropped by the other.

Measured against this repository through a real container and the live endpoint: about 155 files indexed after ignore rules, literal and regular-expression queries with path globs, pagination walking a result set exactly once, create/rename/delete reflected across successive generations, and an index reused across a restart with the service on a new port the daemon rediscovered without being restarted. A real agent session called `exact_search` through a second MCP client and returned 9 matches with the first at `internal/gateway/gateway.go:286`; `git grep` independently reports the same 9 and the same first location.

A second agent query demonstrated the property that makes filesystem enumeration a requirement rather than a preference. Asked for a regular expression, it returned 8 matches across three files. `git grep` finds only 4 across two, because the third file was saved but not yet committed. A filesystem count agrees with the index exactly.

The ignore policy was not a planned item. It came from running against a real checkout: the first build walked a gitignored local directory of 278,000 cached files and never finished. A fixture would not have found it.

Its second half came from the observation that a version-control ignore file answers "what should not be committed", which is not the question the indexer is asking. `.lctkignore` exists so a deliberately uncommitted local directory can still be searched, and so the project rather than Git decides what is worth indexing.

This closes the [ADR-0011](adr/0011-zoekt-exact-search-backend.md) follow-up to define and test compaction thresholds before persistent search is declared complete. Its remaining follow-ups — watcher-driven scheduling, and published multi-architecture images from a release pipeline — belong to Slice 2.2 and to releasing.

The Slice 1.4 chain now includes search:

```text
register folder
→ start stack
→ connect project endpoint
→ project_info
→ exact_search
→ attempt cross-project access and receive refusal/no data
→ stop and receive typed error
→ restart and reuse persistent index
```

The search steps of that chain were run live for this slice. The cross-project refusal and the typed stopped-project error were measured in Slice 1.4 and are not changed by this one, because the route resolves the project before a tool is reached; that scope holds for `exact_search` specifically is covered by an automated test, which also asserts that a foreign project's service is never contacted.

This is the first required vertical slice from the original brief.

After this end-to-end boundary is reproducibly verified, open the documented external pull-request intake process.

## Stage 2 — Watcher and freshness

### Slice 2.1: Host change journal

**Status:** complete. The honesty rule the slice is built around is recorded in [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md), and it closes the [ADR-0005](adr/0005-host-watcher-and-incremental-indexing.md) follow-ups on normalization, coalescing, overflow, and debounce configuration.

The scenario: a developer edits files in an ordinary editor, and LCTK knows what changed without being told, or says plainly that it does not.

Implemented:

- a host watcher over native OS events, with recursion, coalescing, and normalization owned by LCTK rather than by the library: a project-relative path with forward slashes, a rename delivered as a removal of the old path and a write of the new one, a removed directory reported as a directory because once it is gone the filesystem can no longer say what it was, and a newly created directory adopted together with whatever a tool already wrote into it before a watch could be placed;
- a per-project change journal that makes one claim — every change since its checkpoint is in the pending list, or a gap says otherwise — deduplicated by path, atomically written, and versioned;
- a gap for every condition under which observation could have been incomplete, latched at the earliest reason, cleared only by a consumer that can prove it is closing the gap it set out to close rather than one that opened while it worked;
- the project service as the single authority on what belongs to the project, so the watcher derives its directory list from the same code that decides what is indexed instead of holding a second copy of the ignore engine;
- a watch budget that degrades to a capacity gap and reconciliation, rather than to failure or to exhausting the process's handles;
- configurable debounce, accepted at 3 seconds with a 30-second ceiling, as a machine default in the host settings file and a project proposal in the manifest that the host clamps;
- observation that follows use: a watcher is started for a running project, woken by a request to its route, and released when the project stops or goes idle, with the lapse recorded;
- freshness in the tool response — `project_info` reports a `changes` block and an `index.freshness` verdict that is never optimistic, so a project nothing is watching reports `unknown` rather than `fresh`;
- `lctk project watch`, which reads the journal from disk so it answers when no daemon is running, with `--follow` to stream events for diagnosis, and `lctk settings show`.

Verified:

- a burst of saves to one path is one pending change, and a repeated save does not lose the newest observation;
- a directory removal is distinguishable from a file removal, and a directory is never reported as written;
- version-control metadata produces no events at all;
- a gap that opens during a reconciliation is not cleared by it, which is the case that would otherwise declare an index current about changes nobody looked at;
- an oversized change set becomes a bulk gap rather than a pending list too large to be worth applying;
- an unreadable journal, or one written for another project, is reset with the reset visible as a gap rather than treated as fatal;
- a refused request wakes nothing, so observation cannot be provoked by an unauthenticated caller;
- the tests run clean under the race detector.

Measured against this repository through a real daemon on Windows 10: 42 watched directories out of 56,077 on disk, a save settled 3.0 seconds after the write against a 3-second window, 62 raw observations of one path collapsed into one pending change, a deletion recorded as such, the pending list surviving a hard kill of the daemon, the reload recording a fresh `observation_started` gap while the sequence counter continued, and a deliberately reduced watch budget producing exactly one capacity gap and ten registered directories.

Two defects surfaced only on that live run, neither reachable from a fixture. A quiet project had no journal on disk at all, because the document was written on settle and a project nobody had touched never settled — so `lctk project watch` reported "never observed" about a project being observed at that moment. And Windows reports a write against the containing directory as well as the file, so every save produced two pending changes, the second for a path the indexer is certain to discard.

The journal has no consumer until Slice 2.2, so every project currently reports an incomplete record. That is the accurate reading rather than a defect: nothing has yet brought an index up to date from the journal.

### Slice 2.2: Incremental exact index

**Status:** complete. This is the slice that makes the journal do something: search follows edits with nothing asked of the user.

The scenario: a developer saves a file and an agent searches for what was just written, without any reindex command and without knowing a watcher exists.

Implemented:

- a settled batch applied to the project index automatically, with the journal deciding the path — a complete record is applied as a delta, an incomplete one is reconciled, and the second is never mistaken for the first;
- a checkpoint that advances only on success, carrying the index generation it produced, so a failed update leaves the work outstanding and retries at the next settle instead of being lost;
- subtree deletion: a removed directory retracts everything beneath it, because once it is gone nothing can enumerate what was in it;
- bulk detection on batch size as well as on delta depth, at a quarter of the index with a floor of 500 files, so a branch checkout is rebuilt rather than applied one tombstone at a time;
- reconciliation after downtime, which is what closes the gap every daemon start records;
- a search that flushes pending changes before answering, bounded so that giving up on the wait degrades to a reported-stale answer rather than a hung call or a cancelled rebuild;
- freshness in the tool response: `exact_search` carries the same verdict `project_info` does, and reports what is outstanding whenever the answer is not current.

Verified:

- an incomplete record is reconciled and never applied as a batch;
- a created directory is not sent to the index, and a removed one is sent as a subtree;
- a failed update does not advance the checkpoint, and the next flush recovers without an operator;
- a twenty-save burst costs the index one change;
- the flush reaches the index without waiting out the debounce window;
- a file rewritten in the same batch that removed its directory survives the expansion of that removal.

Measured against this repository through a real daemon, a real container, and the live MCP endpoint:

| | |
|---|---|
| Daemon start | reconciled to generation 13, 172 files, gap closed, record complete |
| Write then search | found **0.2 s** after the write, against a 3 s debounce window |
| Delete then search | gone, one generation later |
| Directory of two files, then removed | both files left the index |
| File written while the daemon was down | picked up by the reconciliation on the next start and found by search |

Bulk escalation is covered by automated tests rather than measured live: this repository has 173 indexed files, so a batch reaching the 500-file floor would mean rewriting it several times over.

### Slice 2.3: Resource policies and Admin UI baseline

**Status:** complete.

Implemented:

- `quiet`, `normal`, and `fast` background modes that change what a project actually costs — container CPU limit and concurrent index work — set machine-wide with a per-project override in the registry rather than in the repository manifest, because how much of a machine a project may spend is the machine owner's decision;
- memory left uncapped in every mode unless explicitly configured, because a CPU limit throttles an indexer while a memory limit kills it and leaves the index no better off;
- the parallelism cap passed into the container rather than derived inside it, since the container cannot see the host's policy and would otherwise size itself to the whole machine;
- disk reporting: what the index occupies, how much source it describes, and how much room is left, with `start` and `restart` refusing when less than a gigabyte would remain unless `--yes` is given;
- `lctk project resources` and the resource half of `lctk settings show`.

Verified live against this repository, by inspecting the running container rather than by reading the setting back: `quiet` produced a 1-CPU limit and `LCTK_INDEX_PARALLELISM=1`, `fast` produced no CPU limit and no cap, and clearing the override returned it to the machine's 2 CPUs. Measured disk: 9.98 MiB of index for 1.19 MiB of source across 179 files and two retained generations.

The disk estimate was wrong before it was measured. A guessed ratio of 2× would have predicted 2.4 MiB against an actual 10 MiB, because a shard carries fixed metadata that dominates on a small project and two generations are retained. The model now has that shape, and the roadmap records that it is anchored to one repository and is a guess about large ones.

The original browser admin surface, decided in [ADR-0016](adr/0016-admin-surface-and-local-session.md), closed the [security](security.md) open question about how a local admin session is bootstrapped. [ADR-0025](adr/0025-native-windows-admin-and-complete-uninstall.md) later replaced its HTML/browser client and cookie with the native Windows client and process-memory session headers while retaining the independent Admin API boundary. The original Slice 2.3 evidence was:

- an Admin API at `/admin` on the daemon's loopback listener, with two independent credential systems rather than one with a capability flag — no admin handler reads a project grant, and no project route reads an admin session;
- a session established by an exchange code that is spent on first use, delivered by `lctk admin open` in a one-time link and cleared from the address bar by the page, reissued on every daemon start and removed when the daemon stops;
- three independent defences against a browser being turned against the daemon, all required: a `SameSite=Strict` `HttpOnly` cookie, a `Host` header that must name loopback, and a CSRF token on every state-changing request;
- one embedded HTML page, no build step and no remote asset, served with `default-src 'none'`, inserting every API value as text because a project name is a folder name and a folder can be named anything;
- projects with state, index, and change record; start, stop, restart, and reindex; the resource mode; client grants and revocation; the container-runtime diagnostic; and the daemon's recent log, kept in a bounded ring in memory.

Verified live against a real daemon: an unauthenticated request refused with 401, sign-in with the code accepted, **the same code refused on replay**, a project grant refused on the admin surface, a state-changing request refused without the CSRF header and accepted with it, grants listed with no token anywhere in the payload, and lifecycle actions reaching the runtime. The page itself was opened in a real browser against the live daemon and rendered the project, its 187 indexed files, its up-to-date change record, the grant list including a revoked entry, and the daemon log.

## Stage 3 — Safe coding operations

LCTK does not duplicate built-in VS Code or Codex file-editing tools without adding value.

### Slice 3.1: Git awareness

**Status:** complete.

The scenario: an agent talking to LCTK over HTTP has no shell on the machine. It can search the code but cannot see what has changed since the last commit, which is most of what "what is going on in this project" means.

That absence is also what justifies the tools against the rule above. An editor's own terminal can already run `git`; a client that reaches LCTK across a socket cannot, and the answer it gets here is bound to one project by the route.

Implemented:

- `git_status` — branch, commit, upstream position, dirty state, and the changed paths with their state, whether each is staged, in the working tree, or both, and where a rename came from;
- `git_diff` — a unified diff of the working tree or of what is staged, restricted to given paths, bounded in size and honest when truncated;
- `project_info` gains a `source` block with branch, commit, dirty, and how many paths differ, which is the commit-and-branch half of the [freshness contract](indexing.md#freshness-contract) that had never been implemented;
- a short-lived cache, so carrying the source state on more answers does not cost a subprocess each time;
- Git run on the host rather than in the container: the container mounts the source read-only and Git wants to refresh its index, and the host is where the user's Git configuration actually lives.

Verified:

- a folder that is not a repository is an answer rather than a failure, and so is a repository with no commit yet;
- every kind of change is reported with the right state, including a rename with where it came from, and a file both staged and edited afterwards reports both;
- a path Git would normally C-quote parses verbatim, because the machine-readable NUL-separated form is used rather than the space-separated one;
- a project registered **below** a repository root sees only its own changes, not a sibling directory's, and carries the prefix that relates repository-relative paths to it;
- an absolute path, a parent traversal, and a leading dash are all refused rather than reinterpreted;
- a machine without Git says so and names what still works;
- nothing here writes: no commit, no checkout, no fetch, and locking is disabled so asking LCTK for status cannot collide with a Git command in a terminal.

Measured live against this repository through the endpoint: `tools/list` returning all four tools, `project_info` carrying branch and commit, `git_status` reporting six changed paths with a deliberately wrong `project_id` argument and still answering for the routed project, a 58-line diff of one file, a diff bounded to 80 bytes and reported truncated, and three escaping paths refused with `INVALID_PATH`.

The tests drive a real repository they create rather than a fixture of what Git's output was assumed to be. Porcelain v2 has corners — a rename carries its second path in a separate field — and a fixture only proves the parser agrees with whoever wrote it.

### Slice 3.2: Constrained runner

**Status:** complete. The policy is recorded in [ADR-0017](adr/0017-command-policy-and-the-runner.md).

This is the first part of LCTK that executes code rather than reading it, which changes the trust question entirely. Two failures had to be prevented, and they are different: a client must not run something nobody agreed to, and a repository must not change what "the tests" means after somebody agreed to it.

Implemented:

- three parties with three roles — the repository **proposes** `build`, `test`, and `lint` in its manifest, the machine owner **approves** each one, and a client **runs one by name**. `run_command` has no parameter that carries a command line, so the set a client can execute is exactly the set a human read;
- an approval bound to the exact text approved, by digest. A command rewritten in the repository is refused until a person approves it again, and the manifest is read per request so the lapse is immediate rather than at the next restart;
- the runner image approved the same way, because choosing the container is choosing what a command can do. A project without one runs nothing, which is the deliberate default: LCTK cannot know a toolchain, and guessing wrong would build in an environment that silently differs from the developer's;
- one container per run, which is how the guardrails exist at all — process-tree cleanup is removing the container, and the PID, memory, and CPU caps, the single writable mount, the fixed working directory, and the network policy are the runtime's own flags;
- `none` as the network default, with `full` meaning the project's *own* network rather than the default bridge, so a command with egress still cannot reach another project's services;
- a non-zero exit reported as a result rather than an error, with a timeout reported separately, because "the tests failed" and "the tests never finished" call for different things;
- an append-only audit line per run in the LCTK home, recording what was asked for, what ran, in which image, on which network, which client asked, and the outcome — including the runs that were refused;
- `lctk project commands` to see and change what a project may run, and `project_info` advertising only the commands that are actually runnable.

Verified from inside a running container, which is where a guardrail either exists or does not:

| | |
|---|---|
| `pids.max` | 512 |
| `memory.max` | 2 GiB |
| `cpu.max` | 2 cores, matching the project's resource mode |
| `/workspace` | writable, and the only mount |
| `/var/run/docker.sock` | absent |
| network under `none` | no resolution |
| network under `full` | resolution works |

And through the endpoint against this repository: `lint` ran in a real container in 0.6 s; `test` was refused as not approved and reached no runtime; an invented `deploy` was refused as unknown; a `command` argument supplied alongside the name was ignored and the approved text ran; the manifest's `lint` was rewritten and the command was refused as `COMMAND_CHANGED`, then accepted again once the text was put back. All six outcomes, refusals included, are in the audit log.

The repository now carries its own [`.mcp-project.yaml`](../.mcp-project.yaml), so the mechanism can be exercised against a real project rather than a fixture.

Not done here: project-relative file-read helpers. The rule is that LCTK adds them only where they complement what a client already does, and no case has been identified that `exact_search` and `git_diff` do not already cover. Adding them speculatively would be exactly the duplication this stage forbids.

### Slice 3.3: A write that changed nothing costs nothing

**Status:** complete. The decision is recorded in [ADR-0018](adr/0018-the-index-describes-the-disk.md).

Most coding clients hold a diff before it becomes a file: some show a proposal and write it only once a human accepts, others write immediately and offer a revert. The second shape turns out to cost something, and the first raises a question about what a search result is a claim about.

Implemented:

- a written path is compared against the digest recorded for it in the published index, and dropped when they match. No document is retracted, none is added, and **no generation is published at all** — the generation number, the delta depth, and the build timestamp stay where they were. Delta depth is the budget that forces the next full rebuild, so a no-op delta is not free work: it moves a rebuild closer;
- filtering per path within a batch, so one real edit among eight submitted paths is a one-file delta rather than a bulk rebuild;
- escalation to a rebuild judged on what actually changed rather than on what was submitted, so a branch checked out and immediately checked back is not a bulk change twice over;
- an `Applied` report — paths changed, writes that changed nothing, whether it escalated, generations consumed — surfaced on `/index` and logged by the daemon even when zero, because a filter that works silently cannot be told from a filter that is not running;
- the scope of a search result stated where an agent reads it: an unsaved buffer and an unapplied patch are not searchable, and no channel is added for importing one. Letting a client declare what a file contains would put text in the index that never existed on disk, where a *second* client would find it by search and have no way to tell it from real code.

An edit already written to disk is treated as the project's content whoever wrote it, because the filesystem does not record who wrote a file or why. Any distinction would be a guess, and a wrong guess either hides real code from a search or labels a developer's work as a machine's.

Measured live against this repository, watching the published generation and the delta depth:

| What happened | Generation | Delta depth | Reported |
|---|---|---|---|
| A new file appears | 51 → 52 | 0 → 1 | `applied=1` |
| The same bytes written again | 52 | 1 | `applied=0 unchanged=1` |
| Edited and undone before the index caught up | 52 | 1 | `applied=0 unchanged=1` |
| Edited, and the index caught up | 52 → 53 | 1 → 2 | `applied=1` |
| And only then undone | 53 → 54 | 2 → 3 | `applied=1` |
| Thirty indexed files rewritten byte for byte | 54 | 3 | `applied=0 unchanged=30` |
| The file deleted | 54 → 55 | 3 → 4 | `applied=1` |

The fifth row is the honest one and is meant to cost a generation: the index genuinely held the other content in between, and searches were answered from it. The sixth row previously consumed a generation and a delta step for no change at all.

Nothing is lost to the filter, which was checked rather than assumed: after the thirty-file batch, content from a rewritten file was still found by search, and after the revert the restored text matched while the reverted text did not.

A unit test covers what the live project cannot show: this repository has 206 files, so the 500-file bulk floor means a batch here never escalates on count alone. The test drives a twenty-file index with a floor of four, where eight submitted paths with one real edit among them was previously a full rebuild.

Not done here: an agent still cannot ask "what changed since I last looked". The journal is a work queue and forgets an entry once it has been applied, so answering that needs retention it does not have — a decision of its own rather than an extension of this one.

### Slice 3.4: Recovery after a project moves

**Status:** complete.

Both defects here were found by running a slice against this repository rather than by testing, which is the habit paying for itself twice in one afternoon.

**A project restarted while the daemon runs came back on a new service address, and the index never caught up again.** The worker captured the address when it was created and nothing revisited it, so every drain posted to an endpoint that no longer answered — for as long as the daemon lived.

What it got right is why the defect was findable at all: the failure was loud. The checkpoint refused to advance, the pending list grew, and the reason appeared in `lctk project watch` as well as the log. Nothing claimed to be fresh. What was missing was recovery.

The address is now re-read on the sweep and on any client request, because a client using a project is earlier evidence than the next sweep that it came back somewhere else. The watcher and the journal are kept: the host went on observing throughout, so nothing is a gap and what was pending is simply applicable again. Discarding the journal would have turned a recoverable lag into a full reconciliation.

Measured live, reproducing the original failure:

```
21:10:44  watching project                      port 51794
21:11:12  index could not be brought up to date  dial tcp 51794: refused
21:11:31  the project service moved              port 52984
21:11:31  index brought up to date               generation 70, applied 1, reconciled=false
```

87 ms from noticing to caught up, and `reconciled=false` is the part that matters: the journal survived the move, so it cost one delta rather than a walk of the whole project.

That run recovered through a client request. The sweep is the other path and was measured separately, restarting the project and then making **no client call at all**: the file written after the move was searchable 16 seconds later, on the new port. Both regression tests were confirmed to fail with the fix disabled before being kept.

**`lctk project status` gave two different answers about the manifest.** Asked about one project it re-read the file; asked about every project it reported what the registry recorded at registration, so a manifest added later was reported missing. `lctk project restart` printed the stale answer too.

The read moved into the shared view builder, which removes the duplicate rather than adding a second one, and is skipped when the project's path is unavailable so a listing does not wait on a disconnected drive. The existing test covered only the single-project form, which is exactly why the defect survived; it now asserts the two forms agree.

### Slice 3.5: Every tool called through a second client

**Status:** complete.

Slices 3.1 and 3.2 were measured by hand-driven JSON-RPC against the endpoint. That proves the wire protocol and nothing about whether a client can *discover* and *call* the tools, which is a different question: each carries an input schema, and a schema is where a client and a server disagree. [Slice 1.4](#slice-14-actual-codex-end-to-end) set the standard — the protocol boundary is verified against two clients — and Stage 3 had not met it.

Auditing that turned up a wider hole. The Slice 1.4 harness called `project_info` and left every other tool merely *listed*, and appearing in `tools/list` proves only that the server described a tool. **`exact_search` — the oldest tool here and the one an agent reaches for most — had never been called through a second client at all.**

So the rule is now the stronger one: every tool the endpoint offers is called, not listed. The [Slice 1.4 harness](../spikes/codex-end-to-end/) was extended rather than a second one written, so one command covers the whole client-facing surface and the harness grows as the surface does.

It prepares its first project as a real repository — one commit behind it, an uncommitted edit in front of it, and a manifest proposing `lint` and `test` of which only `lint` is approved. The runner image is the one this repository builds, so no external image is assumed and the command runs in a real container. Driven through `codex-cli 0.146.0-alpha.9.2`, the build [ADR-0012](adr/0012-codex-integration-contract.md) is bound to, with an isolated `CODEX_HOME` and `LCTK_HOME` so the operator's own configuration, registry, and grants are neither read nor written.

All 25 steps pass — the 15 from Slice 1.4 unchanged, plus:

| Step | Result |
|---|---|
| tools discovered | all five: `project_info`, `exact_search`, `git_status`, `git_diff`, `run_command` |
| `exact_search` | found a line **saved and never committed** — the claim the indexing design exists to make, checked from outside LCTK |
| `exact_search` with a broken expression | `INVALID_PATTERN`, not an empty result set that would read as "no such code here" |
| `exact_search` with an invented argument | refused by schema validation before any handler saw it |
| `git_status` | the uncommitted change, with branch and commit, `root: /workspace`, no host path |
| `git_diff` for one path | a real unified patch |
| `git_diff` for `../outside.txt` | `INVALID_PATH: the path must stay inside the repository`, marked `isError` |
| `run_command lint` | ran in a container, output returned |
| `run_command test` | `COMMAND_NOT_APPROVED`, naming the command that fixes it |
| `run_command build` | `COMMAND_NOT_PROPOSED`, naming the manifest key to add |
| `run_command deploy` | `COMMAND_UNKNOWN: LCTK runs only build, test, and lint` |

The refusals are the point. Each calls for something different from whoever reads it, and all of them survive into the client's own tool result where an agent reads them and acts. A single generic failure would have been protocol-correct and useless.

The invented-argument step completes the scope guarantee from the other side. A *declared* argument such as `project_id` is accepted and disregarded, which `scope_survives_a_wrong_argument` already checked; an *undeclared* one fails validation outright. Silently dropping it would leave an agent believing a filter had been applied.

Writing the harness caught two wrong assumptions, both mine rather than the product's: it expected `COMMAND_NOT_APPROVED` for a command the manifest had never proposed and got the more precise `COMMAND_NOT_PROPOSED`, and it guessed a `regex` boolean where the schema has `mode`. Both are the useful direction for a disagreement to resolve in.

`run_command` also confirmed the boundary it exists to hold: the manifest's `test` entry — present, proposed, deliberately unapproved — never reached a container.

### Slice 3.6: The refusal text an agent acts on

**Status:** complete.

A significant change now ends with the whole MCP surface driven by hand against the real daemon, with every answer read rather than asserted on. Passing tests and a green CI are necessary and not sufficient: an assertion checks the substring somebody thought to write down.

The first such pass covered every tool and every variant of each — literal and regex search, case sensitivity, path globs and an escaping glob, paging and a stale cursor, staged versus working-tree Git state, a rename, a file both staged and edited after, diff truncation, each distinct refusal code, a command line smuggled beside an approved name, a rewritten manifest, a non-zero exit, a missing credential, a malformed one, the wrong scheme, and an unknown project.

Almost everything held, including things that had never been checked live: a file written 0.3 seconds earlier was found inside the 3-second debounce window; the staged and working-tree diffs of one path genuinely differ; `commands` lists only what is runnable; a non-zero exit comes back as a result rather than an error; and the three credential answers match [`docs/security.md`](security.md) exactly, including the deliberate `403` for an unknown project that refuses to disclose whether it exists.

Two defects surfaced, both in the text an agent reads and neither reachable by assertion:

- **A refusal ran two sentences together.** The parts are joined with a space, and many messages end on a quoted path or a backticked pattern, so the advice read as part of the value: ``missing closing ]: `[unclosed` Correct the request and try again.`` A message that does not punctuate itself is now closed first, while a colon or semicolon is left alone rather than given a second mark.
- **A stale cursor was told to "correct the request".** Nothing about the request was wrong when it was made — the index moved underneath it — and the message already carried advice that the generic action then duplicated. The message now states what happened and the action says what to do, with the reason attached.

The recommended action became a named function so it can be read back and checked, since that sentence is part of the tool's interface rather than a detail of its implementation.

Six variants earned their way into the [harness](../spikes/codex-end-to-end/), which now runs 29 steps: the staged-versus-working-tree difference, the smuggled command line, the rewritten manifest, and the failing command. Paging with a stale cursor and the credential failure modes stay hand-only for now and are named as such in the [results](spikes/codex-end-to-end-results.md), because a variant nobody lists is a variant nobody runs.

## Stage 4 — Symbol and AST intelligence

Add language adapters in small, independent slices without fixing their order in advance:

- Go;
- TypeScript/JavaScript;
- Python;
- Rust;
- C/C++.

Go was added to the list because it is what LCTK is written in, which makes it the one language whose real corpus is this repository itself. Every other language is verified against source from that language's own project; Go is verified against the code under the verifier's hands.

For each language, separately verify symbols, definitions, references, diagnostics, lifecycle, and resource behavior. The Tree-sitter/AST layer provides file outlines and chunks independently of a specific LSP.

### Slice 4.1: Choosing the symbol engine

**Status:** complete. The decision is recorded in [ADR-0019](adr/0019-tree-sitter-symbol-layer.md), measured against the [evaluation contract](spikes/symbol-backend-evaluation.md) with the results in [`symbol-backend-evaluation-results.md`](spikes/symbol-backend-evaluation-results.md).

Tree-sitter and Universal Ctags were run over the same 933 files of real source — `requests`, `ripgrep`, `zlib`, `nlohmann/json`, `axios`, and this repository — with the extraction queries a production layer would carry rather than simplified stand-ins.

A language server was excluded before measurement, and the reason is a constraint rather than a preference: `tsserver`, `pyright`, `rust-analyzer`, `clangd`, and `gopls` reach their stated precision only with the project's dependencies resolved, and LCTK mounts the source read-only and installs nothing. A server here would answer at a precision it cannot reach while costing a per-language toolchain in a shared image and a per-language process to supervise.

Tree-sitter is selected. Universal Ctags fails two gates that are not a matter of configuration:

| | Tree-sitter | Universal Ctags |
|---|---|---|
| Symbols bounded in bytes | 16,466 of 16,466 | **0 of 27,064** |
| Truncated file reported as broken | 7 of 7 | **0 of 7** |
| End line for JavaScript, TypeScript, Rust | yes | none at all |
| `axios/index.d.ts` declarations | 137 | 24 |
| 933 files, 10 MiB | 9.9 s, 68 MiB peak | 5.0 s, 12 MiB peak |

The last row is ctags' advantage and is recorded rather than dismissed, along with 148 languages out of the box against seven grammars. It does not outweigh a chunk model with no boundaries and a diagnostic that cannot fire.

Three findings came from running against real code rather than a fixture.

**The syntax diagnostic is only trustworthy per language.** The C and C++ grammars have no preprocessor, so 58 of 69 real C files and 134 of 438 real C++ files parse with errors while compiling perfectly. Across the other 426 files — Go, Python, Rust, JavaScript, TypeScript — zero were reported as broken. A verdict is therefore published for those five and withheld for C and C++, where it would otherwise report a defect in most of a project on the first look. A hand-written C fixture parses, so nothing but real code would have shown this.

**Universal Ctags' interactive protocol stalls on ordinary JavaScript.** Driving one warm process — how Zoekt uses it, and the configuration a service would want — stopped replying on a 3.5 KB file from `axios`, after 18 tags. The same file through the command line takes 40 ms and produces 24 tags plus `ignoring null tag`, which is where the framing appears to be lost. Reproduced with a minimal independent driver against versions 5.9.0 and 6.1.0. The protocol offers no way to abandon one file, so recovering costs the warm process that was the reason to use the mode.

**The measurement produced a list of gaps in LCTK's own queries**, which is what it was for: no preprocessor definitions for C and C++, no module-level assignment for Python, no module-level non-function bindings for JavaScript. `requests/docs/conf.py` is 34 names to ctags and zero to Tree-sitter for exactly that reason. They are now written down instead of being discovered later as missing answers.

A fourth finding is about the harness rather than the product: enumerating the corpus through a Windows bind mount took longer than analysing it, so the first attempt measured the filesystem. The corpus lives in a Docker volume.

Not verified here: the grammars **cross-compile** for `linux/arm64` in 27 seconds, but nothing shows the arm64 result running. An emulated build exceeded ten minutes and was abandoned, so that gate is a partial pass and the execution belongs to the release pipeline, next to the same open item [ADR-0011](adr/0011-zoekt-exact-search-backend.md) already carries.

### Slice 4.2: What this file declares, and whether it parses

**Status:** complete. The contract is described in [symbols](symbols.md).

The scope: an agent that has just written a file wants two things no tool here could answer. What does this file declare — so it can find the method it is about to change without reading 800 lines. And does it parse — so it can tell a typo from a logic error before running anything.

Implemented:

- a symbol layer in the project's own container, parsing with Tree-sitter and extracting declarations with LCTK-owned queries per language, starting with Go;
- `file_outline`, reporting each declaration's name, kind, line range, **byte range**, enclosing declaration, nesting depth, and its own opening line;
- containment computed from the extents rather than from a name path, so a constant declared inside a method reports that method — which nothing in the constant's own syntax says;
- a syntax verdict: whether the file parses, how many broken regions, and the first line to look at, with the declarations that *did* parse still reported, because a file midway through an edit is the ordinary case;
- the verdict published **per language**, with `reported: false` meaning unknown rather than fine, for the reason Slice 4.1 measured;
- `"precision": "syntax"` on every answer, stated in the answer rather than only in documentation, because a caller that reads a syntax-only answer as type-resolved will act on it;
- reading decided by the store, using the rules that already decide what is indexed: an escaping path is `INVALID_PATH`, and a symbolic link, version-control metadata, or a file the project's ignore rules exclude is `FILE_NOT_FOUND` — **the same answer as a file that is not there**, so two refusals cannot be differenced to map what is outside the caller's scope;
- an unsupported language refused with `LANGUAGE_UNSUPPORTED` naming what does work, never an empty outline, which would read as "this file declares nothing";
- a bounded parse, and a file abandoned to the budget reported as `PARSE_INCOMPLETE` rather than as having no declarations;
- the tool advertised from what the project's service reports rather than from what the host build expects, so a container predating the symbol layer does not offer a tool it cannot serve.

An outline reads the file rather than the index. There is nothing to flush and no generation it could be behind, so the answer carries a **content digest** instead of an index generation, and freshness is not reported because the concept does not apply.

Measured live against this repository through the endpoint:

| | |
|---|---|
| `internal/gateway/git.go`, 239 lines | 46 declarations with kinds, extents, and depth |
| `spikes/codex-end-to-end/chain.go`, 36,218 bytes | 78 declarations in **87 ms**, including the HTTP round trip |
| A file written milliseconds earlier | outlined through MCP in **196 ms**, `valid: false`, `first_error_line: 6` — the missing brace |
| Image | 55.7 MB → **56.9 MB**; the service binary 32,194,744 → 33,056,616 bytes for one grammar |

All five refusals were driven by hand at the service: an unsupported extension, an escaping path, a gitignored file, an absent file, and a file that does not parse. The gitignored file and the absent file answered identically, which is the point of that pair.

Four steps joined the [harness](../spikes/codex-end-to-end/), which now runs **33** through the real Codex client: the outline itself with containment checked, the truncated file, the unsupported language, and the escaping path. `file_outline` is called rather than listed, per [Slice 3.5](#slice-35-every-tool-called-through-a-second-client).

Adopting cgo changed the build. The image's build stage is now Alpine and links statically against musl, so the runtime stage is still one binary with no toolchain in it — which is what the earlier "no C toolchain" position was protecting. CI now builds with cgo enabled, cross-compiles for arm64 with a real cross compiler, and runs the symbol tests under the race detector, because the parser pool is load-bearing rather than an optimization and nothing else would check it.

That arm64 step failed in CI and passed locally, which is the discrepancy worth naming: locally the cross compiler was installed with recommends, and `--no-install-recommends` leaves out `libc6-dev-arm64-cross`. Without the target's headers the compiler reaches for the host's. Reproduced with the flag and fixed by naming the package.

### Slice 4.3: Where a name is declared, and where it is used

**Status:** complete. The contract is described in [symbols](symbols.md#finding-a-name-across-the-project).

The scenario: an agent about to change a function needs to know what will break. `exact_search` finds the letters, which includes every mention in a comment, a string, and a changelog. What it wants is the identifier.

Implemented:

- `find_definition` and `find_references`, each location saying whether it declares the name, its kind, the declaration that encloses it, and the source line;
- work split so each half does what only it can: **the index narrows** to files holding the name as a whole word, so `Read` does not bring in `ReadAll`, and **the parser decides**, because only a syntax tree knows whether those letters are an identifier or prose;
- `"precision": "name_match"` on a cross-file answer, deliberately a weaker word than the `syntax` an outline reports: within one file the syntax settles what is a declaration, and across files nothing here resolves which declaration a use refers to;
- the index generation and the host's freshness verdict carried, and a search-style flush run first, because unlike an outline this answer **is** index-dependent — a file the index has not caught up with is a file the lookup never opens;
- bounds that are reported rather than implied: at most 200 files examined, `truncated` when the list is cut, and separate counts for candidates skipped as an unsupported language or as unreadable. A caller reading a bounded answer as complete concludes "nothing else refers to this", which is the one wrong conclusion available here;
- a name refused rather than escaped when it is not a plausible identifier, because the name reaches a regular expression and escaping `Widget|Consume` would quietly answer a different question;
- Go's `:=` treated as a declaration. It was not at first, and a lookup that called it a use would have answered "nothing declares this" about most locals in the language.

Measured live against this repository, on the word `terminated`:

| | |
|---|---|
| `git grep -w terminated` | 5 files — three Markdown, two Go |
| Files the index offered | 5 |
| Files with a real occurrence | **1** |
| Skipped as an unsupported language | 3 |
| `internal/hostsettings/settings.go` | excluded — its only hit is the English word in a comment |
| `internal/gateway/gateway.go` | 3 of 4 lines: the two calls and the declaration, **not** the doc comment on line 776 |

The same property on a second name: `MaxDeltaGenerations` reports one declaration and fifteen uses, and not the doc comment sitting directly above the field it documents.

Cost, measured: `err` bounded to 25 files reports 1008 occurrences, 324 of them declarations, in **0.75 s**, with `truncated: true`.

Three steps joined the [harness](../spikes/codex-end-to-end/), which now runs **36** through the real Codex client, with all eight tools discovered. The fixture gained a file that mentions the name only in a comment and a string, so the property is checked from outside rather than asserted inside.

Containment turned out to be tighter than expected and is documented rather than smoothed over: a use inside `other := Needle{…}` is inside the declaration of `other`, which is inside the function. Both are true and only one can be the innermost.

Not done here: parameters, receivers, and `range` bindings are not reported as declarations. Including them would put every parameter of every function into an outline, which is noise in the answer a reader asked for when the signature already shows them.

### Slice 4.4: The other four language families

**Status:** complete. The per-language boundaries are described in [symbols](symbols.md#languages).

Python, Rust, C, C++, JavaScript, TypeScript, and TSX join Go. All three tools serve all eight without a second code path: one engine, one vocabulary, one set of failure modes, and a query per language.

The declaration set is per language and follows that language's own idea of one rather than a lowest common denominator. Four entries exist because [Slice 4.1](#slice-41-choosing-the-symbol-engine) measured their absence: preprocessor macros for C and C++, module-level and class-level assignment for Python, `let` bindings and trait method signatures for Rust, and a plain `const` binding for JavaScript. `requests/docs/conf.py` was thirty-four names to a tag generator and zero to a query that knew only functions and classes.

Measured with the production engine and the production queries over the same 967 real files from `requests`, `ripgrep`, `zlib`, `nlohmann/json`, `axios`, and this repository:

| Language | Files | KiB | Declarations | Nested | Unparsed | ms |
|---|---|---|---|---|---|---|
| c | 87 | 2059 | 2897 | 762 | — | 1042 |
| cpp | 443 | 4801 | 7703 | 3085 | — | 2083 |
| go | 127 | 1022 | 8120 | 6404 | 0 | 381 |
| javascript | 163 | 982 | 4978 | 2736 | 0 | 361 |
| python | 42 | 406 | 940 | 602 | 0 | 124 |
| rust | 98 | 1654 | 7249 | 5942 | 0 | 555 |
| typescript | 7 | 65 | 641 | 483 | 0 | 27 |

**32,528 declarations across 10,992 KiB in 4.57 s, peak RSS 69 MiB.** No file hit the parse budget and none failed. Against the same corpus, Slice 4.1's simplified queries found 16,466 in 9.9 s: the gaps that measurement named account for most of the difference.

Zero unparsed files across Go, Python, Rust, JavaScript, and TypeScript on real code, which is why the verdict is published for those five and withheld for C and C++.

Verified live through the endpoint, one file per language:

```
w.py     python       3 decls  valid=True   Widget:class size:field describe:function
w.rs     rust         4 decls  valid=True   Widget:struct size:field Widget:implementation size_of:function
w.c      c            4 decls  no verdict   WIDGET_MAX:macro Widget:struct size:field widget_size:function
w.hpp    cpp          4 decls  no verdict   shapes:module Widget:class Size:method size_:field
w.js     javascript   3 decls  valid=True   Widget:class describe:method build:function
w.ts     typescript   4 decls  valid=True   Widget:interface size:field Alias:type build:function
w.tsx    tsx          1 decls  valid=True   View:function
```

And `find_definition` for `Widget` across all of them at once: **six declarations, one per language, each with its own kind**, with three candidates counted as an unsupported language rather than dropped.

**One defect came out of that live run and nothing else would have found it.** The C outline reported `Widget` twice. In C, `struct Widget *w` in a parameter list is syntactically a struct declaration carrying the name, so the query was reporting a *use* as a declaration — and `find_definition` in C would have said a struct is declared once per function that takes one. A struct, union, enum, or class is now matched only with its body. A forward declaration is no longer reported, which is an omission where the other was a false claim. The regression test was confirmed to fail without the fix.

Two mechanisms were needed and are worth naming, because both are about not lying:

- **A scope that declares nothing.** A Rust `impl Config` block must give the methods inside it `Config` as their container, and must not be a second declaration of `Config`. It is captured as a scope rather than as a declaration.
- **A pattern that decides a kind.** In JavaScript a `const` bound to an arrow function and one bound to a number are the same grammar node. Two patterns match, so the first one listed wins, and the specific patterns are written before the general ones. Without that rule the same declaration appeared twice with two kinds.

Cost of the seven extra grammars: the image from 56.9 MB to **66.7 MB**, the service binary from 33,056,616 to **41,987,368 bytes**, and a cold image build of about **67 s**. Generated C dominates the build, which is why the build caches are not optional.

### Slice 4.5: Lifecycle, resources, and what there is to invalidate

**Status:** complete. The bounds and the lifecycle are described in [symbols](symbols.md#bounds).

The slice began as verification of what Stage 4 requires per language — lifecycle and resource behaviour — and turned up a defect in the process.

**A wall-clock budget is not load-independent.** Measured on a 920 KiB C++ header in a container limited to two CPUs and 2 GiB:

| Concurrent parses | Peak memory | Answered | Refused as `PARSE_INCOMPLETE` |
|---|---|---|---|
| 16, unbounded | 269 MiB | 16 | 0 |
| 32, unbounded | 522 MiB | 32 | 0 |
| 64, unbounded | **675 MiB** | **2** | **62** |
| 16, bound 2 | 96 MiB | 16 | 0 |
| 32, bound 2 | 97 MiB | 32 | 0 |
| 64, bound 2 | **97 MiB** | **64** | **0** |

Unbounded, memory grows with whatever arrives together, and past the CPU allowance a file that parses in a third of a second starts exceeding its five-second budget. The service was refusing perfectly ordinary files because it was busy — a correctness failure wearing a resource failure's clothes, and one that only shows up under concurrency.

Implemented:

- a bound on how many files are parsed at once, taken from the project's **resource mode** rather than invented here, because it answers the same question the index's own cap answers — how much of this machine the project may spend;
- a slot released when the caller gives up, so an abandoned request holds nothing;
- `PARSE_BUSY`, **retryable**, distinct from `PARSE_INCOMPLETE`: the file is fine and the answer exists, the project was busy. Reporting the second would be a claim about the file;
- the bound reported on the service's status, so the policy in force is visible rather than inferred from a refusal.

Verified live, by inspecting the running container rather than reading the setting back:

| Mode | Container CPUs | Files parsed at once |
|---|---|---|
| `quiet` | 1 | 1 |
| `normal` | 2 | 2 |
| `fast` | no limit | 12, the machine's processors |

Bounded, memory is flat and every request is answered. At 32 concurrent the bound costs **nothing** in wall clock — 5960 ms against 6061 ms — and at 64 it costs latency to return 64 answers instead of 2.

**Lifecycle and invalidation: there is nothing to invalidate, and that is the answer.** The layer persists no symbol table, no cache, and no index of its own, so nothing goes stale, nothing is rebuilt after a restart, and no per-language process exists to supervise. Measured live: an outline answered **205 ms after the project's listener first answered**, with 207 declarations, *while the search index was still building*.

A cache was considered and deliberately not added. A Go file here parses in about 3 ms, and a cache would trade a stale answer about a file being edited — exactly the case these tools exist for — for nothing worth having.

This closes Stage 4. Symbols, definitions, references, syntax diagnostics, lifecycle, and resource behaviour are verified for Go, Python, Rust, C, C++, JavaScript, TypeScript, and TSX.

## Stage 5 — Persistent semantic intelligence

### Slice 5.1: Semantic contract and measured backend choice

**Status:** complete. [ADR-0020](adr/0020-shared-embedding-and-project-semantic-store.md) fixes the shared inference, model, chunk, storage, ranking, and freshness boundaries. The selected image is pinned by a multi-architecture OCI digest; the model is pinned by repository commit, byte length, SHA-256, and Apache-2.0 attribution. The rejected `sqlite-vec` bindings were compile-tested on the shipped toolchain rather than accepted from documentation.

Acceptance requires an immutable model and runtime identity, license attribution, a reproducible quality corpus with lexical and semantic counterexamples, and measured CPU, memory, disk, cold-start, and query costs.

### Slice 5.2: Persistent AST-aware chunks

**Status:** complete. Supported source uses grouped Tree-sitter declaration extents with line-bounded splitting; other text uses bounded overlapping chunks. Structural identity and content digest are separate, unchanged chunks reuse embeddings, and add/change/delete publication is one SQLite transaction.

Acceptance requires restart persistence, rename and deletion retraction, no work for unchanged content, explicit chunk precision, and project-scope refusal tests.

### Slice 5.3: Shared local embedding and vector indexing

**Status:** complete. One loopback-only llama.cpp server is shared by project containers through Docker's explicit host gateway. `lctk bootstrap` verifies and installs the pinned image and model, performs a real 768-dimensional embedding self-test, and revision-labels runtime arguments so configuration changes replace old containers. Failed batches leave the preceding semantic generation intact.

Acceptance requires two projects sharing one inference process while retaining separate stores, offline query after installation, typed unavailable/busy/model-mismatch failures, and a failed batch preserving the previous generation.

### Slice 5.4: Hybrid semantic search through MCP

**Status:** complete. `code_search_semantic` performs deterministic reciprocal-rank fusion over exact cosine and lexical rankings, reports both ranks and scores, model/dimension identity, semantic and exact generations, disk freshness, structural precision, bounds, and typed remediation.

Acceptance requires real semantic wins over exact search, exact identifiers surviving hybrid ranking, saved edits visible before the answer, and calls through every supported MCP client path.

### Slice 5.5: Semantic lifecycle and resource proof

**Status:** complete. A real three-file project published exact and semantic generation 1 with three persistent chunks, then answered through an independent MCP Go SDK client. The conceptual retry/backoff query ranked the syntax chunk first (`vector_score` 0.7003, lexical rank 1) while exact identifiers remained present. The shared process measured approximately 515-769 MiB during active CPU inference; the project service measured approximately 56 MiB. A full-repository dry run exposed and fixed physical-batch, per-slot context, batching, and progress-reporting defects without ever publishing a partial generation. Stage 7.5 subsequently closed the upper stress gate through one million semantic chunks and one million exact-search files.

The live external client completed MCP handshake, tool listing, `project_info`, `exact_search`, and `code_search_semantic` through the authenticated project route. It observed `scope_source: route_and_registry`, `/workspace` paths, exact generation 1, semantic generation 1, and `freshness: fresh`.

## Stage 6 — Graph, repository map, and memory

### Slice 6.1: Persistent derived graph

**Status:** complete. Tree-sitter extracts declaration nodes, language import syntax, called identifiers, and enclosing declarations for Go, Python, Rust, C, C++, JavaScript, TypeScript, and TSX. Graph rows are replaced per changed path and committed in the same SQLite transaction and exact generation as semantic state. Schema v1 migrates to v2 without discarding embeddings; the first graph sync fills the new tables.

### Slice 6.2: Graph tools

**Status:** complete. `callers_find`, `callees_find`, `dependency_path`, and `impact_analyze` expose bounded evidence through the project service and MCP. Calls retain path, caller, callee, line, and column; caller/callee pages use opaque cursors; dependency traversal is deterministic and depth-bounded. Every response states `precision: name_match`, ambiguity, graph/exact generations, and watcher freshness.

### Slice 6.3: Compact repository map

**Status:** complete. `repository_map` ranks files deterministically from incoming/outgoing imports and call activity, emits syntax declarations and signatures, enforces a 256-100000 character budget, and reports character count, whole-project counts, truncation, generation, and name-match precision.

### Slice 6.4: Explicit reviewed project memory

**Status:** complete. `memory_get`, `memory_search`, `memory_put`, and `memory_delete` provide explicit records with validated stable keys and kinds, project-relative provenance, confidence and review labels, source commit, timestamps, and optimistic revisions. Writes embed before their transaction; failed inference cannot create metadata-only state. Search is deterministic lexical/semantic RRF, while an empty query is the inference-free list operation. The gateway supplies the current Git commit and labels stale records without hiding them.

### Slice 6.5: Graph and memory end-to-end proof

**Status:** complete. Container tests cover add/change/delete retraction, duplicate-name ambiguity, pagination, dependency traversal, v1-to-v2 migration, stale revisions, escaping provenance, semantic/lexical memory search, and restart persistence. Gateway tests drive all nine Stage 6 tools through a real MCP SDK session and verify route-bound scope despite a foreign `project_id` argument.

The live five-file fixture migrated an existing Stage 5 database in place: exact, semantic, and graph generation 3 were simultaneously `fresh`, with 13 declaration nodes, three import facts, and nine calls. A watcher-applied JavaScript edit produced `main.js -> dep.js` without manual reindexing; `callers_find` connected JavaScript caller `start` to Go declaration name `RetryFailedRequest`, explicitly as `name_match`. An independent MCP Go SDK client listed 18 tools and called the complete catalog: every read/write path succeeded, while a stale memory revision and an unapproved command produced their expected typed refusals. A separate put/restart/get/delete sequence recovered the same memory content and revision after the project container received a new loopback port.

## Stage 7 — Hardening and public release

### Slice 7.1: Transactional bootstrap

Implement the plan-first `lctk bootstrap` contract from [ADR-0022](adr/0022-transactional-bootstrap-update-and-release-evidence.md), immutable downloads, complete cancellation before download, disk preflight, installation, and functional verification.

**Status:** complete. Bootstrap resolves the signed release envelope when an official component is absent, reports the complete core/image/model download and disk plan before writing, and checks cancellation before every external operation. Apply verifies immutable identities, installs the code image, inference image and model, performs a real embedding self-test, copies the packaged core into the versioned store, and creates the first atomic activation document. A locally packaged Windows launcher/core pair completed this path against an isolated home and then launched through the activated digest.

### Slice 7.2: Update, migration, and rollback

Implement signed release manifests, read-only update plans, schema compatibility, rollback bundles, copy-validate-swap migrations, atomic activation, and automatic restoration after a failed health gate.

**Status:** complete. Ed25519 verification authenticates the exact payload bytes before any URL is trusted. Update refuses non-newer and incompatible releases, pulls the immutable candidate image, migrates each running project on a durable SQLite copy, restores migrated projects in reverse order after a failed health gate, and activates the verified host core last. Explicit rollback restores every available registered-project bundle, restarts only projects that were running, verifies the previous executable, and changes host activation last. Tests cover read-only plans, low disk before network, corrupt bytes, candidate health failure, stopped projects, activation tampering, and two-version rollback.

### Slice 7.3: Release supply-chain evidence

Generate dependency attribution, SPDX SBOMs, checksums, artifact attestations, image signatures, release notes, and migration metadata. Official publication fails closed without the Ed25519 manifest key. ADR-0023 deliberately makes the initial open-source release Windows-only and its executables unsigned, so no platform certificate is required.

**Status:** complete as a fail-closed Windows-only publication implementation; no official release has been claimed. The protected tag workflow requires only the Ed25519 manifest key; runs Windows host tests; publishes explicitly unsigned Windows executables; executes the packaged launcher; builds and executes each Linux image architecture on its native GitHub runner; joins them by digest; and publishes SBOMs, checksums, module attribution, signatures, attestations, migration notes, and the signed manifest only after every gate succeeds. macOS remains in ordinary CI and dry-run artifacts, not the official release graph.

### Slice 7.4: Packaged and client-path verification

Extract and execute both target archives, build and inspect both image architectures, run Docker lifecycle and every MCP tool through Codex plus independent protocol clients, and preserve machine-readable evidence.

**Status:** complete for the Stage 7 evidence boundary, not target-OS certification. The bound Windows launcher/core package bootstrapped and executed from an isolated installation. Codex CLI `0.146.0-alpha.9.2` and an independent Go MCP SDK v1.7.0 client each called the complete 18-tool catalog against the live persistent stack, including typed refusals, route-bound foreign IDs, memory writes, and restart recovery; the SDK pass was repeated against final local image `sha256:4785290b20b5d707d8515e13c5bb800da8461cfd83802b49dc7469da80743d9e`. Hosted run `31012555479` built, extracted, executed, and preserved machine-readable evidence for Windows amd64 and macOS arm64 packages plus healthy native Linux amd64 and arm64 images at commit `7679060`. Earlier native image runs exposed an exact-only typed-nil semantic panic and migration crash windows; regressions, Docker test/race passes, local runtime smoke, and the final rerun close those defects. See [Stage 7 client verification](stage7-client-verification.md) and [releasing](releasing.md).

### Slice 7.5: Compatibility and stress evidence

Provide a parameterized suite through one million files, record resource and latency curves, verify low-disk and corruption refusals, update the honest certification matrix, and document measured Linux portability gaps without promoting Linux to a first-release target.

**Status:** complete for the documented synthetic evidence boundary. Production-schema semantic publication and bounded exact-vector ranking passed at 1,000, 10,000, 100,000, and 1,000,000 chunks. Production exact inventory and Zoekt publication passed the same curve using ordinary bind-mounted files with project state in a Docker volume. Every point published the requested count; the exact target returned one untruncated match, and semantic returned the exact total while retaining only bounded top-K results. Low-disk, corrupt artifact/database, incompatible schema/model, candidate health, and rollback failures are fail-closed in automated tests. The measured Linux portability limits and remaining target certification gaps are recorded in [compatibility](compatibility.md) and [stress evidence](stress.md).

### Stage 8: Windows one-click product path

**Status:** implementation complete; clean-host certification is not yet claimed. ADR-0023 replaces the Windows end-user Docker/Compose dependency with a private Podman `5.8.2` client and explicitly named WSL2 machine. ADR-0024 replaces the temporary browser setup with one native Win32 setup window that performs a read-only host/component/disk plan, lets the user select the installation and runtime-data locations, applies UAC-gated WSL enablement with RunOnce reboot continuation, downloads immutable client and machine artifacts, safely and idempotently extracts the portable client, runs explicit project runtime operations and the host bootstrap self-test, and registers sign-in, Start menu, and uninstall. ADR-0025 replaces the browser administrator with a native Win32 window in the installed GUI executable and adds complete Apps & Features plus Start-menu removal, including exact LCTK Podman configuration and runtime residue. Opening LCTK reconnects to or starts the daemon and opens a fresh native authenticated window; the UI can register and start projects without a shell. Schema-2 release manifests fail closed unless the complete Windows setup, launcher, core, runtime, image, and model set is present. Local Windows 10 build 19045 evidence now includes the native setup recovery, a healthy installed daemon, and an isolated live native administrator window connected to a source-built `0.1.2` daemon. Final certification still requires executing the unsigned artifact through the expected Windows warning on a clean Windows 10 22H2 amd64 host with no Docker Desktop, Go, or Git.
