package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func newEventBusIndexer(t *testing.T, specs []config.EventBusBoundarySpec) *Indexer {
	t.Helper()
	cfg := config.Default().Index
	cfg.EventBus = specs
	return New(graph.New(), parser.NewRegistry(), cfg, zap.NewNop())
}

func TestEventBusBoundaries_FromConfig(t *testing.T) {
	idx := newEventBusIndexer(t, []config.EventBusBoundarySpec{
		{Name: "k", Type: "producer", Callee: "p.send", TopicArg: "topic"},
	})
	b := idx.eventBusBoundaries()
	require.Len(t, b, 1)
	require.Equal(t, "k", b[0].Name)
	require.Equal(t, "p.send", b[0].Callee)
}

func TestEventBusBoundaries_EnvOverride(t *testing.T) {
	t.Setenv("CODEGRAPH_EVENT_CONFIG", `[{"name":"envbus","type":"consumer","match":{"callee_pattern":"EventSource","args":{"url":0}}}]`)
	idx := newEventBusIndexer(t, []config.EventBusBoundarySpec{
		{Name: "fromfile", Type: "producer", Callee: "x.y"},
	})
	b := idx.eventBusBoundaries()
	require.Len(t, b, 1)
	require.Equal(t, "envbus", b[0].Name, "env config must override index.event_bus")
	require.Equal(t, "EventSource", b[0].CalleePattern)
	require.Equal(t, "0", b[0].TopicArg, "args{url:0} should map to topic_arg 0")
}

func TestEventBusBoundaries_Disable(t *testing.T) {
	t.Setenv("GORTEX_EVENT_BUS_DISABLE", "1")
	idx := newEventBusIndexer(t, []config.EventBusBoundarySpec{
		{Name: "k", Type: "producer", Callee: "p.send"},
	})
	require.Empty(t, idx.eventBusBoundaries())
}

func TestEventBusBoundaries_MalformedEnvFallsBack(t *testing.T) {
	t.Setenv("CODEGRAPH_EVENT_CONFIG", `{not valid json`)
	idx := newEventBusIndexer(t, []config.EventBusBoundarySpec{
		{Name: "fromfile", Type: "producer", Callee: "x.y"},
	})
	b := idx.eventBusBoundaries()
	require.Len(t, b, 1)
	require.Equal(t, "fromfile", b[0].Name, "malformed env must fall back to index.event_bus")
}

func TestEventBusBoundaries_SkipsMalformedSpec(t *testing.T) {
	idx := newEventBusIndexer(t, []config.EventBusBoundarySpec{
		{Name: "ok", Type: "producer", Callee: "p.send"},
		{Name: "", Type: "producer", Callee: "p.send"}, // no name → skip
		{Name: "nomatch", Type: "consumer"},            // no match clause → skip
	})
	b := idx.eventBusBoundaries()
	require.Len(t, b, 1)
	require.Equal(t, "ok", b[0].Name)
}

// TestEventBus_EndToEnd indexes a producer + consumer of the same configured
// bus topic and asserts the shared topic contract node carries both an
// EdgeProvides (producer) and EdgeConsumes (consumer) edge — the boundaries
// are now first-class, queryable graph elements.
func TestEventBus_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "producer.py"), []byte(
		"def emit():\n    bus.publish('orders', payload)\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "consumer.py"), []byte(
		"@subscribe(topic='orders')\ndef on_order(msg):\n    pass\n"), 0o644))

	cfg := config.Default().Index
	cfg.EventBus = []config.EventBusBoundarySpec{
		{Name: "orderbus", Type: "producer", Callee: "bus.publish", TopicArg: "0"},
		{Name: "orderbus", Type: "consumer", Decorator: "subscribe", TopicArg: "topic"},
	}
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, cfg, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	node := g.GetNode("topic::orderbus::orders")
	require.NotNil(t, node, "expected a shared topic contract node for the configured bus")

	var provides, consumes int
	for _, e := range g.GetInEdges("topic::orderbus::orders") {
		switch e.Kind {
		case graph.EdgeProvides:
			provides++
		case graph.EdgeConsumes:
			consumes++
		}
	}
	require.GreaterOrEqual(t, provides, 1, "producer should provide the topic")
	require.GreaterOrEqual(t, consumes, 1, "consumer should consume the topic")
}
