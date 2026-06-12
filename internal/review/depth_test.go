package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/llm"
)

// TestClassifyDepthThresholds is the boundary table for the depth classifier:
// at/under QuickMaxLines → quick; at/over DeepMinLines OR DeepMinFiles → deep;
// everything between → standard; zero-config fields fall back to the defaults.
func TestClassifyDepthThresholds(t *testing.T) {
	cfg := config.ReviewConfig{QuickMaxLines: 40, DeepMinLines: 400, DeepMinFiles: 20}

	cases := []struct {
		name  string
		lines int
		files int
		cfg   config.ReviewConfig
		want  Depth
	}{
		{"zero is quick", 0, 0, cfg, DepthQuick},
		{"just under quick ceiling", 39, 3, cfg, DepthQuick},
		{"exactly at quick ceiling is quick", 40, 3, cfg, DepthQuick},
		{"one over quick ceiling is standard", 41, 3, cfg, DepthStandard},
		{"mid-range is standard", 200, 5, cfg, DepthStandard},
		{"just under deep floor is standard", 399, 5, cfg, DepthStandard},
		{"exactly at deep line floor is deep", 400, 5, cfg, DepthDeep},
		{"over deep line floor is deep", 401, 1, cfg, DepthDeep},
		{"at deep file floor is deep even with few lines", 5, 20, cfg, DepthDeep},
		{"over deep file floor is deep even with zero lines", 0, 25, cfg, DepthDeep},
		// deep wins over quick when files trigger but lines are tiny.
		{"file-deep beats line-quick", 10, 20, cfg, DepthDeep},

		// Zero-config: defaults are QuickMaxLines 40, DeepMinLines 400,
		// DeepMinFiles 20 — same ladder as the explicit cfg above.
		{"default quick", 10, 1, config.ReviewConfig{}, DepthQuick},
		{"default quick at ceiling", 40, 1, config.ReviewConfig{}, DepthQuick},
		{"default standard", 100, 2, config.ReviewConfig{}, DepthStandard},
		{"default deep by lines", 400, 1, config.ReviewConfig{}, DepthDeep},
		{"default deep by files", 1, 20, config.ReviewConfig{}, DepthDeep},

		// Partial config: a custom QuickMaxLines with the rest defaulted.
		{"custom quick ceiling only", 60, 1, config.ReviewConfig{QuickMaxLines: 80}, DepthQuick},
		{"custom quick ceiling exceeded", 90, 1, config.ReviewConfig{QuickMaxLines: 80}, DepthStandard},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyDepth(tc.lines, tc.files, tc.cfg)
			require.Equal(t, tc.want, got, "lines=%d files=%d", tc.lines, tc.files)
		})
	}
}

// TestDepthString covers the label round-trip recorded on the report.
func TestDepthString(t *testing.T) {
	require.Equal(t, "quick", DepthQuick.String())
	require.Equal(t, "standard", DepthStandard.String())
	require.Equal(t, "deep", DepthDeep.String())
}

// TestDepthToComplexity proves quick/standard route to the simple model tier and
// deep routes to the complex tier.
func TestDepthToComplexity(t *testing.T) {
	require.Equal(t, llm.ComplexitySimple, DepthToComplexity(DepthQuick))
	require.Equal(t, llm.ComplexitySimple, DepthToComplexity(DepthStandard))
	require.Equal(t, llm.ComplexityComplex, DepthToComplexity(DepthDeep))
}

// TestChangedLinesFromDiff sums the per-hunk new-side spans, clamping a
// degenerate hunk to one line and tolerating a nil diff.
func TestChangedLinesFromDiff(t *testing.T) {
	require.Equal(t, 0, ChangedLinesFromDiff(nil))

	diff := &analysis.DiffResult{Hunks: []analysis.DiffHunk{
		{FilePath: "a.go", StartLine: 1, EndLine: 10}, // 10 lines
		{FilePath: "b.go", StartLine: 5, EndLine: 5},  // 1 line
		{FilePath: "c.go", StartLine: 9, EndLine: 3},  // degenerate → 1 line
	}}
	require.Equal(t, 12, ChangedLinesFromDiff(diff))
}

