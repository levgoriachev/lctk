# ADR-0029: Selectable CPU and NVIDIA GPU inference distributions

- Status: accepted
- Date: 2026-08-06
- Deciders: project maintainers
- Supersedes: the CPU-only inference distribution in [ADR-0020](0020-shared-embedding-and-project-semantic-store.md)
- Amends: [ADR-0022](0022-transactional-bootstrap-update-and-release-evidence.md), [ADR-0023](0023-managed-podman-wsl-runtime-and-windows-installer.md), [ADR-0024](0024-native-windows-setup-and-user-selected-storage.md), and [ADR-0027](0027-native-setup-in-place-upgrade-and-repair.md)

## Context

The first full semantic index on the supported CPU distribution embedded 1,169 chunks in 30 minutes 36 seconds. LCTK must retain a CPU path for machines without a compatible NVIDIA adapter while allowing the machine owner to select a local NVIDIA GPU distribution when the complete Windows, WSL, CDI, image, and model path is verifiably compatible.

The inference service remains installation-wide stateless compute. Project text, embeddings, indexes, credentials, and OAuth state remain project-scoped or host-owned exactly as defined by ADR-0020, ADR-0023, and ADR-0026. A GPU selection must not introduce cloud inference, Docker Desktop, a second WSL distribution, ambient host package state, published WSL ports, or a silent CPU fallback.

An update or distribution change also cannot replace the shared container destructively before its successor is proven. The inference container may be attached to multiple isolated project networks, each with the fixed `lctk-inference` alias. Losing those attachments would break running projects and violate transactional update behavior.

## Decision

Setup presents exactly two inference distributions: `CPU` and `NVIDIA GPU`. CPU is the default for a new installation. An existing installation keeps its persisted selection unless the owner explicitly changes it in the accepted setup plan.

The selection is stored in a separate owner-only, schema-versioned `inference.json` document under the LCTK installation home. A missing document means CPU. The host settings schema is not changed because the verified `0.1.11` rollback target must continue to read its existing settings. The selection file is committed only after the selected distribution passes installation, activation, and a real embedding self-test; failure restores the preceding document atomically.

The signed release manifest retains the existing CPU `inference_image` identity and adds one required NVIDIA GPU image identity plus the exact NVIDIA CDI package artifact. Both distributions use the same pinned llama.cpp source generation and the same pinned embedding model. Setup verifies signed size and SHA-256 before a package or image crosses the managed-machine boundary. No package repository is added to the managed machine.

The NVIDIA GPU distribution is supported only when every gate succeeds:

1. Windows reports a compatible NVIDIA adapter, driver, VRAM, and compute capability.
2. The managed WSL machine exposes `/dev/dxg` and its projected NVIDIA driver libraries.
3. The exact signed `nvidia-container-toolkit-base` RPM installs and verifies at the pinned NEVRA.
4. `nvidia-ctk cdi list` exposes `nvidia.com/gpu=all` through Podman.
5. The digest-pinned CUDA image starts with `--device nvidia.com/gpu=all` and explicit llama.cpp GPU-layer offload.
6. Diagnostics identify the expected physical GPU, CUDA backend, and offloaded model layers.
7. The real OpenAI-compatible embeddings endpoint returns one finite vector with the pinned 768 dimensions.

Each failed gate returns a typed, actionable error. LCTK never starts the CPU image as a fallback while the persisted or requested distribution is NVIDIA GPU.

Activation uses a candidate-and-swap transaction. LCTK starts the selected image under a temporary managed name on the runtime network, performs health, backend, and embedding checks, and records the current container's complete user-defined network attachments and aliases. Only then does it move the old container to its rollback name, disconnect its project networks to prevent duplicate DNS aliases, move the candidate to the final name, and reconnect the exact recorded project networks with their aliases. Any swap or reconnection failure removes the candidate, reconnects and renames the previous container, proves its health, and leaves the previous selection active. Unexpected inspect output is fatal; it is never treated as absence.

Changing from NVIDIA GPU to CPU changes the selected image, device injection, runtime arguments, and reported backend. It does not uninstall the verified CDI package or delete either cached image. Retaining immutable cached components makes repeat switching offline-capable and avoids mutating the managed machine's base packages during a distribution change. Complete uninstall still removes the installation-owned Podman machine and therefore all CDI, image, model, and selection state.

An ordinary update installs and verifies all signed host artifacts, the selected inference distribution, and any code image before stopping a project or activating the new host version. The update continues the current distribution unless setup explicitly records a change. Rolling back to `0.1.11` makes its older core use the CPU distribution because that core does not read `inference.json`; the selection and verified GPU components remain inert and resume on a later compatible update.

Admin reports requested distribution, actual backend, immutable image identity, readiness, and typed failure. For NVIDIA GPU it also reports adapter name, driver, VRAM, compute capability, CDI device identity, CUDA backend, and offloaded-layer evidence. It must not infer GPU readiness solely from the selected value or container image name.

