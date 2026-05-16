package mcp

import (
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// flowBetweenJSON is the minimal shape of the JSON response returned by
// handleFlowBetween — declared here so the tier-surfacing tests can
// assert on EdgeStep fields without depending on the full dataflow
// package internals.
type flowBetweenJSON struct {
	Total int `json:"total"`
	Paths []struct {
		Confidence float64 `json:"confidence"`
		Edges      []struct {
			From   string `json:"from"`
			To     string `json:"to"`
			Kind   string `json:"kind"`
			Origin string `json:"origin"`
			Tier   string `json:"tier"`
		} `json:"edges"`
	} `json:"paths"`
}

// TestTier_FlowBetweenSurfacesPerStepTier asserts that every EdgeStep in
// the JSON response from flow_between carries both the raw Origin tier
// and the coarse `tier` label (ast / lsp / heuristic). This is the
// dataflow half of the N1 per-edge resolver-provenance contract.
func TestTier_FlowBetweenSurfacesPerStepTier(t *testing.T) {
	srv := dataflowTestServer(t)
	driverID := findFunctionID(t, srv, "Driver")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source_id": driverID + "#param:input",
		"sink_id":   findFunctionID(t, srv, "Sink") + "#param:payload",
		"max_depth": float64(10),
	}
	res, err := srv.handleFlowBetween(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var payload flowBetweenJSON
	text := res.Content[0].(mcplib.TextContent).Text
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.Greater(t, payload.Total, 0, "fixture should produce at least one path; got: %s", text)

	validTiers := map[string]bool{"lsp": true, "ast": true, "heuristic": true}
	for _, p := range payload.Paths {
		require.Greater(t, len(p.Edges), 0)
		for _, e := range p.Edges {
			assert.NotEmpty(t, e.Origin, "step %s→%s must carry Origin", e.From, e.To)
			assert.NotEmpty(t, e.Tier, "step %s→%s must carry Tier", e.From, e.To)
			assert.True(t, validTiers[e.Tier],
				"tier %q must be one of lsp/ast/heuristic", e.Tier)
			assert.Equal(t, graph.ResolvedBy(e.Origin), e.Tier,
				"tier must equal ResolvedBy(origin) for step %s→%s", e.From, e.To)
		}
	}
}

// TestTier_FlowBetweenMinTierFiltersTraversal asserts that the new
// min_tier param prunes edges below the requested tier during BFS. The
// fixture has no LSP enrichment so min_tier=lsp_resolved drops every
// path; min_tier=ast_resolved preserves the AST-grade paths.
func TestTier_FlowBetweenMinTierFiltersTraversal(t *testing.T) {
	srv := dataflowTestServer(t)
	driverID := findFunctionID(t, srv, "Driver")
	args := func(minTier string) map[string]any {
		m := map[string]any{
			"source_id": driverID + "#param:input",
			"sink_id":   findFunctionID(t, srv, "Sink") + "#param:payload",
			"max_depth": float64(10),
		}
		if minTier != "" {
			m["min_tier"] = minTier
		}
		return m
	}

	unfilteredReq := mcplib.CallToolRequest{}
	unfilteredReq.Params.Arguments = args("")
	unfilteredRes, err := srv.handleFlowBetween(t.Context(), unfilteredReq)
	require.NoError(t, err)
	require.False(t, unfilteredRes.IsError)
	var unfiltered flowBetweenJSON
	require.NoError(t, json.Unmarshal([]byte(unfilteredRes.Content[0].(mcplib.TextContent).Text), &unfiltered))
	require.Greater(t, unfiltered.Total, 0, "baseline should yield paths")

	astReq := mcplib.CallToolRequest{}
	astReq.Params.Arguments = args(graph.OriginASTResolved)
	astRes, err := srv.handleFlowBetween(t.Context(), astReq)
	require.NoError(t, err)
	require.False(t, astRes.IsError)
	var ast flowBetweenJSON
	require.NoError(t, json.Unmarshal([]byte(astRes.Content[0].(mcplib.TextContent).Text), &ast))
	for _, p := range ast.Paths {
		for _, e := range p.Edges {
			assert.True(t, graph.MeetsMinTier(e.Origin, graph.OriginASTResolved),
				"min_tier=ast_resolved leaked step origin=%s", e.Origin)
		}
	}
	assert.LessOrEqual(t, ast.Total, unfiltered.Total,
		"AST-filter must not yield more paths than unfiltered")

	lspReq := mcplib.CallToolRequest{}
	lspReq.Params.Arguments = args(graph.OriginLSPResolved)
	lspRes, err := srv.handleFlowBetween(t.Context(), lspReq)
	require.NoError(t, err)
	require.False(t, lspRes.IsError)
	var lsp flowBetweenJSON
	require.NoError(t, json.Unmarshal([]byte(lspRes.Content[0].(mcplib.TextContent).Text), &lsp))
	for _, p := range lsp.Paths {
		for _, e := range p.Edges {
			assert.True(t, graph.MeetsMinTier(e.Origin, graph.OriginLSPResolved),
				"min_tier=lsp_resolved leaked step origin=%s", e.Origin)
		}
	}
	assert.LessOrEqual(t, lsp.Total, ast.Total,
		"LSP-filter must not yield more paths than AST-filter")
}

// TestTier_FlowBetweenGCXEmitsTierColumns asserts the GCX1 encoder
// surfaces the per-step `tiers` and `origins` sequences plus the
// per-path `worst_tier` field — the wire-format columns N1 promises.
func TestTier_FlowBetweenGCXEmitsTierColumns(t *testing.T) {
	srv := dataflowTestServer(t)
	driverID := findFunctionID(t, srv, "Driver")
	req := mcplib.CallToolRequest{}
	req.Params.Name = "flow_between"
	req.Params.Arguments = map[string]any{
		"source_id": driverID + "#param:input",
		"sink_id":   findFunctionID(t, srv, "Sink") + "#param:payload",
		"format":    "gcx",
	}
	res, err := srv.handleFlowBetween(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "worst_tier")
	require.Contains(t, text, "tiers")
	require.Contains(t, text, "origins")
}
