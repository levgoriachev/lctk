# Indexing and freshness

## Goal

LCTK maintains an up-to-date representation of code saved to disk. The user does not run a full reindex after routine edits.

Without its own IDE extension, LCTK indexes saved files. Unsaved editor buffers remain the responsibility of the IDE or Codex client.

## Change pipeline

```text
editor saves a file
→ host watcher receives a native filesystem event
→ event is normalized and written to the change journal
→ project idle timer is reset
→ debounce combines closely spaced events
→ batch is classified
→ affected indexes are updated
→ new index generation is published atomically
→ freshness state becomes ready
```

The watcher runs in the host daemon because native Windows and macOS events are more reliable and faster than watching a Docker Desktop bind mount.

The watcher is an accelerator, not the sole source of truth. After downtime, overflow, or a restart, the daemon reconciles the filesystem with the persistent index manifest.

## The change journal

As implemented in Slice 2.1, the host keeps one journal per project in the LCTK home. It makes a single claim: **every change since its checkpoint is in the pending list, or a gap says otherwise.** [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md) records why that claim is the whole design.

The journal holds a monotonic sequence, a checkpoint the consumer advances, the index generation that existed at the checkpoint, one pending entry per changed path, and an optional gap.

Deduplication is by path. A file saved fifty times is one change to apply, not fifty.

A gap is recorded whenever observation *could* have been incomplete — not when something is proven lost:

| Reason | What happened |
|---|---|
| `observation_started` | The journal was loaded, so the period before it was unobserved |
| `observation_suspended` | The watcher was released because the project stopped or went idle |
| `watcher_overflow` | The native watcher lost events |
| `watch_capacity_exceeded` | The project has more directories than the watch budget |
| `watch_set_incomplete` | The service would not describe the whole project |
| `directory_unwatchable` | A directory exists but could not be registered |
| `consumer_backlog` | Events arrived faster than they were taken |
| `bulk_change` | More paths changed than the journal will track |
| `journal_unreadable` | The stored document was reset |

A gap is a latch. It keeps the earliest reason, since that is the moment from which the record stopped being complete, and it is cleared only by a consumer that reconciled the filesystem with the index — and only when the gap it closes is the one it set out to close, not one that opened while it worked.

Loading a journal always records a gap. Nothing on disk can establish that a project was unchanged while the daemon was not running. What persistence buys is that a continuously running daemon never has to reconcile, and that work observed but not yet applied is not lost when the process ends.

`lctk project watch PROJECT` reads the journal, and `--follow` streams normalized events for diagnosis without writing anything.

## What is watched

The host asks the project's own service which directories to observe. The service owns the exclusion policy, so it is the only thing that can answer without disagreeing with the indexer; see [ADR-0015](adr/0015-change-observation-is-complete-or-declared-incomplete.md).

The host hard-codes only `.git`, `.hg`, and `.svn`, which is safe because those are exactly the rules a project cannot override.

A newly created directory is adopted immediately, together with whatever is already inside it. That walk is not tidiness: a tool that writes a whole tree at once fills the directory before a watch can be placed on it, and without the walk those files stay invisible until something else touches them.

Watching costs one native handle per directory, so a project may hold at most `watch.max_watched_directories` of them, 20,000 by default. Past the budget the watcher observes part of the tree and records a capacity gap, which routes the project to reconciliation rather than to either failure or silence.

## Configurable debounce

The shipped default is **3 seconds** after the most recent change, with each new change restarting the wait, and a 30-second ceiling on how long continuous editing may defer an update.

Three layers, each narrower than the last:

| Layer | Where | Role |
|---|---|---|
| Shipped default | `internal/hostsettings` | 3 s window, 30 s ceiling |
| Machine policy | `settings.json` in the LCTK home | What the machine owner wants |
| Project proposal | `index.debounce_ms` in the manifest | What the repository suggests |

The project's value is a proposal, not a setting: the host clamps it to between 200 ms and 60 seconds. The floor exists because an editor save is often a write to a temporary file followed by a rename, and reacting between the two indexes a file that is about to be replaced. The ceiling exists because the point of watching is that an agent's next question sees the edit it just made.

`lctk settings show` prints the policy in force and the path of the file that would change it.

Debounce delays updates to persistent indexes, but not file saves or ordinary source reads. While a batch is pending or being processed, responses show pending changes and freshness lag.

## Applying the journal

