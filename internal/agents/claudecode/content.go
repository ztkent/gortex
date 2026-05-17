// Package claudecode implements the Gortex init integration for
// Anthropic's Claude Code CLI. It manages six on-disk artifacts:
//
//   - .mcp.json                   (project-level MCP stanza, shared)
//   - .claude/commands/gortex-*.md (slash commands)
//   - .claude/settings.json        (MCP tool permissions, shared)
//   - .claude/settings.local.json  (PreToolUse/PreCompact/Stop hooks)
//   - CLAUDE.md                    (appended instructions block)
//   - ~/.claude/skills/gortex-*    (user-level skills)
//
// Global mode additionally writes ~/.claude.json (user-level MCP
// stanza) and ~/.claude/settings.local.json (user-level hooks).
//
// The bulky content blocks (CLAUDE.md instructions, slash-command
// markdown, skill frontmatter) live in this file so the adapter
// logic in adapter.go stays readable. Content is kept as Go string
// constants rather than embedded files so byte-for-byte reproduction
// of the pre-refactor behaviour is trivially verifiable.
package claudecode

import "github.com/zzet/gortex/internal/agents"

// ProjectMCPJSON is the starter content for a project's .mcp.json
// when no file exists yet.
const ProjectMCPJSON = `{
  "mcpServers": {
    "gortex": {
      "command": "gortex",
      "args": [
        "mcp",
        "--index", ".",
        "--watch"
      ],
      "env": {
        "GORTEX_INDEX_WORKERS": "${GORTEX_WORKERS:-8}"
      }
    }
  }
}
`

// ClaudeMdBlock is the canonical "use Gortex tools instead of
// Read/Grep" instructions appended to a project's CLAUDE.md. The
// byte sequence here must match what the previous implementation
// wrote, or the idempotency check (contains "## MANDATORY: Use
// Gortex MCP tools") would misfire on re-runs.
//
// The shared body lives in `agents.InstructionsBody` so every
// doc-aware adapter writes the same rule table. Claude Code
// additionally advertises its slash commands — appended here so the
// block stays self-contained for CLAUDE.md readers.
const ClaudeMdBlock = agents.InstructionsBody + `
## Gortex slash commands

Discovery & analysis: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-dataflow-trace`" + `, ` + "`/gortex-cross-repo-usage`" + `

Refactor & edit: ` + "`/gortex-refactor`" + `, ` + "`/gortex-safe-edit`" + `, ` + "`/gortex-rename`" + `, ` + "`/gortex-extract-function`" + `, ` + "`/gortex-fix-all`" + `, ` + "`/gortex-add-test`" + `

The edit and refactor skills enforce a tool-call order — they exist to keep you on the speculative-execution path (` + "`preview_edit`" + ` → ` + "`simulate_chain`" + ` → ` + "`batch_edit`" + `) and the LSP code-actions path (` + "`get_code_actions`" + ` → ` + "`apply_code_action`" + `) instead of going straight to ` + "`Edit`" + ` / ` + "`Write`" + `.
`

// ClaudeMdSentinel is the substring used to detect whether
// ClaudeMdBlock has already been appended to a project's
// CLAUDE.md. Kept as a named constant so the doctor subcommand can
// query it without pulling in the entire block. Aliased to the shared
// sentinel so idempotency works across adapters writing to the same
// file (e.g. AGENTS.md shared by Codex + Opencode).
const ClaudeMdSentinel = agents.InstructionsSentinel

// SlashCommands maps the filename under .claude/commands/ to its
// markdown content. Each file is a slash command Claude Code
// auto-discovers.
var SlashCommands = map[string]string{
	"gortex-guide.md":             commandGuide,
	"gortex-explore.md":           commandExplore,
	"gortex-debug.md":             commandDebug,
	"gortex-impact.md":            commandImpact,
	"gortex-refactor.md":          commandRefactor,
	"gortex-safe-edit.md":         commandSafeEdit,
	"gortex-fix-all.md":           commandFixAll,
	"gortex-extract-function.md":  commandExtractFunction,
	"gortex-rename.md":            commandRename,
	"gortex-cross-repo-usage.md":  commandCrossRepoUsage,
	"gortex-dataflow-trace.md":    commandDataflowTrace,
	"gortex-add-test.md":          commandAddTest,
}

// GlobalSkills maps the directory name under ~/.claude/skills/ to
// the SKILL.md body. Skill files get YAML frontmatter so Claude Code
// can show them in its skill picker.
var GlobalSkills = map[string]string{
	"gortex-guide": `---
name: gortex-guide
description: "Use when the user asks about Gortex — available tools, graph schema, or workflow reference. Examples: \"What Gortex tools are available?\", \"How do I use Gortex?\""
---
` + commandGuide,

	"gortex-explore": `---
name: gortex-explore
description: "Use when the user asks how code works, wants to understand architecture, trace execution flows, or explore unfamiliar parts of the codebase. Examples: \"How does X work?\", \"What calls this function?\", \"Show me the auth flow\""
---
` + commandExplore,

	"gortex-debug": `---
name: gortex-debug
description: "Use when the user is debugging a bug, tracing an error, or asking why something fails. Examples: \"Why is X failing?\", \"Where does this error come from?\", \"Trace this bug\""
---
` + commandDebug,

	"gortex-impact": `---
name: gortex-impact
description: "Use when the user wants to know what will break if they change something, or needs safety analysis before editing code. Examples: \"Is it safe to change X?\", \"What depends on this?\", \"What will break?\""
---
` + commandImpact,

	"gortex-refactor": `---
name: gortex-refactor
description: "Use when the user wants to rename, extract, split, move, or restructure code safely. Examples: \"Rename this function\", \"Extract this into a module\", \"Refactor this class\""
---
` + commandRefactor,

	"gortex-safe-edit": `---
name: gortex-safe-edit
description: "Use when the user is about to edit source code and wants the edit to be safe — speculative preview, broken-callers / blast-radius check, then on-disk apply. Enforces preview_edit / simulate_chain before batch_edit. Examples: \"Change the signature of X safely\", \"Apply this WorkspaceEdit but verify first\", \"Speculate this edit before writing\""
---
` + commandSafeEdit,

	"gortex-fix-all": `---
name: gortex-fix-all
description: "Use when the user wants to clear LSP diagnostics — a single error, a file's worth of warnings, or the whole project's source.fixAll bundle. Enforces subscribe_diagnostics / get_code_actions / apply_code_action / fix_all_in_file in order. Examples: \"Fix the diagnostics in this file\", \"Run source.fixAll\", \"Apply quick-fixes for these errors\""
---
` + commandFixAll,

	"gortex-extract-function": `---
name: gortex-extract-function
description: "Use when the user wants to extract code into a new function / method / variable via the language server's refactor actions (not a manual rewrite). Enforces get_editing_context / get_code_actions(kind=refactor.extract) / preview_edit / apply_code_action. Examples: \"Extract these lines into a helper\", \"Pull this into its own method\", \"Refactor this block into a function\""
---
` + commandExtractFunction,

	"gortex-rename": `---
name: gortex-rename
description: "Use when the user wants to rename a symbol and have every reference (definition + callers + tests + cross-repo consumers) updated atomically. Enforces search_symbols / verify_change / rename_symbol / batch_edit / check_guards in order. Examples: \"Rename Foo to Bar everywhere\", \"Rename this method\", \"Change the package name\""
---
` + commandRename,

	"gortex-cross-repo-usage": `---
name: gortex-cross-repo-usage
description: "Use when the user needs to see who uses a symbol across consumer repos, not just the current one. Enforces get_active_project / track_repository / find_usages partitioned by repo / analyze cross_repo. Examples: \"Who calls this across all our repos?\", \"What other services consume this API?\", \"Cross-repo blast radius\""
---
` + commandCrossRepoUsage,

	"gortex-dataflow-trace": `---
name: gortex-dataflow-trace
description: "Use when the user wants to trace where a value flows — through assignments, function args, returns, channels, or pub/sub topics. Enforces search_symbols / flow_between / taint_paths / analyze pubsub|channel_ops. Examples: \"Where does this value end up?\", \"Trace the taint from input to sink\", \"Find every flow from X to Y\""
---
` + commandDataflowTrace,

	"gortex-add-test": `---
name: gortex-add-test
description: "Use when the user wants to add tests for under-tested code — coverage gaps, untested symbols, regression repro. Enforces analyze coverage_gaps / get_untested_symbols / suggest_pattern / scaffold / get_test_targets. Examples: \"Add tests for X\", \"Cover the gaps in this package\", \"Write a test for this bug\""
---
` + commandAddTest,
}

const commandGuide = `# Gortex Guide