// TestChangedFilesFromDiff unions the changed-file list with the files the hunks
// and changed symbols land in, deduping.
func TestChangedFilesFromDiff(t *testing.T) {
	require.Equal(t, 0, ChangedFilesFromDiff(nil))

	diff := &analysis.DiffResult{
		ChangedFiles: []string{"a.go", "b.go"},
		Hunks:        []analysis.DiffHunk{{FilePath: "b.go", StartLine: 1, EndLine: 1}, {FilePath: "c.go", StartLine: 1, EndLine: 1}},
		ChangedSymbols: []analysis.ChangedSymbol{
			{FilePath: "c.go"}, {FilePath: "d.go"},
		},
	}
	require.Equal(t, 4, ChangedFilesFromDiff(diff)) // a, b, c, d
}

// TestPlannerCatalogue proves the catalogue is the six reference tools and the
// spec projection mirrors them.
func TestPlannerCatalogue(t *testing.T) {
	cat := PlannerCatalogue()
	require.Len(t, cat, 6)

	names := make([]string, 0, len(cat))
	for _, e := range cat {
		names = append(names, e.Name)
		require.NotEmpty(t, e.Description, "every catalogue entry carries a description")
	}
	require.Equal(t, []string{
		"detect_changes", "diff_context", "explain_change_impact",
		"verify_change", "contracts", "check_guards",
	}, names)

	specs := PlannerCatalogueSpecs()
	require.Len(t, specs, 6)
	require.Equal(t, "detect_changes", specs[0].Name)
	require.IsType(t, llm.ToolSpec{}, specs[0])

	// The returned slice is a copy — mutating it does not corrupt the source.
	cat[0].Name = "mutated"
	require.Equal(t, "detect_changes", PlannerCatalogue()[0].Name)
}

// TestDeepPromptCarriesReferenceCatalogue asserts the deep prompt builder injects
// the planner catalogue, labelled reference-only, and the non-deep prompt does
// not.
func TestDeepPromptCarriesReferenceCatalogue(t *testing.T) {
	pack := &ReviewPack{
		Changed: []PackEntry{{ID: "app/svc.go::Risky", File: "app/svc.go", Line: 3, Tier: TierChanged,
			Diff: "+func Risky() {}\n"}},
	}

	deep := buildReviewPrompt(promptInput{Pack: pack, Deep: true})
	require.Contains(t, deep, "reference only", "the catalogue must be labelled reference-only")
	require.Contains(t, deep, "not callable", "the catalogue must say the tools are not callable here")
	for _, name := range []string{"detect_changes", "diff_context", "explain_change_impact", "verify_change", "contracts", "check_guards"} {
		require.Contains(t, deep, name, "the deep prompt references %s", name)
	}

	shallow := buildReviewPrompt(promptInput{Pack: pack, Deep: false})
	require.NotContains(t, shallow, "reference only", "a non-deep prompt carries no catalogue")
	require.NotContains(t, shallow, "explain_change_impact")
}

// bigDiff builds a unified diff that adds n contiguous lines to one file so the
// flow's depth classifier sizes the change deterministically.
func bigDiff(n int) string {
	var b strings.Builder
	b.WriteString("diff --git a/app/big.go b/app/big.go\n")
	b.WriteString("index 1111111..2222222 100644\n")
	b.WriteString("--- a/app/big.go\n")
	b.WriteString("+++ b/app/big.go\n")
	fmt.Fprintf(&b, "@@ -1,1 +1,%d @@\n", n+1)
	b.WriteString(" package app\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "+var X%d = %d\n", i, i)
	}
	return b.String()
}

