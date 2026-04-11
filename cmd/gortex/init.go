package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/claudemd"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

var initAnalyze bool

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Set up Gortex for a project: creates configs for Claude Code and Kiro IDE",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runInit,
}

func init() {
	initCmd.Flags().BoolVar(&initAnalyze, "analyze", false, "index the repo first to generate a richer CLAUDE.md with codebase overview")
	rootCmd.AddCommand(initCmd)
}

func runInit(_ *cobra.Command, args []string) error {
	root := "."
	if len(args) > 0 {
		root = args[0]
	}

	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	// 1. Create .mcp.json
	if err := writeIfNotExists(filepath.Join(root, ".mcp.json"), mcpJSON); err != nil {
		return err
	}

	// 2. Create .claude/commands/
	cmdDir := filepath.Join(root, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		return err
	}

	for name, content := range commands {
		if err := writeIfNotExists(filepath.Join(cmdDir, name), content); err != nil {
			return err
		}
	}

	// 3. Append Gortex block to CLAUDE.md
	claudeMdPath := filepath.Join(root, "CLAUDE.md")
	block := claudeMdBlock
	if initAnalyze {
		fmt.Fprintf(os.Stderr, "[gortex init] indexing %s...\n", root)
		overview, err := generateOverview(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex init] indexing failed: %v — using static block\n", err)
		} else {
			block = overview + "\n" + claudeMdBlock
		}
	}
	if err := appendGortexBlock(claudeMdPath, block); err != nil {
		return err
	}

	// 4. Install global skills in ~/.claude/skills/gortex-*/
	if err := installGlobalSkills(); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: could not install global skills: %v\n", err)
	}

	// 5. Install MCP tool permissions in .claude/settings.json (shared, committed)
	if err := installPermissions(filepath.Join(root, ".claude", "settings.json")); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: could not install permissions: %v\n", err)
	}

	// 6. Install PreToolUse hook in .claude/settings.local.json (local, not committed)
	if err := installHook(filepath.Join(root, ".claude", "settings.local.json")); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: could not install hook: %v\n", err)
	}

	// 7. Set up Kiro IDE integration
	if err := setupKiro(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Kiro setup failed: %v\n", err)
	}

	// 8. Set up Antigravity integration
	if err := setupAntigravity(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Antigravity setup failed: %v\n", err)
	}

	// 9. Set up Cursor IDE integration
	if err := setupCursor(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Cursor setup failed: %v\n", err)
	}

	// 10. Set up VS Code / GitHub Copilot integration
	if err := setupVSCodeCopilot(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: VS Code / Copilot setup failed: %v\n", err)
	}

	// 11. Set up Windsurf integration
	if err := setupWindsurf(); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Windsurf setup failed: %v\n", err)
	}

	// 12. Set up Continue.dev integration
	if err := setupContinue(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Continue.dev setup failed: %v\n", err)
	}

	// 13. Set up Cline integration
	if err := setupCline(); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: Cline setup failed: %v\n", err)
	}

	// 14. Set up OpenCode integration
	if err := setupOpenCode(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: OpenCode setup failed: %v\n", err)
	}

	// 15. Create/update GlobalConfig for multi-repo mode.
	if err := ensureGlobalConfig(root); err != nil {
		fmt.Fprintf(os.Stderr, "[gortex init] warning: could not update global config: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[gortex init] done — created:\n")
	fmt.Fprintf(os.Stderr, "  .mcp.json                       (MCP server config — shared)\n")
	fmt.Fprintf(os.Stderr, "  .claude/commands/gortex-*.md     (Claude Code slash commands)\n")
	fmt.Fprintf(os.Stderr, "  .claude/settings.json            (Claude Code MCP permissions — shared)\n")
	fmt.Fprintf(os.Stderr, "  .claude/settings.local.json      (Claude Code PreToolUse hook — local)\n")
	fmt.Fprintf(os.Stderr, "  CLAUDE.md                        (Claude Code instructions)\n")
	fmt.Fprintf(os.Stderr, "  ~/.claude/skills/gortex-*        (Claude Code global skills)\n")
	fmt.Fprintf(os.Stderr, "  .kiro/settings/mcp.json          (Kiro MCP server config)\n")
	fmt.Fprintf(os.Stderr, "  .kiro/steering/gortex-*.md       (Kiro steering files)\n")
	fmt.Fprintf(os.Stderr, "  .kiro/hooks/gortex-*.json        (Kiro agent hooks)\n")
	fmt.Fprintf(os.Stderr, "  ~/.gemini/antigravity/knowledge/gortex-workflow/  (Antigravity KI)\n")
	fmt.Fprintf(os.Stderr, "  .cursor/mcp.json                 (Cursor MCP server config)\n")
	fmt.Fprintf(os.Stderr, "  .vscode/mcp.json                 (VS Code / GitHub Copilot MCP config)\n")
	fmt.Fprintf(os.Stderr, "  ~/.codeium/windsurf/mcp_config.json  (Windsurf MCP config)\n")
	fmt.Fprintf(os.Stderr, "  .continue/mcpServers/gortex.json (Continue.dev MCP config)\n")
	fmt.Fprintf(os.Stderr, "  ~/...cline_mcp_settings.json     (Cline MCP config)\n")
	fmt.Fprintf(os.Stderr, "  .opencode/config.json            (OpenCode MCP config)\n")
	fmt.Fprintf(os.Stderr, "\nCommit these files so your team gets Gortex automatically.\n")
	fmt.Fprintf(os.Stderr, "Run `gortex serve --index . --watch` or let your IDE start it via MCP config.\n")
	return nil
}

