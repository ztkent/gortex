package mcp

import (
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// toolBudgetDisableEnv turns off the exploration-call budget hint when set
// to a falsey value (0 / off / false / no).
const toolBudgetDisableEnv = "GORTEX_TOOL_BUDGET"

// budgetAnnotatedTools is the set of read / navigation tools whose
// descriptions carry the exploration-call budget. These are the tools an
// agent can loop on indefinitely while exploring; edit, analysis, and
// one-shot tools are deliberately excluded.
var budgetAnnotatedTools = map[string]bool{
	"search_symbols":      true,
	"smart_context":       true,
	"get_symbol":          true,
	"get_symbol_source":   true,
	"get_editing_context": true,
	"get_file_summary":    true,
	"get_repo_outline":    true,
	"find_usages":         true,
	"get_callers":         true,
	"get_call_chain":      true,
	"get_dependencies":    true,
	"get_dependents":      true,
	"read_file":           true,
}

// budgetForNodeCount maps repo size (graph node count) to a soft ceiling
// on exploration calls. Buckets are deliberately coarse: the number is a
// self-throttle hint for the agent, not an enforced limit.
func budgetForNodeCount(nodes int) int {
	switch {
	case nodes < 2_000:
		return 8
	case nodes < 10_000:
		return 14
	case nodes < 40_000:
		return 22
	case nodes < 120_000:
		return 30
	default:
		return 40
	}
}

// toolBudgetSuffix returns the sentence appended to exploration tools'
// descriptions — a project-size-scaled cap on exploration calls so the
// model self-throttles. Computed once from the graph node count; empty
// when disabled via GORTEX_TOOL_BUDGET.
func (s *Server) toolBudgetSuffix() string {
	s.toolBudgetOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(toolBudgetDisableEnv))) {
		case "0", "off", "false", "no":
			return
		}
		nodes := 0
		if s.graph != nil {
			nodes = s.graph.NodeCount()
		}
		budget := budgetForNodeCount(nodes)
		if nodes > 0 {
			s.toolBudgetCached = fmt.Sprintf(
				" Exploration budget: this repo indexes ~%d nodes — aim to make at most %d exploration calls before acting on what you have found.",
				nodes, budget)
		} else {
			s.toolBudgetCached = fmt.Sprintf(
				" Exploration budget: aim to make at most %d exploration calls before acting on what you have found.",
				budget)
		}
	})
	return s.toolBudgetCached
}

// annotateToolBudget appends the exploration-call budget to an exploration
// tool's description so the cap rides in the schema the model reads every
// turn. No-op for non-exploration tools and when the hint is disabled.
func (s *Server) annotateToolBudget(tool *mcp.Tool) {
	if tool == nil || !budgetAnnotatedTools[tool.Name] {
		return
	}
	suffix := s.toolBudgetSuffix()
	if suffix == "" {
		return
	}
	tool.Description = strings.TrimRight(tool.Description, " ") + suffix
}