Quick reference for all Gortex MCP tools and the knowledge graph schema.

## Always Start Here

1. **Call ` + "`graph_stats`" + `** — confirm Gortex is running, get node/edge counts
2. **Match your task to a command below**
3. **Follow the command's workflow**

> If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with ` + "`path: \".\"`" + ` first.

## Commands

### Discovery & analysis

| Task                                                         | Command                    |
| ------------------------------------------------------------ | -------------------------- |
| Understand architecture / "How does X work?"                 | /gortex-explore            |
| Blast radius / "What breaks if I change X?"                  | /gortex-impact             |
| Trace bugs / "Why is X failing?"                             | /gortex-debug              |
| Trace dataflow / "Where does this value end up?"             | /gortex-dataflow-trace     |
| Cross-repo usage / "Who uses this across all our repos?"     | /gortex-cross-repo-usage   |
| Tools, schema reference                                      | /gortex-guide (this)       |

### Refactor & edit (enforce tool-call order)

These wrap the speculative-execution + LSP-code-actions plumbing so you do not bypass the safety steps by calling ` + "`Edit`" + ` / ` + "`Write`" + ` directly.

| Task                                                         | Command                    |
| ------------------------------------------------------------ | -------------------------- |
| Safe edit / "Apply this WorkspaceEdit but verify first"      | /gortex-safe-edit          |
| Rename / extract / split / restructure                       | /gortex-refactor           |
| Rename one symbol everywhere                                 | /gortex-rename             |
| Extract a function / method / variable via LSP refactor      | /gortex-extract-function   |
| Apply LSP quick-fixes / source.fixAll                        | /gortex-fix-all            |
| Add tests for under-covered code                             | /gortex-add-test           |

## Tools Reference

### Core Navigation
| Tool | What it gives you |
|------|-------------------|
| graph_stats | Node/edge counts by kind and language — session start orientation |
| search_symbols | Find symbols by keyword (BM25 + camelCase-aware). Use instead of Grep |
| winnow_symbols | Structured constraint chain: kind, language, community, path_prefix, min_fan_in, min_churn — returns ranked rows with per-axis score contributions. Use when free-text search is too coarse |
| get_symbol | Single symbol: location, signature, edges. Use instead of Read |
| get_file_summary | All symbols + imports in a file. Use instead of Read |
| get_editing_context | **Primary pre-edit tool.** Symbols, signatures, callers, callees for a file |

### Graph Traversal
| Tool | What it gives you |
|------|-------------------|
| get_dependencies | What a symbol depends on (forward: imports, calls, refs) |
| get_dependents | What depends on a symbol (backward: blast radius) |
| get_call_chain | Forward call graph from a function |
| get_callers | Reverse call graph to a function |
| find_usages | Every reference to a symbol. Use instead of Grep |
| find_implementations | All types implementing an interface |
| get_cluster | Bidirectional neighborhood around a node |

### Coding Workflow
| Tool | What it gives you |
|------|-------------------|
| get_symbol_source | Source code of a single symbol — use instead of Read. Requires a symbol ID like ` + "`path/to/file.go::SymbolName`" + ` (call ` + "`get_file_summary`" + ` first if you only have a file path). Pass ` + "`if_none_match`" + ` with previous ` + "`etag`" + ` to get ` + "`not_modified`" + ` (skip re-reading unchanged source) |
| batch_symbols | Multiple symbols with source/callers/callees in one call |
| find_import_path | Correct import path for a symbol in a target file |
| explain_change_impact | Risk-tiered blast radius with affected processes/communities |
| edit_symbol | Edit symbol source by ID — no Read needed, resolves file + lines |
| edit_file | String-replace any file (markdown / config / spec / source) by absolute or repo-relative path. No pre-Read required. Atomic write (temp+rename), auto-reindex. ` + "`replace_all`" + ` for many occurrences; ` + "`dry_run`" + ` to preview. |
| write_file | Create or overwrite any file by absolute or repo-relative path. No pre-Read required. Atomic write, creates parent dirs, auto-reindex. ` + "`dry_run`" + ` to preview. |
| rename_symbol | Coordinated rename: generates edits for definition + all references |
| get_recent_changes | Files/symbols changed since timestamp (watch mode) |

### Agent-Optimized (token efficiency)
| Tool | What it gives you |
|------|-------------------|
| smart_context | Task-aware minimal context bundle — replaces 5-10 exploration calls |
| plan_turn | Suggested next tool calls for the current task — orchestrator for one turn |
| prefetch_context | Predicts needed symbols from task description + recent activity |
| get_edit_plan | Dependency-ordered edit sequence for multi-file refactors |
| get_test_targets | Maps changed symbols to test files and run commands |
| get_untested_symbols | Lists symbols with no covering test — candidates for new tests |
| suggest_pattern | Extracts code pattern from an example — source, registration, tests |
| export_context | Portable markdown/JSON briefing — share context outside MCP (Slack, PRs, docs) |

### Analysis
| Tool | What it gives you |
|------|-------------------|
| get_communities | Functional clusters via Louvain community detection (with id: returns single community details) |
| get_processes | Discovered execution flows (with id: returns single process step-by-step trace) |
| detect_changes | Git diff -> affected symbols -> blast radius |

### Proactive Safety
| Tool | What it gives you |
|------|-------------------|
| verify_change | Checks proposed signature changes against all callers and interface implementors |
| check_guards | Evaluates project guard rules (.gortex.yaml) against changed symbols |

### Dataflow (CPG-lite)
| Tool | What it gives you |
|------|-------------------|
| flow_between | Ranked dataflow paths between two symbol IDs. Walks ` + "`value_flow`" + ` (intra-procedural) ∪ ` + "`arg_of`" + ` (caller arg → callee param) ∪ ` + "`returns_to`" + ` (callee → assignment). Pass ` + "`max_depth`" + ` (default 8) and ` + "`max_paths`" + ` (default 10). |
| taint_paths | Pattern-driven dataflow sweep — every flow from a matching source to a matching sink. Patterns: bare token = name substring; ` + "`exact:Foo`" + `; ` + "`path:dir/`" + `; ` + "`kind:method`" + ` (clauses combine with AND). Sinks expand functions to their params automatically. |

### Structural Code Search
| Tool | What it gives you |
|------|-------------------|
| search_ast (detector mode) | Bundled cross-language anti-pattern rules. Pass ` + "`detector: \"<name>\"`" + ` for one of: ` + "`error-not-wrapped`" + ` (Go), ` + "`sql-string-concat`" + ` (Go/Python/JS/TS/Ruby), ` + "`weak-crypto`" + ` (Go/Python), ` + "`panic-in-library`" + ` (Go), ` + "`goroutine-without-recover`" + ` (Go), ` + "`http-client-no-timeout`" + ` (Go), ` + "`hardcoded-secret`" + ` (Go/Python/JS/TS/Ruby), ` + "`empty-catch`" + ` (Java/JS/TS/Python), ` + "`java-string-equality`" + ` (Java), ` + "`python-mutable-default-arg`" + ` (Python). Each match returns the enclosing ` + "`symbol_id`" + ` so you can chain into ` + "`find_usages`" + ` / ` + "`apply_code_action`" + `. Test files excluded by default. |
| search_ast (raw pattern) | Tree-sitter S-expression queries. Pass ` + "`pattern: \"...\"`" + ` + ` + "`language: \"...\"`" + `. Capture nodes with ` + "`@name`" + `, anchor with ` + "`@match`" + `, predicates ` + "`(#eq? @x \"literal\")`" + ` / ` + "`(#match? @x \"regex\")`" + `. Example: ` + "`((call_expression function: (identifier) @fn) @match (#eq? @fn \"panic\"))`" + ` finds every direct ` + "`panic()`" + ` call. |
| search_ast (graph filters) | Combine the structural match with graph predicates ast-grep can't express: ` + "`path_prefix`" + ` / ` + "`repo`" + ` / ` + "`project`" + ` / ` + "`ref`" + ` / ` + "`min_fan_in_of_enclosing_func`" + `. The last narrows results to load-bearing code by dropping matches in functions with few callers. |

### Clone Detection
| Tool | What it gives you |
|------|-------------------|
| find_clones | Near-duplicate function/method clusters from the ` + "`similar_to`" + ` graph layer (MinHash + LSH over normalised tokens — catches copy-paste and renamed-variable clones). Every member is flagged ` + "`is_dead`" + `; pass ` + "`dead_only: true`" + ` for the "dead duplicates of live code" diagnostic. Filters: ` + "`min_similarity`" + `, ` + "`path_prefix`" + `, ` + "`repo`" + `, ` + "`limit`" + `. |

### Diagnostics & Code Actions
| Tool | What it gives you |
|------|-------------------|
| subscribe_diagnostics | Opt the session into push ` + "`notifications/diagnostics`" + ` from every running language server. Initial state replays as ` + "`initial_replay: true`" + `; thereafter only delta-changed files are pushed (sha256-suppressed). ` + "`min_severity`" + ` (1=error, 2=warning, 3=info, 4=hint) and ` + "`path_prefix`" + ` filters scope what reaches the session. Eliminates the poll-after-edit loop. |
| unsubscribe_diagnostics | Opt out of push notifications. Idempotent; fires automatically on session disconnect, so explicit calls are only needed when narrowing scope. |
| get_diagnostics | Latest stored ` + "`publishDiagnostics`" + ` for a file (the polling form). Pass ` + "`wait: true`" + ` + ` + "`timeout_ms`" + ` to block on the first publish — useful right after ` + "`didOpen`" + ` when no event has fired yet. |
| get_code_actions | LSP code actions for a file (and optional range). Returns the menu of fixes / refactors / source actions the language server offers. |
| apply_code_action | Apply a single CodeAction → WorkspaceEdit on disk. Atomic temp+rename; supports both legacy ` + "`changes`" + ` and modern ` + "`documentChanges`" + ` shapes; UTF-16 column math correctly maps LSP positions onto the byte offset in the source. |
| fix_all_in_file | One-shot ` + "`source.fixAll`" + ` over an entire file. Bundles every server-suggested fix in a single round-trip. |

### Code Quality
| Tool | What it gives you |
|------|-------------------|
| analyze | Unified graph analysis. Supported kinds: dead_code, hotspots, cycles, would_create_cycle, todos, blame, coverage, stale_code, ownership, coverage_gaps, coverage_summary, stale_flags, releases, cgo_users, wasm_users, orphan_tables, unreferenced_tables, channel_ops, goroutine_spawns, field_writers, annotation_users, config_readers, event_emitters, error_surface, external_calls, routes, models, components, k8s_resources, images, kustomize, cross_repo, dbt_models |
| analyze kind=dead_code | Symbols with zero incoming edges (excludes entry points, tests, exports) |
| analyze kind=hotspots | Over-coupled symbols ranked by fan-in, fan-out, and community crossings |
| analyze kind=cycles | Tarjan's SCC with severity classification |
| analyze kind=would_create_cycle | Pre-flight check before adding a new dependency |
| analyze kind=todos | KindTodo nodes; filter by tag/assignee/ticket |
| analyze kind=blame | Stamps meta.last_authored on every blame-eligible node |
| analyze kind=coverage | Stamps meta.coverage_pct on executable symbols from cover.out |
| analyze kind=stale_code | Symbols whose last-author timestamp is older than ` + "`older_than`" + ` days |
| analyze kind=ownership | Per-author rollup with symbol/file counts and oldest/newest TS |
| analyze kind=coverage_gaps | Symbols inside [min_pct, max_pct) — undertested code |
| analyze kind=coverage_summary | Per-directory coverage rollup (avg, covered, partial, uncovered) |
| analyze kind=stale_flags | Feature flags whose every toggling caller is older than ` + "`older_than`" + ` days |
| analyze kind=releases | Stamps meta.added_in on file nodes from git tags |
| analyze kind=cgo_users / wasm_users | Files that import C / use #[wasm_bindgen] |
| analyze kind=orphan_tables | Tables queried (EdgeQueries) but missing a migration (EdgeProvides) |
| analyze kind=unreferenced_tables | Tables provided by a migration but with zero EdgeQueries |
| analyze kind=channel_ops | Channels grouped by EdgeSends / EdgeRecvs — producer/consumer mismatches |
| analyze kind=goroutine_spawns | EdgeSpawns grouped by spawned target + mode (goroutine/async/promise) |
| analyze kind=field_writers | Mutability hotspots — fields ranked by EdgeWrites; pass ` + "`id`" + ` for one field |
| analyze kind=annotation_users | EdgeAnnotated rollup; pass ` + "`id`" + ` or ` + "`name`" + ` to scope (e.g. @Deprecated) |
| analyze kind=config_readers | config_key nodes grouped by EdgeReadsConfig; ` + "`name`" + ` filter |
| analyze kind=event_emitters | Event/log/metric emit sites grouped by EdgeEmits; ` + "`level`" + `, ` + "`name`" + ` filters |
| analyze kind=pubsub | Event pub/sub topics with publishers (EdgeEmits) + subscribers (EdgeListensOn) — NATS / Kafka / RabbitMQ / Redis / EventEmitter / Socket.IO; ` + "`transport`" + ` / ` + "`name`" + ` / ` + "`role`" + ` filters |
| analyze kind=error_surface | Function/method nodes with their EdgeThrows targets — refactor blast radius |
| analyze kind=external_calls | Stdlib / module-cache attribution — KindModule rollup of call/symbol counts; pass ` + "`id`" + ` for per-symbol detail, ` + "`module_kind`" + ` to filter stdlib vs module_cache |
| analyze kind=routes | Handler↔route pairs from the EdgeHandlesRoute layer (HTTP/gRPC/WS/GraphQL/topic); ` + "`method`" + ` / ` + "`path`" + ` / ` + "`type`" + ` filters |
| analyze kind=models | Model→table edges from EdgeModelsTable across gorm / SQLAlchemy / Django / ActiveRecord / JPA / TypeORM / Ecto; ` + "`orm`" + ` / ` + "`table`" + ` / ` + "`model`" + ` filters |
| analyze kind=components | Parent↔child fan-in/out from EdgeRendersChild (JSX/TSX + Phoenix HEEx); pass ` + "`id`" + ` for per-component child list |
| analyze kind=k8s_resources | KindResource fan-out (depends_on / configures / mounts / exposes / uses_env); ` + "`k8s_kind`" + ` / ` + "`namespace`" + ` / ` + "`name`" + ` filters |
| analyze kind=images | Container images (Dockerfile FROM target or K8s ` + "`container.image`" + `) with consumer count; ` + "`role`" + ` (base/stage) / ` + "`ref`" + ` / ` + "`tag`" + ` filters |
| analyze kind=kustomize | KindKustomization overlay tree with base / resource fan-out; ` + "`dir`" + ` filter |
| analyze kind=cross_repo | Repo-boundary-crossing calls / implements / extends edges grouped by (source repo → target repo, relation); ` + "`repo`" + ` / ` + "`base_kind`" + ` / ` + "`path_prefix`" + ` filters |
| analyze kind=dbt_models | dbt / SQLMesh models, seeds, snapshots, sources (KindTable) with column count + EdgeDependsOn lineage fan-in/out; ` + "`framework`" + ` / ` + "`type`" + ` / ` + "`materialized`" + ` / ` + "`name`" + ` filters |
| index_health | Health score, parse failures, stale files, language coverage |
| get_symbol_history | Symbols modified this session with counts; flags churning (3+ edits) |
| gortex enrich blame\|coverage\|releases\|all (CLI) | Bulk-stamp the graph with the metadata that stale_*/coverage_*/ownership/releases analyzers need |

### Code Generation
| Tool | What it gives you |
|------|-------------------|
| scaffold | Generates code, registration wiring, and test stubs from an example symbol |
| batch_edit | Applies multiple edits in dependency order, re-indexes between steps |
| diff_context | Git diff enriched with callers, callees, community, processes, per-file risk |

### API Contracts
| Tool | What it gives you |
|------|-------------------|
| contracts | API contracts: action=list (default) lists detected contracts; action=check matches providers/consumers and reports orphans across repos. Scope either action with ` + "`repo`" + `, ` + "`project`" + `, or ` + "`ref`" + ` |

### Config Hygiene
| Tool | What it gives you |
|------|-------------------|
| audit_agent_config | Graph-validates backticked symbols in CLAUDE.md / AGENTS.md / ` + "`.cursor/rules`" + ` / Copilot / Windsurf / Antigravity configs — flags stale refs, dead paths, bloat |

### Agent Learning
| Tool | What it gives you |
|------|-------------------|
| feedback (action=record) | Report which symbols from ` + "`smart_context`" + ` were useful / not_needed / missing after a task — improves future bundles |
| feedback (action=query) | Aggregated stats: most useful, most missed, context accuracy over time |

### Multi-Repo
| Tool | What it gives you |
|------|-------------------|
| index_repository | Index a repository path into the graph |
| track_repository | Add a repo to the workspace, index immediately, persist to config |
| untrack_repository | Remove a repo, evict its nodes/edges, persist to config |
| get_active_project | Current project name and member repository list |
| set_active_project | Switch project scope — re-scopes all subsequent queries |

## Graph Schema

**Node kinds:**
- Code structure: file, package, function, method, type, interface, field, variable, constant, import, contract, param, closure, enum_member, generic_param
- Coverage extensions: module (ecosystem deps), table / column (db schema), config_key (env/viper/cli), flag (feature flags), event (logs/metrics/spans), migration, fixture (test data), todo (TODO/FIXME comments), team (CODEOWNERS), license, release (tag boundaries)

**Edge kinds:**
- Calls / structure: calls, imports, defines, implements, extends, references, member_of, instantiates, provides, consumes, composes, aliases, typed_as, returns, captures, param_of
- Concurrency: spawns (goroutine/async/promise), sends / recvs (channels)
- Mutation: reads / writes (fields), reads_config / writes_config
- Dataflow (CPG-lite, ` + "`flow_between`" + ` / ` + "`taint_paths`" + `): value_flow (intra-procedural assignment / return / range), arg_of (caller arg → callee param), returns_to (callee → assignment LHS)
- Metadata: annotated (decorators), emits (events + pub/sub publish), listens_on (pub/sub subscribe), throws (errors), queries (SQL), reads_col / writes_col, toggles_flag, depends_on_module, matches (fixtures), generated_by, tests (test → tested symbol), covered_by, owns (CODEOWNERS), authored, licensed_as
- Similarity: similar_to (function/method near-duplicate — MinHash + LSH clone detection, ` + "`find_clones`" + `)
- Cross-repo: cross_repo_calls / cross_repo_implements / cross_repo_extends (parallel edges materialised when a calls/implements/extends edge crosses a repo boundary, ` + "`analyze kind=cross_repo`" + `)
`

