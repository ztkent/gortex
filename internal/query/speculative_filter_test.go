package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFilterSpeculative(t *testing.T) {
	mk := func() *SubGraph {
		return &SubGraph{Edges: []*graph.Edge{
			{From: "a", To: "b", Kind: graph.EdgeCalls},
			{From: "a", To: "c", Kind: graph.EdgeCalls, Meta: map[string]any{graph.MetaSpeculative: true}},
		}}
	}
	// Default-exclude: speculative dropped.
	sg := mk()
	sg.FilterSpeculative(false)
	if len(sg.Edges) != 1 || sg.Edges[0].To != "b" {
		t.Fatalf("FilterSpeculative(false) must drop speculative edges, got %d", len(sg.Edges))
	}
	// Opt-in: speculative kept.
	sg2 := mk()
	sg2.FilterSpeculative(true)
	if len(sg2.Edges) != 2 {
		t.Fatalf("FilterSpeculative(true) must keep speculative edges, got %d", len(sg2.Edges))
	}
}
