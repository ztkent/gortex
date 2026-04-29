package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddNode_Idempotent proves the invariant the resilience work added
// to the graph: N duplicate AddNode calls converge to the same Stats()
// and the same secondary-index contents as a single call. Without this,
// a daemon restart that loads a snapshot and then re-runs IndexCtx on
// top of it (which doesn't evict first) produces N× the byFile /
// byName / byRepo slice entries — the B1 symptom.
func TestAddNode_Idempotent(t *testing.T) {
	g := New()
	n := &Node{
		ID:         "repo/a.go::Foo",
		Name:       "Foo",
		Kind:       KindFunction,
		FilePath:   "repo/a.go",
		QualName:   "pkg.Foo",
		RepoPrefix: "repo",
	}

	g.AddNode(n)
	base := g.Stats()
	require.Equal(t, 1, base.TotalNodes)

	for i := 0; i < 10; i++ {
		g.AddNode(n)
	}

	got := g.Stats()
	assert.Equal(t, base.TotalNodes, got.TotalNodes,
		"duplicate AddNode must not grow node count")

	byFile := g.GetFileNodes("repo/a.go")
	assert.Len(t, byFile, 1, "byFile must not duplicate")
	byName := g.FindNodesByName("Foo")
	assert.Len(t, byName, 1, "byName must not duplicate")
	byRepo := g.GetRepoNodes("repo")
	assert.Len(t, byRepo, 1, "byRepo must not duplicate")
	assert.Equal(t, n, g.GetNodeByQualName("pkg.Foo"))
}

// TestAddEdge_Idempotent is the edge counterpart of the node test. With
// the same (From, To, Kind, FilePath, Line), repeated AddEdge calls
// converge to a single adjacency-list entry. This is what made the
// "edges double on every daemon restart" symptom recede.
func TestAddEdge_Idempotent(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", Name: "A", Kind: KindFunction, FilePath: "a"})
	g.AddNode(&Node{ID: "b::B", Name: "B", Kind: KindFunction, FilePath: "b"})

	e := &Edge{From: "b::B", To: "a::A", Kind: EdgeCalls, FilePath: "b", Line: 7}
	for i := 0; i < 10; i++ {
		g.AddEdge(e)
	}

	assert.Equal(t, 1, g.EdgeCount(), "duplicate AddEdge must not grow edge count")
	assert.Len(t, g.GetOutEdges("b::B"), 1, "outEdges must have exactly one entry")
	assert.Len(t, g.GetInEdges("a::A"), 1, "inEdges must have exactly one entry")
}

// TestAddEdge_DifferentFromSameTo guards the edgeKey shape: two edges
// with different From but identical (To, Kind, FilePath, Line) must
// both survive, as distinct entries in the target's inEdges bucket.
// An earlier version of the sidecar omitted From from the key, which
// made two such edges collide at the inEdges[to] index — the second
// AddEdge overwrote the first and downstream BFS traversal lost one
// caller. Cross-repo impact analysis regressed until From landed in
// the key.
func TestAddEdge_DifferentFromSameTo(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "target::T", Name: "T", Kind: KindFunction, FilePath: "t"})
	g.AddNode(&Node{ID: "caller1::C1", Name: "C1", Kind: KindFunction, FilePath: "c1"})
	g.AddNode(&Node{ID: "caller2::C2", Name: "C2", Kind: KindFunction, FilePath: "c2"})

	// Both edges lack FilePath/Line — a common shape in tests that
	// construct synthetic graphs. Without From in the key they would
	// dedup to one inEdges entry.
	g.AddEdge(&Edge{From: "caller1::C1", To: "target::T", Kind: EdgeCalls})
	g.AddEdge(&Edge{From: "caller2::C2", To: "target::T", Kind: EdgeCalls})

	in := g.GetInEdges("target::T")
	assert.Len(t, in, 2, "two distinct callers must both appear in inEdges")
}

// TestAddEdge_LineDisambiguates proves that two call-sites from the
// same caller to the same callee at different lines are preserved —
// they're distinct edges, not duplicates. `foo(); foo();` in the same
// function must survive dedup.
func TestAddEdge_LineDisambiguates(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", Name: "A", Kind: KindFunction, FilePath: "a"})
	g.AddNode(&Node{ID: "b::B", Name: "B", Kind: KindFunction, FilePath: "b"})

	g.AddEdge(&Edge{From: "b::B", To: "a::A", Kind: EdgeCalls, FilePath: "b", Line: 7})
	g.AddEdge(&Edge{From: "b::B", To: "a::A", Kind: EdgeCalls, FilePath: "b", Line: 11})

	assert.Equal(t, 2, g.EdgeCount(), "different lines must produce distinct edges")
}

