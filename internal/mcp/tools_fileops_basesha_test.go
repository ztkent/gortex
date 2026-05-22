package mcp

import (
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errorText returns the first text content block of an error result.
// Helper kept local: the drift-error message is the contract, so
// tests assert on the message text and the IsError flag.
func errorText(t *testing.T, result *mcplib.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, result.Content)
	return result.Content[0].(mcplib.TextContent).Text
}

// fileBlobSHA reads a file and returns its git blob SHA — the
// authoritative anchor agents observe at read time. Tests use the
// same primitive the handlers use so a regression in gitBlobSHA
// surfaces as a test failure rather than a silent test-only drift.
func fileBlobSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return gitBlobSHA(data)
}

// --- edit_file -------------------------------------------------------------

func TestEditFile_BaseSHA_Matches_Applies(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target, []byte("hello\n"), 0o644))

	sha := fileBlobSHA(t, target)

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "notes.md",
		"old_string": "hello",
		"new_string": "world",
		"base_sha":   sha,
	})
	assert.False(t, result.IsError, "matching base_sha must not block the write")

	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	// new_sha is required on success and must match the post-write blob SHA.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "world\n", string(got))
	assert.Equal(t, gitBlobSHA(got), resp["new_sha"], "new_sha must be the SHA of the new on-disk content")
}

func TestEditFile_BaseSHA_Stale_RejectsWriteAndPreservesFile(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0o644))

	staleSHA := fileBlobSHA(t, target)

	// A human (or another tool) edits the file between the agent's
	// read and the agent's write. The drift guard must catch this.
	require.NoError(t, os.WriteFile(target, []byte("human edit\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "notes.md",
		"old_string": "human edit",
		"new_string": "agent clobber",
		"base_sha":   staleSHA,
	})
	assert.True(t, result.IsError, "stale base_sha must reject the write")
	assert.Contains(t, errorText(t, result), "base_sha mismatch")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "human edit\n", string(got), "the human's edit must survive — no silent clobber")
}

func TestEditFile_NoBaseSHA_BackwardCompatible(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "notes.md")
	require.NoError(t, os.WriteFile(target, []byte("hello\n"), 0o644))

	// Mutate the file out of band — without base_sha the call must
	// still proceed (legacy behaviour: trust the caller).
	require.NoError(t, os.WriteFile(target, []byte("changed\n"), 0o644))

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "notes.md",
		"old_string": "changed",
		"new_string": "world",
	})
	assert.False(t, result.IsError, "missing base_sha keeps legacy unconditional-write semantics")

	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	// new_sha is still returned on success even when base_sha was not passed,
	// so the caller can pipeline the next edit with the fresh SHA.
	assert.NotEmpty(t, resp["new_sha"])
}

func TestEditFile_BaseSHA_DryRunReturnsNewSHA(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "preview.md")
	require.NoError(t, os.WriteFile(target, []byte("alpha\n"), 0o644))

	sha := fileBlobSHA(t, target)

	result := callTool(t, srv, "edit_file", map[string]any{
		"path":       "preview.md",
		"old_string": "alpha",
		"new_string": "beta",
		"base_sha":   sha,
		"dry_run":    true,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "would_apply", resp["status"])
	require.NotEmpty(t, resp["new_sha"])
	// File is untouched by dry-run.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(got))
}

// --- write_file ------------------------------------------------------------

func TestWriteFile_BaseSHA_Matches_Overwrites(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))

	sha := fileBlobSHA(t, target)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":     "doc.txt",
		"content":  "new",
		"base_sha": sha,
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "overwritten", resp["status"])
	require.NotEmpty(t, resp["new_sha"])

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	assert.Equal(t, gitBlobSHA(got), resp["new_sha"])
}

func TestWriteFile_BaseSHA_Stale_RejectsOverwrite(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(target, []byte("v1"), 0o644))
	staleSHA := fileBlobSHA(t, target)

	// Out-of-band edit between the agent's read and the agent's write.
	require.NoError(t, os.WriteFile(target, []byte("v2-human"), 0o644))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":     "doc.txt",
		"content":  "v2-agent-clobber",
		"base_sha": staleSHA,
	})
	assert.True(t, result.IsError, "stale base_sha must reject overwrite")
	assert.Contains(t, errorText(t, result), "base_sha mismatch")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "v2-human", string(got), "human edit must survive")
}

