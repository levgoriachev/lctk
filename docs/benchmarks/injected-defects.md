# Injected-defect exploration corpus

This is the primary instance generator for measuring whether LCTK reduces the time,
steps, and tokens an agent spends finding code. Instances are constructed: a defect we
author is applied to a pinned commit of a real repository, and the agent is asked to
locate it from a symptom description written the way a user would report it.

It exists because both alternatives are worse. The public SWE-Explore corpus is drawn
from repositories saturated in the target models' training data, which compresses any
effect toward zero ([ADR-0031](../adr/0031-agent-first-tool-surface.md)). Instances
mined from a repository's own fix history — described in [own-corpus.md](own-corpus.md)
and retained as a cross-check — solve contamination but inherit two flaws: the commit
message and pull-request body written by the person who fixed the bug routinely name
the answer, and the filter that removes those leaks biases the surviving sample toward
bugs whose symptom shares no vocabulary with the code.

Authoring the defect removes both. We write the symptom, so it cannot leak by
construction; and we choose the defect, so the corpus can deliberately span the range
of search problems instead of whatever the repository's history happened to contain.

## What is measured, and the honesty condition

Because we choose the defects, we could — accidentally or not — build a corpus whose
tasks all favor the tools we happen to ship. The corpus therefore commits in advance to
four strata chosen to span the plausible outcomes, including two where the client's own
`grep` should be expected to win, and:

**Results are reported per stratum and are never pooled into one number.** A single
headline figure over a corpus we authored would be self-deception. Per-stratum results
answer the question the audit actually left open — not *whether* code intelligence
helps on average, but *which* search problems it helps with — and let a reader weigh
those against the shape of their own work.

## Strata

| Stratum | The search problem | Expected to favor |
|---|---|---|
| `literal` | The symptom quotes a distinctive string that appears verbatim in the source: an error message, a log line, a UI label. | The client's own `grep`. One search finds it. |
| `generic` | The only handle on the defect site is an identifier that appears in many files — a common method or field name. Text search returns a flood; only a parser knows which occurrences are declarations. | `find_symbol` |
| `cross-file` | The symptom appears in one module while the defect is in a different one that it calls, so the site must be reached by following a reference rather than by matching the symptom's words. | `find_symbol` references, `read_span` |
| `family` | Several parallel implementations exist — adapters, handlers, providers — and exactly one is missing a case the others have. The task is to notice the odd one out. | `repository_map`, `file_outline` |

Each stratum must be represented on each repository, and a campaign selects instances
per stratum rather than at random, so a per-stratum readout is always possible.

## Instance record

An injected instance is one JSON object per line in the issues file, carrying two
fields beyond the mined shape:

```json
{
  "instance_id": "flexus-inj__cross-file-03",
  "repo": "flexus",
  "base_commit": "f8290d888...",
  "problem_statement": "Booking confirmations stopped going out ...",
  "defect_patch": "patches/flexus-inj__cross-file-03.patch",
  "stratum": "cross-file"
}
```

The ground truth in the paired explore file is derived mechanically from the patch and
is never hand-written: core regions are the patch's **b-side** hunk ranges — the
coordinates of the tree the agent actually explores, which is the base commit with the
defect applied. This is the opposite side from the mined corpus, where the agent
explores the state before a fix.

A separate audit field records the causal chain from the defect to the symptom. It is
kept with the corpus for review and is never given to an agent.

## Authoring rules

A defect is valid only when all of these hold. They exist because each one, violated,
turns a localization task into either an impossible one or a free one.

1. **The defect is a plausible mistake**, not vandalism: a wrong comparison, a missing
   branch, an argument in the wrong order, a dropped `await`, a stale constant. A change
   no programmer would make produces a symptom no user would report.
2. **The symptom is observable and is written in the user's vocabulary** — what breaks,
   for whom, when. Never a file, a function, a field, a line number, or a stack frame.
3. **The symptom passes the automated leak filter** already built for the mined corpus:
   it is rejected if it contains any path, any `:line` reference, or any code-shaped
   identifier appearing in the changed lines, compared with separators normalized away.
   Authoring by hand does not exempt a statement from the check.
4. **The causal chain is written down** and is short enough to be checked: this defect,
   at this site, produces that symptom by this path. An instance whose chain cannot be
   stated is an instance whose ground truth is a guess.
5. **The patch touches between one and three files** and stays under fifty changed
   lines. Larger defects stop measuring search and start measuring reading.
6. **The stratum is a claim the patch supports.** A `literal` defect must actually carry
   a distinctive string into the symptom; a `cross-file` defect must genuinely separate
   the symptom's module from the defect's.

## Harness contract

Preparation for an injected instance extends the ordinary sequence:

1. Materialize the pinned base commit into the registered benchmark workspace, refusing
   a dirty checkout exactly as today.
2. Apply the defect patch, and record the resulting working-tree state as a digest over
   the tracked diff and the untracked-file list.
3. Wait, outside every measured interval, for the exact, semantic, graph, and watcher
   generations to converge on a generation strictly newer than the pre-patch baseline —
   the index must describe the broken tree, not the clean one.
4. After each arm, verify the working tree still matches the recorded digest. An agent
   that edited the repository is still fatal; the expected state is simply "base commit
   plus this patch" rather than "clean at base commit".

Both arms of a pair see the identical patched tree. The defect is part of the instance,
not part of the treatment.
