# ADR-0023: Managed Podman WSL runtime and one-click Windows installer

- Status: accepted
- Date: 2026-08-05
- Deciders: project maintainers
- Amends: [ADR-0003](0003-reusable-images-and-project-stacks.md), [ADR-0008](0008-platform-and-ci-baseline.md), [ADR-0009](0009-embedded-go-gateway-and-project-runtime.md), and [ADR-0022](0022-transactional-bootstrap-update-and-release-evidence.md)

## Context

The implemented product requires users to install Docker Desktop, build a development image when no official release exists, start a foreground daemon, register projects through the CLI, and configure Codex separately. That proves the architecture but does not satisfy the accepted product requirement that another user can install and operate LCTK without a development toolchain.

The Linux boundary remains mandatory. Zoekt's selected low-level package is Unix-specific, Tree-sitter is compiled with cgo for the Linux service, and the approved-command runner depends on Linux namespaces, cgroups, mount isolation, and an explicit network policy. Removing that boundary would require replacing verified subsystems and designing a new Windows sandbox.

Docker Desktop cannot be silently redistributed as an LCTK component. It is a separately licensed user-facing product, and retaining it would leave installation, startup, updates, and failures outside the LCTK lifecycle.

Windows provides WSL2 as the supported Linux virtualization boundary. Podman supplies an Apache-2.0 headless Windows remote client and a WSL2-backed machine that runs OCI containers without Podman Desktop. Its official release publishes immutable Windows and WSL machine artifacts with SHA-256 identities.

## Decision

The first one-click product and official publication target is Windows 10 22H2 or newer on amd64. LCTK owns the complete user-facing lifecycle while using pinned headless Podman components as an internal runtime implementation. macOS remains a development and hosted-CI compatibility target; it has no official package and cannot block a Windows release. Adding macOS to the official release inventory requires a separately accepted platform-runtime and distribution decision.

The Windows installation contract is:

1. One unsigned LCTK setup executable performs a read-only plan before any mutation. Windows may identify its publisher as unknown or show a SmartScreen warning; this is an accepted initial open-source distribution constraint, not an installation integrity signal.
2. The setup verifies the host version, architecture, virtualization state, WSL2 availability, free space, release signature, component sizes, and SHA-256 digests.
3. When Windows requires WSL or Virtual Machine Platform enablement, setup requests elevation, reports whether a reboot is required, and resumes the same transaction after restart. It never reports a ready installation before WSL2 is usable.
4. Setup installs the versioned LCTK launcher and core, downloads and verifies the pinned portable Podman client and official WSL machine image, and initializes exactly one managed machine named `lctk-runtime`.
5. Podman Desktop is not installed. LCTK invokes its private Podman client by absolute path and never depends on `PATH`, a default Podman connection, or a user-managed machine.
6. Project services remain reusable OCI images with isolated project networks, volumes, and read-only source mounts. They publish no WSL ports. The Windows host reaches each private container address through an authenticated, process-owned SSH tunnel bound to a dynamically allocated Windows loopback port. Compose is removed from the host lifecycle; LCTK issues explicit, deterministically named Podman operations through its owned runtime adapter.
7. The approved-command runner uses the same managed runtime but a separate ephemeral container with its existing writable-mount, process, memory, CPU, network, timeout, and audit contracts.
8. The shared embedding service runs inside the managed runtime and remains installation-wide stateless compute. It joins each requesting project's isolated network under the fixed `lctk-inference` alias; project containers never join one another's networks. Host health checks use the same authenticated machine tunnel, so Docker Desktop and another WSL distribution cannot capture LCTK traffic through the distribution-global localhost relay. Project semantic state remains isolated in project volumes.
9. A per-user background daemon starts at sign-in. A Start-menu launcher starts or reconnects to it and opens the authenticated local Admin UI.
10. The setup UI owns bootstrap progress. The installed Admin UI owns project-folder registration, project lifecycle, MCP connection instructions, OAuth approval and revocation, diagnostics, and the uninstall choice. The IDE owns its configuration and process lifecycle. Routine project use requires no shell; signed update and rollback remain explicit automation surfaces.
11. Uninstall removes LCTK executables, startup registration, the managed Podman client, and the managed WSL machine. It preserves project indexes and memory only when the user explicitly selects that option.
12. `lctk bootstrap`, lifecycle commands, and JSON diagnostics remain supported automation surfaces, but the setup and Admin UI are the primary user path.

