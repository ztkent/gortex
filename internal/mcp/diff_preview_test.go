package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestUnifiedDiff(t *testing.T) {
	require.Equal(t, "", unifiedDiff("x.go", "same\n", "same\n"), "identical content yields no diff")

	d := unifiedDiff("x.go", "line one\nline two\n", "line one\nline TWO\n")
	require.Contains(t, d, "--- a/x.go")
	require.Contains(t, d, "+++ b/x.go")
	require.Contains(t, d, "@@")
	require.Contains(t, d, "-line two")
	require.Contains(t, d, "+line TWO")
}

func callEditHandlerJSON(t *testing.T, h func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool returned error: %v", res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

func TestEditSymbolDryRunPreviewsWithoutWriting(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(mainGo)
	require.NoError(t, err)

	resp := callEditHandlerJSON(t, srv.handleEditSymbol, map[string]any{
		"id":         "main.go::helper",
		"old_source": "func helper() {}",
		"new_source": "func helper() { _ = 1 }",
		"dry_run":    true,
	})

	require.Equal(t, "would_apply", resp["status"])
	require.Equal(t, true, resp["dry_run"])
	diff, _ := resp["diff"].(string)
	require.Contains(t, diff, "+func helper() { _ = 1 }")
	require.Contains(t, diff, "-func helper() {}")

	after, err := os.ReadFile(mainGo)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "dry_run must not modify the file")
}

func TestEditFileDryRunIncludesDiff(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(mainGo)
	require.NoError(t, err)

	resp := callEditHandlerJSON(t, srv.handleEditFile, map[string]any{
		"path":       mainGo,
		"old_string": "Port int",
		"new_string": "Port int // edited",
		"dry_run":    true,
	})

	require.Equal(t, "would_apply", resp["status"])
	diff, _ := resp["diff"].(string)
	require.Contains(t, diff, "+\tPort int // edited")

	after, err := os.ReadFile(mainGo)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "dry_run must not modify the file")
}

func TestWriteFileDryRunIncludesDiff(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(mainGo)
	require.NoError(t, err)

	resp := callEditHandlerJSON(t, srv.handleWriteFile, map[string]any{
		"path":    mainGo,
		"content": "package main\n\nfunc main() {}\n",
		"dry_run": true,
	})

	require.Equal(t, "would_overwrite", resp["status"])
	diff, _ := resp["diff"].(string)
	require.True(t, strings.Contains(diff, "-") && strings.Contains(diff, "@@"), "overwrite diff should show removed lines")

	after, err := os.ReadFile(mainGo)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "dry_run must not modify the file")
}
