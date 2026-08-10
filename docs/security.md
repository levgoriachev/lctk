# Trust model and security boundaries

## First-release scope

LCTK is a local-first, single-user application for the user's own trusted repositories. The first release does not promise safe execution of deliberately malicious code.

Security boundaries are primarily intended to protect against:

- an erroneous tool call by an agent;
- accidental escape from the project root;
- mixing results from different projects;
- hung or excessively resource-intensive commands;
- implicit access by another local process or browser page;
- accidental granting of administrative privileges to a coding session.

## Project isolation as a correctness boundary

Mandatory rules:

1. The authoritative `project_id` comes from the route and a validated OAuth resource audience.
2. `project_id`, `repository_root`, and absolute paths from model arguments are not considered authoritative.
3. A tool operates only on the root obtained from the server-side registry.
4. Project containers mount only their own project folder.
5. Indexes, memory, caches, networks, and volumes are separated by project.
6. Cross-project isolation is verified by integration and security tests.

## Paths

The host daemon canonicalizes the registered folder using host OS facilities. A manifest from Git cannot assign the host mount.

Filesystem tools must accept project-relative paths and protect against:

- `..` traversal;
- absolute paths;
- symlink/junction escape;
- platform-specific path aliases;
- a race between validating a path and opening the file.

The precise safe file-opening strategy requires a separate ADR and OS-specific tests.

As implemented in Slice 1.5, the search boundary holds these in a specific way. The project service has exactly one workspace mounted and no way to express a second, so scope is structural rather than checked. Enumeration skips symbolic links instead of following them, because following one is the ordinary way out of a read-only mount. An absolute path, a parent traversal, or a Windows-style path is refused rather than clamped, in both a path filter and a change batch: silently reinterpreting a request is worse than declining it, because the caller then believes it searched something it did not.

As implemented in Slice 3.1, the Git surface is read-only by construction: it invokes only `status`, `diff`, and `rev-parse`, never a command that writes, and it disables optional locking so a query cannot collide with a Git operation the user is running. A path argument is refused when it is absolute, escapes the repository, or begins with a dash, rather than being reinterpreted. A project registered below a repository root is scoped to its own subtree, so its endpoint cannot report a sibling's changes. Git is executed on the host, which is the same exposure the daemon already accepts by invoking `docker` from PATH.

## Runner

The runner is separated from the gateway and indexers. For the trusted-local profile, the minimum guardrails are:

- mount only one project root;
- a fixed working directory;
- timeout and cancellation;
- an output-size limit;
- process-tree cleanup;
- PID, CPU, and RAM limits;
- explicit selection of the project's `none` or `full` network policy;
- no Docker socket or other projects' volumes;
- a local audit log of the command and result without secrets.

As implemented in Slice 3.2, every one of those guardrails is a flag on a container created and destroyed around one command, which is why they hold against a command that ignores them. Verified from inside a running container: `pids.max` 512, `memory.max` 2 GiB, `cpu.max` 2 cores, `/workspace` writable and the only mount, no `/var/run/docker.sock`, and no network resolution under the `none` policy.

What may run is decided before any of that, in [ADR-0017](adr/0017-command-policy-and-the-runner.md). A repository proposes `build`, `test`, and `lint` in its manifest; the machine owner approves each one; a client runs one **by name and only by name**, with no parameter that carries a command line. An approval is bound to the exact text approved, so a command rewritten in the repository is refused until a person approves it again — otherwise every approval would be a standing grant to run whatever that name later pointed at.

The runner image is approved the same way and for the same reason: choosing the container is choosing what a command can do. A project with no approved image runs nothing.

Every run is recorded in the LCTK home, one append-only line each, including the runs that were refused.

These measures protect against mistakes and runaway processes; they do not guarantee safe execution of hostile malware.

## Client access

The localhost endpoint is protected by owner-approved OAuth. The user never copies or enters a credential manually.

An authorization restricts:

- a specific client;
- one exact project resource URL;
- the minimal project scope;
- access and refresh expiration.

An authorization can be revoked independently of other clients. A token for one exact project resource does not open another project.

