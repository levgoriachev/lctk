# ADR-0013: Versioned JSON document for the local project registry

> Amended by [ADR-0024](0024-native-windows-setup-and-user-selected-storage.md): Windows setup may relocate the LCTK home and records its selected location under `HKCU\Software\LCTK`.

- Status: accepted
- Date: 2026-07-31
- Deciders: project maintainers

## Context

The local registry holds the binding that every other guarantee depends on: a `project_id` mapped to an authoritative host path. Per [ADR-0001](0001-route-bound-project-scope.md) and [`docs/security.md`](../security.md), that binding is the only authority on project scope, and it must live outside Git so a repository author cannot influence it.

Slice 1.1 requires a typed project model with migrations, and it must work without starting any service. The registry is therefore read and written by a short-lived CLI process before any daemon, container, or index exists.

The data is small and its access pattern is narrow. One developer registers a handful of folders. Every operation loads the whole set, changes at most one record, and writes it back. There are no queries, no joins, no partial reads, and no concurrent writers beyond a second CLI invocation the user started themselves.

Later stages will store much heavier state: the change journal in Stage 2, per-project index metadata, and the memory and graph stores in Stage 6. Those have genuinely different requirements, and [ADR-0004](0004-stable-aggregated-tool-api.md) already establishes that persistent schema versions may evolve independently from the components that use them.

Two candidates were considered for Slice 1.1: a versioned JSON document, and an embedded SQLite database.

## Decision

Store the registry as a single versioned JSON document named `registry.json` inside a per-user LCTK home directory.

The home directory is `%LOCALAPPDATA%\lctk` on Windows, `~/Library/Application Support/lctk` on macOS, and the XDG data directory elsewhere, overridable through the `LCTK_HOME` environment variable so that tests and portable installations stay isolated. The directory is created with owner-only permissions because it will later hold client credentials and grants.

The document carries an explicit `schema_version` as its first field. `Load` migrates an older document forward through explicit, reviewable steps and refuses a document written by a newer LCTK with a typed error rather than guessing. A document with no `schema_version` is read as version 1, since no released schema predates that field.

Writes are atomic: the document is written to a temporary file in the same directory, flushed, permission-restricted, and renamed over the target. An interrupted write must never leave a half-written registry, because that would detach projects from their persistent data.

A corrupt document is an error, never a silent reset. Discarding registrations would orphan project volumes and indexes, which is worse than refusing to start.

Validation runs on both load and save. A duplicated `project_id` is rejected outright, because an ambiguous identifier would make a route ambiguous and break scope enforcement.

This decision covers the registry only. It does not choose the storage engine for the change journal, index metadata, or semantic and graph state, each of which will be decided against its own requirements.

## Alternatives considered

### Embedded SQLite

Rejected for this slice, not on principle. SQLite would bring real transactions, concurrent-reader safety, and a natural migration idiom, and it is the obvious answer once state becomes large or is written from several processes.

It is the wrong tool for a list of a dozen records read by a short-lived CLI. A pure-Go driver adds a substantial dependency for no query capability LCTK needs, and a cgo driver would compromise the simple cross-compilation that [ADR-0006](0006-go-language-and-toolchain.md) and the artifact workflow rely on. [ADR-0009](0009-embedded-go-gateway-and-project-runtime.md) also explicitly values not introducing another database and migration stream into installation. Introducing one before any measured need would be the kind of speculative complexity the delivery policy rejects.

Nothing here forecloses it. The store is a narrow package with `Load` and `Save`, so replacing the medium later is a contained change behind an ADR.

### A file per project

Rejected. It removes the whole-document rewrite but makes a consistent multi-record change impossible to apply atomically, and it turns duplicate detection into a directory scan. It also multiplies the number of partially written states a crash can produce.

### Store the registry inside each repository

Rejected outright. It would place the authoritative host path where a repository author controls it, contradicting [ADR-0001](0001-route-bound-project-scope.md) and the manifest trust boundary in [`docs/security.md`](../security.md). Slice 0.4 measured the practical version of this hazard: a repository-local Codex file can shadow a generated entry in a trusted project, which is exactly why LCTK's own authority must not live in the repository.

### YAML or TOML for the registry

Rejected. Both are better for files a human writes, and the manifest uses YAML for that reason. The registry is machine-written and machine-read, where JSON's unambiguous round-tripping of strings and timestamps matters more than authoring comfort, and it needs no dependency.

## Consequences

### Positive

- Registration works with no daemon, no container runtime, and no database, which is what Slice 1.1 requires.
- The file is trivially inspectable and diffable when diagnosing a user's machine.
- Atomic replacement plus refusal to reset on corruption protects the binding that project isolation depends on.
- Host state stays out of Git by construction, and `LCTK_HOME` keeps every test isolated from real user state.
- No new module dependency, so cross-compilation and the artifact workflow are unaffected.
- Schema versioning and migration are established now, before there is a released schema to be compatible with.

### Negative

- Every change rewrites the whole document. Harmless at this scale, and a reason to revisit the choice if the registry ever grows large.
- There is no locking, so two simultaneous writes can lose one of them on a last-writer-wins basis. Acceptable for a single-user CLI, but it must be addressed before the daemon and the Admin UI write concurrently.
- JSON carries no comments, so operator notes cannot live in the registry.
- A second store will eventually exist for heavier state, so LCTK will have more than one persistence mechanism.

### Follow-up

- Decide the concurrency story before the daemon or Admin UI writes the registry; a lock file or serializing writes through the daemon are the obvious candidates.
- Choose storage for the change journal and index metadata in Stage 2 against their own requirements.
- Add a migration test for every future schema version, following the pattern established for version 1.
- Revisit this decision if the registry gains query needs, grows beyond a trivial size, or acquires multiple writers.
