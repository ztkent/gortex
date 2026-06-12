package review

import (
	"strings"
	"testing"
)

// fixedReport is a representative report: a BLOCK verdict with three findings
// across two files (one critical, one error, one warning), a per-file risk
// ranking, a summary line, an adaptive depth, and a populated cost block.
func fixedReport() *ReviewReport {
	return &ReviewReport{
		Verdict: VerdictBlock,
		Summary: "BLOCK: 3 finding(s) across 2 changed file(s)",
		Depth:   "deep",
		Findings: []Finding{
			{
				File: "internal/svc/loop.go", Line: 12, Severity: SevWarning,
				Rule: "go-loop-query-call", Category: "performance",
				Message: "query in loop", Confidence: 0.8,
			},
			{
				File: "internal/svc/handler.go", Line: 7, Severity: SevError,
				Rule: "go-inverted-err-check", Category: "correctness",
				Message: "inverted error check", Confidence: 0.9,
			},
			{
				File: "internal/svc/handler.go", Line: 3, Severity: SevCritical,
				Rule: "nil-deref", Category: "correctness",
				Message: "possible nil dereference", Confidence: 0.95,
			},
		},
		FileRisk: []FileRisk{
			{File: "internal/svc/handler.go", Risk: "high", Findings: 2},
			{File: "internal/svc/loop.go", Risk: "medium", Findings: 1},
		},
		Cost: &CostBreakdown{
			InputTokens: 1234, OutputTokens: 456,
			CacheReadTokens: 2000, CacheWriteTokens: 0,
			USD: 0.012, Estimated: true, ElapsedMs: 4200,
		},
	}
}

