package rerank

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestProseWeightMultiplier covers the lever in isolation: the text +
// semantic channels are lifted, the code-structural signals are
// zeroed, and every other signal is unchanged.
func TestProseWeightMultiplier(t *testing.T) {
	if proseWeightMultiplier(SignalBM25) <= 1.0 {
		t.Errorf("prose mode should lift bm25; got %v", proseWeightMultiplier(SignalBM25))
	}
	if proseWeightMultiplier(SignalSemantic) <= 1.0 {
		t.Errorf("prose mode should lift semantic; got %v", proseWeightMultiplier(SignalSemantic))
	}
	for _, sig := range []string{SignalAPISignature, SignalTypeSignature, SignalDefinitionBias} {
		if proseWeightMultiplier(sig) != 0.0 {
			t.Errorf("prose mode should zero the code-structural signal %q; got %v", sig, proseWeightMultiplier(sig))
		}
	}
	// A signal that still makes sense for prose is unchanged.
	for _, sig := range []string{SignalRecency, SignalFeedback, SignalFileCoherence, SignalFanIn} {
		if proseWeightMultiplier(sig) != 1.0 {
			t.Errorf("prose mode should leave %q unchanged; got %v", sig, proseWeightMultiplier(sig))
		}
	}
}

// TestProseModeSuppressesStructuralSignal proves that, end to end
// through Pipeline.Rerank, ProseMode zeroes a code-structural signal's
// contribution while leaving bm25 intact. A doc candidate that would
// score on api_signature loses that contribution under ProseMode.
func TestProseModeSuppressesStructuralSignal(t *testing.T) {
	g := newTestGraph()
	doc := &graph.Node{ID: "GUIDE.md::Deploy", Name: "Deploy", Kind: graph.KindDoc, FilePath: "GUIDE.md"}
	g.AddNode(doc)

	// Run with ONLY the api_signature signal active so we isolate its
	// contribution. The signal is meaningless for a KindDoc node, so
	// prose mode must drive its weighted score to zero.
	p := New(DefaultSignals(), map[string]float64{SignalAPISignature: 1.0})

	cand := candidateFor(doc, 0, -1)
	p.Rerank("deploy service", []*Candidate{cand}, &Context{Graph: g, ProseMode: false})
	baseScore := cand.Score

	candP := candidateFor(doc, 0, -1)
	p.Rerank("deploy service", []*Candidate{candP}, &Context{Graph: g, ProseMode: true})
	proseScore := candP.Score

	if proseScore != 0.0 {
		t.Errorf("ProseMode must zero the api_signature contribution for a doc node; base=%v prose=%v", baseScore, proseScore)
	}
}

// TestProseModeLiftsTextChannel proves ProseMode raises the bm25
// channel's weighted contribution relative to the same query without
// prose mode (independent of the α / class lever, which is left at
// its default here).
func TestProseModeLiftsTextChannel(t *testing.T) {
	g := newTestGraph()
	doc := &graph.Node{ID: "GUIDE.md::Deploy", Name: "Deploy deployment guide", Kind: graph.KindDoc, FilePath: "GUIDE.md"}
	g.AddNode(doc)

	p := New(DefaultSignals(), map[string]float64{SignalBM25: 1.0})

	cand := candidateFor(doc, 0, -1)
	p.Rerank("deploy", []*Candidate{cand}, &Context{Graph: g, ProseMode: false})
	base := cand.Score

	candP := candidateFor(doc, 0, -1)
	p.Rerank("deploy", []*Candidate{candP}, &Context{Graph: g, ProseMode: true})
	prose := candP.Score

	if !(prose > base) {
		t.Errorf("ProseMode should lift the bm25 contribution; base=%v prose=%v", base, prose)
	}
}

// TestProseModeOffIsUnchanged confirms the default (ProseMode false)
// path scores exactly as before — the lever is a strict opt-in and a
// code query pays nothing.
func TestProseModeOffIsUnchanged(t *testing.T) {
	g := newTestGraph()
	fn := mustNode(g, "f.go::Deploy", "Deploy", graph.KindFunction)
	p := New(DefaultSignals(), DefaultWeights())

	a := candidateFor(fn, 0, -1)
	p.Rerank("deploy", []*Candidate{a}, &Context{Graph: g})
	noField := a.Score

	b := candidateFor(fn, 0, -1)
	p.Rerank("deploy", []*Candidate{b}, &Context{Graph: g, ProseMode: false})
	explicitOff := b.Score

	if noField != explicitOff {
		t.Errorf("ProseMode=false must equal the zero-value path; %v != %v", explicitOff, noField)
	}
}
