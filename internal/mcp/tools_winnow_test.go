package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// buildWinnowGraph creates a tiny deterministic graph with four functions
// across two files and language tags. Edges are shaped so fan-in /
// fan-out differ per node, which lets tests assert ordering by the
// structural axes.
func buildWinnowGraph(t *testing.T) *Server {
	t.Helper()

	g := graph.New()
	addFn := func(id, name, file, lang string) {
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction,
			Name: name, FilePath: file, Language: lang,
			StartLine: 1, EndLine: 10,
		})
	}
	addFn("svc/auth.go::Login", "Login", "svc/auth.go", "go")
	addFn("svc/auth.go::Logout", "Logout", "svc/auth.go", "go")
	addFn("svc/auth.go::validateToken", "validateToken", "svc/auth.go", "go")
	addFn("handlers/user.go::HandleUser", "HandleUser", "handlers/user.go", "go")
	// A TS node to exercise the language filter.
	g.AddNode(&graph.Node{
		ID: "web/api.ts::fetchUser", Kind: graph.KindFunction,
		Name: "fetchUser", FilePath: "web/api.ts", Language: "typescript",
		StartLine: 1, EndLine: 5,
	})

	// Call graph: HandleUser -> Login -> validateToken (x2 refs)
	g.AddEdge(&graph.Edge{From: "handlers/user.go::HandleUser", To: "svc/auth.go::Login", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "svc/auth.go::Login", To: "svc/auth.go::validateToken", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "svc/auth.go::Logout", To: "svc/auth.go::validateToken", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "svc/auth.go::HandleUser", To: "svc/auth.go::validateToken", Kind: graph.EdgeReferences})

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	// Seed community membership so community-filter tests have data.
	srv.communities = &analysis.CommunityResult{
		NodeToComm: map[string]string{
			"svc/auth.go::Login":           "community-0",
			"svc/auth.go::Logout":          "community-0",
			"svc/auth.go::validateToken":   "community-0",
			"handlers/user.go::HandleUser": "community-1",
		},
		Communities: []analysis.Community{
			{ID: "community-0", Label: "authz"},
			{ID: "community-1", Label: "handlers"},
		},
	}
	return srv
}

func TestWinnowSymbols_FilterByKindAndPath(t *testing.T) {
	srv := buildWinnowGraph(t)

	c := winnowConstraints{
		Kinds:      []graph.NodeKind{graph.KindFunction},
		PathPrefix: []string{"svc/"},
		Limit:      10,
	}
	rows := srv.winnowSymbols(c, nil)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, graph.KindFunction, r.Node.Kind)
		assert.True(t, strings.HasPrefix(r.Node.FilePath, "svc/"),
			"path %q violates prefix filter", r.Node.FilePath)
	}
}

func TestWinnowSymbols_LanguageFilterExcludesOthers(t *testing.T) {
	srv := buildWinnowGraph(t)

	c := winnowConstraints{Language: "typescript", Limit: 10}
	rows := srv.winnowSymbols(c, nil)
	require.Len(t, rows, 1)
	assert.Equal(t, "web/api.ts::fetchUser", rows[0].Node.ID)
}

func TestWinnowSymbols_MinFanInDropsLeaves(t *testing.T) {
	srv := buildWinnowGraph(t)

	// validateToken has fan_in=3 (2 calls + 1 reference); Login has 1;
	// Logout + HandleUser + fetchUser have 0.
	c := winnowConstraints{MinFanIn: 2, Limit: 10}
	rows := srv.winnowSymbols(c, nil)
	require.Len(t, rows, 1)
	assert.Equal(t, "svc/auth.go::validateToken", rows[0].Node.ID)
	assert.Equal(t, 3, rows[0].FanIn)
}

func TestWinnowSymbols_RankingByFanIn(t *testing.T) {
	srv := buildWinnowGraph(t)

	c := winnowConstraints{Kinds: []graph.NodeKind{graph.KindFunction}, Language: "go", Limit: 10}
	rows := srv.winnowSymbols(c, nil)
	require.NotEmpty(t, rows)

	// validateToken should top the list because fan_in dominates when no
	// text_match is provided.
	assert.Equal(t, "svc/auth.go::validateToken", rows[0].Node.ID)
	assert.Greater(t, rows[0].Score, 0.0)
	assert.NotEmpty(t, rows[0].Contributions)
	assert.Contains(t, rows[0].Contributions, "fan_in")
}

