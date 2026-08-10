# Releasing

## Development artifacts

Before publishing, `scripts/build-local-rc.ps1` creates unsigned `.artifacts/LCTK-Setup-local-RC.exe` and `.artifacts/LCTK-Uninstall-local-RC.exe` files from the current committed source. It verifies the published template manifest; rebuilds the `linux/amd64` code-intel image through LCTK's private Podman connection; saves and test-loads the Docker archive to derive its deterministic post-load digest; downloads the exact NVIDIA CDI RPM and verifies its pinned size and SHA-256; replaces the setup, core, launcher, code-image, CUDA-image, and CDI-package identities; signs the candidate manifest with an ephemeral local Ed25519 key; and appends the verified code archive and RPM to both native bootstrapper modes. The setup candidate opens the complete installation transaction and exposes its appended artifacts on numeric loopback while that transaction is active. The updater verifies both archive SHA-256 and loaded image manifest digest before a project migration, while GPU setup verifies the RPM again before streaming it into the private machine. The recovery candidate opens the new uninstaller directly without a listener so a partially removed older installation can be cleaned without reinstalling its runtime or replacing files outside the repository. Neither mode creates a tag or GitHub Release. These are local acceptance artifacts, not publication artifacts. See [ADR-0028](adr/0028-authenticated-local-code-image-artifacts.md) and [ADR-0029](adr/0029-selectable-cpu-and-nvidia-gpu-inference.md).

`Dry-run release artifacts` is a manually triggered workflow. It builds the stable launcher and versioned host core for Windows amd64 and macOS arm64, packages the legal notices, extracts each archive, executes `lctk version --json`, generates checksums, and uploads workflow artifacts. Separate native Ubuntu amd64 and arm64 jobs run the production image test target, build the runtime image, verify its platform identity, start it with a read-only workspace, and require container health. Its `-dev` version, missing embedded production trust root, and incomplete publication inventory make it unsuitable for an official release.

[CI run 31012552276](https://github.com/lev-goryachev/lctk/actions/runs/31012552276) and [artifact run 31012555479](https://github.com/lev-goryachev/lctk/actions/runs/31012555479) executed the final matrices at commit `7679060` on 2026-08-05. Windows, macOS, and Linux tests passed; both extracted host archives reported the intended OS and architecture; both native image jobs reported `healthy`; and the amd64 image test included the race detector. The artifact run preserves separate machine-readable host identity and image inspect/health JSON evidence.

The downloaded `0.1.0-stage7-final3` archives independently matched both entries in the published `SHA256SUMS`. The Windows archive was then extracted again on Windows 10 build 19045 with an isolated installation home. Its packaged launcher executed its digest-bound sibling core and reported commit `7679060`, OS `windows`, and architecture `amd64`. This is target-machine dry-run evidence; it predates the complete tagged one-click publication inventory.

## Official release boundary

An official release is created only by pushing a clean `vMAJOR.MINOR.PATCH` tag. The protected `Official release` workflow:

1. derives the Ed25519 public trust root from the protected private key and embeds the public key, key id, and release-manifest URL in the Windows host core and setup;
2. runs the complete host test suite, then builds the stable launcher, versioned host core, and native setup/admin/uninstall executable for Windows amd64;
3. binds the stable Windows launcher to the exact unsigned core size and SHA-256 digest;
4. extracts the Windows archive and executes its packaged launcher on the hosted Windows architecture;
5. tests, builds, and executes each code-intel architecture on its matching native Ubuntu runner, combines the two immutable manifests, verifies the index platforms, and keyless-signs that index with Sigstore;
6. mirrors the pinned Podman `5.8.2` Windows client, WSL machine image, and `nvidia-container-toolkit-base 1.19.1-1` RPM only after exact upstream size and SHA-256 verification;
7. measures the pinned CUDA image transport size, then generates SPDX JSON SBOMs, module attribution, checksums, migration and rollback notes, and a schema-2 signed component manifest binding the setup, launcher, core, runtime, CPU and NVIDIA GPU images, CDI package, and model;
8. creates GitHub artifact attestations for the publication set; and
9. publishes the GitHub Release only after every preceding job succeeds.

Publication fails closed when any Ed25519, GHCR, signature, package-execution, SBOM, checksum, or attestation gate is unavailable or invalid. Windows Authenticode is intentionally absent under ADR-0023 rather than skipped conditionally. macOS is not part of the official release inventory and has no release credential or job.

The two image architectures are built on `ubuntu-24.04` and `ubuntu-24.04-arm`, not through QEMU. Local measurement found that emulated arm64 cgo compilation did not complete, while a glibc cross-build produced a static-linker NSS warning incompatible with an Alpine runtime claim. Both alternatives were rejected. Native jobs preserve the existing static musl/Alpine boundary and execute the actual architecture before its digest can enter the combined index.

## Protected configuration

The release environment must provide:

- `LCTK_RELEASE_ED25519_PRIVATE_KEY`: base64 Ed25519 seed or private key.

No private key is committed or uploaded as an artifact. The protected Ed25519 value is scoped only to the steps that derive the public trust root and sign the release manifest, so checkout, build, SBOM, artifact, attestation, and publication actions never receive it.

The initial official publication is Windows amd64 only. The ordinary CI and dry-run artifact workflows continue to build and test macOS arm64 for portability evidence, but no macOS file enters a GitHub Release or the signed component manifest.

## Installation and update contract

An extracted package starts through the stable `lctk` launcher. Before initial activation it may execute only its sibling packaged `lctk-core`; a successful `lctk bootstrap --yes` copies that exact core into the versioned installation store and writes the first activation document.

Official setup, bootstrap, and update resolve the signed `release-manifest.json`, validate the embedded trust root, immutable image digests, artifact sizes and SHA-256 digests, model identity, host minimum, and project-schema range. Windows releases at 0.1.12 or newer additionally require the complete launcher, installer, Podman client, WSL machine, NVIDIA CDI RPM, pinned CUDA image, and unpacked CUDA storage evidence. Historical schema-2 manifests remain readable for verified 0.1.11 rollback.

Native setup classifies the authenticated manifest as install, in-place upgrade, or same-version repair. Upgrade preserves the recorded installation/runtime-data locations and all persistent project and OAuth state, then uses the same candidate project health gates and host activation transaction as `lctk update`. Repair accepts only the exact immutable component identities for that version. An older setup fails before mutation; downgrades remain the responsibility of verified rollback. Any changed release-candidate or published setup must therefore receive a higher product version.

The Windows setup, launcher, and core are intentionally not Authenticode-signed. Windows can show an unknown-publisher or SmartScreen warning, and managed-device policy can refuse execution. The GitHub Release page must state this explicitly; release integrity is established by the tagged workflow, GitHub attestations, `SHA256SUMS`, launcher-to-core binding, and the Ed25519-signed manifest rather than a Windows publisher certificate.

CLI bootstrap and update remain read-only unless `--yes` is present:

```text
lctk bootstrap --plan
lctk bootstrap --yes
lctk update --plan
lctk update --yes
lctk update rollback
```

Update installs and health-checks the candidate code image for every previously running project before atomically activating the matching host core. Schema 1 to 2 migration runs on a copied SQLite database; the original remains as `semantic.db.rollback-v1`. Any candidate health failure restores migrated projects in reverse order. Explicit rollback verifies the previous core digest, restores available project database bundles, restarts the prior image, and changes the activation document only after those gates pass.

Version rules are defined in [versioning.md](versioning.md), the transaction contract in [ADR-0022](adr/0022-transactional-bootstrap-update-and-release-evidence.md), and the exact evidence boundary in [compatibility.md](compatibility.md).
