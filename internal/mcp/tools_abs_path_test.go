package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestResults_CarryAbsoluteFilePath verifies symbol-listing tools surface
// an absolute_file_path next to the repo-relative file_path, so an editor
// or agent can open a result without reconstructing the path from
// repo_prefix + file_path.
func TestResults_CarryAbsoluteFilePath(t *testing.T) {
	srv, _ := setupTestServer(t)

	assertAbs := func(t *testing.T, m map[string]any) {
		t.Helper()
		rel, _ := m["file_path"].(string)
		require.NotEmpty(t, rel, "result must keep the repo-relative file_path")
		abs, _ := m["absolute_file_path"].(string)
		require.NotEmpty(t, abs, "result must carry absolute_file_path")
		require.Truef(t, filepath.IsAbs(abs), "absolute_file_path %q must be absolute", abs)
		info, err := os.Stat(abs)
		require.NoErrorf(t, err, "absolute_file_path %q must point to a real file", abs)
		require.Falsef(t, info.IsDir(), "absolute_file_path %q must be a file, not a directory", abs)
	}

	// search_symbols — the listing projection built from Node.Brief.
	resp := searchSymbolsResp(t, srv, "Config")
	results, _ := resp["results"].([]any)
	require.NotEmpty(t, results, "Config should surface at least one result")
	for _, r := range results {
		m, ok := r.(map[string]any)
		require.True(t, ok)
		assertAbs(t, m)
	}

	// get_symbol (brief detail) — a single Brief map for one symbol.
	id, _ := results[0].(map[string]any)["id"].(string)
	require.NotEmpty(t, id)
	res := callTool(t, srv, "get_symbol", map[string]any{"id": id})
	require.False(t, res.IsError, "get_symbol must not error")
	var sym map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &sym))
	assertAbs(t, sym)
}
