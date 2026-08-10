# SWE-Explore single-run readiness evidence

## Result

The warm-index SWE-Explore pipeline passed its single-instance readiness gate on 2026-08-07. Mass campaigns may now use the repository-owned harness. This result proves orchestration, isolation, LCTK use, artifact validation, and official-scorer compatibility. It does not establish a product-quality delta; one instance has no statistical meaning.

## Pinned identities

| Input | Identity |
|---|---|
| Instance | `pydata__xarray-4629` |
| Repository commit | `a41edc7bf5302f2ea327943c0c48c532b12009bc` |
| SWE-Explore evaluator | `3c12dc5a551937038afcbdb6eb6bbf19f3ddd8c1` |
| SWE-Explore data | commit `bdb0ae45d7c337d9e1dc3ebfe2a0af6bc7c1fbd9`, SHA-256 `dc4f114ececd0bfb987361c26ae5e2440456e2cccb36adfccb09ea5385aec202` |
| SWE-bench Verified source | commit `c104f840cc67f8b6eec6f759ebc8b2693d585d4a`, exported JSONL SHA-256 `abfa2191021f9e503356e273c4c8ad26f6d3dd2e6eab5316147cdde049b9904b` |
| LCTK | `0.1.12`, build commit `954da54e484ca87d50c32ef3f12b808c6a72c6fa` |
| Codex | CLI `0.147.0`, model `gpt-5.6-sol`, effort `high` |
| Claude Code | CLI `2.1.224`, requested and observed model `claude-sonnet-4-6`, effort `high` |

The LCTK project was already fully indexed before measurement. Its preflight reported exact, semantic, graph, and watcher generation `1`, watcher sequence and checkpoint `0`, pending paths `0`, clean detached source commit `a41edc7`, and empty project memory. Index construction remained outside every measured interval.

## Final arms

| Arm | Elapsed | Regions | LCTK calls | Weighted core coverage | Context efficiency | Recall at 300 | First useful hit |
|---|---:|---:|---:|---:|---:|---:|---:|
| Codex native | 36.671 s | 5 | 0 | 0.256337 | 0.865169 | 0.106428 | 1 |
| Codex with LCTK | 131.251 s | 5 | 27 | 0.244255 | 0.833333 | 0.070601 | 1 |
| Claude native | 7.459 s | 1 | 0 | 0.003554 | 1.000000 | 0.010537 | 1 |
| Claude with LCTK | 62.358 s | 2 | 14 | 0.233594 | 1.000000 | 0.038988 | 1 |

Both treatment traces called `lctk_benchmark.project_info` first. Native traces contained no LCTK call. Every trace produced exactly one parseable final block, every returned path and line range existed in the pinned checkout, and the repository remained clean at the declared commit.

All 17 metrics from the repository-owned parity scorer matched the commit-pinned upstream `eval.py` within `1e-12` for all four final results; the maximum observed floating-point delta was `5.56e-17`. The final ignored artifacts are under `.artifacts/swe-explore-acceptance-versioned/`; they are local audit evidence and are not source-controlled.

## Tool isolation

Codex loaded no user configuration and had web search, apps, remote plugins, multi-agent tools, and memories disabled. Its native MCP table was empty. Its treatment MCP table exposed only the 11 tools declared by the benchmark contract and pre-approved only that read-only server surface.

Claude used strict MCP configuration and advertised exactly `Glob`, `Grep`, and `Read` in the native arm. Its treatment added exactly the same 11 LCTK tools. `run_command`, Git tools, persistent-memory tools, web tools, shell tools, and write tools were absent.

OAuth remained owned by each client. The harness stored only the loopback endpoint configuration, did not read or copy a token, and did not launch an IDE.

## Commands proved by the gate

```powershell
.artifacts/swe-explore-benchmark.exe validate --config .artifacts/swe-explore-config.json
.artifacts/swe-explore-benchmark.exe prepare --config .artifacts/swe-explore-config.json --instance INSTANCE_ID
.artifacts/swe-explore-benchmark.exe pair --config .artifacts/swe-explore-config.json --instance INSTANCE_ID --provider codex --output-dir .artifacts/results/INSTANCE_ID/codex
.artifacts/swe-explore-benchmark.exe pair --config .artifacts/swe-explore-config.json --instance INSTANCE_ID --provider claude --output-dir .artifacts/results/INSTANCE_ID/claude
.artifacts/swe-explore-benchmark.exe official-score --config .artifacts/swe-explore-config.json --result RESULT_JSON --python python
```

For a mass campaign, use the manifest and campaign commands documented in [`swe-explore.md`](swe-explore.md). The campaign runner processes the pinned instance list sequentially, safely fetches an absent public GitHub commit into the stable registered workspace, switches only a clean checkout, and waits for LCTK freshness outside the measured interval. Pair order is deterministically counterbalanced by instance and provider. Any invalid dataset join, digest or client drift, dirty checkout, freshness mismatch, client failure, timeout, malformed output, unexpected tool call, or scorer mismatch is fatal. Immutable receipts make completed arms resumable without treating partial attempts as results.
