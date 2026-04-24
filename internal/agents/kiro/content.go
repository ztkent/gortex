// Package kiro implements the Gortex init integration for Kiro
// IDE. Writes:
//
//   - .kiro/settings/mcp.json    (MCP server stanza, merged)
//   - .kiro/steering/gortex-*.md (steering docs, static)
//   - .kiro/hooks/gortex-*.json  (agent hooks, static)
package kiro

// SteeringFiles maps filename → content under .kiro/steering/. The
// "workflow" doc has inclusion: always; the others are manual
// (surfaced only when the user asks for them).
var SteeringFiles = map[string]string{
	"gortex-workflow.md": steeringWorkflow,
	"gortex-explore.md":  steeringExplore,
	"gortex-debug.md":    steeringDebug,
	"gortex-impact.md":   steeringImpact,
	"gortex-refactor.md": steeringRefactor,
}

// HookFiles maps filename → content under .kiro/hooks/. Each file
// is a Kiro agent-hook definition (when+then JSON).
var HookFiles = map[string]string{
	"gortex-smart-context.json": hookSmartContext,
	"gortex-post-edit.json":     hookPostEdit,
	"gortex-pre-read.json":      hookPreRead,
}

const steeringWorkflow = `---
inclusion: always
---

# Gortex Code Intelligence

Gortex is running as an MCP server. It indexes this repository into an in-memory knowledge graph and exposes tools for code navigation, impact analysis, and refactoring.

## Use Gortex tools instead of file reads whenever possible

### Navigation and Reading

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Reading a whole file for one function | ` + "`get_symbol_source`" + ` with ` + "`id: \"path/to/file.go::SymbolName\"`" + ` (80% fewer tokens) — use ` + "`get_file_summary`" + ` first if you don't know the symbol name |
| Reading to find a function            | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| Searching for references              | ` + "`find_usages`" + ` (zero false positives)     |
| Searching to find a symbol by name    | ` + "`search_symbols`" + ` (BM25 + camelCase)      |
| Filtering ` + "`search_symbols`" + ` by hand    | ` + "`winnow_symbols`" + ` — structured constraint chain (kind, language, community, path_prefix, min_fan_in, min_churn) with per-axis score contributions |
| Reading to understand a file          | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` |
| Reading multiple files to trace calls | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Guessing an import path              | ` + "`find_import_path`" + `                       |
| Reading to check a function signature | ` + "`get_symbol_signature`" + `                   |
| 5-10 calls to explore for a task      | ` + "`smart_context`" + ` (one call)               |

### Impact Analysis and Safety

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Reading files to assess change scope  | ` + "`explain_change_impact`" + ` (includes cross-community warnings) |
| Guessing which tests to run           | ` + "`get_test_targets`" + `                       |
| Manual dependency ordering            | ` + "`get_edit_plan`" + `                          |
| Hoping signature changes are safe     | ` + "`verify_change`" + ` — checks callers and interface implementors |
| Manually checking team conventions    | ` + "`check_guards`" + ` — evaluates guard rules from .gortex.yaml |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |

### Code Quality and Analysis

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`analyze`" + ` with ` + "`kind: \"dead_code\"`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`analyze`" + ` with ` + "`kind: \"hotspots\"`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`analyze`" + ` with ` + "`kind: \"cycles\"`" + ` — Tarjan's SCC with severity classification |
| Checking if the index is stale        | ` + "`index_health`" + ` — health score, parse failures, stale files |
| Wondering what changed this session   | ` + "`get_symbol_history`" + ` — modification counts, flags churning (3+ edits) |

### Code Generation and Editing

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Reading files to learn a pattern      | ` + "`suggest_pattern`" + `                        |
| Manually scaffolding from a pattern   | ` + "`scaffold`" + ` — generates code, wiring, and test stubs from an example |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` — applies edits in dependency order, re-indexes between steps |
| Reading a diff without graph context  | ` + "`diff_context`" + ` — enriches git diff with callers, callees, community, risk |
| Guessing what context you need next   | ` + "`prefetch_context`" + ` — predicts needed symbols from task + recent activity |

### Multi-Repo Management

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Manually adding a repo to config      | ` + "`track_repository`" + ` — indexes immediately, persists to config |
| Manually removing a repo from config  | ` + "`untrack_repository`" + ` — evicts nodes/edges, persists to config |
| Wondering which project is active     | ` + "`get_active_project`" + ` — returns project name and repo list |
| Switching project context             | ` + "`set_active_project`" + ` — re-scopes all subsequent queries |
| Scoping a query to one repo           | Pass ` + "`repo`" + ` param to ` + "`search_symbols`" + `, ` + "`find_usages`" + `, etc. |
| Scoping a query to a project          | Pass ` + "`project`" + ` param to any query tool |
| Filtering by reference tag            | Pass ` + "`ref`" + ` param to any query tool |

## Session workflow

1. Call ` + "`graph_stats`" + ` to confirm Gortex is running. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with path ` + "`\".\"`" + `.
2. In multi-repo mode, call ` + "`get_active_project`" + ` to check scope. Use ` + "`set_active_project`" + ` to switch if needed.
3. For a new task, call ` + "`smart_context`" + ` with the task description.
4. Before editing any file, call ` + "`get_editing_context`" + ` first.
5. Before changing a function signature, call ` + "`verify_change`" + ` to catch contract violations — checks callers across all repos.
6. Before any refactor, call ` + "`get_edit_plan`" + ` for dependency-ordered file list. Use ` + "`batch_edit`" + ` to apply atomically.
7. After editing, call ` + "`check_guards`" + ` to verify team conventions, then ` + "`get_test_targets`" + ` for tests to run (includes cross-repo test files).
8. Before committing, call ` + "`detect_changes`" + ` to verify scope. Use ` + "`diff_context`" + ` for graph-enriched review.
`

