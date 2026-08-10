# ADR-0028: Authenticated local code-image artifacts

- Status: accepted
- Date: 2026-08-06
- Deciders: project maintainers
- Amends: [ADR-0022](0022-transactional-bootstrap-update-and-release-evidence.md) and [ADR-0027](0027-native-setup-in-place-upgrade-and-repair.md)

## Context

The local one-file release-candidate builder replaced the Windows setup, launcher, and core artifacts while retaining the published template's code-intel image. That was sufficient for host-only changes, but a candidate containing code-intel source changes could present a newer unified product version while installing the older published Linux service. Manually retagging a development image after setup would bypass the signed update transaction and produce acceptance evidence for component bytes that the candidate manifest did not bind.

Official releases solve this through a digest-addressed registry image. A local RC must remain unpublished, self-contained, and usable by native setup against LCTK's remote private Podman connection. A host file path cannot be passed to remote `podman load --input`, because the path would be resolved inside the managed Linux machine.

## Decision

When the local RC contains code-intel changes, its builder runs the production Dockerfile through LCTK's explicit private Podman connection, saves the resulting `linux/amd64` image as a Docker archive, and appends that archive to the one-file candidate. Docker format is required because it preserves the image healthcheck; the project runtime also supplies the same explicit health policy when it creates a container. Because Docker archive import normalizes the manifest, the builder streams the completed archive back through the same remote `podman load` boundary used by setup and signs the deterministic post-load manifest digest rather than the pre-save digest. The locally signed manifest replaces the template's code-image identity and adds one `code-image-archive/linux/amd64` artifact.

The update coordinator selects the archive only when that exact optional artifact identity is present. It downloads the bytes from the candidate's numeric-loopback package server, verifies the signed byte count and SHA-256, and streams the verified file to `podman load` over stdin. It then inspects the loaded product-version tag and requires its image manifest digest to equal the signed `code_image` digest before any project is stopped or started. The temporary archive is stored beneath the installation home and removed after the load attempt.

Official manifests without the optional archive continue to pull the immutable registry reference. The local path does not add a mutable-reference fallback, a manual tag override, or a second update transaction.

## Alternatives considered

### Keep the published code image in local RCs

Rejected. Host and Linux service components could claim one version while executing different source generations.

### Preload or retag an image manually before setup

Rejected. The installer would depend on undocumented machine state, and the signed candidate would not authenticate the operation that created the runnable tag.

### Publish every development candidate to GHCR

Rejected. Local acceptance must not create a public release artifact, tag, or registry history.

### Pass the Windows archive path to remote Podman

Rejected. The remote service resolves input paths in the managed Linux machine, outside the verified host download boundary.

## Consequences

- A local candidate now contains the exact Windows and Linux components built from one commit.
- Archive transport and runnable image content have independent signed integrity checks.
- The one-file local setup grows by the compressed code-intel image size.
- Official registry-based updates are unchanged.
- Local RC construction now requires the installed private Podman runtime and a successful production image build.
