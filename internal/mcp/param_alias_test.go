package mcp

import (
	"context"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestReconcileArgKeys_RewritesConfidentAliases(t *testing.T) {
	real := map[string]bool{"id": true, "detail": true, "repo": true}
	cases := []struct {
		name    string
		args    map[string]any
		wantKey string // key that must be present after reconcile
		wantOld string // key that must be gone ("" = no rewrite expected)
	}{
		{"symbol alias for id", map[string]any{"symbol": "x"}, "id", "symbol"},
		{"node_id alias for id", map[string]any{"node_id": "x"}, "id", "node_id"},
		{"typo of detail", map[string]any{"detial": "brief"}, "detail", "detial"},
		{"already-correct key untouched", map[string]any{"id": "x"}, "id", ""},
		{"unknown noise key left alone", map[string]any{"id": "x", "zzz": 1}, "id", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reconcileArgKeys(tc.args, real)
			_, ok := tc.args[tc.wantKey]
			require.Truef(t, ok, "expected key %q present after reconcile", tc.wantKey)
			if tc.wantOld != "" {
				_, gone := tc.args[tc.wantOld]
				require.Falsef(t, gone, "aliased key %q must be removed", tc.wantOld)
			}
		})
	}
}

// TestReconcileArgKeys_KeepsExplicitCanonical confirms an alias never
// overwrites a canonical parameter the caller already supplied.
func TestReconcileArgKeys_KeepsExplicitCanonical(t *testing.T) {
	real := map[string]bool{"query": true}
	args := map[string]any{"query": "real", "search": "alias"}
	reconcileArgKeys(args, real)
	require.Equal(t, "real", args["query"], "an explicit canonical value must not be displaced by an alias")
	require.Contains(t, args, "search", "the alias key stays untouched when the canonical is already set")
}

// TestReconcileArgKeys_PatternAlias confirms `pattern` rewrites to `query`
// on search-side tools that have no real `pattern` parameter, and stays
// inert on tools (search_ast / grep_results / ctx_grep) where `pattern` is
// itself a real parameter.
func TestReconcileArgKeys_PatternAlias(t *testing.T) {
	t.Run("rewrites to query on search tools", func(t *testing.T) {
		// search_symbols / search_text expose `query`, not `pattern`.
		real := map[string]bool{"query": true, "path": true, "kind": true}
		args := map[string]any{"pattern": "validateToken"}
		reconcileArgKeys(args, real)
		require.Equal(t, "validateToken", args["query"], "pattern must alias to query")
		require.NotContains(t, args, "pattern", "the aliased pattern key must be removed")
	})
	t.Run("inert when pattern is a real parameter", func(t *testing.T) {
		// search_ast / grep_results expose a real `pattern` parameter; an
		// agent's `pattern` is valid input and must never be rewritten.
		real := map[string]bool{"pattern": true, "query": true}
		args := map[string]any{"pattern": "(identifier) @match"}
		reconcileArgKeys(args, real)
		require.Equal(t, "(identifier) @match", args["pattern"], "a real pattern parameter must be left untouched")
		require.NotContains(t, args, "query", "no spurious query key must be synthesized")
	})
}

func TestLevenshtein(t *testing.T) {
	require.Equal(t, 0, levenshtein("query", "query"))
	require.Equal(t, 1, levenshtein("quary", "query"))
	require.Equal(t, 1, levenshtein("path", "paths"))
}

// TestReconcileToolParams_EndToEnd drives an aliased argument through the
// real wrapToolHandler chain and confirms the handler accepts it.
func TestReconcileToolParams_EndToEnd(t *testing.T) {
	srv, _ := setupTestServer(t)
	st := srv.MCPServer().GetTool("get_symbol")
	require.NotNil(t, st, "get_symbol must be a live tool")

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol"
	// "symbol" is a hallucinated alias for the real "id" parameter.
	req.Params.Arguments = map[string]any{"symbol": "main.go::helper"}

	res, err := st.Handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Falsef(t, res.IsError, "the aliased 'symbol' argument must be accepted as 'id'")
}