const steeringExplore = `---
inclusion: manual
---

# Exploring Codebases with Gortex

## Workflow

1. ` + "`graph_stats`" + ` — confirm index, get node/edge counts
2. ` + "`get_communities`" + ` — see functional clusters (architecture overview)
3. ` + "`search_symbols({query: \"<concept>\"})`" + ` — find symbols related to a concept
4. ` + "`get_processes`" + ` — discover execution flows
5. ` + "`get_process({id: \"<process-id>\"})`" + ` — trace a specific flow step by step
6. ` + "`get_editing_context({path: \"<file>\"})`" + ` — deep dive on a specific file

## When to use

- "How does authentication work?"
- "What's the project structure?"
- "Show me the main components"
- Understanding code you haven't seen before

## Key tools

- ` + "`get_communities`" + ` for architectural overview (functional clusters with cohesion scores)
- ` + "`get_processes`" + ` for execution flow discovery (entry points to call chains)
- ` + "`search_symbols`" + ` for concept-based symbol search (BM25 + camelCase-aware)
- ` + "`get_editing_context`" + ` for 360-degree file view (symbols, callers, callees, imports)
`

const steeringDebug = `---
inclusion: manual
---

# Debugging with Gortex

## Workflow

1. ` + "`search_symbols({query: \"<error or suspect>\"})`" + ` — find related symbols
2. ` + "`get_callers({id: \"<suspect>\"})`" + ` — who calls it?
3. ` + "`get_call_chain({id: \"<suspect>\"})`" + ` — what does it call?
4. ` + "`get_editing_context({path: \"<file>\"})`" + ` — full file context
5. ` + "`get_process({id: \"<process>\"})`" + ` — trace execution flow

## Debugging patterns

| Symptom              | Gortex Approach |
| -------------------- | --------------- |
| Error message        | ` + "`search_symbols`" + ` for error-related names, then ` + "`get_callers`" + ` on throw sites |
| Wrong return value   | ` + "`get_call_chain`" + ` on the function, trace callees for data flow |
| Intermittent failure | ` + "`get_editing_context`" + `, look for external calls and async deps |
| Performance issue    | ` + "`find_usages`" + `, find symbols with many callers (hot paths) |
| Recent regression    | ` + "`detect_changes`" + `, see what your changes affect |
`

const steeringImpact = `---
inclusion: manual
---

# Impact Analysis with Gortex

## Workflow

1. ` + "`search_symbols({query: \"X\"})`" + ` — find the symbol ID
2. ` + "`explain_change_impact({ids: \"<id1>, <id2>\"})`" + ` — risk-tiered blast radius
3. ` + "`get_dependents({id: \"<symbol-id>\", depth: 3})`" + ` — detailed dependent tree
4. ` + "`detect_changes({scope: \"staged\"})`" + ` — pre-commit check

## Risk tiers

| Depth | Risk Level     | Meaning                  |
| ----- | -------------- | ------------------------ |
| d=1   | WILL BREAK     | Direct callers/importers |
| d=2   | LIKELY AFFECTED| Indirect dependencies    |
| d=3   | MAY NEED TESTING| Transitive effects      |

## Before any non-trivial change

- Call ` + "`explain_change_impact`" + ` with all symbols you plan to modify
- Review the risk level (LOW/MEDIUM/HIGH/CRITICAL)
- Check ` + "`by_depth`" + `: d=1 items WILL BREAK
- Note ` + "`affected_processes`" + ` and ` + "`affected_communities`" + `
- Check ` + "`test_files`" + ` that need re-running
- Before commit: ` + "`detect_changes`" + ` to verify scope
`