func TestWinnowSymbols_CommunityFilterByIDAndLabel(t *testing.T) {
	srv := buildWinnowGraph(t)

	byID := srv.winnowSymbols(winnowConstraints{Community: "community-0", Limit: 10}, nil)
	byLabel := srv.winnowSymbols(winnowConstraints{Community: "authz", Limit: 10}, nil)

	require.Equal(t, len(byID), len(byLabel))
	require.Greater(t, len(byID), 0)
	for _, r := range byID {
		assert.Equal(t, "community-0", r.Community)
	}
	for _, r := range byLabel {
		assert.Equal(t, "community-0", r.Community)
	}
}

func TestWinnowSymbols_ChurnFilter(t *testing.T) {
	srv := buildWinnowGraph(t)
	srv.symHistory.Record("svc/auth.go::Login", false)
	srv.symHistory.Record("svc/auth.go::Login", true)
	srv.symHistory.Record("svc/auth.go::Logout", false)

	c := winnowConstraints{MinChurn: 2, Limit: 10}
	rows := srv.winnowSymbols(c, nil)
	require.Len(t, rows, 1)
	assert.Equal(t, "svc/auth.go::Login", rows[0].Node.ID)
	assert.Equal(t, 2, rows[0].Churn)
}

func TestWinnowSymbols_RequiresConstraint(t *testing.T) {
	srv := buildWinnowGraph(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "winnow_symbols"
	req.Params.Arguments = map[string]any{}

	result, err := srv.handleWinnowSymbols(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	msg := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, msg, "at least one constraint")
}

func TestWinnowSymbols_InvalidKindRejected(t *testing.T) {
	srv := buildWinnowGraph(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": "bogus"}

	result, err := srv.handleWinnowSymbols(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcplib.TextContent).Text, "invalid kind")
}

func TestWinnowSymbols_JSONResponseShape(t *testing.T) {
	srv := buildWinnowGraph(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"kind":       "function",
		"min_fan_in": float64(1),
	}
	result, err := srv.handleWinnowSymbols(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	var payload map[string]any
	raw := result.Content[0].(mcplib.TextContent).Text
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))

	assert.Contains(t, payload, "results")
	assert.Contains(t, payload, "total")
	assert.Contains(t, payload, "weights")

	results, _ := payload["results"].([]any)
	require.NotEmpty(t, results)
	first, _ := results[0].(map[string]any)
	assert.Contains(t, first, "score")
	assert.Contains(t, first, "fan_in")
	assert.Contains(t, first, "contributions")
}

func TestWinnowSymbols_GCXRoundTrip(t *testing.T) {
	srv := buildWinnowGraph(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"kind":   "function",
		"format": "gcx",
	}
	result, err := srv.handleWinnowSymbols(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)

	payload := result.Content[0].(mcplib.TextContent).Text
	dec := wire.NewDecoder(strings.NewReader(payload))
	h, err := dec.Header()
	require.NoError(t, err)
	assert.Equal(t, "winnow_symbols", h.Tool)
	assert.Contains(t, h.Fields, "score")
	assert.Contains(t, h.Fields, "contributions")
	assert.Contains(t, h.Meta, "weights")

	rows, err := dec.All()
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	// contributions is a sorted pipe-joined list: "fan_in=0.700|fan_out=0.300"
	for _, row := range rows {
		if s := row["contributions"]; s != "" {
			parts := strings.Split(s, "|")
			for _, p := range parts {
				assert.Contains(t, p, "=")
			}
		}
	}
}

func TestEncodeWinnowSymbols_EmptyContributionsOk(t *testing.T) {
	rows := []winnowResult{
		{
			Node: &graph.Node{
				ID: "a.go::Foo", Kind: graph.KindFunction,
				Name: "Foo", FilePath: "a.go", StartLine: 3,
				Meta: map[string]any{"signature": "func Foo()"},
			},
		},
	}
	payload, err := encodeWinnowSymbols(rows, 1, 10, nil)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	assert.Equal(t, "winnow_symbols", h.Tool)
	all, err := dec.All()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "", all[0]["contributions"])
}

func TestFormatContributions_DeterministicOrdering(t *testing.T) {
	m := map[string]float64{
		"fan_in":     0.5,
		"text_match": 0.9,
		"churn":      0.2,
	}
	got := formatContributions(m)
	// Sorted alphabetically.
	assert.Equal(t, "churn=0.200|fan_in=0.500|text_match=0.900", got)
}
