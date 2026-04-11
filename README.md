# Gortex

[![CI](https://github.com/zzet/gortex/actions/workflows/ci.yml/badge.svg)](https://github.com/zzet/gortex/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/zzet/gortex)](https://goreportcard.com/report/github.com/zzet/gortex)

Code intelligence engine that indexes repositories into an in-memory knowledge graph and exposes it via CLI, MCP Server, and web UI.

Built for AI coding agents (Claude Code, Kiro, Cursor, Windsurf, Copilot, Continue.dev, Cline, OpenCode, Antigravity) — one `smart_context` call replaces 5-10 file reads, cutting token usage by ~94%.

![Gortex Web UI — force-directed knowledge graph visualization](assets/graph.png)

## Features

- **Knowledge graph** — every file, symbol, import, call chain, and type relationship in one queryable structure
- **Multi-repo workspaces** — index multiple repositories into a single graph with cross-repo symbol resolution, project grouping, reference tags, and per-repo scoping
- **33 languages** — Go, TypeScript, JavaScript, Python, Rust, Java, C#, Kotlin, Swift, Scala, PHP, Ruby, Elixir, C, C++, Dart, OCaml, Lua, Zig, Haskell, Clojure, Erlang, R, Bash/Zsh, SQL, Protobuf, Markdown, HTML, CSS, YAML, TOML, HCL/Terraform, Dockerfile
- **48 MCP tools** — symbol lookup, call chains, blast radius, community detection, process discovery, contract detection, cycle detection, dead code analysis, scaffolding, inline editing, symbol renaming, multi-repo management, and 6 agent-optimized tools
- **Semantic search** — hybrid BM25 + vector search with RRF fusion. Built-in GloVe word vectors for offline use, or connect to Ollama/OpenAI for transformer-quality embeddings. Build tags for ONNX, GoMLX, and Hugot offline transformer backends
- **Type-aware resolution** — infers receiver types from variable declarations, composite literals, and Go constructor conventions to disambiguate same-named methods across types
- **On-disk persistence** — snapshots the graph on shutdown, restores on startup with incremental re-indexing of only changed files (~200ms vs 3-5s full re-index)
- **Bridge Mode** — HTTP/JSON API exposing all MCP tools for IDE plugins, CI tools, and web UIs with CORS support and tool discovery endpoint
- **7 MCP resources** — lightweight graph context without tool calls
- **3 MCP prompts** — `pre_commit`, `orientation`, `safe_to_change` for guided workflows
- **Two-tier config** — global config (`~/.config/gortex/config.yaml`) for projects and repo lists, per-repo `.gortex.yaml` for guards, excludes, and local overrides
- **Guard rules** — project-specific constraints (co-change, boundary) enforced via `check_guards`
- **Watch mode** — surgical graph updates on file change across all tracked repos, live sync with agents
- **Web UI** — Sigma.js force-directed visualization with node size proportional to importance
- **IMPLEMENTS inference** — structural interface satisfaction for Go, TypeScript, Java, Rust, C#, Scala, Swift, Protobuf
- **PreToolUse hooks** — automatic graph context injection on Read and Grep
- **Benchmarked** — per-language parsing, query engine, indexer benchmarks
- **Per-community skills** — `gortex skills` auto-generates SKILL.md per detected community with key files, entry points, cross-community connections, and MCP tool invocations for Claude Code auto-discovery
- **Eval framework** — SWE-bench harness for A/B benchmarking tool effectiveness with Docker-based environments and multi-model support
- **Zero dependencies** — everything runs in-process, in memory, no external services

## Quick Start

```bash
# Build (requires CGO for tree-sitter C bindings)
go build -o gortex ./cmd/gortex/

# Set up Gortex for a project (creates configs for Claude Code, Kiro, Cursor, Copilot, Windsurf, Continue.dev, Cline, OpenCode, Antigravity — auto-detects installed tools)
gortex init /path/to/repo

# Or with codebase analysis for a richer CLAUDE.md
gortex init --analyze /path/to/repo

# Index a repo and print stats
gortex status --index /path/to/repo

# Start MCP server with watch mode and graph caching
gortex serve --index /path/to/repo --watch

# Generate per-community skills for Claude Code
gortex skills /path/to/repo

# Start HTTP bridge API for external integrations
gortex bridge --index /path/to/repo --web --cors-origin '*'

# Multi-repo: track additional repos and set active project
gortex serve --index /path/to/repo --track /path/to/other-repo --project my-project
gortex track /path/to/another-repo
gortex untrack /path/to/another-repo
```

## Multi-Repo Workspaces

Gortex can index multiple repositories into a single shared graph, enabling cross-repo symbol resolution, impact analysis, and navigation.

### Configuration

Two-tier config hierarchy:

- **Global config** (`~/.config/gortex/config.yaml`) — projects, repo lists, active project, reference tags
- **Workspace config** (`.gortex.yaml` per repo) — guards, excludes, local overrides (workspace wins when both define the same setting)

```yaml
# ~/.config/gortex/config.yaml
active_project: my-saas

repos:
  - path: /home/user/projects/gortex
    name: gortex

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
- **PreToolUse hook:** automatic graph context on Read/Grep calls
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

- **MCP config:** `.vscode/mcp.json` — project-level config for Copilot Chat agent mode. All 48 Gortex tools are available in Copilot's agent mode.

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

## MCP Tools (48)

### Core Navigation
| Tool | Description |
|------|-------------|
| `graph_stats` | Node/edge counts by kind, language, and per-repo stats |
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
| `get_symbol_signature` | Just the signature, no body |
| `get_symbol_source` | Source code of a single symbol (80% fewer tokens than Read) |
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

### Analysis
| Tool | Description |
|------|-------------|
| `get_communities` | Functional clusters (Louvain community detection) |
| `get_community` | Members and cohesion for one community |
| `get_processes` | Discovered execution flows |
| `get_process` | Step-by-step trace of an execution flow |
| `detect_changes` | Git diff mapped to affected symbols |
| `index_repository` | Index or re-index a repository path |
| `get_contracts` | List detected API contracts (HTTP, gRPC, GraphQL, topics, WebSocket, env, OpenAPI) |
| `check_contracts` | Detect contract mismatches: orphan providers and orphan consumers |

### Proactive Safety
| Tool | Description |
|------|-------------|
| `verify_change` | Check proposed signature changes against all callers and interface implementors |
| `check_guards` | Evaluate project guard rules (`.gortex.yaml`) against changed symbols |
| `would_create_cycle` | Check if adding a dependency would create a circular dependency |

### Code Quality
| Tool | Description |
|------|-------------|
| `find_dead_code` | Symbols with zero incoming edges (excludes entry points, tests, exports) |
| `find_hotspots` | Symbols ranked by fan-in, fan-out, and community boundary crossings |
| `find_cycles` | Circular dependency detection via Tarjan's SCC, classified by severity |
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
get_contracts                    # list all detected contracts
check_contracts                  # find mismatches and orphans
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
- **MCP tool invocations** — pre-written `get_community`, `smart_context`, `find_usages` calls

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

| Tier | Flag | Quality | Offline |
|------|------|---------|---------|
| Built-in | `--embeddings` | Basic (GloVe word averaging) | Yes |
| API | `--embeddings-url` | Best (transformer model) | No |
| ONNX | `--embeddings` + build tag | Best | Yes |
| GoMLX | `--embeddings` + build tag | Good | Yes |
| Hugot | `--embeddings` + build tag | Good | Yes |

Offline transformer backends via build tags:
```bash
go build -tags embeddings_onnx ./cmd/gortex/   # needs: brew install onnxruntime
go build -tags embeddings_gomlx ./cmd/gortex/  # auto-downloads XLA plugin
go build -tags embeddings_hugot ./cmd/gortex/  # auto-downloads XLA plugin
```

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
