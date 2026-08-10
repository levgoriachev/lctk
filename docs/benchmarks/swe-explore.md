# SWE-Explore warm-index benchmark

This document is the executable measurement contract for [ADR-0030](../adr/0030-warm-index-code-exploration-benchmark.md). It measures whether an already indexed LCTK project improves code exploration by the same agent. Index construction time is deliberately outside every measured arm.

## Pinned inputs

| Input | Required identity | License |
|---|---|---|
| SWE-Explore code | `https://github.com/Qiushao-E/SWE-Explore-Bench.git` at `3c12dc5a551937038afcbdb6eb6bbf19f3ddd8c1` | MIT |
| SWE-Explore public data | `https://huggingface.co/datasets/SWE-Explore-Bench/SWE-Explore-Bench` at `bdb0ae45d7c337d9e1dc3ebfe2a0af6bc7c1fbd9`; `bench.final.public.jsonl` SHA-256 `dc4f114ececd0bfb987361c26ae5e2440456e2cccb36adfccb09ea5385aec202` | CC-BY-NC-ND-4.0 |
| SWE-bench issue source | `https://huggingface.co/datasets/princeton-nlp/SWE-bench_Verified`; campaign configuration must pin a repository commit and source-file SHA-256 | Dataset card terms |

The public SWE-Explore row supplies ground truth but not the issue text or base commit. The harness therefore joins it by exact `instance_id` with an independently pinned SWE-bench source JSONL containing `instance_id`, `repo`, `base_commit`, and `problem_statement`. Duplicate or missing identifiers are fatal.

## Repository-owned corpora

The public dataset is derived from repositories the target models were trained on, so a result on it cannot separate retrieval from recall. The `corpus` command builds the same two JSONL shapes from a repository's own fix history, which makes a private or recent repository measurable through the unchanged join, campaign, and scoring path.

```text
swe-explore-benchmark corpus --repo PATH --repo-name NAME --since YYYY-MM-DD [--until YYYY-MM-DD] \
  --count N --seed TEXT --output-explore FILE --output-issues FILE [--statements FILE] [--report FILE]
```

A commit becomes an instance only when it is a single-parent, non-revert fix that touches between one and eight non-test source files and between 3 and 400 changed lines in them. Lock, generated, asset, and vendored paths are excluded; test paths become optional ground truth rather than core. Ground-truth regions are pre-fix (a-side) hunk coordinates, merged across gaps of at most two lines and clamped to the file's real length at the parent commit, because the agent explores the parent. The single non-test file with the most changed lines is the `main_files` entry, since the evaluator weights it 3.0 against 2.0 for other core files.

The problem statement is the part that decides whether the corpus measures anything. Commit bodies are never used: measured against a real repository they name the changed functions and constants in their first paragraph. The statement is taken, in order, from a pre-fetched ticket file, then from a tracker-URL slug decoded out of the commit body, then from the commit subject with its conventional-commit prefix and every `(#NNNN)` group removed. A commit resolves to tickets through the trailing `(#NNNN)` squash pull-request number matched against `source_pull`, and only otherwise through the subject's inner references matched against `ref`; the two number spaces overlap, so they are never crossed.

Whatever the source, the statement is dropped when it names a ground-truth path, a basename, a code-shaped identifier from the changed lines, a contiguous run of such an identifier's own words, or a `:NNN` line reference. Comparison normalizes both sides to letters and digits, so a decoded slug reading "missing from SESSION DEAD KEYWORDS" is still recognized as `SESSION_DEAD_KEYWORDS`. Over-rejection is the intended failure mode: sample size is recoverable, a leaked instance is not. Every rejection is recorded with its reason in the `--report` JSON.

Selection is deterministic: surviving candidates are ranked by `sha256(seed || 0x00 || instance_id)` and emitted in commit-chronological order. Requesting more instances than survive emits all of them and says so; it never pads. The command prints the SHA-256 of both emitted files so a campaign configuration can pin them like any other input.

The ticket file is JSONL, one `{"ref", "title", "source_pull"}` object per line with an optional `body`; unknown fields are ignored. The builder never contacts a tracker or forge API, so a richer reporter-written statement set arrives as a pinned file rather than as a network dependency.

A private repository cannot be fetched from GitHub, so `benchmark.source_overrides` maps a repository name to a local Git directory or an explicit remote URL. A repository without an entry keeps the strict public `owner/repository` validation, so dataset content still cannot inject a protocol or a filesystem path. `validate` proves every override before a campaign starts.

[`swe-explore-config.example.json`](swe-explore-config.example.json) documents the complete schema. Replace every marker, store the runnable copy under ignored `.artifacts/`, and pin the SHA-256 of the exact exported issue JSONL. The example is intentionally not runnable with placeholder identities.

## Campaign matrix

Run four arms while keeping provider-specific results separate:

| Client | Native control | Warm LCTK treatment |
|---|---|---|
| Codex CLI | `codex exec --ephemeral --json --ignore-user-config` with read-only repository access and web, apps, remote plugins, multi-agent, and memory disabled | the same executable, model, effort, sandbox, prompt, timeout, and budget with only the approved treatment MCP configuration added; its read-only tools use the documented server-scoped `default_tools_approval_mode="approve"` so non-interactive execution does not cancel them |
| Claude Code | non-persistent print mode whose available built-ins are exactly `Read`, `Glob`, and `Grep`, with an empty strict MCP configuration | the same settings with only the approved project-bound treatment MCP server and its configured read-only tool allowlist added |

The prompt is identical across paired arms and requires exploration only, no changes, and exactly one final `RELEVANT_FILES:` block with at most `top_k` ordered `path:start-end` regions and no other final text. `run.prompt_mode` selects the measurement contract:

- `free_choice` (the default): the prompt never mentions MCP, so paired arms differ only in server availability. A treatment run with zero treatment-server calls is a valid non-adoption observation recorded in the result, not a protocol fault.
- `mandated`: the prompt reproduces the ADR-0030 sentence naming the configured treatment server, requiring its `project_info` tool first and its code-intelligence tools as the primary exploration channel while that server is available. A treatment trace without a completed treatment-server call is fatal, which preserves comparability with the earlier mandated campaigns.

In both modes a native trace containing any MCP tool call, to any server, is invalid.

The treatment tool surface is campaign configuration, not harness code: each treatment arm declares a required `treatment_tools` allowlist and an optional `excluded_tools` deny list (LCTK command, Git, and persistent-memory tools stay on the deny side). The harness deliberately does not validate the names against any tool registry, so a campaign can pin a surface that ships in the same release train. A competitor control (for example Serena) is the identical harness pointed at a different treatment server in its own campaign, never a third arm inside this one. Changing the prompt mode or the treatment tool list changes what the campaign measures, so per ADR-0030 it is a new campaign identity, never a resumed or amended one.

## Per-instance sequence

1. Join the public benchmark record to the issue-source record by `instance_id`.
2. Verify the clean benchmark repository and exact `base_commit` before changing it.
3. If the object is absent, shallow-fetch exactly the declared commit from the validated public `https://github.com/<owner>/<repository>.git` source, then check it out while no arm is running.
4. Wait outside the measured interval until the LCTK watcher has no pending paths and exact, semantic, graph, and watcher generations are fresh and equal.
5. Run the first arm in a new non-persistent session, capture its complete machine-readable trace, and reject any repository change.
6. Restore the identical clean commit if necessary, repeat freshness preflight, and run the paired arm in another new session.
7. Alternate arm order deterministically from the instance identifier.
8. Reject the pair if the client version or provider-reported actual model differs between arms.
9. Parse ordered regions, reject unsafe non-project-relative paths, and preserve interval values exactly. The official evaluator treats positive lines beyond EOF or on an unknown path as prediction noise; the harness must not reject or clamp that measured model output.
10. Score the saved prediction through the pinned official SWE-Explore evaluator. A repository-owned parity scorer may test orchestration, but it is not the publication authority.

## Primary and operational measures

The official metric set is `precision`, `recall`, `f1_score`, `hit_file_rate`, `noise_file_rate`, `hit_region_rate`, `noise_region_rate`, `weighted_core_coverage`, `context_efficiency`, `optional_coverage`, `ndcg_at_100`, `ndcg_at_300`, `ndcg_at_500`, `recall_at_100`, `recall_at_300`, `recall_at_500`, and `first_useful_hit`.

The primary product readout is the within-client paired delta for `weighted_core_coverage`, `context_efficiency`, `recall_at_300`, and `first_useful_hit`. Also retain elapsed agent seconds, input/output/cached/cache-creation/reasoning tokens when the client reports them, tool calls, invalid-output rate, timeout rate, and provider-reported cost, API duration, and turns where available. Any API-list-price equivalent is derived later from a campaign-pinned price table and remains distinct from provider-reported cost. Indexing telemetry is reported separately and never added to agent latency or cost.

Operational telemetry receives the same paired inference as quality: the report carries treatment-minus-native per-pair deltas for elapsed milliseconds, every reported token category plus a `billed_input_tokens` sum of the three input categories, total tool calls (treatment-server plus native), and, only when both arms report them, turns and provider-reported cost. Deterministic paired-bootstrap 95% intervals cover the pre-registered telemetry set `elapsed_ms`, `billed_input_tokens`, `output_tokens`, and `tool_calls`. The report also states each provider's adoption rate: the fraction of treatment runs with at least one treatment-server call.

## Paid campaign execution

Build one exact harness executable, create a runnable configuration under ignored `.artifacts/`, and validate it before selecting instances. The `manifest` command deterministically selects a repository-stratified sample. For a 20-instance pilot it first includes one instance from every joined repository, then fills the remaining slots by global SHA-256 rank; execution order is grouped by repository to reduce avoidable checkout and indexing work. The full Git commit is explicit because an executable digest alone does not identify the reviewed source.

