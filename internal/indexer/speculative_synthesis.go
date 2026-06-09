package indexer

import (
	"os"
	"strings"
)

// speculativeDispatchEnabled reports whether the resolver should mint opt-in,
// best-guess speculative call edges for dynamic-dispatch blind spots
// (computed-member calls, getattr, decorator registries).
// GORTEX_SYNTH_SPECULATIVE overrides the index.synthesize_speculative_dispatch
// config key. OFF by default — these edges are heuristic fan-outs and are
// hidden from every default query.
func (idx *Indexer) speculativeDispatchEnabled() bool {
	if v := os.Getenv("GORTEX_SYNTH_SPECULATIVE"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.SpeculativeDispatchEnabledOrDefault()
}
