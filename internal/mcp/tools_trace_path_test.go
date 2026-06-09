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

// tracePathTestServer indexes a Go file with a known call chain
// A→B→C→D plus an isolated function so we can exercise both the found
// and not-found branches of trace_path.
func tracePathTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	src := `package main

func D() {}

func C() { D() }

func B() { C() }

func A() { B() }

func Isolated() {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644))
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

func TestHandleTracePath_FoundChain(t *testing.T) {
	srv := tracePathTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source_id": findFunctionID(t, srv, "A"),
		"sink_id":   findFunctionID(t, srv, "D"),
	}
	res, err := srv.handleTracePath(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool errored: %v", res)

	var payload struct {
		Found bool `json:"found"`
		Paths []struct {
			Nodes  []string `json:"nodes"`
			Length int      `json:"length"`
		} `json:"paths"`
	}
	text := res.Content[0].(mcplib.TextContent).Text
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.True(t, payload.Found, "expected a path; got %s", text)
	require.Len(t, payload.Paths, 1)
	require.Equal(t, 3, payload.Paths[0].Length, "A→B→C→D is 3 hops: %s", text)
	require.Len(t, payload.Paths[0].Nodes, 4)
}

func TestHandleTracePath_NotFoundDiagnosis(t *testing.T) {
	srv := tracePathTestServer(t)
	// A reaches B,C,D; Isolated has no incoming call edges → sink_no_in.
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"source_id": findFunctionID(t, srv, "A"),
		"sink_id":   findFunctionID(t, srv, "Isolated"),
	}
	res, err := srv.handleTracePath(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var payload struct {
		Found bool `json:"found"`
		Gap   *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"gap"`
	}
	text := res.Content[0].(mcplib.TextContent).Text
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.False(t, payload.Found)
	require.NotNil(t, payload.Gap, "expected a gap diagnosis: %s", text)
	require.Equal(t, "sink_no_in_edges", payload.Gap.Reason)
	require.NotEmpty(t, payload.Gap.Message)
}

func TestHandleTracePath_MissingSource(t *testing.T) {
	srv := tracePathTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"sink_id": "anything"}
	res, err := srv.handleTracePath(t.Context(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error for missing source_id")
}

func TestHandleTracePath_GCXFormat(t *testing.T) {
	srv := tracePathTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "trace_path"
	req.Params.Arguments = map[string]any{
		"source_id": findFunctionID(t, srv, "A"),
		"sink_id":   findFunctionID(t, srv, "D"),
		"format":    "gcx",
	}
	res, err := srv.handleTracePath(t.Context(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "trace_path.summary")
	require.Contains(t, text, "trace_path.paths")
}
