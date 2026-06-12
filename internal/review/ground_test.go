package review

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

// newGroundGraph builds a tiny in-memory graph:
//
//   - app/svc.go::WithLoop    — a function with loop_depth=2 (a real loop)
//   - app/svc.go::NoLoop      — a function with no loop metadata
//   - app/svc.go::Mutates     — a function with a resolved write edge
//   - app/svc.go::ReadOnly    — a function with no mutating edge (only a call)
func newGroundGraph() graph.Store {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "app/svc.go::WithLoop", Kind: graph.KindFunction, Name: "WithLoop",
		FilePath: "app/svc.go", Language: "go", Meta: map[string]any{"loop_depth": 2},
	})
	g.AddNode(&graph.Node{
		ID: "app/svc.go::NoLoop", Kind: graph.KindFunction, Name: "NoLoop",
		FilePath: "app/svc.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "app/svc.go::Mutates", Kind: graph.KindFunction, Name: "Mutates",
		FilePath: "app/svc.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "app/svc.go::ReadOnly", Kind: graph.KindFunction, Name: "ReadOnly",
		FilePath: "app/svc.go", Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "app/svc.go::cache", Kind: graph.KindField, Name: "cache",
		FilePath: "app/svc.go", Language: "go",
	})
	// Mutates writes the shared field; ReadOnly only calls something.
	g.AddEdge(&graph.Edge{From: "app/svc.go::Mutates", To: "app/svc.go::cache", Kind: graph.EdgeWrites})
	g.AddEdge(&graph.Edge{From: "app/svc.go::ReadOnly", To: "app/svc.go::cache", Kind: graph.EdgeReads})
	return g
}

func TestGroundLoopCall(t *testing.T) {
	g := newGroundGraph()

	// Graph-confirmed N+1: the enclosing function has a loop.
	keep := GroundLoopCall(g, astquery.Match{Detector: "go-loop-query-call", SymbolID: "app/svc.go::WithLoop"})
	require.True(t, keep, "an N+1 inside a function with loop_depth>=1 must be kept")

	// Graph-refuted N+1: the enclosing function provably has no loop.
	keep = GroundLoopCall(g, astquery.Match{Detector: "go-loop-query-call", SymbolID: "app/svc.go::NoLoop"})
	require.False(t, keep, "an N+1 whose enclosing function has no loop must be dropped")

	// No symbol / nil graph → keep (AST verdict stands).
	require.True(t, GroundLoopCall(g, astquery.Match{Detector: "go-loop-query-call"}))
	require.True(t, GroundLoopCall(nil, astquery.Match{Detector: "go-loop-query-call", SymbolID: "x"}))
}

func TestGroundCheckThenAct(t *testing.T) {
	g := newGroundGraph()

	keep := GroundCheckThenAct(g, astquery.Match{Detector: "go-check-then-act-map", SymbolID: "app/svc.go::Mutates"})
	require.True(t, keep, "a check-then-act in a function that writes state must be kept")

	keep = GroundCheckThenAct(g, astquery.Match{Detector: "go-check-then-act-map", SymbolID: "app/svc.go::ReadOnly"})
	require.False(t, keep, "a check-then-act in a function with no mutating edge must be dropped")
}

// TestGroundReviewMatches proves the post-pass drops graph-refuted N+1
// / check-then-act rows while keeping genuine ones and passing every
// decidable rule through untouched.
func TestGroundReviewMatches(t *testing.T) {
	g := newGroundGraph()

	in := []astquery.Match{
		// Refuted N+1 — enclosing function has no loop.
		{Detector: "go-loop-query-call", File: "app/svc.go", Line: 4, SymbolID: "app/svc.go::NoLoop"},
		// Genuine N+1 — enclosing function has a loop.
		{Detector: "go-loop-query-call", File: "app/svc.go", Line: 9, SymbolID: "app/svc.go::WithLoop"},
		// Refuted check-then-act — enclosing function performs no mutation.
		{Detector: "go-check-then-act-map", File: "app/svc.go", Line: 14, SymbolID: "app/svc.go::ReadOnly"},
		// Genuine check-then-act — enclosing function writes state.
		{Detector: "go-check-then-act-map", File: "app/svc.go", Line: 19, SymbolID: "app/svc.go::Mutates"},
		// Decidable rule — always survives, even with an unknown symbol.
		{Detector: "go-inverted-err-check", File: "app/svc.go", Line: 24, SymbolID: "app/svc.go::NoLoop"},
		// Python decidable rule — survives.
		{Detector: "py-self-comparison", File: "app/svc.py", Line: 2, SymbolID: ""},
	}

	out := GroundReviewMatches(g, in)

	kept := map[string]bool{}
	for _, m := range out {
		kept[m.Detector+"@"+itoa(m.Line)] = true
	}

	require.False(t, kept["go-loop-query-call@4"], "graph-refuted N+1 must be dropped")
	require.True(t, kept["go-loop-query-call@9"], "genuine N+1 must survive")
	require.False(t, kept["go-check-then-act-map@14"], "graph-refuted check-then-act must be dropped")
	require.True(t, kept["go-check-then-act-map@19"], "genuine check-then-act must survive")
	require.True(t, kept["go-inverted-err-check@24"], "decidable rule must pass through")
	require.True(t, kept["py-self-comparison@2"], "decidable rule must pass through")
	require.Len(t, out, 4, "exactly the two refuted rows should be dropped")
}

// TestGroundReviewMatchesNilGraph confirms a nil store grounds nothing
// — findings degrade to pure-AST behaviour instead of being dropped.
func TestGroundReviewMatchesNilGraph(t *testing.T) {
	in := []astquery.Match{
		{Detector: "go-loop-query-call", SymbolID: "x"},
		{Detector: "go-check-then-act-map", SymbolID: "y"},
	}
	out := GroundReviewMatches(nil, in)
	require.Len(t, out, 2, "nil graph must keep every row")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
