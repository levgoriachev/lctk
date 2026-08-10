# ADR-0025: Native Windows administration and complete product uninstall

- Status: accepted
- Date: 2026-08-06
- Deciders: project maintainers
- Amends: [ADR-0016](0016-admin-surface-and-local-session.md), [ADR-0023](0023-managed-podman-wsl-runtime-and-windows-installer.md), and [ADR-0024](0024-native-windows-setup-and-user-selected-storage.md)

## Context

The installed product still opened its administrator surface in the default browser at a visible `127.0.0.1` address. Although ADR-0016 protected that page with an independent one-time session, loopback host validation, and CSRF authorization, it did not look or behave like a Windows application. The setup executable was already a native Win32 GUI and already had to remain installed as the registered uninstaller.

Windows Apps & Features had an uninstall entry, but the Start menu did not expose an uninstall shortcut. The removal transaction stopped the daemon, optionally exported project volumes, removed the named Podman machine, unregistered desktop integration, and removed the installation home. Its contract did not explicitly remove named Podman residue from both the selected runtime-data tree and Podman's per-user machine configuration tree.

## Decision

The installed `lctk-setup.exe` owns two native GUI modes in addition to setup:

- `--admin` opens the complete administrator window;
- `--uninstall` opens the explicit preservation/removal choice and executes removal.

The Start-menu `LCTK` shortcut targets `lctk-setup.exe --admin`. `lctk admin open` starts that same installed GUI process. Neither path opens a browser, starts a WebView, or serves an HTML page. The stable `lctk.exe` remains a console program for scripts and automation.

The native administrator is a client of the daemon's existing loopback Admin API. It exchanges the daemon's owner-only one-time code directly in process, keeps the session credential and CSRF token only in memory, and supports project registration, project lifecycle, reindexing, resource mode, MCP connection instructions, OAuth request approval or denial, client revocation, runtime diagnostics, recent logs, refresh, and uninstall. Project OAuth tokens remain structurally unable to authorize Admin API requests, and credentials are never returned to the window.

Setup registers all of the following per-user Windows integration:

- sign-in daemon startup;
- `LCTK` Start-menu shortcut;
- `Uninstall LCTK` Start-menu shortcut;
- the Apps & Features uninstall entry, including install location and disabled modify/repair actions.

Uninstall shows the recorded installation and runtime-data locations before mutation. It stops the daemon, optionally exports registered project state, removes the exact `lctk-runtime` Podman machine, removes only LCTK-named machine configuration and runtime residue, removes empty shared Podman parent directories only when no unrelated data is present, unregisters startup/shortcuts/Apps & Features, clears `HKCU\Software\LCTK`, and removes the installation home. Neighboring Podman machines and non-empty shared directories are never removed.

The GUI process starts its console-subsystem implementation helpers with `CREATE_NO_WINDOW` and `HideWindow`. This applies to WSL and DISM probes, the private Podman client, host-core verification and bootstrap, background daemon operations, and internal administrator commands. It does not suppress UAC consent, the setup/admin/uninstall windows, the editor selected by the user, or interactive use of the public `lctk.exe` CLI.

Desktop removal is idempotent after a partial attempt. The per-user Programs directory is resolved first through `FOLDERID_Programs` and then through Explorer's `User Shell Folders\Programs` registry contract if the known-folder API transiently fails. Startup, Start-menu, Apps & Features, runtime-residue, and installation-home cleanup are independent after the managed machine has been removed: every exact target is attempted and any failures are reported together.

When the installed uninstaller itself is the final locked file, it starts one hidden, non-interactive system cleanup process. That process waits for the native result dialog to close, retries deletion only after the uninstaller exits, and has no console window or script file. A removed sole machine's generic Podman client scaffold is deleted only when a strict allowlist proves that no unknown machine, image, volume, or storage path shares the selected data root. The selected runtime-data root itself is removed only when it is empty after that cleanup.

Recovery from a partial removal may find that the private Podman client is already gone. This is treated as an absent managed machine only when the system WSL inventory independently confirms that the exact `lctk-runtime` distribution no longer exists; a remaining distribution or an unreadable inventory fails closed.

Local acceptance uses setup and recovery release-candidate bootstrapper modes built by `scripts/build-local-rc.ps1`. A ZIP containing the locally signed manifest plus the candidate setup, core, and launcher is appended to both native executables. The setup mode opens the complete installation transaction and serves only those four files on numeric loopback for the lifetime of the real native process. The recovery mode opens the candidate uninstaller directly for an existing partial installation and never opens a network listener because removal consumes no package artifacts. Both modes remove their extraction directory afterwards. Setup artifact URLs may use plain HTTP only for numeric loopback; the Ed25519 signature, byte length, and SHA-256 identity remain mandatory. Official publication continues to generate HTTPS artifact URLs exclusively. This permits complete local install and recovery candidates without creating a GitHub release or weakening production trust.

## Alternatives considered

### Keep the browser Admin UI

Rejected. Its security boundary was defensible, but it did not meet the required installed Windows application experience.

### Embed WebView2

Rejected. It would retain a browser application model and introduce a runtime availability boundary that the native setup intentionally avoids.

### Publish a separate administrator executable

Rejected. The installed GUI setup binary already supplies the required Windows subsystem, release verification, and uninstall lifetime. A fourth release executable would add manifest, download, update, and signing inventory without adding an independent product boundary.

### Delete the complete selected runtime-data directory

Rejected. The user selects that directory and it may contain unrelated data. Removal is limited to the exact LCTK machine identity and empty Podman-owned parents.

## Consequences

- Installing and operating LCTK no longer opens a browser.
- The CLI remains usable from a terminal without being compiled as a GUI subsystem.
- The administrator UI and uninstaller share the already installed native executable and require no additional runtime.
- Native GUI operations do not flash implementation-detail terminal windows.
- A normal removal path is available in both Apps & Features and the Start menu.
- Real one-file local setup and recovery RCs can exercise install and partial-uninstall repair before any GitHub publication.
- ADR-0016 remains historical evidence for the Admin API's independent credential boundary, but its HTML page, URL delivery, and browser-specific UI conclusions no longer describe the product.
