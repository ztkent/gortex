package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertUniformCRLF fails when content carries any LF that is not part of a
// CRLF pair (i.e. the edit introduced mixed line endings).
func assertUniformCRLF(t *testing.T, content string) {
	t.Helper()
	assert.NotContains(t, strings.ReplaceAll(content, "\r\n", ""), "\n",
		"file must not contain bare-LF terminators after the edit")
}

func TestEditFile_CRLFFileLFOldString(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("alpha\r\nbeta\r\ngamma\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "notes.md",
		"old_string": "beta\ngamma",
		"new_string": "BETA\nGAMMA",
	})
	require.False(t, result.IsError, "LF old_string must match the CRLF file: %v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	assert.Equal(t, float64(1), resp["replacements"])
	assert.Equal(t, true, resp["eol_normalized"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "alpha\r\nBETA\r\nGAMMA\r\n", string(got),
		"new_string must be written with the file's CRLF endings")
	assertUniformCRLF(t, string(got))
}

func TestEditFile_CRLFFileLFOldString_ReplaceAll(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "dup.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("TODO item\r\nkeep\r\nTODO item\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":        "dup.md",
		"old_string":  "TODO item\n",
		"new_string":  "DONE item\n",
		"replace_all": true,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, float64(2), resp["replacements"])
	assert.Equal(t, true, resp["eol_normalized"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "DONE item\r\nkeep\r\nDONE item\r\n", string(got))
	assertUniformCRLF(t, string(got))
}

func TestEditFile_CRLFAmbiguousHintLines(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "many.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("intro\r\nTODO x\r\nTODO x\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "many.md",
		"old_string": "TODO x\n",
		"new_string": "DONE x\n",
	})
	require.True(t, result.IsError, "ambiguous normalized match must be rejected")
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "matches 2 locations")
	assert.Contains(t, text, "first match lines 2, 3",
		"hint must carry correct line numbers for normalized matches")
}

func TestEditFile_LFFileCRLFOldString(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "plain.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("alpha\nbeta\ngamma\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "plain.md",
		"old_string": "beta\r\ngamma",
		"new_string": "BETA\r\nGAMMA",
	})
	require.False(t, result.IsError, "CRLF old_string must match the LF file: %v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, true, resp["eol_normalized"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "alpha\nBETA\nGAMMA\n", string(got),
		"replacement must be rewritten to the file's LF endings")
	assert.NotContains(t, string(got), "\r", "no CR may leak into an LF file")
}

func TestEditFile_CRLFExactMatchStillPreferred(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "exact.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("alpha\r\nbeta\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "exact.md",
		"old_string": "alpha\r\nbeta",
		"new_string": "ALPHA\r\nBETA",
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	assert.Nil(t, resp["eol_normalized"], "byte-exact match must not report eol_normalized")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "ALPHA\r\nBETA\r\n", string(got))
}

func TestEditFile_CRLFSingleLineOldStringMatchesExactly(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "single.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("v0.1 released\r\nv0.2 pending\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "single.md",
		"old_string": "v0.2 pending",
		"new_string": "v0.2 shipped",
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Nil(t, resp["eol_normalized"], "terminator-free fragment matches byte-exactly")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "v0.1 released\r\nv0.2 shipped\r\n", string(got))
	assertUniformCRLF(t, string(got))
}

func TestEditFile_CRLFDryRunReportsNormalized(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "preview.md")
	original := "alpha\r\nbeta\r\n"
	require.NoError(t, os.WriteFile(target, []byte(original), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "preview.md",
		"old_string": "alpha\nbeta",
		"new_string": "ALPHA\nBETA",
		"dry_run":    true,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_apply", resp["status"])
	assert.Equal(t, true, resp["eol_normalized"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "dry_run must not write")
}

func TestEditFile_CRLFIdenticalAfterNormalizationRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "noop.md")
	original := "alpha\r\nbeta\r\n"
	require.NoError(t, os.WriteFile(target, []byte(original), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "noop.md",
		"old_string": "alpha\nbeta",
		"new_string": "alpha\r\nbeta",
	})
	require.True(t, result.IsError, "a no-op after EOL adaptation must be rejected")
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "identical after line-ending normalization")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "file must be untouched")
}

func TestEditFile_CRLFNotFoundStillErrors(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "missing.md")
	require.NoError(t, os.WriteFile(target,
		[]byte("alpha\r\nbeta\r\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "missing.md",
		"old_string": "alpha\nGAMMA",
		"new_string": "x",
	})
	require.True(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "old_string not found in file")
}

func TestEditFile_CRLFBaseSHAGuardUsesRealBytes(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "guarded.md")
	original := []byte("alpha\r\nbeta\r\n")
	require.NoError(t, os.WriteFile(target, original, 0o644))

	// The drift guard must hash the on-disk CRLF bytes, not a normalized
	// copy: the SHA of the real content is accepted...
	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "guarded.md",
		"old_string": "alpha\nbeta",
		"new_string": "ALPHA\nBETA",
		"base_sha":   gitBlobSHA(original),
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])

	// ...and new_sha covers the written CRLF bytes.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, gitBlobSHA(got), resp["new_sha"])
	assertUniformCRLF(t, string(got))
}
