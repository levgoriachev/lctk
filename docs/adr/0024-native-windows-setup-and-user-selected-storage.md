# ADR-0024: Native Windows setup and user-selected storage

- Status: accepted
- Date: 2026-08-05
- Deciders: project maintainers
- Amends: [ADR-0013](0013-registry-persistence.md) and [ADR-0023](0023-managed-podman-wsl-runtime-and-windows-installer.md)

## Context

The first setup implementation exposed its plan through an authenticated HTTP server bound to a random loopback port and opened the system browser. The endpoint was private to the local process, but the visible `127.0.0.1` address looked like a web application rather than an installed Windows product. The setup also selected `%LOCALAPPDATA%\lctk` and Podman's default WSL data location without showing either path. Users could not choose a larger drive before the managed virtual disk, OCI images, volumes, and project indexes were created.

The setup must remain one executable with no Docker Desktop, WebView runtime, JavaScript runtime, framework installer, or build tool prerequisite. Podman 5.8 resolves its WSL distribution directory beneath `XDG_DATA_HOME/containers/podman/machine`; its WSL provider imports the distribution into that resolved directory. Microsoft documents the WSL import location as the supported placement boundary for a distribution.

## Decision

The Windows setup uses a native Win32 window and controls only. It does not start an HTTP listener or open a browser. The window presents the authenticated release identity, host/runtime plan, installation directory, runtime-data directory, download size, progress, failure detail, and completion state.

The user may edit or browse for two absolute directories before accepting the plan:

1. **Installation directory.** Owns the stable launcher, versioned host cores, private Podman client and source archive, embedding model, credentials, registry, logs, and other LCTK host state.
2. **Runtime-data directory.** Is passed exclusively to LCTK's private Podman processes as `XDG_DATA_HOME`. Its Podman-owned descendants contain the managed WSL virtual disk, OCI image layers, named project volumes, indexes, and project memory.

The selected layout is stored in the current user's Windows registry under `HKCU\Software\LCTK`. Every setup, launcher, host core, daemon, update, diagnostic, and uninstall operation resolves the same values. Explicit `LCTK_HOME` and `LCTK_RUNTIME_DATA_HOME` environment overrides remain higher-priority automation and test boundaries.

The fresh Windows default for runtime data is `%LOCALAPPDATA%\lctk-runtime-data`, not Podman's ambient `%USERPROFILE%\.local\share`. This keeps LCTK's private machine and heavy data out of a directory that an unrelated Podman installation may already use. The value remains fully editable before the managed machine exists.

Setup normalizes both paths, rejects empty values and drive roots, recalculates the read-only plan against the selected volumes, and persists the locations only after the user accepts that plan. A fresh sparse WSL machine requires at least 4 GiB free in the selected runtime-data location for the imported base, initial images, and an operational margin; an existing machine reports current free space without pretending its future project growth is known. The private Podman command builder replaces any ambient `XDG_DATA_HOME` in each child process, so a shell or unrelated Podman installation cannot redirect the managed machine.

An existing activated installation and managed WSL machine stay at their recorded locations. Setup does not silently migrate either boundary. Moving a WSL distribution requires an explicit export, unregister, and import transaction with a separately reviewed preservation contract; selecting new directories is therefore allowed only before the corresponding installation or `lctk-runtime` machine exists.

## Alternatives considered

### Keep the loopback browser setup

Rejected. The network boundary was local and authenticated, but the visible browser address did not satisfy the native installed-product experience and could not provide a standard Windows folder-selection flow.

### Embed Edge WebView2

Rejected. It would replace the external browser with another web runtime and make availability or installation of that runtime part of the bootstrap contract.

### Add a third-party desktop GUI framework

Rejected. The setup surface needs a small fixed set of native controls. Shipping a framework or adding a framework-specific runtime is unnecessary product and supply-chain scope.

### Redirect Podman through directory junctions

Rejected. Junctions hide the authoritative storage location, complicate removal and recovery, and are unnecessary because Podman and WSL already expose supported location contracts.

### Silently move an existing WSL machine

Rejected. The required unregister step is destructive if export or re-import validation is incomplete. Relocation needs its own explicit transaction and is not equivalent to choosing a location before first installation.

## Consequences

- Setup looks and behaves like a Windows application without opening a browser.
- Users can place the large sparse WSL disk and all container-managed project data on a chosen drive before installation.
- LCTK remains a single downloaded executable with no additional GUI runtime.
- The Windows-specific UI contains direct Win32 integration that must be exercised on the supported Windows baseline.
- Changing the runtime-data directory after the managed machine exists is refused until an explicit migration contract is implemented.
