package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func newGuardsTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "dom", Kind: graph.KindFunction, Name: "Dom", FilePath: "internal/domain/user.go"})
	g.AddNode(&graph.Node{ID: "inf", Kind: graph.KindFunction, Name: "Inf", FilePath: "internal/infra/db.go"})
	g.AddEdge(&graph.Edge{From: "dom", To: "inf", Kind: graph.EdgeCalls})
	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callCheckGuards(t *testing.T, s *Server, ids string) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"ids": ids}
	res, err := s.handleCheckGuards(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestCheckGuards_ArchitectureLayerViolation(t *testing.T) {
	s := newGuardsTestServer(t)
	s.SetArchitecture(config.ArchitectureConfig{
		Layers: map[string]config.LayerRule{
			"domain": {Paths: []string{"internal/domain/**"}, Deny: []string{"*"}},
			"infra":  {Paths: []string{"internal/infra/**"}, Allow: []string{"domain"}},
		},
	})
	out := callCheckGuards(t, s, "dom")
	violations, _ := out["violations"].([]any)
	require.Len(t, violations, 1)
	v, _ := violations[0].(map[string]any)
	require.Equal(t, "layer", v["kind"])
	require.Equal(t, "domain", v["layer_from"])
	require.Equal(t, "infra", v["layer_to"])
}

func TestCheckGuards_NoRulesNoArchitecture(t *testing.T) {
	s := newGuardsTestServer(t)
	out := callCheckGuards(t, s, "dom")
	// No flat rules and no architecture block — the explicit
	// "nothing configured" message, not a violation list.
	require.Equal(t, "no guard rules configured", out["message"])
}
