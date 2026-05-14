package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzePubsub(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "pubsub"
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

func addPubsubTopic(g *graph.Graph, id, name, transport string) {
	g.AddNode(&graph.Node{
		ID:   id,
		Kind: graph.KindEvent,
		Name: name,
		Meta: map[string]any{"event_kind": "pubsub", "transport": transport, "name": name},
	})
}

func addListensOnEdge(g *graph.Graph, from, to string) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeListensOn})
}

func seedPubsubGraph(srv *Server) {
	g := srv.graph
	addPubsubTopic(g, "event::pubsub::nats::orders.created", "orders.created", "nats")
	addPubsubTopic(g, "event::pubsub::kafka::payments", "payments", "kafka")
	// A non-pubsub observability event must be ignored by analyze pubsub.
	addEventNode(g, "event::log::user.login", "user.login", "log")

	addEmitsEdge(g, "a.go::Pub", "event::pubsub::nats::orders.created", "Publish")
	addListensOnEdge(g, "a.go::Sub", "event::pubsub::nats::orders.created")
	addListensOnEdge(g, "a.go::Sub2", "event::pubsub::nats::orders.created")
	addEmitsEdge(g, "a.go::P2", "event::pubsub::kafka::payments", "Produce")
	addEmitsEdge(g, "a.go::Log", "event::log::user.login", "Info")
}

func TestAnalyzePubsub_GroupsByTopic(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedPubsubGraph(srv)

	out := callAnalyzePubsub(t, srv, map[string]any{})
	rows, _ := out["topics"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 pubsub topics (log event excluded), got %d: %+v", len(rows), rows)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "orders.created" {
		t.Errorf("expected orders.created first (3 edges), got %v", first["name"])
	}
	if first["transport"] != "nats" {
		t.Errorf("transport = %v (want nats)", first["transport"])
	}
	if first["publishes"].(float64) != 1 {
		t.Errorf("publishes = %v (want 1)", first["publishes"])
	}
	if first["subscribes"].(float64) != 2 {
		t.Errorf("subscribes = %v (want 2)", first["subscribes"])
	}
}

func TestAnalyzePubsub_TransportFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedPubsubGraph(srv)

	out := callAnalyzePubsub(t, srv, map[string]any{"transport": "kafka"})
	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("expected 1 topic after transport=kafka, got %v", got)
	}
	rows, _ := out["topics"].([]any)
	if rows[0].(map[string]any)["name"] != "payments" {
		t.Errorf("expected payments, got %v", rows[0])
	}
}

func TestAnalyzePubsub_RoleFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedPubsubGraph(srv)

	// role=subscribe keeps only topics with at least one subscriber.
	out := callAnalyzePubsub(t, srv, map[string]any{"role": "subscribe"})
	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("expected 1 topic after role=subscribe, got %v", got)
	}
	rows, _ := out["topics"].([]any)
	if rows[0].(map[string]any)["name"] != "orders.created" {
		t.Errorf("expected orders.created, got %v", rows[0])
	}
}

func TestAnalyzePubsub_NameFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedPubsubGraph(srv)

	out := callAnalyzePubsub(t, srv, map[string]any{"name": "payments"})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 topic after name=payments, got %v", got)
	}
}
