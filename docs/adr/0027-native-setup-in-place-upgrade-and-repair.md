# ADR-0027: Native setup in-place upgrade and repair

- Status: accepted
- Date: 2026-08-06
- Deciders: project maintainers
- Amends: [ADR-0022](0022-transactional-bootstrap-update-and-release-evidence.md), [ADR-0023](0023-managed-podman-wsl-runtime-and-windows-installer.md), [ADR-0024](0024-native-windows-setup-and-user-selected-storage.md), and [ADR-0025](0025-native-windows-admin-and-complete-uninstall.md)

## Context

The native setup detected an existing activation and managed WSL machine, locked their recorded locations, and told the user that setup would continue them in place. Its coordinator nevertheless always executed bootstrap. Bootstrap deliberately refuses to replace a different active host version, so a newer setup could not update an installed product. The user had to uninstall first, which deleted or exported persistent state and turned a routine product update into a destructive recovery workflow.

The CLI already implemented the required signed update transaction: read-only compatibility and disk preflight, candidate image installation, health-gated migration of every previously running project, reverse-order restoration on failure, and atomic host-core activation last. Duplicating those rules in setup would create two update implementations with different failure behavior. Local release-candidate installers also use an ephemeral Ed25519 trust root per build, so an older locally installed core cannot authenticate a newer local candidate; the new setup has already authenticated that candidate and must own the transaction.

## Decision

The one-file native setup supports four strict outcomes after it authenticates the release manifest and loads the recorded activation:

1. **Install:** no activation exists. Setup performs the complete bootstrap transaction and records the selected locations.
2. **Upgrade:** the manifest version is numerically newer than the active version. Setup preserves the recorded installation and runtime-data locations and applies the shared update transaction in place.
3. **Repair:** the manifest version equals the active version. Setup verifies and restores the exact immutable release components without changing the previous-version rollback pointer.
4. **Refuse downgrade:** the manifest version is older than the active version. Setup writes nothing and directs version reversal through verified rollback.

Every distributed setup executable carries a higher Semantic Versioning product version when its component identity changes. Reusing a version for different host-core bytes is rejected as an immutable-identity violation. Re-running the exact same executable remains useful because repair can complete an interrupted desktop or runtime registration without deleting project data.

Setup and `lctk update` call one internal update coordinator. Manifest signature verification remains at the invoking surface; the coordinator receives only the authenticated manifest. This permits local RC upgrades across ephemeral development trust roots without asking an old installed binary to trust a new key and without weakening production signature verification.

Before upgrade or repair, setup stops only the recorded installation-owned daemon. A stale daemon PID is checked against the Windows process inventory: an absent PID removes only the obsolete state document, while an existing process that cannot be identity-verified fails closed and is never terminated by guesswork. Upgrade then applies the candidate code image to every project that was running at preflight, accepts each candidate through its bounded health gate, and atomically activates the verified versioned host core last. Setup verifies or repairs the pinned private runtime, runs the complete bootstrap component and inference self-test, atomically replaces the stable launcher and setup files, refreshes Start-menu, sign-in, and Apps & Features metadata, and starts the daemon through the stable launcher.

The installation directory, runtime-data directory, managed `lctk-runtime` WSL identity, registry, project registrations, indexes, project memory, settings, MCP OAuth approvals, and client credentials are preserved. Setup never relocates them during update. Already verified immutable runtime downloads and model bytes are reused rather than downloaded again.

If candidate project migration or host activation fails, the shared update coordinator restores migrated projects in reverse order. If a later runtime self-test, desktop activation, or daemon start fails after host activation, setup invokes the same verified rollback transaction with an independent bounded recovery context and restarts the previous daemon. Stable launcher and setup files are individually activated only after their complete signed bytes are present; they remain version-independent selectors of the activation document, so a restored previous core remains launchable even if a later setup binary was already installed.

The setup window explicitly says **Install**, **Update**, or **Repair**, shows the current and target versions for upgrade, states that persistent state is preserved, and asks for confirmation before mutation. A version or activation change while the window is open invalidates the reviewed plan and requires reopening setup.

## Alternatives considered

### Require uninstall before every update

Rejected. Uninstall owns product removal, managed-machine deletion, and optional project export. It is the wrong transaction for preserving a working installation.

### Publish a separate updater executable

Rejected. It would add another signed artifact, GUI, lifecycle, and recovery implementation while setup already owns component acquisition and user confirmation.

### Let setup call the old installed `lctk update`

Rejected. Local RC builds intentionally use ephemeral trust roots, so an older local core cannot authenticate the next local manifest. It would also leave setup unable to coordinate later desktop/runtime failures with the already completed host transaction.

### Treat the same version as already installed and exit

Rejected. A power loss or late registration failure can leave verified host activation present while a launcher, setup file, shortcut, or registration still needs repair. Exact same-version repair is idempotent and preserves rollback history.

### Silently install an older setup package

Rejected. Downgrade may require schema and image rollback. Only the explicit verified rollback transaction owns those changes.

## Consequences

- Users install a newer setup over an existing LCTK without uninstalling or selecting storage again.
- Project data, MCP authorization state, and the managed WSL disk remain in place.
- CLI and GUI updates share one compatibility, health, activation, and rollback implementation.
- Local one-file RCs can exercise real upgrades before any GitHub publication.
- Every changed installer build requires a new product version and immutable component identity.
- Same-version repair is supported; silent downgrade and location migration remain forbidden.
