package rerank

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// --- BM25 + Semantic signals -----------------------------------------

func TestBM25SignalRankToScore(t *testing.T) {
	cases := map[int]float64{
		-1: 0,
		0:  1.0,            // top hit normalised to 1.0
		1:  61.0 / 62.0,    // 1 / (60 + 2) over the top-norm
		60: 61.0 / 121.0,   // half-decay at rank 60
		999: 61.0 / 1060.0, // far tail still > 0
	}
	for rank, want := range cases {
		got := BM25Signal{}.Contribute("q", &Candidate{TextRank: rank}, &Context{})
		if !floatNear(got, want, 1e-9) {
			t.Errorf("rank=%d: got %v, want %v", rank, got, want)
		}
	}
}

func TestSemanticSignalRankToScore(t *testing.T) {
	got := SemanticSignal{}.Contribute("q", &Candidate{VectorRank: 0}, &Context{})
	if !floatNear(got, 1.0, 1e-9) {
		t.Errorf("top vector rank: got %v want 1.0", got)
	}
	got = SemanticSignal{}.Contribute("q", &Candidate{VectorRank: -1}, &Context{})
	if got != 0 {
		t.Errorf("absent vector rank: got %v want 0", got)
	}
}

// --- FanIn / FanOut --------------------------------------------------

func TestFanInOutSignalsNormalised(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	c := mustNode(g, "f.go::C", "C", graph.KindFunction)

	// A has 3 callers, B has 1, C has 0.
	g.AddEdge(&graph.Edge{From: "x1", To: a.ID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "x2", To: a.ID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "x3", To: a.ID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "x4", To: b.ID, Kind: graph.EdgeCalls})
	// A has 2 callees, B has 0, C has 1.
	g.AddEdge(&graph.Edge{From: a.ID, To: "y1", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: a.ID, To: "y2", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: c.ID, To: "y3", Kind: graph.EdgeCalls})

	ctx := &Context{Graph: g}
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1), candidateFor(c, 2, -1)}
	ctx.prepare(cands)

	fi := FanInSignal{}
	if got := fi.Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("A fan-in: got %v want 1.0", got)
	}
	if got := fi.Contribute("q", cands[2], ctx); got != 0 {
		t.Errorf("C fan-in: got %v want 0", got)
	}

	fo := FanOutSignal{}
	if got := fo.Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("A fan-out: got %v want 1.0", got)
	}
}

func TestFanInSignalNoGraphReturnsZero(t *testing.T) {
	a := &graph.Node{ID: "x", Name: "x", Kind: graph.KindFunction}
	if got := (FanInSignal{}).Contribute("q", &Candidate{Node: a}, &Context{}); got != 0 {
		t.Errorf("no-graph fan-in: got %v want 0", got)
	}
}

// --- Churn -----------------------------------------------------------

func TestChurnSignalFromMeta(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	a.Meta = map[string]any{"churn": 5}
	b.Meta = map[string]any{"churn": 1}

	ctx := &Context{Graph: g}
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1)}
	ctx.prepare(cands)

	cs := ChurnSignal{}
	if got := cs.Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("hottest churn: got %v want 1.0", got)
	}
	if got := cs.Contribute("q", cands[1], ctx); !(got > 0 && got < 1) {
		t.Errorf("medium churn: got %v want in (0,1)", got)
	}
}

func TestChurnSignalChurnOfHookWins(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	a.Meta = map[string]any{"churn": 1}

	ctx := &Context{Graph: g, ChurnOf: func(id string) int {
		if id == a.ID {
			return 99
		}
		return 0
	}}
	cands := []*Candidate{candidateFor(a, 0, -1)}
	ctx.prepare(cands)
	if got := (ChurnSignal{}).Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("ChurnOf override should produce 1.0, got %v", got)
	}
}

// --- Community ------------------------------------------------------

func TestCommunitySignalTopicCluster(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	c := mustNode(g, "f.go::C", "C", graph.KindFunction)

	communities := map[string]string{a.ID: "c1", b.ID: "c1", c.ID: "c2"}
	ctx := &Context{Graph: g, CommunityOf: func(id string) string { return communities[id] }}
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1), candidateFor(c, 2, -1)}
	ctx.prepare(cands)

	cs := CommunitySignal{}
	// A and B share c1 (cluster size 2), C alone in c2 (size 1).
	// maxCommunityCount = 2, so A and B get 2/2 = 1.0, C gets 0
	// (cluster-of-one is not a cluster).
	if got := cs.Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("A in topic cluster: got %v want 1.0", got)
	}
	if got := cs.Contribute("q", cands[2], ctx); got != 0 {
		t.Errorf("C alone: got %v want 0", got)
	}
}