const commandExplore = `# Exploring Codebases with Gortex

## Workflow

` + "```" + `
1. graph_stats                                  -> Confirm index, get node/edge counts
2. smart_context({task: "<what you want to understand>"}) -> One-call exploration bundle (start here)
3. get_communities                              -> See functional clusters (architecture overview)
4. search_symbols({query: "<concept>"})         -> Find symbols related to a concept
5. get_processes                                -> Discover execution flows
6. get_processes({id: "<process-id>"})          -> Trace a specific flow step by step
7. get_file_summary({path: "<file>"})           -> Symbols + imports for one file
8. get_editing_context({path: "<file>"})        -> Deep dive on a file (callers + callees)
9. export_context({...})                        -> Share findings as markdown/JSON (PRs, Slack, docs)
` + "```" + `

## Checklist

- Call graph_stats to confirm Gortex is running
- Call smart_context first — one call replaces 5-10 exploration calls
- Call get_communities for architecture overview when smart_context is not enough
- Call search_symbols for the concept you want to understand
- Call get_processes to discover execution flows
- Call get_processes with id on relevant flows for step-by-step traces
- Call get_editing_context on key files for full symbol context
- Call export_context to hand a findings packet outside the session
- Read source files only for implementation details you actually need to edit
`

const commandDebug = `# Debugging with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "<error or suspect>"})          -> Find related symbols
2. get_callers({id: "<suspect>"})                         -> Who calls it?
3. get_call_chain({id: "<suspect>"})                      -> What does it call?
4. get_editing_context({path: "<file>"})                  -> Full file context
5. get_processes({id: "<process>"})                       -> Trace execution flow
6. get_symbol_history                                     -> Symbols churning this session (regression hotspot)
7. explain_change_impact({ids: "<fix target>"})           -> Who else will feel the fix
` + "```" + `

## Debugging Patterns

| Symptom                 | Gortex Approach |
| ----------------------- | --------------- |
| Error message           | search_symbols for error-related names -> get_callers on throw sites; analyze kind=error_surface to map who throws what |
| Wrong return value      | get_call_chain on the function -> trace callees for data flow; flow_between({source_id, sink_id}) when you suspect the wrong value flows through helpers |
| Trace bad value to its origin | flow_between({source_id: producer, sink_id: consumer}) — ranked dataflow paths over value_flow / arg_of / returns_to. Faster than reading source for "where did this value come from?" |
| Find every taint into a sink | taint_paths({source_pattern: "name:Source", sink_pattern: "name:Sink"}) — every flow from any matching source to any matching sink (functions auto-expand to their params on the sink side) |
| Intermittent failure    | get_editing_context -> look for external calls, async deps; analyze kind=goroutine_spawns to find unowned background work |
| Channel deadlock        | analyze kind=channel_ops -> channels with sends but no receivers (or vice versa) |
| Performance issue       | find_usages -> find symbols with many callers (hot paths) |
| Recent regression       | detect_changes -> see what your changes affect. get_symbol_history flags symbols edited 3+ times this session |
| Flaky test              | get_untested_symbols near the suspect -> find coverage gaps the flake may hide |
| Stale index suspect     | index_health -> parse failures and stale files can mask the real bug |
| Stale-flag suspect      | analyze kind=stale_flags -> flags with every caller untouched for ` + "`older_than`" + ` days are dead-rollout candidates |
| Config drift            | analyze kind=config_readers -> who reads this env/viper key? Surfaces forgotten readers |
| Event/log volume spike  | analyze kind=event_emitters with level=error -> find every site that logs an error |
| Mutation race suspicion | analyze kind=field_writers id=<field> -> every function that writes the contended field |
| Annotation drift        | analyze kind=annotation_users name=Deprecated -> every site still using a deprecated API |
| Env var read/write mismatch | find_usages on cfg::env::<NAME> -> Resources/Dockerfile stages declaring it (EdgeUsesEnv) plus code-side os.Getenv consumers via the shared config_key node |
| K8s manifest blast radius | analyze kind=k8s_resources k8s_kind=ConfigMap -> orphan ConfigMaps. find_usages on a ConfigMap Resource ID surfaces every workload that envFroms or mounts it |
| Container image audit   | analyze kind=images role=base -> every external image and how many Dockerfile stages / K8s Resources pull it. Filter by tag=latest to find the unpinned ones |
`

