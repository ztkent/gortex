package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/savings"
)

// TestReadFamilyToolsRecordSavings pins the savings recording surface on a
// single-repo server (the issue-67 shape: one tracked repo, unprefixed
// nodes). Every read-family tool — read_file, get_file_summary,
// get_editing_context — and the original get_symbol_source must book an
// observation; before the lone-repo resolution fix none of them could,
// because the record sites sit behind resolveNodePath/resolveFilePath.
func TestReadFamilyToolsRecordSavings(t *testing.T) {
	srv, _, _ := newSingleRepoServer(t)
	ctx := context.Background()

	store, err := savings.Open("")
	require.NoError(t, err)
	srv.InitSavings(store, "")

	calls := func() int64 {
		return srv.tokenStats.snapshot()["calls_counted"].(int64)
	}
	require.Equal(t, int64(0), calls())

	etagOf := func(raw string) string {
		var parsed struct {
			ETag string `json:"etag"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &parsed))
		require.NotEmpty(t, parsed.ETag)
		return parsed.ETag
	}

	res := callToolByName(t, srv, ctx, "read_file", map[string]any{"path": "main.go"})
	require.False(t, res.IsError, "read_file must succeed on a bare-relative path in single-repo mode")
	require.Equal(t, int64(1), calls(), "read_file must record a savings observation")
	readEtag := etagOf(textOfResult(t, res))

	res = callToolByName(t, srv, ctx, "get_file_summary", map[string]any{"path": "main.go"})
	require.False(t, res.IsError)
	require.Equal(t, int64(2), calls(), "get_file_summary must record a savings observation")
	summaryEtag := etagOf(textOfResult(t, res))

	res = callToolByName(t, srv, ctx, "get_editing_context", map[string]any{"path": "main.go"})
	require.False(t, res.IsError)
	require.Equal(t, int64(3), calls(), "get_editing_context must record a savings observation")

	res = callToolByName(t, srv, ctx, "get_symbol_source", map[string]any{"id": "main.go::Hello"})
	require.False(t, res.IsError, "get_symbol_source must resolve unprefixed single-repo nodes")
	require.Equal(t, int64(4), calls(), "get_symbol_source must record a savings observation")

	// Conditional fetches that hit the etag transfer ~nothing and must
	// book nothing — a polling client would otherwise mint fake savings
	// on every poll.
	res = callToolByName(t, srv, ctx, "read_file", map[string]any{"path": "main.go", "if_none_match": readEtag})
	require.False(t, res.IsError)
	require.Equal(t, int64(4), calls(), "not-modified read_file must not record")

	res = callToolByName(t, srv, ctx, "get_file_summary", map[string]any{"path": "main.go", "if_none_match": summaryEtag})
	require.False(t, res.IsError)
	require.Equal(t, int64(4), calls(), "not-modified get_file_summary must not record")

	snap := srv.tokenStats.snapshot()
	require.Greater(t, snap["tokens_returned"].(int64), int64(0))

	// Single-repo events attribute to the lone repo's prefix — the same
	// bucket key multi-repo mode would use — for every recording tool.
	ledger, lerr := store.Snapshot()
	require.NoError(t, lerr)
	require.NotNil(t, ledger.PerRepo["myrepo"], "events must land in the lone repo's per-repo bucket, got %v", ledger.PerRepo)
	require.Equal(t, int64(4), ledger.PerRepo["myrepo"].CallsCounted)
}

// textOfResult extracts the first text content of a tool result.
func textOfResult(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("tool result has no text content")
	return ""
}
