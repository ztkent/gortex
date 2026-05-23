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
	t.Run("GetFileNodes", func(t *testing.T) { testGetFileNodes(t, factory) })
	t.Run("GetRepoNodes", func(t *testing.T) { testGetRepoNodes(t, factory) })
	t.Run("GetNodeByQualName", func(t *testing.T) { testGetNodeByQualName(t, factory) })
	t.Run("Stats", func(t *testing.T) { testStats(t, factory) })
	t.Run("RepoStats", func(t *testing.T) { testRepoStats(t, factory) })
	t.Run("RepoPrefixes", func(t *testing.T) { testRepoPrefixes(t, factory) })
	t.Run("SetEdgeProvenance", func(t *testing.T) { testSetEdgeProvenance(t, factory) })
	t.Run("ReindexEdge", func(t *testing.T) { testReindexEdge(t, factory) })
	t.Run("Concurrency", func(t *testing.T) { testConcurrency(t, factory) })
	t.Run("EdgeIdentityRevisions", func(t *testing.T) { testEdgeIdentityRevisions(t, factory) })
	t.Run("VerifyEdgeIdentities", func(t *testing.T) { testVerifyEdgeIdentities(t, factory) })
	t.Run("RepoMemoryEstimate", func(t *testing.T) { testRepoMemoryEstimate(t, factory) })
	t.Run("AllRepoMemoryEstimates", func(t *testing.T) { testAllRepoMemoryEstimates(t, factory) })
	t.Run("MetaPreserved", func(t *testing.T) { testMetaPreserved(t, factory) })
	t.Run("EmptyStore", func(t *testing.T) { testEmptyStore(t, factory) })
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
