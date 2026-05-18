package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDefinitionBiasSignal_ExactNameMatch(t *testing.T) {
	g := newTestGraph()
	defNode := mustNode(g, "auth/token.go::Validate", "Validate", graph.KindFunction)
	defNode.FilePath = "auth/token.go"

	c := candidateFor(defNode, 0, -1)
	ctx := &Context{Graph: g}
	ctx.prepare([]*Candidate{c})

	sig := DefinitionBiasSignal{}
	if got := sig.Contribute("Validate", c, ctx); got != 1.0 {
		t.Errorf("exact name match got %v, want 1.0", got)
	}
	// Case-insensitive: an all-caps query (still classified as a
	// symbol query) must match the mixed-case name.
	if got := sig.Contribute("VALIDATE", c, ctx); got != 1.0 {
		t.Errorf("case-insensitive exact match got %v, want 1.0", got)
	}
}

func TestDefinitionBiasSignal_StemMatch(t *testing.T) {
	g := newTestGraph()
	defNode := mustNode(g, "auth/token.go::Helper", "Helper", graph.KindFunction)
	defNode.FilePath = "auth/Validate.go"

	c := candidateFor(defNode, 0, -1)
	ctx := &Context{Graph: g}
	ctx.prepare([]*Candidate{c})

	if got := (DefinitionBiasSignal{}).Contribute("Validate", c, ctx); got != 0.6 {
		t.Errorf("stem-only match got %v, want 0.6", got)
	}
}

func TestDefinitionBiasSignal_PrefixMatch(t *testing.T) {
	g := newTestGraph()
	defNode := mustNode(g, "f.go::ValidateRequest", "ValidateRequest", graph.KindFunction)
	defNode.FilePath = "f.go"

	c := candidateFor(defNode, 0, -1)
	ctx := &Context{Graph: g}
	ctx.prepare([]*Candidate{c})

	if got := (DefinitionBiasSignal{}).Contribute("Validate", c, ctx); got != 0.4 {
		t.Errorf("prefix-only match got %v, want 0.4", got)
	}
}

func TestDefinitionBiasSignal_NLQueryReturnsZero(t *testing.T) {
	g := newTestGraph()
	defNode := mustNode(g, "f.go::Validate", "Validate", graph.KindFunction)
	defNode.FilePath = "f.go"

	c := candidateFor(defNode, 0, -1)
	ctx := &Context{Graph: g}
	ctx.prepare([]*Candidate{c})

	// Multi-word query → IsSymbolQuery=false → signal stays out.
	if got := (DefinitionBiasSignal{}).Contribute("validate user token", c, ctx); got != 0 {
		t.Errorf("NL query got %v, want 0 (signal gated by IsSymbolQuery)", got)
	}
}

func TestDefinitionBiasSignal_NonDefinitionKindZero(t *testing.T) {
	g := newTestGraph()
	// Imports / packages are not definition kinds — even a name match
	// must NOT trigger the bias.
	imp := mustNode(g, "f.go::import:auth", "Validate", graph.KindImport)
	imp.FilePath = "f.go"

	c := candidateFor(imp, 0, -1)
	ctx := &Context{Graph: g}
	ctx.prepare([]*Candidate{c})

	if got := (DefinitionBiasSignal{}).Contribute("Validate", c, ctx); got != 0 {
		t.Errorf("import-kind match got %v, want 0 (not a definition)", got)
	}
}

func TestDefinitionBiasSignal_NilSafety(t *testing.T) {
	sig := DefinitionBiasSignal{}
	if got := sig.Contribute("FooBar", nil, &Context{}); got != 0 {
		t.Errorf("nil candidate got %v, want 0", got)
	}
	if got := sig.Contribute("FooBar", &Candidate{}, &Context{}); got != 0 {
		t.Errorf("nil node got %v, want 0", got)
	}
}

func TestDefinitionBiasSignal_DefinitionRanksAboveUseSite(t *testing.T) {
	g := newTestGraph()
	def := mustNode(g, "auth.go::Validate", "Validate", graph.KindFunction)
	def.FilePath = "auth.go"
	use := mustNode(g, "main.go::caller", "caller", graph.KindFunction)
	use.FilePath = "main.go"

	cands := []*Candidate{
		candidateFor(use, 0, -1), // use site has the better BM25 rank
		candidateFor(def, 1, -1),
	}

	p := New(DefaultSignals(), DefaultWeights())
	out := p.Rerank("Validate", cands, &Context{Graph: g})

	if out[0].Node.ID != def.ID {
		t.Errorf("expected definition first, got %s then %s (definition-bias must outrank a one-place BM25 lead from a use site)",
			out[0].Node.ID, out[1].Node.ID)
	}
}
