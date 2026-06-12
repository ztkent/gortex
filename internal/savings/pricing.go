package savings

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Price is input-token pricing in USD per 1M tokens. Tokens saved are input
// tokens that would have been sent to the model, so we always bill them at
// the input rate.
type Price struct {
	Model        string  `json:"model"`
	USDPerMInput float64 `json:"usd_per_m_input"`
}

// defaultPricing is the built-in table used when no override is configured.
// Input-token list prices in USD per 1M tokens, current as of Opus 4.8 /
// Fable 5. Tokens saved are billed at the input rate. Users can override via
// GORTEX_MODEL_PRICING_JSON='[{"model":"...","usd_per_m_input":N},...]'.
var defaultPricing = []Price{
	{"claude-fable-5", 10.00},
	{"claude-opus-4-8", 5.00},
	{"claude-opus-4-7", 5.00},
	{"claude-opus-4-6", 5.00},
	{"claude-sonnet-4-6", 3.00},
	{"claude-haiku-4-5", 1.00},
	{"gpt-4o", 2.50},
	{"gpt-4o-mini", 0.15},
}

// Pricing returns the active pricing table — default unless overridden by
// GORTEX_MODEL_PRICING_JSON. Falls back to the default on malformed JSON.
func Pricing() []Price {
	raw := os.Getenv("GORTEX_MODEL_PRICING_JSON")
	if raw == "" {
		return defaultPricing
	}
	var custom []Price
	if err := json.Unmarshal([]byte(raw), &custom); err != nil || len(custom) == 0 {
		return defaultPricing
	}
	return custom
}

// CostAvoided returns the dollar value of tokensSaved evaluated against the
// pricing row whose model matches `name` (case-insensitive, substring).
// Returns 0 when no matching entry exists.
func CostAvoided(tokensSaved int64, name string) float64 {
	if tokensSaved <= 0 {
		return 0
	}
	p := findPrice(name)
	if p == nil {
		return 0
	}
	return float64(tokensSaved) * p.USDPerMInput / 1_000_000.0
}

// ModelRate returns the USD-per-1M-token input rate for the named model
// (case-insensitive, substring — see findPrice), or 0 when no pricing row
// matches. Callers that need a ProviderPricing-shaped rate card (the
// review cost block) use this to derive one from the built-in table.
func ModelRate(name string) float64 {
	p := findPrice(name)
	if p == nil {
		return 0
	}
	return p.USDPerMInput
}

// CostAvoidedAll returns the cost across every entry in the pricing table,
// keyed by model name — useful for the CLI's multi-model summary.
func CostAvoidedAll(tokensSaved int64) map[string]float64 {
	out := make(map[string]float64, len(defaultPricing))
	for _, p := range Pricing() {
		out[p.Model] = float64(tokensSaved) * p.USDPerMInput / 1_000_000.0
	}
	return out
}

// findPrice locates a pricing entry by case-insensitive substring match on
// model name so callers can pass "opus" or "claude-opus-4-8" interchangeably.
func findPrice(name string) *Price {
	if name == "" {
		return nil
	}
	target := strings.ToLower(name)
	prices := Pricing()
	// Exact match first, longest substring second — avoids "claude" matching
	// "claude-opus-4-8" when the user passed "claude-sonnet-4-6".
	for i := range prices {
		if strings.EqualFold(prices[i].Model, name) {
			return &prices[i]
		}
	}
	type hit struct {
		i      int
		length int
	}
	var hits []hit
	for i := range prices {
		m := strings.ToLower(prices[i].Model)
		if strings.Contains(m, target) || strings.Contains(target, m) {
			hits = append(hits, hit{i, len(prices[i].Model)})
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].length > hits[b].length })
	return &prices[hits[0].i]
}