func writeIfNotExists(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "[gortex init] skip %s (already exists)\n", path)
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "[gortex init] created %s\n", path)
	return nil
}

func appendGortexBlock(path, block string) error {
	existing, _ := os.ReadFile(path)
	if len(existing) > 0 {
		// Check if Gortex block already present
		if contains(string(existing), "## MANDATORY: Use Gortex MCP tools") {
			fmt.Fprintf(os.Stderr, "[gortex init] skip %s (Gortex block already present)\n", path)
			return nil
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	prefix := ""
	if len(existing) > 0 {
		prefix = "\n\n"
	}
	if _, err := f.WriteString(prefix + block); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] appended Gortex block to %s\n", path)
	return nil
}

func generateOverview(root string) (string, error) {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load("")
	if err != nil {
		cfg = &config.Config{}
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)
	result, err := idx.Index(root)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "[gortex init] indexed %d files (%d nodes, %d edges) in %dms\n",
		result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)

	eng := query.NewEngine(g)
	return claudemd.Generate(eng, 180), nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func installGlobalSkills() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	skillsDir := filepath.Join(home, ".claude", "skills")

	for name, content := range globalSkills {
		dir := filepath.Join(skillsDir, name)
		path := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(os.Stderr, "[gortex init] skip ~/.claude/skills/%s (already exists)\n", name)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[gortex init] created ~/.claude/skills/%s\n", name)
	}
	return nil
}

var globalSkills = map[string]string{
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

func installPermissions(settingsPath string) error {
	// Read existing settings or start fresh.
	var settings map[string]any
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = make(map[string]any)
		}
	} else {
		settings = make(map[string]any)
	}

	// Check if Gortex permission already exists.
	if perms, ok := settings["permissions"].(map[string]any); ok {
		if allow, ok := perms["allow"].([]any); ok {
			for _, entry := range allow {
				if s, ok := entry.(string); ok && contains(s, "mcp__gortex__") {
					fmt.Fprintf(os.Stderr, "[gortex init] skip %s (Gortex permissions already present)\n", settingsPath)
					return nil
				}
			}
		}
	}

	// Ensure permissions.allow exists and append the wildcard rule.
	if _, ok := settings["permissions"]; !ok {
		settings["permissions"] = make(map[string]any)
	}
	perms := settings["permissions"].(map[string]any)
	if _, ok := perms["allow"]; !ok {
		perms["allow"] = []any{}
	}
	allow := perms["allow"].([]any)
	perms["allow"] = append(allow, "mcp__gortex__*")

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] installed MCP permissions in %s\n", settingsPath)
	return nil
}

func installHook(settingsPath string) error {
	// Read existing settings or start fresh.
	var settings map[string]any
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = make(map[string]any)
		}
	} else {
		settings = make(map[string]any)
	}

	// Check if Gortex hook already exists.
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		if pre, ok := hooks["PreToolUse"].([]any); ok {
			for _, h := range pre {
				if hm, ok := h.(map[string]any); ok {
					if hs, ok := hm["hooks"].([]any); ok {
						for _, entry := range hs {
							if em, ok := entry.(map[string]any); ok {
								if cmd, ok := em["command"].(string); ok && contains(cmd, "gortex hook") {
									fmt.Fprintf(os.Stderr, "[gortex init] skip %s (Gortex hook already present)\n", settingsPath)
									return nil
								}
							}
						}
					}
				}
			}
		}
	}

	// Resolve the gortex binary path for the hook command.
	// Try: 1) the binary that's running now, 2) "gortex" in PATH.
	hookCommand := "gortex hook"
	if exe, err := os.Executable(); err == nil {
		hookCommand = exe + " hook"
	}

	// Build the hook entry.
	hookEntry := map[string]any{
		"matcher": "Read|Grep",
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       hookCommand,
				"timeout":       3000,
				"statusMessage": "Enriching with Gortex graph context...",
			},
		},
	}

	// Ensure hooks.PreToolUse exists and append.
	if _, ok := settings["hooks"]; !ok {
		settings["hooks"] = make(map[string]any)
	}
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		hooks["PreToolUse"] = []any{}
	}
	pre := hooks["PreToolUse"].([]any)
	hooks["PreToolUse"] = append(pre, hookEntry)

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] installed PreToolUse hook in %s\n", settingsPath)
	return nil
}

const mcpJSON = `{
  "mcpServers": {
    "gortex": {
      "command": "gortex",
      "args": [
        "serve",
        "--index", ".",
        "--watch",
        "--web"
      ],
      "env": {
        "GORTEX_INDEX_WORKERS": "${GORTEX_WORKERS:-8}"
      }
    }
  }
}
`

const claudeMdBlock = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex is running as an MCP server. You MUST use graph queries instead of file reads whenever possible. This saves thousands of tokens per task.

### Navigation and Reading

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` a whole file for one function  | ` + "`get_symbol_source`" + ` (80% fewer tokens)   |
| ` + "`Read`" + ` to find a function             | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find a symbol by name       | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` |
| ` + "`Read`" + ` multiple files to trace calls  | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Guessing an import path               | ` + "`find_import_path`" + `                       |
| ` + "`Read`" + ` to check a function signature  | ` + "`get_symbol_signature`" + `                   |
| 5-10 calls to explore for a task      | ` + "`smart_context`" + ` (one call)               |