const commandImpact = `# Impact Analysis with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "X"})                                     -> Find the symbol ID
2. explain_change_impact({ids: "<id1>, <id2>"})                     -> Risk-tiered blast radius
3. get_dependents({id: "<symbol-id>", depth: 3})                    -> Detailed dependent tree
4. analyze({kind: "ownership", path_prefix: "<dir>/"})              -> Who owns this area (review pinging)
5. verify_change({id: "<id>", new_signature: "..."})                -> Check callers + interface implementors for signature-level breaks
6. contracts({action: "check"})                                     -> Cross-repo API breakage (HTTP/gRPC/GraphQL/topics)
7. analyze({kind: "would_create_cycle", from: "<a>", to: "<b>"})    -> Before adding a new dep
8. analyze({kind: "error_surface", path_prefix: "<dir>/"})          -> What error surface does this area produce — widening risk
9. get_test_targets({ids: ["<id1>", "<id2>"]})                      -> Tests to re-run (includes cross-repo)
10. analyze({kind: "coverage_gaps", path_prefix: "<dir>/"})         -> Undertested code in the change area — extra-risky refactor zones
11. check_guards({ids: ["<id1>"]})                                  -> Project guard rules from .gortex.yaml
12. flow_between({source_id, sink_id})                              -> Ranked dataflow paths between two symbols — catches consumers reached through helpers that get_dependents misses
13. taint_paths({source_pattern, sink_pattern})                     -> Pattern-driven dataflow sweep — every flow from a matching source to a matching sink
14. detect_changes({scope: "staged"})                               -> Pre-commit scope check
15. diff_context({scope: "staged"})                                 -> Graph-enriched diff for review
` + "```" + `

## Understanding Output

| Depth | Risk Level       | Meaning                  |
| ----- | ---------------- | ------------------------ |
| d=1   | **WILL BREAK**   | Direct callers/importers |
| d=2   | LIKELY AFFECTED  | Indirect dependencies    |
| d=3   | MAY NEED TESTING | Transitive effects       |

## Checklist

- search_symbols to find exact symbol IDs
- explain_change_impact with all symbols you plan to change
- Review risk level (LOW/MEDIUM/HIGH/CRITICAL)
- Check by_depth: d=1 items WILL BREAK
- Note affected_processes and affected_communities
- analyze kind=ownership path_prefix=<dir>/ — who should review (pinging policy without CODEOWNERS)
- verify_change for every signature change (catches contract violations across repos)
- contracts action=check when changing HTTP routes, gRPC methods, topics, env contracts
- analyze kind=error_surface path_prefix=<dir>/ — confirm the change does not widen the error surface
- analyze kind=coverage_gaps path_prefix=<dir>/ — areas with weak coverage need extra scrutiny
- check_guards so team conventions from .gortex.yaml block bad changes early
- get_test_targets to see which test files need re-running
- Before commit: detect_changes to verify scope, diff_context for graph-enriched review
`

