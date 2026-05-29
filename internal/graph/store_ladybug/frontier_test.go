package store_ladybug_test

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

// buildFrontierStore seeds a hub with two callers (a, b) and two
// callees reached by different edge kinds (c via Calls, d via
// References), plus a Calls edge to an unresolved stub and to an
// external stub — both of which ExpandFrontier must filter server-side.
func buildFrontierStore(t *testing.T) *store_ladybug.Store {
	t.Helper()
	s, err := store_ladybug.Open(filepath.Join(t.TempDir(), "frontier.kuzu"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, n := range []*graph.Node{
		{ID: "a", Name: "a", Kind: graph.KindFunction, FilePath: "a.go", WorkspaceID: "ws"},
		{ID: "b", Name: "b", Kind: graph.KindFunction, FilePath: "b.go", WorkspaceID: "ws"},
		{ID: "hub", Name: "hub", Kind: graph.KindFunction, FilePath: "hub.go", WorkspaceID: "ws"},
		{ID: "c", Name: "c", Kind: graph.KindFunction, FilePath: "c.go", WorkspaceID: "ws"},
		{ID: "d", Name: "d", Kind: graph.KindFunction, FilePath: "d.go", WorkspaceID: "ws"},
		// Stub endpoints so the edges below are insertable; ExpandFrontier
		// must still exclude them by id prefix.
		{ID: "unresolved::ghost", Name: "ghost", Kind: graph.KindFunction, FilePath: ""},
		{ID: "external::pkg.Ext", Name: "Ext", Kind: graph.KindFunction, FilePath: ""},
	} {
		s.AddNode(n)
	}
	for _, e := range []*graph.Edge{
		{From: "a", To: "hub", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "b", To: "hub", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2},
		{From: "hub", To: "c", Kind: graph.EdgeCalls, FilePath: "hub.go", Line: 3},
		{From: "hub", To: "d", Kind: graph.EdgeReferences, FilePath: "hub.go", Line: 4},
		{From: "hub", To: "unresolved::ghost", Kind: graph.EdgeCalls, FilePath: "hub.go", Line: 5},
		{From: "hub", To: "external::pkg.Ext", Kind: graph.EdgeCalls, FilePath: "hub.go", Line: 6},
	} {
		s.AddEdge(e)
	}
	return s
}

func neighborIDs(hops []graph.FrontierHop) []string {
	ids := make([]string, 0, len(hops))
	for _, h := range hops {
		ids = append(ids, h.Neighbor.ID)
	}
	sort.Strings(ids)
	return ids
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestExpandFrontier_OutgoingFiltersAndProjection verifies the forward
// expansion: edge-kind filtering, server-side exclusion of
// unresolved/external targets, and that the neighbour node is fully
// projected (columns populated) but meta-free.
func TestExpandFrontier_OutgoingFiltersAndProjection(t *testing.T) {
	s := buildFrontierStore(t)

	// Calls + References → c (Calls) and d (References); the unresolved
	// and external targets are dropped by the server-side id filter.
	hops := s.ExpandFrontier([]string{"hub"}, true, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, 0)
	if got, want := neighborIDs(hops), []string{"c", "d"}; !equalIDs(got, want) {
		t.Fatalf("forward Calls+References neighbours = %v, want %v", got, want)
	}

	// Edge-kind filter: Calls only → just c (d is reached via References).
	callsOnly := s.ExpandFrontier([]string{"hub"}, true, []graph.EdgeKind{graph.EdgeCalls}, 0)
	if got, want := neighborIDs(callsOnly), []string{"c"}; !equalIDs(got, want) {
		t.Fatalf("forward Calls-only neighbours = %v, want %v", got, want)
	}

	// Projection: the c hop carries a populated, meta-free neighbour and
	// the correctly-oriented edge.
	var cHop *graph.FrontierHop
	for i := range callsOnly {
		if callsOnly[i].Neighbor.ID == "c" {
			cHop = &callsOnly[i]
			break
		}
	}
	if cHop == nil {
		t.Fatal("no hop for neighbour c")
	}
	if cHop.Neighbor.Name != "c" || cHop.Neighbor.FilePath != "c.go" || cHop.Neighbor.Kind != graph.KindFunction {
		t.Fatalf("neighbour c under-projected: %+v", cHop.Neighbor)
	}
	if cHop.Neighbor.Meta != nil {
		t.Fatalf("neighbour c should be meta-free, got Meta=%v", cHop.Neighbor.Meta)
	}
	if cHop.Edge.From != "hub" || cHop.Edge.To != "c" || cHop.Edge.Kind != graph.EdgeCalls {
		t.Fatalf("edge hub->c mis-decoded: %+v", cHop.Edge)
	}
}

// TestExpandFrontier_Incoming verifies the reverse expansion: callers of
// the hub are the neighbours, oriented so the edge still points at the
// hub.
func TestExpandFrontier_Incoming(t *testing.T) {
	s := buildFrontierStore(t)

	hops := s.ExpandFrontier([]string{"hub"}, false, []graph.EdgeKind{graph.EdgeCalls}, 0)
	if got, want := neighborIDs(hops), []string{"a", "b"}; !equalIDs(got, want) {
		t.Fatalf("incoming Calls neighbours = %v, want %v", got, want)
	}
	for _, h := range hops {
		if h.Edge.To != "hub" {
			t.Fatalf("incoming hop edge should point at hub, got To=%q", h.Edge.To)
		}
		if h.Edge.From != h.Neighbor.ID {
			t.Fatalf("incoming hop neighbour %q should equal edge.From %q", h.Neighbor.ID, h.Edge.From)
		}
	}
}

// TestExpandFrontier_EmptyInputs guards the early-return contract: no ids
// or no kinds yields no hops (and no query).
func TestExpandFrontier_EmptyInputs(t *testing.T) {
	s := buildFrontierStore(t)
	if got := s.ExpandFrontier(nil, true, []graph.EdgeKind{graph.EdgeCalls}, 0); got != nil {
		t.Fatalf("ExpandFrontier(nil ids) = %v, want nil", got)
	}
	if got := s.ExpandFrontier([]string{"hub"}, true, nil, 0); got != nil {
		t.Fatalf("ExpandFrontier(nil kinds) = %v, want nil", got)
	}
}