### Impact Analysis and Safety

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to assess change scope  | ` + "`explain_change_impact`" + ` (includes cross-community warnings) |
| Guessing which tests to run           | ` + "`get_test_targets`" + `                       |
| Manual dependency ordering            | ` + "`get_edit_plan`" + `                          |
| Hoping signature changes are safe     | ` + "`verify_change`" + ` — checks callers and interface implementors |
| Manually checking team conventions    | ` + "`check_guards`" + ` — evaluates guard rules from .gortex.yaml |
| Wondering if a new dep creates a cycle| ` + "`would_create_cycle`" + ` — checks before you add it |

### Code Quality and Analysis

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`find_dead_code`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`find_hotspots`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`find_cycles`" + ` — Tarjan's SCC with severity classification |
| Checking if the index is stale        | ` + "`index_health`" + ` — health score, parse failures, stale files |
| Wondering what changed this session   | ` + "`get_symbol_history`" + ` — modification counts, flags churning (3+ edits) |

### Code Generation and Editing

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to learn a pattern      | ` + "`suggest_pattern`" + `                        |
| Manually scaffolding from a pattern   | ` + "`scaffold`" + ` — generates code, wiring, and test stubs from an example |
| Read→Edit roundtrip for one symbol    | ` + "`edit_symbol`" + ` — edit source by ID, no Read needed |
| Manual find-and-replace for renames   | ` + "`rename_symbol`" + ` — coordinated rename across all references |
| Sequencing multi-file edits yourself  | ` + "`batch_edit`" + ` — applies edits in dependency order, re-indexes between steps |
| Reading a diff without graph context  | ` + "`diff_context`" + ` — enriches git diff with callers, callees, community, risk |
| Guessing what context you need next   | ` + "`prefetch_context`" + ` — predicts needed symbols from task + recent activity |

### API Contracts

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually tracking API routes/services | ` + "`get_contracts`" + ` — lists HTTP, gRPC, GraphQL, topic, WebSocket, env, OpenAPI |
| Guessing if APIs match across repos   | ` + "`check_contracts`" + ` — detects orphan providers/consumers and mismatches |

### Multi-Repo Management

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually adding a repo to config      | ` + "`track_repository`" + ` — indexes immediately, persists to config |
| Manually removing a repo from config  | ` + "`untrack_repository`" + ` — evicts nodes/edges, persists to config |
| Wondering which project is active     | ` + "`get_active_project`" + ` — returns project name and repo list |
| Switching project context             | ` + "`set_active_project`" + ` — re-scopes all subsequent queries |
| Scoping a query to one repo           | Pass ` + "`repo`" + ` param to ` + "`search_symbols`" + `, ` + "`find_usages`" + `, etc. |
| Scoping a query to a project          | Pass ` + "`project`" + ` param to any query tool |
| Filtering by reference tag            | Pass ` + "`ref`" + ` param to any query tool |

## Session start (Gortex)

1. Call ` + "`graph_stats`" + ` to confirm Gortex is running and get repo orientation.
2. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with path ` + "`\".\"`" + `.
3. In multi-repo mode, call ` + "`get_active_project`" + ` to check scope. Use ` + "`set_active_project`" + ` to switch if needed.
4. For a new task, call ` + "`smart_context`" + ` with the task description.
5. For every file you are about to edit, call ` + "`get_editing_context`" + ` first.
6. Before changing a function signature, call ` + "`verify_change`" + ` to catch contract violations — checks callers across all repos.
7. Before any refactor, call ` + "`get_edit_plan`" + ` for dependency-ordered file list. Use ` + "`batch_edit`" + ` to apply atomically.
8. After editing, call ` + "`check_guards`" + ` to verify team conventions, then ` + "`get_test_targets`" + ` for tests to run (includes cross-repo test files).
9. Before committing, call ` + "`detect_changes`" + ` to verify scope. Use ` + "`diff_context`" + ` for graph-enriched review.

## Gortex slash commands

Use these for guided workflows: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-refactor`" + `
`

var commands = map[string]string{
	"gortex-guide.md":    commandGuide,
	"gortex-explore.md":  commandExplore,
	"gortex-debug.md":    commandDebug,
	"gortex-impact.md":   commandImpact,
	"gortex-refactor.md": commandRefactor,
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
| get_symbol_signature | Just the signature, no body — API boundary check |
| get_symbol_source | Source code of a single symbol — use instead of Read |
| batch_symbols | Multiple symbols with source/callers/callees in one call |
| find_import_path | Correct import path for a symbol in a target file |
| explain_change_impact | Risk-tiered blast radius with affected processes/communities |
| edit_symbol | Edit symbol source by ID — no Read needed, resolves file + lines |
| rename_symbol | Coordinated rename: generates edits for definition + all references |
| get_recent_changes | Files/symbols changed since timestamp (watch mode) |

### Agent-Optimized (token efficiency)
| Tool | What it gives you |
|------|-------------------|
| smart_context | Task-aware minimal context bundle — replaces 5-10 exploration calls |
| get_edit_plan | Dependency-ordered edit sequence for multi-file refactors |
| get_test_targets | Maps changed symbols to test files and run commands |
| suggest_pattern | Extracts code pattern from an example — source, registration, tests |

### Analysis
| Tool | What it gives you |
|------|-------------------|
| get_communities | Functional clusters via Louvain community detection |
| get_community | Members, files, cohesion for one community |
| get_processes | Discovered execution flows (entry points -> call chains) |
| get_process | Full step-by-step trace of one execution flow |
| detect_changes | Git diff -> affected symbols -> blast radius |

### Proactive Safety
| Tool | What it gives you |
|------|-------------------|
| verify_change | Checks proposed signature changes against all callers and interface implementors |
| check_guards | Evaluates project guard rules (.gortex.yaml) against changed symbols |
| would_create_cycle | Checks if adding a dependency would create a circular dependency |

### Code Quality
| Tool | What it gives you |
|------|-------------------|
| find_dead_code | Symbols with zero incoming edges (excludes entry points, tests, exports) |
| find_hotspots | Symbols ranked by fan-in, fan-out, and community boundary crossings |
| find_cycles | Circular dependency detection via Tarjan's SCC, classified by severity |
| index_health | Health score, parse failures, stale files, language coverage |
| get_symbol_history | Symbols modified this session with counts; flags churning (3+ edits) |

### Code Generation
| Tool | What it gives you |
|------|-------------------|
| scaffold | Generates code, registration wiring, and test stubs from an example symbol |
| batch_edit | Applies multiple edits in dependency order, re-indexes between steps |
| diff_context | Git diff enriched with callers, callees, community, processes, per-file risk |
| prefetch_context | Predicts needed symbols from task description + recent activity |

### API Contracts
| Tool | What it gives you |
|------|-------------------|
| get_contracts | Lists detected API contracts: HTTP routes, gRPC, GraphQL, topics, WebSocket, env vars, OpenAPI |
| check_contracts | Matches providers to consumers, reports orphans and mismatches across repos |

### Multi-Repo
| Tool | What it gives you |
|------|-------------------|
| index_repository | Index a repository path into the graph |
| track_repository | Add a repo to the workspace, index immediately, persist to config |
| untrack_repository | Remove a repo, evict its nodes/edges, persist to config |
| get_active_project | Current project name and member repository list |
| set_active_project | Switch project scope — re-scopes all subsequent queries |

## Graph Schema

**Node kinds:** file, function, method, type, interface, variable, import, package, contract
**Edge kinds:** calls, imports, defines, implements, extends, references, member_of, instantiates, provides, consumes
`

