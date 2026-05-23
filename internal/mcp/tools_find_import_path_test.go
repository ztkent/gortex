package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// TestFindImportPath_RespectsTargetLanguage pins the language filter:
// when an identifier exists in both a Go file and a TypeScript file,
// asking for it in the context of a Go file must surface the Go hit,
// not the TS one. Pre-fix, the handler returned whichever
// FindSymbols(name) saw first — typically the TS hit because graph
// iteration order is not language-aware.
func TestFindImportPath_RespectsTargetLanguage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "frontend"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "node.go"),
		[]byte("package pkg\n\ntype Node struct{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "frontend", "node.ts"),
		[]byte("export class Node {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	// Caller is in main.go (a Go file). Even though a TS `Node`
	// exists, the result must be the Go declaration in pkg/.
	res := callTool(t, srv, "find_import_path", map[string]any{
		"name": "Node",
		"path": "main.go",
	})
	require.False(t, res.IsError, "find_import_path should not error: %+v", res.Content)
	var got struct {
		SymbolID   string `json:"symbol_id"`
		ImportPath string `json:"import_path"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &got))
	require.Contains(t, got.SymbolID, "pkg/node.go::Node",
		"Go file context must resolve to the Go Node, got %q", got.SymbolID)
	require.Equal(t, "pkg", got.ImportPath)
}

// TestFindImportPath_AcceptsQualifiedName pins the qualifier path: a
// Go-style "pkg.Node" must split on the dot and bias toward
// candidates whose directory leaf matches the qualifier. Before the
// fix, the whole string was passed to FindSymbols and got back
// `symbol not found` because no node is literally named "pkg.Node".
func TestFindImportPath_AcceptsQualifiedName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "graph"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "graph", "node.go"),
		[]byte("package graph\n\ntype Node struct{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)

	res := callTool(t, srv, "find_import_path", map[string]any{
		"name": "graph.Node",
		"path": "main.go",
	})
	require.False(t, res.IsError,
		"find_import_path must accept Go-qualified names: %+v", res.Content)
	var got struct {
		SymbolID   string `json:"symbol_id"`
		ImportPath string `json:"import_path"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &got))
	require.Contains(t, got.SymbolID, "graph/node.go::Node")
	require.Equal(t, "graph", got.ImportPath)
}
