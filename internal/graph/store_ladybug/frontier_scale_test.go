package store_ladybug_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
	"github.com/zzet/gortex/internal/query"
)

// TestBFS_BoundsHugeFanInHub is the regression guard for the
// smart_context 40 GB / 8-min incident. A routing hub with thousands of
// inbound edges must not drag its entire adjacency across the cgo
// boundary: GetCallers over the ladybug store routes through
// Engine.bfs -> Store.ExpandFrontier, which applies a server-side LIMIT,
// so the result is bounded by the node limit regardless of the hub's
// true degree. Pre-fix, bfs fetched every inbound edge with no LIMIT and
// issued one GetNode cgo round-trip per edge.
func TestBFS_BoundsHugeFanInHub(t *testing.T) {
	const fanIn = 2000 // >> limit (64) and >> frontierRowCap (512)

	s, err := store_ladybug.Open(filepath.Join(t.TempDir(), "fanin.kuzu"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	nodes := make([]*graph.Node, 0, fanIn+1)
	edges := make([]*graph.Edge, 0, fanIn)
	nodes = append(nodes, &graph.Node{ID: "hub", Name: "hub", Kind: graph.KindFunction, FilePath: "hub.go", WorkspaceID: "ws"})
	for i := 0; i < fanIn; i++ {
		id := fmt.Sprintf("caller%05d", i)
		nodes = append(nodes, &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: id + ".go", WorkspaceID: "ws"})
		edges = append(edges, &graph.Edge{From: id, To: "hub", Kind: graph.EdgeCalls, FilePath: id + ".go", Line: 1})
	}
	s.AddBatch(nodes, edges)

	// Sanity: the hub really has fanIn callers in the store.
	if got := len(s.GetInEdges("hub")); got != fanIn {
		t.Fatalf("store seeded with %d inbound edges, want %d", got, fanIn)
	}

	eng := query.NewEngine(s)
	const limit = 64
	start := time.Now()
	sg := eng.GetCallers("hub", query.QueryOptions{Depth: 1, Limit: limit, Detail: "brief", WorkspaceID: "ws"})
	elapsed := time.Since(start)

	// The fix: result bounded by the node limit, not the hub's true degree.
	if len(sg.Nodes) > limit+1 { // +1 for the seed node
		t.Fatalf("GetCallers returned %d nodes, want <= %d (limit+seed) — fan not bounded", len(sg.Nodes), limit+1)
	}
	// Edges are appended only while under the node budget, so they are
	// bounded too — far below the hub's true fan-in (the heap-blowup guard).
	if len(sg.Edges) > limit+1 {
		t.Fatalf("GetCallers returned %d edges, want <= %d — server-side LIMIT not applied (pre-fix: %d)", len(sg.Edges), limit+1, fanIn)
	}
	if !sg.Truncated {
		t.Fatalf("a %d-fan-in hub capped at limit %d must report Truncated", fanIn, limit)
	}
	// The seed must be present and in-scope neighbours must have come back.
	if len(sg.Nodes) < 2 {
		t.Fatalf("GetCallers returned %d nodes, expected the hub plus callers", len(sg.Nodes))
	}
	t.Logf("GetCallers over %d-fan-in hub: %d nodes, %d edges in %s (pre-fix would materialise %d edges + %d GetNode round-trips)",
		fanIn, len(sg.Nodes), len(sg.Edges), elapsed, fanIn, fanIn)
}
