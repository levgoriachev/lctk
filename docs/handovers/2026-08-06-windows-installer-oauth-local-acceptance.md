# Windows installer and OAuth local acceptance handover

## Goal and current result

The work converted LCTK from a developer-operated project into a locally testable Windows product with a native setup, native Admin window, complete uninstaller, managed Podman/WSL runtime, project-scoped MCP instructions, owner-approved OAuth, and in-place setup upgrades.

The current local result is LCTK `0.1.5` installed on Windows 10 build 19045. The daemon is healthy, project `lctk-cqv5dg6m` for `D:\Projets\lctk` is running with the full profile, and Codex Desktop completed OAuth public-client registration plus native owner approval. No GitHub Release or tag was created.

## Accepted requirements and invariants

- End users install one executable and do not install Docker Desktop, Podman Desktop, Go, Git, or build tools.
- Setup, Admin, and uninstall are native Windows windows. Product operations do not open a browser or flash terminal windows.
- The IDE owns its MCP configuration and process lifecycle. LCTK displays a project URL and never launches or edits the IDE.
- MCP authentication uses discovery, dynamic public-client registration, S256 PKCE, a pending request in the native Admin window, explicit owner approval, short-lived access tokens, rotating refresh tokens, and revocation. LCTK never displays a recoverable bearer token.
- Every project endpoint and authorization is server-bound to one exact registered project.
- Setup supports clean install, strictly newer in-place upgrade, exact same-version repair, and fail-closed downgrade refusal.
- In-place update preserves the selected installation/runtime-data locations, managed WSL identity, projects, indexes, memory, settings, and OAuth state. It reuses the CLI update coordinator for candidate project health gates and host rollback.
- A changed local or published setup receives a new Semantic Versioning product version. Different bytes under one installed version are rejected.
- Official and local Windows binaries remain intentionally unsigned under ADR-0023; release identity uses the signed manifest, immutable digests, launcher binding, and publication evidence.
- Local acceptance happens before GitHub publication. Do not create a release or tag until the remaining acceptance path is explicitly completed.

## Completed implementation

- `cmd/lctk-local-setup` builds one-file local setup and recovery-uninstall wrappers.
- `cmd/lctk-setup` owns native setup, Admin, and uninstall modes.
- `internal/setupflow` classifies install/upgrade/repair and coordinates daemon, runtime, host, bootstrap, desktop, and rollback phases.
- `internal/updateflow` is the single update transaction shared by CLI and setup.
- `internal/installation` supports immutable same-version repair without destroying the previous-version rollback pointer.
- `internal/daemonstate` starts/stops only the installation-owned daemon, waits for verified process exit, and recovers an absent stale PID through an independent Windows process inventory while refusing to guess about an inaccessible live PID.
- `internal/projectauth` and the gateway implement the accepted OAuth flow.
- Native Admin displays MCP connection instructions, polls pending OAuth requests, approves/denies them, and lists/revokes authorized clients.
- ADR-0025, ADR-0026, and ADR-0027 record the native Admin/uninstall, OAuth, and setup-upgrade decisions.

## Real local acceptance completed

- The earlier `0.1.2` installation used:
  - installation: `C:\Users\Lev Goriachev\AppData\Local\lctk`;
  - runtime data: `D:\Programs\LCTK_data_store`.
- Local RC `0.1.4` correctly detected an upgrade but stopped before mutation when stale `daemon.json` produced Windows `Access is denied`.
- The stale-PID defect was fixed fail-closed and released locally as `0.1.5`.
- Local RC `0.1.5` upgraded the installed product from `0.1.2` to `0.1.5` in place.
- Installed identity:
  - version: `0.1.5`;
  - source commit: `ab7a4af2439397d086ea4a8ea516badd6b0ce5c6`;
  - host: `windows/amd64`, Go `1.26.5`.
- `GET http://127.0.0.1:4444/health` returned `status: ok`, version `0.1.5`.
- Project `lctk-cqv5dg6m` is running and healthy with container `lctk-lctk-cqv5dg6m-code-intel`.
- An unauthenticated MCP initialize request returned the required `401` and `WWW-Authenticate: Bearer resource_metadata=...` challenge.
- Protected-resource, authorization-server, authorization, token, and dynamic-registration discovery endpoints returned the exact loopback identities.
- Codex registered one public client and received one non-revoked authorization for `lctk-cqv5dg6m` after native approval.
- The old Codex task did not dynamically gain the new MCP server: direct tool lookup was unavailable and `resources/list` reported `unknown MCP server 'lctk'`. This is the first unfinished acceptance gate, not evidence of an LCTK server failure.

