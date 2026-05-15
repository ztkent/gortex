# Gortex

[![CI](https://github.com/zzet/gortex/actions/workflows/ci.yml/badge.svg)](https://github.com/zzet/gortex/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/zzet/gortex)](https://goreportcard.com/report/github.com/zzet/gortex)
[![Latest release](https://img.shields.io/github/v/release/zzet/gortex?logo=github&sort=semver)](https://github.com/zzet/gortex/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/zzet/gortex.svg)](https://pkg.go.dev/github.com/zzet/gortex)
[![Sigstore signed](https://img.shields.io/badge/sigstore-signed-66D4FF?logo=sigstore&logoColor=white)](docs/installation.md#verifying-releases-supply-chain-security)
[![SLSA 3](https://img.shields.io/badge/SLSA-Level%203-green)](https://slsa.dev/spec/v1.0/levels#build-l3)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/zzet/gortex/badge)](https://scorecard.dev/viewer/?uri=github.com/zzet/gortex)
[![VirusTotal](https://img.shields.io/badge/VirusTotal-0%2F91-brightgreen?logo=virustotal)](https://www.virustotal.com/gui/url/00e1094b39c9bd7db4d5a179b1d56173f85c915075057fd3cc64bfbb9b735b11/detection)



Code intelligence engine that indexes repositories into an in-memory knowledge graph and exposes it via CLI, MCP Server, and web UI.

Built for 15 AI coding agents (Claude Code, Kiro, Cursor, Windsurf, VS Code / Copilot, Continue.dev, Cline, OpenCode, Antigravity, Codex CLI, Gemini CLI, Zed, Aider, Kilo Code, OpenClaw) — one `smart_context` call replaces 5-10 file reads, cutting token usage by ~94%.

See [docs/agents.md](docs/agents.md) for the adapter matrix, per-agent schema notes, and the `gortex init --agents=<csv>` CLI contract.

![Gortex Web UI — force-directed knowledge graph visualization](assets/graph.png)

## Installation

```bash
curl -fsSL https://get.gortex.dev | sh
```

> Detects OS/arch, downloads the signed release tarball, verifies the SHA256 against `checksums.txt` (and cosign if installed), drops the binary in `$HOME/.local/bin`, and adds it to your shell rc. Re-runs upgrade in place. No silent sudo. Linux + macOS, amd64 + arm64. Windows support is planned.

For Homebrew, package managers (`.deb` / `.rpm` / `.apk`), direct binary download, supply-chain verification (cosign + SLSA-3 + VirusTotal), and from-source builds — see [docs/installation.md](docs/installation.md).

**New to Gortex?** After installing, see [docs/onboarding.md](docs/onboarding.md) for the 15-minute walkthrough: 
`gortex install` (once per machine) → `gortex daemon start --detach` → `gortex track <path to repo>` → your AI assistant uses graph tools
   
## Features

- **Knowledge graph** — every file, symbol, import, call chain, and type relationship in one queryable structure
- **Multi-repo workspaces** — index multiple repositories into a single graph with cross-repo symbol resolution, project grouping, reference tags, and per-repo scoping. A dedicated cross-repo edge layer materialises `cross_repo_calls` / `cross_repo_implements` / `cross_repo_extends` edges whenever a relation's endpoints live in different repos — surfaced via `analyze` `kind: "cross_repo"`. Resolution is evidence-gated: cross-repo links are only drawn when the symbol evidence clears a confidence bar, so unrelated same-named symbols don't get wired together
- **Per-session workspace isolation** — under the daemon, each MCP session's queries are scoped to the workspace it connected from; sessions on different repos never see each other's graph slices even though they share one process
- **256 languages** across three tiers — bespoke tree-sitter extractors (~30) for the deep-resolution tier (Go, TypeScript, Python, Rust, Java, C#, Kotlin, Swift, C, C++, Ruby, Elixir, OCaml, …), regex extractors (~60) for niche/legacy (ABAP, COBOL, Verse, AL, AutoHotkey, …), and forest-backed signature-only (~165 via `alexaandru/go-sitter-forest`) for the long tail (Vue, Svelte, Astro, GraphQL, Prisma, Latex, Typst, Agda, Idris, Hack, Haxe, MLIR, LLVM, SystemVerilog, Cedar, CEL, TLA+, Robot, Hurl, …). See [docs/languages.md](docs/languages.md) for the full table
- **64 MCP tools** — symbol lookup, call chains, blast radius, community/process discovery, contract detection, unified `analyze` (dead code, hotspots, cycles, channel-ops, goroutine-spawns, field-writers, config-readers, event-emitters, error-surface, external-calls, routes, models, components, k8s_resources, images, kustomize, cross-repo, dbt_models), near-duplicate clone detection (`find_clones`), structural code search (`search_ast` with bundled detectors), CPG-lite dataflow (`flow_between` / `taint_paths` over value-flow / arg-of / returns-to edges), diagnostics push (`subscribe_diagnostics` / `unsubscribe_diagnostics` / `get_diagnostics`) + code actions (`get_code_actions` / `apply_code_action` / `fix_all_in_file`) wired across every running language server, scaffolding, inline editing, symbol renaming, read-free file writes (`edit_file` / `write_file`) with path-traversal guard + `dry_run`, multi-axis structured retrieval (`winnow_symbols`), multi-repo management, agent feedback loop, context export, graph-validated config hygiene (`audit_agent_config`), opening-move routing (`plan_turn`), narrative repo overview (`get_repo_outline`), test-coverage gaps (`get_untested_symbols`), and an optional in-process LLM research agent (`ask`)
- **Semantic search** — hybrid BM25 + vector search with RRF fusion. Hugot (pure-Go ONNX runtime with MiniLM-L6-v2) is bundled by default and auto-downloads the model on first use — zero-config, no native dependencies. GloVe word vectors remain as fallback. Optional build tags switch to ONNX or GoMLX for higher throughput
- **LSP-enriched call-graph tiers** — every edge carries an `origin` tier (`lsp_resolved` / `lsp_dispatch` / `ast_resolved` / `ast_inferred` / `text_matched`); pass `min_tier` to `get_callers`, `find_usages`, `find_implementations`, etc. to restrict results to compiler-verified edges for high-stakes refactors
- **MCP progress notifications** — long-running indexing and track_repository calls emit `notifications/progress` with stage messages (walking files → parsing → resolving → semantic enrichment → search index → contracts → done) so hosts show real progress bars on large repos
- **Type-aware resolution** — infers receiver types from variable declarations, composite literals, and Go constructor conventions to disambiguate same-named methods across types
- **On-disk persistence** — snapshots the graph on shutdown, restores on startup with incremental re-indexing of only changed files (~200ms vs 3-5s full re-index)
- **HTTP server (`gortex server`)** — versioned `/v1/*` JSON API exposing all MCP tools (`/v1/health`, `/v1/tools`, `/v1/tools/{name}`, `/v1/stats`, `/v1/graph`, `/v1/events` SSE) for IDE plugins, CI, and the Next.js web UI. Localhost bind + bearer-token auth (`--auth-token` / `$GORTEX_SERVER_TOKEN`) by default; CORS configurable for separate frontend origins
- **Semantic enrichment** — pluggable SCIP, go/types, and LSP providers upgrade edge confidence from ~70-85% (tree-sitter) to 95-100% (compiler-verified). Additive — graceful degradation when external tools unavailable
- **Agent feedback loop** — unified `feedback` tool (`action: "record"` / `"query"`) lets agents report which symbols were useful/missing. Cross-session persistence improves future `smart_context` quality via feedback-aware reranking
- **Context export** — `export_context` tool + `gortex context` CLI render graph context as portable markdown/JSON briefings for sharing outside MCP (Slack, PRs, docs, non-MCP AI tools)
- **ETag conditional fetch** — content-hash based `if_none_match` on source-reading tools avoids re-transmitting unchanged symbols during iterative editing
- **Token savings tracking** — per-call `tokens_saved` field on source-reading tools + session-level metrics in `graph_stats` (calls counted, tokens returned, tokens saved, efficiency ratio)
- **GCX1 compact wire format** — published, round-trippable text format for MCP tool responses. Opt-in per call via `format: "gcx"` on every list-shaped tool (~17). Auto-served as the default for known clients (Claude Code, Cursor, VS Code, Zed, Aider, Kilo Code, OpenCode, OpenClaw, Codex) when no `format` is passed; explicit `format` always wins. **Median −27.4% tiktoken savings** vs JSON across a 20-case benchmark (best case −38.3%), 100% round-trip integrity. Spec: [`docs/wire-format.md`](docs/wire-format.md). Standalone MIT-licensed reference implementations: Go ([`github.com/gortexhq/gcx-go`](https://github.com/gortexhq/gcx-go)) and TypeScript ([`github.com/gortexhq/gcx-ts`](https://github.com/gortexhq/gcx-ts), npm [`@gortex/wire`](https://www.npmjs.com/package/@gortex/wire)). Reproducible harness: [`bench/wire-format/`](bench/wire-format/)
- **TOON fallback wire format** — second-tier compact text (~10–15% smaller than JSON, lossy but human-friendly) on every list-shaped tool for clients that don't yet speak GCX. Pass `format: "toon"`
- **Budget-by-default MCP responses** — list-shaped tools cap each page at the project default budget and return `next_cursor` for the tail. Pagination, sparse fieldsets, and graceful degradation built in
- **16 MCP resources** — bootstrap state (`gortex://stats`, `gortex://workspace`, `gortex://repos`, `gortex://active-project`, `gortex://schema`, `gortex://session`, `gortex://index-health`), community / process rollups (`gortex://communities`, `gortex://community/{id}`, `gortex://processes`, `gortex://process/{id}`), and analyzer-backed summaries (`gortex://report`, `gortex://god-nodes`, `gortex://surprises`, `gortex://audit`, `gortex://questions`). Read-only, URI-addressable, push `notifications/resources/updated` after each graph re-warm — no polling
- **Framework graph layer** — handler→route edges from HTTP / gRPC / GraphQL / WebSocket / Phoenix / Kafka topic registrations; ORM model→table edges across GORM, SQLAlchemy, Django, ActiveRecord, JPA, TypeORM, Ecto; component-tree edges for JSX/TSX and Phoenix HEEx. Surfaced via `analyze` `kind: "routes" / "models" / "components"`
- **Infrastructure graph layer** — first-class `KindResource` (Kubernetes Deployments, Services, Ingresses, ConfigMaps, Secrets, CronJobs), `KindKustomization` (overlay tree), and `KindImage` (Dockerfile FROM targets and K8s `container.image`) with `depends_on` / `configures` / `mounts` / `exposes` / `uses_env` edges. Cross-references with code-side `os.Getenv` calls automatically. Surfaced via `analyze` `kind: "k8s_resources" / "kustomize" / "images"`
- **CPG-lite dataflow** — `value_flow` (intra-procedural assignment / return / range), `arg_of` (caller arg → callee param), and `returns_to` (callee → assignment LHS) edges built at index time. `flow_between` returns ranked dataflow paths between two symbol IDs; `taint_paths` does pattern-driven source→sink sweeps for security audits
- **Near-duplicate clone detection** — every substantial function body is reduced to a 64-slot token-normalised MinHash signature at index time, LSH banding finds candidate pairs, and a Jaccard threshold filter keeps the true clones — emitted as symmetric `similar_to` edges. `find_clones` surfaces the clusters; `find_clones {dead_only: true}` yields the Gortex-unique "dead duplicates of live code" diagnostic. Gated behind the `clones` coverage domain (default on, threshold tunable in `.gortex.yaml`)
- **Stratified test classification** — test symbols are classified by role (unit / integration / e2e / benchmark / fixture) and the `tests` edges carry that role, so `winnow_symbols` can filter "production functions only, no tests" and coverage analyzers can reason per-tier
- **Local LLM research agent (optional)** — built with `-tags llama` and a configured `llm.model`, the `ask` MCP tool runs a small GGUF model in-process via llama.cpp that navigates the graph with gortex tools and returns a synthesized answer — one call replaces a long chain of `search_symbols` / `get_callers` / `contracts` calls. `chain: true` traces a request across repos (consumer → contract → provider → downstream). Falls through to direct tools when the build tag or model is absent
- **3 MCP prompts** — `pre_commit`, `orientation`, `safe_to_change` for guided workflows
- **LLM features (optional)** — opt-in `ask` research agent + LLM-assisted `search_symbols` ranking, behind a pluggable provider (`local` llama.cpp / Anthropic / OpenAI / Ollama). Off by default; the HTTP providers need no native dependencies. See [LLM Features](#llm-features-optional)
- **Two-tier config** — global config (`~/.config/gortex/config.yaml`) for projects and repo lists, per-repo `.gortex.yaml` for guards, excludes, and local overrides
- **Guard rules** — project-specific constraints (co-change, boundary) enforced via `check_guards`
- **Watch mode** — surgical graph updates on file change across all tracked repos, live sync with agents
- **Web UI** — standalone Next.js 15 app at [`gortexhq/web`](https://github.com/gortexhq/web) (separate repo so it deploys independently) that talks to `gortex server` over `/v1/*`, with Sigma.js 2D graphs and five react-three-fiber 3D views (City, Strata, Galaxies, Constellation, Graph3D)
- **IMPLEMENTS inference** — structural interface satisfaction for Go, TypeScript, Java, Rust, C#, Scala, Swift, Protobuf
- **PreToolUse + PreCompact + Stop hooks** — PreToolUse enriches Read/Grep/Glob/Bash (`codebase-search` / `rg` / `grep` probes routed through graph tools) with graph context and redirects to Gortex MCP tools; matching `Task` also briefs spawned subagents with an inline tool-swap table + task-scoped `smart_context` so subagents don't fall back to grep/Read. PreCompact injects a condensed orientation snapshot (index stats, recently-modified symbols, top hotspots, feedback-ranked symbols) before Claude Code compacts the conversation. Stop runs post-task diagnostics (`detect_changes` → `get_test_targets`, `check_guards`, `analyze dead_code`, `contracts check` on modified symbols) so the agent self-corrects before handoff. All hooks degrade silently when the server is unreachable
- **Long-living daemon (optional)** — `gortex daemon start` runs a single shared process that holds the graph for every tracked repo. Each Claude Code / Cursor / Kiro window connects as a thin stdio proxy over a Unix socket, getting per-client session isolation (recent activity, token stats) + cross-repo queries by default. Live fsnotify watching on every tracked repo so file edits flow into the graph without manual reload. `gortex install` sets up user-level config; `gortex daemon install-service` installs a LaunchAgent (macOS) or systemd `--user` unit (Linux) so the OS supervises lifecycle and auto-starts at login — no sudo required. Binaries fall back to embedded mode if the daemon isn't running; the feature is additive
- **Benchmarked** — per-language parsing, query engine, indexer benchmarks
- **Per-community skills** — `gortex init --skills` (default on) auto-generates SKILL.md per detected community with key files, entry points, cross-community connections, and MCP tool invocations for Claude Code auto-discovery; the same routing table lands in every detected agent's per-repo instructions file
- **Eval framework** — SWE-bench harness for A/B benchmarking tool effectiveness with Docker-based environments and multi-model support
- **`gortex eval` CLI** — first-class evaluation harness. Subcommands: `recall` (fixture-driven any-hit R@1/5/20 + MRR per ranker, per-tier breakdown, p50/p95 latency, tokens-returned, optional LLM judge for CQS-style dual-judge scoring), `embedders` (ONNX variant comparison — size + init + embed latency + end-to-end quality across MiniLM variants, BGE, Jina), `swebench` (passthrough), `tokens` (GCX1 wire-format bench). Seed fixture at [`bench/fixtures/retrieval.yaml`](bench/fixtures/retrieval.yaml); published BM25 baseline on Gortex: **R@1 42.3% · R@5 56.4% · R@20 69.9% · exact R@5 95.2%**
- **Zero dependencies** — everything runs in-process, in memory, no external services

## Quick Start

Setup is split into two commands — `gortex install` runs once per machine, `gortex init` runs once per repo:

- **`gortex install`** writes user-level artifacts: `~/.claude.json` MCP config, `~/.claude/skills/gortex-*` (tool-usage skills), `~/.claude/commands/gortex-*.md` (slash commands), `~/.gemini/antigravity/` Knowledge Items, and (optionally) user-level Claude Code hooks. Codebase-agnostic content lives here so it isn't duplicated into every repo.
- **`gortex init`** writes per-repo artifacts: `.mcp.json`, `.claude/settings.{json,local.json}`, `CLAUDE.md` with the codebase overview and community routing, `.claude/skills/generated/` per-community SKILL.md files, and a marker-guarded community routing block in every other detected agent's per-repo instructions file (`AGENTS.md`, `.windsurfrules`, `GEMINI.md`, `.cursor/rules/gortex-communities.mdc`, etc.).

### One-time machine setup

```bash
gortex install                      # interactive-free: MCP + skills + slash commands at ~/.claude/
gortex install --start --track      # also spawn the daemon and track the current directory
gortex install --no-hooks           # skip user-level hook installation

# Daemon lifecycle (also spawned by `gortex install --start`):
gortex daemon start --detach        # spawn in background
gortex daemon status                # PID, uptime, memory, tracked repos, sessions, server roster
gortex daemon stop                  # graceful shutdown + final snapshot
gortex daemon restart               # stop + start
gortex daemon reload                # re-read config, pick up new/removed repos
gortex daemon logs -n 50            # tail the log file

# Multi-server roster — let the daemon route to additional Gortex servers (local sockets or remote HTTPS):
gortex daemon server list                                                  # show ~/.gortex/servers.toml
gortex daemon server add work --url https://gortex.work.example --auth-token-env WORK_TOK
gortex daemon server remove work

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

### Per-repo setup

```bash
cd ~/projects/myapp
gortex init                             # writes .mcp.json, .claude/settings.*, CLAUDE.md with community routing
gortex init --analyze                   # also index first for a richer CLAUDE.md overview
gortex init --no-skills                 # skip community-routing generation
gortex init --skills-min-size 5 --skills-max 10   # tune the generator
gortex init --hooks-only                # (re)install repo-local hooks only, skip everything else
gortex init --no-hooks                  # full init but skip hook installation

# Run the MCP server standalone (auto-detects daemon via stdio; --no-daemon forces embedded):
gortex mcp --index /path/to/repo --watch
gortex mcp --no-daemon --watch          # explicit embedded mode
```

### Other commands

```bash
gortex server --index .                  # HTTP/JSON API on :4747 (/v1/*). UI lives at github.com/gortexhq/web.
gortex savings                           # cumulative tokens saved + $ avoided across sessions
gortex version
```

## Multi-Repo Workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

### Workspace boundary

Every node and contract is keyed on a **workspace slug**, which is the hard graph boundary for cross-repo work. Two repos that should pair their contracts (an HTTP server and the client that calls it, a Kafka producer and its consumer, etc.) must declare the same `workspace:` in their `.gortex.yaml` — otherwise contract matching stops at the boundary and they look like orphans.

Slug resolution precedence (first match wins):

1. `RepoEntry.workspace` in `~/.config/gortex/config.yaml` — overrides everything, ideal for OSS / read-only repos where you don't want to leave an artifact in the tree
2. `workspace:` in the repo's own `.gortex.yaml` — the default for first-party repos
3. The repo prefix — fallback when neither is set, so each unconfigured repo gets its own isolated workspace

The same chain applies to the optional `project:` slug (a sub-bucket inside a workspace). On `gortex server`, the `--workspace` and `--scope-project` flags filter both indexing and queries: `gortex server --workspace api` will only load repos that resolve to the `api` workspace, and a typo'd value errors out at startup rather than producing an empty graph.

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

### Daemon tuning (optional)

The daemon's defaults handle typical workflows without configuration. These knobs exist for monorepos, branch-heavy workflows, or filesystems without fsnotify support.

```yaml
# ~/.config/gortex/config.yaml (or per-repo .gortex.yaml)
watch:
  debounce_ms: 150            # per-file patch debounce (default 150)

  # Storm mode — when more than N events land within the window,
  # switch from per-file debounced patching to a batched reconcile
  # that defers cross-file resolver + search work until a quiet
  # period has passed. Amortises the cost of bulk operations
  # (rsync, npm install, branch checkout, bulk format-on-save,
  # find-and-replace). Zero = disabled (default).
  storm_threshold: 0          # 0 disables; try 50 on monorepos
  storm_window_ms: 500
  storm_quiet_period_ms: 500
```

Environment variables:

- `GORTEX_RECONCILE_INTERVAL` — janitor tick that walks every tracked repo and runs `IncrementalReindex` against disk. Insurance against fsnotify gaps on NFS/SMB mounts, inotify watch-limit exhaustion, or daemon downtime where edits happened offline. Default `1h`; `"0"` or `"off"` disables; otherwise any Go duration string (e.g., `15m`).
- The daemon also watches each tracked repo's `.git/HEAD`, so branch switches and rebases reconcile incrementally (via `git diff --name-status`) rather than by re-indexing every changed file individually — no configuration needed.

### CLI

```bash
gortex track /path/to/repo          # Add a repo to the workspace
gortex untrack /path/to/repo        # Remove a repo from the workspace
gortex mcp --track /path/to/repo    # Track additional repos on startup
gortex mcp --project my-saas        # Set active project scope
gortex index repo-a/ repo-b/        # Index multiple repos
gortex status                       # Per-repo and per-project stats

# Stamp workspace / project slugs across tracked repos (migration helper)
gortex workspace list                                       # Show what each tracked repo currently declares
gortex workspace set backend api                            # Write workspace=api to backend's .gortex.yaml
gortex workspace set upstream-lib api --global              # OSS-friendly: pin to api in ~/.config/gortex/config.yaml
gortex workspace set-all api --root ~/projects/work --yes   # Bulk: stamp every tracked repo under a prefix

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

After `gortex install` (once per machine) and `gortex init` (once per repo), Claude Code automatically starts Gortex via `.mcp.json`. The agent gets:

- **Slash commands:** `/gortex-guide`, `/gortex-explore`, `/gortex-debug`, `/gortex-impact`, `/gortex-refactor` — installed to `~/.claude/commands/` by `gortex install`
- **Tool-usage skills:** installed to `~/.claude/skills/` by `gortex install` — one copy per user, used across every repo
- **PreToolUse hook:** automatic graph context + graph-tool suggestions on Read/Grep/Glob
- **PreCompact hook:** condensed orientation snapshot injected before context compaction so the agent resumes without re-exploring
- **Stop hook:** post-task diagnostics — tests to run, guard violations, dead code, and contract issues on the changed symbols — injected as context before the agent hands off
- **CLAUDE.md:** per-repo codebase overview (via `--analyze`) plus a marker-guarded community routing block written by `gortex init --skills`

## Usage with other agents

`gortex install` (user-level) and `gortex init` (repo-level) together auto-detect and configure 14 other AI coding assistants — Kiro, Cursor, VS Code / Copilot, Windsurf, Continue.dev, Cline, OpenCode, Antigravity, Codex CLI, Gemini CLI, Zed, Aider, Kilo Code, OpenClaw. Each adapter writes only when its host is present on the machine, and every re-run is idempotent.

Tool-usage guidance for agents that have a user-level surface (Claude Code, Antigravity) lives once per user; for the rest, MCP tool descriptions carry the teaching and `gortex init` adds only a per-repo community-routing block — no more duplicated instructions blocks in every repo.

- **Adapter matrix + per-agent schema notes:** [`docs/agents.md`](docs/agents.md)
- **Audit what's currently configured:** `gortex init doctor` (zero-op; `--json` for CI consumers)
- **Constrain setup:** `gortex init --agents=claude-code,cursor` or `--agents-skip=antigravity` (same flags accepted by `gortex install`)
- **CI / scripted install:** `gortex install --yes --json` then `gortex init --yes --json --dry-run`

## CLI Commands

```
gortex install               One-time machine-wide setup (user-level MCP, skills, hooks, daemon wiring)
gortex init [path]           Per-repo setup (.mcp.json, hooks, community routing, per-community SKILL.md)
gortex init doctor           Zero-op drift report across all detected agents (human or --json)
gortex mcp [flags]            Start the MCP stdio server (auto-detects daemon; --no-daemon / --proxy; --server adds HTTP API)
gortex server [flags]         Start the HTTP/JSON API under /v1/* (--bind, --auth-token, --watch, --cors-origin)
gortex daemon <subcommand>   start / stop / restart / reload / status / logs / install-service / service-status / uninstall-service / server (multi-server roster)
gortex eval <subcommand>     Retrieval + token benchmarks — recall, embedders, swebench, tokens
gortex eval-server [flags]   HTTP server used by the swebench harness
gortex context [flags]       Generate portable context briefing for a task
gortex savings [flags]       Show cumulative token savings + cost avoided across sessions
gortex index [path...]       Index one or more repositories and print stats
gortex status [flags]        Show index status (per-repo and per-project in multi-repo mode)
gortex track <path>          Add a repository to the tracked workspace
gortex untrack <path>        Remove a repository from the tracked workspace
gortex workspace <sub>       list / set / set-all — manage workspace + project slugs across tracked repos
gortex config exclude ...    add / list / remove entries in the effective ignore list
gortex query <subcommand>    Query the knowledge graph from the CLI
gortex clean                 Remove Gortex files from a project
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

## MCP Tools (64)

### Core Navigation
| Tool | Description |
|------|-------------|
| `graph_stats` | Node/edge counts by kind, language, per-repo stats, and session token savings |
| `search_symbols` | Find symbols by name (replaces Grep). Accepts `repo`, `project`, `ref` params |
| `winnow_symbols` | Structured constraint-chain retrieval — `kind`, `language`, `community`, `path_prefix`, `min_fan_in`, `min_fan_out`, `min_churn`, `text_match` with per-axis score contributions |
| `get_symbol` | Symbol location and signature (replaces Read). Accepts `repo`, `project`, `ref` params |
| `get_file_summary` | All symbols and imports in a file. Accepts `repo`, `project`, `ref` params |
| `get_editing_context` | **Primary pre-edit tool** — symbols, signatures, callers, callees |
| `get_repo_outline` | Narrative single-call repo overview — top languages, communities, hotspots, most-imported files, entry points |
| `plan_turn` | Opening-move router — returns ranked next calls with pre-filled args for a task description (~200 tokens) |

### Graph Traversal
| Tool | Description |
|------|-------------|
| `get_dependencies` | What a symbol depends on |
| `get_dependents` | What depends on a symbol (blast radius) |
| `get_call_chain` | Forward call graph |
| `get_callers` | Reverse call graph |
| `find_usages` | Every reference to a symbol |
| `find_implementations` | Types implementing an interface |
| `find_overrides` | Methods that override (children) or are overridden by (parents) a method — backed by `EdgeOverrides` |
| `get_class_hierarchy` | Multi-hop inheritance subgraph around a type, interface, or method — walks `EdgeExtends` + `EdgeImplements` + `EdgeComposes` (type nodes) and `EdgeOverrides` (method nodes); `direction` ∈ up / down / both, `include_methods` pulls members + their override chain |
| `get_cluster` | Bidirectional neighborhood |

### Dataflow (CPG-lite)
| Tool | Description |
|------|-------------|
| `flow_between` | Ranked dataflow paths between two symbols — walks `value_flow` / `arg_of` / `returns_to` edges |
| `taint_paths` | Pattern-driven source→sink dataflow sweep for security and architecture audits |

### Structural Search
| Tool | Description |
|------|-------------|
| `search_ast` | Cross-language structural search by AST shape — raw tree-sitter S-expression `pattern` or a bundled `detector` (e.g. `sql-string-concat`, `weak-crypto`, `hardcoded-secret`) |

### Diagnostics & Code Actions
Wired across every running language server (gopls, tsserver, pyright, rust-analyzer, …).
| Tool | Description |
|------|-------------|
| `subscribe_diagnostics` | Opt the session into push `notifications/diagnostics`; initial state replays immediately, deltas thereafter. Filter by `min_severity` / `path_prefix` |
| `unsubscribe_diagnostics` | Opt back out — idempotent, fires automatically on session disconnect |
| `get_diagnostics` | Latest stored diagnostics for a file; `wait: true` blocks on the first publish |
| `get_code_actions` | LSP code actions (quickfix / organizeImports / refactor / source) at a file location |
| `apply_code_action` | Apply a single code action to disk — atomic temp+rename |
| `fix_all_in_file` | Loop codeAction → apply → re-collect until convergence over the whole file |

### Coding Workflow
| Tool | Description |
|------|-------------|
| `get_symbol_source` | Source code of a single symbol (80% fewer tokens than Read). Returns `tokens_saved` per call |
| `batch_symbols` | Multiple symbols with source/callers/callees in one call |
| `find_import_path` | Correct import path for a symbol |
| `explain_change_impact` | Risk-tiered blast radius with affected processes |
| `get_recent_changes` | Files/symbols changed since timestamp |
| `edit_symbol` | Edit a symbol's source directly by ID — no Read needed |
| `edit_file` | Edit any file (markdown, config, spec, template, source) by exact string replacement — accepts absolute paths or repo-rooted paths. Kills the Read-before-Edit stall on files not in the graph |
| `write_file` | Create or overwrite any file with given content — atomic temp+rename, re-indexes on write |
| `rename_symbol` | Coordinated multi-file rename with all references |

### Agent-Optimized (token efficiency)
| Tool | Description |
|------|-------------|
| `smart_context` | Task-aware minimal context — replaces 5-10 exploration calls |
| `get_edit_plan` | Dependency-ordered edit sequence for multi-file refactors |
| `get_test_targets` | Maps changed symbols to test files and run commands |
| `get_untested_symbols` | Inverse of `get_test_targets` — functions/methods not reached from any test file, ranked by fan-in |
| `suggest_pattern` | Extracts code pattern from an example — source, registration, tests |
| `export_context` | Portable markdown/JSON context briefing for sharing outside MCP |
| `feedback` | `action: "record"`: report useful/missing symbols. `action: "query"`: aggregated stats — most useful, most missed, accuracy metrics |
| `ask` | Optional in-process LLM research agent (`-tags llama` + `llm.model`) — navigates the graph and returns a synthesized answer; `chain: true` for cross-repo call-chain tracing |

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
| `analyze` | Unified graph analysis dispatcher. `kind` ∈ `dead_code`, `hotspots`, `cycles`, `would_create_cycle`, `todos`, `blame`, `coverage`, `coverage_gaps`, `coverage_summary`, `stale_code`, `stale_flags`, `ownership`, `releases`, `cgo_users`, `wasm_users`, `orphan_tables`, `unreferenced_tables`, `channel_ops`, `goroutine_spawns`, `field_writers`, `annotation_users`, `config_readers`, `event_emitters`, `error_surface`, `external_calls`, `routes`, `models`, `components`, `k8s_resources`, `images`, `kustomize`, `cross_repo`, `dbt_models` |
| `find_clones` | Near-duplicate function/method clusters from the MinHash + LSH `similar_to` layer; `dead_only: true` finds dead duplicates of live code |
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
| `list_repos` | List every project/repo in the active workspace |
| `workspace_info` | Workspace identity — bind mode, root directory, marker contents, discovered member set |

## MCP Resources (16)

Read-only, URI-addressable, no args. Clients that speak resources can `resources/subscribe` once and receive `notifications/resources/updated` after each graph re-warm — no polling.

| Resource | Description |
|----------|-------------|
| `gortex://session` | Current session state and activity |
| `gortex://stats` | Graph statistics (node/edge counts) |
| `gortex://schema` | Graph schema reference |
| `gortex://index-health` | Health score, parse failures, stale files |
| `gortex://workspace` | Workspace identity and discovered member set |
| `gortex://repos` | Tracked repo / project list |
| `gortex://active-project` | Active project name and member repos |
| `gortex://communities` | Community list with cohesion scores |
| `gortex://community/{id}` | Single community detail |
| `gortex://processes` | Execution flow list |
| `gortex://process/{id}` | Single process trace |
| `gortex://report` | High-level orientation — graph size, top languages/kinds, hotspot / dead-code / todo counts |
| `gortex://god-nodes` | Top 20 hotspots |
| `gortex://surprises` | Cycles + dead code + cross-community call hubs |
| `gortex://audit` | `audit_agent_config` with discovery defaults |
| `gortex://questions` | TODO / FIXME / XXX / HACK / QUESTION rollup grouped by tag and assignee |

## MCP Prompts (3)

| Prompt | Description |
|--------|-------------|
| `pre_commit` | Review uncommitted changes — shows changed symbols, blast radius, risk level, affected tests |
| `orientation` | Orient in an unfamiliar codebase — graph stats, communities, execution flows, key symbols |
| `safe_to_change` | Analyze whether it's safe to change specific symbols — blast radius, edit plan, affected tests |

## Web UI

The web UI lives in its own repo at [`gortexhq/web`](https://github.com/gortexhq/web) so it can be deployed independently of the backend (Vercel / a static host / your own Next.js deployment). It's a standalone Next.js 15 app that talks to `gortex server` over `/v1/*`:

```bash
# 1) Start the HTTP backend (localhost:4747 by default, bearer-auth in non-localhost binds)
gortex server --index /path/to/repo --watch

# 2) Clone and run the UI in another terminal
git clone https://github.com/gortexhq/web.git gortex-web && cd gortex-web
echo 'NEXT_PUBLIC_GORTEX_URL=http://localhost:4747' > .env.local
npm install && npm run dev
# Open http://localhost:3000
```

| Page | Features |
|------|----------|
| **Dashboard** | Health, stats, language pie chart, node kind bar chart |
| **Graph Explorer** | Sigma.js 2D + five react-three-fiber 3D modes (City / Strata / Galaxies / Constellation / Graph3D), node filters, selection, detail panel |
| **Search** | Semantic + BM25 search via `/v1/*`, results grouped by kind |
| **Symbol Detail** | Source code, signature, callers/callees/usages/deps tabs |
| **Communities** | Community cards with cohesion bars, expandable members |
| **Processes** | Collapsible call-tree steps, product vs test process split |
| **Analysis** | Dead code, hotspots, cycles, index health — 4 tabs |
| **Contracts** | API contracts (HTTP, gRPC, GraphQL, topics, WebSocket, env vars) with provider/consumer matching, request/response type tracing, `yours / tests / deps / all` scope filter |
| **Services** | Service-level graph visualization with per-repo stats |
| **AI Chat** | LLM-powered chat with code context (placeholder) |

## Server Mode

The `gortex server` command exposes all MCP tools as an HTTP/JSON API under versioned `/v1/*` routes:

```bash
# Standalone HTTP backend (default bind 127.0.0.1:4747)
gortex server --index /path/to/repo --watch

# Non-localhost bind requires an auth token
gortex server --index . --bind 0.0.0.0 --auth-token "$(openssl rand -hex 32)"

# HTTP API alongside MCP stdio (same process)
gortex mcp --index /path/to/repo --server --port 8765
```

**Endpoints (all under `/v1/`):**
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/health` | GET | Status, node/edge counts, uptime |
| `/v1/tools` | GET | List all available tools with descriptions |
| `/v1/tools/{name}` | POST | Invoke any MCP tool with JSON arguments. Accepts `?format=gcx` or top-level `"format"` in the body |
| `/v1/stats` | GET | Graph statistics by kind and language, plus `server_id` + `started_at` |
| `/v1/graph` | GET | Full brief-graph dump (nodes + edges + stats); accepts `?project=` and/or `?repo=` for scoping |
| `/v1/events` | GET | SSE stream of graph-change events (requires `--watch`). Accepts `?token=<t>` for `EventSource` auth |

**Auth & binding.** The server defaults to `--bind 127.0.0.1` and runs unauthenticated on localhost only (logs `server: unauthenticated mode; localhost only`). Set `--auth-token <token>` or `$GORTEX_SERVER_TOKEN` to require `Authorization: Bearer <token>` on every `/v1/*` request (constant-time compare; CORS preflights bypass). Non-localhost binds without a token are rejected at startup. CORS origin is configurable via `--cors-origin` (default `*`). `--bind` also accepts `unix:///path/to.sock` for a Unix-domain socket.

**Scoping the graph.** Pass `--workspace <slug>` to restrict both indexing and queries to repos that resolve to that workspace, and `--scope-project <slug>` to narrow further. A scope that matches zero tracked repos errors out at startup rather than silently producing an empty graph.

**Multi-server roster.** When the daemon is running, it can route MCP traffic across multiple Gortex servers — a local Unix socket for the repos on this machine, plus one or more remote HTTPS servers for shared / cloud indexes. The roster lives at `~/.gortex/servers.toml`; manage it with `gortex daemon server list / add / remove`. Auth tokens can be embedded directly (`--auth-token`) or pulled from an env var the daemon reads at request time (`--auth-token-env`, preferred). Restart the daemon to pick up roster changes.

## Cross-Repo API Contracts

Gortex detects API contracts across repos and matches providers to consumers:

```bash
# After indexing, contracts are auto-detected
gortex server --index .

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

`gortex init --skills` (default on) analyzes your codebase, detects functional communities via Louvain clustering, and generates targeted SKILL.md files that Claude Code auto-discovers:

```bash
# Runs as part of `gortex init` by default — community generation is folded in
gortex init

# Tune or disable:
gortex init --skills-min-size 5 --skills-max 10
gortex init --no-skills
```

Each generated skill includes:
- **Community metadata** — size, file count, cohesion score
- **Key files table** — files and their symbols
- **Entry points** — main functions, handlers, controllers detected via process analysis
- **Cross-community connections** — which other areas this community interacts with
- **MCP tool invocations** — pre-written `get_communities`, `smart_context`, `find_usages` calls

For Claude Code, skills are written to `.claude/skills/generated/<DirName>/SKILL.md`, and a routing table is inserted into `CLAUDE.md` between `<!-- gortex:communities:start/end -->` markers. Every other detected agent gets the same routing table inside its per-repo instructions surface (`AGENTS.md` for Codex/OpenCode, `.windsurfrules` for Windsurf, `GEMINI.md` for Gemini CLI, `.cursor/rules/gortex-communities.mdc` for Cursor, etc.) — so the routing is consistent across tools on the same repo.

## Semantic Search

Hybrid BM25 + vector search with Reciprocal Rank Fusion (RRF). Multiple embedding tiers:

```bash
# Built-in word vectors (always available, zero setup)
gortex mcp --index . --embeddings

# Ollama (best quality, local)
ollama pull nomic-embed-text
gortex mcp --index . --embeddings-url http://localhost:11434

# OpenAI (best quality, cloud)
gortex mcp --index . --embeddings-url https://api.openai.com/v1 \
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

## LLM Features (optional)

Gortex can delegate code-intelligence work to an LLM. Two features, both **off by default** and gated on configuring a provider:

- **`ask` MCP tool** — a research agent that drives Gortex's own tools (search, callers, contracts, dependencies) to answer an open-ended question and returns a synthesized answer, instead of the calling agent issuing many tool calls itself. `chain: true` traces cross-system call chains.
- **`search_symbols` `assist` arg** — LLM-assisted ranking on `search_symbols`: `auto` (engage on natural-language queries only), `on`, `off`, `deep` (adds a body-grounded verification pass that reads candidate code + callers and honestly drops irrelevant matches).

### Providers

The backend is chosen by the `llm.provider` key. The three HTTP providers are pure Go — available in any build; only `local` needs a `-tags llama` build (it embeds llama.cpp).

| `llm.provider` | Backend | Needs |
|----------------|---------|-------|
| `local` | in-process llama.cpp | a `-tags llama` build + a `.gguf` model file |
| `anthropic` | Anthropic Messages API | `ANTHROPIC_API_KEY` |
| `openai` | OpenAI Chat Completions | `OPENAI_API_KEY` |
| `ollama` | Ollama daemon | a running Ollama + a pulled model |

### Configuration

The `llm:` block goes in `~/.config/gortex/config.yaml` or a per-repo `.gortex.yaml` (repo-local wins per field, global fills the rest). Configure only the provider you use:

```yaml
# ~/.config/gortex/config.yaml (or per-repo .gortex.yaml)
llm:
  provider: local            # local | anthropic | openai | ollama
  max_steps: 16              # agent tool-loop cap (provider-agnostic)

  local:                     # provider: local — requires a `-tags llama` build
    model: ~/models/qwen2.5-coder-7b-instruct-q4_k_m.gguf
    ctx: 4096                # context window in tokens
    gpu_layers: 999          # layers to offload to GPU (0 = CPU-only)
    template: chatml         # chatml | llama3

  anthropic:                 # provider: anthropic
    model: claude-sonnet-4-6
    api_key_env: ANTHROPIC_API_KEY   # env var holding the key (this is the default)
    # base_url: https://api.anthropic.com

  openai:                    # provider: openai
    model: gpt-4o
    api_key_env: OPENAI_API_KEY

  ollama:                    # provider: ollama
    model: qwen2.5-coder:7b
    host: http://localhost:11434
```

Env overrides: `GORTEX_LLM_PROVIDER`, `GORTEX_LLM_MODEL` (targets the active provider's model), `GORTEX_LLM_MAX_STEPS`. API keys are read from the env var named by `api_key_env` — never stored in the config file.

If the active provider can't be constructed (missing model or API key, or `local` without a `-tags llama` build), the daemon logs a warning and the LLM features stay absent — the rest of Gortex is unaffected. If the `ask` tool isn't in `tools/list`, no provider is configured.

The `assist` prompts are tiered automatically — terser for hosted frontier models, rule-heavy for small local ones. `deep` mode in particular benefits from a 7B-class or hosted model; small local models are unreliable on its disambiguation cases.

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
gortex mcp --index /path/to/repo

# Custom cache directory
gortex mcp --index /path/to/repo --cache-dir /tmp/gortex-cache

# Disable caching
gortex mcp --index /path/to/repo --no-cache
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
  CLI (cobra)    ──> MultiIndexer ──> In-Memory Graph (shared, per-repo indexed)
  MCP (stdio)    ──────────────────> Query Engine (repo/project/ref scoping)
  HTTP /v1/*     ──────────────────> same tools + /v1/graph + /v1/events (SSE)
  Daemon (unix)  ──────────────────> shared graph for every MCP client, session isolation
  MCP Prompts    ──────────────────> (pre_commit, orientation, safe_to_change)
  MCP Resources  ──────────────────> (16 read-only URIs — bootstrap state + analyzer rollups)
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

**Node kinds:**
- Code structure: `file`, `package`, `function`, `method`, `type`, `interface`, `field`, `variable`, `constant`, `import`, `contract`, `param`, `closure`, `enum_member`, `generic_param`
- Coverage extensions: `module`, `table`, `column`, `config_key`, `flag`, `event`, `migration`, `fixture`, `todo`, `team`, `license`, `release`
- Infrastructure: `resource` (K8s manifest), `kustomization` (Kustomize overlay), `image` (Dockerfile FROM / K8s `container.image`)

**Edge kinds:**
- Calls / structure: `calls`, `imports`, `defines`, `implements`, `extends`, `overrides`, `references`, `member_of`, `instantiates`, `provides`, `consumes`, `composes`, `aliases`, `typed_as`, `returns`, `captures`, `param_of`
- Concurrency / mutation: `spawns`, `sends`, `recvs`, `reads`, `writes`, `reads_config`, `writes_config`
- Dataflow (CPG-lite): `value_flow`, `arg_of`, `returns_to`
- Metadata: `annotated`, `emits`, `throws`, `queries`, `reads_col`, `writes_col`, `toggles_flag`, `depends_on_module`, `matches`, `generated_by`, `tests`, `covered_by`, `owns`, `authored`, `licensed_as`
- Framework / infrastructure: `handles_route`, `models_table`, `renders_child`, `configures`, `mounts`, `exposes`, `depends_on`, `uses_env`
- Similarity: `similar_to` (MinHash + LSH near-duplicate clones; `Meta["similarity"]` carries the estimated Jaccard score)
- Cross-repo: `cross_repo_calls` / `cross_repo_implements` / `cross_repo_extends` — materialised whenever a `calls` / `implements` / `extends` edge's endpoints live in different repos

**Multi-repo fields:** Nodes carry `repo_prefix` (empty in single-repo mode). Edges carry `cross_repo` (true when connecting nodes in different repos). Node IDs use `<repo_prefix>/<path>::<Symbol>` format in multi-repo mode.

## Language Support

Gortex indexes **256 languages** across three tiers (bespoke tree-sitter, regex, and forest-backed signature-only) — see **[docs/languages.md](docs/languages.md)** for the full table (extensions, engine, extracted symbols per language).

## Building

```bash
make build          # Build with version from git tags
make test           # go test -race ./...
make bench          # Run all benchmarks
make lint           # golangci-lint
make fmt            # gofmt -s
make install        # go install with version ldflags
```

Requires Go 1.26+ and CGO enabled (for tree-sitter C bindings).

## License

Gortex is **source-available** under a custom license based on PolyForm Small Business. See [LICENSE.md](LICENSE.md) for full terms.

**Free for:** individuals, open-source projects, small businesses (<50 employees / <$500K revenue), nonprofits, education, government, socially critical orgs.

**Commercial license required for:** organizations with 50+ employees or $500K+ revenue, competing products, resale/bundling. Contact [license@zzet.org](mailto:license@zzet.org).

**Contributors:** active contributors listed in [CONTRIBUTORS.md](CONTRIBUTORS.md) receive a free non-competing commercial license for their employer.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on adding features, language extractors, and submitting PRs. Active contributors receive a [contributor commercial license](LICENSE.md#part-3-contributor-license).
