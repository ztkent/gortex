package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func newCheckRefsTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()

	g.AddNode(&graph.Node{ID: "p/handler.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "p/handler.go", StartLine: 5})
	g.AddNode(&graph.Node{ID: "p/router.go::Route", Name: "Route", Kind: graph.KindFunction, FilePath: "p/router.go", StartLine: 10})
	g.AddNode(&graph.Node{ID: "p/router_test.go::TestRoute", Name: "TestRoute", Kind: graph.KindFunction, FilePath: "p/router_test.go", StartLine: 5})

	// Route calls Handle (production caller).
	g.AddEdge(&graph.Edge{From: "p/router.go::Route", To: "p/handler.go::Handle", Kind: graph.EdgeCalls})
	// TestRoute references Handle (test caller).
	g.AddEdge(&graph.Edge{From: "p/router_test.go::TestRoute", To: "p/handler.go::Handle", Kind: graph.EdgeReferences})

	// A different file defines a symbol with the same Name as Handle.
	g.AddNode(&graph.Node{ID: "other/util.go::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: "other/util.go", StartLine: 20})

	// File-import edges so importing_files works: importer.go imports handler.go.
	g.AddNode(&graph.Node{ID: "p/importer.go", Name: "importer.go", Kind: graph.KindFile, FilePath: "p/importer.go"})
	g.AddNode(&graph.Node{ID: "p/handler.go", Name: "handler.go", Kind: graph.KindFile, FilePath: "p/handler.go"})
	g.AddEdge(&graph.Edge{From: "p/importer.go", To: "p/handler.go", Kind: graph.EdgeImports})

	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callCheckRefs(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleCheckReferences(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true}
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestCheckReferences_Referenced(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{
		"symbol_id": "p/handler.go::Handle",
	})

	assert.Equal(t, true, out["referenced"])
	assert.EqualValues(t, 2, out["total_references"].(float64))
	byKind := out["by_kind"].(map[string]any)
	assert.EqualValues(t, 1, byKind["calls"].(float64))
	assert.EqualValues(t, 1, byKind["references"].(float64))
	assert.EqualValues(t, 1, out["caller_count"].(float64))
}

func TestCheckReferences_NoReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x::Lonely", Name: "Lonely", Kind: graph.KindFunction, FilePath: "x.go"})
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	out := callCheckRefs(t, s, map[string]any{"symbol_id": "x::Lonely"})
	assert.Equal(t, false, out["referenced"])
	assert.EqualValues(t, 0, out["total_references"].(float64))
}

func TestCheckReferences_SameNameElsewhere(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{
		"symbol_id": "p/handler.go::Handle",
	})

	sn, _ := out["same_name_elsewhere"].([]any)
	require.Len(t, sn, 1, "other/util.go::Handle should surface as same-name elsewhere")
	row := sn[0].(map[string]any)
	assert.Equal(t, "other/util.go::Handle", row["id"])
}

// TestCheckReferences_EvidencePrefersCallSiteLine pins the bug where
// two distinct calls from the same caller collapsed into evidence
// rows pinned to the caller's start line. The fix prefers Edge.Line
// / Edge.FilePath when populated.
func TestCheckReferences_EvidencePrefersCallSiteLine(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "p/target.go::Target", Name: "Target", Kind: graph.KindFunction, FilePath: "p/target.go", StartLine: 5})
	g.AddNode(&graph.Node{ID: "p/caller.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "p/caller.go", StartLine: 10})
	// Two distinct call sites from Caller, each carrying its own Line.
	g.AddEdge(&graph.Edge{From: "p/caller.go::Caller", To: "p/target.go::Target", Kind: graph.EdgeCalls, FilePath: "p/caller.go", Line: 27})
	g.AddEdge(&graph.Edge{From: "p/caller.go::Caller", To: "p/target.go::Target", Kind: graph.EdgeCalls, FilePath: "p/caller.go", Line: 42})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	out := callCheckRefs(t, s, map[string]any{"symbol_id": "p/target.go::Target"})
	ev, _ := out["evidence"].([]any)
	require.Len(t, ev, 2, "two distinct call sites must produce two evidence rows")
	row0 := ev[0].(map[string]any)
	row1 := ev[1].(map[string]any)
	// Lines must reflect the edges, not the caller's StartLine (10).
	got := []float64{row0["line"].(float64), row1["line"].(float64)}
	require.ElementsMatch(t, []float64{27, 42}, got,
		"evidence line must surface the call-site, got %v", got)
}

func TestCheckReferences_ImportingFilesFound(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{"symbol_id": "p/handler.go::Handle"})

	imps, _ := out["importing_files"].([]any)
	require.Len(t, imps, 1)
	assert.Equal(t, "p/importer.go", imps[0].(string))
}

func TestCheckReferences_ExcludeTests(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{
		"symbol_id":     "p/handler.go::Handle",
		"exclude_tests": true,
	})

	byKind := out["by_kind"].(map[string]any)
	// The TestRoute reference (in router_test.go) is dropped.
	_, hasRefs := byKind["references"]
	assert.False(t, hasRefs, "test-file references dropped under exclude_tests")
	assert.EqualValues(t, 1, out["total_references"].(float64))
}

