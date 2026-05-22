package mcp

import (
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmartContext_PackRootDedup(t *testing.T) {
	srv, _ := setupCompressTestServer(t)

	first := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task": "validate token and parse claims",
	}))
	etag, _ := first["etag"].(string)
	require.NotEmpty(t, etag, "smart_context must return a pack-root etag")

	// Same task, replaying the held pack root → not_modified, no payload.
	deduped := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task":          "validate token and parse claims",
		"if_none_match": etag,
	}))
	assert.Equal(t, true, deduped["not_modified"], "a matching pack root must short-circuit")
	assert.Equal(t, etag, deduped["etag"], "the not_modified response echoes the pack root")
	_, hasSyms := deduped["relevant_symbols"]
	assert.False(t, hasSyms, "a not_modified response must not carry the payload")
}

func TestSmartContext_PackRootMissOnStaleEtag(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task":          "validate token and parse claims",
		"if_none_match": "0000000000000000",
	}))
	assert.NotEqual(t, true, r["not_modified"], "a stale pack root must return the full payload")
	_, hasSyms := r["relevant_symbols"]
	assert.True(t, hasSyms, "a stale pack root must return the payload")
}

func TestSmartContext_PackRootStableAcrossCalls(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	args := map[string]any{"task": "validate token and parse claims", "fidelity": "graded"}
	a := extractTextResult(t, callTool(t, srv, "smart_context", args))
	b := extractTextResult(t, callTool(t, srv, "smart_context", args))
	require.NotEmpty(t, a["etag"])
	assert.Equal(t, a["etag"], b["etag"],
		"identical calls must produce the same pack root")
}

func TestSmartContext_PackRootDedupGraded(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	args := map[string]any{"task": "validate token and parse claims", "fidelity": "graded"}
	first := extractTextResult(t, callTool(t, srv, "smart_context", args))
	etag, _ := first["etag"].(string)
	require.NotEmpty(t, etag)

	replay := map[string]any{"if_none_match": etag}
	for k, v := range args {
		replay[k] = v
	}
	deduped := extractTextResult(t, callTool(t, srv, "smart_context", replay))
	assert.Equal(t, true, deduped["not_modified"], "graded packs must dedup too")
	_, hasMani := deduped["context_manifest"]
	assert.False(t, hasMani, "a not_modified response must not carry the manifest")
}

func TestSmartContext_PackRootGCX(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	r := callTool(t, srv, "smart_context", map[string]any{
		"task":   "validate token and parse claims",
		"format": "gcx",
	})
	require.False(t, r.IsError, "unexpected error: %+v", r.Content)
	require.NotEmpty(t, r.Content)
	tc, ok := r.Content[0].(mcplib.TextContent)
	require.True(t, ok, "expected TextContent, got %T", r.Content[0])
	assert.Contains(t, tc.Text, "etag=",
		"the GCX symbols section must carry the pack-root etag so it can be replayed")
}
