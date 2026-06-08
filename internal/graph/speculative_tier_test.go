package graph

import "testing"

func TestOriginRank_Speculative(t *testing.T) {
	if OriginRank(OriginSpeculative) >= OriginRank(OriginTextMatched) {
		t.Errorf("speculative (%d) must rank below text_matched (%d)",
			OriginRank(OriginSpeculative), OriginRank(OriginTextMatched))
	}
	if OriginRank(OriginSpeculative) <= OriginRank("") {
		t.Errorf("speculative must rank above unknown/empty")
	}
	if MeetsMinTier(OriginSpeculative, OriginTextMatched) {
		t.Errorf("min_tier=text_matched must exclude speculative edges")
	}
	if !MeetsMinTier(OriginSpeculative, "") {
		t.Errorf("empty min_tier must pass speculative edges")
	}
}

func TestEdge_IsSpeculative(t *testing.T) {
	if (&Edge{}).IsSpeculative() {
		t.Errorf("plain edge must not be speculative")
	}
	e := &Edge{Meta: map[string]any{MetaSpeculative: true}}
	if !e.IsSpeculative() {
		t.Errorf("edge with MetaSpeculative=true must be speculative")
	}
}
