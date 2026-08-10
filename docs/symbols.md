# Symbols and syntax

LCTK answers two questions about a source file without a compiler, an interpreter, or the project's installed dependencies: **what does this file declare**, and **does it parse**.

Both come from a real syntax tree. The engine is Tree-sitter, chosen on measured evidence in [Slice 4.1](spikes/symbol-backend-evaluation-results.md) and recorded in [ADR-0019](adr/0019-tree-sitter-symbol-layer.md).

## What an outline is a claim about

`file_outline` reads the file from the read-only project mount and parses it. It does not consult the search index, so:

- there is nothing to flush and no debounce window to wait out — a file saved a moment ago is described correctly;
- the answer carries a **content digest** rather than an index generation, because there is no generation it could be behind;
- freshness is not reported, because the concept does not apply. The answer describes the bytes named by that digest.

This is a stronger claim than a search result makes, and it is stronger for the same reason: a search answers from an index that lags the disk by a debounce window, while an outline answers from the disk.

What it is **not** a claim about is unchanged from [ADR-0018](adr/0018-the-index-describes-the-disk.md): an edit that has not been written to disk does not exist here either, whoever is holding it.

## What a declaration carries

| | |
|---|---|
| `name` | the identifier as written |
| `kind` | `function`, `method`, `type`, `interface`, `struct`, `enum`, `class`, `field`, `constant`, `variable`, `module`, `macro`, or `other` |
| `start_line`, `end_line` | 1-based and inclusive |
| `start_byte`, `end_byte` | the declaration's extent, so it can be shown rather than located |
| `container` | the enclosing declaration's name, empty at the top level |
| `depth` | how deeply nested, so a flat list renders as a tree |
| `signature` | the declaration's own first line, trimmed and bounded to 200 bytes |

The vocabulary is LCTK's, not a grammar's. A caller should not have to learn one parser's node names, and a kind no configured grammar can produce is not in the list.

**Containment is computed from the extents, not from a name path.** A declaration is inside another when its bytes are. That is what makes a constant declared inside a method report that method as its container: nothing in the constant's own syntax says where it lives.

**The extent is the declaration and excludes what precedes it.** A doc comment above a type is not part of the type. One consequence is worth knowing: in Go a `const` or `var` *spec* is the declaration, not the enclosing `const (...)` group, so a single `const Limit = 10` reports a signature of `Limit = 10` rather than `const Limit = 10`. The `kind` field already says which it is, and using the spec is what makes a grouped declaration report its members individually.

**Containment is the innermost declaration**, and it is sometimes tighter than expected. In `other := Needle{Size: 1}` inside a function `Use`, the occurrence of `Needle` is inside the declaration of `other`, which is inside `Use`. Both are true; only one can be the innermost, and the innermost is what is reported.

**Parameters, receivers, and `range` bindings are not reported as declarations.** They are declarations, and including them would put every parameter of every function into an outline — noise in the answer a reader actually asked for, when the declaration's own signature already shows them. A lookup for such a name reports zero declarations and its uses, each placed in the declaration that holds it, which is enough to find it.

## Precision

An answer about **one file** states `"precision": "syntax"`:

- declarations are what the file's syntax says they are;
- nothing is type-resolved;
- nothing outside the file is consulted.

An answer **across files** states `"precision": "name_match"`, which is weaker and named differently on purpose. Within one file the syntax settles what is a declaration. Across files nothing here resolves *which* declaration a use refers to, so two unrelated things that share a name both appear.

Both are stated in the answer rather than only here, because a caller that reads a name-matched answer as a type-resolved one will act on it.

## Finding a name across the project

The MCP surface exposes this as one tool, `find_symbol` ([ADR-0031](adr/0031-agent-first-tool-surface.md)): definitions first, then references, with each location saying which it is. Internally the service still answers the two questions separately.

The work is split, and the split is the design:

- **the index narrows.** It knows which files contain the letters, as a whole word — so `Read` does not bring in `ReadAll`.
- **the parser decides.** Only a syntax tree knows whether those letters are an identifier or prose. A file whose only occurrence is in a comment or a string contributes nothing to the answer.

Measured against this repository: a doc comment reading `// MaxDeltaGenerations is how many delta builds…` sits directly above the field it documents, and the lookup reports the field's declaration and fifteen uses — and not the comment.

