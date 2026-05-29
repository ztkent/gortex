package store_ladybug_test

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

// TestResolveMethodCalls_UniqueBinds verifies that a receiver-method
// call stub (`unresolved::*.querySelect`) is bound to the concrete
// method node when exactly one method in the repo carries that name,
// and is LEFT unresolved when the name is ambiguous (defined on >1
// type) — the no-false-edge guarantee.
func TestResolveMethodCalls_UniqueBinds(t *testing.T) {
	dir := t.TempDir()
	s, err := store_ladybug.Open(filepath.Join(dir, "test.kuzu"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Caller method + the unique target method, same repo.
	s.AddNode(&graph.Node{ID: "pkg/a.go::Store.GetNode", Name: "Store.GetNode", Kind: graph.KindMethod, FilePath: "pkg/a.go", RepoPrefix: "gortex"})
	s.AddNode(&graph.Node{ID: "pkg/b.go::Store.querySelect", Name: "Store.querySelect", Kind: graph.KindMethod, FilePath: "pkg/b.go", RepoPrefix: "gortex", Meta: map[string]any{"receiver": "Store"}})
	// Ambiguous: two types both define Close — must stay unresolved.
	s.AddNode(&graph.Node{ID: "pkg/b.go::Store.Close", Name: "Store.Close", Kind: graph.KindMethod, FilePath: "pkg/b.go", RepoPrefix: "gortex"})
	s.AddNode(&graph.Node{ID: "pkg/c.go::Conn.Close", Name: "Conn.Close", Kind: graph.KindMethod, FilePath: "pkg/c.go", RepoPrefix: "gortex"})

	// Method-call edges in the pre-resolve stub form (the COPY rewrite
	// prefixes the repo; emulate the prefixed form the daemon sees).
	s.AddEdge(&graph.Edge{From: "pkg/a.go::Store.GetNode", To: "gortex::unresolved::*.querySelect", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5})
	s.AddEdge(&graph.Edge{From: "pkg/a.go::Store.GetNode", To: "gortex::unresolved::*.Close", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 6})

	// Stamp kind/name on the stubs (the chain runs this first), then
	// the method-call rule.
	if _, err := s.ResolveAllBulk(); err != nil {
		t.Fatalf("ResolveAllBulk: %v", err)
	}

	// querySelect is unique → the edge must now point at the method.
	out := s.GetOutEdges("pkg/a.go::Store.GetNode")
	var boundQuerySelect, leftClose bool
	for _, e := range out {
		if e.To == "pkg/b.go::Store.querySelect" && e.Kind == graph.EdgeCalls {
			boundQuerySelect = true
		}
		// Close is ambiguous (Store.Close + Conn.Close) → stub stays.
		if graph.IsUnresolvedTarget(e.To) && graph.UnresolvedName(e.To) == "*.Close" {
			leftClose = true
		}
	}
	if !boundQuerySelect {
		t.Fatalf("expected *.querySelect bound to pkg/b.go::Store.querySelect; out edges = %+v", out)
	}
	if !leftClose {
		t.Fatalf("expected ambiguous *.Close to stay unresolved (no false edge); out edges = %+v", out)
	}

	// find_usages-shaped check: the method now has an incoming caller.
	in := s.GetInEdges("pkg/b.go::Store.querySelect")
	if len(in) != 1 || in[0].From != "pkg/a.go::Store.GetNode" {
		t.Fatalf("expected Store.querySelect to have 1 caller; in edges = %+v", in)
	}
}