const commandRefactor = `# Refactoring with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "X"})                                     -> Find the symbol ID
2. explain_change_impact({ids: "<id>"})                             -> Map blast radius
3. analyze({kind: "ownership", path_prefix: "<dir>/"})              -> Who should review (pinging policy)
4. analyze({kind: "coverage_gaps", path_prefix: "<dir>/"})          -> Where is the refactor risky (poor coverage)
5. verify_change({id: "<id>", new_signature: "..."})                -> Catch contract violations in callers + implementors
6. get_editing_context({path: "<file>"})                            -> See all symbols and relationships
7. find_usages({id: "<id>"})                                        -> Every reference to change
8. get_edit_plan({ids: ["<id1>", "<id2>"]})                         -> Dependency-ordered file list
9. batch_edit({edits: [...]})                                       -> Apply edits in order, re-indexing between steps
10. check_guards({ids: [...]})                                      -> Post-edit: team conventions from .gortex.yaml
11. get_test_targets({ids: [...]})                                  -> Tests to re-run (cross-repo aware)
12. detect_changes({scope: "all"})                                  -> Verify scope; diff_context for review
` + "```" + `

## Rename Symbol

- search_symbols to find the symbol ID
- explain_change_impact to assess blast radius
- verify_change before signature-changing renames — fails fast on interface breaks
- rename_symbol({id: "<id>", new_name: "<name>"}) — generates edits for definition + all references
- Review the generated edits, apply via batch_edit or edit_symbol (no Read→Edit roundtrip)
- check_guards, then detect_changes to verify only expected files changed

## Extract Module

- get_editing_context on the source file — see all symbols
- get_dependents on symbols to extract — find external callers
- explain_change_impact on symbols being moved
- analyze({kind: "would_create_cycle", from: "<new module>", to: "<old module>"}) before wiring imports
- suggest_pattern + scaffold from a comparable existing module — generates code, wiring, test stubs
- Extract code, update imports (find_import_path for correct paths)
- get_edit_plan + batch_edit for dependency-ordered atomic application
- check_guards, detect_changes to verify affected scope

## Split Function/Service

- get_call_chain on the function — understand all callees
- Group callees by responsibility
- get_callers to map all call sites that need updating
- find_implementations when splitting along an interface
- explain_change_impact for full blast radius
- Create new functions/services (scaffold from a similar example)
- Update callers (find_usages for precise locations, batch_edit to apply in order)
- check_guards, detect_changes to verify affected scope

## API Contract Changes

- Before changing an HTTP route, gRPC method, topic, env, or OpenAPI contract: contracts({action: "check"}) to find cross-repo consumers
- verify_change on the provider signature
- Coordinate consumer-side edits in the same batch_edit when repos are tracked together
`

const commandSafeEdit = `# Safe Edit with Gortex (speculative-execution path)

Use this **before** you touch ` + "`edit_file`" + ` / ` + "`edit_symbol`" + ` / ` + "`batch_edit`" + ` / ` + "`Edit`" + ` / ` + "`Write`" + `. It runs the edit against the in-memory shadow graph first, reports broken callers + broken interface implementors + blast-radius rollup + suggested tests, and only promotes to disk once those signals are clean.

## Workflow (do not skip steps)

` + "```" + `
1. smart_context({task: "<what you intend to change>"})              -> Working-set bundle
2. surface_memories({task, symbol_ids})                              -> Cross-session invariants on the same symbols
3. get_editing_context({path: "<file>"})                             -> Pre-edit signature + caller map
4. preview_edit({edit: <LSP WorkspaceEdit>})                         -> Single-shot speculative apply on the shadow graph
   -> Inspect: touched / added / removed / renamed / broken_callers / broken_implementors / impact / suggested_tests
5. simulate_chain({steps: [{edit: <e1>}, {edit: <e2>}, ...],         -> Multi-step ordered simulation
                   inherit_overlay: true, stop_on_error: true,
                   keep: false})
   -> Use when the change is more than one WorkspaceEdit (cross-file refactor, signature + every caller, etc.)
6. If broken_callers or broken_implementors is non-empty            -> Fix the chain first, re-run preview_edit / simulate_chain
7. check_guards({ids: [<changed-ids>]})                              -> Team conventions from .gortex.yaml
8. verify_change({id: <id>, new_signature: "..."})                   -> Final signature check before disk write
9. batch_edit({edits: [...]})                                        -> Apply the same edits on disk in dependency order
10. detect_changes({scope: "all"}) + diff_context({scope: "all"})    -> Confirm the on-disk diff matches the simulated one
11. get_test_targets({ids: [<changed-ids>]})                         -> Tests to re-run (cross-repo aware)
` + "```" + `

## When to use ` + "`preview_edit`" + ` vs ` + "`simulate_chain`" + `

| Situation                                              | Tool             | Why |
| ------------------------------------------------------ | ---------------- | --- |
| One self-contained ` + "`WorkspaceEdit`" + ` (e.g. one rename)        | preview_edit     | Single round-trip, simplest. |
| Several dependent edits that must be applied in order  | simulate_chain   | First-failing-step semantics; ` + "`inherit_overlay`" + ` carries state forward. |
| You want the simulated state to become the live overlay so subsequent queries see it | simulate_chain with ` + "`keep: true`" + ` | Promotes the final shadow state into a real overlay session — graph queries see post-edit reality without writing disk. |
| Optional LSP diagnostics on the simulated content      | Either, with ` + "`with_diagnostics: true`" + ` | Drives ` + "`didChange(simulated) → wait → didChange(original)`" + ` round-trip; concurrent sessions never observe simulated state as authoritative. |

## Reading the speculative report

- **broken_callers** — callers whose call sites won't type-check against the new signature. Fix or update every one before disk.
- **broken_implementors** — interface implementors that no longer satisfy the interface contract. Either widen the contract, update implementors, or revert.
- **impact** — ` + "`analysis.AnalyzeImpact`" + ` blast-radius rollup. The same risk tiers ` + "`/gortex-impact`" + ` reports.
- **suggested_tests** — what to feed to ` + "`get_test_targets`" + ` after the apply.
- **added / removed / renamed** — non-trivial-signature unambiguous-candidate heuristic; bare ` + "`func ()`" + ` voids reject as ambiguous, so you may see fewer "renamed" entries than you expect — that is correct behaviour.

## Checklist

- ` + "`smart_context`" + ` before reading any file
- ` + "`surface_memories`" + ` on the same working set — pick up cross-session decisions / gotchas
- ` + "`preview_edit`" + ` (single edit) or ` + "`simulate_chain`" + ` (ordered chain) **before** any on-disk write
- Treat ` + "`broken_callers`" + ` / ` + "`broken_implementors`" + ` as blockers, not warnings
- ` + "`check_guards`" + ` + ` + "`verify_change`" + ` after the simulated diff is clean, before ` + "`batch_edit`" + `
- ` + "`batch_edit`" + ` (dependency-ordered, atomic re-index between steps) over manual sequencing
- ` + "`detect_changes`" + ` + ` + "`diff_context`" + ` to confirm the on-disk diff matches the simulation
- ` + "`get_test_targets`" + ` to run the right tests, cross-repo aware
- If the edit is durable knowledge ("X must hold the lock", "this package never uses gob"), ` + "`store_memory`" + ` so the next agent inherits the lesson
`

