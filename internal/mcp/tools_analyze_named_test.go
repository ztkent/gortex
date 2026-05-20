package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func callAnalyzeNamed(t *testing.T, srv *Server, args map[string]any) (map[string]any, bool) {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyzeNamed(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return nil, true
	}
	text := res.Content[0].(mcplib.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "json: %s", text)
	return out, false
}

func TestAnalyzeNamed_ListsBuiltinBundles(t *testing.T) {
	srv, _ := setupTestServer(t)
	out, isErr := callAnalyzeNamed(t, srv, map[string]any{})
	require.False(t, isErr)

	queries, _ := out["queries"].([]any)
	require.GreaterOrEqual(t, len(queries), 10, "the ten built-in bundles must be listed")

	names := map[string]bool{}
	for _, q := range queries {
		row, _ := q.(map[string]any)
		names[row["name"].(string)] = true
	}
	for _, want := range []string{"sql-injection", "hardcoded-secrets", "weak-crypto", "xss", "debug-leftovers"} {
		require.True(t, names[want], "built-in bundle %q missing from the listing", want)
	}
}

func TestAnalyzeNamed_BuiltinBundleSelectsDetectors(t *testing.T) {
	// The shipped SAST library carries the tags the built-in bundles
	// select on — every built-in must resolve to a non-empty set.
	for _, q := range builtinNamedQueries() {
		dets := selectBundleDetectors(q)
		require.NotEmptyf(t, dets, "built-in bundle %q selects no detectors", q.Name)
	}
}

func TestAnalyzeNamed_UnknownQuery(t *testing.T) {
	srv, _ := setupTestServer(t)
	_, isErr := callAnalyzeNamed(t, srv, map[string]any{"name": "no-such-bundle"})
	require.True(t, isErr, "an unknown bundle name must return an error")
}

func TestAnalyzeNamed_ConfigBundleRunsNamedDetector(t *testing.T) {
	srv, _ := setupTestServer(t)
	// A config bundle selecting one detector by exact name.
	srv.SetNamedQueries([]config.NamedQuery{
		{Name: "rust-unwrap", Description: "rust unwrap calls", Detectors: []string{"unsafe-rust-unwrap"}},
	})
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run(s: &str) {
    let _n: i32 = s.parse().unwrap();
}
`)

	out, isErr := callAnalyzeNamed(t, srv, map[string]any{"name": "rust-unwrap"})
	require.False(t, isErr)
	require.Equal(t, "rust-unwrap", out["query"])

	total, _ := out["total"].(float64)
	require.GreaterOrEqual(t, total, float64(1), "expected the unwrap call to match: %v", out)
}

func TestAnalyzeNamed_ConfigBundleOverridesBuiltin(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.SetNamedQueries([]config.NamedQuery{
		{Name: "my-bundle", Description: "custom", Tags: []string{"crypto"}},
	})
	resolved := srv.resolvedNamedQueries()
	require.Contains(t, resolved, "my-bundle")
	require.Contains(t, resolved, "sql-injection") // built-ins still present
}

func TestAnalyzeNamed_DispatchedViaAnalyze(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "named"}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "dispatcher must route kind=named without error")
}
