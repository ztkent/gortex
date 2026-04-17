# Gortex

[![CI](https://github.com/zzet/gortex/actions/workflows/ci.yml/badge.svg)](https://github.com/zzet/gortex/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/zzet/gortex)](https://goreportcard.com/report/github.com/zzet/gortex)
[![Latest release](https://img.shields.io/github/v/release/zzet/gortex?logo=github&sort=semver)](https://github.com/zzet/gortex/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzet/gortex.svg)](https://pkg.go.dev/github.com/zzet/gortex)
[![Sigstore signed](https://img.shields.io/badge/sigstore-signed-66D4FF?logo=sigstore&logoColor=white)](#verifying-releases-supply-chain-security)
[![SLSA 3](https://img.shields.io/badge/SLSA-Level%203-green)](https://slsa.dev/spec/v1.0/levels#build-l3)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/zzet/gortex/badge)](https://scorecard.dev/viewer/?uri=github.com/zzet/gortex)
[![VirusTotal](https://img.shields.io/badge/VirusTotal-0%2F91-brightgreen?logo=virustotal)](https://www.virustotal.com/gui/url/00e1094b39c9bd7db4d5a179b1d56173f85c915075057fd3cc64bfbb9b735b11/detection)



Code intelligence engine that indexes repositories into an in-memory knowledge graph and exposes it via CLI, MCP Server, and web UI.

Built for AI coding agents (Claude Code, Kiro, Cursor, Windsurf, Copilot, Continue.dev, Cline, OpenCode, Antigravity) — one `smart_context` call replaces 5-10 file reads, cutting token usage by ~94%.

![Gortex Web UI — force-directed knowledge graph visualization](assets/graph.png)

## Features

- **Knowledge graph** — every file, symbol, import, call chain, and type relationship in one queryable structure
- **Multi-repo workspaces** — index multiple repositories into a single graph with cross-repo symbol resolution, project grouping, reference tags, and per-repo scoping
- **33 languages** — Go, TypeScript, JavaScript, Python, Rust, Java, C#, Kotlin, Swift, Scala, PHP, Ruby, Elixir, C, C++, Dart, OCaml, Lua, Zig, Haskell, Clojure, Erlang, R, Bash/Zsh, SQL, Protobuf, Markdown, HTML, CSS, YAML, TOML, HCL/Terraform, Dockerfile
- **47 MCP tools** — symbol lookup, call chains, blast radius, community/process discovery, contract detection, unified `analyze` (dead code, hotspots, cycles), scaffolding, inline editing, symbol renaming, multi-repo management, agent feedback loop, context export, graph-validated config hygiene (`audit_agent_config`), opening-move routing (`plan_turn`), narrative repo overview (`get_repo_outline`), test-coverage gaps (`get_untested_symbols`), and 18 agent-optimized tools
- **Semantic search** — hybrid BM25 + vector search with RRF fusion. Hugot (pure-Go ONNX runtime with MiniLM-L6-v2) is bundled by default and auto-downloads the model on first use — zero-config, no native dependencies. GloVe word vectors remain as fallback. Optional build tags switch to ONNX or GoMLX for higher throughput
- **LSP-enriched call-graph tiers** — every edge carries an `origin` tier (`lsp_resolved` / `lsp_dispatch` / `ast_resolved` / `ast_inferred` / `text_matched`); pass `min_tier` to `get_callers`, `find_usages`, `find_implementations`, etc. to restrict results to compiler-verified edges for high-stakes refactors
- **MCP progress notifications** — long-running indexing and track_repository calls emit `notifications/progress` with stage messages (walking files → parsing → resolving → semantic enrichment → search index → contracts → done) so hosts show real progress bars on large repos
- **Type-aware resolution** — infers receiver types from variable declarations, composite literals, and Go constructor conventions to disambiguate same-named methods across types
- **On-disk persistence** — snapshots the graph on shutdown, restores on startup with incremental re-indexing of only changed files (~200ms vs 3-5s full re-index)
- **Bridge Mode** — HTTP/JSON API exposing all MCP tools for IDE plugins, CI tools, and web UIs with CORS support and tool discovery endpoint
- **Semantic enrichment** — pluggable SCIP, go/types, and LSP providers upgrade edge confidence from ~70-85% (tree-sitter) to 95-100% (compiler-verified). Additive — graceful degradation when external tools unavailable
- **Agent feedback loop** — unified `feedback` tool (`action: "record"` / `"query"`) lets agents report which symbols were useful/missing. Cross-session persistence improves future `smart_context` quality via feedback-aware reranking
- **Context export** — `export_context` tool + `gortex context` CLI render graph context as portable markdown/JSON briefings for sharing outside MCP (Slack, PRs, docs, non-MCP AI tools)
- **ETag conditional fetch** — content-hash based `if_none_match` on source-reading tools avoids re-transmitting unchanged symbols during iterative editing
- **Token savings tracking** — per-call `tokens_saved` field on source-reading tools + session-level metrics in `graph_stats` (calls counted, tokens returned, tokens saved, efficiency ratio)
- **GCX1 compact wire format** — published, round-trippable text format for MCP tool responses. Opt-in per call via `format: "gcx"` on 13 tools. **Median −27.4% tiktoken savings** vs JSON across a 20-case benchmark (best case −38.3%), 100% round-trip integrity. Spec: [`docs/wire-format.md`](docs/wire-format.md). TypeScript decoder on npm: [`@gortex/wire`](https://www.npmjs.com/package/@gortex/wire). Reproducible harness: [`bench/wire-format/`](bench/wire-format/)
- **7 MCP resources** — lightweight graph context without tool calls
- **3 MCP prompts** — `pre_commit`, `orientation`, `safe_to_change` for guided workflows
- **Two-tier config** — global config (`~/.config/gortex/config.yaml`) for projects and repo lists, per-repo `.gortex.yaml` for guards, excludes, and local overrides
- **Guard rules** — project-specific constraints (co-change, boundary) enforced via `check_guards`
- **Watch mode** — surgical graph updates on file change across all tracked repos, live sync with agents
- **Web UI** — Sigma.js force-directed visualization with node size proportional to importance
- **IMPLEMENTS inference** — structural interface satisfaction for Go, TypeScript, Java, Rust, C#, Scala, Swift, Protobuf
- **PreToolUse + PreCompact + Stop hooks** — PreToolUse enriches Read/Grep/Glob with graph context and redirects to Gortex MCP tools; matching `Task` also briefs spawned subagents with an inline tool-swap table + task-scoped `smart_context` so subagents don't fall back to grep/Read. PreCompact injects a condensed orientation snapshot (index stats, recently-modified symbols, top hotspots, feedback-ranked symbols) before Claude Code compacts the conversation. Stop runs post-task diagnostics (`detect_changes` → `get_test_targets`, `check_guards`, `analyze dead_code`, `contracts check` on modified symbols) so the agent self-corrects before handoff. All hooks degrade silently when the bridge is unreachable
- **Long-living daemon (optional)** — `gortex daemon start` runs a single shared process that holds the graph for every tracked repo. Each Claude Code / Cursor / Kiro window connects as a thin stdio proxy over a Unix socket, getting per-client session isolation (recent activity, token stats) + cross-repo queries by default. Live fsnotify watching on every tracked repo so file edits flow into the graph without manual reload. `gortex init --global` sets up user-level config; `gortex daemon install-service` installs a LaunchAgent (macOS) or systemd `--user` unit (Linux) so the OS supervises lifecycle and auto-starts at login — no sudo required. Binaries fall back to embedded mode if the daemon isn't running; the feature is additive
- **Benchmarked** — per-language parsing, query engine, indexer benchmarks
- **Per-community skills** — `gortex skills` auto-generates SKILL.md per detected community with key files, entry points, cross-community connections, and MCP tool invocations for Claude Code auto-discovery
- **Eval framework** — SWE-bench harness for A/B benchmarking tool effectiveness with Docker-based environments and multi-model support
- **Zero dependencies** — everything runs in-process, in memory, no external services

## Installation

Pre-built binaries are published to [GitHub Releases](https://github.com/zzet/gortex/releases) for linux/amd64, linux/arm64, darwin/amd64 (Intel Mac), and darwin/arm64 (Apple Silicon). Every release is **cosign-signed**, ships **SLSA-3 provenance**, and is **VirusTotal-scanned** — see [Verifying releases](#verifying-releases-supply-chain-security) below. Windows support is planned.

**New to Gortex?** After installing, see [docs/onboarding.md](docs/onboarding.md) for the 15-minute walkthrough: `gortex init` → start the server → verify your AI assistant uses graph tools → what to do if it doesn't.

### macOS — Homebrew

```bash
brew install zzet/tap/gortex
```

Homebrew strips the `homebrew-` prefix from tap repositories, so `zzet/homebrew-tap` is installed as `zzet/tap`. Updates via `brew upgrade`. No Gatekeeper prompt — `brew` doesn't set the quarantine attribute on downloads.

### Linux — Debian / Ubuntu (.deb)

```bash
ARCH=$(dpkg --print-architecture)  # amd64 or arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.deb"
sudo dpkg -i "gortex_linux_${ARCH}.deb"
```

### Linux — RHEL / Fedora / CentOS (.rpm)

```bash
ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.rpm"
sudo rpm -ivh "gortex_linux_${ARCH}.rpm"
```

### Linux — Alpine (.apk)

```bash
ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.apk"
sudo apk add --allow-untrusted "gortex_linux_${ARCH}.apk"
```

### Direct binary download (any Linux or macOS)

```bash
# Pick the right asset for your OS/arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')  # linux or darwin
ARCH=$(uname -m)
case $ARCH in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_${OS}_${ARCH}.tar.gz"
tar -xzf "gortex_${OS}_${ARCH}.tar.gz"
sudo mv gortex /usr/local/bin/
```

On macOS, if you downloaded via browser (not `curl`), remove the quarantine flag once:

```bash
xattr -d com.apple.quarantine /usr/local/bin/gortex
```

### Verify the install

```bash
gortex version
```

### Verifying releases (supply-chain security)

Every GitHub release is:

- **Signed with [cosign](https://github.com/sigstore/cosign)** — keyless via GitHub's OIDC identity. Each artifact ships with matching `.sig` and `.pem` files that cryptographically prove the binary came from this repo's release workflow.
- **Attested with [SLSA-3 provenance](https://slsa.dev/spec/v1.0/levels#build-l3)** — a `multiple.intoto.jsonl` file attached to each release records the exact commit, builder, and workflow that produced every artifact. Tamper-evident and non-forgeable.
- **Scanned against ~72 AV engines via [VirusTotal](https://virustotal.com)** — the detection count (e.g. `0 / 72`) is posted in each release's notes, with a link to the full per-engine report.

You don't need to verify manually if you're installing via `brew` / `dpkg` / `rpm` — those paths go through package managers that check integrity themselves. Verification matters when you're redistributing Gortex downstream, running it inside a locked-down enterprise environment, or writing your own installer.

**cosign** — install once via `brew install cosign`, `apt install cosign`, or from [the cosign releases page](https://github.com/sigstore/cosign/releases). Then:

```bash
TAG=v0.5.4                           # replace with the release you downloaded
FILE=gortex_linux_amd64.tar.gz       # pick your artifact

BASE="https://github.com/zzet/gortex/releases/download/${TAG}"
curl -LO "${BASE}/${FILE}"
curl -LO "${BASE}/${FILE}.sig"
curl -LO "${BASE}/${FILE}.pem"

cosign verify-blob \
  --certificate "${FILE}.pem" \
  --signature "${FILE}.sig" \
  --certificate-identity-regexp 'https://github\.com/zzet/gortex/\.github/workflows/.+@refs/tags/v.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${FILE}"
```

Expected output: `Verified OK`. Anything else — stop and delete the binary.

**SLSA-3** — install [`slsa-verifier`](https://github.com/slsa-framework/slsa-verifier/releases) once. Then:

```bash
curl -LO "${BASE}/multiple.intoto.jsonl"

slsa-verifier verify-artifact "${FILE}" \
  --provenance-path multiple.intoto.jsonl \
  --source-uri github.com/zzet/gortex \
  --source-tag "${TAG}"
```

Expected output ends with `PASSED: SLSA verification passed`.

**VirusTotal** — open the release page on GitHub. The notes include a per-asset scan table like `gortex_linux_amd64.tar.gz — 0 / 72` with a link to the full report. A non-zero detection on a Go binary is usually a false positive (Go's static linking + stripped symbols trips heuristics), but you should still compare against prior releases before trusting the download.

### From source

Requires Go 1.25+ and a C toolchain (the tree-sitter extractors are CGO — no way around it).

```bash
git clone https://github.com/zzet/gortex.git
cd gortex
go build -o gortex ./cmd/gortex/
sudo mv gortex /usr/local/bin/
```

Or without cloning:

```bash
go install github.com/zzet/gortex/cmd/gortex@latest
```

`go install` drops the binary into `$(go env GOBIN)` (default `~/go/bin`) — make sure that's on your `PATH`.

## Quick Start

`gortex init` in a terminal opens an interactive wizard that asks whether you want a global daemon or per-project setup, and whether to track the current repo + start the daemon immediately. For scripts or CI the flags below skip the prompt.

### Global mode (recommended when you work across multiple repos)

```bash
# Interactive: walks you through mode choice + follow-ups
cd ~/projects/myapp
gortex init

# Non-interactive equivalent (CI / scripts):
gortex init --global --start --track

# Daemon lifecycle:
gortex daemon start --detach        # spawn in background
gortex daemon status                # PID, uptime, memory, tracked repos, sessions
gortex daemon stop                  # graceful shutdown + final snapshot
gortex daemon restart               # stop + start
gortex daemon reload                # re-read config, pick up new/removed repos
gortex daemon logs -n 50            # tail the log file

# Auto-start at login (launchd on macOS, systemd --user on Linux):
gortex daemon install-service
gortex daemon service-status
gortex daemon uninstall-service

# Track / untrack repos (daemon-first dispatch; falls back to config-only when no daemon):
gortex track ~/projects/backend
gortex untrack backend

# Per-repo status + daemon-wide status share the same command — it picks:
gortex status
```

### Per-repo mode

```bash
# Pick [2] at the wizard, or pass no --global flag and the config stays local:
gortex init /path/to/repo               # creates .mcp.json, CLAUDE.md, hooks, commands
gortex init --analyze /path/to/repo     # indexes first for a richer CLAUDE.md
gortex init --hooks-only /path/to/repo  # (re)install hooks only, skip everything else
gortex init --no-hooks /path/to/repo    # full init but skip hook installation

# Run the MCP server standalone (auto-detects daemon via stdio; --no-daemon forces embedded):
gortex serve --index /path/to/repo --watch
gortex serve --no-daemon --watch        # explicit embedded mode
```

### Other commands

```bash
gortex skills /path/to/repo              # generate per-community SKILL.md files for Claude Code
gortex bridge --index . --web            # HTTP bridge API + web graph UI at :4747
gortex savings                           # cumulative tokens saved + $ avoided across sessions
gortex version
```

## Multi-Repo Workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

### Configuration

Two-tier config hierarchy:

- **Global config** (`~/.config/gortex/config.yaml`) — projects, repo lists, active project, reference tags
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides

Excludes are layered — builtin → global → per-repo entry → workspace — with gitignore semantics. Use `!pattern` in a later layer to re-include something an earlier layer excluded.

```yaml
# ~/.config/gortex/config.yaml
active_project: my-saas

exclude:                            # Applies to every tracked repo
  - "**/*.generated.*"
  - "node_modules/"                 # Already in the builtin baseline

repos:
  - path: /home/user/projects/gortex
    name: gortex
    exclude:                        # Extra patterns just for this repo
      - "results/**"

projects:
  my-saas:
    repos:
      - path: /home/user/projects/frontend
        name: frontend
        ref: work
      - path: /home/user/projects/backend
        name: backend
        ref: work
      - path: /home/user/projects/shared-lib
        name: shared-lib
        ref: opensource
```

### CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex serve --track /path/to/repo  # Track additional repos on startup
gortex serve --project my-saas      # Set active project scope
gortex index repo-a/ repo-b/        # Index multiple repos
gortex status                       # Per-repo and per-project stats

# Manage the effective ignore list used by indexing + watching
gortex config exclude list                          # Show all layers (builtin, global, repo entry, workspace)
gortex config exclude add pkg/generated             # Default target: workspace .gortex.yaml
gortex config exclude add '**/*.bak' --global       # Write to ~/.config/gortex/config.yaml
gortex config exclude add testdata/ --repo backend  # Write to a RepoEntry
gortex config exclude remove pkg/generated          # Remove from the same target
```

### MCP Tools

Agents can manage repos at runtime without CLI access:

| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo, index immediately, persist to config |
| `untrack_repository` | Remove a repo, evict nodes/edges, persist to config |
| `set_active_project` | Switch project scope for all subsequent queries |
| `get_active_project` | Return current project name and repo list |

All query tools (`search_symbols`, `get_symbol`, `find_usages`, `get_file_summary`, `get_call_chain`, `smart_context`) accept optional `repo`, `project`, and `ref` parameters for scoping. When an active project is set, it applies as the default scope.

### How It Works

- **Qualified Node IDs** — in multi-repo mode, IDs become `<repo_prefix>/<path>::<Symbol>` (e.g., `frontend/src/app.ts::App`). Single-repo mode keeps the existing `<path>::<Symbol>` format.
- **Cross-repo edges** — the resolver links symbols across repo boundaries with same-repo preference. Cross-repo edges carry a `cross_repo: true` flag.
- **Impact analysis** — `explain_change_impact`, `verify_change`, and `get_test_targets` follow cross-repo edges automatically, grouping results by repository.
- **Shared repos** — the same repo can appear in multiple projects with different reference tags. It's indexed once and shared across projects.
- **Auto-detection** — set `workspace.auto_detect: true` in `.gortex.yaml` to auto-discover Git repos in a parent directory.

## Usage with Claude Code

After running `gortex init`, Claude Code automatically starts Gortex via `.mcp.json`. The agent gets:

- **Slash commands:** `/gortex-guide`, `/gortex-explore`, `/gortex-debug`, `/gortex-impact`, `/gortex-refactor`
- **Global skills:** installed to `~/.claude/skills/` — available across all repos
- **PreToolUse hook:** automatic graph context + graph-tool suggestions on Read/Grep/Glob
- **PreCompact hook:** condensed orientation snapshot injected before context compaction so the agent resumes without re-exploring
- **Stop hook:** post-task diagnostics — tests to run, guard violations, dead code, and contract issues on the changed symbols — injected as context before the agent hands off
- **CLAUDE.md instructions:** mandatory tool usage table and session workflow

## Usage with Kiro

`gortex init` also sets up Kiro IDE integration automatically:

- **MCP server:** `.kiro/settings/mcp.json` — 40 read-only tools auto-approved for zero-friction use (write tools like `edit_symbol` and `rename_symbol` require approval)
- **Steering files:** `.kiro/steering/gortex-workflow.md` (always active) teaches Kiro to prefer graph queries over file reads. Additional manual steering files for explore, debug, impact, and refactor workflows are available via `#` in chat.
- **Agent hooks:**
  - `gortex-smart-context` — on each prompt, assembles task-relevant context from the graph in one call
  - `gortex-post-edit` — after saving source files, shows blast radius and which tests to run
  - `gortex-pre-read` — before reading source files, enriches with symbol context from the graph

## Usage with Antigravity

`gortex init` also automatically loads project intelligence instructions into the Antigravity agent:

- **Knowledge Item (KI):** Creates a dedicated KI globally in `~/.gemini/antigravity/knowledge/gortex-workflow/`.
- **Workflow Instructions:** Instructs the Antigravity assistant to prioritize executing AST-aware Gortex CLI queries (`./gortex query symbol`, `./gortex query dependents`) via its built-in terminal tool, overriding generic `grep` and file read routines.
- **Token Efficiency:** Significantly reduces context token usage by constraining the AI's reads precisely to verified dependency paths and function definitions.

## Usage with Cursor

`gortex init` detects Cursor (via `.cursor/` directory, `~/.cursor/`, or `cursor` in PATH) and creates:

- **MCP config:** `.cursor/mcp.json` — project-level config, committable to the repo so the whole team gets Gortex automatically.

## Usage with VS Code / GitHub Copilot

`gortex init` detects VS Code (via `.vscode/` directory, `code` in PATH, or VS Code app data directories) and creates:

- **MCP config:** `.vscode/mcp.json` — project-level config for Copilot Chat agent mode. All Gortex tools are available in Copilot's agent mode.

## Usage with Windsurf

`gortex init` detects Windsurf (via `windsurf` in PATH or `~/.codeium/windsurf/` directory) and creates:

- **MCP config:** `~/.codeium/windsurf/mcp_config.json` — global config (Windsurf only reads from this location). Merges into existing config without overwriting other servers.

## Usage with Continue.dev

`gortex init` detects Continue.dev (via `.continue/` directory in project or `~/.continue/`) and creates:

- **MCP config:** `.continue/mcpServers/gortex.json` — project-level config. Continue reads JSON MCP configs from the `.continue/mcpServers/` directory and supports the same format as Claude/Cursor.

## Usage with Cline

`gortex init` detects Cline (via VS Code or Cursor globalStorage directories) and creates:

- **MCP config:** `cline_mcp_settings.json` in the Cline extension's globalStorage directory. Includes an `alwaysAllow` list for read-only Gortex tools.

## Usage with OpenCode

`gortex init` detects OpenCode (via `.opencode/` directory in project, `opencode` in PATH, or `~/.config/opencode/`) and creates:

- **MCP config:** `.opencode/config.json` — project-level config using OpenCode's native format (`"type": "local"`, `"command"` as array, `"environment"` for env vars). Merges into existing config without overwriting other servers.

## CLI Commands

```
gortex init [path]           Set up Gortex for a project + install global skills
gortex serve [flags]         Start the MCP server (--bridge to add HTTP API)
gortex bridge [flags]        Start standalone HTTP bridge API
gortex eval-server [flags]   Start eval HTTP server for benchmarking
gortex skills [path]         Generate per-community SKILL.md files
gortex context [flags]       Generate portable context briefing for a task
gortex savings [flags]       Show cumulative token savings + cost avoided across sessions
gortex index [path...]       Index one or more repositories and print stats
gortex status [flags]        Show index status (per-repo and per-project in multi-repo mode)
gortex track <path>          Add a repository to the tracked workspace
gortex untrack <path>        Remove a repository from the tracked workspace
gortex query <subcommand>    Query the knowledge graph
gortex clean                 Remove Gortex files from a project
gortex claude-md [flags]     Generate CLAUDE.md block
gortex version               Print version
```

### Query Subcommands

```
gortex query symbol <name>              Find symbols matching name
gortex query deps <id>                  Show dependencies
gortex query dependents <id>            Show blast radius
gortex query callers <func-id>          Show who calls a function
gortex query calls <func-id>            Show what a function calls
gortex query implementations <iface>    Show interface implementations
gortex query usages <id>                Show all usages
gortex query stats                      Show graph statistics
```

All query commands support `--format text|json|dot` (DOT output for Graphviz visualization).

## MCP Tools (44)

### Core Navigation
| Tool | Description |
|------|-------------|
| `graph_stats` | Node/edge counts by kind, language, per-repo stats, and session token savings |
| `search_symbols` | Find symbols by name (replaces Grep). Accepts `repo`, `project`, `ref` params |
| `get_symbol` | Symbol location and signature (replaces Read). Accepts `repo`, `project`, `ref` params |
| `get_file_summary` | All symbols and imports in a file. Accepts `repo`, `project`, `ref` params |
| `get_editing_context` | **Primary pre-edit tool** — symbols, signatures, callers, callees |

### Graph Traversal
| Tool | Description |
|------|-------------|
| `get_dependencies` | What a symbol depends on |
| `get_dependents` | What depends on a symbol (blast radius) |
| `get_call_chain` | Forward call graph |
| `get_callers` | Reverse call graph |
| `find_usages` | Every reference to a symbol |
| `find_implementations` | Types implementing an interface |
| `get_cluster` | Bidirectional neighborhood |

### Coding Workflow
| Tool | Description |
|------|-------------|
| `get_symbol_source` | Source code of a single symbol (80% fewer tokens than Read). Returns `tokens_saved` per call |
| `batch_symbols` | Multiple symbols with source/callers/callees in one call |
| `find_import_path` | Correct import path for a symbol |
| `explain_change_impact` | Risk-tiered blast radius with affected processes |
| `get_recent_changes` | Files/symbols changed since timestamp |
| `edit_symbol` | Edit a symbol's source directly by ID — no Read needed |
| `rename_symbol` | Coordinated multi-file rename with all references |

### Agent-Optimized (token efficiency)
| Tool | Description |
|------|-------------|
| `smart_context` | Task-aware minimal context — replaces 5-10 exploration calls |
| `get_edit_plan` | Dependency-ordered edit sequence for multi-file refactors |
| `get_test_targets` | Maps changed symbols to test files and run commands |
| `suggest_pattern` | Extracts code pattern from an example — source, registration, tests |
| `export_context` | Portable markdown/JSON context briefing for sharing outside MCP |
| `feedback` | `action: "record"`: report useful/missing symbols. `action: "query"`: aggregated stats — most useful, most missed, accuracy metrics |

### Analysis
| Tool | Description |
|------|-------------|
| `get_communities` | Functional clusters (Louvain). Without `id`: list all. With `id`: members and cohesion for one community |
| `get_processes` | Discovered execution flows. Without `id`: list all. With `id`: step-by-step trace |
| `detect_changes` | Git diff mapped to affected symbols |
| `index_repository` | Index or re-index a repository path |
| `contracts` | API contracts. `action: "list"` (default): detected HTTP/gRPC/GraphQL/topics/WebSocket/env/OpenAPI. `action: "check"`: orphan providers/consumers |

### Proactive Safety
| Tool | Description |
|------|-------------|
| `verify_change` | Check proposed signature changes against all callers and interface implementors |
| `check_guards` | Evaluate project guard rules (`.gortex.yaml`) against changed symbols |
| `audit_agent_config` | Scan CLAUDE.md / AGENTS.md / `.cursor/rules` / `.github/copilot-instructions.md` / `.windsurf/rules` / `.antigravity/rules` for stale symbol references, dead file paths, and bloat — validated against the Gortex graph (no other competitor does this) |

### Code Quality
| Tool | Description |
|------|-------------|
| `analyze` | Unified graph analysis. `kind: "dead_code"`: symbols with zero incoming edges. `kind: "hotspots"`: high fan-in/out symbols. `kind: "cycles"`: Tarjan's SCC cycle detection. `kind: "would_create_cycle"`: check if a new dependency would form a cycle |
| `index_health` | Health score, parse failures, stale files, language coverage |
| `get_symbol_history` | Symbols modified this session with counts; flags churning (3+ edits) |

### Code Generation
| Tool | Description |
|------|-------------|
| `scaffold` | Generate code, registration wiring, and test stubs from an example symbol |
| `batch_edit` | Apply multiple edits in dependency order, re-index between steps |
| `diff_context` | Git diff enriched with callers, callees, community, processes, per-file risk |
| `prefetch_context` | Predict needed symbols from task description and recent activity |

### Multi-Repo Management
| Tool | Description |
|------|-------------|
| `track_repository` | Add a repo at runtime — indexes immediately, persists to global config |
| `untrack_repository` | Remove a repo — evicts nodes/edges, persists to global config |
| `set_active_project` | Switch active project scope for all subsequent queries |
| `get_active_project` | Return current project name and its member repositories |

## MCP Resources (7)

| Resource | Description |
|----------|-------------|
| `gortex://session` | Current session state and activity |
| `gortex://stats` | Graph statistics (node/edge counts) |
| `gortex://schema` | Graph schema reference |
| `gortex://communities` | Community list with cohesion scores |
| `gortex://community/{id}` | Single community detail |
| `gortex://processes` | Execution flow list |
| `gortex://process/{id}` | Single process trace |

## MCP Prompts (3)

| Prompt | Description |
|--------|-------------|
| `pre_commit` | Review uncommitted changes — shows changed symbols, blast radius, risk level, affected tests |
| `orientation` | Orient in an unfamiliar codebase — graph stats, communities, execution flows, key symbols |
| `safe_to_change` | Analyze whether it's safe to change specific symbols — blast radius, edit plan, affected tests |

## Web UI

Gortex includes a standalone Next.js web application (`web/` directory) with 10 pages:

```bash
# Start the backend
gortex bridge --index /path/to/repo --web --port 4747

# Start the web UI
cd web && npm run dev
# Open http://localhost:3000
```

| Page | Features |
|------|----------|
| **Dashboard** | Health, stats, language pie chart, node kind bar chart |
| **Graph Explorer** | Interactive Sigma.js + ForceAtlas2, node filters, selection, detail panel |
| **Search** | Semantic search via bridge API, results grouped by kind |
| **Symbol Detail** | Source code, signature, callers/callees/usages/deps tabs |
| **Communities** | Community cards with cohesion bars, expandable members |
| **Processes** | Execution flow table with step lists |
| **Analysis** | Dead code, hotspots, cycles, index health — 4 tabs |
| **Contracts** | API contracts (HTTP, gRPC, GraphQL, topics, WebSocket, env vars) with provider/consumer matching |
| **Services** | Service-level graph visualization with per-repo stats |
| **AI Chat** | LLM-powered chat with code context (placeholder) |

The legacy embedded web UI (Sigma.js on `:8765`) is still available via `gortex serve --web`.

## Bridge Mode

The `gortex bridge` command exposes all MCP tools as an HTTP/JSON API for external integrations:

```bash
# Standalone bridge with web UI
gortex bridge --index /path/to/repo --web --port 4747

# Bridge alongside MCP stdio
gortex serve --index /path/to/repo --bridge --port 8765
```

**Endpoints:**
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Status, node/edge counts, uptime |
| `/tools` | GET | List all available tools with descriptions |
| `/tool/{name}` | POST | Invoke any MCP tool with JSON arguments |
| `/stats` | GET | Graph statistics by kind and language |

CORS is enabled by default (`--cors-origin '*'`). The bridge can serve the web UI on the same port with `--web`.

## Cross-Repo API Contracts

Gortex detects API contracts across repos and matches providers to consumers:

```bash
# After indexing, contracts are auto-detected
gortex bridge --index . --web

# Via MCP tools
contracts                        # list all detected contracts (default action)
contracts {action: "check"}      # find mismatches and orphans
```

| Contract Type | Detection | Provider | Consumer |
|--------------|-----------|----------|----------|
| **HTTP Routes** | Framework annotations (gin, Express, FastAPI, Spring, etc.) | Route handler | HTTP client calls (fetch, http.Get) |
| **gRPC** | Proto service definitions | Service RPC | Client stub calls |
| **GraphQL** | Schema type/field definitions | Schema | Query/mutation strings |
| **Message Topics** | Pub/sub patterns (Kafka, NATS, RabbitMQ) | Publish calls | Subscribe calls |
| **WebSocket** | Event emit/listen patterns | emit() | on() |
| **Env Vars** | os.Getenv, process.env, .env files | Setenv / .env | Getenv / process.env |
| **OpenAPI** | Swagger/OpenAPI spec files | Spec paths | (linked to HTTP routes) |

Contracts are normalized to canonical IDs (e.g., `http::GET::/api/users/{id}`) and matched across repos to detect orphan providers/consumers and mismatches.

## Per-Community Skills

`gortex skills` analyzes your codebase, detects functional communities via Louvain clustering, and generates targeted SKILL.md files that Claude Code auto-discovers:

```bash
# Generate skills for the current repo
gortex skills .

# Custom settings
gortex skills . --min-size 5 --max-communities 10 --output-dir .claude/skills/generated/
```

Each generated skill includes:
- **Community metadata** — size, file count, cohesion score
- **Key files table** — files and their symbols
- **Entry points** — main functions, handlers, controllers detected via process analysis
- **Cross-community connections** — which other areas this community interacts with
- **MCP tool invocations** — pre-written `get_communities`, `smart_context`, `find_usages` calls

Skills are written to `.claude/skills/generated/` and a routing table is inserted into CLAUDE.md between `<!-- gortex:skills:start/end -->` markers.

## Semantic Search

Hybrid BM25 + vector search with Reciprocal Rank Fusion (RRF). Multiple embedding tiers:

```bash
# Built-in word vectors (always available, zero setup)
gortex serve --index . --embeddings

# Ollama (best quality, local)
ollama pull nomic-embed-text
gortex serve --index . --embeddings-url http://localhost:11434

# OpenAI (best quality, cloud)
gortex serve --index . --embeddings-url https://api.openai.com/v1 \
  --embeddings-model text-embedding-3-small
```

| Tier | Flag | Quality | Offline | Default build? |
|------|------|---------|---------|----------------|
| Hugot (pure Go) | `--embeddings` | Good (MiniLM-L6-v2) | Yes (model auto-downloads on first use) | **Yes** |
| Built-in | `--embeddings` (when Hugot unavailable) | Basic (GloVe word averaging) | Yes | Yes |
| API | `--embeddings-url` | Best (transformer model) | No | Yes |
| ONNX | `--embeddings` + build tag | Best | Yes (model + libonnxruntime required) | No |
| GoMLX | `--embeddings` + build tag | Good | Yes (XLA plugin auto-downloads) | No |

The default build ships with Hugot using the pure-Go ONNX runtime — no native dependencies, no manual model placement. The MiniLM-L6-v2 model downloads to `~/.cache/gortex/models/` on first use (~90 MB).

Opt-in faster backends via build tags:
```bash
go build -tags embeddings_onnx ./cmd/gortex/   # needs: brew install onnxruntime
go build -tags embeddings_gomlx ./cmd/gortex/  # auto-downloads XLA plugin
```

## Token Savings

Gortex tracks how many tokens it saves compared to naive file reads — per-call, per-session, and cumulative across restarts:

- **Per-call:** `get_symbol_source` and other source-reading tools include a `tokens_saved` field in the response, showing the difference between reading the full file vs the targeted symbol.
- **Session-level:** `graph_stats` returns a `token_savings` object with `calls_counted`, `tokens_returned`, `tokens_saved`, `efficiency_ratio`.
- **Cumulative (cross-session):** `graph_stats` also returns `cumulative_savings` when persistence is wired — includes `first_seen`, `last_updated`, and `cost_avoided_usd` per model (Claude Opus/Sonnet/Haiku, GPT-4o, GPT-4o-mini). Backed by `~/.cache/gortex/savings.json`.

```bash
# Show totals + cost across all default models
gortex savings

# Highlight a single model (fuzzy match: "opus" → claude-opus-4)
gortex savings --model opus

# Machine-readable output
gortex savings --json

# Wipe cumulative totals
gortex savings --reset

# Override pricing (JSON array of {model, usd_per_m_input})
GORTEX_MODEL_PRICING_JSON='[{"model":"mycorp","usd_per_m_input":5}]' gortex savings
```

Token counts use **tiktoken (`cl100k_base`)** — the tokenizer Claude and GPT-4 actually use — via `github.com/pkoukk/tiktoken-go` with an embedded offline BPE loader, so no runtime downloads. The BPE is lazy-loaded on first call. If init fails for any reason, the package falls back to the legacy `chars/4` heuristic so metrics stay usable.

## Graph Persistence

Gortex snapshots the graph to disk on shutdown and restores it on startup, with incremental re-indexing of only changed files:

```bash
# Default cache directory: ~/.cache/gortex/
gortex serve --index /path/to/repo

# Custom cache directory
gortex serve --index /path/to/repo --cache-dir /tmp/gortex-cache

# Disable caching
gortex serve --index /path/to/repo --no-cache
```

The persistence layer uses a pluggable backend interface (`persistence.Store`). The default backend serializes as gob+gzip. Cache is keyed by repo path + git commit hash, with version validation to invalidate on binary upgrades.

## Scale — battle-tested on large repos

Measured on an Apple Silicon laptop with the default CGO build:

| Repository | Files | Nodes | Edges | Index time | Throughput | Peak heap |
| ---------- | ----: | ----: | ----: | ---------: | ---------: | --------: |
| [torvalds/linux](https://github.com/torvalds/linux) | 70,333 | 1,690,174 | 6,239,570 | ~3 min | 300 files/s | 5.07 GB |
| [microsoft/vscode](https://github.com/microsoft/vscode) | 10,762 | 204,501 | 808,902 | ~1 min | 143 files/s | 580 MB |
| zzet/gortex (self) | 430 | 5,583 | 53,830 | 3.4s | 127 files/s | 52 MB |

Parsing dominates wall time (65–80%); reference resolution and search-index build scale sub-linearly. Everything runs in-process — no external services, no database, no network.

## Architecture

```
gortex binary
  CLI (cobra)  ──> MultiIndexer ──> In-Memory Graph (shared, per-repo indexed)
  MCP Server ──────────────────────> Query Engine (repo/project/ref scoping)
  Bridge API ──────────────────────> (HTTP/JSON over MCP tools)
  Web Server ──────────────────────> (Nodes + Edges + byRepo index)
  MCP Prompts ─────────────────────> (pre_commit, orientation, safe_to_change)
  MCP Resources ───────────────────> (session, stats, schema, communities, processes)
                   MultiWatcher <── filesystem events (fsnotify, per-repo)
                   CrossRepoResolver ──> cross-repo edge creation (type-aware)
                   Persistence ──> gob+gzip snapshot (pluggable backend)
```

**Data flow:**
1. On startup, loads cached graph snapshot if available; otherwise performs full indexing
2. MultiIndexer walks each repo directory concurrently, dispatches files to language-specific extractors (tree-sitter)
3. Extractors produce nodes (files, functions, types, etc.) and edges (calls, imports, defines, etc.) with type environment metadata
4. In multi-repo mode, nodes get `RepoPrefix` and IDs become `<repo_prefix>/<path>::<Symbol>`
5. Resolver links cross-file references with type-aware method matching; CrossRepoResolver links cross-repo references with same-repo preference
6. Query Engine answers traversal queries with optional repo/project/ref scoping
7. MultiWatcher detects changes per-repo and surgically patches the graph (debounced per-file), then re-resolves cross-repo edges
8. On shutdown, persists graph snapshot for fast restart

## Graph Schema

**Node kinds:** `file`, `function`, `method`, `type`, `interface`, `variable`, `import`, `package`

**Edge kinds:** `calls`, `imports`, `defines`, `implements`, `extends`, `references`, `member_of`, `instantiates`

**Multi-repo fields:** Nodes carry `repo_prefix` (empty in single-repo mode). Edges carry `cross_repo` (true when connecting nodes in different repos). Node IDs use `<repo_prefix>/<path>::<Symbol>` format in multi-repo mode.

## Language Support (33 languages)

### Code Languages
| Language | Functions | Methods + MemberOf | Types | Interfaces | Imports | Calls | Variables |
|----------|-----------|-------------------|-------|------------|---------|-------|-----------|
| Go | Full | Full (receiver) | Full | Full + Meta["methods"] | Full | Full | Full |
| TypeScript | Full | Full | Full | Full + Meta["methods"] | Full | Full | Full |
| JavaScript | Full | Full | Full | - | Full | Full | Full |
| Python | Full | Full | Full | - | Full | Full | Partial |
| Rust | Full | Full (impl blocks) | Full | Full + Meta["methods"] | Full | Full | Full |
| Java | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| C# | Full | Full | Full | Full + Meta["methods"] | Full | Full | Fields |
| Kotlin | Full | Full | Full | Full | Full | Full | Properties |
| Scala | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| Swift | Full | Full | Full | Full + Meta["methods"] | Full | Full | - |
| PHP | Full | Full | Full | Full | Full | Full | - |
| Ruby | Full | Full | Full | - | Full | Full | Constants |
| Elixir | Full | Full (defmodule) | Modules | - | Full | Full | Attributes |
| C | Full | - | Structs/Enums | - | Full | Full | Globals |
| C++ | Full | Full | Classes/Structs | - | Full | Full | - |
| Dart | Full | Full | Classes/Enums/Mixins/Extensions | Abstract interface | Full | Full | Full |
| OCaml | Full | Full (class) | Types/Modules | Module types | open | Full | Full |
| Lua | Full | Full (M.func/M:method) | - | - | require() | Full | Full |
| Zig | Full | - | Structs/Enums/Unions | - | @import | Full | Full |
| Haskell | Full | - | data/newtype/type | class | import | Full | - |
| Clojure | Full (defn) | - | defrecord/deftype | defprotocol | require/use | Full | def |
| Erlang | Full | - | -type/-record | - | -import | Full | - |
| R | Full | - | - | - | library/require/source | Full | Full |
| Bash/Zsh | Full | - | - | - | source/. | Full | Exports |

### Data & Config Languages
| Language | What it extracts |
|----------|-----------------|
| SQL | Tables (with columns), views, functions, indexes, triggers |
| Protobuf | Messages (with fields), services + RPCs, enums, imports |
| Markdown | Headings, local file links, code block languages |
| HTML | Script/link references, element IDs |
| CSS | Class selectors, ID selectors, custom properties, @import |
| YAML | Top-level keys |
| TOML | Tables, key-value pairs |
| HCL/Terraform | Resource/data/module/variable/output blocks (.tf, .tfvars, .hcl) |
| Dockerfile | FROM (base images), ENV/ARG variables |

## Building

```bash
make build          # Build with version from git tags
make test           # go test -race ./...
make bench          # Run all benchmarks
make lint           # golangci-lint
make fmt            # gofmt -s
make install        # go install with version ldflags
```

Requires Go 1.21+ and CGO enabled (for tree-sitter C bindings).

## License

Gortex is **source-available** under a custom license based on PolyForm Small Business. See [LICENSE.md](LICENSE.md) for full terms.

**Free for:** individuals, open-source projects, small businesses (<50 employees / <$500K revenue), nonprofits, education, government, socially critical orgs.

**Commercial license required for:** organizations with 50+ employees or $500K+ revenue, competing products, resale/bundling. Contact [license@zzet.org](mailto:license@zzet.org).

**Contributors:** active contributors listed in [CONTRIBUTORS.md](CONTRIBUTORS.md) receive a free non-competing commercial license for their employer.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on adding features, language extractors, and submitting PRs. Active contributors receive a [contributor commercial license](LICENSE.md#part-3-contributor-license).