Because the index chooses the candidates, this answer **is** index-dependent, unlike an outline. It carries the index generation and the host's freshness verdict, and a search-style flush runs first for the same reason: a file the index has not caught up with is a file the lookup never opens.

### Bounds, and why they are reported

Every candidate file is parsed, so the work is proportional to how many files hold the name. At most **200** files are examined, and `find_references` accepts a lower `max_files`.

When the candidate list is cut, the answer says `truncated`. That flag matters more than it looks: without it, a caller reading a bounded answer concludes "nothing else refers to this", which is the one wrong conclusion available here. `err` in this repository, bounded to 25 files, reports 1008 occurrences and `truncated: true` in 0.75 s.

`skipped_unsupported_language` and `skipped_unreadable` count candidates that were never examined — a language with no grammar, or a file that vanished between the index and the read. They are counted rather than dropped, because a partial answer that looks complete is worse than a smaller one that says so.

### A name is not a pattern

The name reaches a regular expression inside the service, so anything that is not a plausible identifier is **refused** rather than escaped. Letters, digits, underscores, dollars, and non-ASCII letters are accepted; `Widget|Consume` is not. Escaping it would quietly answer a question the caller did not ask.

## The syntax verdict

`syntax.valid` says whether the file parses. `syntax.errors` counts the broken *regions* — places to look, not the size of the damage — and `syntax.first_error_line` says where to start.

A file that does not parse still reports the declarations that did. An agent asking about the file it is midway through editing is the ordinary case, not an unusual one.

**The verdict is published per language, and `syntax.reported: false` means unknown rather than fine.** That distinction is not caution for its own sake. The C and C++ grammars have no preprocessor, and measured against real projects, 58 of 69 files from `zlib` and 134 of 438 from `nlohmann/json` parse with errors while compiling perfectly. Across 426 real files in Go, Python, Rust, JavaScript, and TypeScript, none did. Publishing a universal verdict would mean reporting a defect in most of a C project on the first look.

Anything beyond syntax belongs to the project's own toolchain. `build`, `test`, and `lint` already run in a container the project chose, under the policy in [ADR-0017](adr/0017-command-policy-and-the-runner.md). LCTK does not implement a second, worse type checker.

## Languages

| Language | Extensions | Syntax verdict |
|---|---|---|
| Go | `.go` | yes |
| Python | `.py`, `.pyi` | yes |
| Rust | `.rs` | yes |
| JavaScript | `.js`, `.mjs`, `.cjs`, `.jsx` | yes |
| TypeScript | `.ts`, `.mts`, `.cts` | yes |
| TSX | `.tsx` | yes |
| C | `.c`, `.h` | **no** — see above |
| C++ | `.cc`, `.cpp`, `.cxx`, `.hh`, `.hpp`, `.hxx` | **no** — see above |

A language with no configured grammar is **explicitly refused**, with `LANGUAGE_UNSUPPORTED` naming what this build does understand. An empty outline would read as "this file declares nothing", which is a different and wrong claim.

`project_info` reports `outline_languages`, so a caller learns the boundary by asking rather than by being refused on a file. It reports what the project's own service advertises, not what the host build expects: a project whose container predates the symbol layer answers nothing there, and the symbol tools are then not offered at all.

TSX is a separate grammar rather than a dialect of TypeScript: the two disagree about how `<T>` is read, so one cannot serve both.

A `.h` is read as **C**. The extension genuinely does not say which language it holds, and C is the reading that parses a C header correctly. A C++ header in a `.h` parses with errors — which costs nothing here, because no syntax verdict is published for either language.

### What each language reports as a declaration

The set is per language and follows that language's own idea of a declaration, not a lowest common denominator. Some entries are worth naming because their absence would be surprising:

- **C and C++**: `#define` and function-style macros. In C a macro is often exactly the declaration a reader is looking for.
- **Python**: module-level and class-level assignment. A configuration module is mostly assignment, and an outline reporting nothing about one would be useless.
- **Go**: `var`, `const`, and `:=`, including inside a function body. `:=` is how most Go variables are declared.
- **Rust**: `let` bindings, enum variants, and trait method signatures.
- **JavaScript and TypeScript**: a `const` bound to an arrow function is reported as a function and one bound to anything else as a variable — the grammar node is the same and only the pattern knows which.

A **Rust `impl` block** is reported as a declaration of kind `implementation`, and it gives the methods inside it the type as their container. It is not counted as a declaration *of that type*: the word `Config` in `impl Config` is a use of a type declared elsewhere, and reporting it otherwise would make `find_definition` say a type is declared in two places.