The CPU distribution keeps llama.cpp physical and logical batches at `4096`. The NVIDIA GPU distribution uses `8192` for both values while retaining eight parallel slots and the 32,768-token aggregate context. This is a distribution-specific execution profile, not an automatic fallback or an image change. On the Pascal acceptance floor, smaller physical batches left the GPU idle between CPU-fed bursts; the larger pinned batch materially increased sustained CUDA occupancy without exceeding the supported adapter's memory.

Every NVIDIA status observation also performs a live, in-container `nvidia-smi` sample bound to the already verified CUDA container. The public owner status includes sample time, GPU utilization, used and total VRAM, power, and temperature. Static CUDA initialization and complete layer offload prove configuration; these live values separately expose whether tensor computation is happening now. Malformed or unavailable live telemetry is a status failure rather than an invented zero.

The trace-only `offloaded N/N layers to GPU` result is captured immediately after the real embedding self-test and persisted as derived owner-only evidence in `inference-evidence.json`. Each record is bound to the container ID, immutable image ID, inference configuration revision, distribution, and exact process start time. Reusing a container may reuse only an exact matching record; a restart changes the start time and requires fresh startup proof. A bounded four-record document retains both candidate and rollback evidence across an activation transaction. The GPU container uses a container-scoped `k8s-file` log capped at 32 MiB so trace diagnostics cannot grow the private machine's shared journal without bound.

## Alternatives considered

### Replace the CPU distribution with CUDA everywhere

Rejected. Machines without a compatible NVIDIA adapter remain supported, and a CUDA image cannot provide an honest CPU fallback contract.

### Install the full NVIDIA Container Toolkit from a live repository

Rejected. Podman needs CDI generation, not the legacy Docker runtime hook. The full package adds unused dependencies, and a live repository would make repeatability depend on mutable external state.

### Store the distribution in the existing host settings schema

Rejected. A schema bump would make the verified `0.1.11` rollback target reject the settings document.

### Remove GPU components when CPU is selected

Rejected. Removal adds rollback risk and prevents an offline repeat switch without improving runtime isolation; CPU activation already omits CDI devices and GPU offload arguments.

### Replace the running inference container in place

Rejected. The current container can serve multiple project networks. Destructive replacement before candidate proof can lose network attachments and leaves no verified rollback target.

### Treat a healthy CUDA container as GPU proof

Rejected. A CUDA-capable image can start without actually offloading model layers. Acceptance requires backend diagnostics plus a real embedding result.

## Consequences

### Positive

- CPU remains explicit and fully supported.
- NVIDIA acceleration is observable, reproducible, and cannot silently degrade to CPU.
- Distribution switches and updates preserve running project network topology or restore the previous service.
- Installed immutable components support offline repair and repeat switching.
- `0.1.11` remains a readable rollback target without compatibility shims in its settings schema.

### Negative

- A complete release carries and verifies two inference image identities and one additional RPM artifact.
- The CUDA image requires substantially more download and storage than the CPU image.
- GPU readiness has host, WSL, CDI, container, backend, and model gates, each of which must remain diagnosable.
- A rollback to `0.1.11` temporarily operates CPU inference even when `inference.json` retains NVIDIA GPU for a later compatible version.

### Acceptance evidence

The follow-up gates were completed on the Windows 10 acceptance machine with local RC `0.1.12` built from commit `954da54e484ca87d50c32ef3f12b808c6a72c6fa`. Native setup upgraded installed `0.1.11` to the NVIDIA GPU distribution, an explicit rollback restored `0.1.11` with CPU inference and its own daemon, and a final native update restored `0.1.12` with the persisted GPU selection. The repeat GPU plan required zero download bytes because the signed RPM, model, and both image identities were retained locally.

The final runtime measured the GeForce GTX 1070, driver `582.53`, 8,192 MiB VRAM, compute capability 6.1, exact RPM `nvidia-container-toolkit-base-1.19.1-1.x86_64`, CDI device `nvidia.com/gpu=all`, CUDA image digest `sha256:37dd122824e58af9ec861955242abdeeade5a1dcf0ad768bf2b37f903c2805c6`, and complete `13/13` layer offload. The installed Admin reported `inference cuda NVIDIA GeForce GTX 1070 | Ready.`

A fresh full build embedded 2,568 chunks from 425 files with zero semantic reuse in 755.588 seconds. GPU telemetry sampled throughout the operation reached 100% utilization and 1,672 MiB. The earlier CPU baseline embedded 1,169 chunks from 356 files in 1,836 seconds, so the GPU run used a 2.197-times larger and non-identical corpus; elapsed time was 2.430 times shorter and observed chunk throughput was 5.338 times higher. These measurements characterize this machine and are not a general performance guarantee. Detailed artifact, transaction, indexing, and OAuth evidence is recorded in [compatibility.md](../compatibility.md) and [the completed dry run](../spikes/nvidia-gpu-inference-installer-dry-run.md).