func TestCheckReferences_NameOnly(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{"name": "Handle"})

	// No symbol_id → no in-edge walk; only same-name + (no) importing files.
	assert.EqualValues(t, 0, out["total_references"].(float64))
	sn, _ := out["same_name_elsewhere"].([]any)
	assert.GreaterOrEqual(t, len(sn), 2, "name-only scan finds both Handle nodes")
}

func TestCheckReferences_RejectsEmptyArgs(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{})
	assert.True(t, out["is_error"] == true)
}

func TestCheckReferences_UnknownSymbolGracefullyHandled(t *testing.T) {
	s := newCheckRefsTestServer(t)
	out := callCheckRefs(t, s, map[string]any{"symbol_id": "does/not/exist::Z"})
	// Unknown symbol id: total_references=0, same_name lookup runs
	// only if name was supplied. Verdict: not referenced.
	assert.Equal(t, false, out["referenced"])
}

func TestCheckReferences_EvidenceLimit(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "t::T", Name: "T", Kind: graph.KindFunction, FilePath: "t.go"})
	// 20 callers.
	for i := range 20 {
		id := "c" + string(rune('A'+i))
		g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: id + ".go"})
		g.AddEdge(&graph.Edge{From: id, To: "t::T", Kind: graph.EdgeCalls})
	}
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	out := callCheckRefs(t, s, map[string]any{
		"symbol_id":      "t::T",
		"evidence_limit": 5,
	})
	ev, _ := out["evidence"].([]any)
	assert.Len(t, ev, 5)
	assert.EqualValues(t, 20, out["total_references"].(float64), "total_references counts every edge, not just evidence rows")
}

func TestCheckReferences_MinTierFilter(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "t::T", Name: "T", Kind: graph.KindFunction, FilePath: "t.go"})
	g.AddNode(&graph.Node{ID: "c1::C", Name: "C", Kind: graph.KindFunction, FilePath: "c.go"})
	g.AddEdge(&graph.Edge{From: "c1::C", To: "t::T", Kind: graph.EdgeCalls, Origin: graph.OriginTextMatched})
	g.AddNode(&graph.Node{ID: "c2::D", Name: "D", Kind: graph.KindFunction, FilePath: "d.go"})
	g.AddEdge(&graph.Edge{From: "c2::D", To: "t::T", Kind: graph.EdgeCalls, Origin: graph.OriginLSPResolved})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	out := callCheckRefs(t, s, map[string]any{
		"symbol_id": "t::T",
		"min_tier":  "lsp_resolved",
	})
	// Only the lsp_resolved edge survives the tier filter.
	assert.EqualValues(t, 1, out["total_references"].(float64))
}

func TestAtOrAboveTier(t *testing.T) {
	assert.True(t, atOrAboveTier("lsp_resolved", "ast_resolved"))
	assert.True(t, atOrAboveTier("lsp_resolved", "lsp_resolved"))
	assert.False(t, atOrAboveTier("text_matched", "ast_resolved"))
	assert.False(t, atOrAboveTier("ast_inferred", "lsp_resolved"))
	// Empty actual treated as text_matched.
	assert.False(t, atOrAboveTier("", "lsp_resolved"))
	assert.True(t, atOrAboveTier("", "text_matched"))
}
