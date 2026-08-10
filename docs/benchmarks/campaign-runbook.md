# Free-choice campaign runbook

The sequence from a merged code change to a defensible result. It exists because the
steps have hard ordering constraints that are easy to get wrong once and then not
notice: a campaign run against a stale installation, or against an index built before
the defect was applied, produces numbers that look fine and mean nothing.

Everything here is unpaid setup except the final step.

## 0. Preconditions

- The product change under test is merged, and `go test ./...` is green on it.
- The two benchmark repositories are cloned into isolated workspaces, so no campaign
  ever switches the checkout of a working tree someone is using:
  `.artifacts/own-corpus/repos/flexus` and `.artifacts/own-corpus/repos/refact`.
- Defect corpora exist under `.artifacts/own-corpus/defects/` with their patches.

## 1. Build and install the product being measured

The installed LCTK is whatever was last released, not what is in the working tree.
Measuring the wrong build is the single easiest way to waste a paid campaign.

```powershell
.\scripts\build-local-rc.ps1 -Version <version>
```

The script refuses a dirty worktree by design, and builds the code-intel image through
LCTK's own managed Podman machine, so that machine must be running first. Install the
resulting setup executable over the existing installation, then confirm the version the
daemon actually reports — not the version you think you built.

## 2. Register and index both repositories

Register each clone as a project and start it. Indexing is installation work and stays
outside every measured interval, but it is not free in wall-clock: a comparable
repository took roughly forty minutes to settle on this hardware, dominated by
semantic-chunk embedding. Plan the calendar around it, and let both projects finish
before the first paid arm.

## 3. Generate the corpus files

Injected instances are already authored; convert them and their patches into the paired
JSONL the harness consumes, then pin both files by SHA-256 in the campaign
configuration. Mined instances, if used as the cross-check, come from the `corpus`
command described in [own-corpus.md](own-corpus.md).

Ground truth for injected instances is derived from the patch, never hand-written; see
[injected-defects.md](injected-defects.md).

## 4. Preflight one instance end to end before spending anything

Run a single instance through both arms of one provider and read the raw trace by hand.
Confirm all of:

- the working tree at measurement time is the base commit **with the defect applied** —
  not the clean commit, and not a half-applied patch;
- exact, semantic, graph, and watcher generations agree, and are strictly newer than the
  pre-patch baseline, so the index describes the broken tree;
- the treatment arm's answers are the new text contract and carry no `/workspace` path;
- the free-choice prompt contains no mention of MCP, so the arms differ only in what is
  available;
- the problem statement really does not name the defect site.

A failure here is cheap. The same failure discovered after twenty paid pairs is not.

## 5. Run the campaign

Build an immutable manifest, then run it. Selection is per stratum rather than at
random, so that a per-stratum readout is always possible.

Campaign identity covers the product build, the client versions and models, the prompt
mode, the treatment tool list, and the corpus files. Changing any of them is a new
campaign, never a resumed one.

## 6. Read the result honestly

- Report **per stratum, never pooled**. A single number over a corpus we authored would
  be self-deception, and the open question after the audit is not whether code
  intelligence helps on average but which search problems it helps with.
- Report Codex and Claude separately. They were never comparable to each other.
- The primary readout is the paired delta with its confidence interval on elapsed time,
  billed tokens, and tool calls, with answer quality required not to regress — that is
  the product claim. Quality metrics alone were the wrong primary in the earlier
  campaigns.
- Report the adoption rate. Under free choice, an agent that ignores the tools is
  reporting a result about the product, not a missing measurement.
- Compare the injected corpus against the mined cross-check. If the two point in
  opposite directions, the authored corpus is unrepresentative and the injected result
  must not be published on its own. Note the caveat measured in
  [own-corpus.md](own-corpus.md): the sampler's output is not usable as this check
  without first hand-picking the sound instances, because most mined statements
  paraphrase the fix.

## Known gaps to close before this is a publication-grade instrument

- Statistical power has never been estimated from real variance; the first campaign's
  spread is what will size the second.
- A competitor control arm — the identical harness pointed at a different code-search
  server — has not been run, so a null result cannot yet distinguish "this product does
  not help" from "no tool of this shape helps here".