func TestWriteFile_BaseSHA_OnMissingFile_IsDrift(t *testing.T) {
	srv, _ := setupTestServer(t)

	// Caller observed a SHA on a file that the human has since deleted.
	// Treating this as "create" would silently resurrect the file in a
	// shape the caller never confirmed; refuse instead.
	result := callTool(t, srv, "write_file", map[string]any{
		"path":     "gone.txt",
		"content":  "resurrected",
		"base_sha": "0123456789abcdef0123456789abcdef01234567",
	})
	assert.True(t, result.IsError)
	assert.Contains(t, errorText(t, result), "base_sha mismatch")
}

func TestWriteFile_NoBaseSHA_BackwardCompatible(t *testing.T) {
	srv, dir := setupTestServer(t)
	target := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o644))
	// Mutate out of band.
	require.NoError(t, os.WriteFile(target, []byte("human"), 0o644))

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "doc.txt",
		"content": "new",
	})
	assert.False(t, result.IsError, "no base_sha keeps legacy unconditional-overwrite semantics")
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "overwritten", resp["status"])
	require.NotEmpty(t, resp["new_sha"])
}

func TestWriteFile_BaseSHA_NewFileCreation(t *testing.T) {
	// Creating a brand-new file requires base_sha to be empty —
	// passing one would imply the file already existed at that SHA.
	srv, dir := setupTestServer(t)

	result := callTool(t, srv, "write_file", map[string]any{
		"path":    "fresh.txt",
		"content": "hello",
	})
	assert.False(t, result.IsError)
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "created", resp["status"])
	require.NotEmpty(t, resp["new_sha"])

	got, err := os.ReadFile(filepath.Join(dir, "fresh.txt"))
	require.NoError(t, err)
	assert.Equal(t, gitBlobSHA(got), resp["new_sha"])
}

// --- edit_symbol -----------------------------------------------------------

func TestEditSymbol_BaseSHA_Matches_Applies(t *testing.T) {
	srv, dir := setupTestServer(t)

	mainPath := filepath.Join(dir, "main.go")
	sha := fileBlobSHA(t, mainPath)

	// setupTestServer indexes a `helper` function at the bottom of main.go.
	// Edit its body — empty `{}` → `{ _ = 1 }` — to exercise the drift path
	// on a real symbol the indexer wired up.
	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "main.go::helper",
		"old_source": "func helper() {}",
		"new_source": "func helper() { _ = 1 }",
		"base_sha":   sha,
	})
	assert.False(t, result.IsError, "matching base_sha must not block the symbol edit")
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	require.NotEmpty(t, resp["new_sha"])
	// new_sha must equal the SHA of the file as it now sits on disk.
	assert.Equal(t, fileBlobSHA(t, mainPath), resp["new_sha"])
}

func TestEditSymbol_BaseSHA_Stale_RejectsWriteAndPreservesFile(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainPath := filepath.Join(dir, "main.go")

	originalBytes, err := os.ReadFile(mainPath)
	require.NoError(t, err)
	staleSHA := gitBlobSHA(originalBytes)

	// Out-of-band edit between read and write.
	humanEdited := append([]byte("// human edit\n"), originalBytes...)
	require.NoError(t, os.WriteFile(mainPath, humanEdited, 0o644))

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "main.go::helper",
		"old_source": "func helper() {}",
		"new_source": "func helper() { _ = 1 }",
		"base_sha":   staleSHA,
	})
	assert.True(t, result.IsError, "stale base_sha must reject the symbol edit")
	assert.Contains(t, errorText(t, result), "base_sha mismatch")

	got, err := os.ReadFile(mainPath)
	require.NoError(t, err)
	assert.Equal(t, string(humanEdited), string(got), "human edit must survive on the disk")
}

func TestEditSymbol_NoBaseSHA_BackwardCompatible(t *testing.T) {
	// Legacy behaviour: without base_sha the call is unconditional.
	// We confirm that an edit on an unchanged file still applies and
	// that new_sha rides on the success response so callers can
	// pipeline the next edit even when they did not opt into the
	// drift guard.
	srv, dir := setupTestServer(t)
	mainPath := filepath.Join(dir, "main.go")

	result := callTool(t, srv, "edit_symbol", map[string]any{
		"id":         "main.go::helper",
		"old_source": "func helper() {}",
		"new_source": "func helper() { _ = 1 }",
	})
	assert.False(t, result.IsError, "no base_sha keeps legacy semantics")
	resp := decodeFileOpsResult(t, result)
	assert.Equal(t, "applied", resp["status"])
	require.NotEmpty(t, resp["new_sha"])
	// new_sha must equal the SHA of the file as it now sits on disk —
	// the contract is identical whether or not base_sha was supplied.
	assert.Equal(t, fileBlobSHA(t, mainPath), resp["new_sha"])
}