A **struct, union, enum, or class in C and C++** is reported only where it has a body. In C, `struct Widget *w` in a parameter list is syntactically a struct declaration carrying the name, so matching it would report a *use* as a declaration. The cost is that a forward declaration is not reported — an omission, where the other was a false claim.

## What may be read

The store decides, using the same rules that decide what is indexed. That is one authority rather than two, and it is why an outline cannot reach a file the index would not hold:

- a path that is not project-relative is refused with `INVALID_PATH`;
- a symbolic link is refused rather than followed;
- version-control metadata is never readable;
- a file the project's ignore rules exclude answers `FILE_NOT_FOUND` — **the same answer as a file that is not there**, so the difference between two refusals cannot be used to map what exists outside the caller's scope.

## Bounds

| | |
|---|---|
| Per-file parse budget | 5 s, `LCTK_SYMBOL_BUDGET` |
| Largest file outlined | 4 MiB, `LCTK_SYMBOL_MAX_FILE_BYTES` |
| Files parsed at once | the project's resource mode, `LCTK_SYMBOL_PARALLELISM` |

The parse bound is wall clock rather than a byte count because the cost is not proportional to size: a pathological construct in a file of ordinary length is what holds a parser, and the container it would hold is the project's own. A file abandoned to the budget reports `PARSE_INCOMPLETE`, never an empty outline — "not analysed" and "declares nothing" must not arrive as the same answer.

The size bound is deliberately larger than the index's own 1 MiB file limit, because the two exist for different reasons. A large file costs the index space in every generation it appears in; an outline costs one parse and is not stored.

### Why the concurrency bound is not an optimization

A wall-clock budget is not load-independent, and that turns out to matter. Measured on a 920 KiB C++ header in a container limited to two CPUs and 2 GiB:

| Concurrent parses | Peak memory | Answered | Refused as `PARSE_INCOMPLETE` |
|---|---|---|---|
| 16, unbounded | 269 MiB | 16 | 0 |
| 32, unbounded | 522 MiB | 32 | 0 |
| 64, unbounded | **675 MiB** | **2** | **62** |
| 16, bound 2 | 96 MiB | 16 | 0 |
| 32, bound 2 | 97 MiB | 32 | 0 |
| 64, bound 2 | **97 MiB** | **64** | **0** |

Unbounded, memory grows with whatever arrives together, and past the CPU allowance a file that parses in a third of a second starts exceeding a five-second budget — so the service refuses perfectly ordinary files because it is busy. That is a correctness failure wearing a resource failure's clothes.

Bounded, memory is flat and every request is answered. At 32 concurrent the bound costs nothing in wall clock (5960 ms against 6061 ms); at 64 it costs latency and returns 64 answers instead of 2.

The bound comes from the project's **resource mode** — the same figure that caps index work, because it answers the same question: how much of this machine the project may spend.

| Mode | Container CPUs | Files parsed at once |
|---|---|---|
| `quiet` | 1 | 1 |
| `normal` | 2 | 2 |
| `fast` | no limit | the machine's processors |

`fast` sets no CPU limit, so the machine's processor count is used. That is not a compromise: past the number of things that can actually parse in parallel, more concurrency adds memory and latency and never throughput.

A request that waits and is then abandoned gets `PARSE_BUSY`, which is **retryable** — the only refusal here that waiting fixes. The file is fine and the answer exists; the project was busy at that moment. Reporting it as `PARSE_INCOMPLETE` would be a claim about the file.

## Lifecycle

The symbol layer has none of its own, and that is the design rather than an omission.

- **Nothing is persisted.** No symbol table, no cache, no index of its own. There is therefore nothing to invalidate when a file changes, nothing to rebuild after a restart, and no way for an answer to be stale: every answer is produced from the bytes on disk at the moment it is asked for.
- **No warm-up.** Measured live: an outline answered **205 ms after the project's listener first answered**, returning 207 declarations, *while the search index was still building*.
- **No per-language process.** One engine in the process that already exists, so the container lifecycle and resource modes govern it unchanged. There is nothing to supervise, restart, or bound separately.

A cache was considered and deliberately not added. A Go file in this repository parses in about 3 ms; a cache would trade a correctness risk — a stale answer about a file being edited, which is the case these tools exist for — for nothing worth having.
