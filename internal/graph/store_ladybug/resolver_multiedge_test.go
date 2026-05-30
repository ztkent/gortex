package store_ladybug_test

// Regression guard: the in-engine `MATCH (caller)-[e]->(stub) … DELETE e;
// CREATE newE->(target)` rewrite must delete exactly the matched edge
// instance(s) and leave unrelated edges intact — even though liblbug rel
// tables have no primary key (edge identity is the bound instance).
// Multi-edge stress: one caller, several edges to the same stub plus
// edges to other stubs / already-resolved targets.

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveSameFile_MultiEdge_DeletesOnlyResolvedEdges(t *testing.T) {
	s := openTmp(t)
	const file = "pkg/a.go"
	s.AddNode(&graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
	s.AddNode(&graph.Node{ID: file + "::Foo", Name: "Foo", Kind: graph.KindFunction, FilePath: file})     // resolution target
	s.AddNode(&graph.Node{ID: file + "::Other", Name: "Other", Kind: graph.KindFunction, FilePath: file}) // unrelated real target
	s.AddNode(&graph.Node{ID: "unresolved::Foo", Name: "Foo", Kind: graph.NodeKind("unresolved")})
	s.AddNode(&graph.Node{ID: "unresolved::Bar", Name: "Bar", Kind: graph.NodeKind("unresolved")})

	mk := func(to string, kind graph.EdgeKind, line int) {
		s.AddEdge(&graph.Edge{From: file + "::Caller", To: to, Kind: kind, FilePath: file, Line: line})
	}
	mk("unresolved::Foo", graph.EdgeCalls, 1)      // -> resolve to Foo
	mk("unresolved::Foo", graph.EdgeReferences, 2) // multi-edge, same stub, diff kind -> resolve, keep references
	mk("unresolved::Bar", graph.EdgeCalls, 3)      // no real Bar -> stays unresolved
	mk(file+"::Other", graph.EdgeCalls, 4)         // already resolved -> untouched

	if _, err := s.ResolveSameFile(); err != nil {
		t.Fatalf("ResolveSameFile: %v", err)
	}

	type ek struct {
		to   string
		kind graph.EdgeKind
	}
	got := map[ek]int{}
	for _, e := range s.GetOutEdges(file + "::Caller") {
		if e != nil {
			got[ek{e.To, e.Kind}]++
		}
	}

	want := map[ek]int{
		{file + "::Foo", graph.EdgeCalls}:      1,
		{file + "::Foo", graph.EdgeReferences}: 1,
		{"unresolved::Bar", graph.EdgeCalls}:   1,
		{file + "::Other", graph.EdgeCalls}:    1,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("want %v x%d, got x%d (full: %v)", k, n, got[k], got)
		}
	}
	for _, k := range []ek{{"unresolved::Foo", graph.EdgeCalls}, {"unresolved::Foo", graph.EdgeReferences}} {
		if got[k] != 0 {
			t.Errorf("edge %v should have been deleted, %d remain", k, got[k])
		}
	}
	total := 0
	for _, n := range got {
		total += n
	}
	if total != 4 {
		t.Errorf("expected exactly 4 out-edges, got %d: %v", total, got)
	}
}
