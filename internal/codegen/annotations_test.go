package codegen

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func annoNode(id, name string) *graph.Node {
	return &graph.Node{ID: id, Kind: graph.KindType, Name: name, Meta: map[string]any{"kind": "annotation"}}
}

func TestMarkAnnotatedGenerated_Lombok(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "User.java::User", Kind: graph.KindType, Name: "User", FilePath: "User.java"},
		annoNode("annotation::java::Data", "Data"),
	}
	edges := []*graph.Edge{
		{From: "User.java::User", To: "annotation::java::Data", Kind: graph.EdgeAnnotated},
	}
	extra, stats := MarkAnnotatedGenerated(nodes, edges)
	require.Equal(t, 1, stats.NodesMarked)
	require.Len(t, extra, 1)
	require.Equal(t, graph.EdgeGeneratedBy, extra[0].Kind)
	require.Equal(t, "external::generator-tool:lombok", extra[0].To)

	host := nodes[0]
	require.Equal(t, true, host.Meta["has_generated_members"])
	require.Equal(t, "lombok", host.Meta["codegen_tool"])
	require.Contains(t, host.Meta["generated_members"].([]string), "getters")
}

func TestMarkAnnotatedGenerated_MapStruct(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "M.java::M", Kind: graph.KindInterface, Name: "M", FilePath: "M.java"},
		annoNode("annotation::java::Mapper", "org.mapstruct.Mapper"),
	}
	edges := []*graph.Edge{{From: "M.java::M", To: "annotation::java::Mapper", Kind: graph.EdgeAnnotated}}
	_, stats := MarkAnnotatedGenerated(nodes, edges)
	require.Equal(t, 1, stats.NodesMarked)
	require.Equal(t, "mapstruct", nodes[0].Meta["codegen_tool"])
}

func TestMarkAnnotatedGenerated_IgnoresNonCodegen(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "C.java::C", Kind: graph.KindType, Name: "C"},
		annoNode("annotation::java::Override", "Override"),
	}
	edges := []*graph.Edge{{From: "C.java::C", To: "annotation::java::Override", Kind: graph.EdgeAnnotated}}
	extra, stats := MarkAnnotatedGenerated(nodes, edges)
	require.Equal(t, 0, stats.NodesMarked)
	require.Empty(t, extra)
	require.Nil(t, nodes[0].Meta)
}

func TestMarkAnnotatedGenerated_MergesAndDedups(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "C::C", Kind: graph.KindType, Name: "C"},
		annoNode("annotation::java::Getter", "Getter"),
		annoNode("annotation::java::Setter", "Setter"),
	}
	edges := []*graph.Edge{
		{From: "C::C", To: "annotation::java::Getter", Kind: graph.EdgeAnnotated},
		{From: "C::C", To: "annotation::java::Setter", Kind: graph.EdgeAnnotated},
	}
	extra, stats := MarkAnnotatedGenerated(nodes, edges)
	require.Equal(t, 2, stats.NodesMarked)
	require.Len(t, extra, 1, "one EdgeGeneratedBy per (symbol, tool)")
	members := nodes[0].Meta["generated_members"].([]string)
	require.Contains(t, members, "getters")
	require.Contains(t, members, "setters")
}

func TestMarkAnnotatedGenerated_MVVMSourceGenerators(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "vm.cs::name", Kind: graph.KindField, Name: "name", FilePath: "vm.cs"},
		annoNode("annotation::csharp::ObservableProperty", "ObservableProperty"),
	}
	edges := []*graph.Edge{
		{From: "vm.cs::name", To: "annotation::csharp::ObservableProperty", Kind: graph.EdgeAnnotated},
	}
	_, stats := MarkAnnotatedGenerated(nodes, edges)
	require.Equal(t, 1, stats.NodesMarked)
	require.Equal(t, "mvvm_toolkit", nodes[0].Meta["codegen_tool"])
	require.Contains(t, nodes[0].Meta["generated_members"].([]string), "observable_property")
}

func TestNormalizeAnnotationName(t *testing.T) {
	require.Equal(t, "Data", normalizeAnnotationName("@Data"))
	require.Equal(t, "Data", normalizeAnnotationName("lombok.Data"))
	require.Equal(t, "Mapper", normalizeAnnotationName(" @org.mapstruct.Mapper "))
}