As implemented in Slice 2.2, a settled batch is applied to the project index without anyone asking.

The journal decides which of two paths is taken, and the choice is not the consumer's to make:

- **complete record** — the pending list is applied as a delta, and the checkpoint advances to the sequence that was applied, together with the index generation it produced;
- **gap** — the pending list is a lower bound and cannot be applied, so the service reconciles its own inventory against the filesystem instead, and the gap is cleared only if it is the same gap the reconciliation set out to close.

A failed update advances nothing. The batch stays pending and is tried again at the next settle, and the reason is reported in `lctk project watch` rather than only logged, because an index that has stopped advancing looks exactly like one with nothing to do.

Benchmark preparation samples `chunks_total`, `chunks_embedded`, `chunks_reused`, `progress_at`, and stall state on every retry after a two-second delay and keeps that JSONL outside measured agent latency. Probe execution time is part of the sampling interval rather than hidden by an inaccurate fixed-rate claim. For the NVIDIA distribution the same row includes live CUDA utilization, VRAM, power, and temperature. Increasing chunk counters proves index progress; GPU load proves where embedding computation is executing; neither process liveness nor temperature alone substitutes for those facts.

The watcher does not impose one fixed deadline on the complete drain. A gap may require a repository-wide semantic rebuild, whose duration depends on repository size and local inference hardware. Canceling that aggregate operation would discard the transactional semantic work and cause every retry to restart from zero. Individual embedding requests retain their own fail-fast timeout, and stopping the project service terminates its in-flight request.

### The project can move under the watcher

A project restarted while the daemon is running can come back with a different private container address and loopback tunnel. The host's observation is untouched by that: the watcher holds directories on the host, and the journal is in the LCTK home.

So the address is re-read rather than captured once — on the sweep, and on any client request, because a client using a project is the earliest evidence available that it came back somewhere else. Nothing is recorded as a gap: what failed was *applying* the record, not producing it, so the pending list is still complete and is simply applicable again. Discarding the journal here would turn a recoverable lag into a full reconciliation.

Reported honestly while it lasts, which is what made the defect findable: before this, a restart left the worker posting to a dead port forever, with the failure in the log and in `lctk project watch` and the checkpoint correctly refusing to advance — a loud failure that never recovered without restarting the daemon.

Three translations matter:

| Observation | Sent to the index |
|---|---|
| A file written | apply the path |
| A file removed | retract the path |
| A directory removed | retract the path **and everything beneath it** |
| A directory created | nothing; its files arrive as their own observations |

The directory case is the one that cannot be reconstructed later. Once a directory is gone, nothing can enumerate what was in it, so the change carries a subtree flag and the service expands it against its own inventory. Sent as an ordinary path it would retract one entry the index does not hold, and every file that had been inside would stay searchable.

### A search sees the edit that just happened

An agent that writes a file and immediately searches for it is the common case, and waiting out a 3-second debounce window would tell it the code it just wrote does not exist. So a search flushes the pending batch first and waits, bounded by a few seconds.

The bound applies to the wait, not to the work. A caller that gives up waiting does not cancel a rebuild halfway; the search runs against what the index holds and reports its freshness, which is still an honest answer.

Measured against this repository: a file written and searched for **0.2 seconds later** was found, against a 3-second debounce window.

### A write that changed nothing costs nothing

A save does not imply an edit. A formatter with nothing to reformat, an editor writing on focus loss, a build tool rewriting a generated file byte for byte, and an edit applied to disk and then undone all produce a write event for a file whose content the index already holds.

Before applying a written path, the service compares the file's content digest against the digest recorded for it in the published index. When they match, the change is dropped: no document is retracted, none is added, and **no generation is published at all**. The generation number, the delta depth, and the build timestamp stay where they were.

Dropping the change is not merely cheaper than applying it. A delta that retracted the entry and re-added identical text would produce the same searchable index while spending a delta generation, and delta depth is the budget that forces the next full rebuild. Without this filter an editor's autosave sets the rebuild schedule.

Two consequences worth stating:

- **A batch is filtered per path, not accepted or rejected whole.** Eight paths submitted with one real edit among them is a one-file delta, and the report says one changed and seven unchanged.
- **An edit undone before the index caught up is free; an edit undone after it is two changes.** The journal keeps one entry per path, so a write and its revert inside the same batch arrive as a single change whose content already matches. Once the index has caught up, the index genuinely held the other content and search results reflected it, so restoring the original is a real change and costs a generation. That is the honest outcome, not a limitation to remove.

