package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeResolutionOutcomes(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	args["kind"] = "resolution_outcomes"
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

func TestAnalyzeResolutionOutcomes_Taxonomy(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	// caller (go)
	g.AddNode(&graph.Node{ID: "a.go::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.go", Language: "go"})

	// ambiguous_multi_match: two same-name go funcs named "doThing".
	g.AddNode(&graph.Node{ID: "x.go::doThing", Kind: graph.KindFunction, Name: "doThing", FilePath: "x.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "y.go::doThing", Kind: graph.KindFunction, Name: "doThing", FilePath: "y.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go::caller", To: "unresolved::doThing", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})

	// candidate_out_of_scope: exactly one same-lang def named "single".
	g.AddNode(&graph.Node{ID: "z.go::single", Kind: graph.KindFunction, Name: "single", FilePath: "z.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go::caller", To: "unresolved::single", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3})

	// cross_language_only: only a python def named "pyOnly".
	g.AddNode(&graph.Node{ID: "p.py::pyOnly", Kind: graph.KindFunction, Name: "pyOnly", FilePath: "p.py", Language: "python"})
	g.AddEdge(&graph.Edge{From: "a.go::caller", To: "unresolved::pyOnly", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 4})

	// no_definition: nothing named "ghost".
	g.AddEdge(&graph.Edge{From: "a.go::caller", To: "unresolved::ghost", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5})

	out := callAnalyzeResolutionOutcomes(t, srv, map[string]any{})
	byReason, _ := out["by_reason"].(map[string]any)
	check := func(reason string, want int) {
		if got, _ := byReason[reason].(float64); int(got) != want {
			t.Errorf("by_reason[%q] = %v, want %d", reason, byReason[reason], want)
		}
	}
	check(outcomeAmbiguousMultiMatch, 1)
	check(outcomeCandidateOutOfScope, 1)
	check(outcomeCrossLanguageOnly, 1)
	check(outcomeNoDefinition, 1)
}

func TestAnalyzeResolutionOutcomes_ReasonFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	g.AddNode(&graph.Node{ID: "a.go::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go::caller", To: "unresolved::ghost", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5})

	out := callAnalyzeResolutionOutcomes(t, srv, map[string]any{"reason": outcomeNoDefinition})
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("reason filter: want 1 row, got %d", len(rows))
	}
	if rows[0].(map[string]any)["reason"] != outcomeNoDefinition {
		t.Errorf("row reason = %v", rows[0])
	}
}
