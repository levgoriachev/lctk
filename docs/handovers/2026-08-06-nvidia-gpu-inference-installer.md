# NVIDIA GPU inference installer handover

## Goal and approved direction

Continue from the completed Windows `0.1.11` acceptance and deliver the next local release with an installer choice between CPU and NVIDIA GPU inference distributions.

The machine owner explicitly approved this direction on 2026-08-06 after the first full CPU semantic index took 30 minutes 36 seconds. The accepted product contract is:

- setup presents an explicit CPU or NVIDIA GPU inference distribution choice;
- the choice is persisted and an update continues the selected distribution unless the owner changes it explicitly;
- NVIDIA GPU setup requires a compatible NVIDIA adapter, WSL GPU exposure, Podman CDI support, the pinned CUDA image, and a real embedding self-test;
- a CUDA failure is reported as a typed, actionable failure; it must not silently run the CPU backend while claiming GPU acceleration;
- CPU remains a supported explicit distribution for machines without a compatible GPU;
- Admin UI reports the actual inference backend and useful GPU diagnostics;
- no cloud inference is introduced, project text remains local, and project-scoped state isolation is unchanged.

This changes the CPU-only decision in [ADR-0020](../adr/0020-shared-embedding-and-project-semantic-store.md), so implementation must start with a new superseding ADR rather than silently editing history.

## Completed stable slice

Installed local RC `0.1.11` is active and healthy. It includes:

- collision-safe semantic stable IDs with fail-fast validation before inference;
- live exact, semantic, and graph progress in the native Admin UI;
- selection, caret, and log-scroll preservation across polling;
- inference-container identity comparison by immutable image ID, preserving project-network attachments;
- Windows update synchronization, post-load image identity signing, archive healthcheck preservation, and missing-image bootstrap;
- exact installed-Admin release before replacement, without closing a same-name executable from another path.

The real Windows upgrade left the installed Admin UI open. Setup closed that exact executable, activated `0.1.11`, restarted the daemon, and reopened Admin. Installation location, runtime-data location, project state, indexes, and the existing Codex OAuth authorization were preserved.

The first full CPU semantic build covered 356 files and 1,169 chunks. It ran from `2026-08-06T19:56:17.413235717Z` through `2026-08-06T20:26:53.189649004Z`, then atomically published semantic and graph generation 7. The subsequent compatibility-document edit advanced exact, semantic, and graph to generation 8 with fresh state.

The already-authorized Codex task made read-only calls without changing configuration or credentials:

- `project_info`: project `lctk-cqv5dg6m`, root `/workspace`, scope source `route_and_registry`, healthy full profile `0.1.11`, watcher complete, zero pending paths, exact generation 7 before the documentation edit;
- `repository_map`: fresh exact/graph generation 7, 356 files, 14,643 nodes;
- after the documentation edit, a second bounded `repository_map` reported fresh exact/graph generation 8;
- semantic search reported matching exact and semantic generation 7 before the documentation edit.

This evidence is recorded in [compatibility.md](../compatibility.md).

## CPU baseline diagnosis

The delay was computation, not a stalled watcher or graph build:

- pinned model: `nomic-embed-text-v1.5.Q4_K_M.gguf`, 768 dimensions;
- HTTP batches: 32 chunks;
- llama.cpp slots: 8;
- observed average input: approximately 643 tokens per chunk, maximum 1,913;
- estimated first-build input: approximately 750,000 tokens;
- inference CPU reached approximately 413% across the four WSL vCPUs and used approximately 806 MiB;
- progress advanced throughout the run and the graph published only after semantic publication.

The exact index completed independently in seconds. The 30-minute latency belongs to CPU embedding of the first semantic corpus.

## Current machine evidence for the GPU dry run

- Windows GPU: `NVIDIA GeForce GTX 1070`, 8,192 MiB, compute capability 6.1.
- Windows NVIDIA driver: `582.53`.
- The private WSL2 machine sees `/dev/dxg` and `/usr/lib/wsl/lib/libcuda.so*`.
- The machine does not currently contain `nvidia-ctk` or `nvidia-container-cli`.
- Managed machine OS: Fedora Linux 43 container image with `dnf` and `rpm`.
- Managed Podman: 5.8.2, which supports the CDI schema emitted by current NVIDIA Container Toolkit releases.
- Current inference image is CPU-only. No CUDA image or NVIDIA Container Toolkit package has been downloaded or installed yet.

Primary references already checked:

- NVIDIA documents CDI as the supported Podman device-injection path and shows `--device nvidia.com/gpu=all`: <https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html>.
- NVIDIA release notes state that CDI generation is the recommended WSL2 and Podman mechanism: <https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/release-notes.html>.
- llama.cpp publishes CUDA server images and requires GPU-layer offload for CUDA execution; use the pinned image matching the existing CPU build rather than a floating tag.

## First unfinished gate: GPU distribution dry run

Do not begin production implementation until the following dry run is complete and its results are recorded:

