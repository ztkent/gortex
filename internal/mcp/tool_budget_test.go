package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBudgetForNodeCount_ScalesWithRepoSize(t *testing.T) {
	require.Equal(t, 8, budgetForNodeCount(0))
	require.Equal(t, 8, budgetForNodeCount(1999))
	require.Equal(t, 14, budgetForNodeCount(2000))
	require.Equal(t, 40, budgetForNodeCount(500_000))
	// The budget must be monotone non-decreasing in repo size.
	prev := 0
	for _, n := range []int{0, 5_000, 25_000, 80_000, 300_000} {
		b := budgetForNodeCount(n)
		require.GreaterOrEqualf(t, b, prev, "budget must not shrink as the repo grows (n=%d)", n)
		prev = b
	}
}

// TestToolBudget_AnnotatesExplorationTools verifies the exploration-call
// budget rides in the description of navigation tools (so the model reads
// the self-throttle hint each turn) but not in edit tools.
func TestToolBudget_AnnotatesExplorationTools(t *testing.T) {
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()

	ss, ok := live["search_symbols"]
	require.True(t, ok, "search_symbols must be live")
	require.Contains(t, ss.Tool.Description, "Exploration budget",
		"an exploration tool's description must carry the call budget")
	require.Contains(t, ss.Tool.Description, "exploration calls")

	if wf, ok := live["write_file"]; ok {
		require.NotContains(t, wf.Tool.Description, "Exploration budget",
			"an edit tool must not carry the exploration budget hint")
	}
}

// TestToolBudget_DisabledByEnv confirms GORTEX_TOOL_BUDGET=0 suppresses
// the hint entirely.
func TestToolBudget_DisabledByEnv(t *testing.T) {
	t.Setenv(toolBudgetDisableEnv, "0")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	ss, ok := live["search_symbols"]
	require.True(t, ok)
	require.NotContains(t, ss.Tool.Description, "Exploration budget",
		"the budget hint must be suppressed when disabled by env")
}