const commandExplore = `# Exploring Codebases with Gortex

## Workflow

` + "```" + `
1. graph_stats                                  -> Confirm index, get node/edge counts
2. get_communities                              -> See functional clusters (architecture overview)
3. search_symbols({query: "<concept>"})         -> Find symbols related to a concept
4. get_processes                                -> Discover execution flows
5. get_process({id: "<process-id>"})            -> Trace a specific flow step by step
6. get_editing_context({file_path: "<file>"})   -> Deep dive on a specific file
` + "```" + `

## Checklist

- Call graph_stats to confirm Gortex is running
- Call get_communities for architecture overview
- Call search_symbols for the concept you want to understand
- Call get_processes to discover execution flows
- Call get_process on relevant flows for step-by-step traces
- Call get_editing_context on key files for full symbol context
- Read source files only for implementation details you actually need to edit
`

const commandDebug = `# Debugging with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "<error or suspect>"})          -> Find related symbols
2. get_callers({function_id: "<suspect>"})                -> Who calls it?
3. get_call_chain({function_id: "<suspect>"})             -> What does it call?
4. get_editing_context({file_path: "<file>"})             -> Full file context
5. get_process({id: "<process>"})                         -> Trace execution flow
` + "```" + `

## Debugging Patterns

| Symptom              | Gortex Approach |
| -------------------- | --------------- |
| Error message        | search_symbols for error-related names -> get_callers on throw sites |
| Wrong return value   | get_call_chain on the function -> trace callees for data flow |
| Intermittent failure | get_editing_context -> look for external calls, async deps |
| Performance issue    | find_usages -> find symbols with many callers (hot paths) |
| Recent regression    | detect_changes -> see what your changes affect |
`

const commandImpact = `# Impact Analysis with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "X"})                                     -> Find the symbol ID
2. explain_change_impact({symbol_ids: "<id1>, <id2>"})              -> Risk-tiered blast radius
3. get_dependents({id: "<symbol-id>", depth: 3})                    -> Detailed dependent tree
4. detect_changes({scope: "staged"})                                -> Pre-commit check
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
- Check test_files that need re-running
- Before commit: detect_changes to verify scope
`

const commandRefactor = `# Refactoring with Gortex

## Workflow

` + "```" + `
1. search_symbols({query: "X"})                                     -> Find the symbol ID
2. explain_change_impact({symbol_ids: "<id>"})                      -> Map blast radius
3. get_editing_context({file_path: "<file>"})                       -> See all symbols and relationships
4. find_usages({id: "<id>"})                                        -> Every reference to change
5. Plan update order: interfaces -> implementations -> callers -> tests
6. detect_changes({scope: "all"})                                   -> Verify after changes
` + "```" + `

## Rename Symbol

- search_symbols to find the symbol ID
- explain_change_impact to assess blast radius
- rename_symbol({id: "<id>", new_name: "<name>"}) — generates all edits (definition + references)
- Review the generated edits, then apply with the Edit tool
- detect_changes to verify only expected files changed

## Extract Module

- get_editing_context on the source file — see all symbols
- get_dependents on symbols to extract — find external callers
- explain_change_impact on symbols being moved
- Define new module interface
- Extract code, update imports (use find_import_path for correct paths)
- detect_changes to verify affected scope

## Split Function/Service

- get_call_chain on the function — understand all callees
- Group callees by responsibility
- get_callers to map all call sites that need updating
- explain_change_impact for full blast radius
- Create new functions/services
- Update callers (use find_usages for precise locations)
- detect_changes to verify affected scope
`

// ─── Multi-Repo GlobalConfig ────────────────────────────────────────────────