func TestCommunitySignalRepoLocality(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	a.RepoPrefix = "myrepo"
	b.RepoPrefix = "otherrepo"

	ctx := &Context{Graph: g, RepoPrefix: "myrepo"}
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1)}
	ctx.prepare(cands)

	cs := CommunitySignal{}
	if got := cs.Contribute("q", cands[0], ctx); !floatNear(got, 1.0, 1e-9) {
		t.Errorf("same-repo: got %v want 1.0", got)
	}
	if got := cs.Contribute("q", cands[1], ctx); got != 0 {
		t.Errorf("other-repo: got %v want 0", got)
	}
}

func TestCommunitySignalProjectLocalityFallback(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	a.RepoPrefix = "repo1"
	a.ProjectID = "myproj"

	ctx := &Context{Graph: g, ProjectID: "myproj"}
	cands := []*Candidate{candidateFor(a, 0, -1)}
	ctx.prepare(cands)

	if got := (CommunitySignal{}).Contribute("q", cands[0], ctx); !floatNear(got, 0.7, 1e-9) {
		t.Errorf("same-project: got %v want 0.7", got)
	}
}

// --- MinHash --------------------------------------------------------

func TestMinHashSignalCohesion(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	c := mustNode(g, "f.go::C", "C", graph.KindFunction)

	g.AddEdge(&graph.Edge{From: a.ID, To: b.ID, Kind: graph.EdgeSimilarTo, Meta: map[string]any{"similarity": 0.8}})
	g.AddEdge(&graph.Edge{From: b.ID, To: a.ID, Kind: graph.EdgeSimilarTo, Meta: map[string]any{"similarity": 0.8}})

	ctx := &Context{Graph: g}
	cands := []*Candidate{candidateFor(a, 0, -1), candidateFor(b, 1, -1), candidateFor(c, 2, -1)}
	ctx.prepare(cands)

	cs := MinHashSignal{}
	// A and B are both in cluster with sim=0.8; C is isolated.
	if got := cs.Contribute("q", cands[0], ctx); !floatNear(got, 0.8, 1e-9) {
		t.Errorf("A in similar cluster: got %v want 0.8", got)
	}
	if got := cs.Contribute("q", cands[2], ctx); got != 0 {
		t.Errorf("C isolated: got %v want 0", got)
	}
}

// --- Recency --------------------------------------------------------

func TestRecencySignalDecay(t *testing.T) {
	now := time.Now().Unix()
	day := int64(86400)
	cases := []struct {
		dtDays int64
		want   float64
	}{
		{0, 1.0},
		{30, 0.5},
		{60, 0.25},
		{120, 0.0625},
	}
	for _, c := range cases {
		n := &graph.Node{ID: "x", Kind: graph.KindFunction, Meta: map[string]any{
			"last_authored": map[string]any{"timestamp": now - c.dtDays*day},
		}}
		got := (RecencySignal{}).Contribute("q", &Candidate{Node: n}, &Context{Now: now})
		if !floatNear(got, c.want, 1e-3) {
			t.Errorf("dt=%dd: got %v want %v", c.dtDays, got, c.want)
		}
	}
}

func TestRecencySignalNoMetaReturnsZero(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindFunction}
	if got := (RecencySignal{}).Contribute("q", &Candidate{Node: n}, &Context{Now: time.Now().Unix()}); got != 0 {
		t.Errorf("no last_authored: got %v want 0", got)
	}
}

// --- Signature signals ----------------------------------------------

func TestAPISignatureSignalOverlap(t *testing.T) {
	n := &graph.Node{
		ID: "x", Kind: graph.KindFunction, Name: "validateToken",
		Meta: map[string]any{"signature": "func validateToken(t string) error"},
	}
	got := (APISignatureSignal{}).Contribute("validate token", &Candidate{Node: n}, &Context{})
	if !floatNear(got, 1.0, 1e-9) {
		t.Errorf("full overlap: got %v want 1.0", got)
	}
	got = (APISignatureSignal{}).Contribute("hash password", &Candidate{Node: n}, &Context{})
	if got != 0 {
		t.Errorf("no overlap: got %v want 0", got)
	}
}

