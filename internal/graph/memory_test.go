package graph

import (
	"testing"
)

func TestRepoMemoryEstimate_Empty(t *testing.T) {
	g := New()
	est := g.RepoMemoryEstimate("nonexistent")
	if est.NodeCount != 0 || est.EdgeCount != 0 || est.Total() != 0 {
		t.Errorf("empty repo should estimate zero, got %+v", est)
	}
}

func TestRepoMemoryEstimate_NodesAndEdges(t *testing.T) {
	g := New()
	n1 := &Node{ID: "r/pkg/a.go::Foo", Kind: KindFunction, Name: "Foo",
		QualName: "pkg.Foo", FilePath: "pkg/a.go", Language: "go",
		RepoPrefix: "r"}
	n2 := &Node{ID: "r/pkg/a.go::Bar", Kind: KindFunction, Name: "Bar",
		QualName: "pkg.Bar", FilePath: "pkg/a.go", Language: "go",
		RepoPrefix: "r"}
	g.AddNode(n1)
	g.AddNode(n2)
	g.AddEdge(&Edge{From: n1.ID, To: n2.ID, Kind: "CALLS", FilePath: "pkg/a.go"})

	est := g.RepoMemoryEstimate("r")
	if est.NodeCount != 2 {
		t.Errorf("expected 2 nodes, got %d", est.NodeCount)
	}
	if est.EdgeCount != 1 {
		t.Errorf("expected 1 edge, got %d", est.EdgeCount)
	}
	if est.NodeBytes == 0 || est.EdgeBytes == 0 {
		t.Errorf("expected non-zero byte estimates, got %+v", est)
	}
}

func TestRepoMemoryEstimate_MetaContributes(t *testing.T) {
	g := New()
	short := &Node{ID: "r/a::s", Kind: KindVariable, Name: "s",
		FilePath: "a", RepoPrefix: "r"}
	long := &Node{ID: "r/a::l", Kind: KindVariable, Name: "l",
		FilePath: "a", RepoPrefix: "r",
		Meta: map[string]any{
			"signature": "func Foo(ctx context.Context, x, y int) (string, error)",
			"docstring": "A long docstring that takes many bytes to store in memory",
			"tags":      []string{"public", "deprecated", "hot-path"},
		}}

	g.AddNode(short)
	g.AddNode(long)

	shortEst := g.RepoMemoryEstimate("r").NodeBytes / 2 // rough avg — need per-node
	_ = shortEst

	// Direct per-node check via unexported helper is fine from test in same package.
	if nodeBytes(long) <= nodeBytes(short) {
		t.Errorf("a node with Meta should be bigger than one without: long=%d short=%d",
			nodeBytes(long), nodeBytes(short))
	}
}

func TestMetaBytes_HandlesCommonTypes(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{"s": "hello"},
		{"b": true, "i": 42, "f": 3.14},
		{"list": []string{"a", "b", "c"}},
		{"nested": map[string]any{"k": "v"}},
	}
	for i, m := range cases {
		// Should never panic; empty map returns non-zero-ish (header).
		got := metaBytes(m)
		if m == nil && got != 0 {
			t.Errorf("case %d: nil map should be 0, got %d", i, got)
		}
	}
}