// ensureGlobalConfig creates or updates the GlobalConfig at ~/.config/gortex/config.yaml
// with the current repository entry.
func ensureGlobalConfig(root string) error {
	gc, err := config.LoadGlobal()
	if err != nil {
		return err
	}

	// Add the current repo to the global config.
	if err := gc.AddRepo(config.RepoEntry{Path: root}); err != nil {
		return err
	}

	if err := gc.Save(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "[gortex init] updated global config at %s\n", gc.ConfigPath())
	return nil
}

// ─── Kiro IDE Integration ───────────────────────────────────────────────────

// isKiroInstalled checks whether Kiro IDE is present on this system or project.
// Returns true if any of: ~/.kiro exists, "kiro" is in PATH, or .kiro/ already exists in the project.
func isKiroInstalled(root string) bool {
	// 1. Project already has .kiro/ (team member uses Kiro).
	if _, err := os.Stat(filepath.Join(root, ".kiro")); err == nil {
		return true
	}
	// 2. User-level Kiro config exists.
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".kiro")); err == nil {
			return true
		}
	}
	// 3. "kiro" CLI is in PATH.
	if path, err := exec.LookPath("kiro"); err == nil && path != "" {
		return true
	}
	return false
}

// setupKiro creates .kiro/settings/mcp.json, .kiro/steering/*.md, and .kiro/hooks/*.json.
// Only runs if Kiro is detected on the system or project.
func setupKiro(root string) error {
	if !isKiroInstalled(root) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip Kiro setup (Kiro not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Kiro IDE integration...\n")

	// 1. MCP config: .kiro/settings/mcp.json
	kiroMCPDir := filepath.Join(root, ".kiro", "settings")
	if err := os.MkdirAll(kiroMCPDir, 0o755); err != nil {
		return fmt.Errorf("creating .kiro/settings: %w", err)
	}
	kiroMCPPath := filepath.Join(kiroMCPDir, "mcp.json")
	if err := writeMergeKiroMCP(kiroMCPPath); err != nil {
		return err
	}

	// 2. Steering files: .kiro/steering/gortex-*.md
	steeringDir := filepath.Join(root, ".kiro", "steering")
	if err := os.MkdirAll(steeringDir, 0o755); err != nil {
		return fmt.Errorf("creating .kiro/steering: %w", err)
	}
	for name, content := range kiroSteering {
		if err := writeIfNotExists(filepath.Join(steeringDir, name), content); err != nil {
			return err
		}
	}

	// 3. Hooks: .kiro/hooks/gortex-*.json
	hooksDir := filepath.Join(root, ".kiro", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating .kiro/hooks: %w", err)
	}
	for name, content := range kiroHooks {
		if err := writeIfNotExists(filepath.Join(hooksDir, name), content); err != nil {
			return err
		}
	}

	return nil
}

// writeMergeKiroMCP writes or merges the gortex server into .kiro/settings/mcp.json.
func writeMergeKiroMCP(path string) error {
	var config map[string]any

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			config = make(map[string]any)
		}
	} else {
		config = make(map[string]any)
	}

	servers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}

	if _, exists := servers["gortex"]; exists {
		fmt.Fprintf(os.Stderr, "[gortex init] skip %s (gortex server already configured)\n", path)
		return nil
	}

	servers["gortex"] = map[string]any{
		"command":  "gortex",
		"args":     []string{"serve", "--index", ".", "--watch"},
		"env":      map[string]string{"GORTEX_INDEX_WORKERS": "8"},
		"disabled": false,
		"autoApprove": []string{
			"graph_stats", "search_symbols", "get_symbol", "get_file_summary",
			"get_editing_context", "get_dependencies", "get_dependents",
			"get_call_chain", "get_callers", "find_implementations", "find_usages",
			"get_cluster", "get_symbol_signature", "get_symbol_source", "batch_symbols",
			"find_import_path", "explain_change_impact", "get_recent_changes",
			"smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
			"get_communities", "get_community", "get_processes", "get_process",
			"detect_changes", "index_repository",
			"verify_change", "check_guards", "prefetch_context",
			"find_dead_code", "find_hotspots", "find_cycles", "would_create_cycle",
			"diff_context", "index_health", "get_symbol_history",
			"scaffold", "batch_edit",
		},
	}
	config["mcpServers"] = servers

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] created %s\n", path)
	return nil
}

var kiroSteering = map[string]string{
	"gortex-workflow.md": kiroSteeringWorkflow,
	"gortex-explore.md":  kiroSteeringExplore,
	"gortex-debug.md":    kiroSteeringDebug,
	"gortex-impact.md":   kiroSteeringImpact,
	"gortex-refactor.md": kiroSteeringRefactor,
}

const kiroSteeringWorkflow = `---
inclusion: always
---

# Gortex Code Intelligence

Gortex is running as an MCP server. It indexes this repository into an in-memory knowledge graph and exposes tools for code navigation, impact analysis, and refactoring.

## Use Gortex tools instead of file reads whenever possible

### Navigation and Reading

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Reading a whole file for one function | ` + "`get_symbol_source`" + ` (80% fewer tokens)   |
| Reading to find a function            | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| Searching for references              | ` + "`find_usages`" + ` (zero false positives)     |
| Searching to find a symbol by name    | ` + "`search_symbols`" + ` (BM25 + camelCase)      |
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
| Wondering if a new dep creates a cycle| ` + "`would_create_cycle`" + ` — checks before you add it |

### Code Quality and Analysis

| Instead of...                         | Use...                                   |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`find_dead_code`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`find_hotspots`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`find_cycles`" + ` — Tarjan's SCC with severity classification |
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