// TestFlowQuickDepthSkipsLLM proves a quick changeset (under QuickMaxLines, with
// adaptive thresholds configured) skips the LLM MAIN phase entirely — the
// stubbed seam is never hit — and still returns the deterministic rule findings.
func TestFlowQuickDepthSkipsLLM(t *testing.T) {
	gen := &stubGen{reply: `[{"file":"app/big.go","snippet":"var X0 = 0","message":"x","severity":"error","category":"y"}]`}

	matches := []astquery.Match{
		{Detector: "go-self-comparison", File: "app/big.go", Line: 2, EndLine: 2,
			Severity: "warning", Text: "self comparison", SymbolID: "app/big.go::X0"},
	}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot:        "/tmp/repo",
		Diff:            bigDiff(5), // 5 added lines → quick under the 40-line ceiling
		RulepackMatches: matches,
		Rules:           testResolver(t),
		UseLLM:          true,
		Config:          config.ReviewConfig{QuickMaxLines: 40, DeepMinLines: 400, DeepMinFiles: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Equal(t, "quick", report.Depth)
	require.Equal(t, "quick", report.Stats.Depth)
	require.Empty(t, gen.lastPrompt, "the LLM seam must NOT be called at quick depth")
	require.Equal(t, 0, report.Stats.LLM, "no LLM findings at quick depth")
	require.Len(t, report.Findings, 1, "the deterministic rule finding still surfaces")
	require.Equal(t, "rulepack", report.Findings[0].Source)
	require.Equal(t, VerdictReview, report.Verdict, "a warning rule finding → REVIEW")
}

// TestFlowDeepDepthRunsFullPipeline proves a deep changeset (over DeepMinLines)
// runs the LLM MAIN phase, grounds the prompt in the reference-only planner
// catalogue, and relocates the LLM finding into the report.
func TestFlowDeepDepthRunsFullPipeline(t *testing.T) {
	gen := &stubGen{reply: `[{"file":"app/big.go","snippet":"var X7 = 7","message":"unused global","severity":"warning","category":"style"}]`}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot: "/tmp/repo",
		Diff:     bigDiff(500), // 500 added lines → deep over the 400-line floor
		Rules:    testResolver(t),
		UseLLM:   true,
		Config:   config.ReviewConfig{QuickMaxLines: 40, DeepMinLines: 400, DeepMinFiles: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Equal(t, "deep", report.Depth)
	require.NotEmpty(t, gen.lastPrompt, "the LLM seam must be called at deep depth")
	require.Contains(t, gen.lastPrompt, "reference only", "the deep prompt carries the reference-only catalogue")
	require.Contains(t, gen.lastPrompt, "explain_change_impact", "the deep prompt references a graph tool")

	var llmFinding *Finding
	for i := range report.Findings {
		if report.Findings[i].Source == "llm" {
			llmFinding = &report.Findings[i]
			break
		}
	}
	require.NotNil(t, llmFinding, "the relocated LLM finding is in the report")
	require.Greater(t, llmFinding.Line, 0, "the LLM finding is line-anchored")
	require.Equal(t, 1, report.Stats.LLM)
}

// TestFlowStandardDepthRunsLLMWithoutCatalogue proves a mid-sized change runs a
// single LLM pass but the prompt does NOT carry the deep planner catalogue.
func TestFlowStandardDepthRunsLLMWithoutCatalogue(t *testing.T) {
	gen := &stubGen{reply: `[{"file":"app/big.go","snippet":"var X3 = 3","message":"m","severity":"info","category":"c"}]`}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot: "/tmp/repo",
		Diff:     bigDiff(100), // 100 added lines → standard
		Rules:    testResolver(t),
		UseLLM:   true,
		Config:   config.ReviewConfig{QuickMaxLines: 40, DeepMinLines: 400, DeepMinFiles: 20},
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Equal(t, "standard", report.Depth)
	require.NotEmpty(t, gen.lastPrompt, "the LLM seam runs at standard depth")
	require.NotContains(t, gen.lastPrompt, "reference only", "standard depth does NOT inject the catalogue")
	require.Equal(t, 1, report.Stats.LLM)
}

// TestFlowNoThresholdsPreservesLegacyBehavior proves that with no depth
// thresholds configured the flow keeps its pre-adaptive behaviour: the LLM runs
// even on a tiny diff (which would otherwise classify quick).
func TestFlowNoThresholdsPreservesLegacyBehavior(t *testing.T) {
	gen := &stubGen{reply: `[{"file":"app/svc.go","snippet":"return p.Balance","message":"nil deref","severity":"critical","category":"correctness"}]`}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot: "/tmp/repo",
		Diff:     sampleDiff, // 3 added lines — quick under defaults, but no thresholds set
		Rules:    testResolver(t),
		UseLLM:   true,
		// Config zero-value: no depth thresholds → adaptive gating inert.
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	require.NotEmpty(t, gen.lastPrompt, "without configured thresholds the LLM still runs (legacy behaviour)")
	require.Equal(t, 1, report.Stats.LLM, "the LLM finding survives — gating is inert")
	require.Equal(t, "quick", report.Depth, "depth is still classified and recorded, just not gated")
}
