package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedClosureGraph injects a small dependency chain onto the server's
// graph: a.go::A -> b.go::B -> c.go::C via calls, plus a.go::A
// references d.go::D. Returns nothing; the test reads it back through
// the context_closure tool.
func seedClosureGraph(t *testing.T, srv *Server) {
	t.Helper()
	g := srv.graph
	add := func(id, name, file string, start, end int) {
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name, FilePath: file,
			Language: "go", StartLine: start, EndLine: end,
			Meta: map[string]any{"signature": "func " + name + "()"},
		})
	}
	add("a.go::A", "A", "a.go", 1, 3)
	add("b.go::B", "B", "b.go", 1, 3)
	add("c.go::C", "C", "c.go", 1, 3)
	add("d.go::D", "D", "d.go", 1, 3)
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})

	g.AddEdge(&graph.Edge{From: "a.go::A", To: "b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "b.go::B", To: "c.go::C", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "a.go::A", To: "d.go::D", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 2})
	// The file node a.go "defines" A so a file seed resolves to A.
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::A", Kind: graph.EdgeDefines, FilePath: "a.go", Line: 1})
}

func memberDistance(t *testing.T, out map[string]any, id string) (int, bool) {
	t.Helper()
	members, ok := out["members"].([]any)
	require.True(t, ok, "members must be a list")
	for _, m := range members {
		row := m.(map[string]any)
		if row["id"] == id {
			return int(row["distance"].(float64)), true
		}
	}
	return 0, false
}

func TestContextClosure_ExpandsAndRanksByDistance(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "context_closure", map[string]any{
		"symbols":    "a.go::A",
		"edge_kinds": "calls,references",
	}))

	// The closure expands over both calls and references.
	dA, okA := memberDistance(t, out, "a.go::A")
	require.True(t, okA)
	assert.Equal(t, 0, dA)
	dB, okB := memberDistance(t, out, "b.go::B")
	require.True(t, okB)
	assert.Equal(t, 1, dB)
	dC, okC := memberDistance(t, out, "c.go::C")
	require.True(t, okC)
	assert.Equal(t, 2, dC)
	dD, okD := memberDistance(t, out, "d.go::D")
	require.True(t, okD)
	assert.Equal(t, 1, dD)

	// The token-budgeted manifest is attached.
	manifest, ok := out["context_manifest"].(map[string]any)
	require.True(t, ok, "context_manifest must be present")
	_, hasEntries := manifest["entries"]
	assert.True(t, hasEntries, "manifest must carry entries")
}

func TestContextClosure_FileSeedResolvesDefinedSymbols(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "context_closure", map[string]any{
		"files":      "a.go",
		"edge_kinds": "calls,references",
	}))

	// The file seed pulls in A (defined in a.go) as a distance-0 seed,
	// then expands the closure from it.
	dA, okA := memberDistance(t, out, "a.go::A")
	require.True(t, okA)
	assert.Equal(t, 0, dA)
	_, okB := memberDistance(t, out, "b.go::B")
	assert.True(t, okB)

	resolved, ok := out["resolved_files"].([]any)
	require.True(t, ok)
	assert.Contains(t, resolved, "a.go")
}

func TestContextClosure_MultiSeedMergeTakesNearest(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "context_closure", map[string]any{
		"symbols":    "a.go::A,b.go::B",
		"edge_kinds": "calls",
	}))

	// C is 1 hop from B, 2 from A -> nearest (1) wins.
	dC, okC := memberDistance(t, out, "c.go::C")
	require.True(t, okC)
	assert.Equal(t, 1, dC)

	seeds, ok := out["seeds"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"a.go::A", "b.go::B"}, seeds)
}

func TestContextClosure_BudgetCapTruncates(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	out := extractTextResult(t, callTool(t, srv, "context_closure", map[string]any{
		"symbols":    "a.go::A",
		"edge_kinds": "calls",
		"max_nodes":  float64(2),
	}))

	mc := int(out["member_count"].(float64))
	assert.LessOrEqual(t, mc, 2)
	assert.True(t, out["truncated"].(bool))
}

func TestContextClosure_NoSeedsIsError(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	res := callTool(t, srv, "context_closure", map[string]any{
		"symbols": "does/not::Exist",
	})
	assert.True(t, res.IsError, "a closure with no resolvable seeds must be an error")
}

func TestContextClosure_AbsoluteFileSeedNormalizes(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedClosureGraph(t, srv)

	// An absolute path that is not under any tracked repo passes through
	// repoRelative unchanged and resolves nothing -> reported as missing,
	// not crashing. (a.go lives only in the injected graph, not on disk.)
	abs := filepath.Join(t.TempDir(), "nope.go")
	res := callTool(t, srv, "context_closure", map[string]any{
		"files":   abs,
		"symbols": "a.go::A",
	})
	out := extractTextResult(t, res)
	missing, ok := out["missing_files"].([]any)
	require.True(t, ok, "missing_files must list the unresolved seed file")
	assert.Contains(t, missing, abs)
}