const kiroSteeringExplore = `---
inclusion: manual
---

# Exploring Codebases with Gortex

## Workflow

1. ` + "`graph_stats`" + ` — confirm index, get node/edge counts
2. ` + "`get_communities`" + ` — see functional clusters (architecture overview)
3. ` + "`search_symbols({query: \"<concept>\"})`" + ` — find symbols related to a concept
4. ` + "`get_processes`" + ` — discover execution flows
5. ` + "`get_process({id: \"<process-id>\"})`" + ` — trace a specific flow step by step
6. ` + "`get_editing_context({file_path: \"<file>\"})`" + ` — deep dive on a specific file

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

const kiroSteeringDebug = `---
inclusion: manual
---

# Debugging with Gortex

## Workflow

1. ` + "`search_symbols({query: \"<error or suspect>\"})`" + ` — find related symbols
2. ` + "`get_callers({function_id: \"<suspect>\"})`" + ` — who calls it?
3. ` + "`get_call_chain({function_id: \"<suspect>\"})`" + ` — what does it call?
4. ` + "`get_editing_context({file_path: \"<file>\"})`" + ` — full file context
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

const kiroSteeringImpact = `---
inclusion: manual
---

# Impact Analysis with Gortex

## Workflow

1. ` + "`search_symbols({query: \"X\"})`" + ` — find the symbol ID
2. ` + "`explain_change_impact({symbol_ids: \"<id1>, <id2>\"})`" + ` — risk-tiered blast radius
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

const kiroSteeringRefactor = `---
inclusion: manual
---

# Refactoring with Gortex

## Workflow

1. ` + "`search_symbols({query: \"X\"})`" + ` — find the symbol ID
2. ` + "`explain_change_impact({symbol_ids: \"<id>\"})`" + ` — map blast radius
3. ` + "`get_editing_context({file_path: \"<file>\"})`" + ` — see all symbols and relationships
4. ` + "`find_usages({id: \"<id>\"})`" + ` — every reference to change
5. ` + "`get_edit_plan({symbol_ids: \"<ids>\"})`" + ` — dependency-ordered edit sequence
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

var kiroHooks = map[string]string{
	"gortex-smart-context.json": `{
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
`,
	"gortex-post-edit.json": `{
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
`,
	"gortex-pre-read.json": `{
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
`}

// ─── Antigravity Integration ───────────────────────────────────────────────

func setupAntigravity(root string) error {
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Antigravity integration...\n")
	
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	kiDir := filepath.Join(home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	if err := os.MkdirAll(kiDir, 0o755); err != nil {
		return fmt.Errorf("creating KI dir: %w", err)
	}

	// 1. Create metadata.json
	metadataPath := filepath.Join(kiDir, "metadata.json")
	if err := writeIfNotExists(metadataPath, antigravityMetadata); err != nil {
		return err
	}

	// 2. Create artifacts/gortex-instructions.md
	artifactsDir := filepath.Join(kiDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return fmt.Errorf("creating KI artifacts dir: %w", err)
	}

	instructionsPath := filepath.Join(artifactsDir, "gortex-instructions.md")
	if err := writeIfNotExists(instructionsPath, antigravityInstructions); err != nil {
		return err
	}

	return nil
}

const antigravityMetadata = `{
  "summary": "MANDATORY: Instructions on how to use the local gortex engine CLI to significantly improve codebase intelligence. Antigravity must use run_command with gortex query over standard file read commands.",
  "references": ["artifacts/gortex-instructions.md"]
}
`

const antigravityInstructions = `---
type: "Knowledge Item"
description: "Gortex Workflow and Tools for Antigravity"
---

# Gortex Engine Usage

This codebase is indexed by Gortex, an in-memory knowledge graph code intelligence engine.

**MANDATORY:** You MUST prioritize using the Gortex CLI via ` + "`run_command`" + ` instead of using standard text searches (` + "`grep_search`" + `) or full file reads (` + "`view_file`" + `). This drastically improves token efficiency and prevents hallucination.

## Setup
If the index is not running or tracking your target repos, you can trigger an index manually:
` + "```bash" + `
./gortex index .
` + "```" + `

## Standard Workflow Translation

| Instead of... | You MUST use... (via ` + "`run_command`" + `) |
|---|---|
| ` + "`grep_search`" + ` to find a class or function | ` + "`./gortex query symbol <name> --format text`" + ` (AST-aware search) |
| ` + "`grep_search`" + ` to find all references | ` + "`./gortex query usages <id>`" + ` (zero false positives) |
| ` + "`view_file`" + ` to read a whole file to find a method | ` + "`./gortex query symbol <name>`" + ` or ` + "`./gortex query callers <func_id>`" + ` |
| Guessing what breaks during a refactor | ` + "`./gortex query dependents <id>`" + ` (impact analysis) |
| Creating circular dependencies | Evaluate ` + "`./gortex query deps <id>`" + ` first |

## Example Usage

### 1. View Architecture and Communities
` + "```bash" + `
./gortex query stats
` + "```" + `

### 2. Find specific symbol definition
` + "```bash" + `
./gortex query symbol MyController
` + "```" + `

### 3. Trace blast radius
If you are modifying ` + "`core/parser.go::Parse`" + `, check what will break:
` + "```bash" + `
./gortex query dependents core/parser.go::Parse --depth 2
` + "```" + `

This gives you perfectly accurate AST-level analysis, guaranteeing safe edits.
`

// ─── Generic MCP JSON merge helper ─────────────────────────────────────────

// writeMergeMCPJSON writes or merges the gortex server entry into an MCP JSON
// config file. The file uses the standard {"mcpServers": {...}} format shared by
// Claude Code, Cursor, VS Code / Copilot, Continue.dev, and Cline.
// If extraFields is non-nil, they are merged into the gortex server entry
// (e.g. "alwaysAllow" for Cline).
func writeMergeMCPJSON(path string, extraFields map[string]any) error {
	var config map[string]any

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			config = make(map[string]any)
		}
	} else {
		config = make(map[string]any)
	}

	servers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}

	if _, exists := servers["gortex"]; exists {
		fmt.Fprintf(os.Stderr, "[gortex init] skip %s (gortex server already configured)\n", path)
		return nil
	}

	entry := map[string]any{
		"command": "gortex",
		"args":    []string{"serve", "--index", ".", "--watch"},
		"env":     map[string]string{"GORTEX_INDEX_WORKERS": "8"},
	}
	for k, v := range extraFields {
		entry[k] = v
	}

	servers["gortex"] = entry
	config["mcpServers"] = servers

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] created %s\n", path)
	return nil
}

// ─── Cursor IDE Integration ────────────────────────────────────────────────

// isCursorInstalled checks whether Cursor IDE is present on this system or project.
func isCursorInstalled(root string) bool {
	// 1. Project already has .cursor/ directory.
	if _, err := os.Stat(filepath.Join(root, ".cursor")); err == nil {
		return true
	}
	// 2. User-level Cursor config exists.
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".cursor")); err == nil {
			return true
		}
	}
	// 3. "cursor" CLI is in PATH.
	if p, err := exec.LookPath("cursor"); err == nil && p != "" {
		return true
	}
	return false
}

// setupCursor creates .cursor/mcp.json in the project root.
// Cursor reads MCP config from .cursor/mcp.json (project-level).
func setupCursor(root string) error {
	if !isCursorInstalled(root) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip Cursor setup (Cursor not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Cursor IDE integration...\n")

	cursorDir := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		return fmt.Errorf("creating .cursor: %w", err)
	}

	return writeMergeMCPJSON(filepath.Join(cursorDir, "mcp.json"), nil)
}

// ─── VS Code / GitHub Copilot Integration ──────────────────────────────────

// isVSCodeInstalled checks whether VS Code is present on this system or project.
func isVSCodeInstalled(root string) bool {
	// 1. Project already has .vscode/ directory.
	if _, err := os.Stat(filepath.Join(root, ".vscode")); err == nil {
		return true
	}
	// 2. "code" CLI is in PATH.
	if p, err := exec.LookPath("code"); err == nil && p != "" {
		return true
	}
	// 3. VS Code app data directories exist (macOS / Linux).
	if home, err := os.UserHomeDir(); err == nil {
		candidates := []string{
			filepath.Join(home, "Library", "Application Support", "Code"),       // macOS
			filepath.Join(home, ".config", "Code"),                              // Linux
			filepath.Join(home, ".vscode"),                                      // common
		}
		for _, dir := range candidates {
			if _, err := os.Stat(dir); err == nil {
				return true
			}
		}
	}
	return false
}

// setupVSCodeCopilot creates .vscode/mcp.json in the project root.
// VS Code with GitHub Copilot reads MCP config from .vscode/mcp.json.
func setupVSCodeCopilot(root string) error {
	if !isVSCodeInstalled(root) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip VS Code / Copilot setup (VS Code not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up VS Code / GitHub Copilot integration...\n")

	vscodeDir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(vscodeDir, 0o755); err != nil {
		return fmt.Errorf("creating .vscode: %w", err)
	}

	return writeMergeMCPJSON(filepath.Join(vscodeDir, "mcp.json"), nil)
}

// ─── Windsurf Integration ──────────────────────────────────────────────────

// isWindsurfInstalled checks whether Windsurf (Codeium) is present on this system.
func isWindsurfInstalled() bool {
	// 1. "windsurf" CLI is in PATH.
	if p, err := exec.LookPath("windsurf"); err == nil && p != "" {
		return true
	}
	// 2. Windsurf config directory exists.
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".codeium", "windsurf")); err == nil {
			return true
		}
	}
	return false
}

// setupWindsurf creates or merges gortex into ~/.codeium/windsurf/mcp_config.json.
// Windsurf reads MCP config from this global location only.
func setupWindsurf() error {
	if !isWindsurfInstalled() {
		fmt.Fprintf(os.Stderr, "[gortex init] skip Windsurf setup (Windsurf not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Windsurf integration...\n")

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	windsurfDir := filepath.Join(home, ".codeium", "windsurf")
	if err := os.MkdirAll(windsurfDir, 0o755); err != nil {
		return fmt.Errorf("creating ~/.codeium/windsurf: %w", err)
	}

	return writeMergeMCPJSON(filepath.Join(windsurfDir, "mcp_config.json"), nil)
}

// ─── Continue.dev Integration ──────────────────────────────────────────────

// isContinueInstalled checks whether Continue.dev is present on this system or project.
func isContinueInstalled(root string) bool {
	// 1. Project already has .continue/ directory.
	if _, err := os.Stat(filepath.Join(root, ".continue")); err == nil {
		return true
	}
	// 2. User-level Continue config exists.
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".continue")); err == nil {
			return true
		}
	}
	return false
}

// setupContinue creates .continue/mcpServers/gortex.json in the project root.
// Continue.dev reads JSON MCP configs from .continue/mcpServers/ directory.
func setupContinue(root string) error {
	if !isContinueInstalled(root) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip Continue.dev setup (Continue not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Continue.dev integration...\n")

	continueDir := filepath.Join(root, ".continue", "mcpServers")
	if err := os.MkdirAll(continueDir, 0o755); err != nil {
		return fmt.Errorf("creating .continue/mcpServers: %w", err)
	}

	return writeMergeMCPJSON(filepath.Join(continueDir, "gortex.json"), nil)
}

// ─── Cline Integration ─────────────────────────────────────────────────────

// isClineInstalled checks whether Cline extension is present by looking for its
// globalStorage directories in VS Code or Cursor.
func isClineInstalled(home string) bool {
	candidates := clineSettingsPaths(home)
	for _, path := range candidates {
		dir := filepath.Dir(path)
		if _, err := os.Stat(dir); err == nil {
			return true
		}
	}
	return false
}

// setupCline creates or merges gortex into the Cline MCP settings file.
// Cline stores config in the VS Code globalStorage directory. The location
// varies by OS but follows the pattern:
//   - macOS:  ~/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json
//   - Linux:  ~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json
//
// We also check for Cursor-based Cline installs.
func setupCline() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	if !isClineInstalled(home) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip Cline setup (Cline not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up Cline integration...\n")

	// Cline auto-approve list
	alwaysAllow := []string{
		"graph_stats", "search_symbols", "get_symbol", "get_file_summary",
		"get_editing_context", "get_dependencies", "get_dependents",
		"get_call_chain", "get_callers", "find_implementations", "find_usages",
		"get_cluster", "get_symbol_signature", "get_symbol_source", "batch_symbols",
		"find_import_path", "explain_change_impact", "get_recent_changes",
		"smart_context", "get_edit_plan", "get_test_targets", "suggest_pattern",
		"get_communities", "get_community", "get_processes", "get_process",
		"detect_changes", "index_repository",
		"verify_change", "check_guards", "prefetch_context",
		"find_dead_code", "find_hotspots", "find_cycles", "would_create_cycle",
		"diff_context", "index_health", "get_symbol_history",
		"scaffold", "batch_edit",
	}

	// Candidate paths for Cline settings (VS Code and Cursor variants)
	candidates := clineSettingsPaths(home)

	for _, path := range candidates {
		dir := filepath.Dir(path)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue // skip if the parent globalStorage dir doesn't exist
		}
		if err := writeMergeMCPJSON(path, map[string]any{"alwaysAllow": alwaysAllow}); err != nil {
			fmt.Fprintf(os.Stderr, "[gortex init] warning: could not configure Cline at %s: %v\n", path, err)
		}
	}

	return nil
}

// clineSettingsPaths returns candidate file paths for Cline's MCP settings
// based on the current OS.
func clineSettingsPaths(home string) []string {
	var paths []string

	// VS Code Cline extension
	vscodeGlobalStorage := []string{
		// macOS
		filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
		// Linux
		filepath.Join(home, ".config", "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
	}

	// Cursor Cline extension
	cursorGlobalStorage := []string{
		// macOS
		filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
		// Linux
		filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "saoudrizwan.claude-dev", "settings"),
	}

	for _, dir := range append(vscodeGlobalStorage, cursorGlobalStorage...) {
		paths = append(paths, filepath.Join(dir, "cline_mcp_settings.json"))
	}

	return paths
}

// ─── OpenCode Integration ──────────────────────────────────────────────────

// isOpenCodeInstalled checks whether OpenCode is present on this system or project.
func isOpenCodeInstalled(root string) bool {
	// 1. Project already has .opencode/ directory.
	if _, err := os.Stat(filepath.Join(root, ".opencode")); err == nil {
		return true
	}
	// 2. "opencode" CLI is in PATH.
	if p, err := exec.LookPath("opencode"); err == nil && p != "" {
		return true
	}
	// 3. Global OpenCode config exists.
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".config", "opencode")); err == nil {
			return true
		}
	}
	return false
}

// setupOpenCode creates or merges gortex into .opencode/config.json (project-level).
// OpenCode uses a different config format: {"mcp": {"name": {"type": "local", "command": [...]}}}
func setupOpenCode(root string) error {
	if !isOpenCodeInstalled(root) {
		fmt.Fprintf(os.Stderr, "[gortex init] skip OpenCode setup (OpenCode not detected)\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "[gortex init] setting up OpenCode integration...\n")

	opencodeDir := filepath.Join(root, ".opencode")
	if err := os.MkdirAll(opencodeDir, 0o755); err != nil {
		return fmt.Errorf("creating .opencode: %w", err)
	}

	return writeMergeOpenCodeConfig(filepath.Join(opencodeDir, "config.json"))
}

// writeMergeOpenCodeConfig writes or merges the gortex server into an OpenCode config file.
// OpenCode uses: {"mcp": {"gortex": {"type": "local", "command": [...], "environment": {...}}}}
func writeMergeOpenCodeConfig(path string) error {
	var config map[string]any

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			config = make(map[string]any)
		}
	} else {
		config = make(map[string]any)
	}

	mcpSection, ok := config["mcp"].(map[string]any)
	if !ok {
		mcpSection = make(map[string]any)
	}

	if _, exists := mcpSection["gortex"]; exists {
		fmt.Fprintf(os.Stderr, "[gortex init] skip %s (gortex server already configured)\n", path)
		return nil
	}

	mcpSection["gortex"] = map[string]any{
		"type":    "local",
		"command": []string{"gortex", "serve", "--index", ".", "--watch"},
		"environment": map[string]string{
			"GORTEX_INDEX_WORKERS": "8",
		},
		"enabled": true,
	}
	config["mcp"] = mcpSection

	// Preserve the $schema field if not already set.
	if _, hasSchema := config["$schema"]; !hasSchema {
		config["$schema"] = "https://opencode.ai/config.json"
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex init] created %s\n", path)
	return nil
}
