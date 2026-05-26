// Package storetest provides a conformance test suite that every
// graph.Store implementation MUST pass. Each backend (in-memory,
// bbolt-on-disk, SQLite-on-disk, remote-network-client) has a thin
// _test.go that calls RunConformance(t, factory) and inherits the
// full battery.
//
// The contract this package encodes is the union of behaviour the
// rest of gortex depends on from *graph.Graph today. New Store
// implementations are expected to satisfy every test before they can
// be considered a drop-in replacement.
package storetest

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Factory constructs a fresh, empty Store. RunConformance calls it
// many times across subtests; each invocation must yield an
// independent store with no leakage from previous runs. Backends with
// on-disk state should use t.TempDir() internally to isolate.
type Factory func(t *testing.T) graph.Store

// RunConformance runs the full conformance suite against the Store
// produced by factory. Backends invoke it from a _test.go in their
// own package.
func RunConformance(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("AddGetNode", func(t *testing.T) { testAddGetNode(t, factory) })
	t.Run("AddGetEdge", func(t *testing.T) { testAddGetEdge(t, factory) })
	t.Run("AddNodeIdempotent", func(t *testing.T) { testAddNodeIdempotent(t, factory) })
	t.Run("AddEdgeIdempotent", func(t *testing.T) { testAddEdgeIdempotent(t, factory) })
	t.Run("AddEdgeLineDisambiguates", func(t *testing.T) { testAddEdgeLineDisambiguates(t, factory) })
	t.Run("AddBatch", func(t *testing.T) { testAddBatch(t, factory) })
	t.Run("RemoveEdge", func(t *testing.T) { testRemoveEdge(t, factory) })
	t.Run("EvictFile", func(t *testing.T) { testEvictFile(t, factory) })
	t.Run("EvictFile_NoNodes", func(t *testing.T) { testEvictFileNoNodes(t, factory) })
	t.Run("EvictRepo", func(t *testing.T) { testEvictRepo(t, factory) })
	t.Run("EvictRepo_NoNodes", func(t *testing.T) { testEvictRepoNoNodes(t, factory) })
	t.Run("NodeAndEdgeCount", func(t *testing.T) { testNodeAndEdgeCount(t, factory) })
	t.Run("AllNodesAndEdges", func(t *testing.T) { testAllNodesAndEdges(t, factory) })
	t.Run("FindNodesByName", func(t *testing.T) { testFindNodesByName(t, factory) })
	t.Run("FindNodesByNameInRepo", func(t *testing.T) { testFindNodesByNameInRepo(t, factory) })
	t.Run("FindNodesByNameContaining", func(t *testing.T) { testFindNodesByNameContaining(t, factory) })
	t.Run("GetFileNodes", func(t *testing.T) { testGetFileNodes(t, factory) })
	t.Run("GetRepoNodes", func(t *testing.T) { testGetRepoNodes(t, factory) })
	t.Run("GetRepoEdges", func(t *testing.T) { testGetRepoEdges(t, factory) })
	t.Run("GetNodeByQualName", func(t *testing.T) { testGetNodeByQualName(t, factory) })
	t.Run("Stats", func(t *testing.T) { testStats(t, factory) })
	t.Run("RepoStats", func(t *testing.T) { testRepoStats(t, factory) })
	t.Run("RepoPrefixes", func(t *testing.T) { testRepoPrefixes(t, factory) })
	t.Run("SetEdgeProvenance", func(t *testing.T) { testSetEdgeProvenance(t, factory) })
	t.Run("SetEdgeProvenanceBatch", func(t *testing.T) { testSetEdgeProvenanceBatch(t, factory) })
	t.Run("ReindexEdge", func(t *testing.T) { testReindexEdge(t, factory) })
	t.Run("ReindexEdges", func(t *testing.T) { testReindexEdges(t, factory) })
	t.Run("Concurrency", func(t *testing.T) { testConcurrency(t, factory) })
	t.Run("EdgeIdentityRevisions", func(t *testing.T) { testEdgeIdentityRevisions(t, factory) })
	t.Run("VerifyEdgeIdentities", func(t *testing.T) { testVerifyEdgeIdentities(t, factory) })
	t.Run("RepoMemoryEstimate", func(t *testing.T) { testRepoMemoryEstimate(t, factory) })
	t.Run("AllRepoMemoryEstimates", func(t *testing.T) { testAllRepoMemoryEstimates(t, factory) })
	t.Run("MetaPreserved", func(t *testing.T) { testMetaPreserved(t, factory) })
	t.Run("EmptyStore", func(t *testing.T) { testEmptyStore(t, factory) })
	t.Run("EdgesByKind", func(t *testing.T) { testEdgesByKind(t, factory) })
	t.Run("NodesByKind", func(t *testing.T) { testNodesByKind(t, factory) })
	t.Run("EdgesWithUnresolvedTarget", func(t *testing.T) { testEdgesWithUnresolvedTarget(t, factory) })
	t.Run("GetNodesByIDs", func(t *testing.T) { testGetNodesByIDs(t, factory) })
	t.Run("FindNodesByNames", func(t *testing.T) { testFindNodesByNames(t, factory) })
	t.Run("GetEdgesByNodeIDs", func(t *testing.T) { testGetEdgesByNodeIDs(t, factory) })
	t.Run("SymbolBundleSearcher", func(t *testing.T) { testSymbolBundleSearcher(t, factory) })
	t.Run("DeadCodeCandidator", func(t *testing.T) { testDeadCodeCandidator(t, factory) })
	t.Run("IfaceImplementsScanner", func(t *testing.T) { testIfaceImplementsScanner(t, factory) })
}

// -- fixture helpers ---------------------------------------------------

func mkNode(id, name, file string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{
		ID:        id,
		Kind:      kind,
		Name:      name,
		FilePath:  file,
		StartLine: 1,
		EndLine:   10,
		Language:  "go",
	}
}

func mkRepoNode(id, name, file, repo string, kind graph.NodeKind) *graph.Node {
	n := mkNode(id, name, file, kind)
	n.RepoPrefix = repo
	return n
}