func TestAPISignatureSignalIgnoresNonFunctions(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindType, Name: "User"}
	if got := (APISignatureSignal{}).Contribute("user", &Candidate{Node: n}, &Context{}); got != 0 {
		t.Errorf("type kind: got %v want 0", got)
	}
}

func TestTypeSignatureSignalNameOverlap(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindType, Name: "UserRecord"}
	got := (TypeSignatureSignal{}).Contribute("user record", &Candidate{Node: n}, &Context{})
	if !floatNear(got, 1.0, 1e-9) {
		t.Errorf("type name overlap: got %v want 1.0", got)
	}
}

func TestTypeSignatureIgnoresFunctions(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindFunction, Name: "ParseUser"}
	if got := (TypeSignatureSignal{}).Contribute("user", &Candidate{Node: n}, &Context{}); got != 0 {
		t.Errorf("function kind: got %v want 0", got)
	}
}

// --- Feedback (session) ---------------------------------------------

func TestFeedbackSignalMaxOfSources(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindFunction}
	ctx := &Context{
		FeedbackOf:      func(string) float64 { return 0.5 },
		FrecencyBoostOf: func(string) float64 { return 1.5 }, // saturates → 1.0
		ComboBoostOf:    func(string) float64 { return 1.0 }, // no boost
	}
	got := (FeedbackSignal{}).Contribute("q", &Candidate{Node: n}, ctx)
	if !floatNear(got, 1.0, 1e-9) {
		t.Errorf("max-merge feedback: got %v want 1.0", got)
	}
}

func TestFeedbackSignalNegativeFeedbackIgnored(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindFunction}
	ctx := &Context{
		FeedbackOf: func(string) float64 { return -1.0 },
	}
	if got := (FeedbackSignal{}).Contribute("q", &Candidate{Node: n}, ctx); got != 0 {
		t.Errorf("negative feedback: got %v want 0", got)
	}
}

func TestFeedbackSignalAbsentHooksReturnsZero(t *testing.T) {
	n := &graph.Node{ID: "x", Kind: graph.KindFunction}
	if got := (FeedbackSignal{}).Contribute("q", &Candidate{Node: n}, &Context{}); got != 0 {
		t.Errorf("no hooks: got %v want 0", got)
	}
}

// --- End-to-end mini integration ------------------------------------

func TestPipelineIntegratesAllSignalsWithoutPanic(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "validateToken", graph.KindFunction)
	a.Meta = map[string]any{
		"signature":     "func validateToken(t string) error",
		"churn":         3,
		"last_authored": map[string]any{"timestamp": time.Now().Unix()},
	}
	a.RepoPrefix = "myrepo"
	b := mustNode(g, "f.go::B", "B", graph.KindFunction)
	b.RepoPrefix = "otherrepo"

	g.AddEdge(&graph.Edge{From: "x1", To: a.ID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: a.ID, To: b.ID, Kind: graph.EdgeSimilarTo, Meta: map[string]any{"similarity": 0.9}})

	ctx := &Context{
		Graph:           g,
		RepoPrefix:      "myrepo",
		CommunityOf:     func(string) string { return "c1" },
		ChurnOf:         func(string) int { return 0 },
		FeedbackOf:      func(string) float64 { return 0.3 },
		FrecencyBoostOf: func(string) float64 { return 1.2 },
		ComboBoostOf:    func(string) float64 { return 1.0 },
	}
	p := NewDefault()
	cands := []*Candidate{candidateFor(a, 0, 0), candidateFor(b, 1, 1)}
	out := p.Rerank("validate token", cands, ctx)
	if len(out) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out))
	}
	if out[0].Node.ID != a.ID {
		t.Errorf("expected A first (every signal favors it), got %s", out[0].Node.ID)
	}
	// Every signal should have produced a value for A.
	for _, name := range AllSignalNames() {
		if _, ok := out[0].Signals[name]; !ok {
			t.Errorf("expected signal %q recorded", name)
		}
	}
}

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