const commandFixAll = `# Fix LSP Diagnostics with Gortex (LSP code-actions path)

Use this when the user wants errors / warnings cleared — for one symbol, one file, or the whole project. Bridges Gortex to the 18-language LSP coverage (gopls / tsserver / pyright / rust-analyzer / clangd / jdtls / kotlin-language-server / omnisharp / ruby-lsp / phpactor / lua-language-server / sourcekit-lsp / haskell-language-server / elixir-ls / ocamllsp / zls / terraform-ls / yaml-language-server / json-language-server / bash-language-server). No manual ` + "`Edit`" + ` of error messages.

## Workflow (do not skip steps)

` + "```" + `
1. subscribe_diagnostics({min_severity: 1, path_prefix: "<dir>/"})   -> Push notifications, replays initial state once
2. get_diagnostics({path: "<file>", wait: true, timeout_ms: 2000})   -> Poll form when you need a synchronous snapshot
3. get_code_actions({path: "<file>", range: {...}})                  -> Menu of fixes / refactors / source actions
4. For each chosen action:
     preview_edit({edit: <action.workspaceEdit>})                    -> Speculative apply on the shadow graph
     apply_code_action({path, action_id})                            -> Atomic on-disk apply (UTF-16 column math)
5. fix_all_in_file({path: "<file>"})                                 -> One-shot source.fixAll over an entire file
6. get_diagnostics({path: "<file>"})                                 -> Confirm the file is now clean
7. check_guards({ids: [<changed-ids>]}) + get_test_targets({ids})    -> Post-fix guardrails
8. unsubscribe_diagnostics                                           -> Only when narrowing scope; auto-fires on disconnect
` + "```" + `

## Diagnostic-fix patterns

| Situation                                              | Tool ordering | Notes |
| ------------------------------------------------------ | ------------- | ----- |
| One specific error                                     | ` + "`get_code_actions`" + ` at the diagnostic range -> pick one -> ` + "`apply_code_action`" + ` | Smallest possible edit. |
| Every error in a file                                  | ` + "`fix_all_in_file`" + ` | Bundles every server-suggested fix in a single round-trip. |
| Refactor offered as a code action (` + "`refactor.*`" + `)       | ` + "`get_code_actions`" + ` -> pick by ` + "`kind`" + ` (e.g. ` + "`refactor.extract`" + `) -> ` + "`preview_edit`" + ` first | See ` + "`/gortex-extract-function`" + `. |
| Source action (` + "`source.organizeImports`" + `, ` + "`source.fixAll`" + `) | ` + "`get_code_actions`" + ` with the right ` + "`only`" + ` kind, or ` + "`fix_all_in_file`" + ` | Whole-file scope. |
| Watching diagnostics during a long edit session         | ` + "`subscribe_diagnostics`" + ` with ` + "`min_severity`" + ` + ` + "`path_prefix`" + ` filters | SHA-suppressed delta payloads; only changed files reach you. |

## Reading the diagnostic stream

- ` + "`initial_replay: true`" + ` on the first push — that's the synchronous snapshot of the current LSP state, not a new event.
- Every subsequent push carries only files whose ` + "`publishDiagnostics`" + ` SHA changed.
- ` + "`min_severity`" + ` 1 = error, 2 = warning, 3 = info, 4 = hint. Default to 1 unless you specifically need warnings.
- ` + "`path_prefix`" + ` is your scope filter — pin it to the area you're working on so the rest of the project doesn't drown the stream.

## When LSP cannot fix it

If ` + "`get_code_actions`" + ` returns an empty menu, the server doesn't know how to fix this diagnostic. Fall back to:
1. ` + "`search_symbols`" + ` for the symbol the diagnostic references
2. ` + "`get_editing_context`" + ` on the file
3. ` + "`/gortex-safe-edit`" + ` to author the edit by hand, with ` + "`preview_edit`" + ` to verify

## Checklist

- ` + "`subscribe_diagnostics`" + ` once per session, with ` + "`min_severity`" + ` + ` + "`path_prefix`" + ` scoped to the area you're touching
- ` + "`get_code_actions`" + ` is the menu — never invent fixes
- ` + "`preview_edit`" + ` (or ` + "`apply_code_action`" + ` directly when the action is small and well-known) before any disk write
- ` + "`fix_all_in_file`" + ` for whole-file source.fixAll — one round-trip beats N targeted fixes
- Re-run ` + "`get_diagnostics`" + ` after the fix; assume nothing
- ` + "`check_guards`" + ` and ` + "`get_test_targets`" + ` after the diagnostics are clean — fixing the LSP error is not the same as fixing the test
`

const commandExtractFunction = `# Extract Function with Gortex (LSP refactor path)

Use this when the user wants to extract code into a new function / method / variable / constant via the **language server's refactor actions**, not by hand-editing braces. Maps onto LSP ` + "`refactor.extract.*`" + ` code actions; works wherever the underlying server supports them (gopls / tsserver / pyright / rust-analyzer / jdtls / kotlin-language-server / omnisharp / and others).

## Workflow (do not skip steps)

` + "```" + `
1. get_editing_context({path: "<file>"})                                -> See enclosing symbol, callers, callees
2. (optional) find_usages({id: "<enclosing-fn>"})                       -> Who calls the function you are about to split
3. get_code_actions({path: "<file>", range: {start, end},                -> Menu filtered to refactor.extract.* actions
                     only: ["refactor.extract.function",
                            "refactor.extract.method",
                            "refactor.extract.variable",
                            "refactor.extract.constant"]})
4. preview_edit({edit: <chosen action.workspaceEdit>})                  -> Speculative apply
   -> Inspect: touched / added / renamed / broken_callers / broken_implementors / impact
5. If the extraction crosses files (e.g. moving the helper to a sibling file):
     simulate_chain({steps: [...]})                                     -> Ordered chain with broken-caller carry-over
6. apply_code_action({path: "<file>", action_id: "<id>"})               -> Atomic temp+rename, UTF-16 column math
7. check_guards({ids: [<changed-ids>]}) + verify_change                  -> Convention + signature gate
8. get_test_targets({ids: [<old-id>, <new-id>]})                        -> Run tests for both the caller and the extract
9. detect_changes + diff_context                                        -> Final scope check
` + "```" + `

## Choosing the right extract action

| User intent                                | Code-action kind                  | Notes |
| ------------------------------------------ | --------------------------------- | ----- |
| Pull a block of statements into a helper   | ` + "`refactor.extract.function`" + `             | Most servers infer params + return type from the selection. |
| Move statements into a method on a type    | ` + "`refactor.extract.method`" + `               | Receiver / this binding chosen by the server. |
| Extract a sub-expression to a local        | ` + "`refactor.extract.variable`" + `             | Best for naming repeated sub-expressions. |
| Extract a literal to a package-level const | ` + "`refactor.extract.constant`" + `             | gopls and tsserver expose this; rust-analyzer uses ` + "`refactor.extract.module`" + ` for similar moves. |

If you don't know which kinds the server offers, call ` + "`get_code_actions`" + ` **without** ` + "`only`" + ` and inspect the menu first.

## Range selection

LSP positions are line + UTF-16 character offsets. The ` + "`apply_code_action`" + ` mapper handles this correctly, but when you compose the range yourself:
1. Get the file body via ` + "`get_symbol_source`" + ` (compress_bodies:false) or ` + "`get_file_summary`" + `
2. Use the symbol's line numbers as anchors; expand to the precise statement boundaries
3. Pass ` + "`range: {start: {line, character}, end: {line, character}}`" + ` to ` + "`get_code_actions`" + `

## When the server has no extract action

Some servers (yaml-language-server, json-language-server, bash-language-server) don't ship refactor actions. Fall back to ` + "`/gortex-safe-edit`" + ` and author the extract by hand — but still ` + "`preview_edit`" + ` first.

## Checklist

- ` + "`get_editing_context`" + ` to understand the enclosing symbol before selecting a range
- ` + "`get_code_actions`" + ` with ` + "`only: refactor.extract.*`" + ` — pick from the menu, don't invent
- ` + "`preview_edit`" + ` before ` + "`apply_code_action`" + ` (broken callers / blast radius)
- ` + "`simulate_chain`" + ` if the extract moves the helper to a new file with follow-on rename of call sites
- ` + "`check_guards`" + `, ` + "`verify_change`" + `, ` + "`get_test_targets`" + ` after the apply
- ` + "`get_symbol`" + ` on the new symbol to confirm Gortex indexed it under the expected ID
`