func mkEdge(from, to string, kind graph.EdgeKind) *graph.Edge {
	return &graph.Edge{
		From: from, To: to, Kind: kind,
		FilePath: "test.go", Line: 1,
		Confidence: 1.0,
		Origin:     graph.OriginASTResolved,
	}
}

func sortNodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func sortEdgeKeys(edges []*graph.Edge) []string {
	keys := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			keys = append(keys, fmt.Sprintf("%s|%s|%s|%d", e.From, e.To, e.Kind, e.Line))
		}
	}
	sort.Strings(keys)
	return keys
}

// -- individual subtests ----------------------------------------------

func testAddGetNode(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	s.AddNode(n)
	got := s.GetNode("a.go::Foo")
	if got == nil {
		t.Fatalf("GetNode returned nil for inserted node")
	}
	if got.Name != "Foo" || got.FilePath != "a.go" || got.Kind != graph.KindFunction {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if s.GetNode("missing") != nil {
		t.Fatalf("GetNode should return nil for missing key")
	}
}

func testAddGetEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))

	out := s.GetOutEdges("a")
	if len(out) != 1 || out[0].To != "b" {
		t.Fatalf("GetOutEdges(a) = %+v, want one edge to b", out)
	}
	in := s.GetInEdges("b")
	if len(in) != 1 || in[0].From != "a" {
		t.Fatalf("GetInEdges(b) = %+v, want one edge from a", in)
	}
}

func testAddNodeIdempotent(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("dup", "Dup", "x.go", graph.KindFunction)
	s.AddNode(n)
	s.AddNode(n)
	s.AddNode(n)
	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount after 3x add = %d, want 1", s.NodeCount())
	}
}

func testAddEdgeIdempotent(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	s.AddEdge(e)
	s.AddEdge(e)
	s.AddEdge(e)
	if got := len(s.GetOutEdges("a")); got != 1 {
		t.Fatalf("OutEdges after 3x add = %d, want 1", got)
	}
}

func testAddEdgeLineDisambiguates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 5
	s.AddEdge(e1)
	s.AddEdge(e2)
	if got := len(s.GetOutEdges("a")); got != 2 {
		t.Fatalf("OutEdges with different lines = %d, want 2", got)
	}
}

func testAddBatch(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	nodes := []*graph.Node{
		mkNode("a", "A", "x.go", graph.KindFunction),
		mkNode("b", "B", "x.go", graph.KindFunction),
		mkNode("c", "C", "y.go", graph.KindType),
	}
	edges := []*graph.Edge{
		mkEdge("a", "b", graph.EdgeCalls),
		mkEdge("b", "c", graph.EdgeReferences),
	}
	s.AddBatch(nodes, edges)
	if s.NodeCount() != 3 {
		t.Fatalf("NodeCount after AddBatch = %d, want 3", s.NodeCount())
	}
	if s.EdgeCount() != 2 {
		t.Fatalf("EdgeCount after AddBatch = %d, want 2", s.EdgeCount())
	}
}

func testRemoveEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	s.AddEdge(e)
	if !s.RemoveEdge("a", "b", graph.EdgeCalls) {
		t.Fatalf("RemoveEdge returned false for existing edge")
	}
	if len(s.GetOutEdges("a")) != 0 {
		t.Fatalf("OutEdges after RemoveEdge = nonzero")
	}
	if len(s.GetInEdges("b")) != 0 {
		t.Fatalf("InEdges after RemoveEdge = nonzero")
	}
	// Removing non-existent should report false but not panic.
	if s.RemoveEdge("a", "b", graph.EdgeCalls) {
		t.Fatalf("RemoveEdge returned true for missing edge")
	}
}

func testEvictFile(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindFunction))
	s.AddEdge(mkEdge("a.go::Foo", "a.go::Bar", graph.EdgeCalls))
	s.AddEdge(mkEdge("a.go::Bar", "b.go::Baz", graph.EdgeCalls))

	nodesRemoved, edgesRemoved := s.EvictFile("a.go")
	if nodesRemoved != 2 {
		t.Fatalf("EvictFile nodesRemoved = %d, want 2", nodesRemoved)
	}
	if edgesRemoved == 0 {
		t.Fatalf("EvictFile edgesRemoved should be > 0")
	}
	if s.GetNode("a.go::Foo") != nil {
		t.Fatalf("evicted node still present")
	}
	if s.GetNode("b.go::Baz") == nil {
		t.Fatalf("unrelated node was evicted")
	}
}

func testEvictFileNoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n, e := s.EvictFile("nonexistent.go")
	if n != 0 || e != 0 {
		t.Fatalf("EvictFile on empty file returned (%d, %d), want (0, 0)", n, e)
	}
}

func testEvictRepo(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	s.AddEdge(mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls))

	nodesRemoved, edgesRemoved := s.EvictRepo("r1")
	if nodesRemoved != 2 {
		t.Fatalf("EvictRepo nodesRemoved = %d, want 2", nodesRemoved)
	}
	if edgesRemoved == 0 {
		t.Fatalf("EvictRepo edgesRemoved should be > 0")
	}
	if s.GetNode("r1/a.go::Foo") != nil {
		t.Fatalf("r1 node still present")
	}
	if s.GetNode("r2/x.go::Baz") == nil {
		t.Fatalf("r2 node was evicted")
	}
}

func testEvictRepoNoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n, e := s.EvictRepo("nonexistent-repo")
	if n != 0 || e != 0 {
		t.Fatalf("EvictRepo on missing repo returned (%d, %d), want (0, 0)", n, e)
	}
}

func testNodeAndEdgeCount(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	if s.NodeCount() != 0 || s.EdgeCount() != 0 {
		t.Fatalf("empty store reports nonzero counts")
	}
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	if s.NodeCount() != 2 {
		t.Fatalf("NodeCount = %d, want 2", s.NodeCount())
	}
	if s.EdgeCount() != 1 {
		t.Fatalf("EdgeCount = %d, want 1", s.EdgeCount())
	}
}

func testAllNodesAndEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "y.go", graph.KindType))
	s.AddEdge(mkEdge("a", "b", graph.EdgeReferences))

	gotN := sortNodeIDs(s.AllNodes())
	wantN := []string{"a", "b"}
	if fmt.Sprint(gotN) != fmt.Sprint(wantN) {
		t.Fatalf("AllNodes = %v, want %v", gotN, wantN)
	}
	gotE := sortEdgeKeys(s.AllEdges())
	if len(gotE) != 1 {
		t.Fatalf("AllEdges = %v, want one entry", gotE)
	}
}

func testFindNodesByName(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Foo", "Foo", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Bar", "Bar", "c.go", graph.KindFunction))
	got := sortNodeIDs(s.FindNodesByName("Foo"))
	want := []string{"a.go::Foo", "b.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByName(Foo) = %v, want %v", got, want)
	}
	if len(s.FindNodesByName("MissingName")) != 0 {
		t.Fatalf("FindNodesByName for missing name should be empty")
	}
}

func testFindNodesByNameInRepo(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/a.go::Foo", "Foo", "r2/a.go", "r2", graph.KindFunction))
	got := sortNodeIDs(s.FindNodesByNameInRepo("Foo", "r1"))
	want := []string{"r1/a.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameInRepo(Foo, r1) = %v, want %v", got, want)
	}
}

func testFindNodesByNameContaining(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Three "log"-containing names + one unrelated.
	s.AddNode(mkNode("a.go::Login", "Login", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::LoginHandler", "LoginHandler", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Logout", "Logout", "c.go", graph.KindFunction))
	s.AddNode(mkNode("d.go::Unrelated", "Unrelated", "d.go", graph.KindFunction))

	// Case-insensitive substring match should return exactly the 3
	// "log"-bearing nodes.
	got := sortNodeIDs(s.FindNodesByNameContaining("log", 10))
	want := []string{"a.go::Login", "b.go::LoginHandler", "c.go::Logout"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameContaining(log, 10) = %v, want %v", got, want)
	}

	// Mixed-case query — must still match (case-insensitive).
	gotUpper := sortNodeIDs(s.FindNodesByNameContaining("LOG", 10))
	if fmt.Sprint(gotUpper) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameContaining(LOG, 10) = %v, want %v", gotUpper, want)
	}

	// Limit is honoured. Asking for 2 must return at most 2.
	gotLimited := s.FindNodesByNameContaining("log", 2)
	if len(gotLimited) != 2 {
		t.Fatalf("FindNodesByNameContaining(log, 2) returned %d, want 2", len(gotLimited))
	}

	// Empty needle returns nothing — never the whole graph.
	if got := s.FindNodesByNameContaining("", 10); len(got) != 0 {
		t.Fatalf("FindNodesByNameContaining(\"\") returned %d, want 0", len(got))
	}

	// No match — empty slice.
	if got := s.FindNodesByNameContaining("nonexistent_substring_xyz", 10); len(got) != 0 {
		t.Fatalf("FindNodesByNameContaining(no-match) returned %d, want 0", len(got))
	}
}

func testGetFileNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindFunction))
	got := sortNodeIDs(s.GetFileNodes("a.go"))
	want := []string{"a.go::Bar", "a.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetFileNodes(a.go) = %v, want %v", got, want)
	}
}

func testGetRepoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	got := sortNodeIDs(s.GetRepoNodes("r1"))
	want := []string{"r1/a.go::Foo", "r1/b.go::Bar"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetRepoNodes(r1) = %v, want %v", got, want)
	}
}

// testGetRepoEdges asserts that GetRepoEdges returns every edge whose
// SOURCE node carries the requested RepoPrefix, regardless of where
// the target lives — same-repo intra edges, cross-repo edges (source
// in r1 → target in r2), AND unresolved::* targets all count. Edges
// whose source is in a different repo (or unscoped) MUST NOT appear.
// Empty prefix returns nil so callers don't accidentally fall through
// to a full-graph scan.
func testGetRepoEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// r1 has two nodes that originate outgoing edges; r2 has a target
	// node and one of its own source nodes.
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/y.go::Qux", "Qux", "r2/y.go", "r2", graph.KindFunction))

	// r1-intra (Foo → Bar) — same repo.
	s.AddEdge(mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls))
	// r1 → r2 cross-repo (Foo → Baz).
	s.AddEdge(mkEdge("r1/a.go::Foo", "r2/x.go::Baz", graph.EdgeCalls))
	// r1 → unresolved (Bar → unresolved::Missing) — counts because
	// source is in r1.
	s.AddEdge(mkEdge("r1/b.go::Bar", "unresolved::Missing", graph.EdgeCalls))
	// r2-intra (Qux → Baz) — MUST NOT appear in r1's slice.
	s.AddEdge(mkEdge("r2/y.go::Qux", "r2/x.go::Baz", graph.EdgeCalls))
	// r2 → r1 cross-repo (Qux → Foo) — MUST NOT appear in r1's slice
	// because the source is in r2.
	s.AddEdge(mkEdge("r2/y.go::Qux", "r1/a.go::Foo", graph.EdgeCalls))

	gotR1 := sortEdgeKeys(s.GetRepoEdges("r1"))
	wantR1 := sortEdgeKeys([]*graph.Edge{
		mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls),
		mkEdge("r1/a.go::Foo", "r2/x.go::Baz", graph.EdgeCalls),
		mkEdge("r1/b.go::Bar", "unresolved::Missing", graph.EdgeCalls),
	})
	if fmt.Sprint(gotR1) != fmt.Sprint(wantR1) {
		t.Fatalf("GetRepoEdges(r1) =\n  %v\nwant\n  %v", gotR1, wantR1)
	}

	gotR2 := sortEdgeKeys(s.GetRepoEdges("r2"))
	wantR2 := sortEdgeKeys([]*graph.Edge{
		mkEdge("r2/y.go::Qux", "r2/x.go::Baz", graph.EdgeCalls),
		mkEdge("r2/y.go::Qux", "r1/a.go::Foo", graph.EdgeCalls),
	})
	if fmt.Sprint(gotR2) != fmt.Sprint(wantR2) {
		t.Fatalf("GetRepoEdges(r2) =\n  %v\nwant\n  %v", gotR2, wantR2)
	}

	// Empty prefix MUST return nothing (use AllEdges for the global
	// view). Disk backends must not fall through to a full scan.
	if got := s.GetRepoEdges(""); len(got) != 0 {
		t.Fatalf("GetRepoEdges(\"\") = %d edges, want 0", len(got))
	}

	// Unknown prefix MUST return empty (no panic, no fallthrough).
	if got := s.GetRepoEdges("nope"); len(got) != 0 {
		t.Fatalf("GetRepoEdges(nope) = %d edges, want 0", len(got))
	}
}

