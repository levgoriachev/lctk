# Changelog

All notable changes to LCTK will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases will follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Initial architecture and product documentation.
- Apache-2.0 `LICENSE`, `NOTICE`, security policy, governance, support, and pre-contribution repository policy.
- Go 1.25 foundation with a single `lctk` executable.
- `GET /health` and `/mcp` using the official MCP Go SDK v1.6.1, with temporary compatibility tool `foundation_info`.
- Native watcher event-delivery proof using `fsnotify v1.10.1`.
- Read-only Docker Desktop availability diagnostic using Moby client v0.5.0.
- Automated CLI, HTTP, MCP transport, and watcher tests.
- Hosted Windows/macOS CI and non-publishing Windows amd64 and Darwin arm64 dry-run archives plus checksums.
- Reproducible Slice 0.2 gateway harness, measurements, and hard-gate results.
- Accepted ADR-0009 selecting an LCTK-owned Go gateway embedded in the host daemon.
- Registered project lifecycle with isolated Linux containers, persistent state volumes, loopback-only published ports, and route-bound bearer grants.
- Persistent Zoekt exact search, native change observation, reconciliation after downtime, resource modes, and typed freshness and recovery status.
- Read-only Git status/diff tools, an approved-command runner with container-enforced limits, and a local Admin API/UI separated from project MCP authority.
- Tree-sitter outlines, definitions, references, and syntax status for Go, Python, Rust, C, C++, JavaScript, TypeScript, and TSX.
- Persistent AST-aware hybrid semantic search backed by one shared pinned local embedding process and an isolated SQLite store per project.
- Derived name-match callers, callees, dependency, impact, and repository-map tools plus explicit reviewed project memory with optimistic revisions.
- Transactional signed-manifest bootstrap, update, schema migration, host activation, and rollback through a stable digest-verifying launcher.
- Fail-closed Ed25519 manifest and provenance gates, native Linux amd64/arm64 image execution, SBOMs, checksums, attestations, and signed release manifests.
- Complete 18-tool verification through Codex and an independent MCP Go SDK client, plus parameterized semantic and exact-search stress evidence through one million files or chunks.
- Windows one-click setup with a native Win32 plan and folder selection, UAC-gated WSL2 enablement and reboot continuation, a pinned private Podman runtime, sign-in daemon, Start-menu launcher, and shell-free project registration in the Admin UI.
- Schema-2 signed release inventory binding the Windows setup, launcher, host core, Podman client, WSL machine image, OCI images, and embedding model.
- Native Windows administrator window with project lifecycle, Codex launch, grants, diagnostics, logs, and uninstall controls, without a browser or WebView runtime.
- One-file locally signed Windows release-candidate builder for complete pre-publication setup testing.

### Changed

- Windows setup now stores the selected installation and Podman runtime-data locations under `HKCU\Software\LCTK`; private Podman processes pin the latter as `XDG_DATA_HOME` so the WSL disk, images, volumes, indexes, and project memory use the chosen drive.
- Windows project, inference, diagnostic, and approved-command lifecycles now use the explicit `lctk-runtime-root` Podman connection and deterministic runtime plans instead of Docker Desktop, Moby, and Compose.
- Official Windows executables are intentionally unsigned for the initial open-source release; integrity remains enforced by the tagged workflow, launcher binding, SHA-256 checksums, GitHub attestations, and Ed25519-signed component manifest.
- The first official release inventory is Windows amd64 only; macOS remains a non-publishing development and CI compatibility target.
- Windows Apps & Features and Start-menu uninstall now remove the managed machine, LCTK-named Podman residue, desktop integration, registry locations, and program files while preserving unrelated Podman data.

### Fixed

- Repeated setup now leaves verified Podman executables untouched and stops the managed machine before repairing a changed helper that Windows can lock.
- Start-menu registration now targets the physical per-user Programs folder and creates shortcuts through `IShellLinkW`, including when the user profile path contains spaces or non-ASCII text.
- Bootstrap now recognizes Podman's `image not known` response as an absent local image and proceeds to pull the signed image digest.
- Windows setup now accepts a working WSL2 runtime as authoritative virtualization evidence when an active Hyper-V hypervisor hides the firmware capability flag.
- Verified Windows setup repairs replace rejected existing downloads and private runtime executables with the fully staged immutable artifact.
- Exact-only code-intel containers no longer treat a typed nil semantic store as enabled during startup reconciliation.
- Semantic ranking retains bounded top-K candidates while preserving exact total counts and deterministic tie-breaking.
- Exact inventory hashing uses bounded concurrency instead of serial storage round trips.
- Schema migration activation, validation, failure preservation, and rollback now recover every process-interruption state without leaving the authoritative database name absent or trusting a replacement inode.
