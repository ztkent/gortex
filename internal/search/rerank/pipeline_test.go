package rerank

import (
	"math"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// --- Test helpers -----------------------------------------------------

// newTestGraph returns an empty graph; tests populate it via AddNode /
// AddEdge directly. Avoiding the indexer means tests run in
// microseconds and don't need a tree-sitter binary.
func newTestGraph() *graph.Graph { return graph.New() }

func mustNode(g *graph.Graph, id, name string, kind graph.NodeKind) *graph.Node {
	n := &graph.Node{ID: id, Name: name, Kind: kind, FilePath: "file.go"}
	g.AddNode(n)
	return n
}

func candidateFor(n *graph.Node, textRank, vecRank int) *Candidate {
	return &Candidate{Node: n, TextRank: textRank, VectorRank: vecRank}
}

// --- Pipeline tests ---------------------------------------------------

func TestPipelineEmptyInputReturnsEmpty(t *testing.T) {
	p := NewDefault()
	got := p.Rerank("q", nil, nil)
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}

func TestPipelineBM25OrderPreservedWhenOnlyTextSignalActive(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	c := mustNode(g, "f.go::C", "C", graph.KindFunction)

	weights := map[string]float64{SignalBM25: 1.0}
	p := New(DefaultSignals(), weights)
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1), candidateFor(c, 2, -1)}
	out := p.Rerank("anything", cands, &Context{Graph: g})

	if out[0].Node.ID != a.ID || out[1].Node.ID != b.ID || out[2].Node.ID != c.ID {
		t.Fatalf("expected A,B,C order, got %v", []string{out[0].Node.ID, out[1].Node.ID, out[2].Node.ID})
	}
	if out[0].Score <= out[1].Score || out[1].Score <= out[2].Score {
		t.Fatalf("expected monotonically decreasing scores, got %v %v %v",
			out[0].Score, out[1].Score, out[2].Score)
	}
}

func TestPipelineSemanticPromotesTopVectorHit(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)

	// A is BM25 #2, B is BM25 #1 but semantic #1 with high weight.
	weights := map[string]float64{SignalBM25: 1.0, SignalSemantic: 5.0}
	p := New(DefaultSignals(), weights)
	cands := []*Candidate{candidateFor(a, 1, 5), candidateFor(b, 0, 0)}
	out := p.Rerank("q", cands, &Context{Graph: g})
	if out[0].Node.ID != b.ID {
		t.Fatalf("expected B first (semantic-driven), got %s", out[0].Node.ID)
	}
}

func TestPipelinePerSignalContributionsRecorded(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	p := NewDefault()
	cands := []*Candidate{candidateFor(a, 0, -1)}
	p.Rerank("q", cands, &Context{Graph: g})
	if cands[0].Signals == nil {
		t.Fatalf("expected Signals to be populated")
	}
	if _, ok := cands[0].Signals[SignalBM25]; !ok {
		t.Fatalf("expected bm25 signal in contributions")
	}
}

func TestPipelineZeroWeightSkipsSignal(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	weights := DefaultWeights()
	weights[SignalBM25] = 0
	p := New(DefaultSignals(), weights)
	cands := []*Candidate{candidateFor(a, 0, -1)}
	p.Rerank("q", cands, &Context{Graph: g})
	if _, ok := cands[0].Signals[SignalBM25]; ok {
		t.Fatalf("expected bm25 to be skipped under weight=0")
	}
}

func TestPipelineSetWeight(t *testing.T) {
	p := NewDefault()
	p.SetWeight(SignalBM25, 7.5)
	if got := p.Weights()[SignalBM25]; got != 7.5 {
		t.Fatalf("expected weight 7.5, got %v", got)
	}
}

func TestPipelineWeightsReturnsCopy(t *testing.T) {
	p := NewDefault()
	w := p.Weights()
	w[SignalBM25] = 999
	if got := p.Weights()[SignalBM25]; got == 999 {
		t.Fatalf("expected Weights() to return a defensive copy")
	}
}

func TestPipelineStableSortDeterministic(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	c := mustNode(g, "f.go::C", "C", graph.KindFunction)

	// Three candidates with identical scores — secondary key is
	// TextRank, then node ID. Repeated runs must produce same order.
	weights := map[string]float64{} // no signals active → all scores 0
	p := New(DefaultSignals(), weights)

	for range 5 {
		cands := []*Candidate{candidateFor(c, 2, -1), candidateFor(a, 0, -1), candidateFor(b, 1, -1)}
		out := p.Rerank("q", cands, &Context{Graph: g})
		// All scores are 0 → secondary key (TextRank ascending) → A,B,C.
		if out[0].Node.ID != a.ID || out[1].Node.ID != b.ID || out[2].Node.ID != c.ID {
			t.Fatalf("tie-break order not stable: %s %s %s",
				out[0].Node.ID, out[1].Node.ID, out[2].Node.ID)
		}
	}
}

func TestAllSignalNamesCoversDefaultSignals(t *testing.T) {
	want := AllSignalNames()
	got := map[string]bool{}
	for _, sig := range DefaultSignals() {
		got[sig.Name()] = true
	}
	if len(want) != len(got) {
		t.Fatalf("AllSignalNames count %d != DefaultSignals %d", len(want), len(got))
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("AllSignalNames lists %q but DefaultSignals has no such signal", n)
		}
	}
}

// --- Helper / tokens tests -------------------------------------------

func TestTokenizeSplitsCamelAndSnake(t *testing.T) {
	cases := map[string][]string{
		"ParseHTTPHeader":      {"parse", "http", "header"},
		"validate_user_token":  {"validate", "user", "token"},
		"user2Token":           {"user", "2", "token"},
		"hashPasswordSha256":   {"hash", "password", "sha", "256"},
		"":                     nil,
		"ABC":                  {"abc"},
		"already-clean tokens": {"already", "clean", "tokens"},
	}
	for in, want := range cases {
		got := tokenize(in)
		if !sliceEq(got, want) {
			t.Errorf("tokenize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestJaccardAndOverlap(t *testing.T) {
	if got := Jaccard(nil, []string{"a"}); got != 0 {
		t.Errorf("Jaccard(nil, …) = %v, want 0", got)
	}
	if got := Jaccard([]string{"a", "b"}, []string{"b", "c"}); !floatEq(got, 1.0/3.0) {
		t.Errorf("Jaccard expected 1/3, got %v", got)
	}
	if got := overlap([]string{"a", "b"}, []string{"a", "b", "c"}); got != 1.0 {
		t.Errorf("overlap expected 1.0 (full subset), got %v", got)
	}
	if got := overlap([]string{"a"}, []string{"b", "c"}); got != 0 {
		t.Errorf("overlap expected 0 (no intersection), got %v", got)
	}
}

func TestNormLogClamps(t *testing.T) {
	if got := normLog(0, 10); got != 0 {
		t.Errorf("normLog(0,10) = %v, want 0", got)
	}
	if got := normLog(5, 0); got != 0 {
		t.Errorf("normLog(5,0) = %v, want 0", got)
	}
	if got := normLog(10, 10); !floatEq(got, 1.0) {
		t.Errorf("normLog(10,10) = %v, want 1.0", got)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func floatEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
