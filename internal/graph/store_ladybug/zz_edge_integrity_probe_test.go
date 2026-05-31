package store_ladybug_test

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

// openProbe opens a fresh on-disk store for the integrity probes.
func openProbe(t *testing.T) *store_ladybug.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store_ladybug.OpenWithOptions(filepath.Join(dir, "test.kuzu"),
		store_ladybug.Options{BufferPoolMB: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type wantEdge struct {
	from, to string
	kind     graph.EdgeKind
	file     string
	line     int
}

// TestEdgeFieldIntegrity_BulkAddBatch is the decisive ground-truth probe:
// it bulk-writes edges spanning multiple "repos" (distinct file_path
// prefixes), distinct edge kinds, and some carrying Meta, then reads them
// back and asserts every (from,to,kind,file_path,line) tuple round-trips
// EXACTLY. If kind/file_path get scrambled across edges this fails loudly.
func TestEdgeFieldIntegrity_BulkAddBatch(t *testing.T) {
	s := openProbe(t)

	// Three simulated repos, each with a caller that calls a callee.
	// We deliberately use different edge kinds and file_path prefixes
	// so a cross-edge scramble is detectable.
	type spec struct {
		repo string
		kind graph.EdgeKind
	}
	specs := []spec{
		{"gortex", graph.EdgeCalls},
		{"rate_checkers_detector", graph.EdgeReferences},
		{"gcx-ts", graph.EdgeReturns},
		{"web", graph.EdgeInstantiates},
		{"infra", graph.EdgeReads},
	}

	var nodes []*graph.Node
	var edges []*graph.Edge
	var want []wantEdge
	for i, sp := range specs {
		file := fmt.Sprintf("%s/internal/pkg/file%d.go", sp.repo, i)
		caller := fmt.Sprintf("%s::Caller%d", file, i)
		callee := fmt.Sprintf("%s::Callee%d", file, i)
		nodes = append(nodes,
			&graph.Node{ID: caller, Name: fmt.Sprintf("Caller%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: sp.repo},
			&graph.Node{ID: callee, Name: fmt.Sprintf("Callee%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: sp.repo},
		)
		line := 100 + i
		e := &graph.Edge{From: caller, To: callee, Kind: sp.kind, FilePath: file, Line: line}
		// Give a couple of edges Meta to exercise the base64 meta column.
		if i%2 == 0 {
			e.Meta = map[string]any{"semantic_source": "ast", "idx": i}
		}
		edges = append(edges, e)
		want = append(want, wantEdge{caller, callee, sp.kind, file, line})
	}

	s.AddBatch(nodes, edges)

	for _, w := range want {
		in := s.GetInEdges(w.to)
		if len(in) != 1 {
			t.Fatalf("GetInEdges(%s) = %d edges, want 1", w.to, len(in))
		}
		got := in[0]
		if got.From != w.from || got.To != w.to || got.Kind != w.kind || got.FilePath != w.file || got.Line != w.line {
			t.Errorf("edge to %s SCRAMBLED:\n  got  from=%s kind=%s file=%s line=%d\n  want from=%s kind=%s file=%s line=%d",
				w.to, got.From, got.Kind, got.FilePath, got.Line, w.from, w.kind, w.file, w.line)
		}
	}
}

// TestEdgeFieldIntegrity_ResolverApply exercises the resolver apply path
// (ReindexEdges -> reindexEdgesBulk): seed unresolved call edges, then
// rebind each To onto the real callee and assert the resolved edge keeps
// its original kind + file_path + line.
func TestEdgeFieldIntegrity_ResolverApply(t *testing.T) {
	s := openProbe(t)

	repos := []string{"gortex", "rate_checkers_detector", "gcx-ts", "web"}
	var nodes []*graph.Node
	var unresolved []*graph.Edge
	type resolvePlan struct {
		from, oldTo, newTo, file string
		kind                     graph.EdgeKind
		line                     int
	}
	var plans []resolvePlan
	for i, repo := range repos {
		file := fmt.Sprintf("%s/internal/pkg/r%d.go", repo, i)
		caller := fmt.Sprintf("%s::Fn%d", file, i)
		callee := fmt.Sprintf("%s::Target%d", file, i)
		stub := fmt.Sprintf("%s::unresolved::Target%d", repo, i)
		nodes = append(nodes,
			&graph.Node{ID: caller, Name: fmt.Sprintf("Fn%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo},
			&graph.Node{ID: callee, Name: fmt.Sprintf("Target%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo},
		)
		line := 200 + i
		unresolved = append(unresolved, &graph.Edge{From: caller, To: stub, Kind: graph.EdgeCalls, FilePath: file, Line: line})
		plans = append(plans, resolvePlan{caller, stub, callee, file, graph.EdgeCalls, line})
	}
	s.AddBatch(nodes, unresolved)

	// Build the reindex batch: each edge's To is rebound from stub to
	// the real callee. Kind/FilePath/Line are unchanged (a plain call
	// resolution), matching what Resolver.ResolveAll does.
	var batch []graph.EdgeReindex
	for _, p := range plans {
		batch = append(batch, graph.EdgeReindex{
			Edge:  &graph.Edge{From: p.from, To: p.newTo, Kind: p.kind, FilePath: p.file, Line: p.line},
			OldTo: p.oldTo,
		})
	}
	s.ReindexEdges(batch)

	for _, p := range plans {
		in := s.GetInEdges(p.newTo)
		if len(in) != 1 {
			t.Fatalf("after resolve, GetInEdges(%s) = %d, want 1", p.newTo, len(in))
		}
		got := in[0]
		if got.From != p.from || got.Kind != p.kind || got.FilePath != p.file || got.Line != p.line {
			t.Errorf("resolved edge to %s SCRAMBLED:\n  got  from=%s kind=%s file=%s line=%d\n  want from=%s kind=%s file=%s line=%d",
				p.newTo, got.From, got.Kind, got.FilePath, got.Line, p.from, p.kind, p.file, p.line)
		}
		// The stub edge must be gone.
		if stubIn := s.GetInEdges(p.oldTo); len(stubIn) != 0 {
			t.Errorf("stub %s still has %d incoming edges after resolve", p.oldTo, len(stubIn))
		}
	}
}

// TestEdgeFieldIntegrity_AllEdges sanity-checks AllEdges agrees with the
// per-node reads after a multi-repo bulk load (no scramble in the full
// table scan path either).
func TestEdgeFieldIntegrity_AllEdges(t *testing.T) {
	s := openProbe(t)
	var nodes []*graph.Node
	var edges []*graph.Edge
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeReturns, graph.EdgeTypedAs}
	for i := 0; i < 20; i++ {
		repo := []string{"gortex", "rate_checkers_detector", "gcx-ts"}[i%3]
		file := fmt.Sprintf("%s/p/f%d.go", repo, i)
		from := fmt.Sprintf("%s::A%d", file, i)
		to := fmt.Sprintf("%s::B%d", file, i)
		nodes = append(nodes,
			&graph.Node{ID: from, Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo},
			&graph.Node{ID: to, Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo})
		edges = append(edges, &graph.Edge{From: from, To: to, Kind: kinds[i%len(kinds)], FilePath: file, Line: i + 1})
	}
	s.AddBatch(nodes, edges)

	all := s.AllEdges()
	byFrom := map[string]*graph.Edge{}
	for _, e := range all {
		byFrom[e.From] = e
	}
	var froms []string
	for _, e := range edges {
		froms = append(froms, e.From)
	}
	sort.Strings(froms)
	for _, e := range edges {
		got, ok := byFrom[e.From]
		if !ok {
			t.Errorf("AllEdges missing edge from %s", e.From)
			continue
		}
		if got.To != e.To || got.Kind != e.Kind || got.FilePath != e.FilePath || got.Line != e.Line {
			t.Errorf("AllEdges scrambled edge from %s:\n  got  to=%s kind=%s file=%s line=%d\n  want to=%s kind=%s file=%s line=%d",
				e.From, got.To, got.Kind, got.FilePath, got.Line, e.To, e.Kind, e.FilePath, e.Line)
		}
	}
}
