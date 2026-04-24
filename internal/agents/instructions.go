// Instructions shared across every doc-aware adapter. Centralising the
// body here avoids per-adapter drift: Cursor's .cursor/rules file,
// Copilot's .github/copilot-instructions.md, Codex's AGENTS.md, and
// Claude Code's CLAUDE.md all read from the same constant, so when the
// "prefer Gortex over Read/Grep" story evolves we update it once and
// every agent sees the change on the next `gortex init`.
//
// The claudecode adapter extends this body with its own slash-commands
// appendix — that part is Claude-Code-specific and lives in
// claudecode/content.go, keyed off the same sentinel so idempotency
// checks line up across adapters.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstructionsSentinel is the substring every doc-aware adapter checks
// for when deciding whether to append the instructions block. If it's
// already present (wherever it came from — a prior `gortex init`, a
// user-copied block, another adapter writing to a shared rules file
// like AGENTS.md) we skip to stay idempotent.
const InstructionsSentinel = "## MANDATORY: Use Gortex MCP tools"

// CommunitiesStartMarker / CommunitiesEndMarker fence the generated
// community-routing block that `gortex init` writes into per-repo
// instructions files. Fenced (not just start-only) because this block
// is regenerated on every `init` re-run as the codebase evolves, so
// we need to identify and overwrite it precisely without clobbering
// user edits around it.
const (
	CommunitiesStartMarker = "<!-- gortex:communities:start -->"
	CommunitiesEndMarker   = "<!-- gortex:communities:end -->"
)

// InstructionsBody is the shared rule block every adapter writes to
// its agent's instructions file. Tool names in the tables (Read, Grep)
// are Claude-Code-specific flavour; models outside Claude Code read
// them as "any file-reading tool" — the principle stays the same so
// we keep one body rather than branch by agent.
const InstructionsBody = `## MANDATORY: Use Gortex MCP tools instead of Read/Grep

Gortex is running as an MCP server. You MUST use graph queries instead of file reads whenever possible. This saves thousands of tokens per task.

### Navigation and Reading

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| ` + "`Read`" + ` a whole file for one function  | ` + "`get_symbol_source`" + ` with ` + "`id: \"path/to/file.go::SymbolName\"`" + ` (80% fewer tokens) — use ` + "`get_file_summary`" + ` first if you don't know the symbol name |
| ` + "`Read`" + ` to find a function             | ` + "`get_symbol`" + ` or ` + "`get_editing_context`" + `    |
| Multiple ` + "`get_symbol`" + ` calls           | ` + "`batch_symbols`" + ` (one call for N symbols) |
| ` + "`Grep`" + ` for references                 | ` + "`find_usages`" + ` (zero false positives)     |
| ` + "`Grep`" + ` to find a symbol by name       | ` + "`search_symbols`" + ` (BM25 + camelCase-aware)|
| Filtering ` + "`search_symbols`" + ` by hand    | ` + "`winnow_symbols`" + ` — structured constraint chain (kind, language, community, path_prefix, min_fan_in, min_churn) with per-axis score contributions |
| ` + "`Read`" + ` to understand a file           | ` + "`get_file_summary`" + ` or ` + "`get_editing_context`" + ` |
| ` + "`Read`" + ` multiple files to trace calls  | ` + "`get_call_chain`" + ` / ` + "`get_callers`" + `         |
| Guessing an import path               | ` + "`find_import_path`" + `                       |
| ` + "`Read`" + ` to check a function signature  | ` + "`get_symbol`" + ` (signature is in ` + "`meta.signature`" + `) |
| 5-10 calls to explore for a task      | ` + "`smart_context`" + ` (one call)               |

### Impact Analysis and Safety

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Reading files to assess change scope  | ` + "`explain_change_impact`" + ` (includes cross-community warnings) |
| Guessing which tests to run           | ` + "`get_test_targets`" + `                       |
| Manual dependency ordering            | ` + "`get_edit_plan`" + `                          |
| Hoping signature changes are safe     | ` + "`verify_change`" + ` — checks callers and interface implementors |
| Manually checking team conventions    | ` + "`check_guards`" + ` — evaluates guard rules from .gortex.yaml |
| Wondering if a new dep creates a cycle| ` + "`analyze`" + ` with ` + "`kind: \"would_create_cycle\"`" + ` — checks before you add it |

### Code Quality and Analysis

| Instead of...                         | You MUST use...                          |
|---------------------------------------|------------------------------------------|
| Manually hunting unused code          | ` + "`analyze`" + ` with ` + "`kind: \"dead_code\"`" + ` — zero incoming edges (excludes entry points, tests, exports) |
| Guessing which symbols are over-coupled| ` + "`analyze`" + ` with ` + "`kind: \"hotspots\"`" + ` — ranks by fan-in, fan-out, community crossings |
| Manually scanning for circular deps   | ` + "`analyze`" + ` with ` + "`kind: \"cycles\"`" + ` — Tarjan's SCC with severity classification |
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
| Manually tracking API routes/services | ` + "`contracts`" + ` (default ` + "`action: \"list\"`" + `) — lists HTTP, gRPC, GraphQL, topic, WebSocket, env, OpenAPI; filter by ` + "`repo`" + `, ` + "`project`" + `, or ` + "`ref`" + ` |
| Guessing if APIs match across repos   | ` + "`contracts`" + ` with ` + "`action: \"check\"`" + ` — detects orphan providers/consumers and mismatches; scope with ` + "`repo`" + ` / ` + "`project`" + ` / ` + "`ref`" + ` |

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
`

