package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/lint"
)

func goLinterAvailable(t *testing.T) {
	t.Helper()
	ls := lint.NewRegistry().ForLanguage("go")
	if len(ls) == 0 || !ls[0].Available() {
		t.Skip("gofmt not on PATH")
	}
}

func TestLintFileReportsGoSyntaxError(t *testing.T) {
	goLinterAvailable(t)
	srv, dir := setupTestServer(t)
	broken := filepath.Join(dir, "broken.go")
	require.NoError(t, os.WriteFile(broken, []byte("package main\n\nfunc main() {\n"), 0o644))

	resp := callEditHandlerJSON(t, srv.handleLintFile, map[string]any{"path": broken})
	require.Equal(t, false, resp["healthy"])
	require.NotEmpty(t, resp["diagnostics"].([]any))
	require.Contains(t, resp["linters_ran"], "gofmt")
}

func TestLintFileCleanGo(t *testing.T) {
	goLinterAvailable(t)
	srv, dir := setupTestServer(t)
	clean := filepath.Join(dir, "clean.go")
	require.NoError(t, os.WriteFile(clean, []byte("package main\n\nfunc f() {}\n"), 0o644))

	resp := callEditHandlerJSON(t, srv.handleLintFile, map[string]any{"path": clean})
	require.Equal(t, true, resp["healthy"])
	require.Equal(t, "go", resp["language"])
}

func TestLintFileUnknownLanguageIsHealthy(t *testing.T) {
	srv, dir := setupTestServer(t)
	txt := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(txt, []byte("hello\n"), 0o644))

	resp := callEditHandlerJSON(t, srv.handleLintFile, map[string]any{"path": txt})
	require.Equal(t, true, resp["healthy"])
	require.Equal(t, "", resp["language"])
}

func TestEditFileSurfacesSyntaxHealthOnBreak(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")

	// Drop helper's closing brace — the file no longer parses.
	resp := callEditHandlerJSON(t, srv.handleEditFile, map[string]any{
		"path":       mainGo,
		"old_string": "func helper() {}",
		"new_string": "func helper() {",
	})
	require.Equal(t, "applied", resp["status"])
	health, ok := resp["syntax_health"].(map[string]any)
	require.True(t, ok, "a broken edit must surface syntax_health")
	require.Equal(t, false, health["healthy"])
}

func TestEditFileCleanEditOmitsSyntaxHealth(t *testing.T) {
	srv, dir := setupTestServer(t)
	mainGo := filepath.Join(dir, "main.go")

	resp := callEditHandlerJSON(t, srv.handleEditFile, map[string]any{
		"path":       mainGo,
		"old_string": "Port int",
		"new_string": "Port int // ok",
	})
	require.Equal(t, "applied", resp["status"])
	_, has := resp["syntax_health"]
	require.False(t, has, "a clean edit must not surface syntax_health")
}