```text
swe-explore-benchmark manifest --config CONFIG --campaign-id ID --count 20 --seed TEXT --harness-commit FULL_SHA --output MANIFEST
swe-explore-benchmark campaign --config CONFIG --manifest MANIFEST --output-dir OUTPUT --python PYTHON
```

The manifest pins the exact configuration, datasets, harness, clients, models, efforts, repositories, and base commits. Campaign startup rejects any digest, version, model, or effort drift before a paid arm starts. Before changing a checkout, preparation records a settled baseline generation. A changed commit is eligible only after exact, semantic, graph, and watcher state converge on a strictly newer generation; this prevents a fast Git switch from temporarily reusing the preceding checkout's still-fresh status. The unmeasured freshness wait may be configured up to 12 hours for large repositories on constrained local hardware; the paid arm timeout remains an independent, much smaller limit.

Each paid arm writes into a unique attempt directory. Its raw machine-readable trace, normalized result, and official score become complete only when an immutable arm receipt references all three SHA-256 digests and the repository-owned parity scorer matches all 17 official metrics within `1e-12`. A pair receipt is published only after native and LCTK arms have matching client versions and provider-reported actual models. Interrupted and failed attempts remain diagnostic artifacts but are never included in an aggregate.

Resume validates every referenced digest and reuses a completed arm without calling the model again. When all four arm receipts for an instance are valid, it also skips checkout and index preparation; a missing pair receipt or progress report is reconstructed from immutable arm evidence alone. Missing arm receipts cause a new attempt after normal preparation; malformed or modified receipts and artifacts are fatal before the shared workspace is touched. Progress reports are immutable snapshots rebuilt from receipts. They keep Codex and Claude separate and include all official means, all paired deltas, deterministic paired-bootstrap 95% intervals for the four primary quality metrics and the four pre-registered telemetry metrics, elapsed time, token categories, treatment-server and native tool calls, adoption counters, provider-specific cost and duration fields, and the failed-attempt count partitioned into failure classes (`timeout`, `invalid_output`, `no_adoption` in mandated mode, `other`). Raw traces and receipts remain the publication audit authority.

One exact model and effort per provider is the primary campaign contract. Model-tier and effort comparisons are separate pre-registered sensitivity manifests so they cannot be selected after observing the primary result.

## Single-run readiness gate

Mass testing is allowed only after all of these pass:

- configuration, dataset join, duplicate detection, path validation, output parsing, tool-isolation checks, scorer parity, timeout handling, and repository-mutation detection pass automated tests;
- a deterministic synthetic agent completes one native and one LCTK-shaped arm through the full artifact pipeline;
- a synthetic campaign proves immutable receipts, all-metric aggregation, resume without rerunning completed arms or preparing their workspace, and fatal digest verification before workspace mutation after artifact modification;
- each locally runnable real client completes one native arm and one authorized LCTK arm on the same single instance;
- every treatment preflight records matching fresh generations and zero watcher pending paths;
- the official scorer accepts the emitted prediction JSONL;
- no OAuth token is copied, printed, or stored by the harness, and no IDE is launched.

An unavailable client or unapproved OAuth connection is a failed readiness gate, not a skipped success.

The accepted 2026-08-07 gate evidence is recorded in [the single-run readiness report](swe-explore-single-run.md).

## NVIDIA GPU one-instance campaign gate

Installed local `0.1.14` and campaign `swe-explore-sol-opus5-high-20-v6` passed the first immutable instance on 2026-08-08. The selected instance was `astropy__astropy-13453` at commit `19cc80471739bcb67b7e8099246b391c355023ee`. Preparation published 20,622 semantic chunks and converged exact, semantic, graph, and watcher state at generation 12 with zero pending paths. The SHA-256-bound JSONL contains 288 durable observations, including 261 advancing-counter samples, 147 samples at or above 80% GPU utilization, a 100% GPU peak, and an 87 C maximum. Settlement took 2,488.363 seconds and remains outside measured agent time.

The first preparation attempt failed fast before any paid client because the dedicated project stack was stopped. After starting that exact registered project, preparation completed without relaxing a gate. Native receipts were explicitly re-attested from the preceding immutable campaign; treatment receipts were produced only by `codex-cli 0.147.0` with requested `gpt-5.6-sol`/high and Claude Code `2.1.224` with reported `claude-opus-5`/high. Native traces contain zero LCTK calls; treatment traces contain 15 Codex and four Claude LCTK calls. All four arm receipts preserve matching raw/result/official-score SHA-256 values, both pair receipts reference the exact arm receipts, and an independent rerun of both scorers matched all 17 metrics and every predicted region.

This single instance validates the GPU/progress observability and full paid artifact pipeline, not product benefit. Its treatment-minus-native deltas were mixed or negative: Codex context efficiency `-0.152984`, recall@300 `-0.003124`, and weighted core coverage `-0.000887`; Claude context efficiency `0`, recall@300 `-0.020973`, and weighted core coverage `+0.005488`. Statistical interpretation starts only after the preregistered 20-instance campaign completes.
