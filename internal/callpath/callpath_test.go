package callpath

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func newGraph(ids ...string) *graph.Graph {
	g := graph.New()
	for _, id := range ids {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "test.go"})
	}
	return g
}

func calls(g *graph.Graph, from, to string) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: "test.go", Origin: graph.OriginASTResolved})
}

func callsTier(g *graph.Graph, from, to, origin string) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: "test.go", Origin: origin})
}

func TestShortestPath_Direct(t *testing.T) {
	g := newGraph("A", "B")
	calls(g, "A", "B")
	res := New(g).ShortestPath("A", "B", Options{})
	if !res.Found || len(res.Paths) != 1 {
		t.Fatalf("expected 1 path, got found=%v paths=%d gap=%+v", res.Found, len(res.Paths), res.Gap)
	}
	p := res.Paths[0]
	if p.Length != 1 || len(p.Nodes) != 2 || p.Nodes[0] != "A" || p.Nodes[1] != "B" {
		t.Errorf("unexpected path: %+v", p)
	}
	if p.Confidence <= 0 || p.Confidence > 1 {
		t.Errorf("confidence out of range: %v", p.Confidence)
	}
}

func TestShortestPath_MultiHop(t *testing.T) {
	g := newGraph("A", "B", "C", "D")
	calls(g, "A", "B")
	calls(g, "B", "C")
	calls(g, "C", "D")
	res := New(g).ShortestPath("A", "D", Options{})
	if !res.Found || len(res.Paths) != 1 {
		t.Fatalf("expected found, got %+v", res)
	}
	got := res.Paths[0].Nodes
	want := []string{"A", "B", "C", "D"}
	if len(got) != len(want) {
		t.Fatalf("unexpected nodes: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node %d = %q, want %q (path %v)", i, got[i], want[i], got)
		}
	}
}

// TestShortestPath_BidirectionalMeet asserts the engine returns the SHORTEST of
// two routes (the length-2 diamond top, not a longer detour).
func TestShortestPath_BidirectionalMeet(t *testing.T) {
	g := newGraph("A", "B", "C", "D", "X", "Y", "Z")
	// Short route: A->B->D
	calls(g, "A", "B")
	calls(g, "B", "D")
	// Long route: A->C->X->Y->Z->D
	calls(g, "A", "C")
	calls(g, "C", "X")
	calls(g, "X", "Y")
	calls(g, "Y", "Z")
	calls(g, "Z", "D")
	res := New(g).ShortestPath("A", "D", Options{})
	if !res.Found {
		t.Fatalf("expected found, got %+v", res.Gap)
	}
	if res.Paths[0].Length != 2 {
		t.Errorf("expected shortest length 2, got %d (%v)", res.Paths[0].Length, res.Paths[0].Nodes)
	}
}

func TestShortestPath_Disconnected(t *testing.T) {
	g := newGraph("A", "B", "C", "D")
	calls(g, "A", "B")
	calls(g, "C", "D")
	res := New(g).ShortestPath("A", "D", Options{})
	if res.Found {
		t.Fatalf("expected no path, got %+v", res.Paths)
	}
	if res.Gap == nil || res.Gap.Reason != ReasonDisconnected {
		t.Fatalf("expected disconnected, got %+v", res.Gap)
	}
	if res.Gap.ForwardReached != 1 || res.Gap.BackwardReached != 1 {
		t.Errorf("expected 1 reached each side, got fwd=%d bwd=%d", res.Gap.ForwardReached, res.Gap.BackwardReached)
	}
	if len(res.Gap.FurthestFromSource) == 0 || len(res.Gap.NearestToSink) == 0 {
		t.Errorf("expected frontier reports, got %+v", res.Gap)
	}
}

