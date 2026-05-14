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

Use these for guided workflows: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-refactor`" + `
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
	"gortex-guide.md":    commandGuide,
	"gortex-explore.md":  commandExplore,
	"gortex-debug.md":    commandDebug,
	"gortex-impact.md":   commandImpact,
	"gortex-refactor.md": commandRefactor,
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
}

const commandGuide = `# Gortex Guide

Quick reference for all Gortex MCP tools and the knowledge graph schema.

## Always Start Here

1. **Call ` + "`graph_stats`" + `** — confirm Gortex is running, get node/edge counts
2. **Match your task to a command below**
3. **Follow the command's workflow**

> If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with ` + "`path: \".\"`" + ` first.

## Commands

| Task                                         | Command                  |
| -------------------------------------------- | ------------------------ |
| Understand architecture / "How does X work?" | /gortex-explore          |
| Blast radius / "What breaks if I change X?"  | /gortex-impact           |
| Trace bugs / "Why is X failing?"             | /gortex-debug            |
| Rename / extract / split / refactor          | /gortex-refactor         |
| Tools, schema reference                      | /gortex-guide (this)     |

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
| analyze | Unified graph analysis. Supported kinds: dead_code, hotspots, cycles, would_create_cycle, todos, blame, coverage, stale_code, ownership, coverage_gaps, coverage_summary, stale_flags, releases, cgo_users, wasm_users, orphan_tables, unreferenced_tables, channel_ops, goroutine_spawns, field_writers, annotation_users, config_readers, event_emitters, error_surface, external_calls, routes, models, components, k8s_resources, images, kustomize |
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
