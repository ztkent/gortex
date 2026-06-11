package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
)

// sampleDiff is a tiny unified diff with two added lines the relocation phase can
// anchor verbatim snippets against.
const sampleDiff = `diff --git a/app/svc.go b/app/svc.go
index 1111111..2222222 100644
--- a/app/svc.go
+++ b/app/svc.go
@@ -1,3 +1,6 @@
 package app

+func Risky(p *Account) int {
+	return p.Balance // nil-deref: p may be nil
+}
`

// stubGen returns a canned reply regardless of the prompt. It records the last
// prompt it was given so the test can assert grounding without a live model.
type stubGen struct {
	reply      string
	err        error
	lastPrompt string
}

func (s *stubGen) gen(_ context.Context, prompt string, _ int) (string, error) {
	s.lastPrompt = prompt
	if s.err != nil {
		return "", s.err
	}
	return s.reply, nil
}

func testResolver(t *testing.T) *RuleResolver {
	t.Helper()
	// repoRoot empty + customPath empty → only the global + embedded layers; the
	// embedded `**` catch-all guarantees RuleFor always resolves.
	r, err := NewRuleResolver("", "")
	require.NoError(t, err)
	return r
}

// TestRunRelocatesAndBlocks proves the happy path: a stubbed LLM returns a
// critical finding whose snippet matches an added line; the relocation phase
// anchors it to that exact new-side line, the deterministic rulepack finding is
// merged in, and the worst-of verdict over the merged set is BLOCK.
func TestRunRelocatesAndBlocks(t *testing.T) {
	gen := &stubGen{reply: `[
	  {"file":"app/svc.go","snippet":"return p.Balance","message":"possible nil dereference of p","severity":"critical","category":"correctness"}
	]`}

	// A deterministic rulepack match the caller hands in (already line-exact).
	matches := []astquery.Match{
		{Detector: "go-loop-query-call", File: "app/svc.go", Line: 4, EndLine: 4,
			Severity: "warning", Text: "query inside loop", SymbolID: "app/svc.go::Risky"},
	}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot:        "/tmp/repo",
		Diff:            sampleDiff,
		RulepackMatches: matches,
		Rules:           testResolver(t),
		UseLLM:          true,
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	// The LLM finding relocated to an exact line (RELOCATE worked).
	var llm *Finding
	for i := range report.Findings {
		if report.Findings[i].Source == "llm" {
			llm = &report.Findings[i]
			break
		}
	}
	require.NotNil(t, llm, "the LLM finding must survive relocation")
	require.Greater(t, llm.Line, 0, "an LLM finding must be line-anchored")
	require.Equal(t, llm.Line, llm.StartLine)
	require.NotEqual(t, AnchorUnresolved, llm.Anchor, "the finding must carry a resolved anchor tier")
	require.Equal(t, "app/svc.go", llm.File)

	// The deterministic rulepack finding is merged in.
	hasRulepack := false
	for _, f := range report.Findings {
		if f.Source == "rulepack" {
			hasRulepack = true
		}
	}
	require.True(t, hasRulepack, "the deterministic rule finding must be merged into the report")

	// Worst-of verdict: the critical finding blocks.
	require.Equal(t, VerdictBlock, report.Verdict)
	require.Equal(t, 1, report.Stats.Rulepack)
	require.Equal(t, 1, report.Stats.LLM)
	require.Equal(t, 0, report.Stats.Dropped)
}

// TestRunDisabledLLMKeepsDeterministic proves Run is total when the LLM is off:
// it still returns the deterministic rule findings plus a verdict, no error, no
// LLM call.
func TestRunDisabledLLMKeepsDeterministic(t *testing.T) {
	matches := []astquery.Match{
		{Detector: "go-inverted-err-check", File: "app/svc.go", Line: 3, EndLine: 3,
			Severity: "error", Text: "inverted error check", SymbolID: "app/svc.go::Risky"},
	}

	report, err := Run(context.Background(), nil, nil /* gen */, Options{
		RepoRoot:        "/tmp/repo",
		Diff:            sampleDiff,
		RulepackMatches: matches,
		Rules:           testResolver(t),
		UseLLM:          false,
	})
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Findings, 1, "only the deterministic rule finding")
	require.Equal(t, "rulepack", report.Findings[0].Source)
	require.Equal(t, VerdictBlock, report.Verdict, "an error-severity rule finding blocks")
	require.Equal(t, 0, report.Stats.LLM)
	require.False(t, report.Stats.LLMRequested)
}

