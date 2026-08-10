# Own-corpus exploration benchmark

This document is the measurement contract for instances built from a repository's
own fix history, rather than from the public SWE-Explore dataset. It exists
because [ADR-0031](../adr/0031-agent-first-tool-surface.md) identified training-data
contamination as a false-negative bias in the [SWE-Explore campaigns](swe-explore.md):
every SWE-bench Verified repository is saturated in the training data of the target
models, so a model that can localize a fix from memory needs no exploration tool at
all, and the achievable treatment effect is compressed toward zero regardless of the
product's real value.

The value proposition of a warm index peaks on code the model has never seen. That
is what this corpus measures.

## Corpora and their contamination tiers

The tier is the claim being made about model prior knowledge, and it is the reason
each repository is in the set. It is recorded with every campaign because it is the
main thing that makes results from different corpora incomparable.

| Corpus | Tier | Evidence for the tier |
|---|---|---|
| `smallcloudai/flexus` | **unseen** | Private since creation on 2025-05-13; never published. ~2,760 tracked files, ~6,800 commits, ~1,870 of them in 2026. Python, TypeScript/React, Rust, SQL, GraphQL. |
| `JegernOUTT/refact` | **partially known** | A fork of the public `smallcloudai/refact` (created 2023-04-19, ~3,500 stars), whose architecture and older code the models have certainly seen. The fork's own work — over a thousand commits after 2026-05-30 — is later than the model cutoff. |
| `lev-goryachev/lctk` | **unseen** | Public but created 2026-07-25 with no stars and no downstream copies, so it postdates the target models' training data. Go, and the only corpus that exercises the derived Go import graph. |

The primary product readout comes from the unseen tier. The partially-known tier is
a deliberate contrast: if a treatment effect appears on unseen code and disappears on
code the model already knows, that is evidence about *when* the product helps, which
is more useful than a single pooled number. Corpora are never pooled into one score.

## Instance construction

An instance is built from one fix commit. The agent explores the commit's parent —
the repository exactly as it stood while the bug was live — and is scored on whether
it points at the lines the fix went on to change.

1. **Candidate commits.** A single-parent, non-merge, non-revert commit whose subject
   begins with a fix marker (`fix`, `fix(scope):`, `bugfix`, `hotfix`).
2. **Path filtering.** Lockfiles, generated output, vendored trees, build directories,
   migrations, and binary assets are never ground truth. Test files are eligible but
   classified separately.
3. **Size bounds.** Between 1 and 8 eligible non-test source files and between 3 and
   400 changed lines. A commit that rewrites two hundred files is a refactor, and
   "find the code" has no meaningful answer for it.
4. **Ground truth in pre-fix line numbers.** Regions come from the `a`-side hunk
   ranges of the diff, because those are the coordinates of the checkout the agent
   actually explores. A hunk that only inserts lines yields a one-line anchor at the
   insertion point: where the code should have gone is a real localization target.
   Adjacent regions are merged and every region is clamped to the file's real length
   at the parent commit.
5. **Core, optional, and main files.** Non-test source regions are core. Test-file
   regions are optional, matching the upstream split where tests are evidence rather
   than the fix site. The single non-test file carrying the most changed lines is the
   main file, which the weighted-coverage metric weights above the rest.
6. **Base commit.** The fix commit's parent.

## The problem statement, and why it is filtered

This is the part that decides whether the benchmark measures anything.

A commit subject and a pull-request body are written *after* the fix, by the person
who found it, and they routinely name the answer. One real example from the Flexus
history states the failing function and the exact `path:line` of the defect in its
opening paragraph. An agent given that text does not need to explore, and a benchmark
built on it measures nothing.

The statement is therefore built from the reporter's words where they exist, and
mechanically checked for leaks:

- **Preferred source: the linked ticket title.** Where the commit body links an issue
  tracker whose URL carries a human-readable slug, the slug decodes to the symptom as
  the reporter described it — for example *"Session never renews in a background tab
  403 storm until the user clicks the tab"*. That is a genuine bug report: it says what
  broke, not where.
- **Fallback: the commit subject**, with conventional-commit prefixes and trailing
  issue references stripped.
- **Leak filter, applied to every candidate.** An instance is dropped when its
  statement contains any ground-truth path or file basename, any `:line` reference, or
  any code-shaped identifier that appears in the lines the fix changed. Prose words are
  not identifiers; `dig_out_sesh` and `isSessionDeadError` are.
- Statements shorter than 25 characters or longer than 2,000 are dropped as unusable.

Every rejection is recorded with its reason, so the selection is auditable rather than
a matter of trust.

## Scoring authority

Instances are emitted in the exact record shapes the existing pipeline already
consumes, so the dataset join, the seventeen metrics, the campaign runner, the
immutable receipts, and the paired telemetry inference work unchanged.

The authority differs by corpus, and this is a real limitation rather than a
formality:

- For **SWE-Explore instances**, the pinned upstream `eval.py` remains the publication
  authority, and the repository-owned scorer is parity evidence.
- For **own-corpus instances**, no upstream evaluator exists, so the repository-owned
  scorer is the authority. Its formulas are the same code that matches upstream within
  `1e-12` on SWE-Explore rows, which is the strongest available evidence that the
  metric definitions did not drift.

Ground truth here is the fix that shipped. It is an approximation of "the code you had
to understand", in the same way the upstream benchmark's trajectory-derived ground
truth is: a competent exploration may legitimately read code the fix did not change,
and a fix may touch a line that required no understanding at all. Results are
comparable across arms of one campaign, never across corpora or against the upstream
leaderboard.

## Running a campaign

Corpus construction is a one-time, unpaid step:

```text
swe-explore-benchmark corpus --repo LOCAL_PATH --repo-name NAME \
  --since YYYY-MM-DD --count N --seed TEXT \
  --output-explore EXPLORE.jsonl --output-issues ISSUES.jsonl --report REPORT.json
```

The emitted files are then pinned by SHA-256 in a campaign configuration exactly as
the public dataset is, and the campaign runs through the ordinary
[manifest and campaign commands](swe-explore.md). A private repository is reachable
because the configuration may declare a local source for it; the harness still refuses
a dirty checkout and still verifies that `HEAD` equals the declared base commit after
switching.

Because these instances are generated rather than published, the emitted corpus files
are part of the campaign's identity: regenerating them with different bounds, a
different window, or a different seed produces a different measurement and therefore a
new campaign, never a resumed one.