## Verification commands and results

All commands ran from `D:\Projets\lctk`:

```text
go test ./...       PASS
go vet ./...        PASS
git diff --check    PASS
scripts/build-local-rc.ps1  PASS
```

Targeted coverage includes install/upgrade/repair classification, downgrade refusal, shared update coordination, candidate project restoration, late setup rollback, same-version immutable repair, previous-daemon restart, stale PID plus access-denied recovery, and fail-closed inaccessible-live-PID behavior.

## Local artifacts

These files are generated beneath ignored `.artifacts/` and are intentionally not committed:

- `D:\Projets\lctk\.artifacts\LCTK-Setup-local-RC.exe`
  - version `0.1.5`;
  - source commit `ab7a4af2439397d086ea4a8ea516badd6b0ce5c6`;
  - SHA-256 `2AAF519BEFB23BBA8A7652EC74A6BCEB784C0AAFD4B26C837615F7C3D96C239E`.
- `D:\Projets\lctk\.artifacts\LCTK-Uninstall-local-RC.exe`
  - SHA-256 `DFCE89180ACBC52AEEAFCA5DFEF4E6B7840E9C296554517E584596E91B40FF4E`.

The embedded local manifest uses an ephemeral Ed25519 key and binds version `0.1.5` to commit `ab7a4af`. Do not rebuild different binaries as `0.1.5`; bump the version first.

## Git state at handover preparation

- Repository/worktree: `D:\Projets\lctk`.
- Branch: `codex/fix-podman-missing-image-bootstrap`.
- Remote: `origin` -> `https://github.com/lev-goryachev/lctk.git`.
- The branch was fetched immediately before handover and was `0` behind / `16` ahead of `origin/main` after the acceptance-evidence documentation commit and before this handover commit.
- Relevant commits, newest implementation first:
  - `56e1fd2` — record Windows upgrade and OAuth acceptance evidence;
  - `ab7a4af` — recover setup from stale daemon state;
  - `54eb5f2` — add native in-place setup upgrades;
  - `8999436` — replace editor-managed grants with approved OAuth;
  - `b1b4312` through `aaa0ea5` — native installer/admin/uninstall/runtime recovery sequence.
- This handover file is the only change after `56e1fd2`; the pushed remote branch tip must contain it directly above that commit.

## Active processes and external state

- Installed native Admin window: `lctk-setup.exe --admin` under the recorded installation directory.
- Installed daemon: `versions\0.1.5\lctk-core.exe daemon`.
- Managed project `lctk-cqv5dg6m` is running and healthy.
- No local setup wrapper or extracted setup workspace remains active.
- Do not stop these processes merely to begin the next task; the first MCP acceptance call should use the running product.

## First unfinished gate

Create a new Codex task for `D:\Projets\lctk`, which should load the already configured and authorized MCP server. Without editing configuration or copying any bearer value:

1. confirm that the LCTK MCP tool catalog is present;
2. call a read-only project tool such as `project_info` or `repository_map` through the configured LCTK server;
3. verify the response is scoped to `lctk-cqv5dg6m` and reports current freshness/index evidence;
4. record the real result in `docs/compatibility.md` in the same change;
5. if the new task still does not load the server, diagnose Codex server/task binding using evidence from Codex configuration and the existing valid authorization; do not add a manual token, launch the IDE from LCTK, or weaken OAuth.

## Remaining acceptance after the first gate

- Same-version `0.1.5` repair with preserved rollback metadata.
- In-place upgrade while a project and persistent index already exist and are running before setup begins.
- Explicit rollback followed by recovery to the current version.
- Complete uninstall acceptance, including the preserve/remove choice and absence of terminal windows.
- Clean-host WSL enablement/reboot and non-default-drive creation.
- GitHub publication only after the user accepts the complete local path.

## Completion criteria for the next task

The immediate next task is complete when a newly created Codex task has made at least one authenticated read-only LCTK MCP tool call against `lctk-cqv5dg6m`, the exact result is documented, targeted checks pass, and its scoped commit is pushed to the same remote branch. Do not repeat installer implementation or completed OAuth setup unless new evidence shows a defect.
