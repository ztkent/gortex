package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeStringEmitters(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "string_emitters"
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

func addStringNode(g *graph.Graph, id, value, ctx string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindString,
		Name: value,
		Meta: map[string]any{"context": ctx, "value": value},
	})
}

func addStringEmitEdge(g *graph.Graph, from, to, ctx, method string) {
	g.AddEdge(&graph.Edge{
		From: from,
		To:   to,
		Kind: graph.EdgeEmits,
		Meta: map[string]any{"context": ctx, "method": method},
	})
}

func TestAnalyzeStringEmitters_GroupsByString(t *testing.T) {
	srv, _ := setupTestServer(t)
	addStringNode(srv.graph, "string::metric::orders.success", "orders.success", "metric")
	addStringNode(srv.graph, "string::error_msg::not authorized", "not authorized", "error_msg")
	addStringEmitEdge(srv.graph, "f.go::Checkout", "string::metric::orders.success", "metric", "Increment")
	addStringEmitEdge(srv.graph, "f.go::Refund", "string::metric::orders.success", "metric", "Increment")
	addStringEmitEdge(srv.graph, "f.go::Auth", "string::error_msg::not authorized", "error_msg", "errors.New")

	out := callAnalyzeStringEmitters(t, srv, map[string]any{})
	rows, _ := out["strings"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 strings, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["value"] != "orders.success" {
		t.Errorf("expected orders.success first (more emits), got %v", first["value"])
	}
	if first["context"] != "metric" {
		t.Errorf("expected first.context = metric, got %v", first["context"])
	}
	if int(first["emits"].(float64)) != 2 {
		t.Errorf("expected 2 emits, got %v", first["emits"])
	}
}

func TestAnalyzeStringEmitters_ContextFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addStringNode(srv.graph, "string::metric::a", "a", "metric")
	addStringNode(srv.graph, "string::route::/x", "/x", "route")
	addStringEmitEdge(srv.graph, "f.go::A", "string::metric::a", "metric", "Increment")
	addStringEmitEdge(srv.graph, "f.go::B", "string::route::/x", "route", "HandleFunc")

	out := callAnalyzeStringEmitters(t, srv, map[string]any{
		"context": "route",
	})
	rows, _ := out["strings"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 string after context=route filter, got %d", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["context"] != "route" {
		t.Errorf("filter leaked: got context=%v", first["context"])
	}
}

func TestAnalyzeStringEmitters_NameSubstringFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addStringNode(srv.graph, "string::metric::orders.success", "orders.success", "metric")
	addStringNode(srv.graph, "string::metric::server.memory", "server.memory", "metric")
	addStringEmitEdge(srv.graph, "f.go::A", "string::metric::orders.success", "metric", "Increment")
	addStringEmitEdge(srv.graph, "f.go::B", "string::metric::server.memory", "metric", "Gauge")

	out := callAnalyzeStringEmitters(t, srv, map[string]any{
		"name": "orders",
	})
	rows, _ := out["strings"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 string after name=orders filter, got %d", len(rows))
	}
}

func TestAnalyzeStringEmitters_IgnoresNonStringEmitTargets(t *testing.T) {
	// EdgeEmits to KindEvent (the legacy log target) shouldn't appear
	// in string_emitters results.
	srv, _ := setupTestServer(t)
	addEventNode(srv.graph, "event::log::user.login", "user.login", "log")
	addEmitsEdge(srv.graph, "f.go::Auth", "event::log::user.login", "Info")

	out := callAnalyzeStringEmitters(t, srv, map[string]any{})
	rows, _ := out["strings"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected 0 string emitters (only event present), got %d", len(rows))
	}
}