const steeringRefactor = `---
inclusion: manual
---

# Refactoring with Gortex

## Workflow

1. ` + "`search_symbols({query: \"X\"})`" + ` — find the symbol ID
2. ` + "`explain_change_impact({ids: \"<id>\"})`" + ` — map blast radius
3. ` + "`get_editing_context({path: \"<file>\"})`" + ` — see all symbols and relationships
4. ` + "`find_usages({id: \"<id>\"})`" + ` — every reference to change
5. ` + "`get_edit_plan({ids: \"<ids>\"})`" + ` — dependency-ordered edit sequence
6. Edit in order: interfaces -> implementations -> callers -> tests
7. ` + "`detect_changes({scope: \"all\"})`" + ` — verify after changes

## Rename symbol

- ` + "`find_usages`" + ` to get every reference location
- ` + "`explain_change_impact`" + ` to assess blast radius
- Edit in dependency order: definition, then callers, then tests

## Extract module

- ` + "`get_editing_context`" + ` on the source file to see all symbols
- ` + "`get_dependents`" + ` on symbols to extract to find external callers
- ` + "`find_import_path`" + ` for correct import paths in the new location

## Split function/service

- ` + "`get_call_chain`" + ` to understand all callees
- ` + "`get_callers`" + ` to map all call sites that need updating
- ` + "`explain_change_impact`" + ` for full blast radius
`

const hookSmartContext = `{
  "name": "Gortex: Smart Context on Prompt",
  "version": "1.0.0",
  "description": "On each new prompt, calls smart_context to assemble task-relevant code context from the knowledge graph in one shot.",
  "when": {
    "type": "promptSubmit"
  },
  "then": {
    "type": "askAgent",
    "prompt": "If the user's message describes a coding task (adding a feature, fixing a bug, refactoring, understanding code), call Gortex's smart_context tool with the task description to get relevant symbols, source code, relationships, and an edit plan in one call. Skip this for non-coding questions or simple chat."
  }
}
`

const hookPostEdit = `{
  "name": "Gortex: Post-Edit Impact Check",
  "version": "1.0.0",
  "description": "After saving a source file, runs detect_changes and get_test_targets to show blast radius and which tests to run.",
  "when": {
    "type": "fileEdited",
    "patterns": ["**/*.go", "**/*.ts", "**/*.tsx", "**/*.js", "**/*.jsx", "**/*.py", "**/*.rs", "**/*.java", "**/*.kt", "**/*.scala", "**/*.swift", "**/*.rb", "**/*.cs", "**/*.php"]
  },
  "then": {
    "type": "askAgent",
    "prompt": "A source file was just edited. Call Gortex detect_changes with scope unstaged to see which symbols were affected and the risk level. If symbols were changed, also call get_test_targets with those symbol IDs to identify which tests should be run. Briefly report the risk level and test commands."
  }
}
`

const hookPreRead = `{
  "name": "Gortex: Enrich File Reads",
  "version": "1.0.0",
  "description": "Before reading a source file, calls get_editing_context to inject symbol context, callers, callees, and imports.",
  "when": {
    "type": "preToolUse",
    "toolTypes": ["read"]
  },
  "then": {
    "type": "askAgent",
    "prompt": "SKIP this hook entirely (do nothing, proceed with the read) if ANY of these are true: (1) the file path contains .kiro/, .claude/, .github/, .vscode/, or node_modules/, (2) the file extension is .md, .json, .yaml, .yml, .toml, .txt, .lock, .sum, .mod, .env, .gitignore, .html, .css, or .svg, (3) the file is not a source code file. ONLY for source code files (.go, .ts, .tsx, .js, .jsx, .py, .rs, .java, .kt, .cs, .rb, .php, .swift, .scala, .c, .cpp, .h): call get_editing_context or get_file_summary for the file to get symbol context before reading it."
  }
}
`

// AutoApproveTools is the list of Gortex MCP tools Kiro should
// auto-approve without prompting. Baked into the mcp.json entry so
// the user isn't interrupted for every query call.
var AutoApproveTools = []string{
	"graph_stats", "search_symbols", "winnow_symbols", "get_symbol", "get_file_summary",
	"get_editing_context", "get_dependencies", "get_dependents",
	"get_call_chain", "get_callers", "find_implementations", "find_usages",
	"get_cluster", "get_symbol_source", "batch_symbols",
	"find_import_path", "explain_change_impact", "get_recent_changes",
	"smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
	"get_communities", "get_processes",
	"detect_changes", "index_repository",
	"verify_change", "check_guards", "prefetch_context",
	"analyze",
	"diff_context", "index_health", "get_symbol_history",
	"scaffold", "batch_edit",
	"contracts", "feedback",
}