// TestAddNode_Replace verifies that re-adding a node with an updated
// Meta preserves the slice positions and replaces the pointer in place.
// This is the "same ID, new signature / new line" case that happens
// during IncrementalReindex after a file edit.
func TestAddNode_Replace(t *testing.T) {
	g := New()
	n1 := &Node{ID: "a::X", Name: "X", Kind: KindFunction, FilePath: "a",
		Meta: map[string]any{"signature": "X()"}}
	g.AddNode(n1)

	n2 := &Node{ID: "a::X", Name: "X", Kind: KindFunction, FilePath: "a",
		Meta: map[string]any{"signature": "X(arg int)"}}
	g.AddNode(n2)

	got := g.GetNode("a::X")
	require.NotNil(t, got)
	assert.Equal(t, "X(arg int)", got.Meta["signature"],
		"replacement must install new pointer")
	assert.Len(t, g.GetFileNodes("a"), 1, "byFile must not grow on replace")
	assert.Len(t, g.FindNodesByName("X"), 1, "byName must not grow on replace")
	// The slice entry must be the new pointer — readers iterate byFile
	// and rely on it reflecting the current node state.
	assert.Same(t, n2, g.GetFileNodes("a")[0])
}

// TestAddNode_MigrateBuckets verifies that when a replacement changes
// the node's FilePath / Name / RepoPrefix, the secondary-index entry
// moves from the old bucket to the new one. Without this, a rename
// (unusual but legal) would leave ghost entries in both buckets.
func TestAddNode_MigrateBuckets(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "x::X", Name: "OldName", Kind: KindFunction,
		FilePath: "old.go", RepoPrefix: "oldrepo", QualName: "pkg.Old"})
	g.AddNode(&Node{ID: "x::X", Name: "NewName", Kind: KindFunction,
		FilePath: "new.go", RepoPrefix: "newrepo", QualName: "pkg.New"})

	assert.Empty(t, g.GetFileNodes("old.go"), "old bucket must be emptied")
	assert.Len(t, g.GetFileNodes("new.go"), 1, "new bucket must have the entry")
	assert.Empty(t, g.FindNodesByName("OldName"))
	assert.Len(t, g.FindNodesByName("NewName"), 1)
	assert.Empty(t, g.GetRepoNodes("oldrepo"))
	assert.Len(t, g.GetRepoNodes("newrepo"), 1)
	assert.Nil(t, g.GetNodeByQualName("pkg.Old"))
	assert.NotNil(t, g.GetNodeByQualName("pkg.New"))
}

// TestAddNode_PreservesRepoPrefixOnEmptyDowngrade pins the warmup bug
// where some path re-AddNode'd existing repo-stamped nodes with
// RepoPrefix="" — clearing them out of byRepo[prefix] without touching
// the underlying nodes map. The user-visible symptom: per-repo queries
// (RepoStats / GetRepoNodes / RepoMemoryEstimate) returned empty for
// repos whose nodes were still present in the graph. Defensive fix:
// a non-empty prev RepoPrefix is sticky — the empty new value is
// promoted to prev's value rather than allowed to silently strip the
// node from its bucket.
func TestAddNode_PreservesRepoPrefixOnEmptyDowngrade(t *testing.T) {
	g := New()
	original := &Node{
		ID: "myrepo/file.go::Foo", Name: "Foo", Kind: KindFunction,
		FilePath: "myrepo/file.go", RepoPrefix: "myrepo",
	}
	g.AddNode(original)
	require.Len(t, g.GetRepoNodes("myrepo"), 1, "node must land in byRepo at first add")

	// Re-add with empty RepoPrefix (the buggy caller).
	g.AddNode(&Node{
		ID: "myrepo/file.go::Foo", Name: "Foo", Kind: KindFunction,
		FilePath: "myrepo/file.go",
		// RepoPrefix intentionally empty.
	})

	assert.Len(t, g.GetRepoNodes("myrepo"), 1,
		"byRepo[myrepo] must still contain the node after empty-prefix re-add")
	assert.NotNil(t, g.GetNode("myrepo/file.go::Foo"),
		"node itself must still exist")
	assert.Equal(t, "myrepo", g.GetNode("myrepo/file.go::Foo").RepoPrefix,
		"RepoPrefix on the stored node must be preserved")
}

// TestEvictFile_SwapWithLast exercises the sidecar-based swap-with-last
// removal path. Uses enough nodes per file that iteration order would
// surface a mis-tracked sidecar position. The assertion is simple: post
// eviction, the graph is empty of entries for that file.
func TestEvictFile_SwapWithLast(t *testing.T) {
	g := New()
	for i := 0; i < 100; i++ {
		g.AddNode(&Node{
			ID:       fmt.Sprintf("f.go::Sym%d", i),
			Name:     fmt.Sprintf("Sym%d", i),
			Kind:     KindFunction,
			FilePath: "f.go",
		})
	}
	assert.Len(t, g.GetFileNodes("f.go"), 100)

	n, _ := g.EvictFile("f.go")
	assert.Equal(t, 100, n)
	assert.Empty(t, g.GetFileNodes("f.go"))
	assert.Equal(t, 0, g.NodeCount())
}

