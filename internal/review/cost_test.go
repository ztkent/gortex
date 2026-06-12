package review

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/llm"
)

// TestCostFromUsage_TokenSplitAndUSD asserts the breakdown carries the
// exact token split and a USD figure computed from the price card.
func TestCostFromUsage_TokenSplitAndUSD(t *testing.T) {
	usage := llm.TokenUsage{InputTokens: 1234, OutputTokens: 456, CacheReadTokens: 2000, CacheWriteTokens: 300}
	price := llm.ProviderPricing{Input: 3.0, Output: 15.0} // USD per 1M tokens

	cb := CostFromUsage(usage, price, 4210)

	require.Equal(t, 1234, cb.InputTokens)
	require.Equal(t, 456, cb.OutputTokens)
	require.Equal(t, 2000, cb.CacheReadTokens)
	require.Equal(t, 300, cb.CacheWriteTokens)
	require.Equal(t, int64(4210), cb.ElapsedMs)
	require.True(t, cb.Estimated, "a non-zero usage is a grounded estimate")

	// 1234 * 3.0/1e6 + 456 * 15.0/1e6
	wantUSD := (1234*3.0 + 456*15.0) / 1_000_000.0
	require.InDelta(t, wantUSD, cb.USD, 1e-12)
	require.Greater(t, cb.USD, 0.0, "a priced custom provider yields a nonzero USD")
}

// TestCostFromUsage_ZeroUsageNotEstimated asserts a no-usage provider
// still yields a (zero) cost block flagged Estimated:false.
func TestCostFromUsage_ZeroUsageNotEstimated(t *testing.T) {
	cb := CostFromUsage(llm.TokenUsage{}, llm.ProviderPricing{Input: 3.0, Output: 15.0}, 1000)
	require.False(t, cb.Estimated)
	require.Equal(t, 0.0, cb.USD)
	require.Equal(t, int64(1000), cb.ElapsedMs)
}

// TestAttributeFindingTokens_SumsToTotal asserts output tokens are split
// across findings proportionally and conserve the total (±rounding folded
// into the last finding).
func TestAttributeFindingTokens_SumsToTotal(t *testing.T) {
	findings := []Finding{
		{Message: "short", Body: "tiny body"},
		{Message: "a much longer finding message", Body: "a considerably longer body with more tokens to weigh"},
		{Message: "mid", Body: "medium body here"},
	}
	total := llm.TokenUsage{OutputTokens: 1000}

	AttributeFindingTokens(findings, total)

	sum := 0
	for _, f := range findings {
		require.GreaterOrEqual(t, f.GenTokens, 0)
		sum += f.GenTokens
	}
	require.Equal(t, total.OutputTokens, sum, "per-finding shares must sum to the output total")
	// The longest finding must get at least as many tokens as the shortest.
	require.GreaterOrEqual(t, findings[1].GenTokens, findings[0].GenTokens)
}

func TestAttributeFindingTokens_NoOpOnEmpty(t *testing.T) {
	// Nothing to attribute — must not panic and must leave GenTokens zero.
	AttributeFindingTokens(nil, llm.TokenUsage{OutputTokens: 100})
	f := []Finding{{Message: "x"}}
	AttributeFindingTokens(f, llm.TokenUsage{OutputTokens: 0})
	require.Equal(t, 0, f[0].GenTokens)
}

// TestRunWithUsage_SurfacesCostBreakdown asserts a review driven through
// the usage-aware seam attaches a CostBreakdown to the report, with the
// summed usage, USD from the price card, and per-finding GenTokens.
func TestRunWithUsage_SurfacesCostBreakdown(t *testing.T) {
	reply := `[
	  {"file":"app/svc.go","snippet":"return p.Balance","message":"possible nil dereference of p","severity":"critical","category":"correctness"}
	]`
	usage := llm.TokenUsage{InputTokens: 800, OutputTokens: 200, CacheReadTokens: 100}
	gen := func(_ context.Context, _ string, _ int) (string, llm.TokenUsage, error) {
		return reply, usage, nil
	}
	price := llm.ProviderPricing{Input: 3.0, Output: 15.0}

	report, err := RunWithUsage(context.Background(), nil, gen, price, Options{
		RepoRoot: "/tmp/repo",
		Diff:     sampleDiff,
		Rules:    testResolver(t),
		UseLLM:   true,
	})
	require.NoError(t, err)
	require.NotNil(t, report)
	require.NotNil(t, report.Cost, "a usage-aware seam must surface a cost block")

	require.Equal(t, 800, report.Cost.InputTokens)
	require.Equal(t, 200, report.Cost.OutputTokens)
	require.Equal(t, 100, report.Cost.CacheReadTokens)
	require.True(t, report.Cost.Estimated)
	require.Greater(t, report.Cost.USD, 0.0)

	// The LLM finding carries an attributed token share.
	var sawLLM bool
	for _, f := range report.Findings {
		if f.Source == "llm" {
			sawLLM = true
			require.Greater(t, f.GenTokens, 0, "an LLM finding must carry attributed gen tokens")
		}
	}
	require.True(t, sawLLM, "the LLM finding must survive into the report")
}

// TestRunWithUsage_NilSeamZeroCost asserts a nil seam still produces a
// report with a zero, Estimated:false cost block (deterministic-only).
func TestRunWithUsage_NilSeamZeroCost(t *testing.T) {
	report, err := RunWithUsage(context.Background(), nil, nil, llm.ProviderPricing{}, Options{
		RepoRoot: "/tmp/repo",
		Diff:     sampleDiff,
		Rules:    testResolver(t),
		UseLLM:   true,
	})
	require.NoError(t, err)
	require.NotNil(t, report.Cost)
	require.False(t, report.Cost.Estimated)
	require.Equal(t, 0.0, report.Cost.USD)
}