// TestRenderSummary_AgentGolden pins the terse agent rendering byte-for-byte.
// Findings are ordered worst-first (critical → error → warning); the verdict
// line carries the severity histogram; one compact line per finding; one cost
// line. There is no narrative prose.
func TestRenderSummary_AgentGolden(t *testing.T) {
	got := RenderSummary(fixedReport(), AudienceAgent)

	want := "" +
		"VERDICT: block (1 critical, 1 error, 1 warning)\n" +
		"findings:\n" +
		"  internal/svc/handler.go:3 critical nil-deref — possible nil dereference\n" +
		"  internal/svc/handler.go:7 error go-inverted-err-check — inverted error check\n" +
		"  internal/svc/loop.go:12 warning go-loop-query-call — query in loop\n" +
		"cost: in=1234 out=456 cache_r=2000 cache_w=0 usd=0.012000 elapsed=4.2s\n"

	if got != want {
		t.Fatalf("agent rendering drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderSummary_AgentTerse_NoNarrative asserts the agent rendering carries
// only the verdict line, the findings block, and the cost line — no markdown
// headers, no per-file risk section, no prose.
func TestRenderSummary_AgentTerse_NoNarrative(t *testing.T) {
	got := RenderSummary(fixedReport(), AudienceAgent)

	// One-line verdict first.
	first := strings.SplitN(got, "\n", 2)[0]
	if !strings.HasPrefix(first, "VERDICT: block") {
		t.Fatalf("agent output must lead with a one-line VERDICT, got %q", first)
	}
	// Cost line present.
	if !strings.Contains(got, "cost: in=1234 out=456 cache_r=2000 cache_w=0 usd=0.012000 elapsed=4.2s") {
		t.Errorf("agent output must carry the cost line, got:\n%s", got)
	}
	// Prose / narrative markers from the human packet must NOT appear.
	for _, marker := range []string{"## ", "Verdict:", "File risk:", "Findings (", "No inline findings", "finding(s) across"} {
		if strings.Contains(got, marker) {
			t.Errorf("agent output must not contain narrative marker %q, got:\n%s", marker, got)
		}
	}
	// Every finding line is a single compact line under the findings block.
	if c := strings.Count(got, " — "); c != 3 {
		t.Errorf("expected 3 compact finding lines, found %d em-dashes in:\n%s", c, got)
	}
}

// TestRenderSummary_HumanGolden pins the readable section packet byte-for-byte:
// the verdict header, the summary line, the per-file risk ranking, and the
// findings grouped by file and ordered by line, then the cost block.
func TestRenderSummary_HumanGolden(t *testing.T) {
	got := RenderSummary(fixedReport(), AudienceHuman)

	want := "" +
		"Verdict: BLOCK\n" +
		"BLOCK: 3 finding(s) across 2 changed file(s)\n" +
		"\n" +
		"File risk:\n" +
		"  high     internal/svc/handler.go (2 finding(s))\n" +
		"  medium   internal/svc/loop.go (1 finding(s))\n" +
		"\n" +
		"Findings (3):\n" +
		"\n" +
		"internal/svc/handler.go\n" +
		"  L3     critical possible nil dereference  [correctness/nil-deref]\n" +
		"  L7     error    inverted error check  [correctness/go-inverted-err-check]\n" +
		"\n" +
		"internal/svc/loop.go\n" +
		"  L12    warning  query in loop  [performance/go-loop-query-call]\n" +
		"\n" +
		"cost: in=1234 out=456 cache_r=2000 cache_w=0 usd=0.012000 elapsed=4.2s\n"

	if got != want {
		t.Fatalf("human rendering drift:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderSummary_HumanIsSectionPacket asserts the human rendering carries the
// readable sections the agent form omits.
func TestRenderSummary_HumanIsSectionPacket(t *testing.T) {
	got := RenderSummary(fixedReport(), AudienceHuman)
	for _, want := range []string{"Verdict: BLOCK", "File risk:", "Findings (3):", "internal/svc/handler.go", "[correctness/nil-deref]"} {
		if !strings.Contains(got, want) {
			t.Errorf("human packet missing section marker %q, got:\n%s", want, got)
		}
	}
	// The terse one-line verdict form must NOT be present in the human packet.
	if strings.Contains(got, "VERDICT: block") {
		t.Errorf("human packet must use the readable header, not the terse VERDICT line:\n%s", got)
	}
}

// TestRenderSummary_DefaultIsHuman asserts AudienceHuman is the zero value, so a
// caller that does not opt in gets the readable packet.
func TestRenderSummary_DefaultIsHuman(t *testing.T) {
	var aud Audience // zero value
	if aud != AudienceHuman {
		t.Fatalf("the zero-value Audience must be AudienceHuman, got %d", aud)
	}
	if RenderSummary(fixedReport(), aud) != RenderSummary(fixedReport(), AudienceHuman) {
		t.Error("zero-value Audience must render identically to AudienceHuman")
	}
}

// TestRenderSummary_NilReport asserts a nil report renders an empty APPROVE for
// both audiences without panicking.
func TestRenderSummary_NilReport(t *testing.T) {
	agent := RenderSummary(nil, AudienceAgent)
	if !strings.HasPrefix(agent, "VERDICT: approve") {
		t.Errorf("nil report agent rendering must lead with approve, got %q", agent)
	}
	if !strings.Contains(agent, "cost: in=0 out=0") {
		t.Errorf("nil report agent rendering must carry a zero cost line, got %q", agent)
	}
	human := RenderSummary(nil, AudienceHuman)
	if !strings.Contains(human, "Verdict: APPROVE") || !strings.Contains(human, "No inline findings") {
		t.Errorf("nil report human rendering must be an empty APPROVE packet, got %q", human)
	}
}

// TestRenderSummary_NoCostStillEmitsZeroLine asserts a report with a nil Cost
// (the deterministic-only path) still renders a parseable zero cost line in both
// audiences.
func TestRenderSummary_NoCostStillEmitsZeroLine(t *testing.T) {
	r := fixedReport()
	r.Cost = nil
	for _, aud := range []Audience{AudienceAgent, AudienceHuman} {
		got := RenderSummary(r, aud)
		if !strings.Contains(got, "cost: in=0 out=0 cache_r=0 cache_w=0 usd=0.000000 elapsed=0.0s") {
			t.Errorf("audience %d must emit a zero cost line when Cost is nil, got:\n%s", aud, got)
		}
	}
}
