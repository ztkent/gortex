package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileCoherenceSignal_SoloFileContributesZero(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "fileA.go::A", "A", graph.KindFunction)
	a.FilePath = "fileA.go"
	cands := []*Candidate{candidateFor(a, 0, -1)}

	ctx := &Context{Graph: g}
	ctx.prepare(cands)

	if got := (FileCoherenceSignal{}).Contribute("q", cands[0], ctx); got != 0 {
		t.Errorf("solo-file candidate got %v, want 0", got)
	}
}

func TestFileCoherenceSignal_MultiChunkFileGetsBoost(t *testing.T) {
	g := newTestGraph()
	// File A has 3 candidates at ranks 0, 2, 9 → strong evidence.
	a1 := mustNode(g, "fileA.go::A1", "A1", graph.KindFunction)
	a1.FilePath = "fileA.go"
	a2 := mustNode(g, "fileA.go::A2", "A2", graph.KindFunction)
	a2.FilePath = "fileA.go"
	a3 := mustNode(g, "fileA.go::A3", "A3", graph.KindFunction)
	a3.FilePath = "fileA.go"
	// File B has 2 candidates at ranks 4, 7 → moderate evidence.
	b1 := mustNode(g, "fileB.go::B1", "B1", graph.KindFunction)
	b1.FilePath = "fileB.go"
	b2 := mustNode(g, "fileB.go::B2", "B2", graph.KindFunction)
	b2.FilePath = "fileB.go"
	// File C has 1 candidate → solo, no boost.
	c1 := mustNode(g, "fileC.go::C1", "C1", graph.KindFunction)
	c1.FilePath = "fileC.go"

	cands := []*Candidate{
		candidateFor(a1, 0, -1),
		candidateFor(a2, 2, -1),
		candidateFor(a3, 9, -1),
		candidateFor(b1, 4, -1),
		candidateFor(b2, 7, -1),
		candidateFor(c1, 1, -1),
	}
	ctx := &Context{Graph: g}
	ctx.prepare(cands)

	sig := FileCoherenceSignal{}
	scoreA := sig.Contribute("q", cands[0], ctx)
	scoreB := sig.Contribute("q", cands[3], ctx)
	scoreC := sig.Contribute("q", cands[5], ctx)

	// A's per-file sum = 1 + 1/3 + 1/10 = 1.433…
	// B's per-file sum = 1/5 + 1/8 = 0.325
	// max = A's sum → A scores 1.0, B scores 0.325/1.433 ≈ 0.227, C scores 0.
	if scoreA != 1.0 {
		t.Errorf("file-A leader got %v, want 1.0 (top of multi-chunk file)", scoreA)
	}
	if scoreB <= 0 || scoreB >= scoreA {
		t.Errorf("file-B leader got %v, want a positive fraction less than file-A's %v", scoreB, scoreA)
	}
	if scoreC != 0 {
		t.Errorf("solo-file candidate got %v, want 0", scoreC)
	}
	// Every candidate in a multi-chunk file gets the same per-file
	// score — the signal isn't "only the leader"; it's "this file is
	// strong evidence so every chunk from it benefits".
	scoreA3 := sig.Contribute("q", cands[2], ctx)
	if scoreA != scoreA3 {
		t.Errorf("file-A non-leader got %v but leader got %v; expected equal per-file boost", scoreA3, scoreA)
	}
}

func TestFileCoherenceSignal_NoFilePathOrZeroSums(t *testing.T) {
	g := newTestGraph()
	n := mustNode(g, "::X", "X", graph.KindFunction)
	n.FilePath = ""
	cands := []*Candidate{candidateFor(n, 0, -1), candidateFor(n, 1, -1)}
	ctx := &Context{Graph: g}
	ctx.prepare(cands)
	if got := (FileCoherenceSignal{}).Contribute("q", cands[0], ctx); got != 0 {
		t.Errorf("empty FilePath got %v, want 0", got)
	}

	// File with two candidates both at TextRank=-1 (no rank info) →
	// fileScoreSum is 0 for that file → signal returns 0 (no boost).
	g2 := newTestGraph()
	a := mustNode(g2, "f.go::A", "A", graph.KindFunction)
	a.FilePath = "f.go"
	b := mustNode(g2, "f.go::B", "B", graph.KindFunction)
	b.FilePath = "f.go"
	cands2 := []*Candidate{candidateFor(a, -1, -1), candidateFor(b, -1, -1)}
	ctx2 := &Context{Graph: g2}
	ctx2.prepare(cands2)
	if got := (FileCoherenceSignal{}).Contribute("q", cands2[0], ctx2); got != 0 {
		t.Errorf("no-rank multi-candidate file got %v, want 0", got)
	}
}

func TestFileCoherenceSignal_NilSafety(t *testing.T) {
	sig := FileCoherenceSignal{}
	if got := sig.Contribute("q", nil, &Context{}); got != 0 {
		t.Errorf("nil candidate got %v, want 0", got)
	}
	if got := sig.Contribute("q", &Candidate{}, &Context{}); got != 0 {
		t.Errorf("nil node got %v, want 0", got)
	}
	if got := sig.Contribute("q", &Candidate{Node: &graph.Node{}}, nil); got != 0 {
		t.Errorf("nil context got %v, want 0", got)
	}
}