func TestShortestPath_DynamicDispatchBoundary(t *testing.T) {
	g := newGraph("A", "B", "sink")
	calls(g, "A", "B")
	// B's only onward call is to an unresolved (dynamic-dispatch) target.
	g.AddNode(&graph.Node{ID: "unresolved::handler", Kind: graph.KindFunction, Name: "handler"})
	calls(g, "B", "unresolved::handler")
	// sink has an incoming edge from an unrelated, unreachable node.
	g.AddNode(&graph.Node{ID: "Z", Kind: graph.KindFunction, Name: "Z"})
	calls(g, "Z", "sink")
	res := New(g).ShortestPath("A", "sink", Options{})
	if res.Found {
		t.Fatalf("expected no path, got %+v", res.Paths)
	}
	if res.Gap.Reason != ReasonDynamicDispatch {
		t.Fatalf("expected dynamic_dispatch, got %s", res.Gap.Reason)
	}
	found := false
	for _, b := range res.Gap.BoundaryHits {
		if b.Target == "unresolved::handler" && b.Reason == boundaryDynamicDispatch {
			found = true
		}
	}
	if !found {
		t.Errorf("expected boundary hit for unresolved::handler, got %+v", res.Gap.BoundaryHits)
	}
}

func TestShortestPath_StubBoundary(t *testing.T) {
	g := newGraph("A", "B", "sink")
	calls(g, "A", "B")
	g.AddNode(&graph.Node{ID: "stdlib::fmt.Println", Kind: graph.KindFunction})
	calls(g, "B", "stdlib::fmt.Println")
	g.AddNode(&graph.Node{ID: "Z", Kind: graph.KindFunction})
	calls(g, "Z", "sink")
	res := New(g).ShortestPath("A", "sink", Options{})
	if res.Found {
		t.Fatalf("expected no path")
	}
	if res.Gap.Reason != ReasonExternalBoundary {
		t.Fatalf("expected external boundary, got %s", res.Gap.Reason)
	}
	if len(res.Gap.BoundaryHits) == 0 || res.Gap.BoundaryHits[0].Reason != "stdlib" {
		t.Errorf("expected stdlib boundary hit, got %+v", res.Gap.BoundaryHits)
	}
}

func TestShortestPath_DepthExceeded(t *testing.T) {
	g := newGraph("A", "B", "C", "D", "E")
	calls(g, "A", "B")
	calls(g, "B", "C")
	calls(g, "C", "D")
	calls(g, "D", "E")
	res := New(g).ShortestPath("A", "E", Options{MaxDepth: 2})
	if res.Found {
		t.Fatalf("expected no path within depth 2, got %+v", res.Paths)
	}
	if res.Gap.Reason != ReasonDepthExceeded {
		t.Fatalf("expected depth_exceeded, got %s", res.Gap.Reason)
	}
}

func TestShortestPath_NotFound(t *testing.T) {
	g := newGraph("A", "B")
	calls(g, "A", "B")
	if res := New(g).ShortestPath("MISSING", "B", Options{}); res.Found || res.Gap.Reason != ReasonSrcNotFound {
		t.Errorf("expected src_not_found, got %+v", res)
	}
	if res := New(g).ShortestPath("A", "MISSING", Options{}); res.Found || res.Gap.Reason != ReasonSinkNotFound {
		t.Errorf("expected sink_not_found, got %+v", res)
	}
}

func TestShortestPath_SrcNoOutSinkNoIn(t *testing.T) {
	g := newGraph("A", "B", "C")
	calls(g, "B", "C")
	// A has no out edges.
	if res := New(g).ShortestPath("A", "C", Options{}); res.Found || res.Gap.Reason != ReasonSrcNoOut {
		t.Errorf("expected src_no_out, got %+v", res.Gap)
	}
	// A has out edge to B but B is the target with no incoming... use a fresh
	// graph where the sink genuinely has no in-edges.
	g2 := newGraph("A", "B", "C")
	calls(g2, "A", "B")
	if res := New(g2).ShortestPath("A", "C", Options{}); res.Found || res.Gap.Reason != ReasonSinkNoIn {
		t.Errorf("expected sink_no_in, got %+v", res.Gap)
	}
}

func TestShortestPath_SameNode(t *testing.T) {
	g := newGraph("A")
	res := New(g).ShortestPath("A", "A", Options{})
	if !res.Found || len(res.Paths) != 1 || res.Paths[0].Length != 0 {
		t.Errorf("expected trivial 0-length path, got %+v", res)
	}
}

