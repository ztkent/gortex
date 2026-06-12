package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// critiqueFindings is a small fixture of three findings the critique pass
// adjudicates: a real error, a warning, and an info-level note.
func critiqueFindings() []Finding {
	return []Finding{
		{Rule: "nil-deref", Severity: SevError, Category: "nil-deref", File: "app/svc.go", Line: 12,
			Message: "p may be nil", SourceLine: "return p.Balance"},
		{Rule: "style-nit", Severity: SevWarning, Category: "style", File: "app/svc.go", Line: 20,
			Message: "prefer fmt.Errorf"},
		{Rule: "doc-todo", Severity: SevInfo, Category: "doc", File: "app/util.go", Line: 4,
			Message: "missing doc comment"},
	}
}

// TestCritiqueDropsOneFinding drives the happy path: a stubbed critique LLM marks
// the middle finding a false positive. The pass removes exactly that finding,
// records it in Dropped with its reason, keeps the other two, and recomputes the
// verdict over the kept set.
func TestCritiqueDropsOneFinding(t *testing.T) {
	findings := critiqueFindings()
	stub := &stubGen{reply: `[
		{"index":0,"verdict":"keep","reason":"genuine nil deref"},
		{"index":1,"verdict":"drop","reason":"style nit, not a defect"},
		{"index":2,"verdict":"keep","reason":"valid doc gap"}
	]`}

	res := Critique(context.Background(), stub.gen, findings, sampleDiff, 0)

	require.True(t, res.LLMUsed)
	require.Len(t, res.Kept, 2, "the dropped finding must be removed from the kept set")
	require.Len(t, res.Dropped, 1)
	require.Equal(t, "style-nit", res.Dropped[0].Finding.Rule)
	require.Equal(t, CritiqueDrop, res.Dropped[0].Verdict)
	require.Equal(t, "style nit, not a defect", res.Dropped[0].Reason)

	// The two surviving findings are the nil-deref and the doc-todo.
	keptRules := map[string]bool{}
	for _, f := range res.Kept {
		keptRules[f.Rule] = true
	}
	require.True(t, keptRules["nil-deref"])
	require.True(t, keptRules["doc-todo"])
	require.False(t, keptRules["style-nit"])

	// An error survived, so the verdict stays BLOCK.
	require.Equal(t, VerdictBlock, res.Verdict)
	require.Contains(t, res.Summary, "dropped 1")
}

// TestCritiqueVerdictDowngrade proves dropping the only error finding honestly
// downgrades the worst-of verdict over the kept set.
func TestCritiqueVerdictDowngrade(t *testing.T) {
	findings := critiqueFindings()
	stub := &stubGen{reply: `[
		{"index":0,"verdict":"drop","reason":"p is checked above the diff window"},
		{"index":1,"verdict":"keep","reason":""},
		{"index":2,"verdict":"keep","reason":""}
	]`}

	res := Critique(context.Background(), stub.gen, findings, "", 0)

	require.Len(t, res.Dropped, 1)
	require.Equal(t, "nil-deref", res.Dropped[0].Finding.Rule)
	// With the error dropped, the worst remaining is a warning → REVIEW.
	require.Equal(t, VerdictReview, res.Verdict)
}

// TestCritiqueDisabledIsNoOp proves a nil gen is a pass-through: every finding is
// kept, nothing dropped, no error, LLMUsed false.
func TestCritiqueDisabledIsNoOp(t *testing.T) {
	findings := critiqueFindings()

	res := Critique(context.Background(), nil, findings, sampleDiff, 0)

	require.False(t, res.LLMUsed)
	require.Len(t, res.Kept, len(findings))
	require.Empty(t, res.Dropped)
	require.Equal(t, VerdictBlock, res.Verdict) // unchanged worst-of
	require.Contains(t, res.Summary, "kept all")
}

