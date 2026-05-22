package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestEnclosingName_FromEdgeMemberOf(t *testing.T) {
	g := graph.New()
	owner := &graph.Node{ID: "pkg/x.go::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "pkg/x.go"}
	// A field whose ID does NOT encode the owner -- only the
	// EdgeMemberOf edge can recover it.
	field := &graph.Node{ID: "pkg/x.go::field_42", Kind: graph.KindField, Name: "count", FilePath: "pkg/x.go"}
	g.AddNode(owner)
	g.AddNode(field)
	g.AddEdge(&graph.Edge{From: field.ID, To: owner.ID, Kind: graph.EdgeMemberOf})

	id, name := enclosingName(field, g)
	if id != "pkg/x.go::Widget" || name != "Widget" {
		t.Fatalf("enclosingName via EdgeMemberOf = (%q, %q), want (pkg/x.go::Widget, Widget)", id, name)
	}
}

func TestEnclosingName_MethodIDFallback(t *testing.T) {
	g := graph.New()
	// A method with no EdgeMemberOf edge -- the ID-prefix fallback
	// must still recover the receiver type.
	method := &graph.Node{ID: "pkg/d.go::Decoder.Decode", Kind: graph.KindMethod, Name: "Decode", FilePath: "pkg/d.go"}
	g.AddNode(method)
	id, name := enclosingName(method, g)
	if id != "pkg/d.go::Decoder" || name != "Decoder" {
		t.Fatalf("enclosingName ID fallback = (%q, %q), want (pkg/d.go::Decoder, Decoder)", id, name)
	}
}

func TestEnclosingName_TopLevelHasNone(t *testing.T) {
	g := graph.New()
	fn := &graph.Node{ID: "pkg/d.go::TopLevel", Kind: graph.KindFunction, Name: "TopLevel", FilePath: "pkg/d.go"}
	g.AddNode(fn)
	if id, name := enclosingName(fn, g); id != "" || name != "" {
		t.Fatalf("a top-level function should have no enclosing owner, got (%q, %q)", id, name)
	}
	if id, name := enclosingName(nil, g); id != "" || name != "" {
		t.Fatal("enclosingName(nil) should return empty strings")
	}
}

// TestFindUsages_GroupByFile drives find_usages with group_by:"file"
// and confirms the per-file bucketing shape: groups keyed by the
// caller's file, sorted by descending use count, each carrying the
// enclosing symbol of every reference.
func TestFindUsages_GroupByFile(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "pkg/lib.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/lib.go"}
	// Two callers in file a.go, one in b.go.
	a1 := &graph.Node{ID: "pkg/a.go::CallerA1", Kind: graph.KindFunction, Name: "CallerA1", FilePath: "pkg/a.go", StartLine: 1, EndLine: 9}
	a2 := &graph.Node{ID: "pkg/a.go::CallerA2", Kind: graph.KindFunction, Name: "CallerA2", FilePath: "pkg/a.go", StartLine: 10, EndLine: 19}
	b1 := &graph.Node{ID: "pkg/b.go::CallerB1", Kind: graph.KindFunction, Name: "CallerB1", FilePath: "pkg/b.go", StartLine: 1, EndLine: 9}
	for _, n := range []*graph.Node{target, a1, a2, b1} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: a1.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 5})
	g.AddEdge(&graph.Edge{From: a2.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 15})
	g.AddEdge(&graph.Edge{From: b1.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "pkg/b.go", Line: 5})

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewBM25())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": target.ID, "group_by": "file"}
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Equal(t, "file", resp["grouped_by"])
	require.EqualValues(t, 2, resp["file_count"])
	require.EqualValues(t, 3, resp["total_uses"])

	groups := resp["groups"].([]any)
	require.Len(t, groups, 2)
	// a.go has 2 uses, so it sorts first.
	first := groups[0].(map[string]any)
	require.Equal(t, "pkg/a.go", first["file"])
	require.EqualValues(t, 2, first["count"])
	uses := first["uses"].([]any)
	require.Len(t, uses, 2)
	// Each use carries its enclosing symbol.
	gotSyms := map[string]bool{}
	for _, u := range uses {
		gotSyms[u.(map[string]any)["symbol_name"].(string)] = true
	}
	require.True(t, gotSyms["CallerA1"] && gotSyms["CallerA2"],
		"group_by:file uses should name the enclosing caller symbols; got %v", gotSyms)
}

// TestFindUsages_FlatByDefault confirms the default (no group_by) path
// still returns the flat SubGraph -- the grouping is opt-in.
func TestFindUsages_FlatByDefault(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "pkg/lib.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/lib.go"}
	caller := &graph.Node{ID: "pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a.go"}
	g.AddNode(target)
	g.AddNode(caller)
	g.AddEdge(&graph.Edge{From: caller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 3})

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewBM25())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": target.ID}
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	// Flat SubGraph shape -- has "nodes"/"edges", not "groups".
	require.Contains(t, resp, "edges")
	require.NotContains(t, resp, "groups")
}