const commandRename = `# Rename Symbol with Gortex (cross-file coordinated rename)

Use this when the user wants to rename a function / method / type / variable / package and have every reference (definition, callers, tests, cross-repo consumers, doc-comments where applicable) updated atomically. Picks the right tool for the job — graph-coordinated ` + "`rename_symbol`" + ` for Gortex-known symbols, LSP ` + "`textDocument/rename`" + ` (surfaced as a ` + "`refactor.rename`" + ` code action where supported) for server-driven cases.

## Workflow (do not skip steps)

` + "```" + `
1. search_symbols({query: "<old name>"})                              -> Resolve the symbol ID(s)
2. winnow_symbols({...})                                              -> Disambiguate if multiple matches survive
3. explain_change_impact({ids: "<id>"})                               -> Blast radius (callers, implementors, processes)
4. find_usages({id: "<id>"})                                          -> Every reference (BM25-free; zero false positives)
5. verify_change({id: "<id>", new_signature: "<old sig with new name>"}) -> Catches interface breaks early
6. rename_symbol({id: "<id>", new_name: "<new>"})                     -> Generates dependency-ordered WorkspaceEdit (Gortex path)
   OR get_code_actions({path, range, only: ["refactor.rename"]})      -> LSP-driven path (when you want server-side rename semantics)
7. preview_edit({edit: <generated WorkspaceEdit>})                    -> Speculative apply; check broken_callers / broken_implementors
8. batch_edit({edits: [...]})                                         -> Atomic on-disk apply, re-index between steps
   OR apply_code_action({...})                                        -> If you went the LSP route
9. check_guards({ids: [<old, new>]})                                  -> Naming conventions, banned terms from .gortex.yaml
10. get_test_targets({ids: [<new-id>]})                               -> Tests to re-run; cross-repo aware
11. detect_changes + diff_context                                     -> Confirm no stray reference survived
` + "```" + `

## Choosing the right rename tool

| Situation                                                  | Tool                          | Why |
| ---------------------------------------------------------- | ----------------------------- | --- |
| Symbol is in the graph and rename is mechanical             | ` + "`rename_symbol`" + `                  | Generates the WorkspaceEdit from edges; deterministic, fast, no LSP round-trip. |
| Symbol semantics depend on the server (TS shadowed locals, Java generics, Rust trait resolution) | ` + "`get_code_actions`" + ` + ` + "`apply_code_action`" + ` with ` + "`refactor.rename`" + ` | Server resolves identifier scoping the parser can't fully replicate. |
| Cross-repo rename (consumer repos tracked together)         | ` + "`rename_symbol`" + ` + ` + "`contracts({action: check})`" + ` | Graph carries cross-repo edges; ` + "`contracts`" + ` flags HTTP / gRPC / topic-side breakage. |
| Rename of a published API surface                           | ` + "`rename_symbol`" + ` **plus** ` + "`contracts({action: check})`" + ` and a deprecation shim | Pair with ` + "`/gortex-cross-repo-usage`" + ` to find consumer repos. |

## Cross-repo renames

` + "`rename_symbol`" + ` covers every repo the graph knows about — track consumer repos first (` + "`track_repository`" + `) and switch ` + "`set_active_project`" + ` to the multi-repo scope, then run the rename. ` + "`/gortex-cross-repo-usage`" + ` is the partition view that confirms every consumer repo was touched.

## Renaming through interfaces

If the renamed symbol is part of an interface contract:
1. ` + "`find_implementations`" + ` on the interface — every implementor must be renamed in the same chain
2. ` + "`verify_change`" + ` against the interface's signature with the new name
3. ` + "`simulate_chain`" + ` with one step per implementor file so ` + "`broken_implementors`" + ` carries forward correctly
4. Apply via ` + "`batch_edit`" + ` (or ` + "`apply_code_action`" + ` if you went the LSP route per-implementor)

## Checklist

- ` + "`search_symbols`" + ` + ` + "`winnow_symbols`" + ` until exactly one ID resolves to the symbol you mean
- ` + "`explain_change_impact`" + ` + ` + "`find_usages`" + ` before authoring any edit
- ` + "`verify_change`" + ` on the new signature — catches interface breaks before anything is written
- Pick ` + "`rename_symbol`" + ` (graph path) or LSP ` + "`refactor.rename`" + ` (server path); never hand-author cross-file renames
- ` + "`preview_edit`" + ` / ` + "`simulate_chain`" + ` on the generated edits before disk
- ` + "`batch_edit`" + ` is dependency-ordered + re-indexes between steps — atomic in the sense that matters for the graph
- ` + "`check_guards`" + `, ` + "`get_test_targets`" + `, ` + "`diff_context`" + ` after the apply
`

const commandCrossRepoUsage = `# Cross-Repo Usage with Gortex

Use this when the user needs to see who uses a symbol **across every consumer repo**, not just the one they happen to be in. Wraps ` + "`find_usages`" + ` + the cross-repo edge layer (` + "`cross_repo_calls`" + ` / ` + "`cross_repo_implements`" + ` / ` + "`cross_repo_extends`" + `) so the answer is partitioned by repo and includes contract-level consumers (HTTP / gRPC / topics) that wouldn't show up as a direct call edge.

## Workflow (do not skip steps)

` + "```" + `
1. get_active_project                                                  -> See current scope (single repo or multi-repo project)
2. list_repos                                                          -> See which repos are tracked
3. For each consumer repo not yet tracked:
     track_repository({path: "<consumer repo path>"})                  -> Index immediately, persist to config
4. set_active_project({name: "<multi-repo project>"})                  -> Switch scope so subsequent queries span repos
5. search_symbols({query: "X"})                                        -> Resolve the symbol ID in the providing repo
6. find_usages({id: "<id>"})                                           -> All references; group by repo prefix
7. analyze({kind: "cross_repo",                                        -> Repo-boundary-crossing edges with relation
            base_kind: "calls|implements|extends",
            repo: "<providing repo>"})
8. contracts({action: "check"})                                        -> Contract-level consumers (HTTP routes, gRPC methods, topics, env, OpenAPI)
9. get_test_targets({ids: [<id>]})                                     -> Cross-repo tests that exercise this symbol
10. export_context({format: "markdown"})                               -> Per-repo report for PR / Slack / docs
` + "```" + `

## Partitioning ` + "`find_usages`" + ` by repo

The graph stores repo as a prefix on each node ID. Group results by the leading path segment so the user sees one section per consumer:

| Repo                      | References |
| ------------------------- | ---------- |
| my-org/api-gateway        | 7 sites    |
| my-org/billing            | 3 sites    |
| my-org/notifications      | 1 site     |

` + "`analyze kind=cross_repo`" + ` complements this — it reports the typed edges (calls / implements / extends) that physically cross the boundary, which is the count that matters for "is this safe to change without coordinating with consumer teams."

## Contract-level consumers

For published API surfaces, ` + "`find_usages`" + ` won't show consumers that go through the wire. ` + "`contracts({action: check})`" + ` catches:
- HTTP route consumers (client-side ` + "`fetch`" + ` / ` + "`http.Get`" + ` / generated SDK methods)
- gRPC method consumers (generated client stubs across repos)
- Pub/sub subscribers (NATS / Kafka / RabbitMQ / Redis / EventEmitter / Socket.IO)
- Env-var consumers (one process writes, another reads)
- OpenAPI / GraphQL schema consumers

## Onboarding a consumer repo

If a known consumer is missing from the tracked-repo list:
1. ` + "`track_repository`" + ` with its path — indexes immediately, persists to config
2. Re-run ` + "`find_usages`" + ` and ` + "`analyze kind=cross_repo`" + ` — the new edges materialise
3. ` + "`untrack_repository`" + ` only when you're done; leaving it tracked keeps subsequent cross-repo queries cheap

## Checklist

- ` + "`get_active_project`" + ` first — you may already be in multi-repo scope
- ` + "`track_repository`" + ` every consumer repo you care about; ` + "`find_usages`" + ` only sees what's indexed
- Partition the ` + "`find_usages`" + ` rows by repo prefix; the per-repo breakdown is the deliverable
- ` + "`analyze kind=cross_repo`" + ` for the typed-edge view (calls / implements / extends)
- ` + "`contracts({action: check})`" + ` for wire-level consumers that ` + "`find_usages`" + ` cannot see
- ` + "`get_test_targets`" + ` returns cross-repo tests too — run them, not just the local ones
- Hand the per-repo report to the user via ` + "`export_context`" + ` for PR descriptions / Slack
`

