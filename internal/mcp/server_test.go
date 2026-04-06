package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"

	"github.com/zzet/gortex/internal/config"
	"os"
	"path/filepath"
)

func setupTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "fmt"

type Config struct {
	Port int
}

func main() {
	fmt.Println("hello")
	helper()
}

func helper() {}
`), 0o644)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop())
	srv.RunAnalysis()
	return srv, dir
}

func callTool(t *testing.T, srv *Server, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args

	result, err := findAndCallHandler(srv, name, context.Background(), req)
	require.NoError(t, err)
	return result
}

// findAndCallHandler dispatches a tool call by name via the registered handlers.
// Since MCPServer doesn't expose handlers directly, we recreate the request flow.
func findAndCallHandler(srv *Server, name string, ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	// Map tool names to handlers.
	handlers := map[string]func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error){
		"graph_stats":          srv.handleGraphStats,
		"search_symbols":       srv.handleSearchSymbols,
		"get_symbol":           srv.handleGetSymbol,
		"get_file_summary":     srv.handleGetFileSummary,
		"get_dependencies":     srv.handleGetDependencies,
		"get_dependents":       srv.handleGetDependents,
		"get_call_chain":       srv.handleGetCallChain,
		"get_callers":          srv.handleGetCallers,
		"find_implementations": srv.handleFindImplementations,
		"find_usages":          srv.handleFindUsages,
		"get_cluster":          srv.handleGetCluster,
		"get_editing_context":  srv.handleGetEditingContext,
		"get_symbol_signature": srv.handleGetSymbolSignature,
		"find_import_path":     srv.handleFindImportPath,
		"explain_change_impact": srv.handleEnhancedChangeImpact,
		"get_recent_changes":   srv.handleGetRecentChanges,
	}
	h, ok := handlers[name]
	if !ok {
		return mcplib.NewToolResultError("unknown tool: " + name), nil
	}
	return h(ctx, req)
}

func TestGraphStats(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "graph_stats", nil)
	assert.False(t, result.IsError)

	// Parse response.
	text := result.Content[0].(mcplib.TextContent).Text
	var stats graph.GraphStats
	require.NoError(t, json.Unmarshal([]byte(text), &stats))
	assert.Greater(t, stats.TotalNodes, 0)
	assert.Greater(t, stats.TotalEdges, 0)
}

func TestSearchSymbols(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "search_symbols", map[string]any{"query": "helper"})
	assert.False(t, result.IsError)

	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	assert.Greater(t, resp["total"], float64(0))
}

func TestGetSymbol(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "search_symbols", map[string]any{"query": "main"})
	text := result.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	json.Unmarshal([]byte(text), &resp)

	// Symbol not found returns error.
	result = callTool(t, srv, "get_symbol", map[string]any{"id": "nonexistent"})
	assert.True(t, result.IsError)
}

func TestGetEditingContext(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "get_editing_context", map[string]any{"file_path": "main.go"})
	assert.False(t, result.IsError)

	text := result.Content[0].(mcplib.TextContent).Text
	var ctx editingContext
	require.NoError(t, json.Unmarshal([]byte(text), &ctx))
	assert.NotNil(t, ctx.File)
	assert.Greater(t, len(ctx.Defines), 0)
}

func TestGetRecentChanges_NoWatcher(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := callTool(t, srv, "get_recent_changes", nil)
	assert.True(t, result.IsError) // watch mode not active
}
