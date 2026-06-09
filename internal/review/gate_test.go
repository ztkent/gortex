package review

import (
	"testing"

	"github.com/zzet/gortex/internal/config"
)

func mkFinding(rule string, sev Severity, cat string, conf float64) Finding {
	return Finding{
		Rule:       rule,
		Severity:   sev,
		Category:   cat,
		Confidence: conf,
		File:       "pkg/x.go",
		SymbolID:   "pkg/x.go::" + rule,
		Message:    rule,
	}
}

// TestGatePassThrough proves a zero-value config keeps every finding and drops
// nothing — the legacy no-op behaviour callers rely on.
func TestGatePassThrough(t *testing.T) {
	in := []Finding{
		mkFinding("a", SevInfo, "style", 0.1),
		mkFinding("b", SevCritical, "security", 0.0),
		mkFinding("c", SevWarning, "perf", 0.5),
	}
	kept, stats := NewGate(config.ReviewConfig{}).Apply(in)

	if len(kept) != len(in) {
		t.Fatalf("pass-through dropped findings: kept %d of %d", len(kept), len(in))
	}
	if stats.Suppressed() != 0 {
		t.Fatalf("pass-through suppressed %d findings, want 0: %+v", stats.Suppressed(), stats)
	}
	if stats.Input != 3 || stats.Kept != 3 {
		t.Fatalf("pass-through stats wrong: %+v", stats)
	}
}

// TestGateBelowConfidenceDropped drops findings under MinConfidence and counts
// them under BelowConfidence only.
func TestGateBelowConfidenceDropped(t *testing.T) {
	in := []Finding{
		mkFinding("low", SevError, "security", 0.3),
		mkFinding("hi", SevError, "security", 0.9),
		mkFinding("edge", SevError, "security", 0.7), // == floor, kept
	}
	kept, stats := NewGate(config.ReviewConfig{MinConfidence: 0.7}).Apply(in)

	if len(kept) != 2 {
		t.Fatalf("want 2 kept, got %d: %+v", len(kept), kept)
	}
	if stats.BelowConfidence != 1 {
		t.Fatalf("want BelowConfidence 1, got %d: %+v", stats.BelowConfidence, stats)
	}
	if stats.BelowSeverity != 0 || stats.CategoryFiltered != 0 || stats.OverCap != 0 {
		t.Fatalf("unexpected other drops: %+v", stats)
	}
	for _, f := range kept {
		if f.Rule == "low" {
			t.Fatalf("low-confidence finding survived: %+v", kept)
		}
	}
}

// TestGateBelowSeverityDropped drops findings below the MinSeverity rank.
func TestGateBelowSeverityDropped(t *testing.T) {
	in := []Finding{
		mkFinding("info", SevInfo, "style", 1.0),
		mkFinding("warn", SevWarning, "style", 1.0),
		mkFinding("err", SevError, "style", 1.0),
		mkFinding("crit", SevCritical, "style", 1.0),
	}
	kept, stats := NewGate(config.ReviewConfig{MinSeverity: "error"}).Apply(in)

	if len(kept) != 2 {
		t.Fatalf("want 2 kept (err+crit), got %d: %+v", len(kept), kept)
	}
	if stats.BelowSeverity != 2 {
		t.Fatalf("want BelowSeverity 2, got %d: %+v", stats.BelowSeverity, stats)
	}
	for _, f := range kept {
		if severityRank(f.Severity) < severityRank(SevError) {
			t.Fatalf("kept a sub-floor finding: %+v", f)
		}
	}
}

// TestGateOutOfCategoryDropped keeps only allow-listed categories and is
// case / whitespace insensitive.
func TestGateOutOfCategoryDropped(t *testing.T) {
	in := []Finding{
		mkFinding("a", SevError, "Security", 1.0), // case-insensitive match
		mkFinding("b", SevError, "style", 1.0),    // not allow-listed
		mkFinding("c", SevError, " perf ", 1.0),   // whitespace-tolerant match
	}
	cfg := config.ReviewConfig{Categories: []string{"security", "perf"}}
	kept, stats := NewGate(cfg).Apply(in)

	if len(kept) != 2 {
		t.Fatalf("want 2 kept, got %d: %+v", len(kept), kept)
	}
	if stats.CategoryFiltered != 1 {
		t.Fatalf("want CategoryFiltered 1, got %d: %+v", stats.CategoryFiltered, stats)
	}
	for _, f := range kept {
		if normalizeCategory(f.Category) == "style" {
			t.Fatalf("out-of-category finding survived: %+v", kept)
		}
	}
}