// TestRunGarbageLLMDegrades proves a failing / garbage LLM never errors and never
// drops the deterministic findings: a gen that errors and a gen that returns
// non-JSON both yield a report with the rule findings + a verdict.
func TestRunGarbageLLMDegrades(t *testing.T) {
	matches := []astquery.Match{
		{Detector: "go-self-comparison", File: "app/svc.go", Line: 3,
			Severity: "warning", Text: "self comparison", SymbolID: "app/svc.go::Risky"},
	}

	cases := []*stubGen{
		{err: errors.New("model offline")},
		{reply: "I think the code looks fine, no JSON here."},
		{reply: "[]"}, // valid empty array
	}

	for _, gen := range cases {
		report, err := Run(context.Background(), nil, gen.gen, Options{
			RepoRoot:        "/tmp/repo",
			Diff:            sampleDiff,
			RulepackMatches: matches,
			Rules:           testResolver(t),
			UseLLM:          true,
		})
		require.NoError(t, err)
		require.NotNil(t, report)
		require.Len(t, report.Findings, 1, "the deterministic finding survives a bad LLM")
		require.Equal(t, "rulepack", report.Findings[0].Source)
		require.Equal(t, VerdictReview, report.Verdict, "a warning-severity finding → REVIEW")
		require.Equal(t, 0, report.Stats.LLM)
	}
}

// TestRunUnresolvedLLMDropped proves an LLM finding whose snippet matches nothing
// in the change is dropped (not anchored to a bogus line) and counted.
func TestRunUnresolvedLLMDropped(t *testing.T) {
	gen := &stubGen{reply: `[
	  {"file":"app/svc.go","snippet":"this text appears nowhere in the diff","message":"ghost","severity":"error","category":"x"}
	]`}

	report, err := Run(context.Background(), nil, gen.gen, Options{
		RepoRoot:        "/tmp/repo",
		Diff:            sampleDiff,
		RulepackMatches: nil,
		Rules:           testResolver(t),
		UseLLM:          true,
	})
	require.NoError(t, err)
	require.Empty(t, report.Findings, "an un-anchorable LLM finding is dropped")
	require.Equal(t, 1, report.Stats.Dropped)
	require.Equal(t, VerdictApprove, report.Verdict, "no findings → APPROVE")
}

// TestBuildReviewPromptPure asserts the prompt builder is a pure function whose
// output carries the resolved per-file rule and the pack context — the grounding
// the MAIN phase relies on.
func TestBuildReviewPromptPure(t *testing.T) {
	rules := map[string]config.ReviewRule{
		"app/svc.go": {Name: "auth-strict", Path: "app/**", Severity: "error", Rulepack: "security"},
	}
	pack := &ReviewPack{
		Changed: []PackEntry{{ID: "app/svc.go::Risky", File: "app/svc.go", Line: 3, Tier: TierChanged,
			Diff: "+func Risky(p *Account) int {\n"}},
		Budget: 0,
	}
	det := []Finding{{Rule: "test", Severity: SevWarning, File: "app/svc.go", Line: 4, Message: "existing rulepack hit"}}

	in := promptInput{Rules: rules, Pack: pack, Deterministic: det}
	p1 := buildReviewPrompt(in)
	p2 := buildReviewPrompt(in)
	require.Equal(t, p1, p2, "buildReviewPrompt must be deterministic (pure)")

	require.Contains(t, p1, "auth-strict", "the resolved rule name must be in the prompt")
	require.Contains(t, p1, "security", "the resolved rulepack must be in the prompt")
	require.Contains(t, p1, "app/svc.go", "the changed file must be grounded in the prompt")
	require.Contains(t, p1, "func Risky", "the pack diff context must be in the prompt")
	require.Contains(t, p1, "existing rulepack hit", "established deterministic findings must be in the prompt")
	require.True(t, strings.Contains(p1, "JSON"), "the prompt must instruct a JSON reply")
}

// TestCandidateSeverityFloor proves a rule's severity floor raises a weaker model
// verdict when a candidate is converted to a finding.
func TestCandidateSeverityFloor(t *testing.T) {
	c := reviewCandidate{File: "app/svc.go", Message: "x", Severity: "info"}
	f := candidateToFinding(c, config.ReviewRule{Name: "r", Severity: "error"})
	require.Equal(t, SevError, f.Severity, "the rule severity floor must raise the finding severity")
	require.Equal(t, "llm", f.Source)
}

// TestRankFileRiskUsesImpact proves per-file risk is taken from the impact map
// and the report is ranked worst-first.
func TestRankFileRiskUsesImpact(t *testing.T) {
	diff := &analysis.DiffResult{
		ChangedSymbols: []analysis.ChangedSymbol{
			{ID: "app/a.go::A", FilePath: "app/a.go", Line: 1},
			{ID: "app/b.go::B", FilePath: "app/b.go", Line: 1},
		},
		ChangedFiles: []string{"app/a.go", "app/b.go"},
	}
	impact := map[string]*analysis.ImpactResult{
		"app/a.go::A": {Risk: analysis.RiskCritical},
		"app/b.go::B": {Risk: analysis.RiskLow},
	}
	rows := rankFileRisk(diff, impact, nil, "", true)
	require.Len(t, rows, 2)
	require.Equal(t, "app/a.go", rows[0].File, "the critical-risk file ranks first")
	require.Equal(t, string(analysis.RiskCritical), rows[0].Risk)
	require.Equal(t, string(analysis.RiskLow), rows[1].Risk)
}

