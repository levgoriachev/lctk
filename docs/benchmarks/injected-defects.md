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

The campaign report carries this as `providers.<provider>.strata`, giving each stratum
the same paired quality deltas, telemetry deltas, bootstrap intervals, and adoption rate
that the provider-level aggregate carries. **That is the view to read.** The
provider-level aggregate is retained, but for an authored corpus its pooled mean is
partly an artifact of how many instances of each stratum we chose to write. With a
handful of pairs per stratum the intervals are wide; that width is the honest size of
the evidence and is expected.

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

## Building the corpus files

Authored defects are written as a JSONL file whose `defect_patch` is relative to that
file's own directory, carrying `instance_id`, `repo`, `base_commit`,
`problem_statement`, `defect_patch`, `stratum`, `causal_note`, and
`author_rationale`; unknown fields are ignored. The build converts them into the two
JSONL shapes the rest of the pipeline already consumes:

```text
swe-explore-benchmark corpus-injected --defects FILE --repo LOCAL_PATH --repo-name NAME \
  --output-explore FILE --output-issues FILE [--report FILE]
```

The emitted `defect_patch` is rewritten relative to the **issues** file, because that is
what every consumer resolves it against, and a path that would have to climb out of that
directory is refused rather than emitted — so the corpus directory stays relocatable and
a dataset row can never name an arbitrary file on the machine running the campaign.

Every rule is fatal and names its instance. A mined commit that fails a check is one of
thousands and is skipped; a skipped authored defect would leave a corpus that no longer
matches the patches its author wrote, and nothing downstream would notice. The build
refuses a patch that does not apply to its pinned commit, one touching more than three
files or changing more than fifty lines, a statement under 25 characters, an
abbreviated base commit, an unknown stratum, and a statement that fails the leak filter.

Verification never touches the repository the defects were authored in: each patch is
applied in a temporary Git worktree that is removed afterwards, so a corpus build cannot
leave the author's checkout dirty.

Ground truth is re-derived from the tree the patch produced, at zero context, rather
than read off the patch's own hunk headers. The same defect written with three lines of
context would otherwise claim six extra lines per hunk as the answer, and the agent
would be scored for finding code the defect never touched.

### The literal-stratum exemption

Rule 3 has exactly one exception, and it is narrow. A `literal` instance exists because
its symptom quotes a distinctive user-visible string, so an identifier that matches
**inside a double- or single-quoted run** of the statement is expected evidence rather
than a leak. Nothing else is exempt: a path leak and a `:line` leak stay fatal in every
stratum, an identifier appearing outside quotation marks is an ordinary leak even here,
and a single quote is only treated as a delimiter when it does not sit inside a word, so
the apostrophe in "doesn't" cannot launder a leak through half a sentence.

Every exemption granted is printed and recorded in the `--report` JSON with the quoted
text that granted it. The exemption is a place the filter was deliberately relaxed, so
it is reviewed by eye rather than counted.

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

## Where the corpus lives, and why it is not in this repository

Defect patches carry context lines from the repository they were authored against.
For a private corpus that makes them proprietary source, and this repository is public,
so the corpus stays under ignored `.artifacts/` and is pinned into a campaign by
SHA-256 exactly as the public SWE-Explore dataset is. The same rule is applied to every
corpus regardless of the source repository's visibility, because a per-repository
exception is the kind of asymmetry that eventually gets applied to the wrong one.

This makes the corpus a durable, unversioned asset: authoring it is real work, losing it
means redoing that work, and nothing in the repository will restore it. Back it up
wherever the private repositories themselves are backed up.

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
not part of the treatment, and a pair whose two arms record different digests is fatal.

### The expected working-tree state

A patched tree is dirty by construction, and the harness has several gates that used to
mean "the checkout is clean". They now all assert against one declared state instead:

- a mined or public instance expects **clean at the base commit** — unchanged behaviour,
  down to the "benchmark repository is not clean" message;
- an injected instance expects **HEAD at the base commit and the working tree digesting
  to the value recorded when the defect was applied**.

The digest covers both `git diff HEAD` and `git status --porcelain=v1
--untracked-files=all`, normalized for line endings. Neither is sufficient alone: the
diff records an edited line but is blind to a file the agent created, and the status
listing names that new file but never shows a changed line.

The two ways of failing the gate are reported separately, because they call for opposite
responses. "The defect patch is no longer applied" means the instance was never prepared,
or was reverted; "the working tree digest is X, want Y" means a tree that *was* prepared
correctly has since been changed. The pre-arm gate additionally proves the tree is
exactly base plus this patch — the patch reverses cleanly and nothing outside the files
it names differs from HEAD — before it adopts the observed digest as the state the
post-arm gate must find unchanged.

`Materialize` keeps its own refusal to switch a dirty checkout. Because a patched
workspace is dirty, the harness records which defect it applied and reverses exactly
that patch before preparing the next instance; a workspace that no longer matches the
recorded state is refused rather than reset, so nobody's work is silently discarded.
Re-applying a patch to the same commit reproduces the digest exactly, and a
re-application that contradicts a digest already attested in a receipt is fatal — that is
what catches a corpus file edited underneath a resumed campaign.

Each arm result records the stratum and the defect digest, so a receipt proves which
tree was measured rather than only which commit it started from.
