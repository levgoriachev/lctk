# Development

## Prerequisites

- Go 1.25.x;
- Git;
- the pinned private Podman client and `lctk-runtime` machine only for managed-runtime integration work.

No container runtime is required for unit tests. Runtime integration tests use `LCTK_PODMAN_PATH` to select a private client and still require the explicit `lctk-runtime-root` connection. `lctk doctor` reports an actionable error when that managed runtime is unavailable.

## Checks

On Windows PowerShell:

```powershell
./scripts/check.ps1
```

## Warm-index exploration benchmark

The development-only `swe-explore-benchmark` command implements the fail-fast orchestration contract in [the SWE-Explore benchmark specification](benchmarks/swe-explore.md). Build it from source; it is not included in the installed LCTK product:

```powershell
go build -o .artifacts/swe-explore-benchmark.exe ./cmd/swe-explore-benchmark
.artifacts/swe-explore-benchmark.exe validate --config .artifacts/swe-explore-config.json
.artifacts/swe-explore-benchmark.exe prepare --config .artifacts/swe-explore-config.json --instance INSTANCE_ID
.artifacts/swe-explore-benchmark.exe run --config .artifacts/swe-explore-config.json --instance INSTANCE_ID --arm ARM_ID --output .artifacts/results/INSTANCE_ID/ARM_ID.json
.artifacts/swe-explore-benchmark.exe pair --config .artifacts/swe-explore-config.json --instance INSTANCE_ID --provider codex --output-dir .artifacts/results/INSTANCE_ID/codex
.artifacts/swe-explore-benchmark.exe pair --config .artifacts/swe-explore-config.json --instance INSTANCE_ID --provider claude --output-dir .artifacts/results/INSTANCE_ID/claude
.artifacts/swe-explore-benchmark.exe official-score --config .artifacts/swe-explore-config.json --result .artifacts/results/INSTANCE_ID/ARM_ID.json --python python
```

Keep datasets, client binaries, task checkouts, MCP endpoint-only configuration, and run artifacts under ignored `.artifacts/`. The issue-source JSONL must contain only `instance_id`, `repo`, `base_commit`, and `problem_statement`; the harness joins it to the unchanged public SWE-Explore JSONL by exact identifier and verifies both configured SHA-256 digests before every run.

The configured workspace must be a dedicated clean Git checkout registered once as a full LCTK project. `prepare` refuses a dirty checkout, switches to the exact base commit, and waits outside the measured interval for matching fresh exact, semantic, graph, and watcher generations. The MCP JSON contains exactly one loopback server URL and no headers or credentials. OAuth remains owned by each client.

Native Codex arms use the official non-interactive `exec --ephemeral --json --ignore-user-config` boundary with external context features disabled. Native Claude arms advertise only `Read`, `Glob`, and `Grep` under strict empty MCP configuration. Treatment arms add only the single configured project endpoint and the explicit read-only code-intelligence allowlist, and are rejected unless their trace proves a successful LCTK tool call. Do not interpret the repository-owned scorer as publication evidence; `official-score` imports `eval.py` from the configured commit-pinned SWE-Explore checkout. The accepted single-run evidence is in [the readiness report](benchmarks/swe-explore-single-run.md).

On macOS or another POSIX shell:

```sh
./scripts/check.sh
```

The checks require canonical `gofmt` output, run `go vet ./...`, and execute `go test -cover ./...`.

## Build and run

```sh
go build -trimpath -o lctk ./cmd/lctk
./lctk version
./lctk daemon
```

The foreground daemon listens on `127.0.0.1:4444` by default. Slice 0.1 provides:

- `GET /health`;
- Streamable HTTP MCP at `/mcp` with temporary compatibility tool `foundation_info`;
- `lctk watch-once <directory>` for a raw single-event probe on any directory, registered or not; the project watcher is `lctk project watch --follow`;
- `lctk doctor` for a read-only managed Podman identity probe.

These commands are foundation evidence, not the complete project lifecycle described elsewhere.

## Connecting a client

LCTK does not configure or launch a client. After registering a project, obtain its endpoint from `lctk project add` output or the native administrator:

```sh
http://127.0.0.1:4444/projects/{project_id}/mcp
```

Add it as a Streamable HTTP MCP server in the client, restart the client when its UI requests that, and select **Authenticate**. The client starts OAuth; approve the resulting pending connection in the native LCTK window. LCTK stores only credential hashes and never edits the client configuration or exposes a token. See [ADR-0026](adr/0026-owner-approved-oauth-for-project-mcp.md).

## Watching a project