1. Read the complete applicable `AGENTS.md`, this handover, ADR-0020, ADR-0023, ADR-0024, ADR-0026, ADR-0027, `docs/compatibility.md`, and the release/update architecture touched by the installer.
2. Verify Git, the active installation, the private Podman machine, and current container state instead of trusting this snapshot.
3. Determine the immutable CUDA server image that corresponds to the already pinned llama.cpp build/image. Record digest, supported architectures, compressed and installed sizes, license/notice implications, and GTX 1070 compatibility.
4. Determine the exact NVIDIA Container Toolkit/CDI packages required inside the managed Fedora 43 WSL machine. Pin package identities and dependencies; do not add an ambient package-manager dependency or an unverified repository at install time.
5. Model clean install, CPU-to-GPU change, GPU-to-CPU change, ordinary same-distribution update, rollback, offline repeat install, uninstall, and incompatible/no-GPU behavior before changing code.
6. Prove the selected package/image path in an isolated repo-owned test harness first. Any write to the installed runtime at `D:\Programs\LCTK_data_store` requires the already-approved GPU acceptance operation to be explicit and must occur only after the immutable plan is inspectable.
7. Run a real CUDA embedding self-test and verify from llama.cpp diagnostics or device telemetry that work executed on the GTX 1070. Merely starting the CUDA image is not evidence.
8. Compare an intentionally fresh full semantic reindex against the recorded 30-minute-36-second CPU baseline. Reused embeddings are not acceptable performance evidence.

The dry run should reject the implementation plan if it cannot preserve transactional update/rollback, offline repeatability after installation, explicit backend identity, and project isolation.

## Expected implementation areas after the dry run

Search before editing; these are current known areas, not permission to invent adjacent paths:

- `internal/inference`: runtime identity, container arguments, self-test, diagnostics, and selected backend;
- `internal/releasebundle` and `cmd/release-manifest`: immutable component identities, download sizes, and release envelope;
- `internal/runtimeinstall` and managed-machine bootstrap: pinned CDI installation and verification;
- `internal/setupflow`, `internal/updateflow`, and `cmd/lctk-setup`: setup choice, persisted selection, update/rollback behavior, and native controls;
- `internal/adminapi`, `internal/adminclient`, and native Admin UI: actual backend, GPU identity, VRAM, readiness, and typed failures;
- `docs/adr`, architecture, compatibility, security, release, and third-party notices.

Add detailed English comments to new code as required by repository policy. Preserve fail-fast behavior, immutable identities, and server-enforced project scope.

## Verification and release acceptance

At minimum, acceptance must include:

- focused unit/transaction tests for distribution selection, persistence, immutable identity validation, CDI planning, CUDA capability refusal, update, rollback, and uninstall;
- `go test ./...`, `go vet ./...`, `git diff --check`;
- Linux/cgo tests for any changed code-intel image path, including the race gate used by the preceding slice;
- a clean local RC build with signed post-load image identities;
- a real `0.1.11` to next-version Windows update through the installed native setup;
- installed Admin UI evidence for backend identity and progress;
- existing OAuth authorization still calling `project_info` and `repository_map` read-only with fresh matching generations;
- a fresh full GPU semantic-index duration and proof of actual GPU utilization;
- compatibility documentation updated with measured evidence.

Do not create a GitHub Release, tag, or pull request. Commit and push only to `codex/fix-podman-missing-image-bootstrap`.

## Git, installation, and process state

- Repository: `D:\Projets\lctk`.
- Branch: `codex/fix-podman-missing-image-bootstrap`.
- Stable pushed head before this handover: `1ab76f1677456c05b875699482c837d6c1e9429f`.
- Origin branch matched that head and the worktree was clean before adding this document.
- Active installed core: `0.1.11`, `C:\Users\Lev Goriachev\AppData\Local\lctk\versions\0.1.11\lctk-core.exe`.
- Previous verified core: `0.1.9`.
- Installed Admin UI was open from `C:\Users\Lev Goriachev\AppData\Local\lctk\bin\lctk-setup.exe`.
- Daemon health: `status: ok` at `127.0.0.1:4444/health`.
- Running containers: `lctk-inference` with the pinned CPU image and `lctk-lctk-cqv5dg6m-code-intel` with `localhost/lctk/code-intel:0.1.11`.
- No task-owned background shell or helper process remains.
- `.artifacts/LCTK-Setup-local-RC.exe` is an ignored local `0.1.11` candidate. No temporary UI helper remains.

After committing this document, replace the stable head above with the handover commit in the startup prompt and verify origin matches it.

## Continuation result

The delegated continuation completed the dry run, implementation, corrective acceptance loops, and real GTX 1070 acceptance. ADR-0029 and the immutable plan were committed before runtime mutation. The final accepted implementation commit is `954da54e484ca87d50c32ef3f12b808c6a72c6fa`; it includes the fixes discovered by real update, rollback, CUDA-log, and full-reindex acceptance. The exact measured result and RC checksums are recorded in [the completed dry run](../spikes/nvidia-gpu-inference-installer-dry-run.md) and [compatibility.md](../compatibility.md).

Installed `0.1.12` is active from `C:\Users\Lev Goriachev\AppData\Local\lctk\versions\0.1.12\lctk-core.exe`; the previous verified version is `0.1.11`. The private Podman project and GPU inference containers are healthy, Admin reports the actual GTX 1070 CUDA backend as ready, a fresh 2,568-chunk GPU build completed in 755.588 seconds with zero reuse, and existing OAuth read-only calls returned fresh matching generation 54. No IDE was launched, no token was copied, and no GitHub Release, tag, or pull request was created.