The runtime provider, portable client, WSL machine image, code-intel image, inference image, model, host core, and setup executable are immutable release components. The signed release manifest binds every version, URL, byte length, and SHA-256 digest. ADR-0022 continues to govern host-core, project-image, and project-schema update and rollback.

Official Windows executables are deliberately distributed without Authenticode. Their release identity comes from the protected version tag, GitHub artifact attestations, published checksums, the launcher's embedded core digest, and the Ed25519-signed component manifest. Absence of a Windows certificate is explicit release policy rather than a silently skipped signing gate.

Docker remains permitted in repository CI to build and execute OCI artifacts on Linux runners. It is not a Windows end-user dependency or a supported production runtime after this migration.

## Alternatives considered

### Bundle or automatically install Docker Desktop

Rejected. It retains a separately licensed user-facing product, an independent updater and daemon, and a lifecycle LCTK cannot make atomic with its own installation.

### Fully native Windows code intelligence and runner

Rejected. It breaks the accepted Zoekt Linux boundary and would require a new command-isolation design before it could preserve the verified security contract.

### A custom LCTK Linux distribution with a custom container engine

Rejected. Maintaining a kernel-facing OCI engine, networking stack, image store, and security update channel is not an LCTK product capability. Podman already supplies that bounded implementation behind LCTK-owned contracts.

### Podman Desktop

Rejected. It replaces one separately managed desktop application with another. Only the portable headless client and managed WSL machine are required.

### Mandatory Authenticode signing

Rejected for the initial open-source release path. A publicly trusted certificate has recurring cost or depends on acceptance by an external sponsorship service. It does not replace the component identity contract already enforced by the Ed25519 manifest, digests, launcher binding, and GitHub provenance. Authenticode can be reconsidered only through a new accepted ADR; it is not a hidden release prerequisite.

### One combined Windows and macOS release gate

Rejected for the initial release. The accepted one-click runtime exists only on Windows, while a distributable macOS package would introduce a separate runtime decision plus paid Apple signing and notarization credentials. Keeping those jobs in the same publication graph would prevent the completed Windows product from being released. macOS build and test evidence remains in ordinary CI without becoming a claimed package.

### MSIX-only installation

Rejected as the sole bootstrap layer. Enabling Windows optional features, coordinating elevation and reboot, and resuming a multi-component transaction require a dedicated bootstrap executable. MSIX may later package the already-provisioned desktop payload, but it cannot own the complete first-run contract.

## Consequences

### Positive

- A user needs no Go toolchain, Docker Desktop, Podman Desktop, image build, or shell workflow.
- The verified Linux and OCI boundaries remain intact.
- LCTK owns runtime identity, diagnostics, installation, removal, and UI.
- Podman is replaceable behind the existing LCTK lifecycle contracts.
- The installation can remain small by downloading signed, digest-pinned components after the user approves the plan.
- Public Windows releases do not require a paid certificate or external signing sponsor.
- Windows publication cannot be blocked by an unimplemented or uncredentialed macOS package.

### Negative

- A machine without WSL2 may require administrator approval and one reboot.
- Windows can show an unknown-publisher or SmartScreen warning, and managed corporate devices may refuse the unsigned executable by policy.
- macOS users do not receive an official package under this release contract.
- Windows installation adds a third-party runtime and WSL machine update responsibility.
- The existing Compose-specific lifecycle and Docker diagnostic must be replaced rather than wrapped.
- Fresh-machine acceptance needs a Windows host with virtualization; hosted CI alone cannot certify it.
- macOS remains on the previous Docker Desktop development path until a separately accepted runtime decision is implemented and measured.

### Follow-up

- Implement a fail-fast Podman runtime adapter and migrate project, runner, inference, disk, and diagnostic paths.
- Extend the signed release manifest with portable runtime and WSL machine components.
- Implement the setup transaction, sign-in startup, first-run Admin UI, and explicit uninstall preservation choice.
- Publish Podman attribution and all transitive notices required by the downloaded runtime artifacts.
- Verify the unsigned installer warning, install, reboot continuation, first project, all MCP tools, update, rollback, and uninstall on a clean Windows 10 22H2 amd64 host without Docker Desktop, Go, or Git.
