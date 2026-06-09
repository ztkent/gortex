package mcp

import (
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func apiImpactTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	// Handler + response type with a field-level shape.
	g.AddNode(&graph.Node{ID: "api/users.go::ListUsers", Kind: graph.KindFunction, Name: "ListUsers", FilePath: "api/users.go"})
	g.AddNode(&graph.Node{ID: "t.go::UsersResponse", Kind: graph.KindType, Name: "UsersResponse", FilePath: "t.go",
		Meta: map[string]any{"shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "data", Type: "[]User"}, {Name: "pagination", Type: "Pagination"}}}}})
	// Two consumer shapes: one clean, one accessing a missing field.
	g.AddNode(&graph.Node{ID: "web::CleanShape", Kind: graph.KindType, Name: "CleanShape",
		Meta: map[string]any{"shape": &contracts.Shape{Kind: "interface", Fields: []contracts.ShapeField{{Name: "data", Type: "any"}, {Name: "pagination", Type: "any"}}}}})
	g.AddNode(&graph.Node{ID: "web::MismatchShape", Kind: graph.KindType, Name: "MismatchShape",
		Meta: map[string]any{"shape": &contracts.Shape{Kind: "interface", Fields: []contracts.ShapeField{{Name: "items", Type: "any"}}}}})
	g.AddNode(&graph.Node{ID: "web/list.tsx::GrantsList", Kind: graph.KindFunction, Name: "GrantsList", FilePath: "web/list.tsx"})
	g.AddNode(&graph.Node{ID: "web/hook.ts::useUsers", Kind: graph.KindFunction, Name: "useUsers", FilePath: "web/hook.ts"})

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)

	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{
		ID: "http::GET::/v1/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: "api/users.go::ListUsers", FilePath: "api/users.go",
		Meta: map[string]any{
			"method": "GET", "path": "/v1/users",
			"response_type":     "t.go::UsersResponse",
			"response_envelope": []map[string]any{{"name": "data"}, {"name": "pagination"}},
		},
	})
	reg.Add(contracts.Contract{
		ID: "http::GET::/v1/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "web/list.tsx::GrantsList", FilePath: "web/list.tsx", RepoPrefix: "web",
		Meta: map[string]any{"response_type": "web::CleanShape"},
	})
	reg.Add(contracts.Contract{
		ID: "http::GET::/v1/users", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		SymbolID: "web/hook.ts::useUsers", FilePath: "web/hook.ts", RepoPrefix: "web",
		Meta: map[string]any{"response_type": "web::MismatchShape"},
	})
	srv.contractRegistry = reg
	return srv
}

func callAPIImpact(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := srv.handleAPIImpact(t.Context(), req)
	require.NoError(t, err)
	return res
}

func TestAPIImpact_ResolveByRoute(t *testing.T) {
	srv := apiImpactTestServer(t)
	res := callAPIImpact(t, srv, map[string]any{"route": "/v1/users"})
	require.False(t, res.IsError, "errored: %v", res)

	var rep apiImpactReport
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &rep))
	require.Equal(t, "http::GET::/v1/users", rep.Route)
	require.Equal(t, "GET", rep.Method)
	require.Equal(t, "api/users.go::ListUsers", rep.Handler)
	require.Equal(t, []string{"data", "pagination"}, rep.ResponseShape.Success)
	require.Len(t, rep.Consumers, 2)
	require.NotEmpty(t, rep.ImpactSummary.RiskLevel)
	require.Equal(t, 2, rep.ImpactSummary.DirectConsumers)

	// Consumer accessed fields come from each consumer's response_type shape.
	byName := map[string][]string{}
	for _, c := range rep.Consumers {
		byName[c.Name] = c.Accesses
	}
	require.Equal(t, []string{"data", "pagination"}, byName["GrantsList"])
	require.Equal(t, []string{"items"}, byName["useUsers"])
}

func TestAPIImpact_ResolveByFile(t *testing.T) {
	srv := apiImpactTestServer(t)
	res := callAPIImpact(t, srv, map[string]any{"file": "api/users"})
	require.False(t, res.IsError)
	var rep apiImpactReport
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &rep))
	require.Equal(t, "http::GET::/v1/users", rep.Route)
}

func TestAPIImpact_NoMatch(t *testing.T) {
	srv := apiImpactTestServer(t)
	res := callAPIImpact(t, srv, map[string]any{"route": "/nonexistent"})
	require.True(t, res.IsError, "expected error for no matching route")
}

func TestAPIImpact_RequiresTarget(t *testing.T) {
	srv := apiImpactTestServer(t)
	res := callAPIImpact(t, srv, map[string]any{})
	require.True(t, res.IsError, "expected error when neither route nor file is given")
}

func TestAPIImpact_MiddlewareUnavailable(t *testing.T) {
	srv := apiImpactTestServer(t)
	res := callAPIImpact(t, srv, map[string]any{"route": "/v1/users"})
	var rep apiImpactReport
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &rep))
	require.Empty(t, rep.Middleware)
	require.Contains(t, rep.MiddlewareDetection, "unavailable")
}

func TestAPIImpact_GCXFormat(t *testing.T) {
	srv := apiImpactTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "api_impact"
	req.Params.Arguments = map[string]any{"route": "/v1/users", "format": "gcx"}
	res, err := srv.handleAPIImpact(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "api_impact.routes")
	require.Contains(t, text, "api_impact.consumers")
}

func TestFuseRouteRisk(t *testing.T) {
	cases := []struct {
		name      string
		consumers int
		mismatch  int
		impact    analysis.RiskLevel
		breaking  int
		want      analysis.RiskLevel
		upgrade   bool
	}{
		{"many consumers", 11, 0, analysis.RiskLow, 0, analysis.RiskHigh, false},
		{"medium consumers", 5, 0, analysis.RiskLow, 0, analysis.RiskMedium, false},
		{"low bumped by mismatch", 2, 1, analysis.RiskLow, 0, analysis.RiskMedium, false},
		{"breaking forces high", 0, 0, analysis.RiskLow, 1, analysis.RiskHigh, true},
		{"impact dominates", 0, 0, analysis.RiskHigh, 0, analysis.RiskHigh, false},
		{"all low", 1, 0, analysis.RiskLow, 0, analysis.RiskLow, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, upgrade := fuseRouteRisk(tc.consumers, tc.mismatch, tc.impact, tc.breaking)
			if got != tc.want {
				t.Errorf("fuseRouteRisk = %s, want %s", got, tc.want)
			}
			if (upgrade != "") != tc.upgrade {
				t.Errorf("upgrade=%q, want present=%v", upgrade, tc.upgrade)
			}
		})
	}
}
