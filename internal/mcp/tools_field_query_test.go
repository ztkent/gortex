package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// searchSymbolsResp runs search_symbols and decodes the JSON response.
func searchSymbolsResp(t *testing.T, srv *Server, query string) map[string]any {
	t.Helper()
	res := callTool(t, srv, "search_symbols", map[string]any{"query": query})
	require.Falsef(t, res.IsError, "search %q errored", query)
	text := res.Content[0].(mcplib.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	return resp
}

func resultIDs(resp map[string]any) []string {
	raw, _ := resp["results"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// TestSearchSymbols_FieldQualified drives the field-qualified query
// syntax and the fuzzy fallback through the real search_symbols
// handler against the setupTestServer fixture (a Go main.go with a
// Config type plus main / helper functions).
func TestSearchSymbols_FieldQualified(t *testing.T) {
	srv, _ := setupTestServer(t)

	// kind: clause narrows to type nodes — the Config struct surfaces.
	resp := searchSymbolsResp(t, srv, "kind:type Config")
	foundConfig := false
	for _, id := range resultIDs(resp) {
		if strings.Contains(id, "Config") {
			foundConfig = true
		}
	}
	require.True(t, foundConfig, "kind:type Config should surface the Config type")
	require.Nil(t, resp["filters_relaxed"], "an exact field match must not relax filters")

	// lang: clause — the whole fixture is Go, so lang:go keeps results.
	resp = searchSymbolsResp(t, srv, "lang:go helper")
	require.NotEmpty(t, resultIDs(resp), "lang:go helper should return results")

	// Fuzzy fallback: there is no interface in the fixture, so
	// kind:interface filters everything out; the query then retries on
	// the free text alone and flags the relaxation.
	resp = searchSymbolsResp(t, srv, "kind:interface Config")
	require.Equal(t, true, resp["filters_relaxed"],
		"an unsatisfiable field clause must trigger the fuzzy fallback")
	require.NotEmpty(t, resultIDs(resp), "fallback should still surface Config")
}