const commandDataflowTrace = `# Dataflow Trace with Gortex (CPG-lite)

Use this when the user asks where a value flows — through assignments, function args, returns, channels, event buses, or pub/sub topics. Built on the CPG-lite edge layer (` + "`value_flow`" + ` ∪ ` + "`arg_of`" + ` ∪ ` + "`returns_to`" + `) plus the pub/sub + channel + emit layers, so flows survive crossing function and process boundaries that a plain caller-graph walk would lose.

## Workflow (do not skip steps)

` + "```" + `
1. smart_context({task: "<the flow you want to understand>"})         -> Working-set bundle
2. search_symbols({query: "<source>"})                                 -> Resolve the producing symbol ID
3. search_symbols({query: "<sink>"})                                   -> Resolve the consuming symbol ID
4. flow_between({source_id: "<src>", sink_id: "<sink>",                -> Ranked paths over value_flow ∪ arg_of ∪ returns_to
                 max_depth: 8, max_paths: 10})
5. taint_paths({source_pattern: "<pattern>",                           -> Every flow from any matching source to any matching sink
                sink_pattern: "<pattern>",
                max_depth: 8})
6. analyze({kind: "channel_ops"})                                      -> Channel producer/consumer mismatch (Go)
7. analyze({kind: "pubsub",                                            -> Pub/sub topics with publishers + subscribers
            transport: "nats|kafka|rabbitmq|redis|eventemitter|socketio",
            name: "<topic>",
            role: "publish|subscribe"})
8. analyze({kind: "event_emitters", level: "error"})                   -> Log/metric/span emit sites
9. get_call_chain({id: "<sink>"}) + get_callers({id: "<source>"})      -> Caller-graph cross-check for any path flow_between missed
10. export_context({format: "markdown"})                               -> Hand the trace to a PR / Slack / doc
` + "```" + `

## Pattern syntax for ` + "`taint_paths`" + `

Each pattern is one or more clauses combined with AND:

| Clause             | Meaning                                          | Example                       |
| ------------------ | ------------------------------------------------ | ----------------------------- |
| ` + "`<bare token>`" + `   | Substring match on the symbol name               | ` + "`Decode`" + `                    |
| ` + "`exact:<name>`" + `   | Exact name match                                 | ` + "`exact:HandleRequest`" + `       |
| ` + "`path:<prefix>`" + `  | Path prefix on the file the symbol lives in     | ` + "`path:internal/http/`" + `       |
| ` + "`kind:<kind>`" + `    | Restrict to ` + "`function`" + ` / ` + "`method`" + ` / ` + "`field`" + ` / etc. | ` + "`kind:method`" + `               |

Functions auto-expand to their params on the sink side, so ` + "`taint_paths`" + ` with a function-shaped sink pattern reports flows into any of its parameters.

## Crossing process / transport boundaries

` + "`flow_between`" + ` walks intra-process edges. To follow a value across a wire:

| Boundary                          | Tool                                                 |
| --------------------------------- | ---------------------------------------------------- |
| Channel (Go)                       | ` + "`analyze kind=channel_ops`" + ` — find the matching ` + "`recv`" + ` site, then ` + "`flow_between`" + ` from there |
| Pub/sub topic                      | ` + "`analyze kind=pubsub name=<topic>`" + ` — get every subscriber; ` + "`flow_between`" + ` from each |
| HTTP / gRPC / GraphQL contract     | ` + "`contracts({action: check})`" + ` — match providers to consumers; the contract ID is your bridge node |
| Env var (one process writes, another reads) | ` + "`analyze kind=config_readers name=<KEY>`" + ` -> consumers; ` + "`find_usages`" + ` on ` + "`cfg::env::<KEY>`" + ` |
| Cross-repo call                    | ` + "`analyze kind=cross_repo base_kind=calls`" + ` — typed boundary-crossing edges |

## Reading a ranked path

Each row from ` + "`flow_between`" + ` is a sequence of edges with the kind annotated (` + "`value_flow`" + ` / ` + "`arg_of`" + ` / ` + "`returns_to`" + `). The rank reflects path length + edge confidence. Treat the top-3 paths as the load-bearing ones; the long tail tends to be incidental cross-references.

## Checklist

- ` + "`smart_context`" + ` first — pick up the working set before asking flow_between for a path
- Resolve **both** source and sink to symbol IDs before calling ` + "`flow_between`" + ` (path search needs anchors)
- Use ` + "`taint_paths`" + ` when "every flow from a kind of source to a kind of sink" matters more than one specific pair
- Cross process boundaries with the right analyzer (` + "`channel_ops`" + ` / ` + "`pubsub`" + ` / ` + "`config_readers`" + ` / ` + "`contracts`" + `) — ` + "`flow_between`" + ` alone won't follow a value across the wire
- Cross-check with ` + "`get_call_chain`" + ` / ` + "`get_callers`" + ` when ` + "`flow_between`" + ` returns zero paths for a flow you're sure exists — the producer or consumer may not yet be in the dataflow layer
- ` + "`export_context`" + ` to hand the trace to a PR / Slack / doc without losing the structure
`

const commandAddTest = `# Add Tests with Gortex (coverage-led)

Use this when the user wants tests added for under-covered code, untested symbols, or a specific regression. Walks the coverage analyzers first so the new test goes where it matters; uses ` + "`suggest_pattern`" + ` + ` + "`scaffold`" + ` to author the new test in the style of an existing one rather than from scratch.

## Workflow (do not skip steps)

` + "```" + `
1. gortex enrich coverage  (CLI)                                       -> Stamp meta.coverage_pct on executable symbols from cover.out
2. analyze({kind: "coverage_summary", path_prefix: "<dir>/"})          -> Per-directory rollup (avg / covered / partial / uncovered)
3. analyze({kind: "coverage_gaps",                                     -> Symbols inside [min_pct, max_pct) — the candidate list
            path_prefix: "<dir>/",
            min_pct: 0, max_pct: 50})
4. get_untested_symbols({path_prefix: "<dir>/"})                       -> Symbols with no covering test
5. For the chosen target symbol:
     get_editing_context({path: "<file>"})                             -> Signature, callers, callees
     get_callers({id: "<id>"})                                         -> How the symbol is invoked in real code (drives test inputs)
6. suggest_pattern({example_id: "<similar tested symbol>"})            -> Extracts the test-authoring pattern (source + registration + tests)
7. scaffold({kind: "test", from: "<example>", target: "<new test>"})   -> Generate test stub + wiring
8. preview_edit({edit: <generated WorkspaceEdit>}) -> apply / batch_edit -> Speculative apply, then on-disk
9. get_test_targets({ids: [<symbol-under-test>]})                      -> Confirms the new test file is discovered + maps to a run command
10. Run the test command; if green, re-check ` + "`analyze kind=coverage_gaps`" + ` to confirm the gap closed
11. check_guards + detect_changes + diff_context                       -> Standard post-edit gates
` + "```" + `

## Picking the right target

| Source of priority                  | Tool                                                        | Use when |
| ----------------------------------- | ----------------------------------------------------------- | -------- |
| User named the symbol               | ` + "`search_symbols`" + ` -> ` + "`get_untested_symbols`" + ` (confirm gap)            | Direct request. |
| Recent regression                   | ` + "`detect_changes`" + ` -> ` + "`analyze kind=coverage_gaps`" + ` on the change set   | "Add a test for what just broke." |
| Hot path with weak coverage         | ` + "`analyze kind=hotspots`" + ` -> filter by ` + "`coverage_pct`" + ` < 50           | High-impact gaps. |
| Whole directory                     | ` + "`analyze kind=coverage_summary`" + ` -> walk lowest-coverage subdirs   | "Cover this package." |
| Mutation hotspot without tests       | ` + "`analyze kind=field_writers`" + ` + ` + "`get_untested_symbols`" + `              | Race / state-mutation risk. |

## Authoring the test

` + "`suggest_pattern`" + ` returns the **shape** of a comparable test in the project (table-driven, parallel, golden-file, etc.) so the new test feels native. ` + "`scaffold`" + ` then materialises the file with the right imports, package, and registration. Hand-edit only the bits ` + "`scaffold`" + ` couldn't infer (inputs, expected values, edge cases).

When the target is interface-implementing or LSP-driven, also run ` + "`find_implementations`" + ` and ensure each implementor has at least one test in the new file (table-driven test with one row per implementor is idiomatic in this repo).

## Closing the loop

After the new test runs green:
1. ` + "`gortex enrich coverage`" + ` (CLI) to refresh ` + "`meta.coverage_pct`" + `
2. Re-run ` + "`analyze kind=coverage_gaps`" + ` over the same scope
3. Confirm the symbol is no longer in the gap list; if it still is, the test exercises a different branch — add another row

## Checklist

- Run ` + "`gortex enrich coverage`" + ` (CLI) at least once per session so the coverage analyzers have data to work with
- ` + "`analyze kind=coverage_summary`" + ` for the per-directory rollup; ` + "`analyze kind=coverage_gaps`" + ` for the per-symbol list
- ` + "`get_untested_symbols`" + ` is the truth for "zero covering tests" — coverage_pct=0 ≠ "no test"; some tests register but don't execute
- ` + "`suggest_pattern`" + ` + ` + "`scaffold`" + ` for the test body — author in the project's style, not from scratch
- ` + "`preview_edit`" + ` even for new files; the speculative report flags broken_callers if you accidentally add a name that collides
- ` + "`get_test_targets`" + ` after the apply — the test must be discoverable + mapped to a run command
- Re-run ` + "`analyze kind=coverage_gaps`" + ` once the test is green; confirm the gap actually closed (a passing test that doesn't exercise the gap is theatre)
- If the test encodes a project-wide invariant ("Bar must hold the lock"), ` + "`store_memory kind:invariant`" + ` so the next agent inherits the constraint
`
