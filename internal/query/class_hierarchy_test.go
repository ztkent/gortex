package query

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// buildHierarchyGraph models a small OO subtree:
//
//	interface Animal
//	    ↑ implements         (in-edges of Animal)
//	  type Dog                ← composes Tail
//	     ↑ extends            (in-edges of Dog)
//	  type Puppy              ← extends
//	  type ServiceDog         ← extends
//
//	method Animal.Speak       — interface method
//	method Dog.Speak          — overrides Animal.Speak
//	method Puppy.Speak        — overrides Dog.Speak
//	method ServiceDog.Speak   — overrides Dog.Speak
//
// Plus an unrelated method Tail.Wag attached to the composed Tail type
// so include_methods doesn't accidentally pull it in via the composition
// edge alone.
func buildHierarchyGraph() *graph.Graph {
	g := graph.New()

	nodes := []*graph.Node{
		{ID: "animal.go::Animal", Kind: graph.KindInterface, Name: "Animal", FilePath: "animal.go", Language: "go"},
		{ID: "animal.go::Animal.Speak", Kind: graph.KindMethod, Name: "Animal.Speak", FilePath: "animal.go", Language: "go"},

		{ID: "dog.go::Dog", Kind: graph.KindType, Name: "Dog", FilePath: "dog.go", Language: "go"},
		{ID: "dog.go::Dog.Speak", Kind: graph.KindMethod, Name: "Dog.Speak", FilePath: "dog.go", Language: "go"},

		{ID: "tail.go::Tail", Kind: graph.KindType, Name: "Tail", FilePath: "tail.go", Language: "go"},
		{ID: "tail.go::Tail.Wag", Kind: graph.KindMethod, Name: "Tail.Wag", FilePath: "tail.go", Language: "go"},

		{ID: "puppy.go::Puppy", Kind: graph.KindType, Name: "Puppy", FilePath: "puppy.go", Language: "go"},
		{ID: "puppy.go::Puppy.Speak", Kind: graph.KindMethod, Name: "Puppy.Speak", FilePath: "puppy.go", Language: "go"},

		{ID: "service.go::ServiceDog", Kind: graph.KindType, Name: "ServiceDog", FilePath: "service.go", Language: "go"},
		{ID: "service.go::ServiceDog.Speak", Kind: graph.KindMethod, Name: "ServiceDog.Speak", FilePath: "service.go", Language: "go"},
	}
	for _, n := range nodes {
		g.AddNode(n)
	}

	edges := []*graph.Edge{
		// Type hierarchy.
		{From: "dog.go::Dog", To: "animal.go::Animal", Kind: graph.EdgeImplements, FilePath: "dog.go"},
		{From: "dog.go::Dog", To: "tail.go::Tail", Kind: graph.EdgeComposes, FilePath: "dog.go"},
		{From: "puppy.go::Puppy", To: "dog.go::Dog", Kind: graph.EdgeExtends, FilePath: "puppy.go"},
		{From: "service.go::ServiceDog", To: "dog.go::Dog", Kind: graph.EdgeExtends, FilePath: "service.go"},

		// Method membership.
		{From: "animal.go::Animal.Speak", To: "animal.go::Animal", Kind: graph.EdgeMemberOf, FilePath: "animal.go"},
		{From: "dog.go::Dog.Speak", To: "dog.go::Dog", Kind: graph.EdgeMemberOf, FilePath: "dog.go"},
		{From: "tail.go::Tail.Wag", To: "tail.go::Tail", Kind: graph.EdgeMemberOf, FilePath: "tail.go"},
		{From: "puppy.go::Puppy.Speak", To: "puppy.go::Puppy", Kind: graph.EdgeMemberOf, FilePath: "puppy.go"},
		{From: "service.go::ServiceDog.Speak", To: "service.go::ServiceDog", Kind: graph.EdgeMemberOf, FilePath: "service.go"},

		// Method overrides.
		{From: "dog.go::Dog.Speak", To: "animal.go::Animal.Speak", Kind: graph.EdgeOverrides, FilePath: "dog.go"},
		{From: "puppy.go::Puppy.Speak", To: "dog.go::Dog.Speak", Kind: graph.EdgeOverrides, FilePath: "puppy.go"},
		{From: "service.go::ServiceDog.Speak", To: "dog.go::Dog.Speak", Kind: graph.EdgeOverrides, FilePath: "service.go"},
	}
	for _, e := range edges {
		g.AddEdge(e)
	}
	return g
}

