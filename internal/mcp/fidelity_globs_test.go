package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/elide"
)

func TestParseFidelityGlobs(t *testing.T) {
	rules := parseFidelityGlobs("internal/**:full,*_test.go:omit,vendor/**:compress")
	require.Len(t, rules, 3)
	assert.Equal(t, "internal/**", rules[0].glob)
	assert.Equal(t, elide.FidelityFull, rules[0].fidelity)
	assert.Equal(t, "*_test.go", rules[1].glob)
	assert.Equal(t, elide.FidelityOmit, rules[1].fidelity)
	assert.Equal(t, "vendor/**", rules[2].glob)
	assert.Equal(t, elide.FidelityCompress, rules[2].fidelity)

	// Malformed clauses are skipped, not fatal.
	assert.Empty(t, parseFidelityGlobs(""))
	assert.Empty(t, parseFidelityGlobs("nofidelity"))
	assert.Empty(t, parseFidelityGlobs("glob:bogus"))
	assert.Empty(t, parseFidelityGlobs(":full"))
	mixed := parseFidelityGlobs("good/**:full, ,bad, *.go:omit")
	require.Len(t, mixed, 2, "only the two well-formed clauses survive")
}

func TestMatchFidelityGlob(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		// Trailing /** matches the dir and everything beneath.
		{"internal/**", "internal/foo/bar.go", true},
		{"internal/**", "internal", true},
		{"internal/**", "internalish/x.go", false},
		{"internal/**", "cmd/main.go", false},
		// Basename glob works without a **/ prefix.
		{"*_test.go", "internal/mcp/foo_test.go", true},
		{"*_test.go", "foo_test.go", true},
		{"*_test.go", "foo.go", false},
		// Leading **/ matches any depth.
		{"**/*.go", "a/b/c/x.go", true},
		{"**/testdata/*.json", "a/testdata/fixture.json", true},
		{"**/testdata/*.json", "a/b/testdata/fixture.json", true},
		// Bare ** matches everything.
		{"**", "anything/at/all.rs", true},
		// Bare directory prefix.
		{"vendor", "vendor/x/y.go", true},
		{"vendor", "vendored/x.go", false},
		// Single-segment * never crosses a slash.
		{"internal/*.go", "internal/x.go", true},
		{"internal/*.go", "internal/sub/x.go", false},
	}
	for _, c := range cases {
		got := matchFidelityGlob(c.pattern, c.rel)
		assert.Equalf(t, c.want, got, "matchFidelityGlob(%q, %q)", c.pattern, c.rel)
	}
}

func TestFidelityDecideForPath(t *testing.T) {
	rules := parseFidelityGlobs("internal/**:full,*_test.go:omit")
	// First matching rule wins (order matters).
	dFull := fidelityDecideForPath(rules, "internal/mcp/server.go")
	require.NotNil(t, dFull)
	assert.Equal(t, elide.FidelityFull, dFull(elide.Decl{}))

	dOmit := fidelityDecideForPath(rules, "cmd/foo_test.go")
	require.NotNil(t, dOmit)
	assert.Equal(t, elide.FidelityOmit, dOmit(elide.Decl{}))

	// No matching rule -> nil decider (caller falls back to compress).
	assert.Nil(t, fidelityDecideForPath(rules, "cmd/main.go"))
	assert.Nil(t, fidelityDecideForPath(nil, "anything.go"))
}

// TestReadFile_FidelityGlobsOmit exercises the end-to-end MCP path: a
// fidelity rule that omits every declaration in the matched file
// produces omit markers and drops the bodies, while compress_bodies is
// set so the elide path runs.
func TestReadFile_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "omitted", "omit rule must leave a marker")
	assert.NotContains(t, content, `strings.Split(t, ".")`,
		"omitted declaration body must be gone")
	assert.NotContains(t, content, "func ValidateToken",
		"omitted declaration signature must be gone")
	assert.Equal(t, true, m["bodies_elided"])
}

// TestReadFile_FidelityGlobsFull asserts a `full` rule leaves the file
// uncompressed (body present, no stub) even with compress_bodies set.
func TestReadFile_FidelityGlobsFull(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:full",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, `strings.Split(t, ".")`,
		"a full rule must keep the body verbatim")
	assert.NotContains(t, content, "lines elided",
		"a full rule must not stub any body")
}

// TestReadFile_FidelityGlobsCompressFallback asserts that when no rule
// matches the file, the call falls back to the plain compress_bodies
// behaviour (body stubbed, signature kept).
func TestReadFile_FidelityGlobsCompressFallback(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "vendor/**:omit", // does not match service.go
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "signature kept on compress fallback")
	assert.Contains(t, content, "lines elided", "body stubbed on compress fallback")
	assert.NotContains(t, content, "omitted", "no omit marker when the omit rule does not match")
}

// TestReadFile_FidelityGlobsKeepComposes asserts the per-symbol keep
// predicate overrides an omit rule: the kept symbol survives at full
// source while the rest of the file is omitted.
func TestReadFile_FidelityGlobsKeepComposes(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
		"keep":            "ValidateToken",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "kept symbol survives omit rule")
	assert.Contains(t, content, `strings.Split(t, ".")`, "kept symbol keeps its body")
	assert.Contains(t, content, "omitted", "other declarations still omitted")
}

// TestGetEditingContext_FidelityGlobsOmit asserts the same fidelity_globs
// wiring on get_editing_context's source_compressed view.
func TestGetEditingContext_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	sc, _ := m["source_compressed"].(string)
	require.NotEmpty(t, sc, "source_compressed must be present")
	assert.Contains(t, sc, "omitted", "omit rule must mark declarations")
	assert.NotContains(t, sc, `strings.Split(t, ".")`, "omitted body must be gone")
}
