package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// TestReturnSubGraph_DiagramFormats asserts the shared traversal-result
// funnel renders mermaid and dot diagrams when asked, so a daemon-backed
// `gortex query callers --format mermaid|dot` gets a real diagram rather
// than JSON.
func TestReturnSubGraph_DiagramFormats(t *testing.T) {
	srv, _ := setupTestServer(t)
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			{ID: "pkg/a.go::Foo", Name: "Foo", Kind: graph.KindFunction, FilePath: "pkg/a.go", StartLine: 10},
			{ID: "pkg/b.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "pkg/b.go", StartLine: 20},
		},
		Edges: []*graph.Edge{
			{From: "pkg/b.go::Bar", To: "pkg/a.go::Foo", Kind: graph.EdgeCalls},
		},
	}

	mreq := mcp.CallToolRequest{}
	mreq.Params.Arguments = map[string]any{"format": "mermaid"}
	res, err := srv.returnSubGraph(context.Background(), mreq, sg)
	require.NoError(t, err)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "graph LR", "mermaid format should emit a mermaid graph")

	dreq := mcp.CallToolRequest{}
	dreq.Params.Arguments = map[string]any{"format": "dot"}
	res, err = srv.returnSubGraph(context.Background(), dreq, sg)
	require.NoError(t, err)
	tc, ok = res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "digraph", "dot format should emit a graphviz digraph")
}
