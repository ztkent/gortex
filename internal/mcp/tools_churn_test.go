package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedChurnGraph builds a small graph with two function nodes whose
// meta.churn data the read-side handler is supposed to surface. We
// stamp the metadata directly instead of running the enricher — the
// read path is what's under test here; the enrich pass has its own
// tests in internal/churn.
func seedChurnGraph(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	now := time.Now().UTC()

	g.AddNode(&graph.Node{
		ID:        "foo.go::dead",
		Kind:      graph.KindFunction,
		Name:      "dead",
		FilePath:  "foo.go",
		StartLine: 3, EndLine: 5,
		Meta: map[string]any{
			"churn": map[string]any{
				"commit_count":   1,
				"age_days":       0,
				"churn_rate":     1.0,
				"last_author":    "alice@example.com",
				"last_commit_at": now.Format(time.RFC3339),
			},
		},
	})
	g.AddNode(&graph.Node{
		ID:        "foo.go::live",
		Kind:      graph.KindFunction,
		Name:      "live",
		FilePath:  "foo.go",
		StartLine: 7, EndLine: 9,
		Meta: map[string]any{
			"churn": map[string]any{
				"commit_count":   4,
				"age_days":       2,
				"churn_rate":     2.0,
				"last_author":    "bob@example.com",
				"last_commit_at": now.Format(time.RFC3339),
			},
		},
	})

	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callChurnHandler(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGetChurnRate(context.Background(), req)
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

func TestChurnRate_BothFunctionsSurface(t *testing.T) {
	s := seedChurnGraph(t)
	out := callChurnHandler(t, s, map[string]any{})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 2, "both dead and live should surface")
}

func TestChurnRate_SortByCommitCount(t *testing.T) {
	s := seedChurnGraph(t)
	out := callChurnHandler(t, s, map[string]any{"sort_by": "commit_count"})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 2)

	first := symbols[0].(map[string]any)
	second := symbols[1].(map[string]any)
	assert.Greater(t, int(first["commit_count"].(float64)), int(second["commit_count"].(float64)))
	assert.Equal(t, "live", first["name"], "live has 4 commits, should rank above dead's 1")
}

func TestChurnRate_MinCommitsFilter(t *testing.T) {
	s := seedChurnGraph(t)
	// dead has 1, live has 4 — threshold of 3 keeps only live.
	out := callChurnHandler(t, s, map[string]any{"min_commits": 3})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 1)
	assert.Equal(t, "live", symbols[0].(map[string]any)["name"])
}

func TestChurnRate_LimitTruncates(t *testing.T) {
	s := seedChurnGraph(t)
	out := callChurnHandler(t, s, map[string]any{"limit": 1})
	symbols, _ := out["symbols"].([]any)
	assert.Len(t, symbols, 1)
	assert.Equal(t, true, out["truncated"])
}

func TestChurnRate_PathPrefixFilter(t *testing.T) {
	s := seedChurnGraph(t)
	// Prefix that matches none of the nodes' file paths.
	out := callChurnHandler(t, s, map[string]any{"path_prefix": "/no/such/path"})
	// With no in-scope nodes carrying meta we hit the structured
	// error path — assert the suggestion is present.
	assert.Equal(t, "gortex enrich churn", out["suggestion"])
}

func TestChurnRate_ScannedFilesCount(t *testing.T) {
	s := seedChurnGraph(t)
	out := callChurnHandler(t, s, map[string]any{})
	// One file (foo.go) — scanned once even with two symbols.
	assert.EqualValues(t, 1, out["scanned_files"].(float64))
}

func TestChurnRate_ErrorsWhenNoMeta(t *testing.T) {
	// Graph with a function node but no meta.churn → error response.
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "bar.go::x", Name: "x", Kind: graph.KindFunction,
		FilePath: "bar.go", StartLine: 2, EndLine: 2,
	})
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	out := callChurnHandler(t, s, map[string]any{})
	require.NotEmpty(t, out["error"], "expected structured error when no meta.churn is present")
	assert.Equal(t, "gortex enrich churn", out["suggestion"])
}

func TestChurnRate_SortByOptions(t *testing.T) {
	s := seedChurnGraph(t)
	for _, sortBy := range []string{"churn_rate", "commit_count", "age_days"} {
		out := callChurnHandler(t, s, map[string]any{"sort_by": sortBy})
		assert.Equal(t, sortBy, out["sort_by"], "sort_by echoed")
		symbols, _ := out["symbols"].([]any)
		assert.NotEmpty(t, symbols, "sort_by=%s should still return rows", sortBy)
	}
}

func TestChurnRate_TimestampShape(t *testing.T) {
	s := seedChurnGraph(t)
	out := callChurnHandler(t, s, map[string]any{})
	symbols, _ := out["symbols"].([]any)
	require.NotEmpty(t, symbols)
	row := symbols[0].(map[string]any)
	ts, ok := row["last_commit_at"].(string)
	require.True(t, ok)
	_, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err)
}

func TestChurnRate_TolerantMetaTypes(t *testing.T) {
	// gob → JSON → Go round-trip can widen ints to float64. Verify the
	// projection handles both forms transparently.
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "f.go::a", Name: "a", Kind: graph.KindFunction,
		FilePath: "f.go", StartLine: 1, EndLine: 1,
		Meta: map[string]any{
			"churn": map[string]any{
				"commit_count":   float64(7), // came back from JSON
				"age_days":       int64(3),   // came back from gob int64
				"churn_rate":     float64(2.33),
				"last_author":    "x@y",
				"last_commit_at": "2026-05-01T00:00:00Z",
			},
		},
	})
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	out := callChurnHandler(t, s, map[string]any{})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 1)
	row := symbols[0].(map[string]any)
	assert.EqualValues(t, 7, row["commit_count"])
	assert.EqualValues(t, 3, row["age_days"])
	assert.InDelta(t, 2.33, row["churn_rate"].(float64), 0.001)
}

// TestChurnRate_SidecarReadPath proves the change-A primary path:
// churn populated in the typed sidecar (BulkSetChurn) — with NO
// Meta["churn"] on the nodes — is surfaced by get_churn_rate via the
// ChurnEnrichmentReader index read, not the AllNodes Meta scan.
func TestChurnRate_SidecarReadPath(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "foo.go::a", Kind: graph.KindFunction, Name: "a", FilePath: "foo.go", StartLine: 1, EndLine: 2})
	g.AddNode(&graph.Node{ID: "foo.go::b", Kind: graph.KindFunction, Name: "b", FilePath: "foo.go", StartLine: 3, EndLine: 4})
	require.NoError(t, g.BulkSetChurn("", []graph.ChurnEnrichment{
		{NodeID: "foo.go::a", CommitCount: 7, ChurnRate: 3.0, LastAuthor: "a@x"},
		{NodeID: "foo.go::b", CommitCount: 2, ChurnRate: 0.5, LastAuthor: "b@x"},
	}))
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	out := callChurnHandler(t, s, map[string]any{"sort_by": "commit_count"})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 2, "both sidecar rows must surface")
	first, _ := symbols[0].(map[string]any)
	assert.Equal(t, "foo.go::a", first["symbol_id"], "sort_by commit_count: a (7) before b (2)")
	assert.EqualValues(t, 7, first["commit_count"])
	assert.Equal(t, "a@x", first["last_author"])

	out2 := callChurnHandler(t, s, map[string]any{"min_commits": 5})
	syms2, _ := out2["symbols"].([]any)
	require.Len(t, syms2, 1, "min_commits=5 keeps only a")
}
