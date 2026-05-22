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

func TestIsGeneratedFile(t *testing.T) {
	gen := []string{
		"api.pb.go", "schema_pb2.py", "model_gen.go", "x.gen.go",
		"zz_generated.deepcopy.go", "mock_service.go", "service_mock.go",
		"user.g.dart",
	}
	for _, p := range gen {
		if !isGeneratedFile(p) {
			t.Errorf("isGeneratedFile(%q) = false, want true", p)
		}
	}
	notGen := []string{"service.go", "main.go", "handler.py", "generator.go", "gen.go"}
	for _, p := range notGen {
		if isGeneratedFile(p) {
			t.Errorf("isGeneratedFile(%q) = true, want false", p)
		}
	}
}

func TestLooksBinary(t *testing.T) {
	if !looksBinary([]byte("text\x00more")) {
		t.Error("a NUL byte must read as binary")
	}
	if looksBinary([]byte("plain text\nsecond line\n")) {
		t.Error("plain UTF-8 text must not read as binary")
	}
	if looksBinary(nil) {
		t.Error("empty content must not read as binary")
	}
}

func TestPathOmissions(t *testing.T) {
	v := pathOmissions("vendor/github.com/x/y.go")
	require.Len(t, v, 1)
	assert.Equal(t, "vendored", v[0]["kind"])

	g := pathOmissions("internal/api.pb.go")
	require.Len(t, g, 1)
	assert.Equal(t, "generated", g[0]["kind"])

	assert.Empty(t, pathOmissions("internal/auth/token.go"))
}

func TestOmissionKindsCSV(t *testing.T) {
	notes := []map[string]any{omission("compressed", "x"), omission("vendored", "y")}
	assert.Equal(t, "compressed,vendored", omissionKindsCSV(notes))
	assert.Equal(t, "", omissionKindsCSV(nil))
}

// omissionKindSet pulls the omission `kind` tokens out of a tool result.
func omissionKindSet(t *testing.T, m map[string]any) map[string]bool {
	t.Helper()
	raw, ok := m["omissions"].([]any)
	require.Truef(t, ok, "response must carry an omissions list, got: %v", m)
	kinds := map[string]bool{}
	for _, r := range raw {
		e, ok := r.(map[string]any)
		require.True(t, ok, "each omission must be an object")
		k, _ := e["kind"].(string)
		detail, _ := e["detail"].(string)
		assert.NotEmpty(t, k, "omission kind must be set")
		assert.NotEmpty(t, detail, "omission detail must be set")
		kinds[k] = true
	}
	return kinds
}

func TestReadFile_OmissionBinary(t *testing.T) {
	srv, dir := setupCompressTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"),
		[]byte{0x00, 0x01, 0x02, 0x00, 0xff, 0x10}, 0o644))
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{"path": "blob.bin"}))
	assert.Contains(t, omissionKindSet(t, m), "binary")
}

func TestReadFile_OmissionVendoredAndGenerated(t *testing.T) {
	srv, dir := setupCompressTestServer(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "vendor", "ext"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "vendor", "ext", "lib.go"),
		[]byte("package ext\n\nfunc Helper() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schema.pb.go"),
		[]byte("package big\n\nfunc Marshal() {}\n"), 0o644))

	v := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{"path": "vendor/ext/lib.go"}))
	assert.Contains(t, omissionKindSet(t, v), "vendored")

	g := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{"path": "schema.pb.go"}))
	assert.Contains(t, omissionKindSet(t, g), "generated")
}

func TestReadFile_OmissionCompressed(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
	}))
	assert.Contains(t, omissionKindSet(t, m), "compressed")
}

func TestReadFile_OmissionTruncated(t *testing.T) {
	srv, dir := setupCompressTestServer(t)
	var b strings.Builder
	b.WriteString("package big\n\nfunc Long(n int) int {\n\tx := 0\n")
	for i := 0; i < 40; i++ {
		b.WriteString("\tx = x + ")
		b.WriteString(itoaInt(i))
		b.WriteString("\n")
	}
	b.WriteString("\treturn x\n}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "long.go"), []byte(b.String()), 0o644))

	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":      "long.go",
		"max_lines": float64(15),
	}))
	assert.Contains(t, omissionKindSet(t, m), "truncated")
}

func TestGetEditingContext_OmissionCompressed(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
	}))
	assert.Contains(t, omissionKindSet(t, m), "compressed")
}

func TestGetSymbolSource_OmissionCompressed(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	var symbolID string
	for _, n := range srv.graph.AllNodes() {
		if n.Name == "ValidateToken" && (n.Kind == "function" || n.Kind == "method") {
			symbolID = n.ID
			break
		}
	}
	require.NotEmpty(t, symbolID)
	m := extractTextResult(t, callTool(t, srv, "get_symbol_source", map[string]any{
		"id":              symbolID,
		"compress_bodies": true,
	}))
	assert.Contains(t, omissionKindSet(t, m), "compressed")
}

func TestGetSymbolSource_OmissionGCX(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	var symbolID string
	for _, n := range srv.graph.AllNodes() {
		if n.Name == "ValidateToken" && (n.Kind == "function" || n.Kind == "method") {
			symbolID = n.ID
			break
		}
	}
	require.NotEmpty(t, symbolID)
	r := callTool(t, srv, "get_symbol_source", map[string]any{
		"id":              symbolID,
		"compress_bodies": true,
		"format":          "gcx",
	})
	require.False(t, r.IsError, "unexpected error: %+v", r.Content)
	tc, ok := r.Content[0].(mcplib.TextContent)
	require.True(t, ok, "expected TextContent, got %T", r.Content[0])
	assert.Contains(t, tc.Text, "omissions=",
		"GCX output must carry the omission kinds in its header")
}
