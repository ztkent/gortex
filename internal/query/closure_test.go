package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// distanceByID flattens a closure result into an id→distance map for
// terse assertions.
func distanceByID(res *ClosureResult) map[string]int {
	out := make(map[string]int, len(res.Nodes))
	for _, m := range res.Nodes {
		out[m.Node.ID] = m.Distance
	}
	return out
}

func TestImportClosure_ExpandsOverDependencyEdges(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Default edge kinds (dependency allowlist) from main reach the
	// whole call chain plus the referenced type.
	res := e.ImportClosure([]string{"main.go::main"}, ClosureOptions{})
	dist := distanceByID(res)

	require.Contains(t, dist, "main.go::main")
	assert.Equal(t, 0, dist["main.go::main"])
	assert.Contains(t, dist, "pkg/server.go::Start")
	assert.Contains(t, dist, "pkg/db.go::Connect")
	assert.Contains(t, dist, "pkg/db.go::Ping")
	// references edge is in the dependency allowlist.
	assert.Contains(t, dist, "pkg/db.go::DBImpl")
}

func TestImportClosure_ExpandsOverImports(t *testing.T) {
	g := buildTestGraph()
	e := NewEngine(g)

	// The pkg/server.go file imports pkg/db.go. Seeding the file node
	// follows the imports edge into the dependency closure.
	res := e.ImportClosure([]string{"pkg/server.go"}, ClosureOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeImports},
	})
	dist := distanceByID(res)

	assert.Equal(t, 0, dist["pkg/server.go"])
	if assert.Contains(t, dist, "pkg/db.go") {
		assert.Equal(t, 1, dist["pkg/db.go"])
	}
}

func TestImportClosure_DistanceRanking(t *testing.T) {
	e := NewEngine(buildTestGraph())

	res := e.ImportClosure([]string{"main.go::main"}, ClosureOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls},
	})
	dist := distanceByID(res)

	// main -> Start -> Connect -> Ping is a depth-3 call chain.
	assert.Equal(t, 0, dist["main.go::main"])
	assert.Equal(t, 1, dist["pkg/server.go::Start"])
	assert.Equal(t, 2, dist["pkg/db.go::Connect"])
	assert.Equal(t, 3, dist["pkg/db.go::Ping"])

	// The result must be sorted by (distance, id) regardless of edge
	// enumeration order.
	for i := 1; i < len(res.Nodes); i++ {
		prev, cur := res.Nodes[i-1], res.Nodes[i]
		if prev.Distance == cur.Distance {
			assert.LessOrEqual(t, prev.Node.ID, cur.Node.ID)
		} else {
			assert.Less(t, prev.Distance, cur.Distance)
		}
	}
}

func TestImportClosure_NodeBudgetCap(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Cap at two nodes: the seed plus the single nearest neighbour.
	res := e.ImportClosure([]string{"main.go::main"}, ClosureOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls},
		MaxNodes:  2,
	})
	assert.LessOrEqual(t, len(res.Nodes), 2)
	assert.True(t, res.Truncated, "expected truncation under a 2-node cap on a 4-node chain")
	// The seed is always kept; the deepest node must have been dropped.
	dist := distanceByID(res)
	assert.Contains(t, dist, "main.go::main")
	assert.NotContains(t, dist, "pkg/db.go::Ping")
}

func TestImportClosure_DepthCap(t *testing.T) {
	e := NewEngine(buildTestGraph())

	res := e.ImportClosure([]string{"main.go::main"}, ClosureOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls},
		MaxDepth:  1,
	})
	dist := distanceByID(res)
	// Depth 1 reaches Start but not the deeper Connect / Ping.
	assert.Contains(t, dist, "pkg/server.go::Start")
	assert.NotContains(t, dist, "pkg/db.go::Connect")
	assert.NotContains(t, dist, "pkg/db.go::Ping")
}

func TestImportClosure_MultiSeedMergeTakesNearest(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Seed both main (distance to Ping = 3) and Connect (distance to
	// Ping = 1). The merged closure must record the NEAREST distance.
	res := e.ImportClosure([]string{"main.go::main", "pkg/db.go::Connect"}, ClosureOptions{
		EdgeKinds: []graph.EdgeKind{graph.EdgeCalls},
	})
	dist := distanceByID(res)

	assert.Equal(t, 0, dist["main.go::main"])
	assert.Equal(t, 0, dist["pkg/db.go::Connect"])
	// Ping is 1 hop from Connect, 3 from main -> nearest wins.
	assert.Equal(t, 1, dist["pkg/db.go::Ping"])
	// Both seeds are recorded, de-duplicated and sorted.
	assert.Equal(t, []string{"main.go::main", "pkg/db.go::Connect"}, res.SeedIDs)
}

func TestImportClosure_SkipsUnresolvedAndOutOfScope(t *testing.T) {
	g := buildTestGraph()
	for _, n := range g.AllNodes() {
		n.WorkspaceID = "main"
	}
	g.AddNode(&graph.Node{
		ID: "other/x.go::Foreign", Kind: graph.KindFunction, Name: "Foreign",
		FilePath: "other/x.go", Language: "go", WorkspaceID: "other",
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/db.go::Ping", To: "other/x.go::Foreign",
		Kind: graph.EdgeCalls, FilePath: "pkg/db.go", Line: 20,
	})
	// An unresolved target must never enter the closure.
	g.AddEdge(&graph.Edge{
		From: "main.go::main", To: "unresolved::Mystery",
		Kind: graph.EdgeCalls, FilePath: "main.go", Line: 9,
	})
	e := NewEngine(g)

	res := e.ImportClosure([]string{"main.go::main"}, ClosureOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		WorkspaceID: "main",
	})
	dist := distanceByID(res)
	assert.NotContains(t, dist, "other/x.go::Foreign", "out-of-scope neighbour must be dropped")
	assert.NotContains(t, dist, "unresolved::Mystery", "unresolved target must be dropped")
}

func TestImportClosure_NoSeedsResolved(t *testing.T) {
	e := NewEngine(buildTestGraph())

	res := e.ImportClosure([]string{"does/not::Exist", "unresolved::Nope"}, ClosureOptions{})
	assert.Empty(t, res.Nodes)
	assert.Empty(t, res.SeedIDs)
}