// TestCritiqueGarbageKeepsAll proves an unparseable or non-JSON model response is
// treated as a no-op pass-through rather than an error or a finding wipe.
func TestCritiqueGarbageKeepsAll(t *testing.T) {
	findings := critiqueFindings()
	for _, reply := range []string{
		"I think these all look fine to me, no JSON here.",
		"```\nnot json\n```",
		"",
	} {
		stub := &stubGen{reply: reply}
		res := Critique(context.Background(), stub.gen, findings, sampleDiff, 0)
		require.False(t, res.LLMUsed, "reply %q should not count as an LLM-driven critique", reply)
		require.Len(t, res.Kept, len(findings), "garbage reply %q must keep every finding", reply)
		require.Empty(t, res.Dropped)
	}
}

// TestCritiqueModelErrorKeepsAll proves a failing model degrades to pass-through.
func TestCritiqueModelErrorKeepsAll(t *testing.T) {
	findings := critiqueFindings()
	stub := &stubGen{err: errors.New("provider unavailable")}

	res := Critique(context.Background(), stub.gen, findings, sampleDiff, 0)

	require.False(t, res.LLMUsed)
	require.Len(t, res.Kept, len(findings))
	require.Empty(t, res.Dropped)
}

// TestCritiqueUnknownVerdictKeeps proves an unrecognised verdict token, an
// unadjudicated finding (no row for its index), and an "uncertain" verdict all
// keep the finding — only an explicit "drop" removes one.
func TestCritiqueUnknownVerdictKeeps(t *testing.T) {
	findings := critiqueFindings()
	// index 0 → gibberish verdict (keep), index 1 → uncertain (keep + counted),
	// index 2 → no row at all (default keep).
	stub := &stubGen{reply: `[
		{"index":0,"verdict":"probably-fine","reason":"who knows"},
		{"index":1,"verdict":"uncertain","reason":"cannot tell from the diff"}
	]`}

	res := Critique(context.Background(), stub.gen, findings, sampleDiff, 0)

	require.True(t, res.LLMUsed)
	require.Len(t, res.Kept, 3, "no explicit drop → keep everything")
	require.Empty(t, res.Dropped)
	require.Equal(t, 1, res.Uncertain)
}

// TestCritiqueExtractsArrayFromProse proves the parser slices the JSON array out
// of a response wrapped in prose / code fences and ignores brackets inside reason
// strings.
func TestCritiqueExtractsArrayFromProse(t *testing.T) {
	findings := critiqueFindings()
	stub := &stubGen{reply: "Sure, here is my critique:\n```json\n[" +
		`{"index":1,"verdict":"drop","reason":"the [style] nit is cosmetic"}` +
		"]\n```\nHope that helps!"}

	res := Critique(context.Background(), stub.gen, findings, sampleDiff, 0)

	require.True(t, res.LLMUsed)
	require.Len(t, res.Dropped, 1)
	require.Equal(t, "style-nit", res.Dropped[0].Finding.Rule)
	require.Equal(t, "the [style] nit is cosmetic", res.Dropped[0].Reason)
}

// TestBuildCritiquePromptGrounding proves the prompt carries the diff grounding,
// each numbered finding, and its flagged source line.
func TestBuildCritiquePromptGrounding(t *testing.T) {
	findings := critiqueFindings()
	prompt := BuildCritiquePrompt(findings, sampleDiff)

	require.Contains(t, prompt, "SECOND PASS")
	require.Contains(t, prompt, "CHANGESET DIFF")
	require.Contains(t, prompt, "func Risky") // from sampleDiff
	require.Contains(t, prompt, "[0]")
	require.Contains(t, prompt, "rule=nil-deref")
	require.Contains(t, prompt, "flagged source: return p.Balance")
	require.Contains(t, prompt, `"verdict":"keep|drop|uncertain"`)
}

// TestCritiqueEmptyFindings proves a zero-finding input is a clean no-op.
func TestCritiqueEmptyFindings(t *testing.T) {
	stub := &stubGen{reply: `[]`}
	res := Critique(context.Background(), stub.gen, nil, sampleDiff, 0)
	require.False(t, res.LLMUsed)
	require.Empty(t, res.Kept)
	require.Empty(t, res.Dropped)
	require.Equal(t, VerdictApprove, res.Verdict)
	require.NotContains(t, strings.ToLower(res.Summary), "dropped")
}
