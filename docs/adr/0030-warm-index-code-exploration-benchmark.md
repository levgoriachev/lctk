# ADR-0030: Warm-index code-exploration benchmark

- Status: accepted
- Date: 2026-08-07
- Deciders: project maintainers

## Context

LCTK needs repeatable evidence that its project-scoped code intelligence improves an agent's ability to locate the code required to understand and fix an issue. Index construction is installation work, not part of an agent turn, so charging it to every query would measure deployment rather than the product's steady-state value.

The public SWE-Explore benchmark already defines exploration-only tasks, ordered file-and-line predictions, and line-, file-, region-, coverage-, ranking-, and efficiency-based metrics. Its public benchmark data is derived from SWE-bench instances, while issue text and base commits come from the corresponding pinned SWE-bench source data.

Comparing different agents against each other would confound the tool effect with model and client behavior. Reusing a session, an index that is still updating, memory written by an earlier arm, or a checkout that differs from the declared base commit would create the same problem.

## Decision

SWE-Explore is the primary exploration benchmark. Each client is compared only with itself:

- Codex native versus Codex with LCTK;
- Claude Code native versus Claude Code with LCTK.

Every pair uses the same exact model, reasoning effort, issue prompt, repository base commit, top-k, timeout, and budget. Every arm starts in a fresh non-persistent session. Native arms have no LCTK MCP server. LCTK arms expose the project-bound LCTK MCP server and must produce trace evidence that at least one LCTK code-intelligence tool was called. Web access, gold patches, hidden tests, solution trajectories, benchmark ground truth, write tools, and persistent project memory are unavailable to both arms.

The measured interval begins immediately before the agent process starts and ends when it exits. Repository acquisition, checkout, LCTK registration, indexing, watcher settlement, and preflight are outside that interval. Their time is recorded only as operational telemetry and is never included in agent latency or cost. Preparation writes and synchronizes a JSONL observation on every retry after a two-second delay; status and GPU probes add their real duration to that interval. Each row separates exact/semantic/graph state, semantic chunk counters, derived completion/rate/ETA, stall evidence, verified inference identity, and live GPU utilization, VRAM, power, and temperature. A process being healthy is therefore never treated as indexing progress.

A LCTK arm is eligible only when a fail-fast preflight proves all of the following for the exact materialized snapshot:

1. the repository is clean and `HEAD` equals the instance's declared base commit;
2. the installed project stack is running and healthy;
3. the watcher journal is complete, has no pending paths, and its checkpoint equals its sequence;
4. exact, semantic, and graph indexes are ready and fresh;
5. exact, semantic, graph, and watcher generations are equal;
6. explicit project memory is empty for the initial benchmark campaign.

One stable registered benchmark-project root is reused sequentially. A new instance is checked out only while no agent is running, then LCTK is allowed to settle before the next measured arm. Baseline and LCTK arm order is deterministically counterbalanced across instances. No two arms mutate or index the same root concurrently.

The benchmark code and the two source datasets are external, commit-pinned inputs. They are not vendored. SWE-Explore code remains under MIT; its public benchmark data remains under CC-BY-NC-ND-4.0 and is consumed unchanged. Repository-owned unit fixtures are synthetic and contain no copied benchmark records.

The canonical prediction artifact is compatible with the official SWE-Explore evaluator: ordered `path`, `start`, and `end` regions per instance. The harness canonicalizes and confines paths to project-relative form but preserves interval values exactly. In particular, the official evaluator counts positive lines beyond EOF or on an unknown path as prediction noise, so the harness neither rejects nor clamps them before scoring. Raw client JSONL, parsed predictions, tool-call evidence, elapsed time, exit status, model identity, client version, repository commit, LCTK freshness proof, and scorer identity are retained for audit. Aggregate reports must keep Codex and Claude separate and must report paired deltas with confidence intervals rather than one pooled headline score.

Paid campaigns use an immutable repository-stratified manifest. The manifest pins the sampling seed and method, exact joined instance identities, dataset and configuration digests, harness commit and executable digest, and both client executable digests, versions, models, and efforts. A campaign processes the manifest sequentially and may resume only from immutable completion receipts whose SHA-256 references validate the raw trace, normalized result, and official score. The harness publishes an arm receipt only after the repository-owned scorer matches all 17 official metrics within `1e-12`; it publishes a pair receipt only after both arms have matching client and actual-model identities. Failed or interrupted attempts remain separate diagnostic evidence and never count as completed observations.

A product or harness identity change creates a new immutable campaign. Native control evidence may be copied from the preceding campaign only through the explicit import command, for one currently checked-out instance at a time. The command requires identical instance, dataset, official evaluator, client executable digest/version, model, effort, and native-arm identity; validates every source SHA-256; copies the raw trace; reruns both official and local scoring on the exact checkout; and writes a new target receipt with source provenance. LCTK treatment receipts are never importable. Campaign execution can stop after preparation and can be bounded to the first `N` manifest instances, allowing a one-instance GPU/progress gate before any mass paid run.

Preparation settles and records the current LCTK generation before a checkout change. When the base commit changes, freshness is accepted only after exact, semantic, graph, and watcher state converge on a generation strictly newer than that baseline. This closes the watcher-observation race where the old checkout can remain momentarily reported as fresh immediately after Git switches files on disk.

Aggregate reports are reproducible views over receipts, not primary evidence. They retain all 17 official metric means, treatment-minus-native paired deltas, deterministic paired-bootstrap 95% confidence intervals for the four primary metrics, elapsed agent time, every provider-reported token category, LCTK tool-call counts, and provider-specific cost, API-duration, and turn fields when the client reports them. Materialization and freshness-settlement telemetry remains separate from agent latency.

## Alternatives considered

### Include indexing time in every LCTK arm

Rejected. Users query an already indexed project. Repeating initial installation work for every issue answers a different question and overwhelms the steady-state code-intelligence effect.

### Compare Codex directly with Claude Code

Rejected. The result would mix client and model differences with the LCTK treatment. Each agent is its own control.

### Reuse one conversation across arms

Rejected. Conversation state leaks discoveries from the first arm into the second and invalidates the pair.

### Vendor the public benchmark dataset

Rejected. The dataset's non-derivative license and independent release history require an unchanged, externally pinned input rather than a repository copy.

### Trust timestamps or successful queries as freshness proof

Rejected. Freshness is an explicit multi-generation contract. A successful query can still come from a stale or partially rebuilt index.

## Consequences

### Positive

- The measured difference isolates warm LCTK-assisted exploration for each supported client.
- Failed isolation, stale indexes, mismatched commits, and missing tool use are visible exclusions rather than silent noise.
- Artifacts can be rescored by the pinned official evaluator without rerunning paid agent turns.
- The stable endpoint requires one client authorization instead of one authorization per task snapshot.

### Negative

- Tasks run sequentially through one registered root, so indexing cannot be parallelized on that endpoint.
- Separate native and LCTK client configurations must be maintained and preflighted.
- Full statistical results require repeated paid runs and therefore explicit campaign budgets.

### Follow-up

- Complete one synthetic pipeline test and one real single-instance pilot for every locally runnable client before a mass campaign.
- Record the exact SWE-Explore, SWE-Explore dataset, SWE-bench source-data, client, model, and LCTK identities with every campaign.
- Pre-register one primary model-and-effort profile per client. Additional model tiers or effort sweeps are separate sensitivity campaigns, never post-hoc substitutions in the primary campaign.
- Add downstream patch-quality evaluation only after the exploration-only comparison is stable; do not mix it into the primary metric.
