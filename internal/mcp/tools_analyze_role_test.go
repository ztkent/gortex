package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func newRoleTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()

	// dead: no edges.
	g.AddNode(&graph.Node{ID: "p/dead.go::Dead", Name: "Dead", Kind: graph.KindFunction, FilePath: "p/dead.go", StartLine: 1, EndLine: 3})

	// entry: no in-edges, ≥1 out-edge.
	g.AddNode(&graph.Node{ID: "p/main.go::Run", Name: "Run", Kind: graph.KindFunction, FilePath: "p/main.go", StartLine: 1, EndLine: 5})
	g.AddNode(&graph.Node{ID: "p/svc.go::Process", Name: "Process", Kind: graph.KindFunction, FilePath: "p/svc.go", StartLine: 1, EndLine: 50})
	g.AddEdge(&graph.Edge{From: "p/main.go::Run", To: "p/svc.go::Process", Kind: graph.EdgeCalls})

	// leaf: ≥1 in-edge, no out-edge.
	g.AddNode(&graph.Node{ID: "p/util.go::Leaf", Name: "Leaf", Kind: graph.KindFunction, FilePath: "p/util.go", StartLine: 1, EndLine: 5})
	g.AddEdge(&graph.Edge{From: "p/svc.go::Process", To: "p/util.go::Leaf", Kind: graph.EdgeCalls})

	// utility: high fan-in, ≤2 fan-out (≥1 to dodge the leaf rule), ≤30 lines.
	g.AddNode(&graph.Node{ID: "p/util.go::Format", Name: "Format", Kind: graph.KindFunction, FilePath: "p/util.go", StartLine: 10, EndLine: 20})
	g.AddNode(&graph.Node{ID: "p/util.go::sprintf", Name: "sprintf", Kind: graph.KindFunction, FilePath: "p/util.go", StartLine: 30, EndLine: 40})
	g.AddEdge(&graph.Edge{From: "p/util.go::Format", To: "p/util.go::sprintf", Kind: graph.EdgeCalls})
	for i := range 5 {
		callerID := "p/cN" + string(rune('A'+i)) + ".go::C" + string(rune('A'+i))
		g.AddNode(&graph.Node{ID: callerID, Name: "C" + string(rune('A'+i)), Kind: graph.KindFunction, FilePath: "p/cN" + string(rune('A'+i)) + ".go", StartLine: 1, EndLine: 10})
		g.AddEdge(&graph.Edge{From: callerID, To: "p/util.go::Format", Kind: graph.EdgeCalls})
	}

	// core: anything else. Use Process which has 1 in (Run) + 1 out (Leaf), large.
	// Process is already "core" by elimination.

	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callAnalyzeRole(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyzeRole(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func rolesByID(out map[string]any) map[string]string {
	m := map[string]string{}
	syms, _ := out["symbols"].([]any)
	for _, s := range syms {
		row := s.(map[string]any)
		m[row["symbol_id"].(string)] = row["role"].(string)
	}
	return m
}

func TestAnalyzeRole_AllSixClasses(t *testing.T) {
	s := newRoleTestServer(t)
	out := callAnalyzeRole(t, s, map[string]any{})

	got := rolesByID(out)
	assert.Equal(t, "dead", got["p/dead.go::Dead"])
	assert.Equal(t, "entry", got["p/main.go::Run"])
	assert.Equal(t, "leaf", got["p/util.go::Leaf"])
	assert.Equal(t, "utility", got["p/util.go::Format"])
	assert.Equal(t, "core", got["p/svc.go::Process"])
}

func TestAnalyzeRole_AdapterDetectedViaCommunities(t *testing.T) {
	s := newRoleTestServer(t)
	// Mark Process as an adapter: callers in c-edge, callees in c-core.
	s.analysisMu.Lock()
	s.communities = &analysis.CommunityResult{
		NodeToComm: map[string]string{
			"p/main.go::Run":      "c-edge",
			"p/svc.go::Process":   "c-mid",
			"p/util.go::Leaf":     "c-core",
		},
	}
	s.analysisMu.Unlock()

	out := callAnalyzeRole(t, s, map[string]any{})
	got := rolesByID(out)
	assert.Equal(t, "adapter", got["p/svc.go::Process"], "incoming from c-edge, outgoing to c-core")
}

func TestAnalyzeRole_FilterByRole(t *testing.T) {
	s := newRoleTestServer(t)
	out := callAnalyzeRole(t, s, map[string]any{"role": "entry"})

	syms, _ := out["symbols"].([]any)
	for _, sym := range syms {
		assert.Equal(t, "entry", sym.(map[string]any)["role"])
	}
}

func TestAnalyzeRole_TallyMatchesUnfilteredTotal(t *testing.T) {
	s := newRoleTestServer(t)
	out := callAnalyzeRole(t, s, map[string]any{})
	tally := out["tally_by_role"].(map[string]any)

	sum := 0
	for _, v := range tally {
		sum += int(v.(float64))
	}
	syms, _ := out["symbols"].([]any)
	assert.Equal(t, sum, len(syms), "tally sums to total symbols returned")
}

func TestAnalyzeRole_PathPrefixScope(t *testing.T) {
	s := newRoleTestServer(t)
	out := callAnalyzeRole(t, s, map[string]any{"path_prefix": "p/util"})
	syms, _ := out["symbols"].([]any)
	for _, sym := range syms {
		assert.Contains(t, sym.(map[string]any)["file"], "p/util")
	}
}

func TestClassifyRole_DirectCases(t *testing.T) {
	n := &graph.Node{ID: "x", StartLine: 1, EndLine: 5}
	g := graph.New()
	g.AddNode(n)

	assert.Equal(t, "dead", classifyRole(n, 0, 0, g, nil))
	assert.Equal(t, "entry", classifyRole(n, 0, 3, g, nil))
	assert.Equal(t, "leaf", classifyRole(n, 4, 0, g, nil))
	// utility: fan-in≥3, fan-out≤2, lines≤30.
	utilNode := &graph.Node{ID: "u", StartLine: 1, EndLine: 10}
	g.AddNode(utilNode)
	assert.Equal(t, "utility", classifyRole(utilNode, 5, 1, g, nil))
	// core: doesn't match utility thresholds.
	coreNode := &graph.Node{ID: "c", StartLine: 1, EndLine: 100}
	g.AddNode(coreNode)
	assert.Equal(t, "core", classifyRole(coreNode, 5, 1, g, nil))
}

func TestCountCallEdges_FiltersNonCall(t *testing.T) {
	edges := []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls},
		{From: "a", To: "b", Kind: graph.EdgeCalls}, // dup
		{From: "a", To: "c", Kind: graph.EdgeCalls},
		{From: "f", To: "b", Kind: graph.EdgeDefines}, // structural
	}
	assert.Equal(t, 2, countCallEdges(edges))
}

func TestAnalyzeRole_IntegrationViaDispatch(t *testing.T) {
	s := newRoleTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": "role"}
	res, err := s.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, "analyze kind=role must dispatch")
}
