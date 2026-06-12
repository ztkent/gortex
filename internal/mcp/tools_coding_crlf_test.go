package mcp

import (
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCRLFGoFixture writes a CRLF-terminated Go file into the test repo,
// indexes it, and returns its absolute path. The file defines CrlfTarget
// (with a doc comment) followed by a sentinel function whose bytes must
// survive any edit to CrlfTarget untouched.
func writeCRLFGoFixture(t *testing.T, srv *Server, dir string) string {
	t.Helper()
	src := "package main\r\n" +
		"\r\n" +
		"// CrlfTarget exercises CRLF-file symbol editing.\r\n" +
		"func CrlfTarget() {\r\n" +
		"\ta := 1\r\n" +
		"\tb := 2\r\n" +
		"\t_ = a\r\n" +
		"\t_ = b\r\n" +
		"}\r\n" +
		"\r\n" +
		"func crlfSentinel() {\r\n" +
		"\tc := 3\r\n" +
		"\t_ = c\r\n" +
		"}\r\n"
	path := filepath.Join(dir, "crlf.go")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	require.NoError(t, srv.indexer.IndexFile(path))
	return path
}

func TestEditSymbol_CRLFFileLFOldSource(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "\ta := 1\n\tb := 2",
		"new_source": "\ta := 10\n\tb := 20",
	})
	require.False(t, result.IsError, "LF old_source must match the CRLF file: %v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	assert.Equal(t, true, resp["eol_normalized"])

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "\ta := 10\r\n\tb := 20\r\n",
		"new_source must be written with CRLF endings")
	assert.Contains(t, string(got), "func crlfSentinel() {\r\n\tc := 3\r\n",
		"bytes after the edited symbol must be untouched — offset arithmetic")
	assert.Contains(t, string(got), "// CrlfTarget exercises CRLF-file symbol editing.\r\n",
		"bytes before the edited symbol must be untouched")
	assertUniformCRLF(t, string(got))
}

func TestEditSymbol_CRLFExactMatchStillPreferred(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "\ta := 1\r\n\tb := 2",
		"new_source": "\ta := 10\r\n\tb := 20",
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	assert.Nil(t, resp["eol_normalized"], "byte-exact match must not report eol_normalized")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "\ta := 10\r\n\tb := 20\r\n")
	assertUniformCRLF(t, string(got))
}

func TestEditSymbol_CRLFUniquenessStillEnforced(t *testing.T) {
	srv, dir := setupTestServer(t)
	writeCRLFGoFixture(t, srv, dir)

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "\t_ = ",
		"new_source": "\t_, _ = 0, ",
	})
	require.True(t, result.IsError, "ambiguous fragment within the symbol must be rejected")
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "appears multiple times")
}

func TestEditSymbol_CRLFIdenticalAfterNormalizationRejected(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "\ta := 1\n\tb := 2",
		"new_source": "\ta := 1\r\n\tb := 2",
	})
	require.True(t, result.IsError, "a no-op after EOL adaptation must be rejected")
	text := result.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "identical after line-ending normalization")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "file must be untouched")
}

func TestEditSymbol_CRLFDocCommentIncludedInFragment(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)

	// Agents often paste the doc comment along with the signature — the
	// doc-comment expansion path must stay EOL-tolerant too.
	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "// CrlfTarget exercises CRLF-file symbol editing.\nfunc CrlfTarget() {",
		"new_source": "// CrlfTarget exercises CRLF-file symbol editing (edited).\nfunc CrlfTarget() {",
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got),
		"// CrlfTarget exercises CRLF-file symbol editing (edited).\r\nfunc CrlfTarget() {\r\n")
	assertUniformCRLF(t, string(got))
}

func TestEditSymbol_CRLFDryRunReportsNormalized(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := writeCRLFGoFixture(t, srv, dir)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "crlf.go::CrlfTarget",
		"old_source": "\ta := 1\n\tb := 2",
		"new_source": "\ta := 10\n\tb := 20",
		"dry_run":    true,
	})
	require.False(t, result.IsError, "%v", result.Content)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_apply", resp["status"])
	assert.Equal(t, true, resp["eol_normalized"])

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "dry_run must not write")
}
