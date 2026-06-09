package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// implementsFixture builds a graph where type A and type B each have a method M
// matching interface I (same repo), so InferImplements should infer A→I, B→I.
func implementsFixture() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "iface.go::I", Kind: graph.KindInterface, Name: "I", Meta: map[string]any{"methods": []string{"M"}}})
	for _, ty := range []string{"A", "B"} {
		g.AddNode(&graph.Node{ID: "x.go::" + ty, Kind: graph.KindType, Name: ty})
		g.AddNode(&graph.Node{ID: "x.go::" + ty + ".M", Kind: graph.KindMethod, Name: "M"})
		g.AddEdge(&graph.Edge{From: "x.go::" + ty + ".M", To: "x.go::" + ty, Kind: graph.EdgeMemberOf})
	}
	return g
}

func implementsEdges(g *graph.Graph) map[string]bool {
	out := map[string]bool{}
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeImplements {
			out[e.From+"->"+e.To] = true
		}
	}
	return out
}

// TestInferImplementsScoped_ParityFull asserts the scoped pass, given the
// changed interface in its scope, re-derives the SAME implements edges as the
// full pass — even for implementor types in unchanged files (the EvictFile
// in-edge-drop regression guard).
func TestInferImplementsScoped_ParityFull(t *testing.T) {
	full := implementsFixture()
	New(full).InferImplements()
	wantEdges := implementsEdges(full)
	if len(wantEdges) != 2 {
		t.Fatalf("setup: expected 2 implements edges from full pass, got %d", len(wantEdges))
	}

	// Scope = the interface changed (its in-edges were dropped); scoped must
	// re-check every type against it.
	scoped := implementsFixture()
	New(scoped).InferImplementsScoped(map[string]bool{}, map[string]bool{"iface.go::I": true})
	if got := implementsEdges(scoped); len(got) != len(wantEdges) {
		t.Fatalf("scoped (changed iface) = %v, want %v", got, wantEdges)
	}
}

func TestInferImplementsScoped_AffectedType(t *testing.T) {
	// Only type A is affected (changed); scoped must re-check A against all
	// interfaces and add A->I, but not touch B.
	g := implementsFixture()
	New(g).InferImplementsScoped(map[string]bool{"x.go::A": true}, map[string]bool{})
	got := implementsEdges(g)
	if !got["x.go::A->iface.go::I"] {
		t.Errorf("expected A->I from affected type A, got %v", got)
	}
	if got["x.go::B->iface.go::I"] {
		t.Errorf("B was not affected and has no survivor edge here, must not be inferred: %v", got)
	}
}

func TestInferImplementsScoped_EmptyScopeNoWork(t *testing.T) {
	g := implementsFixture()
	if n := New(g).InferImplementsScoped(map[string]bool{}, map[string]bool{}); n != 0 {
		t.Errorf("empty scope must do zero work, added %d", n)
	}
	if len(implementsEdges(g)) != 0 {
		t.Errorf("empty scope must add no edges")
	}
}

// TestInferOverridesScoped_Parity: child C overrides parent P's method; scoped
// with the parent in scope re-derives the override edge.
func TestInferOverridesScoped_Parity(t *testing.T) {
	build := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "p.go::P", Kind: graph.KindType, Name: "P"})
		g.AddNode(&graph.Node{ID: "p.go::P.M", Kind: graph.KindMethod, Name: "M", FilePath: "p.go"})
		g.AddEdge(&graph.Edge{From: "p.go::P.M", To: "p.go::P", Kind: graph.EdgeMemberOf})
		g.AddNode(&graph.Node{ID: "c.go::C", Kind: graph.KindType, Name: "C"})
		g.AddNode(&graph.Node{ID: "c.go::C.M", Kind: graph.KindMethod, Name: "M", FilePath: "c.go"})
		g.AddEdge(&graph.Edge{From: "c.go::C.M", To: "c.go::C", Kind: graph.EdgeMemberOf})
		g.AddEdge(&graph.Edge{From: "c.go::C", To: "p.go::P", Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved})
		return g
	}
	overrideEdges := func(g *graph.Graph) int {
		n := 0
		for _, e := range g.AllEdges() {
			if e.Kind == graph.EdgeOverrides {
				n++
			}
		}
		return n
	}
	full := build()
	New(full).InferOverrides()
	if overrideEdges(full) != 1 {
		t.Fatalf("setup: expected 1 override edge from full pass, got %d", overrideEdges(full))
	}
	// Parent changed → scope includes P; scoped must re-derive C.M->P.M.
	scoped := build()
	New(scoped).InferOverridesScoped(map[string]bool{"p.go::P": true})
	if overrideEdges(scoped) != 1 {
		t.Errorf("scoped override (parent in scope) = %d, want 1", overrideEdges(scoped))
	}
}
