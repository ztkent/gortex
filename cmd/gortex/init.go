package main

import (
	"fmt"
	"os"
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
	Short: "Set up Gortex for a project: creates .mcp.json, .claude/commands/, and CLAUDE.md block",
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

	fmt.Fprintf(os.Stderr, "[gortex init] done — created:\n")
	fmt.Fprintf(os.Stderr, "  .mcp.json                     (MCP server config)\n")
	fmt.Fprintf(os.Stderr, "  .claude/commands/gortex-*.md   (slash commands)\n")
	fmt.Fprintf(os.Stderr, "  CLAUDE.md                      (Gortex instructions block)\n")
	fmt.Fprintf(os.Stderr, "\nCommit these files so your team gets Gortex automatically.\n")
	fmt.Fprintf(os.Stderr, "Run `gortex serve --index . --watch` or let Claude Code start it via .mcp.json.\n")
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
	defer f.Close()

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
	defer logger.Sync()

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

const mcpJSON = `{
  "mcpServers": {
    "gortex": {
      "command": "gortex",
      "args": [
        "serve",
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

const claudeMdBlock = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex is running as an MCP server. You MUST use graph queries instead of file reads whenever possible. This saves thousands of tokens per task.

**Before editing any file:** call ` + "`get_editing_context(\"<file>\")`" + ` first. This replaces 5-10 Read calls with a single ~250 token response.

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` to find a function             | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find a symbol by name       | ` + "`search_symbols`" + `                         |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` |
| ` + "`Read`" + ` multiple files to trace calls  | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Guessing an import path               | ` + "`find_import_path`" + `                       |
| Reading files to assess change scope  | ` + "`explain_change_impact`" + `                  |
| ` + "`Read`" + ` to check a function signature  | ` + "`get_symbol_signature`" + `                   |

## Session start (Gortex)

1. Call ` + "`graph_stats`" + ` to confirm Gortex is running and get repo orientation.
2. If ` + "`total_nodes`" + ` is 0, call ` + "`index_repository`" + ` with path ` + "`\".\"`" + `.
3. For every file you are about to edit, call ` + "`get_editing_context`" + ` first.
4. Before any refactor affecting a shared type or function, call ` + "`explain_change_impact`" + `.
5. Before committing, call ` + "`detect_changes`" + ` to verify scope.

## Gortex slash commands

Use these for guided workflows: ` + "`/gortex-guide`" + `, ` + "`/gortex-explore`" + `, ` + "`/gortex-debug`" + `, ` + "`/gortex-impact`" + `, ` + "`/gortex-refactor`" + `
`

var commands = map[string]string{
	"gortex-guide.md": commandGuide,
	"gortex-explore.md": commandExplore,
	"gortex-debug.md": commandDebug,
	"gortex-impact.md": commandImpact,
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
| search_symbols | Find symbols by name (substring match). Use instead of Grep |
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
| find_import_path | Correct import path for a symbol in a target file |
| explain_change_impact | Risk-tiered blast radius with affected processes/communities |
| get_recent_changes | Files/symbols changed since timestamp (watch mode) |

### Analysis
| Tool | What it gives you |
|------|-------------------|
| get_communities | Functional clusters via Louvain community detection |
| get_community | Members, files, cohesion for one community |
| get_processes | Discovered execution flows (entry points -> call chains) |
| get_process | Full step-by-step trace of one execution flow |
| detect_changes | Git diff -> affected symbols -> blast radius |

## Graph Schema

**Node kinds:** file, function, method, type, interface, variable, import, package
**Edge kinds:** calls, imports, defines, implements, extends, references, member_of, instantiates
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
- find_usages to get every reference location
- get_editing_context on each affected file
- Edit in dependency order: definition -> callers -> tests
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