// TestRestartStability simulates the daemon-restart cycle: snapshot
// into a fresh graph (via AddNode/AddEdge replay, which is what
// loadSnapshot does), and verify Stats() matches the original. Repeat
// many times to catch any state that drifts across restarts.
//
// Before the sidecar landed, Stats().TotalEdges doubled on every cycle;
// after, the invariant holds for arbitrary N.
func TestRestartStability(t *testing.T) {
	orig := buildRepresentativeGraph()
	want := orig.Stats()

	for cycle := 0; cycle < 5; cycle++ {
		replay := New()
		for _, n := range orig.AllNodes() {
			replay.AddNode(n)
		}
		for _, e := range orig.AllEdges() {
			replay.AddEdge(e)
		}

		// Simulate a second "IndexCtx on top" pass — this is what
		// the old warmup did after loadSnapshot. Without idempotent
		// writes, this pass doubles every edge.
		for _, n := range orig.AllNodes() {
			replay.AddNode(n)
		}
		for _, e := range orig.AllEdges() {
			replay.AddEdge(e)
		}

		got := replay.Stats()
		assert.Equal(t, want.TotalNodes, got.TotalNodes,
			"cycle %d: node count drifted", cycle)
		assert.Equal(t, want.TotalEdges, got.TotalEdges,
			"cycle %d: edge count drifted (B1 regression)", cycle)
	}
}

func buildRepresentativeGraph() *Graph {
	g := New()
	// Build a small call graph that stresses every secondary index:
	// multiple files, multiple names, multiple repos, calls + imports.
	files := []struct {
		path, repo string
	}{
		{"r1/a.go", "r1"},
		{"r1/b.go", "r1"},
		{"r2/c.go", "r2"},
	}
	for _, f := range files {
		for i := 0; i < 5; i++ {
			g.AddNode(&Node{
				ID:         fmt.Sprintf("%s::Fn%d", f.path, i),
				Name:       fmt.Sprintf("Fn%d", i),
				Kind:       KindFunction,
				FilePath:   f.path,
				RepoPrefix: f.repo,
			})
		}
	}
	// Add a few call edges between files.
	g.AddEdge(&Edge{From: "r1/a.go::Fn0", To: "r1/b.go::Fn1", Kind: EdgeCalls, FilePath: "r1/a.go", Line: 10})
	g.AddEdge(&Edge{From: "r1/a.go::Fn0", To: "r2/c.go::Fn2", Kind: EdgeCalls, FilePath: "r1/a.go", Line: 12})
	g.AddEdge(&Edge{From: "r1/b.go::Fn3", To: "r2/c.go::Fn4", Kind: EdgeCalls, FilePath: "r1/b.go", Line: 5})
	return g
}

// TestReindexEdge_UpdatesSidecar verifies ReindexEdge migrates the
// inEdges bucket + both sidecars when the resolver changes an edge's
// To field (unresolved::X → real::X). A bug here would show up as
// GetInEdges returning zero entries after resolve, or later AddEdge
// refusing to dedup because the key changed out from under the sidecar.
func TestReindexEdge_UpdatesSidecar(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "a::A", Name: "A", Kind: KindFunction, FilePath: "a"})
	g.AddNode(&Node{ID: "b::B", Name: "B", Kind: KindFunction, FilePath: "b"})
	g.AddNode(&Node{ID: "unresolved::real", Name: "real", Kind: KindFunction, FilePath: "u"})

	e := &Edge{From: "a::A", To: "unresolved::real", Kind: EdgeCalls, FilePath: "a", Line: 3}
	g.AddEdge(e)

	require.Len(t, g.GetInEdges("unresolved::real"), 1)
	require.Len(t, g.GetInEdges("b::B"), 0)

	// Resolver-style mutation.
	oldTo := e.To
	e.To = "b::B"
	g.ReindexEdge(e, oldTo)

	assert.Len(t, g.GetInEdges("unresolved::real"), 0,
		"old target bucket must be emptied")
	assert.Len(t, g.GetInEdges("b::B"), 1,
		"new target bucket must hold the edge")

	// Adding the same edge with its NEW identity must dedup via the
	// updated sidecar — if ReindexEdge forgot to rewrite the
	// outEdgeIdx key, this would append a duplicate.
	g.AddEdge(e)
	assert.Equal(t, 1, g.EdgeCount(), "AddEdge after ReindexEdge must still dedup")
}
