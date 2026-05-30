package resolver_test

// Guards the cache-routing fix: during ResolveAll the per-pass name
// cache (warmLookupCache) must serve the method/function/type/field
// cascade, so the worker pool issues ZERO per-edge FindNodesByNameInRepo
// store calls. Before the fix, warmLookupCache seeded names from the raw
// `unresolved::*.<name>` stub id (never stripped), so every cascade
// lookup missed the cache and fell through to a per-edge
// FindNodesByNameInRepo — the warmup storm/hang on the 100k+ multi-repo
// prefixed-stub population.

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// countingStore wraps the in-memory graph and counts the repo-scoped
// per-edge lookup the cascade used to fire once per pending edge.
type countingStore struct {
	*graph.Graph
	findInRepoCalls int
}

func (c *countingStore) FindNodesByNameInRepo(name, repo string) []*graph.Node {
	c.findInRepoCalls++
	return c.Graph.FindNodesByNameInRepo(name, repo)
}

func TestResolveAll_Cascade_ServedFromCache_NoPerEdgeLookup(t *testing.T) {
	g := graph.New()
	cs := &countingStore{Graph: g}

	// A method call (resolveMethodCall path) and a plain function call
	// (resolveFunctionCall path) — both went through FindNodesByNameInRepo.
	g.AddNode(&graph.Node{ID: "r1/a.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "r1/a.go", RepoPrefix: "r1"})
	g.AddNode(&graph.Node{ID: "r1/b.go::doThing", Name: "doThing", Kind: graph.KindMethod, FilePath: "r1/b.go", RepoPrefix: "r1", Meta: map[string]any{"receiver": "T"}})
	g.AddNode(&graph.Node{ID: "r1/c.go::helper", Name: "helper", Kind: graph.KindFunction, FilePath: "r1/c.go", RepoPrefix: "r1"})
	g.AddEdge(&graph.Edge{From: "r1/a.go::Caller", To: "unresolved::*.doThing", Kind: graph.EdgeCalls, FilePath: "r1/a.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "r1/a.go::Caller", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "r1/a.go", Line: 2})

	// graph.Graph is not a BackendResolver, so ResolveAll runs the pure
	// Go worker-pool path — exactly the cascade under test.
	resolver.New(cs).ResolveAll()

	if cs.findInRepoCalls != 0 {
		t.Errorf("cascade issued %d per-edge FindNodesByNameInRepo calls; want 0 (cache should serve them)", cs.findInRepoCalls)
	}
}
