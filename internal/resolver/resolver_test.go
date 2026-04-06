package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

func TestResolveAll_InternalCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "pkg/b.go", Language: "go"})

	// Foo calls Bar (unresolved).
	callEdge := &graph.Edge{From: "pkg/a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Bar", callEdge.To)
}

func TestResolveAll_ExternalImport(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"})

	importEdge := &graph.Edge{From: "main.go", To: "unresolved::import::fmt", Kind: graph.EdgeImports, FilePath: "main.go", Line: 3}
	g.AddEdge(importEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.External)
	assert.Equal(t, "external::fmt", importEdge.To)
}

func TestResolveAll_MethodCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "pkg/b.go", Language: "go"})

	callEdge := &graph.Edge{From: "pkg/a.go::Caller", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/b.go::Server.Start", callEdge.To)
}

func TestResolveAll_Unresolvable(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})

	callEdge := &graph.Edge{From: "a.go::Foo", To: "unresolved::NonExistent", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveAll()

	assert.Equal(t, 1, stats.Unresolved)
}

func TestInferImplements(t *testing.T) {
	g := graph.New()

	// Interface with two methods.
	g.AddNode(&graph.Node{
		ID: "pkg/store.go::Repository", Kind: graph.KindInterface, Name: "Repository",
		FilePath: "pkg/store.go", Language: "go", StartLine: 1,
		Meta: map[string]any{"methods": []string{"FindByID", "Save"}},
	})

	// Type that satisfies the interface.
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo", Kind: graph.KindType, Name: "SQLRepo",
		FilePath: "pkg/db.go", Language: "go", StartLine: 1,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo.FindByID", Kind: graph.KindMethod, Name: "FindByID",
		FilePath: "pkg/db.go", Language: "go", StartLine: 5,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::SQLRepo.Save", Kind: graph.KindMethod, Name: "Save",
		FilePath: "pkg/db.go", Language: "go", StartLine: 10,
	})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::SQLRepo.FindByID", To: "pkg/db.go::SQLRepo", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 5})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::SQLRepo.Save", To: "pkg/db.go::SQLRepo", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 10})

	// Type that does NOT satisfy (missing Save).
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::ReadOnly", Kind: graph.KindType, Name: "ReadOnly",
		FilePath: "pkg/db.go", Language: "go", StartLine: 20,
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::ReadOnly.FindByID", Kind: graph.KindMethod, Name: "FindByID",
		FilePath: "pkg/db.go", Language: "go", StartLine: 22,
	})
	g.AddEdge(&graph.Edge{From: "pkg/db.go::ReadOnly.FindByID", To: "pkg/db.go::ReadOnly", Kind: graph.EdgeMemberOf, FilePath: "pkg/db.go", Line: 22})

	r := New(g)
	added := r.InferImplements()

	assert.Equal(t, 1, added)

	// Verify the implements edge was added.
	edges := g.GetInEdges("pkg/store.go::Repository")
	var implEdges []*graph.Edge
	for _, e := range edges {
		if e.Kind == graph.EdgeImplements {
			implEdges = append(implEdges, e)
		}
	}
	assert.Len(t, implEdges, 1)
	assert.Equal(t, "pkg/db.go::SQLRepo", implEdges[0].From)
}

func TestInferImplements_EmptyInterface(t *testing.T) {
	g := graph.New()

	// Empty interface should not match anything.
	g.AddNode(&graph.Node{
		ID: "pkg/any.go::Any", Kind: graph.KindInterface, Name: "Any",
		FilePath: "pkg/any.go", Language: "go", StartLine: 1,
		Meta: map[string]any{"methods": []string{}},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/db.go::Foo", Kind: graph.KindType, Name: "Foo",
		FilePath: "pkg/db.go", Language: "go", StartLine: 1,
	})

	r := New(g)
	added := r.InferImplements()
	assert.Equal(t, 0, added)
}

func TestResolveFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "b.go", Language: "go"})

	callEdge := &graph.Edge{From: "a.go::Foo", To: "unresolved::Bar", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
	g.AddEdge(callEdge)

	r := New(g)
	stats := r.ResolveFile("a.go")

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "b.go::Bar", callEdge.To)
}
