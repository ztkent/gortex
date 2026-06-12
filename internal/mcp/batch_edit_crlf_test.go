package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeBatchEditResults(t *testing.T, res *mcplib.CallToolResult) []map[string]any {
	t.Helper()
	require.NotEmpty(t, res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	raw, ok := resp["results"].([]any)
	require.True(t, ok, "results array expected: %v", resp)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func TestBatchEdit_FileOpCRLF(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "win.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("alpha\r\nbeta\r\ngamma\r\n"), 0o644))

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_file",
			"path":       target,
			"old_string": "beta\ngamma",
			"new_string": "BETA\nGAMMA",
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "applied", results[0]["status"])
	assert.Equal(t, true, results[0]["eol_normalized"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "alpha\r\nBETA\r\nGAMMA\r\n", string(got))
	assertUniformCRLF(t, string(got))
}

func TestBatchEdit_FileOpCRLFReplaceAll(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "win2.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("TODO x\r\nkeep\r\nTODO x\r\n"), 0o644))

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":          "edit_file",
			"path":        target,
			"old_string":  "TODO x\n",
			"new_string":  "DONE x\n",
			"replace_all": true,
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "applied", results[0]["status"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "DONE x\r\nkeep\r\nDONE x\r\n", string(got))
	assertUniformCRLF(t, string(got))
}

func TestBatchEdit_FileOpCRLFAmbiguousRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "win3.md")
	original := "TODO x\r\nkeep\r\nTODO x\r\n"
	require.NoError(t, os.WriteFile(target, []byte(original), 0o644))

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_file",
			"path":       target,
			"old_string": "TODO x\n",
			"new_string": "DONE x\n",
		},
	}})
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "failed", results[0]["status"])
	assert.Contains(t, results[0]["error"], "matches 2 locations")
	assert.Contains(t, results[0]["error"], "first match lines 1, 3")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "file must be untouched")
}

func TestBatchEdit_SymbolOpCRLF(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "crlf.go::CrlfTarget",
			"old_source": "\ta := 1\n\tb := 2",
			"new_source": "\ta := 10\n\tb := 20",
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "applied", results[0]["status"])
	assert.Equal(t, true, results[0]["eol_normalized"])

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "\ta := 10\r\n\tb := 20\r\n",
		"new_source must be written with CRLF endings")
	assert.Contains(t, string(got), "func crlfSentinel() {\r\n\tc := 3\r\n",
		"bytes after the edited symbol must be untouched — offset arithmetic")
	assertUniformCRLF(t, string(got))
}

func TestBatchEdit_SymbolOpCRLFExactStillPreferred(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "crlf.go::CrlfTarget",
			"old_source": "\ta := 1\r\n\tb := 2",
			"new_source": "\ta := 10\r\n\tb := 20",
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "applied", results[0]["status"])
	assert.Nil(t, results[0]["eol_normalized"], "byte-exact match must not report eol_normalized")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "\ta := 10\r\n\tb := 20\r\n")
}

func TestBatchEdit_SymbolOpCRLFDocCommentIncludedInFragment(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	// Parity with edit_symbol: the batch op must also expand the search
	// window over the preceding doc comment, EOL-tolerantly.
	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "crlf.go::CrlfTarget",
			"old_source": "// CrlfTarget exercises CRLF-file symbol editing.\nfunc CrlfTarget() {",
			"new_source": "// CrlfTarget exercises CRLF-file symbol editing (edited).\nfunc CrlfTarget() {",
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "applied", results[0]["status"], "error: %v", results[0]["error"])

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got),
		"// CrlfTarget exercises CRLF-file symbol editing (edited).\r\nfunc CrlfTarget() {\r\n")
	assertUniformCRLF(t, string(got))
}

func TestBatchEdit_SymbolOpIdenticalSourcesRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "crlf.go::CrlfTarget",
			"old_source": "\ta := 1\r\n\tb := 2",
			"new_source": "\ta := 1\r\n\tb := 2",
		},
	}})
	results := decodeBatchEditResults(t, res)
	require.Len(t, results, 1)
	assert.Equal(t, "failed", results[0]["status"])
	assert.Contains(t, results[0]["error"], "identical")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "no silent no-op write")
}

func TestBatchEdit_MixedOpsCRLF(t *testing.T) {
	srv, dir := setupTestServer(t)
	goPath := writeCRLFGoFixture(t, srv, dir)
	mdPath := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(mdPath,
		[]byte("intro\r\nbody text\r\n"), 0o644))

	res := callBatchEdit(t, srv, map[string]any{"edits": []any{
		map[string]any{
			"op":         "edit_symbol",
			"id":         "crlf.go::CrlfTarget",
			"old_source": "\t_ = a\n\t_ = b",
			"new_source": "\t_, _ = a, b",
		},
		map[string]any{
			"op":         "edit_file",
			"path":       mdPath,
			"old_string": "intro\nbody",
			"new_string": "INTRO\nBODY",
		},
	}})
	require.False(t, res.IsError, "%v", res.Content)

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	summary := resp["summary"].(map[string]any)
	require.EqualValues(t, 2, summary["applied"])
	require.EqualValues(t, 0, summary["failed"])

	gotGo, err := os.ReadFile(goPath)
	require.NoError(t, err)
	assert.Contains(t, string(gotGo), "\t_, _ = a, b\r\n")
	assertUniformCRLF(t, string(gotGo))

	gotMd, err := os.ReadFile(mdPath)
	require.NoError(t, err)
	assert.Equal(t, "INTRO\r\nBODY text\r\n", string(gotMd))
	assertUniformCRLF(t, string(gotMd))
}
