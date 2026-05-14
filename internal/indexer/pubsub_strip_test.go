package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func pubsubStripFixture() *parser.ExtractionResult {
	return &parser.ExtractionResult{
		Nodes: []*graph.Node{
			{ID: "f.go", Kind: graph.KindFile},
			{ID: "f.go::Pub", Kind: graph.KindFunction},
			{ID: "event::log::user.login", Kind: graph.KindEvent, Meta: map[string]any{"event_kind": "log"}},
			{ID: "event::pubsub::nats::orders", Kind: graph.KindEvent, Meta: map[string]any{"event_kind": "pubsub"}},
		},
		Edges: []*graph.Edge{
			{From: "f.go", To: "f.go::Pub", Kind: graph.EdgeDefines},
			{From: "f.go::Pub", To: "event::log::user.login", Kind: graph.EdgeEmits},
			{From: "f.go::Pub", To: "event::pubsub::nats::orders", Kind: graph.EdgeEmits},
			{From: "f.go::Pub", To: "event::pubsub::nats::orders", Kind: graph.EdgeListensOn},
		},
	}
}

func countNodeKind(nodes []*graph.Node, k graph.NodeKind) int {
	n := 0
	for _, node := range nodes {
		if node.Kind == k {
			n++
		}
	}
	return n
}

func countEdgeKind(edges []*graph.Edge, k graph.EdgeKind) int {
	n := 0
	for _, e := range edges {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func TestStripObservabilityArtifacts_KeepsPubsub(t *testing.T) {
	r := pubsubStripFixture()
	stripObservabilityArtifacts(r)

	// The log event is gone; the pub/sub topic node survives.
	if got := countNodeKind(r.Nodes, graph.KindEvent); got != 1 {
		t.Fatalf("expected 1 KindEvent left (pubsub), got %d", got)
	}
	for _, n := range r.Nodes {
		if n.Kind == graph.KindEvent && !isPubsubEventNode(n) {
			t.Errorf("non-pubsub event %q survived observability strip", n.ID)
		}
	}
	// The EdgeEmits into the log event is gone; the EdgeEmits and
	// EdgeListensOn into the pub/sub topic are untouched.
	if got := countEdgeKind(r.Edges, graph.EdgeEmits); got != 1 {
		t.Errorf("expected 1 EdgeEmits left (pubsub publish), got %d", got)
	}
	if got := countEdgeKind(r.Edges, graph.EdgeListensOn); got != 1 {
		t.Errorf("expected EdgeListensOn untouched, got %d", got)
	}
	if got := countEdgeKind(r.Edges, graph.EdgeDefines); got != 1 {
		t.Errorf("expected EdgeDefines untouched, got %d", got)
	}
}

func TestStripPubsubArtifacts_KeepsObservability(t *testing.T) {
	r := pubsubStripFixture()
	stripPubsubArtifacts(r)

	// The pub/sub topic node is gone; the log event survives.
	if got := countNodeKind(r.Nodes, graph.KindEvent); got != 1 {
		t.Fatalf("expected 1 KindEvent left (log), got %d", got)
	}
	for _, n := range r.Nodes {
		if isPubsubEventNode(n) {
			t.Errorf("pubsub event %q survived pubsub strip", n.ID)
		}
	}
	// EdgeListensOn is pubsub-only — fully stripped. The EdgeEmits into
	// the pub/sub topic is stripped; the EdgeEmits into the log event
	// stays.
	if got := countEdgeKind(r.Edges, graph.EdgeListensOn); got != 0 {
		t.Errorf("expected 0 EdgeListensOn, got %d", got)
	}
	if got := countEdgeKind(r.Edges, graph.EdgeEmits); got != 1 {
		t.Errorf("expected 1 EdgeEmits left (log emit), got %d", got)
	}
}

func TestStripBothDomains(t *testing.T) {
	r := pubsubStripFixture()
	stripObservabilityArtifacts(r)
	stripPubsubArtifacts(r)

	if got := countNodeKind(r.Nodes, graph.KindEvent); got != 0 {
		t.Errorf("expected no KindEvent nodes after both strips, got %d", got)
	}
	if got := countEdgeKind(r.Edges, graph.EdgeEmits); got != 0 {
		t.Errorf("expected no EdgeEmits after both strips, got %d", got)
	}
	if got := countEdgeKind(r.Edges, graph.EdgeListensOn); got != 0 {
		t.Errorf("expected no EdgeListensOn after both strips, got %d", got)
	}
	// Non-event structure is untouched.
	if got := countEdgeKind(r.Edges, graph.EdgeDefines); got != 1 {
		t.Errorf("expected EdgeDefines preserved, got %d", got)
	}
	if len(r.Nodes) != 2 {
		t.Errorf("expected file + function nodes preserved, got %d", len(r.Nodes))
	}
}
