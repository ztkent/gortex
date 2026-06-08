package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestGetCallChain_RecordsDispatchBoundary: a forward call-chain walk that
// drops an unresolved (dynamic-dispatch) out-edge must flag the reachable set
// as a floor and name the boundary.
func TestGetCallChain_RecordsDispatchBoundary(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "A", Kind: graph.KindFunction, Name: "A"})
	g.AddNode(&graph.Node{ID: "B", Kind: graph.KindFunction, Name: "B"})
	g.AddEdge(&graph.Edge{From: "A", To: "B", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	// B dispatches to an unresolved target the resolver could not bind.
	g.AddEdge(&graph.Edge{From: "B", To: "unresolved::handler", Kind: graph.EdgeCalls, Origin: graph.OriginASTInferred})

	sg := NewEngine(g).GetCallChain("A", QueryOptions{Depth: 3, Limit: 50})
	if !sg.LowerBound {
		t.Fatalf("expected LowerBound=true, got boundaries=%+v", sg.Boundaries)
	}
	if len(sg.Boundaries) != 1 {
		t.Fatalf("expected 1 boundary, got %d: %+v", len(sg.Boundaries), sg.Boundaries)
	}
	b := sg.Boundaries[0]
	if b.SeedID != "B" || b.Target != "handler" || b.Reason != graph.BoundaryDynamicDispatch || b.Direction != "callees" {
		t.Errorf("unexpected boundary: %+v", b)
	}
}

// TestGetCallChain_NoBoundary: a fully-resolved chain reports no boundary and
// no lower-bound flag (so existing responses are unchanged).
func TestGetCallChain_NoBoundary(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "A", Kind: graph.KindFunction, Name: "A"})
	g.AddNode(&graph.Node{ID: "B", Kind: graph.KindFunction, Name: "B"})
	g.AddEdge(&graph.Edge{From: "A", To: "B", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	sg := NewEngine(g).GetCallChain("A", QueryOptions{Depth: 3, Limit: 50})
	if sg.LowerBound || len(sg.Boundaries) != 0 {
		t.Errorf("expected no boundary, got LowerBound=%v boundaries=%+v", sg.LowerBound, sg.Boundaries)
	}
}