func testGetNodeByQualName(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	n.QualName = "pkg.Foo"
	s.AddNode(n)
	got := s.GetNodeByQualName("pkg.Foo")
	if got == nil || got.ID != "a.go::Foo" {
		t.Fatalf("GetNodeByQualName(pkg.Foo) = %v, want a.go::Foo", got)
	}
	if s.GetNodeByQualName("missing.Qual") != nil {
		t.Fatalf("GetNodeByQualName missing should be nil")
	}
}

func testStats(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "y.go", graph.KindType))
	s.AddEdge(mkEdge("a", "b", graph.EdgeReferences))
	st := s.Stats()
	if st.TotalNodes != 2 || st.TotalEdges != 1 {
		t.Fatalf("Stats = %+v, want TotalNodes=2, TotalEdges=1", st)
	}
	if st.ByKind[string(graph.KindFunction)] != 1 || st.ByKind[string(graph.KindType)] != 1 {
		t.Fatalf("Stats.ByKind = %v, want one each", st.ByKind)
	}
}

func testRepoStats(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindType))
	st := s.RepoStats()
	if len(st) != 2 {
		t.Fatalf("RepoStats has %d entries, want 2", len(st))
	}
	if st["r1"].TotalNodes != 1 {
		t.Fatalf("RepoStats[r1].TotalNodes = %d, want 1", st["r1"].TotalNodes)
	}
}

func testRepoPrefixes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindType))
	got := s.RepoPrefixes()
	sort.Strings(got)
	want := []string{"r1", "r2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("RepoPrefixes = %v, want %v", got, want)
	}
}

func testSetEdgeProvenance(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	e.Origin = graph.OriginTextMatched
	s.AddEdge(e)

	bumped := s.SetEdgeProvenance(e, graph.OriginLSPResolved)
	if !bumped {
		t.Fatalf("SetEdgeProvenance returned false for real upgrade")
	}
	out := s.GetOutEdges("a")
	if len(out) != 1 || out[0].Origin != graph.OriginLSPResolved {
		t.Fatalf("Origin did not propagate: %+v", out)
	}
}

func testReindexEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Build a small graph with three out-edges from "a" pointing at
	// three different targets, then re-bind all three to a fourth
	// target in one batched call.
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "x.go", graph.KindFunction))
	s.AddNode(mkNode("d", "D", "x.go", graph.KindFunction))
	s.AddNode(mkNode("z", "Z", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "c", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "d", graph.EdgeCalls)
	e3.Line = 3
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	// Mutate each edge's To, then hand the batch over. After the
	// call, all three edges must show as in-edges of z; none of the
	// originals must remain.
	e1.To, e2.To, e3.To = "z", "z", "z"
	s.ReindexEdges([]graph.EdgeReindex{
		{Edge: e1, OldTo: "b"},
		{Edge: e2, OldTo: "c"},
		{Edge: e3, OldTo: "d"},
	})

	for _, oldID := range []string{"b", "c", "d"} {
		if got := len(s.GetInEdges(oldID)); got != 0 {
			t.Fatalf("GetInEdges(%q) after batch reindex = %d, want 0", oldID, got)
		}
	}
	if got := len(s.GetInEdges("z")); got != 3 {
		t.Fatalf("GetInEdges(z) after batch reindex = %d, want 3", got)
	}
	if got := len(s.GetOutEdges("a")); got != 3 {
		t.Fatalf("GetOutEdges(a) after batch reindex = %d, want 3", got)
	}

	// Empty batch is a no-op.
	s.ReindexEdges(nil)
	s.ReindexEdges([]graph.EdgeReindex{})
}

func testSetEdgeProvenanceBatch(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e1.Origin = graph.OriginTextMatched
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 2
	e2.Origin = graph.OriginTextMatched
	e3 := mkEdge("a", "b", graph.EdgeCalls)
	e3.Line = 3
	e3.Origin = graph.OriginLSPResolved // already at target tier — should be no-op
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	changed := s.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{
		{Edge: e1, NewOrigin: graph.OriginLSPResolved},
		{Edge: e2, NewOrigin: graph.OriginLSPResolved},
		{Edge: e3, NewOrigin: graph.OriginLSPResolved},
	})
	if changed != 2 {
		t.Fatalf("SetEdgeProvenanceBatch reported %d changed, want 2 (one was already at target tier)", changed)
	}
	// Verify both promotions landed in the persisted edges.
	out := s.GetOutEdges("a")
	if len(out) != 3 {
		t.Fatalf("GetOutEdges(a) = %d, want 3", len(out))
	}
	for _, e := range out {
		if e.Origin != graph.OriginLSPResolved {
			t.Fatalf("edge %s->%s Origin = %q, want lsp_resolved", e.From, e.To, e.Origin)
		}
	}

	// Empty batch is a no-op and returns 0.
	if got := s.SetEdgeProvenanceBatch(nil); got != 0 {
		t.Fatalf("empty batch returned %d, want 0", got)
	}
	if got := s.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{}); got != 0 {
		t.Fatalf("empty batch returned %d, want 0", got)
	}
}

func testReindexEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("old", "Old", "x.go", graph.KindFunction))
	s.AddNode(mkNode("new", "New", "x.go", graph.KindFunction))
	e := mkEdge("a", "old", graph.EdgeCalls)
	s.AddEdge(e)

	e.To = "new"
	s.ReindexEdge(e, "old")

	if got := len(s.GetInEdges("old")); got != 0 {
		t.Fatalf("InEdges(old) after reindex = %d, want 0", got)
	}
	in := s.GetInEdges("new")
	if len(in) != 1 || in[0].From != "a" {
		t.Fatalf("InEdges(new) = %+v, want one edge from a", in)
	}
}

func testConcurrency(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				id := fmt.Sprintf("w%d/n%d", w, i)
				s.AddNode(mkNode(id, fmt.Sprintf("N%d", i), fmt.Sprintf("f%d.go", w), graph.KindFunction))
			}
		}(w)
	}
	wg.Wait()
	if got, want := s.NodeCount(), workers*perWorker; got != want {
		t.Fatalf("concurrent NodeCount = %d, want %d", got, want)
	}
}

func testEdgeIdentityRevisions(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Just ensure the method exists and returns a non-negative int.
	// The semantic invariant ("bumps on origin change") is
	// implementation-defined; backends may return 0 if they don't
	// track this.
	if got := s.EdgeIdentityRevisions(); got < 0 {
		t.Fatalf("EdgeIdentityRevisions negative: %d", got)
	}
}

func testVerifyEdgeIdentities(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	if err := s.VerifyEdgeIdentities(); err != nil {
		t.Fatalf("VerifyEdgeIdentities on consistent store: %v", err)
	}
}

func testRepoMemoryEstimate(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	// Backends may return zero (disk/remote) or a real estimate
	// (in-memory). The contract is that the call succeeds and
	// NodeCount matches what we inserted.
	est := s.RepoMemoryEstimate("r1")
	if est.NodeCount != 1 {
		t.Fatalf("RepoMemoryEstimate NodeCount = %d, want 1", est.NodeCount)
	}
}

func testAllRepoMemoryEstimates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	all := s.AllRepoMemoryEstimates()
	if len(all) != 2 {
		t.Fatalf("AllRepoMemoryEstimates len = %d, want 2", len(all))
	}
}

func testMetaPreserved(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	n.Meta = map[string]any{
		"signature":  "func Foo(x int) error",
		"visibility": "public",
	}
	s.AddNode(n)
	got := s.GetNode("a.go::Foo")
	if got == nil {
		t.Fatalf("GetNode returned nil")
	}
	if got.Meta == nil {
		t.Fatalf("Meta not preserved")
	}
	if got.Meta["signature"] != "func Foo(x int) error" {
		t.Fatalf("Meta[signature] = %v", got.Meta["signature"])
	}
	if got.Meta["visibility"] != "public" {
		t.Fatalf("Meta[visibility] = %v", got.Meta["visibility"])
	}
}

func testEdgesByKind(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindType))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "c", graph.EdgeReferences)
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	var calls []*graph.Edge
	for e := range s.EdgesByKind(graph.EdgeCalls) {
		calls = append(calls, e)
	}
	if len(calls) != 2 {
		t.Fatalf("EdgesByKind(EdgeCalls) yielded %d, want 2", len(calls))
	}
	for _, e := range calls {
		if e.Kind != graph.EdgeCalls {
			t.Fatalf("yielded edge has wrong kind: %s", e.Kind)
		}
	}

	var refs []*graph.Edge
	for e := range s.EdgesByKind(graph.EdgeReferences) {
		refs = append(refs, e)
	}
	if len(refs) != 1 {
		t.Fatalf("EdgesByKind(EdgeReferences) yielded %d, want 1", len(refs))
	}

	// Unknown kind yields nothing.
	count := 0
	for range s.EdgesByKind(graph.EdgeKind("nonexistent")) {
		count++
	}
	if count != 0 {
		t.Fatalf("EdgesByKind(nonexistent) yielded %d, want 0", count)
	}

	// Early stop honours the contract.
	stopped := 0
	for range s.EdgesByKind(graph.EdgeCalls) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

func testNodesByKind(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindType))

	var fns []*graph.Node
	for n := range s.NodesByKind(graph.KindFunction) {
		fns = append(fns, n)
	}
	if len(fns) != 2 {
		t.Fatalf("NodesByKind(KindFunction) yielded %d, want 2", len(fns))
	}
	for _, n := range fns {
		if n.Kind != graph.KindFunction {
			t.Fatalf("yielded node has wrong kind: %s", n.Kind)
		}
	}

	var types []*graph.Node
	for n := range s.NodesByKind(graph.KindType) {
		types = append(types, n)
	}
	if len(types) != 1 {
		t.Fatalf("NodesByKind(KindType) yielded %d, want 1", len(types))
	}

	// Early stop honours the contract.
	stopped := 0
	for range s.NodesByKind(graph.KindFunction) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

func testEdgesWithUnresolvedTarget(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "unresolved::Foo", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "unresolved::Bar", graph.EdgeCalls)
	e3.Line = 3
	e4 := mkEdge("a", "resolved", graph.EdgeCalls)
	e4.Line = 4
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)

	var unres []*graph.Edge
	for e := range s.EdgesWithUnresolvedTarget() {
		unres = append(unres, e)
	}
	if len(unres) != 2 {
		t.Fatalf("EdgesWithUnresolvedTarget yielded %d, want 2", len(unres))
	}
	for _, e := range unres {
		if !strings.HasPrefix(e.To, "unresolved::") {
			t.Fatalf("yielded edge has non-unresolved To: %s", e.To)
		}
	}
}

func testEmptyStore(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	if s.NodeCount() != 0 {
		t.Fatalf("empty NodeCount = %d, want 0", s.NodeCount())
	}
	if s.EdgeCount() != 0 {
		t.Fatalf("empty EdgeCount = %d, want 0", s.EdgeCount())
	}
	if len(s.AllNodes()) != 0 {
		t.Fatalf("empty AllNodes nonzero")
	}
	if len(s.AllEdges()) != 0 {
		t.Fatalf("empty AllEdges nonzero")
	}
	if len(s.RepoPrefixes()) != 0 {
		t.Fatalf("empty RepoPrefixes nonzero")
	}
}

