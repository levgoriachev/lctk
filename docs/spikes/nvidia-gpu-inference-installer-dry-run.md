# NVIDIA GPU inference installer dry run

- Status: accepted and completed
- Date: 2026-08-06
- Target release: `0.1.12`
- Architecture: [ADR-0029](../adr/0029-selectable-cpu-and-nvidia-gpu-inference.md)

## Purpose

This dry run fixes the complete immutable package, validation, transaction, rollback, and acceptance plan before any GPU component is installed in the managed LCTK runtime. It rejects silent fallback, mutable package repositories, unverified image tags, destructive container replacement, and acceptance based only on container startup.

The verified starting point is clean branch `codex/fix-podman-missing-image-bootstrap` at `72f8135fe2c55e48873a724a0dcd9475720bb67b`, installed LCTK `0.1.11`, private Podman `5.8.2` on Fedora 43, and a healthy CPU inference container. Windows reports an NVIDIA GeForce GTX 1070 with 8,192 MiB, compute capability 6.1, and driver `582.53`. The managed WSL machine exposes `/dev/dxg` and projected `libcuda.so`, but has no NVIDIA Container Toolkit package or CDI specification.

## Immutable inputs

### llama.cpp CUDA server

The CPU and CUDA images are the same llama.cpp build `b10257`, source commit `22dc605c4ead20e36f447cc67b55ef87e523bd55`.

| Property | Pinned value |
|---|---|
| OCI reference | `ghcr.io/ggml-org/llama.cpp:server-cuda-b10257@sha256:37dd122824e58af9ec861955242abdeeade5a1dcf0ad768bf2b37f903c2805c6` |
| OCI index digest | `sha256:37dd122824e58af9ec861955242abdeeade5a1dcf0ad768bf2b37f903c2805c6` |
| `linux/amd64` manifest | `sha256:cab262d82fc13edd1c3721b357260d2ba5c7495dff2ddfeba948c38336d86ab9` |
| `linux/arm64` manifest | `sha256:5709d379d3571bcbf74626e47e7237159e3763d844bf0403b41c270585e3b750` |
| `linux/amd64` manifest bytes | 3,273 |
| `linux/amd64` compressed content bytes | 2,586,107,421 |
| `linux/amd64` unpacked layer bytes | 4,360,073,216 |
| Layer count | 13 |
| CUDA runtime | 12.8.1 on Ubuntu 24.04 |
| License addition | NVIDIA Deep Learning Container License from `/NGC-DL-CONTAINER-LICENSE` |

The repo-owned measurement streamed all 13 blobs, verified every compressed SHA-256 against the platform manifest, and counted decompressed bytes without installing the image. The production release size uses compressed content bytes; real acceptance must additionally record Podman's installed `Size` after pull.

The exact llama.cpp build configuration includes CUDA virtual architecture 61, which covers the GTX 1070. NVIDIA's CUDA 12.8 release notes require Windows driver 572.61 or newer for CUDA 12.8 GA; the observed 582.53 passes. Production code must compare parsed driver versions and compute capability, not merely the adapter name.

The container command adds `--n-gpu-layers 99`; Podman adds `--device nvidia.com/gpu=all`. All existing model, embedding, pooling, parallel, context, batch, bind-mount, no-host-port, and runtime-network constraints remain unchanged.

Primary evidence:

