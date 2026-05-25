//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestAlgo_Probe walks the ALGO extension's surface:
//
//  1. INSTALL ALGO + LOAD EXTENSION ALGO (mirrors FTS / VECTOR dance)
//  2. CALL PROJECT_GRAPH('G', ['Node'], ['Edge']) — declare a projected
//     subgraph the algos run over
//  3. CALL page_rank, louvain, weakly_connected_components,
//     strongly_connected_components, k_core_decomposition each in turn
//     against the projection
//  4. CALL DROP_PROJECTED_GRAPH('G') to clean up (we want to know if a
//     projection is per-call or persistent)
//
// Liberal logging so the probe surfaces what works regardless of where
// the algo extension's surface lands relative to the docs.
func TestAlgo_Probe(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-algo-probe-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Step 1: install + load. INSTALL may report "already installed" on
	// repeat runs — log and continue either way.
	for _, q := range []string{`INSTALL ALGO`, `LOAD EXTENSION ALGO`} {
		if err := tryRunCypher(s, q); err != nil {
			t.Logf("%s: %v", q, err)
		} else {
			t.Logf("%s: ok", q)
		}
	}

	// Step 2: seed a small directed graph with two clear communities
	// plus a hub node that ties them together. Layout:
	//
	//   a -> b -> c -> a   (triangle 1, SCC + community A)
	//   d -> e -> f -> d   (triangle 2, SCC + community B)
	//   c -> d             (bridge — makes it one WCC but two SCCs)
	//   hub <- a,b,c,d,e,f (incoming hub → high PageRank)
	for _, n := range []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a", FilePath: "x.go"},
		{ID: "b", Kind: graph.KindFunction, Name: "b", FilePath: "x.go"},
		{ID: "c", Kind: graph.KindFunction, Name: "c", FilePath: "x.go"},
		{ID: "d", Kind: graph.KindFunction, Name: "d", FilePath: "y.go"},
		{ID: "e", Kind: graph.KindFunction, Name: "e", FilePath: "y.go"},
		{ID: "f", Kind: graph.KindFunction, Name: "f", FilePath: "y.go"},
		{ID: "hub", Kind: graph.KindFunction, Name: "hub", FilePath: "z.go"},
	} {
		s.AddNode(n)
	}
	for _, e := range []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "b", To: "c", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "c", To: "a", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "d", To: "e", Kind: graph.EdgeCalls, FilePath: "y.go"},
		{From: "e", To: "f", Kind: graph.EdgeCalls, FilePath: "y.go"},
		{From: "f", To: "d", Kind: graph.EdgeCalls, FilePath: "y.go"},
		{From: "c", To: "d", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "a", To: "hub", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "b", To: "hub", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "c", To: "hub", Kind: graph.EdgeCalls, FilePath: "x.go"},
		{From: "d", To: "hub", Kind: graph.EdgeCalls, FilePath: "y.go"},
		{From: "e", To: "hub", Kind: graph.EdgeCalls, FilePath: "y.go"},
		{From: "f", To: "hub", Kind: graph.EdgeCalls, FilePath: "y.go"},
	} {
		s.AddEdge(e)
	}
	t.Logf("seeded %d nodes, %d edges", s.NodeCount(), s.EdgeCount())

	// Step 3: declare the projection. Try the simple form first; fall
	// back to alternate spellings if the binder rejects the literal.
	for _, q := range []string{
		`CALL PROJECT_GRAPH('G', ['Node'], ['Edge'])`,
		`CALL project_graph('G', ['Node'], ['Edge'])`,
	} {
		if err := tryRunCypher(s, q); err != nil {
			t.Logf("%s: %v", q, err)
		} else {
			t.Logf("%s: ok", q)
			break
		}
	}

	// Step 4: try every algo. Each is logged independently so a single
	// missing function doesn't abort the others.
	probes := []struct {
		name string
		q    string
	}{
		{"page_rank", `CALL page_rank('G') RETURN node.id AS id, rank ORDER BY rank DESC LIMIT 10`},
		{"page_rank_with_opts", `CALL page_rank('G', dampingFactor := 0.85, maxIterations := 20) RETURN node.id AS id, rank ORDER BY rank DESC LIMIT 10`},
		{"louvain", `CALL louvain('G') RETURN node.id AS id, louvain_id ORDER BY louvain_id LIMIT 20`},
		{"weakly_connected_components", `CALL weakly_connected_components('G') RETURN node.id AS id, group_id ORDER BY group_id LIMIT 20`},
		{"strongly_connected_components", `CALL strongly_connected_components('G') RETURN node.id AS id, group_id ORDER BY group_id LIMIT 20`},
		{"strongly_connected_components_kosaraju", `CALL strongly_connected_components_kosaraju('G') RETURN node.id AS id, group_id ORDER BY group_id LIMIT 20`},
		{"k_core_decomposition", `CALL k_core_decomposition('G') RETURN node.id AS id, k_degree ORDER BY k_degree DESC LIMIT 20`},
	}
	for _, p := range probes {
		rows, qerr := tryQueryCypher(s, p.q, nil)
		if qerr != nil {
			t.Logf("%s: error: %v", p.name, qerr)
			continue
		}
		t.Logf("%s → %d rows", p.name, len(rows))
		for _, r := range rows {
			t.Logf("  %v", r)
		}
	}

	// Step 5: drop the projection and see whether re-projecting is
	// allowed. If not, projections are per-session / per-call.
	for _, q := range []string{
		`CALL DROP_PROJECTED_GRAPH('G')`,
		`CALL drop_projected_graph('G')`,
	} {
		if err := tryRunCypher(s, q); err != nil {
			t.Logf("%s: %v", q, err)
		} else {
			t.Logf("%s: ok", q)
			break
		}
	}
}