func testGetNodesByIDs(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindType))

	got := s.GetNodesByIDs([]string{"a.go::Foo", "b.go::Baz", "missing", "a.go::Bar", "a.go::Foo"})
	if len(got) != 3 {
		t.Fatalf("GetNodesByIDs len = %d, want 3 (3 present, 1 missing, 1 duplicate)", len(got))
	}
	if got["a.go::Foo"] == nil || got["a.go::Foo"].Name != "Foo" {
		t.Fatalf("missing or wrong Foo: %v", got["a.go::Foo"])
	}
	if got["b.go::Baz"] == nil || got["b.go::Baz"].Kind != graph.KindType {
		t.Fatalf("missing or wrong Baz: %v", got["b.go::Baz"])
	}
	if _, present := got["missing"]; present {
		t.Fatalf("missing ID should not be in map, got %v", got["missing"])
	}

	// Empty / nil input is a no-op.
	if got := s.GetNodesByIDs(nil); len(got) != 0 {
		t.Fatalf("nil input returned %d entries", len(got))
	}
	if got := s.GetNodesByIDs([]string{}); len(got) != 0 {
		t.Fatalf("empty input returned %d entries", len(got))
	}
	if got := s.GetNodesByIDs([]string{""}); len(got) != 0 {
		t.Fatalf("empty-string ID returned %d entries", len(got))
	}
}

func testFindNodesByNames(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Foo", "Foo", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Bar", "Bar", "c.go", graph.KindFunction))

	got := s.FindNodesByNames([]string{"Foo", "Missing", "Bar", "Foo"})
	if len(got) != 2 {
		t.Fatalf("FindNodesByNames len = %d, want 2 (2 present, 1 missing, 1 duplicate)", len(got))
	}
	foos := got["Foo"]
	if len(foos) != 2 {
		t.Fatalf("Foo matches = %d, want 2", len(foos))
	}
	for _, n := range foos {
		if n.Name != "Foo" {
			t.Fatalf("matched node has wrong Name: %s", n.Name)
		}
	}
	bars := got["Bar"]
	if len(bars) != 1 || bars[0].Name != "Bar" {
		t.Fatalf("Bar matches wrong: %v", bars)
	}
	if _, present := got["Missing"]; present {
		t.Fatalf("missing name should not be in map")
	}

	// Empty / nil input.
	if got := s.FindNodesByNames(nil); len(got) != 0 {
		t.Fatalf("nil input returned %d entries", len(got))
	}
	if got := s.FindNodesByNames([]string{}); len(got) != 0 {
		t.Fatalf("empty input returned %d entries", len(got))
	}
}

// testGetEdgesByNodeIDs covers the batched fan-in / fan-out edge
// lookups. Builds a small graph with mixed fan-in/out, calls both
// methods with a mix of present and missing ids (plus an empty
// string), and asserts the per-id slices match what GetInEdges /
// GetOutEdges would return individually.
func testGetEdgesByNodeIDs(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Nodes
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindFunction))
	s.AddNode(mkNode("d", "D", "y.go", graph.KindFunction))
	// Edges: a→b, a→c, b→c, d→c (so c has 3 in-edges, a has 2 out-edges).
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	s.AddEdge(mkEdge("a", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("b", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("d", "c", graph.EdgeReferences))

	// --- GetOutEdgesByNodeIDs ---
	outMap := s.GetOutEdgesByNodeIDs([]string{"a", "b", "d", "missing", "a"})
	// a has 2 out-edges (a→b, a→c).
	if got := sortEdgeKeys(outMap["a"]); len(got) != 2 {
		t.Fatalf("GetOutEdgesByNodeIDs[a] = %v, want 2 edges", got)
	}
	// b has 1 out-edge (b→c).
	if got := outMap["b"]; len(got) != 1 || got[0].To != "c" {
		t.Fatalf("GetOutEdgesByNodeIDs[b] = %v, want one edge to c", got)
	}
	// d has 1 out-edge (d→c).
	if got := outMap["d"]; len(got) != 1 || got[0].To != "c" {
		t.Fatalf("GetOutEdgesByNodeIDs[d] = %v, want one edge to c", got)
	}
	// missing key — range over nil is a no-op, so callers can index
	// without an ok-check.
	if got := outMap["missing"]; len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs[missing] = %v, want empty", got)
	}

	// --- GetInEdgesByNodeIDs ---
	inMap := s.GetInEdgesByNodeIDs([]string{"a", "b", "c", "missing"})
	// a has 0 in-edges.
	if got := inMap["a"]; len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs[a] = %v, want empty", got)
	}
	// b has 1 in-edge (a→b).
	if got := inMap["b"]; len(got) != 1 || got[0].From != "a" {
		t.Fatalf("GetInEdgesByNodeIDs[b] = %v, want one edge from a", got)
	}
	// c has 3 in-edges (a→c, b→c, d→c).
	if got := inMap["c"]; len(got) != 3 {
		t.Fatalf("GetInEdgesByNodeIDs[c] = %v, want 3 edges", got)
	}
	froms := map[string]bool{}
	for _, e := range inMap["c"] {
		froms[e.From] = true
	}
	for _, want := range []string{"a", "b", "d"} {
		if !froms[want] {
			t.Fatalf("GetInEdgesByNodeIDs[c] missing edge from %q", want)
		}
	}
	if got := inMap["missing"]; len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs[missing] = %v, want empty", got)
	}

	// Empty / nil / empty-string inputs are no-ops.
	if got := s.GetOutEdgesByNodeIDs(nil); len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs(nil) returned %d entries", len(got))
	}
	if got := s.GetInEdgesByNodeIDs(nil); len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs(nil) returned %d entries", len(got))
	}
	if got := s.GetOutEdgesByNodeIDs([]string{}); len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs([]) returned %d entries", len(got))
	}
	if got := s.GetInEdgesByNodeIDs([]string{""}); len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs([\"\"]) returned %d entries", len(got))
	}
}