func TestShortestPath_KShortest(t *testing.T) {
	g := newGraph("A", "B", "C", "D")
	// Two equal-length routes A->B->D and A->C->D.
	calls(g, "A", "B")
	calls(g, "B", "D")
	calls(g, "A", "C")
	calls(g, "C", "D")
	res := New(g).ShortestPath("A", "D", Options{K: 2})
	if !res.Found {
		t.Fatalf("expected found, got %+v", res.Gap)
	}
	if len(res.Paths) != 2 {
		t.Fatalf("expected 2 paths with K=2, got %d: %+v", len(res.Paths), res.Paths)
	}
	for _, p := range res.Paths {
		if p.Length != 2 {
			t.Errorf("expected length-2 path, got %d", p.Length)
		}
	}
}

func TestShortestPath_KShortest_RankedByConfidence(t *testing.T) {
	g := newGraph("A", "B", "C", "D")
	// A->B->D is all ast_resolved; A->C->D goes through a text_matched edge.
	callsTier(g, "A", "B", graph.OriginASTResolved)
	callsTier(g, "B", "D", graph.OriginASTResolved)
	callsTier(g, "A", "C", graph.OriginTextMatched)
	callsTier(g, "C", "D", graph.OriginASTResolved)
	res := New(g).ShortestPath("A", "D", Options{K: 2})
	if len(res.Paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(res.Paths))
	}
	if res.Paths[0].Nodes[1] != "B" {
		t.Errorf("expected higher-confidence path (via B) first, got %v", res.Paths[0].Nodes)
	}
	if res.Paths[0].Confidence <= res.Paths[1].Confidence {
		t.Errorf("expected paths ranked by confidence desc, got %v then %v",
			res.Paths[0].Confidence, res.Paths[1].Confidence)
	}
}

func TestShortestPath_MinTierPrune(t *testing.T) {
	g := newGraph("A", "B", "C")
	// Only route A->B->C exists, and B->C is text_matched.
	callsTier(g, "A", "B", graph.OriginASTResolved)
	callsTier(g, "B", "C", graph.OriginTextMatched)
	// Without min_tier the path is found.
	if res := New(g).ShortestPath("A", "C", Options{}); !res.Found {
		t.Fatalf("expected found without min_tier, got %+v", res.Gap)
	}
	// With min_tier=ast_resolved the weak edge is pruned and no path remains.
	res := New(g).ShortestPath("A", "C", Options{MinTier: graph.OriginASTResolved})
	if res.Found {
		t.Errorf("expected min_tier prune to drop the path, got %+v", res.Paths)
	}
}

func TestShortestPath_EdgeMatchesCrossService(t *testing.T) {
	g := newGraph("consumer", "providerContract", "handler")
	g.AddNode(&graph.Node{ID: "consumerContract", Kind: graph.KindContract})
	// consumer -> calls -> consumerContract -> matches -> providerContract -> calls -> handler
	calls(g, "consumer", "consumerContract")
	g.AddEdge(&graph.Edge{From: "consumerContract", To: "providerContract", Kind: graph.EdgeMatches, Origin: graph.OriginASTResolved})
	calls(g, "providerContract", "handler")
	res := New(g).ShortestPath("consumer", "handler", Options{})
	if !res.Found {
		t.Fatalf("expected cross-service path via EdgeMatches, got %+v", res.Gap)
	}
	if res.Paths[0].Length != 3 {
		t.Errorf("expected length 3, got %d (%v)", res.Paths[0].Length, res.Paths[0].Nodes)
	}
}

func TestShortestPath_IncludeReferencesFalse(t *testing.T) {
	g := newGraph("A", "B", "C")
	calls(g, "A", "B")
	// Only wiring from B to C is an EdgeReferences (method-value registration).
	g.AddEdge(&graph.Edge{From: "B", To: "C", Kind: graph.EdgeReferences, Origin: graph.OriginASTResolved})
	// Default (references included) finds it.
	if res := New(g).ShortestPath("A", "C", Options{IncludeReferences: true}); !res.Found {
		t.Fatalf("expected path with references, got %+v", res.Gap)
	}
	// references excluded drops the wiring hop.
	if res := New(g).ShortestPath("A", "C", Options{IncludeReferences: false}); res.Found {
		t.Errorf("expected no pure-call path, got %+v", res.Paths)
	}
}
