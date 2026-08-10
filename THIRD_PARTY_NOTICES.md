# Third-Party Notices

LCTK's host executable currently has these direct Go dependencies:

- `github.com/modelcontextprotocol/go-sdk v1.7.0`;
- `github.com/fsnotify/fsnotify v1.10.1`;
- `github.com/pelletier/go-toml/v2 v2.4.3`;
- `golang.org/x/sys v0.47.0`;
- `gopkg.in/yaml.v3 v3.0.1`.

Their transitive versions and module checksums are recorded in `go.mod` and `go.sum`.

The tracked Slice 0.3 evidence module under `spikes/search-backend-evaluation` directly depends on:

- `github.com/sourcegraph/zoekt v0.0.0-20260724095353-2b2ce2e398e6` (Apache-2.0).

Its transitive versions and checksums are isolated in that directory's `go.mod` and `go.sum`. The harness is research evidence, not part of the current `lctk` executable.

The tracked Slice 4.1 evidence module under `spikes/symbol-backend-evaluation` directly depends on:

- `github.com/tree-sitter/go-tree-sitter v0.25.0` (MIT), which vendors the Tree-sitter C library (MIT);
- `github.com/tree-sitter/tree-sitter-go v0.25.0`, `tree-sitter-python v0.25.0`, `tree-sitter-javascript v0.25.0`, `tree-sitter-rust v0.24.2`, `tree-sitter-c v0.24.2`, `tree-sitter-cpp v0.23.4`, and `tree-sitter-typescript v0.23.2` (each MIT);
- `github.com/mattn/go-pointer v0.0.1` (MIT).

It also drives Universal Ctags (GPL-2.0) as a separate executable installed in the harness image. Ctags is a candidate under evaluation, not a dependency of anything LCTK distributes, and the [decision](docs/adr/0019-tree-sitter-symbol-layer.md) does not select it.

The distributed `code-intel` image additionally includes:

- `github.com/sourcegraph/zoekt` (Apache-2.0);
- Tree-sitter and the eight grammar modules listed above (MIT);
- `github.com/ncruces/go-sqlite3 v0.35.3` and its embedded SQLite runtime (MIT and public domain respectively).

Semantic bootstrap installs, after immutable digest verification:

- `ggml-org/llama.cpp` server image (MIT), pinned by OCI index digest;
- `nomic-ai/nomic-embed-text-v1.5-GGUF`, Q4_K_M (Apache-2.0), pinned by repository commit and SHA-256.

When the machine owner selects the NVIDIA GPU inference distribution, setup additionally installs after immutable identity verification:

- `ggml-org/llama.cpp` CUDA server image (MIT), pinned to build `b10257` and an OCI index digest;
- NVIDIA CUDA runtime libraries carried by that image under the [NVIDIA Deep Learning Container License](https://docs.nvidia.com/deeplearning/frameworks/container-release-notes/license.html);
- `nvidia-container-toolkit-base 1.19.1-1` (Apache-2.0), pinned by exact RPM NEVRA, byte length, and SHA-256 and installed only inside the LCTK-owned Podman machine.

The CPU and NVIDIA GPU distributions use the same pinned Apache-2.0 embedding model. LCTK does not redistribute a Windows NVIDIA driver and does not install the toolkit into Windows or another WSL distribution.

The Windows installer downloads and verifies these separately versioned Apache-2.0 runtime components from the signed release manifest:

- Podman remote client `v5.8.2`, including `podman.exe`, `gvproxy.exe`, and `win-sshproxy.exe`;
- Podman Machine OS `v5.8.2` for WSL2.

LCTK invokes these components privately and does not install or redistribute Podman Desktop.

Each dependency remains subject to its own license. This file will contain the generated complete attribution inventory before the first distributed release. Current workflow archives are non-publishing dry runs; the pre-alpha repository does not yet publish a GitHub Release, production binary, or container image.