func sortedNodeIDs(sg *SubGraph) []string {
	ids := make([]string, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestClassHierarchy_TypeUp(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// From a leaf type, walking "up" should reach its parent (Dog),
	// the interface Dog implements (Animal), and the type Dog composes (Tail).
	sg := e.ClassHierarchy("puppy.go::Puppy", HierarchyUp, 5, false, QueryOptions{})

	ids := sortedNodeIDs(sg)
	assert.Contains(t, ids, "puppy.go::Puppy")
	assert.Contains(t, ids, "dog.go::Dog")
	assert.Contains(t, ids, "animal.go::Animal")
	assert.Contains(t, ids, "tail.go::Tail")
	// Sibling ServiceDog should not appear in an "up"-only walk from Puppy.
	assert.NotContains(t, ids, "service.go::ServiceDog")
}

func TestClassHierarchy_TypeDown(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// From the root interface Animal, "down" should reach implementers
	// and their subclasses transitively.
	sg := e.ClassHierarchy("animal.go::Animal", HierarchyDown, 5, false, QueryOptions{})

	ids := sortedNodeIDs(sg)
	assert.Contains(t, ids, "animal.go::Animal")
	assert.Contains(t, ids, "dog.go::Dog")
	assert.Contains(t, ids, "puppy.go::Puppy")
	assert.Contains(t, ids, "service.go::ServiceDog")
	// Tail is a composition-up from Dog, not a child — must not appear.
	assert.NotContains(t, ids, "tail.go::Tail")
}

func TestClassHierarchy_TypeBoth(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	sg := e.ClassHierarchy("dog.go::Dog", HierarchyBoth, 5, false, QueryOptions{})
	ids := sortedNodeIDs(sg)

	// Both directions from Dog: parents (Animal, Tail) and children (Puppy, ServiceDog).
	assert.Contains(t, ids, "animal.go::Animal")
	assert.Contains(t, ids, "tail.go::Tail")
	assert.Contains(t, ids, "puppy.go::Puppy")
	assert.Contains(t, ids, "service.go::ServiceDog")
}

func TestClassHierarchy_DepthLimit(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// Depth=1 from Animal should reach direct implementers but not
	// their grandchildren.
	sg := e.ClassHierarchy("animal.go::Animal", HierarchyDown, 1, false, QueryOptions{})
	ids := sortedNodeIDs(sg)
	assert.Contains(t, ids, "dog.go::Dog")
	assert.NotContains(t, ids, "puppy.go::Puppy")
	assert.NotContains(t, ids, "service.go::ServiceDog")
}

func TestClassHierarchy_MethodSeed(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// Dog.Speak — up should reach Animal.Speak, down should reach
	// Puppy.Speak and ServiceDog.Speak.
	sg := e.ClassHierarchy("dog.go::Dog.Speak", HierarchyBoth, 5, false, QueryOptions{})
	ids := sortedNodeIDs(sg)
	assert.Contains(t, ids, "animal.go::Animal.Speak")
	assert.Contains(t, ids, "puppy.go::Puppy.Speak")
	assert.Contains(t, ids, "service.go::ServiceDog.Speak")
}

func TestClassHierarchy_IncludeMethods(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// From Dog with include_methods, we should pull in Dog.Speak (member),
	// then walk EdgeOverrides up (to Animal.Speak) and down (to Puppy.Speak,
	// ServiceDog.Speak). Tail.Wag should also appear because Tail is in
	// the type subgraph and we surface its members. No override chain
	// rooted at Tail.Wag exists, so it stays as a leaf.
	sg := e.ClassHierarchy("dog.go::Dog", HierarchyBoth, 5, true, QueryOptions{})
	ids := sortedNodeIDs(sg)

	assert.Contains(t, ids, "dog.go::Dog.Speak")
	assert.Contains(t, ids, "animal.go::Animal.Speak")
	assert.Contains(t, ids, "puppy.go::Puppy.Speak")
	assert.Contains(t, ids, "service.go::ServiceDog.Speak")
	assert.Contains(t, ids, "tail.go::Tail.Wag")
}

func TestClassHierarchy_NoMethods(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())

	// Without include_methods, no method nodes should be pulled in just
	// from a type seed (only types in the hierarchy plus Tail via composition).
	sg := e.ClassHierarchy("dog.go::Dog", HierarchyBoth, 5, false, QueryOptions{})
	for _, n := range sg.Nodes {
		assert.NotEqual(t, graph.KindMethod, n.Kind, "unexpected method node %q in non-include_methods walk", n.ID)
	}
}

func TestClassHierarchy_UnknownSeed(t *testing.T) {
	e := NewEngine(buildHierarchyGraph())
	sg := e.ClassHierarchy("does/not/exist", HierarchyBoth, 5, false, QueryOptions{})
	assert.Empty(t, sg.Nodes)
	assert.Empty(t, sg.Edges)
}

func TestClassHierarchy_WorkspaceScope(t *testing.T) {
	g := graph.New()
	// Two workspaces. ws=alpha contains Base, ws=beta contains Sub.
	g.AddNode(&graph.Node{ID: "alpha::base", Kind: graph.KindType, Name: "Base", FilePath: "a/base.go", WorkspaceID: "alpha"})
	g.AddNode(&graph.Node{ID: "beta::sub", Kind: graph.KindType, Name: "Sub", FilePath: "b/sub.go", WorkspaceID: "beta"})
	g.AddEdge(&graph.Edge{From: "beta::sub", To: "alpha::base", Kind: graph.EdgeExtends})

	e := NewEngine(g)

	// Walking down from Base with no scope sees Sub.
	sg := e.ClassHierarchy("alpha::base", HierarchyDown, 5, false, QueryOptions{})
	require.Equal(t, 2, len(sg.Nodes))

	// Confined to the alpha workspace, the cross-workspace child is dropped.
	sg = e.ClassHierarchy("alpha::base", HierarchyDown, 5, false, QueryOptions{WorkspaceID: "alpha"})
	ids := sortedNodeIDs(sg)
	assert.Equal(t, []string{"alpha::base"}, ids)
}

func TestClassHierarchy_MinTier(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "p", Kind: graph.KindInterface, Name: "P"})
	g.AddNode(&graph.Node{ID: "c1", Kind: graph.KindType, Name: "C1"})
	g.AddNode(&graph.Node{ID: "c2", Kind: graph.KindType, Name: "C2"})
	// One LSP-resolved implements edge, one inferred.
	g.AddEdge(&graph.Edge{From: "c1", To: "p", Kind: graph.EdgeImplements, Origin: graph.OriginLSPResolved})
	g.AddEdge(&graph.Edge{From: "c2", To: "p", Kind: graph.EdgeImplements, Origin: graph.OriginASTInferred})

	e := NewEngine(g)
	// Without min_tier, both implementers come back.
	sg := e.ClassHierarchy("p", HierarchyDown, 5, false, QueryOptions{})
	assert.Equal(t, 2, len(sg.Edges))

	// With min_tier=lsp_resolved, only the high-confidence edge survives.
	sg = e.ClassHierarchy("p", HierarchyDown, 5, false, QueryOptions{MinTier: graph.OriginLSPResolved})
	require.Equal(t, 1, len(sg.Edges))
	assert.Equal(t, "c1", sg.Edges[0].From)
}
