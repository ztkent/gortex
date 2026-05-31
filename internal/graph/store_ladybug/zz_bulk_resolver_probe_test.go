package store_ladybug_test

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
)

// TestBulkResolver_EdgeFieldIntegrity exercises the in-engine
// ResolveAllBulk Cypher rules (the path NOT covered by the existing
// zz_edge_integrity_probe tests). Each rule does
//
//	MATCH (caller)-[e]->(stub) ... DELETE e
//	CREATE (caller)-[newE {kind: e.kind, file_path: e.file_path, line: e.line, ...}]->(target)
//
// i.e. it reads e.kind / e.file_path / e.line off the SAME relationship it
// just DELETEd, inside one statement, across many edges. The hypothesis is
// that under this pattern the CREATE picks up another edge's kind/file_path
// while From/To/Line survive.
func TestBulkResolver_EdgeFieldIntegrity(t *testing.T) {
	s := openProbe(t)

	// Many callers, each in a DISTINCT repo / file, each with an
	// unresolved edge of a DISTINCT kind, all pointing at a stub whose
	// bare name resolves UNIQUELY to one real target node. Distinct
	// kinds + file_paths make a cross-edge scramble loud.
	type spec struct {
		repo string
		kind graph.EdgeKind
	}
	specs := []spec{
		{"gortex", graph.EdgeCalls},
		{"rate_checkers_detector", graph.EdgeReturns},
		{"gcx-ts", graph.EdgeInstantiates},
		{"web", graph.EdgeTypedAs},
		{"gortex-cloud", graph.EdgeReferences},
		{"gcx-go", graph.EdgeReads},
		{"infra", graph.EdgeCalls},
		{"docs", graph.EdgeReturns},
	}

	var nodes []*graph.Node
	var edges []*graph.Edge
	type plan struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}
	var plans []plan

	for i, sp := range specs {
		file := fmt.Sprintf("%s/internal/pkg/file%d.go", sp.repo, i)
		caller := fmt.Sprintf("%s::Caller%d", file, i)
		// Each target has a UNIQUE name so ResolveUniqueNames binds it
		// (exactly one candidate). The target lives in the SAME repo so
		// type-gated kinds (returns/typed_as) still resolve to a type.
		targetName := fmt.Sprintf("Target%d", i)
		targetFile := fmt.Sprintf("%s/internal/pkg/target%d.go", sp.repo, i)
		target := fmt.Sprintf("%s::%s", targetFile, targetName)
		// Type-position kinds must land on a KindType; others can land on
		// a function. Pick the target node kind accordingly so the
		// kind-gate in the rules doesn't reject the resolution.
		tgtKind := graph.KindFunction
		switch sp.kind {
		case graph.EdgeReturns, graph.EdgeTypedAs:
			tgtKind = graph.KindType
		}
		// Stub id in the multi-repo form the COPY rewrite produces.
		stub := fmt.Sprintf("%s::unresolved::%s", sp.repo, targetName)

		nodes = append(nodes,
			&graph.Node{ID: caller, Name: fmt.Sprintf("Caller%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: sp.repo, Language: "go"},
			&graph.Node{ID: target, Name: targetName, Kind: tgtKind, FilePath: targetFile, RepoPrefix: sp.repo, Language: "go"},
		)
		line := 400 + i
		edges = append(edges, &graph.Edge{From: caller, To: stub, Kind: sp.kind, FilePath: file, Line: line, Origin: "ast"})
		plans = append(plans, plan{from: caller, to: target, file: file, kind: sp.kind, line: line})
	}

	s.AddBatch(nodes, edges)

	// Drive the in-engine bulk resolver chain — the real cold-warmup path.
	n, err := s.ResolveAllBulk()
	if err != nil {
		t.Logf("ResolveAllBulk returned err (non-fatal per design): %v", err)
	}
	t.Logf("ResolveAllBulk resolved=%d", n)

	scrambled := 0
	for _, p := range plans {
		in := s.GetInEdges(p.to)
		if len(in) != 1 {
			t.Errorf("after bulk resolve, GetInEdges(%s) = %d edges, want 1", p.to, len(in))
			continue
		}
		got := in[0]
		ok := got.From == p.from && got.Kind == p.kind && got.FilePath == p.file && got.Line == p.line
		if !ok {
			scrambled++
			t.Errorf("BULK-RESOLVED edge to %s SCRAMBLED:\n  got  from=%s kind=%s file=%q line=%d\n  want from=%s kind=%s file=%q line=%d",
				p.to, got.From, got.Kind, got.FilePath, got.Line, p.from, p.kind, p.file, p.line)
		}
	}
	if scrambled > 0 {
		t.Errorf("BULK RESOLVER SCRAMBLED %d/%d edges", scrambled, len(plans))
	}
}

// TestBulkResolver_ManyEdgesSameTarget stresses the pattern further: a
// single popular target name with many same-name candidates is ambiguous
// (won't resolve), so use distinct names but a LARGER batch and interleave
// kinds so the engine pipelines DELETE+CREATE over a wide vector.
func TestBulkResolver_ManyEdgesSameTarget(t *testing.T) {
	s := openProbe(t)

	const repo = "gortex"
	kinds := []graph.EdgeKind{
		graph.EdgeCalls, graph.EdgeReturns, graph.EdgeInstantiates,
		graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReads,
	}

	var nodes []*graph.Node
	var edges []*graph.Edge
	type plan struct {
		from, to, file string
		kind           graph.EdgeKind
		line           int
	}
	var plans []plan

	const N = 60
	for i := 0; i < N; i++ {
		kind := kinds[i%len(kinds)]
		file := fmt.Sprintf("%s/pkg/a/caller%d.go", repo, i)
		caller := fmt.Sprintf("%s::Caller%d", file, i)
		targetName := fmt.Sprintf("Sym%d", i)
		targetFile := fmt.Sprintf("%s/pkg/b/sym%d.go", repo, i)
		target := fmt.Sprintf("%s::%s", targetFile, targetName)
		tgtKind := graph.KindFunction
		if kind == graph.EdgeReturns || kind == graph.EdgeTypedAs {
			tgtKind = graph.KindType
		}
		stub := fmt.Sprintf("%s::unresolved::%s", repo, targetName)
		nodes = append(nodes,
			&graph.Node{ID: caller, Name: fmt.Sprintf("Caller%d", i), Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo, Language: "go"},
			&graph.Node{ID: target, Name: targetName, Kind: tgtKind, FilePath: targetFile, RepoPrefix: repo, Language: "go"},
		)
		line := 1000 + i
		edges = append(edges, &graph.Edge{From: caller, To: stub, Kind: kind, FilePath: file, Line: line, Origin: "ast"})
		plans = append(plans, plan{from: caller, to: target, file: file, kind: kind, line: line})
	}

	s.AddBatch(nodes, edges)
	n, err := s.ResolveAllBulk()
	if err != nil {
		t.Logf("ResolveAllBulk err (non-fatal): %v", err)
	}
	t.Logf("ResolveAllBulk resolved=%d of %d", n, N)

	scrambled := 0
	wrongKind := 0
	wrongFile := 0
	for _, p := range plans {
		in := s.GetInEdges(p.to)
		if len(in) != 1 {
			t.Errorf("GetInEdges(%s)=%d want 1", p.to, len(in))
			continue
		}
		got := in[0]
		if got.From != p.from || got.Line != p.line {
			t.Errorf("from/line drift to=%s got from=%s line=%d want from=%s line=%d", p.to, got.From, got.Line, p.from, p.line)
		}
		if got.Kind != p.kind {
			wrongKind++
		}
		if got.FilePath != p.file {
			wrongFile++
		}
		if got.Kind != p.kind || got.FilePath != p.file {
			scrambled++
			if scrambled <= 10 {
				t.Logf("SCRAMBLE to=%s: got kind=%s file=%q ; want kind=%s file=%q (from=%s line=%d both)",
					p.to, got.Kind, got.FilePath, p.kind, p.file, got.From, got.Line)
			}
		}
	}
	if scrambled > 0 {
		t.Errorf("SCRAMBLED %d/%d (wrongKind=%d wrongFile=%d)", scrambled, N, wrongKind, wrongFile)
	}
}

var _ = store_ladybug.Options{}