A running daemon watches each running project and records what it sees in a per-project change journal. See [indexing](indexing.md#the-change-journal) for what the journal claims and [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md) for why.

```sh
./lctk project watch PROJECT             # what the daemon has recorded
./lctk project watch --follow PROJECT    # stream normalized events, writing nothing
./lctk settings show                     # the debounce and watch policy in force
```

`watch` reads the journal from disk, so it answers even when no daemon is running — which is when the last recorded state is most worth seeing. `--follow` starts a watcher of its own for diagnosis and does not write to the journal.

A running daemon applies what it observes to the index by itself, so `lctk project reindex` is no longer part of an editing loop. It remains useful without a daemon, and it is the documented recovery for a corrupt index.

The machine policy lives in `settings.json` in the LCTK home, and `settings show` prints its path whether or not the file exists:

```json
{
  "schema_version": 1,
  "watch": {
    "debounce_ms": 3000,
    "max_debounce_ms": 30000,
    "max_watched_directories": 20000,
    "idle_stop_seconds": 900
  }
}
```

A project may propose its own window with `index.debounce_ms` in the manifest. The host clamps it.

## What a project is allowed to cost

```sh
./lctk project resources PROJECT                 # mode, limits, and disk use
./lctk project resources --mode quiet PROJECT    # set this project's mode
./lctk project resources --mode default PROJECT  # follow the machine again
```

Modes are `quiet`, `normal`, and `fast`; see [indexing](indexing.md#resource-modes) for what each costs. Limits are applied when a container is created, so a change takes effect at the next restart.

`start` and `restart` refuse when the volume is short of space, and `--yes` overrides that.

## Git tools

A running project serves `git_status` and `git_diff` alongside `project_info` and `exact_search`. They are read-only, bound to the project by the route, and they exist for the client that has no shell on the machine; an editor's own terminal does not need them.

`git_status` reports the branch, commit, upstream position, and changed paths. `git_diff` returns a bounded unified diff, optionally of what is staged and optionally restricted to given paths. Paths are repository-relative, and `prefix` says where the project sits inside the repository when the two differ.

## Running a project's commands

A repository proposes commands in its manifest; nothing runs until the machine owner approves each one and names an image.

```sh
./lctk project commands PROJECT                          # what is proposed, approved, and runnable
./lctk project commands --image golang:1.25 PROJECT      # what approved commands run in
./lctk project commands --approve test PROJECT           # approve the text as it stands now
./lctk project commands --network full PROJECT           # give commands network access
./lctk project commands --revoke test PROJECT
```

An approval is bound to the exact command text. Edit it in the manifest and the approval lapses, which the status line reports as `CHANGED`. See [ADR-0017](adr/0017-command-policy-and-the-runner.md).

This repository has its own [`.mcp-project.yaml`](../.mcp-project.yaml), so `lctk project commands lctk` shows the mechanism against a real project.

## The native administrator

```sh
./lctk admin open            # open the installed native Windows administrator
```

The installed GUI process reads and spends the daemon's one-time code directly. The credential remains in process memory and is never placed in a URL or browser. See [ADR-0025](adr/0025-native-windows-admin-and-complete-uninstall.md).

## Local one-file release candidate

```powershell
./scripts/build-local-rc.ps1
```

This creates `.artifacts/LCTK-Setup-local-RC.exe` and `.artifacts/LCTK-Uninstall-local-RC.exe` without a tag, push, or GitHub Release. Both executables contain the locally built setup, host core, stable launcher, and a locally signed manifest; the manifest retains the verified runtime, image, and model identities from the published template release. The first opens setup and starts a temporary numeric-loopback file endpoint while the real native setup process is open. The second directly opens the fixed uninstaller for recovery from a partial older removal and does not open a network listener. Both remove their extracted payload after the native process exits. Both candidates are unsigned and are for local acceptance only.

## The code-intel service

[`images/code-intel`](../images/code-intel) is a separate Go module that builds the per-project search service. It links Zoekt, whose low-level index package is Unix-specific, so it is excluded from the root module's build on purpose and never compiles into the host executable.

That also means the ordinary checks above never touch it. It has its own CI job on Linux, and locally it is built and tested inside a container:

```sh
docker run --rm -v "$PWD/images/code-intel:/src" -w /src -e CGO_ENABLED=1 golang:1.25 go test ./...
```

`lctk image build` compiles it into the reusable image. A project must be restarted after a rebuild to pick up the new service.

## Dependency policy

Direct dependencies are pinned in `go.mod`, and checksums are committed in `go.sum`. Prefer the standard library. A new dependency requires a concrete capability, license review, maintenance review, and documentation update when it changes an architectural contract.
