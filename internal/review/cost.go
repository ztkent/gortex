package review

import (
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/registry"
	"github.com/zzet/gortex/internal/tokens"
)

// CostBreakdown is the per-review token + cost accounting surfaced on a
// ReviewReport. It carries the token split a provider reported (input /
// output / prompt-cache read / prompt-cache write), the wall-clock cost
// of the LLM phase, and a USD estimate derived from the active
// provider's pricing. Estimated is false when no usage was available
// (a subprocess provider, or a provider whose usage block is not
// decoded) — the cost block is still emitted, just zero.
type CostBreakdown struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	USD              float64 `json:"usd"`
	Estimated        bool    `json:"estimated"`
	ElapsedMs        int64   `json:"elapsed_ms"`
}

// CostFromUsage builds a CostBreakdown from a provider's token usage, the
// USD-per-1M-token pricing, and the elapsed LLM time. The price carries
// the input / output rates; CostFromUsage uses registry.EstimateCost so
// the USD math is never re-implemented here. Estimated is true whenever
// any token was reported — a non-zero usage means the number is grounded
// in a real accounting rather than a heuristic.
func CostFromUsage(usage llm.TokenUsage, price llm.ProviderPricing, elapsedMs int64) CostBreakdown {
	cb := CostBreakdown{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		ElapsedMs:        elapsedMs,
		Estimated:        !usage.IsZero(),
	}
	cb.USD = registry.EstimateCost(
		llm.CustomProvider{Pricing: price},
		int64(usage.InputTokens),
		int64(usage.OutputTokens),
	)
	return cb
}

// AttributeFindingTokens distributes the run's output tokens across the
// findings in proportion to each finding's body+message token weight,
// writing the share onto Finding.GenTokens. The deterministic rulepack
// findings carry no generated text, so weight falls naturally on the LLM
// findings; a finding with no body/message gets a minimum weight of one
// so the total is conserved. Rounding remainders are folded into the
// last finding so the per-finding shares sum to the total output tokens.
func AttributeFindingTokens(findings []Finding, total llm.TokenUsage) {
	if len(findings) == 0 || total.OutputTokens <= 0 {
		return
	}
	weights := make([]int, len(findings))
	sum := 0
	for i := range findings {
		w := tokens.Count(findings[i].Body) + tokens.Count(findings[i].Message)
		if w <= 0 {
			w = 1
		}
		weights[i] = w
		sum += w
	}
	if sum <= 0 {
		return
	}
	assigned := 0
	for i := range findings {
		share := total.OutputTokens * weights[i] / sum
		findings[i].GenTokens = share
		assigned += share
	}
	// Fold the rounding remainder into the last finding so the shares
	// sum exactly to the output-token total.
	if rem := total.OutputTokens - assigned; rem != 0 {
		findings[len(findings)-1].GenTokens += rem
	}
}