### Bulk changes

A batch touching much of the index costs more as a delta than as a rebuild, because every retracted path leaves a tombstone that later queries have to resolve. Two thresholds escalate a batch to a full rebuild:

- more than a quarter of the indexed files in one batch, and
- at least 500 files, so a small project is not rebuilt whenever two files change.

Above those, and above the delta-depth limit from [Slice 1.5](#persistent-exact-index), the next update is a rebuild. The journal has its own upper bound at 10,000 pending paths, past which it records a bulk gap and the change goes to reconciliation instead.

Escalation is judged on what actually changed rather than on what was submitted, which is the same decision as the filter above. A branch checked out and immediately checked back would otherwise be a bulk change twice over and force two full rebuilds for no net edit.

An explicit `lctk project reindex --full` is stronger than an automatic exact-index escalation. It recomputes every semantic embedding and graph fact, reports zero reused chunks, and publishes the replacement only after all inference succeeds. The transaction replaces only derived semantic and graph rows; explicit project memory remains intact. The CLI bounds this recovery at 45 minutes, which covers the measured 30-minute CPU baseline and matches the native setup transaction budget. Automatic bulk and delta-depth exact rebuilds still reuse unchanged semantic chunks because they are ordinary catch-up, not an operator request for fresh inference evidence.

## Layered updates

A routine file change does not trigger a full repository rebuild:

- exact search: replace the changed file's document or segments;
- AST: parse the file again;
- symbols/LSP: update the document and affected compilation units;
- semantic: remove the file's previous chunks and add AST-aware chunks for the new version;
- graph: recompute relationships for affected symbols and modules;
- repository map: update local summaries and centrality when the change is significant.

Generation publication must prevent partially consistent mixed state from being returned. Different backends may have different generations or freshness states if the response makes this explicit.

## Batch classification

The indexer distinguishes:

- **small change** — incremental update;
- **large/bulk change** — targeted or repository rescan;
- **watcher overflow/unknown gap** — manifest reconciliation;
- **schema mismatch/corruption** — rebuild of the specific index.

Thresholds must have safe defaults. It has not been decided whether advanced thresholds will be user-configurable or remain engine-owned.

Special events:

- create/modify/delete/rename;
- branch checkout;
- rebase/merge;
- lockfile and build-configuration changes;
- bulk generation;
- Git worktree changes.

## Initial indexing

Initial indexing proceeds in layers:

1. project metadata and file inventory;
2. exact search;
3. AST/file outlines;
4. symbols/LSP availability;
5. semantic chunks/embeddings;
6. graph and repository map enrichment.

Early capabilities become available while heavyweight layers continue to build. Multi-hour CPU-only indexing of a large project is acceptable when progress is reported and the resource mode is controlled.

Semantic chunk identities must be unique before inference begins. Structural chunks retain a concise user-facing anchor, while their persistent identity also includes the complete syntax declaration and deterministic occurrence. This distinguishes same-named methods on different receivers and overloaded declarations. The complete prepared set is checked for collisions before any embedding request; a collision is a typed terminal error rather than a full repository computation that can only fail during SQLite publication.

The chunk limit applies to the complete document input sent to inference, not only source content. The task prefix, project-relative path, anchor, separators, and content must fit the pinned runtime context together. The chunking algorithm carries its own persistent revision: a changed revision disables semantic reuse and publishes its replacement vectors and revision in one transaction, while the preceding generation remains queryable if inference fails.

Semantic status reports total, embedded, and reused chunks together with start time, last progress time, the previous terminal error while a retry is active, and a `stalled` verdict after three minutes without counter movement. The Admin UI displays these fields and the derived graph state so slow work, stopped progress, and a failed generation are not presented as the same indefinite "indexing" state.

## Resource modes

As implemented in Slice 2.3, the background-load mode decides what a project is allowed to cost:

| Mode | Container CPU | Concurrent index work |
|---|---|---|
| `quiet` | 1 | 1 at a time |
| `normal` (default) | 2 | 2 at a time |
| `fast` | no limit | as many as the container allows |

The mode is set machine-wide in `settings.json` and can be overridden per project with `lctk project resources --mode`. The override lives in the registry rather than in the repository manifest: how much of someone's machine a project may spend is theirs to decide, and a repository author has no say in it.

Limits are applied when a container is created, so a change takes effect at the next `lctk project restart`. The command says so rather than leaving it to be discovered.

**Memory is not capped by default, in any mode.** A CPU limit throttles — indexing takes longer and the machine stays usable — while a memory limit kills, and a container terminated mid-build leaves the index no better off than before. An operator who wants the guarantee sets `resources.memory_limit_mb`; nobody gets it by accident.

The parallelism cap is passed into the container rather than derived inside it, because the container cannot see the host's policy and the engine would otherwise size itself to the whole machine no matter what was asked for.

The embedding inference process may be a shared compute resource across projects. Project chunks, vector collections, and metadata remain separate.

## Disk

`lctk project resources` reports what a project costs and what room is left:

```text
  index:       10.0 MiB on disk for 1.2 MiB of source
  free space:  626.7 GiB
```

`lctk project start` and `lctk project restart` refuse when starting would leave less than a gigabyte free, and `--yes` overrides the refusal. Failing beforehand with one sentence is better than failing partway through a build and leaving a partial generation plus two symptoms to untangle.

For a project with no index yet, the expected size is estimated as a fixed cost plus a share of the source. The shape comes from the format — a shard carries metadata whose size does not depend on its content, and two generations are retained, so a project pays that twice before paying for anything else — and the numbers are anchored to one measurement: this repository, 179 files and 1.19 MiB of source, occupies 9.98 MiB of index. One small repository is not a sample, and on a large project the estimate will be high; that is the safe direction for a figure that only ever warns.

Free space is measured on the volume holding the LCTK home, which is where Docker Desktop keeps its data in a default installation on both target platforms. The index itself lives in a Docker volume rather than in a directory LCTK owns, so there is nothing more direct to measure, and an operator who has relocated Docker's data directory gets a proxy rather than a measurement.

## Freshness contract

Every index-dependent response must report the following compactly:

- source commit/branch;
- dirty state;
- index generation/version;
- pending file count or lag;
- freshness (`fresh`, `updating`, `stale`, `unknown`);
- backend provenance;
- timestamp of the last successful update.

The source commit, branch, and dirty state come from Git, reported in `project_info` under `source` as of Slice 3.1. They are absent for a project that is not a repository, and on a machine without Git, which is a truthful "not known" rather than a fabricated clean state. A short-lived cache keeps repeated answers from costing a subprocess each.

Freshness is never optimistic. `unknown` is reported for a project nothing is watching, because "nobody looked" is not evidence that nothing changed, and an agent told `fresh` will not check again. An incomplete record reports `stale` with the gap reason attached, and the pending count is then a lower bound rather than a total.

### What freshness is a claim about

`fresh` says the index matches the project's files **as they are written to disk**. It is not a claim that the index accounts for every edit a caller has in mind.

Two states are invisible here by design:

- an editor buffer that has not been saved;
- a patch a client is holding and has not applied — the diff most coding agents show for approval before writing it.

Neither can be observed from outside the client that holds it, and the only way to import one would be to let a client state what a file contains. LCTK refuses that for the same reason a manifest cannot mount a path: it would let a client put text into the index that never existed on disk, where a second client would find it by search and treat it as real. The scope is stated in the `exact_search` tool description so an agent reads it rather than infers it.

The reverse case — an edit that *has* been written to disk but is still awaiting a human's approval, and may be reverted — is not distinguished either, and deliberately so. The filesystem does not record who wrote a file or why, and treating a written file as anything other than the project's current content would mean guessing. Such an edit is indexed, searchable, and reported by `git_status` as a working-tree change, because that is what it is. If it is reverted before the index catches up, the revert costs nothing (see [A write that changed nothing costs nothing](#a-write-that-changed-nothing-costs-nothing)).

`project_info` carries this as an `index.freshness` verdict alongside a `changes` block naming what the host has observed:

```json
{
  "index": { "ready": true, "generation": 12, "file_count": 169, "freshness": "stale" },
  "changes": {
    "watching": true,
    "pending": 1,
    "complete": false,
    "gap_reason": "observation_started",
    "debounce_seconds": 3
  }
}
```

Example state:

```json
{
  "commit": "abc123",
  "dirty": true,
  "freshness": "updating",
  "pending_files": 3,
  "indexes": {
    "exact": { "state": "ready", "generation": 42 },
    "semantic": { "state": "updating", "generation": 41 },
    "graph": { "state": "stale", "lag_seconds": 12 }
  }
}
```

The system must not silently return a stale result as fully current.

## Persistence

Project volumes store:

- engine index data;
- index manifests;
- content hashes/file inventory;
- change journal checkpoint;
- schema and engine versions;
- last indexed commit/dirty snapshot;
- rebuild reason and status.

Updates must support atomic commit or swap where the backend permits. After a crash, the previous committed generation remains readable, or the index is explicitly marked as corrupt.

The change journal itself lives on the host rather than in the project volume, one owner-only document per project under `journals/` in the LCTK home. It belongs with the registry and the grants for the reason given in [security](security.md): it is host state about a project, not project content, and it must not be reachable from inside a repository. It is written atomically at each settle, so an interrupted write cannot leave a shorter pending list that would read as complete.

## Exclusions

Rules come from three files, each using the familiar ignore syntax, applied in this order within every directory. A later rule beats an earlier one, so a file further down the list can both add exclusions and re-include with `!`.

| File | Role | Tracked |
|---|---|---|
| `.gitignore` | A default, not an authority | yes |
| `.lctkignore` | The project's decision about **indexing** | yes |
| `.lctkignore.local` | The same decision for one machine | no |

A version-control ignore file answers "what should not be committed". That is a different question from "what should not be indexed", and the two disagree in a specific and common way: a local scratch directory is deliberately uncommitted and is exactly the sort of thing its owner wants to search. So `.gitignore` is consulted because it is usually right and always present, and `.lctkignore` has the last word because only the project knows what it wants searched:

```gitignore
# .lctkignore
!.work/        # uncommitted, but I search it constantly
fixtures/big/  # committed, but nothing here is worth a match
```

`.lctkignore.local` is untracked by convention and is applied last, so a personal choice does not end up in a shared file. It mirrors the manifest's local override described in [security](security.md).

A project whose version-control rules say nothing useful about indexing can start from a clean slate: a first line of `!/**` in `.lctkignore` re-includes everything, and the rules after it narrow the set again.

All three are honoured in nested directories, where they apply to their own subtree and not to siblings.

Enumeration comes from the filesystem rather than from Git objects, so a file that is saved but never committed is indexed. That is a statement about content, not about scope: a directory the project has told LCTK to ignore is not part of the project.

The index status reports which of the three files were actually found, so the rules in effect are visible rather than inferred from what went missing.

LCTK adds a short default list, applied *before* all of them so the project can overrule any of it:

```text
node_modules  .venv  venv  __pycache__  .mypy_cache  .pytest_cache
.tox  .gradle  .turbo  .next  .nuxt  .parcel-cache
```

The list is deliberately short. Names like `dist`, `build`, `target`, and `vendor` are derived output in some projects and real source in others; excluding them by default would lose code silently, and a project that treats them as output has already said so in its ignore file.

Version-control metadata — `.git`, `.hg`, `.svn` — is excluded unconditionally and cannot be re-included.

Symbolic links are skipped rather than followed. Following one is the ordinary way out of a read-only mount, and the mount is the boundary the project service is trusted to stay inside.

The number of entries excluded by ignore rules and by the file-size limit is reported in the index status, so the effect of an exclusion is visible rather than silent.

The manifest may add project-specific exclusions. Generated and dependency paths generally must not reset the idle timer or create an indexing storm, but the precise relationship among watcher exclusions, test artifacts, and explicitly included generated code requires a separate policy.

## Persistent exact index

As implemented in Slice 1.5, the exact-search index is a directory of published generations inside the project volume, with a `current` link naming the live one.

A build never writes into the published generation. It stages a new directory, hard-links the previous shards where a delta build needs them, writes its state, and then replaces one symlink. A concurrent search therefore reads either the whole previous generation or the whole new one, and a crash mid-build leaves the previous generation intact rather than a half-written index.

Delta depth is bounded. The engine resolves a query across every shard in the generation, so an unbounded pile of deltas would slow every future search, not only the next update. Past the threshold the next update is escalated to a full rebuild. The shipped policy is 32 delta generations, 2 retained generations, and a 1 MiB per-file limit; a project may raise the file limit and the delta threshold through the environment.

A published generation written by a different schema version is treated as corrupt rather than read, because the shard format belongs to the engine and LCTK does not attempt to migrate it. A corrupt index is reported as a typed error, never as an empty result: answering "no matches" would look like a correct answer about the project.