// AppendInstructions appends body to path, creating the file if
// missing. Idempotent: when `sentinel` is already present anywhere in
// the file we skip with ActionSkip and log the reason. Callers pass
// the adapter's ApplyOpts through so --dry-run / --global / --force
// all flow to the right FileAction status.
//
// Not atomic. Rules files are plaintext a human edits, matching the
// historical CLAUDE.md append behaviour — a concurrent external writer
// during init is extraordinarily unlikely and atomic rename of a file
// a human is editing would fight their editor.
func AppendInstructions(w io.Writer, path, body, sentinel string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	if existed && strings.Contains(string(existing), sentinel) {
		if w != nil {
			_, _ = fmt.Fprintf(w, "[gortex init] skip %s (Gortex block already present)\n", path)
		}
		return FileAction{Path: path, Action: ActionSkip, Reason: "block-present"}, nil
	}

	if opts.DryRun {
		action := ActionWouldMerge
		if !existed {
			action = ActionWouldCreate
		}
		return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
	}

	// Two blank lines between existing content and the block so the
	// appended section reads as a separate document and doesn't glue
	// onto the last paragraph the user wrote.
	prefix := ""
	if existed && len(existing) > 0 {
		prefix = "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return FileAction{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(prefix + body); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		_, _ = fmt.Fprintf(w, "[gortex init] appended Gortex block to %s\n", path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"gortex-block"}}, nil
}

// CursorMDCFrontmatter wraps the instructions body in the YAML
// frontmatter Cursor expects for MDC rules files. Cursor reads
// `alwaysApply: true` rules on every chat turn — which is what we
// want for the MANDATORY-prefer-Gortex block.
//
// Kept separate from AppendInstructions because MDC files are
// one-rule-per-file (Cursor owns the filename, not the content), so
// they use WriteIfNotExists semantics, not append.
func CursorMDCFrontmatter(body string) string {
	return `---
description: Gortex code intelligence — prefer graph tools over file reads
alwaysApply: true
---

` + body
}

// UpsertMarkedBlock writes `body` into `path` between `startMarker`
// and `endMarker`. Unlike AppendInstructions, this is idempotent AND
// regeneratable: if the markers already exist the block between them
// is replaced; otherwise the block is appended with a blank-line gap
// to existing content. If `body` is empty and the markers exist, the
// block is removed (migration use case). Creates the file if missing.
//
// Designed for the per-repo community-routing block which regenerates
// on every `gortex init` run as the graph evolves.
func UpsertMarkedBlock(w io.Writer, path, body, startMarker, endMarker string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}
	existed := readErr == nil
	text := ""
	if existed {
		text = string(existing)
	}

	hasBlock := existed && strings.Contains(text, startMarker) && strings.Contains(text, endMarker)
	empty := strings.TrimSpace(body) == ""

	// Nothing to do: empty body and no existing block.
	if empty && !hasBlock {
		return FileAction{Path: path, Action: ActionSkip, Reason: "no-communities"}, nil
	}

	fenced := startMarker + "\n" + body + "\n" + endMarker + "\n"

	var next string
	switch {
	case hasBlock:
		start := strings.Index(text, startMarker)
		end := strings.Index(text, endMarker) + len(endMarker)
		// Trim trailing newline after the end marker so we don't
		// accumulate blank lines on repeated re-runs.
		if end < len(text) && text[end] == '\n' {
			end++
		}
		if empty {
			next = text[:start] + text[end:]
		} else {
			next = text[:start] + fenced + text[end:]
		}
	case !existed:
		next = fenced
	default:
		prefix := ""
		if len(text) > 0 {
			if !strings.HasSuffix(text, "\n") {
				prefix = "\n\n"
			} else if !strings.HasSuffix(text, "\n\n") {
				prefix = "\n"
			}
		}
		next = text + prefix + fenced
	}

	// Skip when the file would end up byte-identical to what's
	// already there — important for AssertIdempotent semantics and
	// for avoiding spurious mtime bumps on `gortex init` re-runs
	// when the graph hasn't changed.
	if existed && next == text {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		switch {
		case !existed:
			return FileAction{Path: path, Action: ActionWouldCreate, Keys: []string{"communities-block"}}, nil
		case hasBlock:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		default:
			return FileAction{Path: path, Action: ActionWouldMerge, Keys: []string{"communities-block"}}, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, err
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return FileAction{}, err
	}
	if w != nil {
		verb := "updated"
		if !existed {
			verb = "wrote"
		}
		_, _ = fmt.Fprintf(w, "[gortex init] %s %s (communities block)\n", verb, path)
	}
	action := ActionMerge
	if !existed {
		action = ActionCreate
	}
	return FileAction{Path: path, Action: action, Keys: []string{"communities-block"}}, nil
}
