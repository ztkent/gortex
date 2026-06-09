package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func specGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::handler", Kind: graph.KindFunction, Name: "handler", FilePath: "main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::A.run", Kind: graph.KindMethod, Name: "run", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::B.run", Kind: graph.KindMethod, Name: "run", FilePath: "b.go", Language: "go"})
	// computed-member call `obj["run"]()` left unresolved + tagged.
	g.AddEdge(&graph.Edge{
		From: "main.go::handler", To: "unresolved::*", Kind: graph.EdgeCalls, FilePath: "main.go",
		Meta: map[string]any{"dyn_shape": "computed_member", "dyn_key": "run"},
	})
	return g
}

func specEdges(g *graph.Graph, from string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeCalls && e.IsSpeculative() {
			out = append(out, e)
		}
	}
	return out
}

func TestResolveSpeculativeDispatch_LiteralKey(t *testing.T) {
	g := specGraph()
	n := ResolveSpeculativeDispatch(g, true)
	if n != 2 {
		t.Fatalf("expected 2 speculative edges, got %d", n)
	}
	edges := specEdges(g, "main.go::handler")
	if len(edges) != 2 {
		t.Fatalf("expected 2 speculative out-edges, got %d", len(edges))
	}
	for _, e := range edges {
		if e.Origin != graph.OriginSpeculative {
			t.Errorf("expected OriginSpeculative, got %q", e.Origin)
		}
		if cc, _ := e.Meta["candidate_count"].(int); cc != 2 {
			t.Errorf("expected candidate_count 2, got %v", e.Meta["candidate_count"])
		}
		if e.Confidence <= 0 || e.Confidence > 0.45 {
			t.Errorf("speculative confidence out of range: %v", e.Confidence)
		}
	}
}

func TestResolveSpeculativeDispatch_DisabledNoop(t *testing.T) {
	g := specGraph()
	if n := ResolveSpeculativeDispatch(g, false); n != 0 {
		t.Errorf("disabled pass must be a no-op, got %d", n)
	}
	if len(specEdges(g, "main.go::handler")) != 0 {
		t.Errorf("disabled pass must not mint edges")
	}
}

func TestResolveSpeculativeDispatch_NoLiteralKey(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::h", Kind: graph.KindFunction, Name: "h", FilePath: "main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::A.run", Kind: graph.KindMethod, Name: "run", FilePath: "a.go", Language: "go"})
	// variable-key obj[name]() — no dyn_key → unbounded → no guess in v1.
	g.AddEdge(&graph.Edge{From: "main.go::h", To: "unresolved::*", Kind: graph.EdgeCalls, Meta: map[string]any{"dyn_shape": "computed_member"}})
	if n := ResolveSpeculativeDispatch(g, true); n != 0 {
		t.Errorf("variable-key (no literal) must not guess, got %d", n)
	}
}

func TestResolveSpeculativeDispatch_HardCap(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::h", Kind: graph.KindFunction, Name: "h", FilePath: "main.go", Language: "go"})
	for i := 0; i < speculativeHardCap+5; i++ {
		id := "f" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".go::T.run"
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: "run", FilePath: id, Language: "go"})
	}
	g.AddEdge(&graph.Edge{From: "main.go::h", To: "unresolved::*", Kind: graph.EdgeCalls, Meta: map[string]any{"dyn_shape": "computed_member", "dyn_key": "run"}})
	if n := ResolveSpeculativeDispatch(g, true); n != 0 {
		t.Errorf("candidate set above hard cap must be dropped as noise, got %d", n)
	}
}

func TestResolveSpeculativeDispatch_Idempotent(t *testing.T) {
	g := specGraph()
	first := ResolveSpeculativeDispatch(g, true)
	second := ResolveSpeculativeDispatch(g, true)
	if first != 2 {
		t.Fatalf("expected 2, got %d", first)
	}
	if len(specEdges(g, "main.go::handler")) != 2 {
		t.Errorf("idempotency: edge count must stay 2, got %d (second run reported %d)", len(specEdges(g, "main.go::handler")), second)
	}
}
