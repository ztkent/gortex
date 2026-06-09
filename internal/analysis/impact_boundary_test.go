package analysis

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestAnalyzeImpact_LowerBound_InterfaceDispatch: a changed method that
// implements an interface has callers that may dispatch through the interface,
// so its blast radius is a floor, not an exact count.
func TestAnalyzeImpact_LowerBound_InterfaceDispatch(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg.T.M", Kind: graph.KindMethod, Name: "M"})
	g.AddNode(&graph.Node{ID: "pkg.caller", Kind: graph.KindFunction, Name: "caller"})
	g.AddEdge(&graph.Edge{From: "pkg.caller", To: "pkg.T.M", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "pkg.T.M", To: "iface::I.M", Kind: graph.EdgeImplements, Origin: graph.OriginASTResolved})

	res := AnalyzeImpact(g, []string{"pkg.T.M"}, nil, nil)
	if !res.LowerBound {
		t.Fatalf("expected LowerBound=true, got summary=%q boundaries=%+v", res.Summary, res.Boundaries)
	}
	if len(res.Boundaries) != 1 || res.Boundaries[0].Reason != graph.BoundaryInterfaceDispatch {
		t.Errorf("expected one interface_dispatch boundary, got %+v", res.Boundaries)
	}
}

// TestAnalyzeImpact_NoBoundary: a plain function with no dispatch participation
// reports an exact count (no lower-bound flag).
func TestAnalyzeImpact_NoBoundary(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg.f", Kind: graph.KindFunction, Name: "f"})
	g.AddNode(&graph.Node{ID: "pkg.g", Kind: graph.KindFunction, Name: "g"})
	g.AddEdge(&graph.Edge{From: "pkg.g", To: "pkg.f", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved})
	res := AnalyzeImpact(g, []string{"pkg.f"}, nil, nil)
	if res.LowerBound || len(res.Boundaries) != 0 {
		t.Errorf("expected no lower bound, got LowerBound=%v boundaries=%+v", res.LowerBound, res.Boundaries)
	}
}