// testSymbolBundleSearcher exercises the optional
// graph.SymbolBundleSearcher capability. The interface is opt-in
// (today only the Ladybug backend implements it; the in-memory
// *Graph deliberately leaves it unimplemented so the engine's
// fallback path stays exercised) — backends without the capability
// skip the subtest cleanly.
//
// Coverage:
//   - SymbolSearcher.BulkUpsertSymbolFTS + BuildSymbolIndex must be
//     called first so the FTS index is populated.
//   - SearchSymbolBundles returns a bundle per matched id with the
//     correct in/out edges attached.
//   - Empty / no-match query returns an empty bundle slice.
func testSymbolBundleSearcher(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	bs, ok := s.(graph.SymbolBundleSearcher)
	if !ok {
		t.Skip("backend does not implement graph.SymbolBundleSearcher")
	}
	ss, ok := s.(graph.SymbolSearcher)
	if !ok {
		t.Skip("backend implements SymbolBundleSearcher but not SymbolSearcher — cannot populate FTS")
	}

	// Build a small graph: A → B → C, plus an unrelated isolated D.
	// FTS-searchable name tokens that should land on the same hit.
	s.AddNode(mkNode("a", "AlphaWidget", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "BetaWidget", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "GammaWidget", "y.go", graph.KindFunction))
	s.AddNode(mkNode("d", "Delta", "y.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	s.AddEdge(mkEdge("b", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("a", "c", graph.EdgeCalls))

	// Populate the FTS sidecar — every searchable node carries its
	// tokenised name as the FTS text.
	items := []graph.SymbolFTSItem{
		{NodeID: "a", Tokens: "alpha widget"},
		{NodeID: "b", Tokens: "beta widget"},
		{NodeID: "c", Tokens: "gamma widget"},
		{NodeID: "d", Tokens: "delta"},
	}
	if err := ss.BulkUpsertSymbolFTS(items); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS: %v", err)
	}
	if err := ss.BuildSymbolIndex(); err != nil {
		t.Fatalf("BuildSymbolIndex: %v", err)
	}

	// Querying for "widget" should match a/b/c and not d. Each bundle
	// must carry the correct in/out edges off the graph.
	bundles, err := bs.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolBundles: %v", err)
	}
	if len(bundles) == 0 {
		t.Fatalf("SearchSymbolBundles returned no bundles — expected matches for a/b/c")
	}
	gotIDs := make(map[string]graph.SymbolBundle, len(bundles))
	for _, b := range bundles {
		if b.Node == nil {
			t.Fatalf("bundle has nil node: %+v", b)
		}
		gotIDs[b.Node.ID] = b
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := gotIDs[want]; !ok {
			t.Fatalf("missing bundle for id %q; got ids=%v", want, idsOf(bundles))
		}
	}
	if _, ok := gotIDs["d"]; ok {
		t.Fatalf("unexpected bundle for id %q (no 'widget' token in its FTS row)", "d")
	}

	// Edge verification: per-bundle in/out edges must match the
	// in-memory truth surfaced via the existing GetIn/Out edges.
	for id, b := range gotIDs {
		wantOut := s.GetOutEdges(id)
		if !edgeSlicesMatch(wantOut, b.OutEdges) {
			t.Fatalf("bundle[%s].OutEdges mismatch: want=%v got=%v", id, edgeKeys(wantOut), edgeKeys(b.OutEdges))
		}
		wantIn := s.GetInEdges(id)
		if !edgeSlicesMatch(wantIn, b.InEdges) {
			t.Fatalf("bundle[%s].InEdges mismatch: want=%v got=%v", id, edgeKeys(wantIn), edgeKeys(b.InEdges))
		}
	}

	// Empty query is a clean no-op.
	if empty, err := bs.SearchSymbolBundles("", 10); err != nil || len(empty) != 0 {
		t.Fatalf("SearchSymbolBundles(\"\"): err=%v len=%d, want empty", err, len(empty))
	}
	// No-match query — backend MAY return nil or empty slice; both
	// are valid.
	if no, err := bs.SearchSymbolBundles("nomatchforanything", 10); err != nil {
		t.Fatalf("SearchSymbolBundles(nomatch): err=%v", err)
	} else if len(no) != 0 {
		t.Fatalf("SearchSymbolBundles(nomatch) returned %d bundles, want 0", len(no))
	}
}

// idsOf is a small helper for the bundle assertions above.
func idsOf(bs []graph.SymbolBundle) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Node != nil {
			out = append(out, b.Node.ID)
		}
	}
	sort.Strings(out)
	return out
}

// edgeSlicesMatch reports whether two edge slices contain the same
// (from, to, kind) tuples regardless of order. Used by the bundle
// assertions to ignore back-end-imposed ordering differences.
func edgeSlicesMatch(want, got []*graph.Edge) bool {
	if len(want) != len(got) {
		return false
	}
	wantKeys := edgeKeys(want)
	gotKeys := edgeKeys(got)
	sort.Strings(wantKeys)
	sort.Strings(gotKeys)
	for i := range wantKeys {
		if wantKeys[i] != gotKeys[i] {
			return false
		}
	}
	return true
}

// edgeKeys flattens a slice of edges into deterministic (from→to:kind)
// strings for ordered diffing.
func edgeKeys(es []*graph.Edge) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		if e == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s->%s:%s", e.From, e.To, e.Kind))
	}
	return out
}

