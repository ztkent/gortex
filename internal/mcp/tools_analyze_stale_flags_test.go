package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeStaleFlags(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "stale_flags"
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

// addFlagWithCallers wires a flag node + N caller functions, each
// stamped with last_authored.timestamp = ageDays ago.
func addFlagWithCallers(g *graph.Graph, flagID, provider, name string, callers map[string]int /* callerID → ageDays */) {
	g.AddNode(&graph.Node{
		ID:   flagID,
		Kind: graph.KindFlag,
		Name: name,
		Meta: map[string]any{
			"provider": provider,
			"name":     name,
		},
	})
	now := time.Now().Unix()
	for callerID, ageDays := range callers {
		ts := now - int64(ageDays*24*3600)
		g.AddNode(&graph.Node{
			ID:        callerID,
			Kind:      graph.KindFunction,
			Name:      callerID,
			FilePath:  "f.go",
			StartLine: 1,
			Meta: map[string]any{
				"last_authored": map[string]any{
					"email":     "alice@x",
					"timestamp": ts,
				},
			},
		})
		g.AddEdge(&graph.Edge{
			From: callerID,
			To:   flagID,
			Kind: graph.EdgeTogglesFlag,
		})
	}
}

func TestAnalyzeStaleFlags_AllCallersOldHitsThreshold(t *testing.T) {
	srv, _ := setupTestServer(t)
	addFlagWithCallers(srv.graph, "flag::launchdarkly::signup_v2", "launchdarkly", "signup_v2",
		map[string]int{"f.go::A": 500, "f.go::B": 700})

	out := callAnalyzeStaleFlags(t, srv, map[string]any{})
	flags, _ := out["flags"].([]any)
	if len(flags) != 1 {
		t.Fatalf("expected 1 stale flag, got %d", len(flags))
	}
	row := flags[0].(map[string]any)
	if row["name"] != "signup_v2" {
		t.Errorf("name = %v", row["name"])
	}
	// AgeDays should reflect the *newest* caller (500, not 700).
	age, _ := row["age_days"].(float64)
	if age < 499 || age > 501 {
		t.Errorf("age_days = %v, expected ~500", age)
	}
}

func TestAnalyzeStaleFlags_OneFreshCallerKeepsItAlive(t *testing.T) {
	srv, _ := setupTestServer(t)
	addFlagWithCallers(srv.graph, "flag::launchdarkly::active", "launchdarkly", "active",
		map[string]int{"f.go::A": 30, "f.go::B": 700}) // mix of old + recent

	out := callAnalyzeStaleFlags(t, srv, map[string]any{})
	flags, _ := out["flags"].([]any)
	if len(flags) != 0 {
		t.Errorf("flag with one recent caller should not be stale, got %d", len(flags))
	}
}

func TestAnalyzeStaleFlags_OrphanFlagSurfaces(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Flag with no callers at all.
	srv.graph.AddNode(&graph.Node{
		ID:   "flag::unleash::orphan",
		Kind: graph.KindFlag,
		Name: "orphan",
		Meta: map[string]any{
			"provider": "unleash",
			"name":     "orphan",
		},
	})

	out := callAnalyzeStaleFlags(t, srv, map[string]any{})
	flags, _ := out["flags"].([]any)
	if len(flags) != 1 {
		t.Fatalf("orphan flag should always surface, got %d", len(flags))
	}
	row := flags[0].(map[string]any)
	callers, _ := row["callers"].(float64)
	if callers != 0 {
		t.Errorf("orphan callers = %v, want 0", callers)
	}
	age, _ := row["age_days"].(float64)
	if age != -1 {
		t.Errorf("orphan age_days = %v, want -1 sentinel", age)
	}
}

func TestAnalyzeStaleFlags_ProviderFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addFlagWithCallers(srv.graph, "flag::launchdarkly::a", "launchdarkly", "a",
		map[string]int{"x::1": 500})
	addFlagWithCallers(srv.graph, "flag::growthbook::b", "growthbook", "b",
		map[string]int{"x::2": 500})

	out := callAnalyzeStaleFlags(t, srv, map[string]any{"provider": "launchdarkly"})
	flags, _ := out["flags"].([]any)
	if len(flags) != 1 {
		t.Fatalf("provider filter wrong, got %d", len(flags))
	}
	row := flags[0].(map[string]any)
	if row["provider"] != "launchdarkly" {
		t.Errorf("got %v", row["provider"])
	}
}

func TestAnalyzeStaleFlags_UnscoredCounted(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Flag with a caller but no blame meta — should be unscored.
	srv.graph.AddNode(&graph.Node{
		ID:   "flag::launchdarkly::no_blame",
		Kind: graph.KindFlag,
		Name: "no_blame",
		Meta: map[string]any{"provider": "launchdarkly"},
	})
	srv.graph.AddNode(&graph.Node{
		ID:   "f.go::Caller",
		Kind: graph.KindFunction,
	})
	srv.graph.AddEdge(&graph.Edge{
		From: "f.go::Caller",
		To:   "flag::launchdarkly::no_blame",
		Kind: graph.EdgeTogglesFlag,
	})

	out := callAnalyzeStaleFlags(t, srv, map[string]any{})
	flags, _ := out["flags"].([]any)
	if len(flags) != 0 {
		t.Errorf("no-blame flag must not be reported as stale (insufficient data), got %d", len(flags))
	}
	if got, _ := out["unscored"].(float64); got != 1 {
		t.Errorf("unscored = %v, want 1", got)
	}
}
