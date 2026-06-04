package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// callBatchEdit invokes handleBatchEdit directly: the shared callTool test
// helper's handler map does not register batch_edit.
func callBatchEdit(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = args
	res, err := srv.handleBatchEdit(context.Background(), req)
	require.NoError(t, err)
	return res
}

func TestParseBatchEdits(t *testing.T) {
	// Legacy JSON-string form.
	items, err := parseBatchEdits(`[{"id":"a","old_source":"x","new_source":"y"}]`)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "a", items[0].SymbolID)

	// Structured array form (what a typed-schema client now sends).
	items, err = parseBatchEdits([]any{
		map[string]any{"op": "edit_file", "path": "p", "old_string": "o", "new_string": "n"},
	})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "edit_file", items[0].kind())
	require.Equal(t, "p", items[0].Path)

	_, err = parseBatchEdits(nil)
	require.Error(t, err, "missing edits must error")

	_, err = parseBatchEdits(`{not json`)
	require.Error(t, err, "malformed edits must error")
}

func TestBatchEditItemKind(t *testing.T) {
	require.Equal(t, "edit_symbol", batchEditItem{SymbolID: "a"}.kind(), "default is edit_symbol")
	require.Equal(t, "edit_file", batchEditItem{Path: "p"}.kind(), "a path infers edit_file")
	require.Equal(t, "edit_file", batchEditItem{Op: "edit_file", Path: "p"}.kind())
	require.Equal(t, "edit_symbol", batchEditItem{Op: "edit_symbol", Path: "p"}.kind(), "explicit op wins over inference")
}

func TestBatchEditItemsSchemaOneOf(t *testing.T) {
	schema := batchEditItemsSchema()
	branches, ok := schema["oneOf"].([]any)
	require.True(t, ok, "items schema must be a oneOf")
	require.Len(t, branches, 2)
	for _, b := range branches {
		m := b.(map[string]any)
		require.Equal(t, "object", m["type"])
		props := m["properties"].(map[string]any)
		op := props["op"].(map[string]any)
		require.Contains(t, []any{"edit_symbol", "edit_file"}, op["const"], "each branch is discriminated by an op const")
		require.NotEmpty(t, m["required"], "each branch declares required fields")
	}
}

// TestBatchEditHeterogeneousApply drives a real mixed batch (one symbol edit +
// one file edit) against the same file and asserts both land on disk.
func TestBatchEditHeterogeneousApply(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")

	edits := []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "main.go::helper",
			"old_source": "func helper() {}",
			"new_source": "func helper() { _ = 1 }",
		},
		map[string]any{
			"op":         "edit_file",
			"path":       mainGo,
			"old_string": "Port int",
			"new_string": "Port int // configured",
		},
	}

	res := callBatchEdit(t, srv, map[string]any{"edits": edits})
	require.False(t, res.IsError, "batch should succeed: %v", res.Content)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	summary := resp["summary"].(map[string]any)
	require.EqualValues(t, 2, summary["applied"])
	require.EqualValues(t, 0, summary["failed"])

	got, err := os.ReadFile(mainGo)
	require.NoError(t, err)
	require.Contains(t, string(got), "func helper() { _ = 1 }", "symbol edit applied")
	require.Contains(t, string(got), "Port int // configured", "file edit applied")
}

// TestBatchEditDryRunReportsOps confirms dry_run reports the op kind per entry
// and orders symbol edits before file edits.
func TestBatchEditDryRunReportsOps(t *testing.T) {
	srv, _ := setupTestServer(t)
	edits := []any{
		map[string]any{"id": "main.go::helper", "old_source": "x", "new_source": "y"},
		map[string]any{"op": "edit_file", "path": "main.go", "old_string": "a", "new_string": "b"},
	}
	res := callBatchEdit(t, srv, map[string]any{"edits": edits, "dry_run": true})
	require.False(t, res.IsError)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Equal(t, true, resp["dry_run"])
	plan := resp["plan"].([]any)
	require.Len(t, plan, 2)
	require.Equal(t, "edit_symbol", plan[0].(map[string]any)["op"], "symbol edits sort first")
	require.Equal(t, "edit_file", plan[1].(map[string]any)["op"], "file edits sort last")
}