// TestGateOverCapTrimsWorst caps at MaxFindings, keeping the highest-severity
// then highest-confidence findings and recording the trimmed count.
func TestGateOverCapTrimsWorst(t *testing.T) {
	in := []Finding{
		mkFinding("info", SevInfo, "c", 0.9),
		mkFinding("crit", SevCritical, "c", 0.5),
		mkFinding("warn", SevWarning, "c", 0.9),
		mkFinding("err-lo", SevError, "c", 0.4),
		mkFinding("err-hi", SevError, "c", 0.8),
	}
	kept, stats := NewGate(config.ReviewConfig{MaxFindings: 3}).Apply(in)

	if len(kept) != 3 {
		t.Fatalf("want 3 kept, got %d: %+v", len(kept), kept)
	}
	if stats.OverCap != 2 {
		t.Fatalf("want OverCap 2, got %d: %+v", stats.OverCap, stats)
	}
	// Worst-first order: critical, then the two errors by confidence desc.
	wantOrder := []string{"crit", "err-hi", "err-lo"}
	for i, want := range wantOrder {
		if kept[i].Rule != want {
			t.Fatalf("cap kept wrong/ordered set: got %s at %d, want %s (kept=%+v)", kept[i].Rule, i, want, kept)
		}
	}
}

// TestGateStatsCountEachReason exercises every drop reason at once and checks
// the suppression summary attributes each to the right counter.
func TestGateStatsCountEachReason(t *testing.T) {
	in := []Finding{
		mkFinding("lowconf", SevError, "security", 0.2),  // below confidence
		mkFinding("lowsev", SevInfo, "security", 1.0),    // below severity
		mkFinding("badcat", SevError, "style", 1.0),      // out of category
		mkFinding("keep1", SevCritical, "security", 1.0), // kept
		mkFinding("keep2", SevError, "security", 0.9),    // kept then capped
		mkFinding("keep3", SevError, "security", 0.8),    // capped (over cap)
	}
	cfg := config.ReviewConfig{
		MinConfidence: 0.5,
		MinSeverity:   "warning",
		Categories:    []string{"security"},
		MaxFindings:   2,
	}
	kept, stats := NewGate(cfg).Apply(in)

	if stats.Input != 6 {
		t.Fatalf("want Input 6, got %d", stats.Input)
	}
	if stats.BelowConfidence != 1 {
		t.Errorf("want BelowConfidence 1, got %d", stats.BelowConfidence)
	}
	if stats.BelowSeverity != 1 {
		t.Errorf("want BelowSeverity 1, got %d", stats.BelowSeverity)
	}
	if stats.CategoryFiltered != 1 {
		t.Errorf("want CategoryFiltered 1, got %d", stats.CategoryFiltered)
	}
	if stats.OverCap != 1 {
		t.Errorf("want OverCap 1, got %d", stats.OverCap)
	}
	if len(kept) != 2 || stats.Kept != 2 {
		t.Fatalf("want 2 kept, got %d (stats.Kept=%d): %+v", len(kept), stats.Kept, kept)
	}
	if stats.Suppressed() != 4 {
		t.Fatalf("want Suppressed 4, got %d: %+v", stats.Suppressed(), stats)
	}
	// The two survivors are the worst-ranked of the allow-listed, in-confidence,
	// at-or-above-severity findings.
	if kept[0].Rule != "keep1" || kept[1].Rule != "keep2" {
		t.Fatalf("cap kept wrong survivors: %+v", kept)
	}
}

// TestGateInputNotMutated proves Apply does not reorder or alter the caller's
// slice.
func TestGateInputNotMutated(t *testing.T) {
	in := []Finding{
		mkFinding("a", SevInfo, "c", 0.9),
		mkFinding("b", SevCritical, "c", 0.9),
	}
	_, _ = NewGate(config.ReviewConfig{MaxFindings: 1}).Apply(in)
	if in[0].Rule != "a" || in[1].Rule != "b" {
		t.Fatalf("Apply mutated the input slice: %+v", in)
	}
}

// TestRunSurfacesGate proves the review flow applies the gate from Options and
// surfaces both the gated finding set and the suppression summary on the
// report, and that an unset Config leaves the legacy behaviour intact.
func TestRunSurfacesGate(t *testing.T) {
	// A pasted diff + caller-supplied rulepack matches gives a graph-free,
	// LLM-free deterministic run we can gate.
	low := mkFinding("low", SevInfo, "style", 1.0)
	high := mkFinding("high", SevCritical, "security", 1.0)

	report := &ReviewReport{}
	plan := &reviewPlan{ruleFinds: []Finding{low, high}}

	// Gated: MinSeverity warning drops the info finding.
	opts := Options{Config: config.ReviewConfig{MinSeverity: "warning"}}
	report = compress(opts, plan, nil, 0, false)
	if len(report.Findings) != 1 || report.Findings[0].Rule != "high" {
		t.Fatalf("gate not applied in flow: %+v", report.Findings)
	}
	if report.Stats.Gate.BelowSeverity != 1 {
		t.Fatalf("gate stats not surfaced: %+v", report.Stats.Gate)
	}
	if report.Stats.Total != 1 {
		t.Fatalf("report total should reflect gated set: %d", report.Stats.Total)
	}

	// Unset Config: pass-through, both findings survive.
	report = compress(Options{}, plan, nil, 0, false)
	if len(report.Findings) != 2 {
		t.Fatalf("zero-value Config dropped findings: %+v", report.Findings)
	}
	if report.Stats.Gate.Suppressed() != 0 {
		t.Fatalf("zero-value Config suppressed findings: %+v", report.Stats.Gate)
	}
}