// testDeadCodeCandidator exercises the optional
// graph.DeadCodeCandidator capability. Builds a small graph with
// nodes that fall into each filter case the analyzer cares about:
//
//   - zero in-edges (dead).
//   - in-edges of disallowed kind only (dead).
//   - in-edges of allowed kind (alive).
//   - mixed kinds across the candidate set (per-row allowlist must apply).
//
// The in-memory *graph.Graph implements this; Ladybug overrides with
// a server-side Cypher query. Both must return the same candidate set.
func testDeadCodeCandidator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	dc, ok := s.(graph.DeadCodeCandidator)
	if !ok {
		t.Skip("backend does not implement graph.DeadCodeCandidator")
	}

	// Functions: AliveFunc (called), DeadFunc (no in-edges),
	// ReadOnlyFunc (only EdgeReads — disallowed for KindFunction).
	s.AddNode(mkNode("AliveFunc", "AliveFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("DeadFunc", "DeadFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("ReadOnlyFunc", "ReadOnlyFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Caller", "Caller", "a.go", graph.KindFunction))
	// Types: AliveType (referenced), DeadType (no in-edges).
	s.AddNode(mkNode("AliveType", "AliveType", "b.go", graph.KindType))
	s.AddNode(mkNode("DeadType", "DeadType", "b.go", graph.KindType))
	// Methods: AliveMethod (called), DeadMethod (no in-edges).
	s.AddNode(mkNode("AliveMethod", "AliveMethod", "c.go", graph.KindMethod))
	s.AddNode(mkNode("DeadMethod", "DeadMethod", "c.go", graph.KindMethod))

	// Edges that exercise the per-kind allowlist.
	e1 := mkEdge("Caller", "AliveFunc", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("Caller", "ReadOnlyFunc", graph.EdgeReads)
	e2.Line = 2
	e3 := mkEdge("Caller", "AliveMethod", graph.EdgeCalls)
	e3.Line = 3
	e4 := mkEdge("Caller", "AliveType", graph.EdgeReferences)
	e4.Line = 4
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)

	// Per-kind allowlist mirrors analysis.incomingUsageKinds for the
	// three kinds under test. Functions are alive on Calls/References;
	// methods on Calls/Implements; types on References/Instantiates.
	allowedKinds := []graph.NodeKind{
		graph.KindFunction,
		graph.KindMethod,
		graph.KindType,
	}
	allowedInEdges := map[graph.NodeKind][]graph.EdgeKind{
		graph.KindFunction: {graph.EdgeCalls, graph.EdgeReferences},
		graph.KindMethod:   {graph.EdgeCalls, graph.EdgeImplements},
		graph.KindType:     {graph.EdgeReferences, graph.EdgeInstantiates},
	}

	got := dc.DeadCodeCandidates(allowedKinds, allowedInEdges)
	gotIDs := sortNodeIDs(got)
	// Caller has zero in-edges of any kind, so it surfaces too — the
	// analyzer's per-kind allowlist would also flag it as a candidate
	// here. The backend's job is just the candidate set; post-filters
	// (exported / test / entry-point) run in Go.
	want := []string{"Caller", "DeadFunc", "DeadMethod", "DeadType", "ReadOnlyFunc"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("DeadCodeCandidates = %v\nwant %v", gotIDs, want)
	}

	// Empty kind list returns nothing — never the whole graph.
	if got := dc.DeadCodeCandidates(nil, allowedInEdges); len(got) != 0 {
		t.Fatalf("DeadCodeCandidates(nil) = %d, want 0", len(got))
	}

	// Empty per-kind allowlist means "any incoming edge counts as
	// usage" — AliveFunc and ReadOnlyFunc (both have *some* in-edge)
	// drop out; only DeadFunc + Caller remain among functions.
	anyKind := map[graph.NodeKind][]graph.EdgeKind{
		graph.KindFunction: nil,
	}
	gotAny := dc.DeadCodeCandidates([]graph.NodeKind{graph.KindFunction}, anyKind)
	gotAnyIDs := sortNodeIDs(gotAny)
	wantAny := []string{"Caller", "DeadFunc"}
	if fmt.Sprint(gotAnyIDs) != fmt.Sprint(wantAny) {
		t.Fatalf("DeadCodeCandidates(any-kind) = %v\nwant %v", gotAnyIDs, wantAny)
	}
}

// testIfaceImplementsScanner exercises the optional
// graph.IfaceImplementsScanner capability. Seeds two interfaces (one
// with methods Meta, one without) plus a type that implements each;
// the row set must include only the (type, iface) tuple whose target
// has a Meta["methods"] payload — the no-meta interface drops out.
func testIfaceImplementsScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scanner, ok := s.(graph.IfaceImplementsScanner)
	if !ok {
		t.Skip("backend does not implement graph.IfaceImplementsScanner")
	}

	// Interface with required methods.
	ifaceA := mkNode("iface_A", "Reader", "a.go", graph.KindInterface)
	ifaceA.Meta = map[string]any{"methods": []string{"Read", "Close"}}
	s.AddNode(ifaceA)
	// Interface with no Meta — must not appear in the row set.
	ifaceB := mkNode("iface_B", "Empty", "a.go", graph.KindInterface)
	s.AddNode(ifaceB)
	// Implementing type for each.
	s.AddNode(mkNode("type_A", "ReaderImpl", "a.go", graph.KindType))
	s.AddNode(mkNode("type_B", "EmptyImpl", "a.go", graph.KindType))
	s.AddEdge(mkEdge("type_A", "iface_A", graph.EdgeImplements))
	s.AddEdge(mkEdge("type_B", "iface_B", graph.EdgeImplements))

	rows := scanner.IfaceImplementsRows()
	if len(rows) != 1 {
		t.Fatalf("IfaceImplementsRows len = %d, want 1 (iface_B has no Meta)", len(rows))
	}
	r := rows[0]
	if r.TypeID != "type_A" || r.IfaceID != "iface_A" {
		t.Fatalf("row = %+v, want type_A → iface_A", r)
	}
	if r.IfaceMeta == nil {
		t.Fatalf("IfaceMeta is nil")
	}
	raw, ok := r.IfaceMeta["methods"]
	if !ok {
		t.Fatalf("IfaceMeta missing methods key: %+v", r.IfaceMeta)
	}
	// Meta encoding round-trips lists differently between backends
	// (in-memory keeps []string; gob-encoded comes back as []any).
	// Accept either.
	var methods []string
	switch v := raw.(type) {
	case []string:
		methods = v
	case []any:
		for _, m := range v {
			if str, ok := m.(string); ok {
				methods = append(methods, str)
			}
		}
	default:
		t.Fatalf("unexpected methods type %T: %v", raw, raw)
	}
	sort.Strings(methods)
	if fmt.Sprint(methods) != fmt.Sprint([]string{"Close", "Read"}) {
		t.Fatalf("methods = %v, want [Close Read]", methods)
	}
}