- [llama.cpp CUDA Dockerfile at the pinned source commit](https://raw.githubusercontent.com/ggml-org/llama.cpp/22dc605c4ead20e36f447cc67b55ef87e523bd55/.devops/cuda.Dockerfile)
- [llama.cpp CUDA architecture defaults at the pinned source commit](https://raw.githubusercontent.com/ggml-org/llama.cpp/22dc605c4ead20e36f447cc67b55ef87e523bd55/ggml/src/ggml-cuda/CMakeLists.txt)
- [CUDA 12.8 release notes](https://docs.nvidia.com/cuda/archive/12.8.1/cuda-toolkit-release-notes/index.html)
- [NVIDIA Deep Learning Container License](https://docs.nvidia.com/deeplearning/frameworks/container-release-notes/license.html)

### NVIDIA CDI package

Only the CDI-capable base package is required. The full toolkit and legacy Docker runtime hook are outside scope.

| Property | Pinned value |
|---|---|
| Artifact | `nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm` |
| Official URL | `https://nvidia.github.io/libnvidia-container/stable/rpm/x86_64/nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm` |
| RPM NEVRA | `nvidia-container-toolkit-base-1.19.1-1.x86_64` |
| Download bytes | 6,190,068 |
| Installed bytes from RPM metadata | 26,628,031 |
| SHA-256 | `b12de77bdffd3df13cea4589a1b04a133b1ffcb250b860f7349420eed37aeb5d` |
| Upstream SHA-512 | `0a286f412d19effeb9f2e40aa10fe157b9a2ddc790a6fd75e323d10e724698ba12312f406786ba61de53bd9b860d5ed73ab500ff004d7e402c5566b90fb92622` |
| Signing-key fingerprint | `c95b321b61e88c1809c4f759ddcae044f796ecb0` |
| License | Apache-2.0 |

The repo-owned RPM harness verified the signed RPM against the official key in an isolated RPM database, inspected payload and metadata, and ran `rpm -Uvh --test` against the actual Fedora 43 machine. Dependencies are limited to facilities already provided by the managed base OS. The package provides `nvidia-ctk`, `nvidia-cdi-hook`, `nvidia-container-runtime`, and `nvidia-cdi-refresh` systemd units. Production setup transports the already verified local RPM directly into the private machine and invokes `rpm`; it does not configure `dnf`, add a repository, or download from inside WSL.

NVIDIA documents that the base package is sufficient for CDI-only use, recommends CDI for Podman and WSL2, and supports device injection with `--device nvidia.com/gpu=all`:

- [NVIDIA Container Toolkit package installation](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
- [NVIDIA CDI support](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html)
- [NVIDIA Container Toolkit 1.19.1 release notes](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/release-notes.html)

## Persistent state and signed manifest

The owner selection is stored at `<lctk-home>/inference.json` with schema 1 and exactly one distribution enum: `cpu` or `nvidia_gpu`. Missing means `cpu`. Writes use an owner-only temporary file, flush, atomic replacement, immediate readback validation, and rollback backup. The document contains no device-derived readiness claim.

Release manifest schema 2 remains readable by the verified `0.1.11` rollback path. The existing `inference_image` remains the CPU identity. A required `nvidia_gpu_inference_image` carries the CUDA reference, manifest digest, supported platforms, compressed size, and unpacked-size evidence. A generic signed artifact named `nvidia-container-toolkit-base/linux/amd64` carries the RPM URL, byte count, and SHA-256. Windows `0.1.12` manifests are invalid unless both inference images, the model, and the CDI artifact are complete and mutually supported.

The verified RPM is retained under `<lctk-home>/runtime/nvidia/1.19.1/downloads/`. Both pulled images and the model remain in installation-owned storage. This is the offline source for repair and repeat distribution changes after the first successful online acquisition.

## Typed validation gates

| Code | Gate | Actionable failure |
|---|---|---|
| `nvidia_adapter_missing` | Windows NVIDIA adapter probe | Select CPU or install a supported NVIDIA adapter. |
| `nvidia_compute_unsupported` | Compute capability is below the pinned build's floor | Select CPU or use a supported adapter. |
| `nvidia_driver_unsupported` | Parsed Windows driver is older than the pinned CUDA minimum | Update the NVIDIA driver, then retry. |
| `wsl_gpu_unavailable` | `/dev/dxg` or projected `libcuda.so` is absent | Repair WSL GPU support or the driver, then retry. |
| `nvidia_package_invalid` | RPM bytes, signature, NEVRA, or installed identity mismatch | Repair from the signed LCTK artifact; do not use an ambient repository. |
| `nvidia_cdi_unavailable` | CDI generation/listing lacks `nvidia.com/gpu=all` | Regenerate CDI through pinned `nvidia-ctk` and report its exact diagnostic. |
| `cuda_image_invalid` | Image digest/platform/size mismatch or pull failure | Repair the signed image artifact. |
| `cuda_device_unavailable` | Container cannot query the expected physical adapter | Repair CDI/WSL/driver exposure. |
| `cuda_offload_missing` | llama.cpp does not report CUDA backend and offloaded layers | Reject activation; never report NVIDIA GPU. |
| `embedding_self_test_failed` | Health or real 768-dimensional embedding fails | Preserve or restore the prior distribution. |
| `inference_swap_failed` | Final-name or project-network activation fails | Restore the old container, aliases, and selection. |

All probe parsers reject missing, ambiguous, malformed, or multi-device output they cannot represent honestly. Diagnostics preserve typed code plus sanitized command detail; they never contain credentials or OAuth material.

## Transaction dry run

Every scenario has explicit preconditions, a no-return boundary, and a verified terminal state.

| Scenario | Ordered transaction | Required terminal state |
|---|---|---|
| Clean CPU install | Verify signed host/runtime/CPU image/model; install private runtime; pull CPU image; start candidate; health and embedding self-test; activate; atomically save CPU | CPU selected and reported; no CDI device or GPU arguments; no project or OAuth state exists yet |
| Clean NVIDIA GPU install | Pass Windows probe; install runtime; pass WSL probe; verify/install pinned RPM; prove CDI; pull pinned CUDA image; start GPU candidate; prove physical adapter, CUDA offload, and embedding; activate; save NVIDIA GPU | NVIDIA GPU selected and actual; exact adapter/driver/VRAM/CC/CDI/offload diagnostics available |
| CPU to NVIDIA GPU | Keep current CPU service live; pass all GPU acquisition and candidate gates; inspect and validate CPU network topology; swap; reconnect aliases; save selection | GPU service healthy on every previous project network; CPU recoverable until commit |
| NVIDIA GPU to CPU | Keep current GPU service live; verify CPU image/model and candidate; inspect topology; swap; reconnect aliases; save selection | CPU actual; no CDI device or offload args; cached GPU components retained |
| Same-distribution update | Stage signed host, code, model, selected image, and pinned CDI if GPU; prove candidate before project stop/host activation; swap and preserve topology; activate host last | Version and selected distribution updated together; previous host/container remain rollback targets until commit |
| Failed update | Stop at the failing preflight or candidate gate; if swap began, restore old container and exact networks; restore selection; execute existing host/project rollback | Previous version, distribution, projects, and OAuth state are healthy |
| Rollback to `0.1.11` | Execute signed host rollback; old core ignores `inference.json` and runs verified CPU image; retain GPU cache and selection document inertly | `0.1.11` healthy on CPU; later compatible update resumes persisted selection |
| Offline repeat switch/repair | Use retained signed RPM, model, and locally verified image IDs; make no network request; run the normal candidate transaction | Same validation and reporting as online activation |
| Uninstall | Stop exact LCTK processes; remove registered integration; delete installation-owned machine, installation home, runtime data, and selection through existing uninstall ownership checks | No LCTK CDI, images, model, selection, projects, credentials, or processes remain |
| No or incompatible GPU | Fail the read-only Windows plan gate before runtime mutation; CPU remains explicitly selectable | No GPU package/image installed and no selection change |
| WSL/CDI failure after a clean machine is created | Abort before candidate activation; existing installations preserve current service; clean installs remove the incomplete owned transaction | No false GPU selection and no silent CPU activation |

## Candidate-and-swap proof plan

Podman permits renaming containers in any state and permits explicit network-scoped aliases. The implementation must nevertheless verify actual Podman 5.8.2 behavior in an isolated, task-named network and disposable containers before changing `lctk-inference`.

The proof executes this sequence:

1. Create a uniquely named isolated network and old container with a fixed network alias.
2. Start and health-check a candidate on the runtime network without exposing a host port.
3. Inspect the old container's complete non-runtime network map and aliases and reject malformed topology.
4. Rename the old container to a unique rollback name and disconnect its saved non-runtime networks so no DNS alias is ambiguous.
5. Rename candidate to the final test name and connect the saved network with each saved alias.
6. Resolve the fixed alias from a third disposable container and prove it reaches the candidate.
7. Inject one forced reconnection failure, remove the candidate, restore the old name and saved aliases, and prove the old endpoint resolves again.
8. Remove only the uniquely named test containers and network and confirm absence.

Production follows the same state machine. The candidate is never attached to project networks before it passes backend and embedding checks. The old container is not deleted until the final container is healthy through every restored network and the selection file is committed.

### Proof result

The isolated proof passed on the installed private Podman 5.8.2 runtime after this plan became inspectable at commit `2acb81744be1661bf8cb9af0546b008eef59e630`. A pinned, already-local Alpine image represented old and candidate HTTP services without a network pull. The old alias returned `old`, the activated candidate returned `new`, and the forced rollback returned `old`; each bounded DNS and HTTP probe succeeded on its first attempt. A deliberately absent network produced Podman exit 125 and triggered the rollback branch. The saved inspection contained the runtime network, the test project network, and the exact `lctk-inference` alias. All uniquely named test containers and the test network were then removed, and the production inference and project containers were not changed.

Primary Podman contracts:

- [podman rename](https://docs.podman.io/en/stable/markdown/podman-rename.1.html)
- [podman network connect](https://docs.podman.io/en/stable/markdown/podman-network-connect.1.html)

## Acceptance sequence

Implementation may begin only after this document and ADR-0029 are committed. The approved runtime mutation then proceeds in these gates:

1. Run the isolated Podman candidate/swap/rollback proof and remove its artifacts.
2. Implement focused selection, manifest, package, probe, inference, transaction, update, Admin, release, uninstall, and documentation tests.
3. Run focused packages, `go test ./...`, `go vet ./...`, `git diff --check`, and required Linux/cgo/race gates.
4. Build one clean signed local `0.1.12` candidate containing all source changes and authenticated local Linux image artifacts.
5. Use native setup to update the installed `0.1.11`, explicitly select NVIDIA GPU, and preserve installation/runtime paths, projects, indexes, OAuth state, and IDE independence.
6. Prove the installed Admin reports the actual GTX 1070, driver, VRAM, compute capability, CDI identity, CUDA backend, offloaded layers, pinned image, and ready embedding endpoint.
7. Delete only the derived semantic index through the product-owned acceptance path, trigger a fresh full build over the same repository corpus, and record start/end timestamps, files, chunks, generations, and duration against the CPU baseline of 30 minutes 36 seconds.
8. Re-run authorized read-only `project_info` and `repository_map` through the existing OAuth integration and require healthy state with fresh matching generations. Do not copy tokens or launch an IDE from LCTK.
9. Update architecture, compatibility, release, security, third-party notices, and the handover evidence with measured facts.

Acceptance fails if any evidence is inferred from configuration instead of measured runtime state, if embeddings are reused for the performance comparison, if project networks or OAuth authorization change, or if GPU failure results in an unreported CPU backend.

## Acceptance result

All gates completed on the Windows 10 acceptance machine. The final local RC was built from committed source `954da54e484ca87d50c32ef3f12b808c6a72c6fa`:

- `LCTK-Setup-local-RC.exe`: 42,948,531 bytes, SHA-256 `5e5e2b5a550732363fa6a251e9fb14ba03509de86d3f3c7bd9434e3e0fc7d9b6`;
- `LCTK-Uninstall-local-RC.exe`: 42,948,531 bytes, SHA-256 `0be582f239085377f76bf3f438c4604e9f05e20c5f531c9b4ea6c1321e4332c7`.

Native setup preserved `C:\Users\Lev Goriachev\AppData\Local\lctk`, `D:\Programs\LCTK_data_store`, the registered project, its indexes, and the existing OAuth authorization. It upgraded installed `0.1.11` to the selected NVIDIA GPU distribution. An explicit rollback then ran `versions\0.1.11\lctk-core.exe` and the signed `0.1.11` project image with CPU inference even though the newer GPU selection remained persisted and inert. The final native update activated `versions\0.1.12\lctk-core.exe` and restored the NVIDIA GPU distribution. The final repeat setup plan reported `Download: 0 B`, proving use of the retained signed artifacts rather than an online repair path.

The running private machine reported exact RPM `nvidia-container-toolkit-base-1.19.1-1.x86_64` and exactly one CDI device, `nvidia.com/gpu=all`. Podman ran CUDA image ID `1a8b5d7aeb67950d649c4c68dc7e8f70d7b81ab4070fdc7234350b5124ad40ef` at the pinned digest. `podman image inspect` measured 4,360,099,002 installed bytes. Runtime diagnostics identified the GTX 1070 and recorded `offloaded 13/13 layers to GPU`, a 66.92 MiB CUDA model buffer, and a 200.16 MiB CUDA compute buffer. Windows independently reported driver `582.53`, 8,192 MiB VRAM, and compute capability 6.1. The installed Admin window reported `LCTK 0.1.12 | podman 5.8.2 linux | inference cuda NVIDIA GeForce GTX 1070 | Ready.`

The fresh full semantic build ran from `2026-08-06T23:13:45.6187191Z` through `2026-08-06T23:26:21.2582770Z`, or 755.588 seconds. It covered 425 files and 2,568 chunks; live status started with all chunks pending, reported zero reuse throughout, and the command completed successfully after embedding the complete corpus. Telemetry collected 676 samples, with peaks of 100% GPU utilization and 1,672 MiB GPU memory. The published full build used generation 52. Two user edits created watcher deltas immediately afterward; after they settled, exact, semantic, and graph were all fresh at generation 54 over 427 files, with watcher pending zero.

The prior CPU baseline took 1,836 seconds for 1,169 chunks over 356 files. Because the repository changed, this is not a same-corpus latency comparison: the GPU corpus was 2.197 times larger. The measured GPU run nevertheless completed in 0.412 of the CPU elapsed time and increased observed chunk throughput from 0.637 to 3.399 chunks per second, a 5.338-times throughput ratio on this machine.

The existing Codex OAuth integration called `project_info` and `repository_map` without configuration changes, token copying, or an IDE launch. It reported healthy installed `0.1.12`, route-and-registry scope for `lctk-cqv5dg6m` at `/workspace`, watcher pending zero, and fresh matching exact/semantic/graph generation 54. The active client rotated its own refresh state during the calls; the authorization remained usable throughout rollback and update acceptance.
