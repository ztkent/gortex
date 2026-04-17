package hooks

import (
	"strings"
)

// enrichTask produces a condensed graph-orientation briefing for a Task
// (subagent) spawn. Claude Code's `Task` tool receives PreToolUse
// additionalContext before the subagent begins, so this is the hook point
// for "subagent start".
//
// The briefing combines:
//   - repo orientation (graph_stats)
//   - task-relevant symbols (smart_context over description + prompt)
//   - recently-modified symbols from this session (get_symbol_history)
//
// Returns an empty result when the bridge is unreachable or when there is no
// meaningful task text to derive context from. The hook must degrade silently
// and never block subagent spawning.
func enrichTask(toolInput map[string]any, port int) enrichResult {
	description, _ := toolInput["description"].(string)
	prompt, _ := toolInput["prompt"].(string)

	task := strings.TrimSpace(description + "\n" + prompt)
	if task == "" {
		return enrichResult{}
	}
	// Cap the task text we send to the bridge — full prompts can be huge.
	const maxTaskLen = 2000
	if len(task) > maxTaskLen {
		task = task[:maxTaskLen]
	}

	stats := callBridgeTool(port, "graph_stats", nil)
	if stats == "" {
		// Bridge unreachable — silent.
		return enrichResult{}
	}

	var sb strings.Builder
	sb.WriteString("[Gortex] Subagent briefing — this repo has a Gortex MCP server.\n")
	sb.WriteString("Subagents don't inherit CLAUDE.md, so the rules below are restated inline:\n\n")

	sb.WriteString(gortexToolGuidance)
	sb.WriteString("\n")

	if summary := renderStatsSummary(stats); summary != "" {
		sb.WriteString("**Index:** ")
		sb.WriteString(summary)
		sb.WriteString("\n\n")
	}

	if ctx := renderTaskContext(port, task); ctx != "" {
		sb.WriteString("### Relevant Symbols (from `smart_context`)\n\n")
		sb.WriteString(ctx)
		sb.WriteString("\n")
	}

	if churn := renderSymbolHistory(port); churn != "" {
		sb.WriteString("### Recently Modified (this session)\n\n")
		sb.WriteString(churn)
		sb.WriteString("\n")
	}

	sb.WriteString("_First call: `smart_context` with your task description. Before editing any file: `get_editing_context`. Never Read/Grep an indexed source file._\n")
	sb.WriteString("_For list-shaped responses (search_symbols, find_usages, analyze, batch_symbols, get_callers), pass `format:\"gcx\"` to save ~27% tokens — round-trippable, spec at docs/wire-format.md._\n")

	return enrichResult{context: sb.String()}
}

// gortexToolGuidance is the condensed tool-swap reference injected into every
// subagent briefing. Kept short (~14 lines) so the token overhead per Task
// spawn stays small; the full table lives in CLAUDE.md for parent-agent use.
const gortexToolGuidance = "### Use Gortex MCP tools instead of Read/Grep/Glob\n" +
	"\n" +
	"| Instead of...                    | Use...                                |\n" +
	"|----------------------------------|---------------------------------------|\n" +
	"| `Read` a whole source file       | `get_symbol_source` (one symbol)      |\n" +
	"| `Read` to understand a file      | `get_editing_context` / `get_file_summary` |\n" +
	"| `Grep` for a symbol              | `search_symbols` (BM25, camelCase)    |\n" +
	"| `Grep` for references            | `find_usages` (zero false positives)  |\n" +
	"| `Grep` to find callers           | `get_callers` / `get_call_chain`      |\n" +
	"| `Glob` over source files         | `search_symbols` (returns file paths) |\n" +
	"| Many Read calls to explore       | `smart_context` (one call)            |\n" +
	"| Reading to pick tests to run     | `get_test_targets`                    |\n" +
	"\n" +
	"**Token tip:** 13 tools accept `format:\"gcx\"` for compact round-trippable output (~27% fewer tokens). Pass it on any list-shaped query: `search_symbols`, `find_usages`, `analyze`, `contracts`, `batch_symbols`, `get_callers`/`get_call_chain`/`get_dependencies`/`get_dependents`/`find_implementations`, `get_file_summary`, `get_editing_context`, `smart_context`.\n"

// renderTaskContext calls smart_context with the subagent task text and
// returns a compacted body. Falls back to empty on any error.
func renderTaskContext(port int, task string) string {
	raw := callBridgeTool(port, "smart_context", map[string]any{
		"task":    task,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return cappedLines(raw, 12)
}