// TestRankFileRiskNormalizesRepoPrefix pins the multi-repo shape: changed
// symbols carry graph-prefixed file paths while the diff's changed files are
// repo-relative. The rollup must merge both onto one row per file (keyed
// relative), carrying the symbol's impact tier — not emit a prefixed
// impact-tier row plus a LOW diff-only duplicate.
func TestRankFileRiskNormalizesRepoPrefix(t *testing.T) {
	diff := &analysis.DiffResult{
		ChangedSymbols: []analysis.ChangedSymbol{
			{ID: "myrepo/app/a.go::A", FilePath: "myrepo/app/a.go", Line: 1},
		},
		ChangedFiles: []string{"app/a.go", "app/b.go"},
	}
	impact := map[string]*analysis.ImpactResult{
		"myrepo/app/a.go::A": {Risk: analysis.RiskCritical},
	}
	rows := rankFileRisk(diff, impact, nil, "myrepo", true)
	require.Len(t, rows, 2, "one row per file, prefixed and relative forms merged")
	require.Equal(t, "app/a.go", rows[0].File)
	require.Equal(t, string(analysis.RiskCritical), rows[0].Risk)
	require.Equal(t, "app/b.go", rows[1].File)
}

// TestRankFileRiskCoverageEvidence pins the per-file coverage rollup: the
// widest blast radius among the file's changed symbols, the changed-symbol
// count, and how many of them lack a covering test.
func TestRankFileRiskCoverageEvidence(t *testing.T) {
	diff := &analysis.DiffResult{
		ChangedSymbols: []analysis.ChangedSymbol{
			{ID: "app/a.go::A", FilePath: "app/a.go", Line: 1},
			{ID: "app/a.go::B", FilePath: "app/a.go", Line: 20},
		},
		ChangedFiles: []string{"app/a.go"},
	}
	impact := map[string]*analysis.ImpactResult{
		"app/a.go::A": {Risk: analysis.RiskCritical, TotalAffected: 42, TestFiles: []string{"app/a_test.go"}},
		"app/a.go::B": {Risk: analysis.RiskLow, TotalAffected: 3},
	}
	rows := rankFileRisk(diff, impact, nil, "", true)
	require.Len(t, rows, 1)
	require.Equal(t, 42, rows[0].Affected, "the widest symbol's blast radius wins")
	require.Equal(t, 2, rows[0].Symbols)
	require.Equal(t, 1, rows[0].Uncovered, "B has no covering test")
}

// TestRankFileRiskCoverageUnknown pins the epistemic guard: when the graph
// indexes no test symbols (coverageKnown false), no row may claim untested
// symbols — blindness is not a finding. The risk tier itself stays.
func TestRankFileRiskCoverageUnknown(t *testing.T) {
	diff := &analysis.DiffResult{
		ChangedSymbols: []analysis.ChangedSymbol{
			{ID: "app/a.go::A", FilePath: "app/a.go", Line: 1},
		},
		ChangedFiles: []string{"app/a.go"},
	}
	impact := map[string]*analysis.ImpactResult{
		"app/a.go::A": {Risk: analysis.RiskCritical, TotalAffected: 42},
	}
	rows := rankFileRisk(diff, impact, nil, "", false)
	require.Len(t, rows, 1)
	require.Equal(t, string(analysis.RiskCritical), rows[0].Risk, "the blast-radius tier stays")
	require.Equal(t, 42, rows[0].Affected, "blast radius is a graph fact, not a coverage claim")
	require.Zero(t, rows[0].Symbols, "no coverage claims when the index carries no tests")
	require.Zero(t, rows[0].Uncovered)
}

// TestSummarizeCoveragePhrasing pins the three risk-driven headlines: untested
// risk, fully covered risk, and unknown coverage.
func TestSummarizeCoveragePhrasing(t *testing.T) {
	critical := string(analysis.RiskCritical)

	untested := summarize(VerdictBlock, nil, []FileRisk{{File: "a.go", Risk: critical, Symbols: 2, Uncovered: 1}})
	require.Contains(t, untested, "1 without covering tests")

	covered := summarize(VerdictReview, nil, []FileRisk{{File: "a.go", Risk: critical, Symbols: 2}})
	require.Contains(t, covered, "all test-covered")

	unknown := summarize(VerdictBlock, nil, []FileRisk{{File: "a.go", Risk: critical}})
	require.Contains(t, unknown, "test coverage unknown")
}

// TestComputeVerdictCoverageCap pins the coverage temper: a critical-risk
// file whose changed symbols are all test-covered contributes at most
// REVIEW; the same file with an untested changed symbol blocks.
func TestComputeVerdictCoverageCap(t *testing.T) {
	covered := []FileRisk{{File: "a.go", Risk: string(analysis.RiskCritical), Symbols: 2, Uncovered: 0}}
	require.Equal(t, VerdictReview, computeVerdict(nil, covered),
		"blast radius alone must not block a fully test-covered change")

	untested := []FileRisk{{File: "a.go", Risk: string(analysis.RiskCritical), Symbols: 2, Uncovered: 1}}
	require.Equal(t, VerdictBlock, computeVerdict(nil, untested))

	// No coverage evidence (no impact data) keeps the conservative ladder.
	unknown := []FileRisk{{File: "a.go", Risk: string(analysis.RiskCritical)}}
	require.Equal(t, VerdictBlock, computeVerdict(nil, unknown))
}