As implemented by [ADR-0026](adr/0026-owner-approved-oauth-for-project-mcp.md), project registration issues no credential. An IDE discovers the loopback authorization server, dynamically registers a public client, and uses authorization code with S256 PKCE. The pending request shows the machine owner the client, project, scope, callback, and expiry. Approval creates a single-use code, a 15-minute opaque access token, and a rotating refresh token. Only hashes are persisted in the owner-only LCTK home; bearer values exist only in the client response and are never displayed or logged.

The endpoint checks the credential before consulting the registry, so an unauthenticated caller cannot learn which projects exist. A valid credential scoped to another project receives a distinct refusal from an unknown credential, so a client can correct itself without being told what else is registered. An unauthenticated probe of any project route receives the same typed `401` whether the project exists or not, so the diagnostic surface does not become a way to enumerate projects.

Credential delivery is entirely the OAuth client's responsibility. LCTK does not launch an editor, write client configuration, inject environment variables, or reveal tokens. The authorization code, client, exact callback, PKCE challenge, and RFC 8707 resource are bound together; refresh tokens rotate and revocation invalidates every token in the authorization.

## Browser access

An ordinary web page does not receive access merely because it knows the `127.0.0.1` address.

An external browser origin requires an explicit flow:

1. show the origin;
2. show the requested projects and capabilities;
3. obtain user confirmation;
4. issue a restricted browser session;
5. validate the origin on subsequent requests;
6. support allow-once, persistent allow, and revocation.

The native Admin UI uses a separate local session, as amended by [ADR-0025](adr/0025-native-windows-admin-and-complete-uninstall.md). The daemon issues an exchange code into the owner-only LCTK home; the installed native process reads and spends it directly, then keeps its session credential and CSRF token only in process memory. No admin credential is placed in a URL, browser history, or browser cookie store.

Every request must still carry a `Host` header naming loopback and every state-changing request must echo the session's CSRF token. These checks keep the loopback API fail-closed if a non-native client attempts to reach it.

No admin handler accepts a project OAuth token and no project route accepts an admin session. A coding agent holding a project token cannot administer the machine, and the admin surface never serves an OAuth credential to the native client.

## Manifest trust

A tracked `.mcp-project.yaml` may contain safe declarations:

- profile;
- excludes;
- language and tooling preferences;
- index settings;
- proposed build, test, and lint commands.

The local registry stores and confirms:

- the actual host path;
- project ID binding;
- client credentials;
- grants;
- host resource limits;
- applied command and network permissions.

The repository manifest cannot mount arbitrary host directories, grant the admin capability, or provide secrets. It may propose an indexing debounce window, which the host clamps rather than adopts, because the setting costs the machine resources and host policy owns that.

Two further documents live in the owner-only LCTK home for the same reason as the registry: they are host state about a project rather than project content, and nothing inside a repository may reach them. `settings.json` holds machine-wide watcher and resource policy. One change journal per project holds project-relative paths the host observed, and no file content.

`inference.json` is also owner-only host state. It records exactly the selected local distribution and never stores a readiness claim, GPU identity, project text, credential, or OAuth value. GPU readiness is measured across Windows, the private WSL machine, CDI, the running container, llama.cpp CUDA startup logs, and an embedding response. The pinned GPU server uses trace verbosity because b10257 otherwise omits offload evidence; acceptance verified that trace records startup, slot identity, and token counts but not embedding input text. Diagnostics read only the fixed two-minute startup window and reject output above 1 MiB. A successful complete-offload result is then stored separately in owner-only `inference-evidence.json`, bound to the exact container ID, image ID, configuration revision, distribution, and process start time. The document retains at most four candidate/rollback records; a different or restarted process cannot reuse them. The trace stream is isolated in a 32 MiB container log instead of the shared system journal. The NVIDIA RPM crosses into the private machine only after signed size and SHA-256 verification; setup adds no package repository and never invokes an ambient Podman, WSL distribution, or NVIDIA toolkit installation.

## Guarantees not claimed

The first release does not guarantee:

- hostile repository containment;
- protection from the machine owner;
- multi-user authorization;
- enterprise compliance;
- network egress allowlisting;
- the security of third-party LSP or index binaries beyond ordinary dependency hygiene.
