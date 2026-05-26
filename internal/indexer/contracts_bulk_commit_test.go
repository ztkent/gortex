package indexer

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// recordingBulkGraph embeds *graph.Graph (auto-satisfying graph.Store)
// and adds the BulkLoader methods so it also satisfies
// graph.BulkLoader. It records the order of BeginBulkLoad / AddBatch
// / FlushBulk calls so a test can assert that the contracts commit
// path routes through the bulk fast lane instead of per-row
// AddNode / AddEdge writes.
type recordingBulkGraph struct {
	*graph.Graph

	calls   []string
	addNode atomic.Int64
	addEdge atomic.Int64
}

func newRecordingBulkGraph() *recordingBulkGraph {
	return &recordingBulkGraph{Graph: graph.New()}
}

func (r *recordingBulkGraph) BeginBulkLoad() {
	r.calls = append(r.calls, "BeginBulkLoad")
}

func (r *recordingBulkGraph) FlushBulk() error {
	r.calls = append(r.calls, "FlushBulk")
	return nil
}

func (r *recordingBulkGraph) AddNode(n *graph.Node) {
	r.addNode.Add(1)
	r.Graph.AddNode(n)
}

func (r *recordingBulkGraph) AddEdge(e *graph.Edge) {
	r.addEdge.Add(1)
	r.Graph.AddEdge(e)
}

func (r *recordingBulkGraph) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	r.calls = append(r.calls, "AddBatch")
	r.Graph.AddBatch(nodes, edges)
}

// TestCommitContracts_BatchesViaAddBatch asserts that the final
// write phase of commitContracts emits all contract nodes and
// edges through a single AddBatch call and does NOT engage the
// BulkLoader COPY bracket. Contract IDs frequently coincide with
// existing source-symbol IDs (a handler appears as both a Go
// function and an HTTP-contract anchor), and Ladybug's COPY FROM
// is INSERT-only on the node table — wrapping the contracts pass
// in BeginBulkLoad/FlushBulk would crash on the first collision.
// AddBatch's per-call MERGE path absorbs duplicates safely.
func TestCommitContracts_BatchesViaAddBatch(t *testing.T) {
	g := newRecordingBulkGraph()
	require.Implements(t, (*graph.BulkLoader)(nil), graph.Store(g))

	// Anchor symbol the contract's provides-edge will point from.
	g.Graph.AddNode(&graph.Node{
		ID:       "pkg/foo.go::Handler.List",
		Kind:     graph.KindMethod,
		Name:     "List",
		FilePath: "pkg/foo.go",
		Language: "go",
	})

	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())

	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{
		ID:       "http::GET::/v1/items",
		Type:     contracts.ContractHTTP,
		Role:     contracts.RoleProvider,
		SymbolID: "pkg/foo.go::Handler.List",
		FilePath: "pkg/foo.go",
		Line:     42,
	})
	reg.Add(contracts.Contract{
		ID:       "http::POST::/v1/items",
		Type:     contracts.ContractHTTP,
		Role:     contracts.RoleConsumer,
		SymbolID: "pkg/foo.go::Handler.List",
		FilePath: "pkg/foo.go",
		Line:     58,
	})

	idx.commitContracts(reg)

	require.Equal(t,
		[]string{"AddBatch"},
		g.calls,
		"contracts commit must batch through a single AddBatch call",
	)
	require.Zero(t, g.addNode.Load(), "no per-row AddNode calls expected")
	require.Zero(t, g.addEdge.Load(), "no per-row AddEdge calls expected")

	require.NotNil(t, g.GetNode("http::GET::/v1/items"))
	require.NotNil(t, g.GetNode("http::POST::/v1/items"))

	// Provider contract emits both EdgeProvides and EdgeHandlesRoute;
	// consumer contract emits only EdgeConsumes.
	provides := g.GetOutEdges("pkg/foo.go::Handler.List")
	var nProvides, nConsumes, nHandles int
	for _, e := range provides {
		switch e.Kind {
		case graph.EdgeProvides:
			nProvides++
		case graph.EdgeConsumes:
			nConsumes++
		case graph.EdgeHandlesRoute:
			nHandles++
		}
	}
	require.Equal(t, 1, nProvides, "expected 1 EdgeProvides for the provider contract")
	require.Equal(t, 1, nConsumes, "expected 1 EdgeConsumes for the consumer contract")
	require.Equal(t, 1, nHandles, "expected 1 EdgeHandlesRoute for the HTTP provider")
}

// TestCommitContracts_NoBulkLoader_FallsBackToAddBatch asserts that
// when the backend does not implement graph.BulkLoader (the
// in-memory *graph.Graph case) commitContracts still issues a
// single AddBatch — not the per-row AddNode / AddEdge writes — and
// does not attempt to call BeginBulkLoad / FlushBulk.
func TestCommitContracts_NoBulkLoader_FallsBackToAddBatch(t *testing.T) {
	g := graph.New()
	require.NotImplements(t, (*graph.BulkLoader)(nil), graph.Store(g))

	g.AddNode(&graph.Node{
		ID:       "pkg/foo.go::Handler.List",
		Kind:     graph.KindMethod,
		Name:     "List",
		FilePath: "pkg/foo.go",
		Language: "go",
	})

	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())

	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{
		ID:       "http::GET::/v1/items",
		Type:     contracts.ContractHTTP,
		Role:     contracts.RoleProvider,
		SymbolID: "pkg/foo.go::Handler.List",
		FilePath: "pkg/foo.go",
		Line:     42,
	})

	idx.commitContracts(reg)

	require.NotNil(t, g.GetNode("http::GET::/v1/items"))
	out := g.GetOutEdges("pkg/foo.go::Handler.List")
	var nProvides, nHandles int
	for _, e := range out {
		switch e.Kind {
		case graph.EdgeProvides:
			nProvides++
		case graph.EdgeHandlesRoute:
			nHandles++
		}
	}
	require.Equal(t, 1, nProvides)
	require.Equal(t, 1, nHandles)
}

// TestExtractGoModContracts_UsesAddBatch asserts that go.mod
// dependency-contract emission goes through a single AddBatch
// call (with the bulk path engaged when the backend supports it)
// instead of the per-row AddNode loop that previously did one
// cgo round-trip per dependency on the Ladybug backend.
func TestExtractGoModContracts_UsesAddBatch(t *testing.T) {
	dir := t.TempDir()
	goMod := []byte(`module example.com/test

go 1.22

require (
	github.com/dep/one v1.0.0
	github.com/dep/two v0.5.0
)
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o644))

	g := newRecordingBulkGraph()
	idx := New(g, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.rootPath = dir

	reg := contracts.NewRegistry()
	idx.extractGoModContracts(reg)

	require.Contains(t, g.calls, "AddBatch",
		"extractGoModContracts must emit dep nodes via a single AddBatch")
	require.Zero(t, g.addNode.Load(), "no per-row AddNode calls expected")
}

