package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestClassifyQuery(t *testing.T) {
	cases := map[string]QueryClass{
		"validateToken":          QueryClassSymbol,
		"HTTPServer":             QueryClassSymbol,
		"pkg.Type":               QueryClassSymbol,
		"validate_user_token":    QueryClassSymbol,
		"internal/auth/token.go": QueryClassPath,
		"auth/handler":           QueryClassPath,
		"func(ctx) error":        QueryClassSignature,
		"(string) bool":          QueryClassSignature,
		"map[string]int => T":    QueryClassSignature,
		"how does auth refresh":  QueryClassConcept,
		"validate user token":    QueryClassConcept,
		"":                       QueryClassConcept,
	}
	for q, want := range cases {
		if got := ClassifyQuery(q); got != want {
			t.Errorf("ClassifyQuery(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestParseQueryClass(t *testing.T) {
	valid := map[string]QueryClass{
		"":          QueryClassUnknown,
		"auto":      QueryClassUnknown,
		"symbol":    QueryClassSymbol,
		"CONCEPT":   QueryClassConcept,
		" path ":    QueryClassPath,
		"signature": QueryClassSignature,
	}
	for s, want := range valid {
		got, ok := ParseQueryClass(s)
		if !ok || got != want {
			t.Errorf("ParseQueryClass(%q) = (%v, %v), want (%v, true)", s, got, ok, want)
		}
	}
	if _, ok := ParseQueryClass("bogus"); ok {
		t.Errorf("ParseQueryClass(bogus) should report ok=false")
	}
}

func TestQueryClassString(t *testing.T) {
	cases := map[QueryClass]string{
		QueryClassUnknown:   "unknown",
		QueryClassSymbol:    "symbol",
		QueryClassConcept:   "concept",
		QueryClassPath:      "path",
		QueryClassSignature: "signature",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", c, got, want)
		}
	}
}

func TestAlphaForClassOrdering(t *testing.T) {
	// Path leans hardest on BM25 (smallest α), concept the least.
	if !(AlphaForClass(QueryClassPath) < AlphaForClass(QueryClassSymbol)) {
		t.Errorf("path α must be below symbol α")
	}
	if !(AlphaForClass(QueryClassSymbol) < AlphaForClass(QueryClassSignature)) {
		t.Errorf("symbol α must be below signature α")
	}
	if !(AlphaForClass(QueryClassSignature) < AlphaForClass(QueryClassConcept)) {
		t.Errorf("signature α must be below concept α")
	}
	// Unknown falls back to the concept (natural-language) blend.
	if AlphaForClass(QueryClassUnknown) != AlphaForClass(QueryClassConcept) {
		t.Errorf("unknown α must fall back to concept α")
	}
}

func TestClassWeightMultiplier(t *testing.T) {
	// Concept is the neutral baseline — 1.0 across the board.
	if ClassWeightMultiplier(QueryClassConcept, SignalBM25) != 1.0 ||
		ClassWeightMultiplier(QueryClassConcept, SignalSemantic) != 1.0 {
		t.Errorf("concept class must be the neutral 1.0/1.0 baseline")
	}
	// Symbol / path / signature push BM25 up and semantic down.
	for _, c := range []QueryClass{QueryClassSymbol, QueryClassPath, QueryClassSignature} {
		if !(ClassWeightMultiplier(c, SignalBM25) > 1.0) {
			t.Errorf("%v must raise the bm25 weight", c)
		}
		if !(ClassWeightMultiplier(c, SignalSemantic) < 1.0) {
			t.Errorf("%v must lower the semantic weight", c)
		}
	}
	// Non-text signals and an unknown class are untouched.
	if ClassWeightMultiplier(QueryClassSymbol, SignalFanIn) != 1.0 {
		t.Errorf("non-text signals must not be class-scaled")
	}
	if ClassWeightMultiplier(QueryClassUnknown, SignalBM25) != 1.0 {
		t.Errorf("unknown class must not scale any signal")
	}
}

func TestRerank_QueryClassTunesTextVsSemantic(t *testing.T) {
	g := newTestGraph()
	textOnly := mustNode(g, "f.go::TextOnly", "TextOnly", graph.KindFunction)
	vecOnly := mustNode(g, "f.go::VecOnly", "VecOnly", graph.KindFunction)
	weights := map[string]float64{SignalBM25: 1.0, SignalSemantic: 1.0}
	p := New(DefaultSignals(), weights)

	score := func(class QueryClass, id string) float64 {
		cands := []*Candidate{
			{Node: textOnly, TextRank: 0, VectorRank: -1},
			{Node: vecOnly, TextRank: -1, VectorRank: 0},
		}
		p.Rerank("q", cands, &Context{Graph: g, QueryClass: class})
		for _, c := range cands {
			if c.Node.ID == id {
				return c.Score
			}
		}
		t.Fatalf("candidate %s missing from rerank output", id)
		return 0
	}

	// The symbol class boosts BM25 and trims semantic relative to the
	// neutral concept baseline.
	if !(score(QueryClassSymbol, textOnly.ID) > score(QueryClassConcept, textOnly.ID)) {
		t.Errorf("symbol class must raise the text-only candidate's score")
	}
	if !(score(QueryClassSymbol, vecOnly.ID) < score(QueryClassConcept, vecOnly.ID)) {
		t.Errorf("symbol class must lower the semantic-only candidate's score")
	}
}

func TestRerank_AutoClassifiesWhenUnset(t *testing.T) {
	g := newTestGraph()
	a := mustNode(g, "f.go::A", "A", graph.KindFunction)
	p := New(DefaultSignals(), map[string]float64{SignalBM25: 1.0})
	ctx := &Context{Graph: g} // QueryClass left at the zero value.
	p.Rerank("internal/auth/token.go", []*Candidate{{Node: a, TextRank: 0, VectorRank: -1}}, ctx)
	if ctx.QueryClass != QueryClassPath {
		t.Errorf("Rerank must auto-classify an unset QueryClass; got %v", ctx.QueryClass)
	}
}
