package store_ladybug_test

// Regression guard for the resolver kind-gate: the name-only in-engine
// rules (ResolveSameFile / ResolveSamePackage / ResolveImportAware /
// ResolveCrossRepo / ResolveUniqueNames) must never re-point a
// type-position edge (returns / typed_as / extends / implements /
// composes) onto a function/method that merely shares the name — only
// onto a type/interface. Without the gate, a `returns` edge landed on a
// same-named function (a wrong edge that also made the function look
// dead, since returns/typed_as aren't counted as a use of a function).
// Mirrors resolveTypeRef in internal/resolver/resolver.go. Runs through
// the whole ResolveAllBulk chain so it guards every rule.

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	store_ladybug "github.com/zzet/gortex/internal/graph/store_ladybug"
)

func TestResolveBulk_KindGate_TypePositionEdgeNeverLandsOnFunction(t *testing.T) {
	const file = "pkg/a.go"

	// Negative case: only a FUNCTION named "test" exists. A `returns`
	// edge must NOT bind to it; the `calls` edge must.
	t.Run("function_only", func(t *testing.T) {
		s := openTmp(t)
		s.AddNode(&graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
		s.AddNode(&graph.Node{ID: file + "::test", Name: "test", Kind: graph.KindFunction, FilePath: file})
		s.AddNode(&graph.Node{ID: "unresolved::test", Name: "test", Kind: graph.NodeKind("unresolved")})
		s.AddEdge(&graph.Edge{From: file + "::Caller", To: "unresolved::test", Kind: graph.EdgeCalls, FilePath: file, Line: 1})
		s.AddEdge(&graph.Edge{From: file + "::Caller", To: "unresolved::test", Kind: graph.EdgeReturns, FilePath: file, Line: 2})

		if _, err := s.ResolveAllBulk(); err != nil {
			t.Fatalf("ResolveAllBulk: %v", err)
		}
		byKind := callerEdgesByKind(s, file+"::Caller")
		if byKind[graph.EdgeCalls] != file+"::test" {
			t.Errorf("calls edge: want -> %s::test, got -> %q", file, byKind[graph.EdgeCalls])
		}
		if byKind[graph.EdgeReturns] == file+"::test" {
			t.Errorf("BUG: returns edge re-pointed onto the FUNCTION %s::test — kind gate missing", file)
		}
	})

	// Positive case: a TYPE named "test" exists. The `returns` edge
	// SHOULD resolve to it (the gate must allow type-position -> type).
	t.Run("type_present", func(t *testing.T) {
		s := openTmp(t)
		s.AddNode(&graph.Node{ID: file + "::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: file})
		s.AddNode(&graph.Node{ID: file + "::test", Name: "test", Kind: graph.KindType, FilePath: file})
		s.AddNode(&graph.Node{ID: "unresolved::test", Name: "test", Kind: graph.NodeKind("unresolved")})
		s.AddEdge(&graph.Edge{From: file + "::Caller", To: "unresolved::test", Kind: graph.EdgeReturns, FilePath: file, Line: 1})

		if _, err := s.ResolveAllBulk(); err != nil {
			t.Fatalf("ResolveAllBulk: %v", err)
		}
		byKind := callerEdgesByKind(s, file+"::Caller")
		if byKind[graph.EdgeReturns] != file+"::test" {
			t.Errorf("returns edge to a TYPE: want -> %s::test, got -> %q (gate over-blocked a legit type-position resolution)", file, byKind[graph.EdgeReturns])
		}
	})
}

func openTmp(t *testing.T) *store_ladybug.Store {
	t.Helper()
	s, err := store_ladybug.Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callerEdgesByKind(s *store_ladybug.Store, from string) map[graph.EdgeKind]string {
	out := map[graph.EdgeKind]string{}
	for _, e := range s.GetOutEdges(from) {
		if e != nil {
			out[e.Kind] = e.To
		}
	}
	return out
}
