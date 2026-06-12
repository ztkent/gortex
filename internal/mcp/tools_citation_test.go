package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// newCitationTestServer builds a bare Server suitable for verify_citation
// — the handler only needs resolveFilePath (which works for absolute
// paths without an indexer) plus the session machinery.
func newCitationTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

// initTestRepo creates a git repo at dir with one committed file
// (path, contents). Returns the HEAD sha.
func initTestRepo(t *testing.T, dir, file, contents string) string {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	full := filepath.Join(dir, file)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(contents), 0o644))

	for _, args := range [][]string{
		{"add", file},
		{"commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	sha, err := runGit(context.Background(), dir, "rev-parse", "HEAD")
	require.NoError(t, err)
	return strings.TrimSpace(sha)
}

func callCitationHandler(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleVerifyCitation(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true, "content": res.Content}
	}
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestVerifyCitation_HappyPath(t *testing.T) {
	dir := t.TempDir()
	sha := initTestRepo(t, dir, "foo.go", "package foo\n\nfunc Bar() int {\n\treturn 42\n}\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "return 42",
		"file_path": filepath.Join(dir, "foo.go"),
		"sha":       "HEAD",
	})

	assert.Equal(t, true, out["verified"])
	assert.EqualValues(t, 1, out["match_count"].(float64))
	assert.EqualValues(t, 4, out["span_first_line"].(float64))
	assert.Equal(t, sha, out["sha_resolved"])
	_, hasErr := out["error"]
	assert.False(t, hasErr)
}

func TestVerifyCitation_SpanMissing(t *testing.T) {
	dir := t.TempDir()
	_ = initTestRepo(t, dir, "foo.go", "package foo\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "this string is not in the file",
		"file_path": filepath.Join(dir, "foo.go"),
	})

	assert.Equal(t, false, out["verified"])
	assert.EqualValues(t, 0, out["match_count"].(float64))
	assert.EqualValues(t, 0, out["span_first_line"].(float64))
}

func TestVerifyCitation_AbbreviatedSHA(t *testing.T) {
	dir := t.TempDir()
	sha := initTestRepo(t, dir, "foo.go", "package foo\n\nfunc Bar() {}\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "func Bar()",
		"file_path": filepath.Join(dir, "foo.go"),
		"sha":       sha[:8], // abbreviated
	})

	assert.Equal(t, true, out["verified"])
	assert.Equal(t, sha, out["sha_resolved"], "rev-parse expands the abbreviated sha to full hex")
}

func TestVerifyCitation_BadSHA(t *testing.T) {
	dir := t.TempDir()
	_ = initTestRepo(t, dir, "foo.go", "package foo\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "package foo",
		"file_path": filepath.Join(dir, "foo.go"),
		"sha":       "deadbeef1234567890",
	})

	assert.Equal(t, false, out["verified"])
	errMsg, _ := out["error"].(string)
	assert.Contains(t, errMsg, "rev-parse")
}

func TestVerifyCitation_NotInGitTree(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "loose.go")
	require.NoError(t, os.WriteFile(full, []byte("hi\n"), 0o644))

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "hi",
		"file_path": full,
	})

	assert.Equal(t, false, out["verified"])
	errMsg, _ := out["error"].(string)
	assert.Contains(t, errMsg, "git working tree")
}

func TestVerifyCitation_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	_ = initTestRepo(t, dir, "foo.go", "package foo\n\n// TODO: x\n// TODO: y\n// TODO: z\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "TODO",
		"file_path": filepath.Join(dir, "foo.go"),
	})

	assert.Equal(t, true, out["verified"])
	assert.EqualValues(t, 3, out["match_count"].(float64))
	assert.EqualValues(t, 3, out["span_first_line"].(float64))
}

func TestVerifyCitation_DefaultsToHEAD(t *testing.T) {
	dir := t.TempDir()
	sha := initTestRepo(t, dir, "foo.go", "package foo\n")

	s := newCitationTestServer(t)
	out := callCitationHandler(t, s, map[string]any{
		"span":      "package foo",
		"file_path": filepath.Join(dir, "foo.go"),
		// sha omitted — default HEAD
	})

	assert.Equal(t, true, out["verified"])
	assert.Equal(t, sha, out["sha_resolved"])
}

func TestVerifyCitation_RejectsEmptyArgs(t *testing.T) {
	s := newCitationTestServer(t)

	out1 := callCitationHandler(t, s, map[string]any{
		"file_path": "/tmp/x.go",
	})
	assert.True(t, out1["is_error"] == true, "empty span returns error result")

	out2 := callCitationHandler(t, s, map[string]any{
		"span": "x",
	})
	assert.True(t, out2["is_error"] == true, "empty file_path returns error result")
}

func TestVerifyCitation_WhitespaceSignificant(t *testing.T) {
	dir := t.TempDir()
	_ = initTestRepo(t, dir, "foo.go", "package foo\n\nfunc Bar() {\n\treturn\n}\n")

	s := newCitationTestServer(t)
	// Tab-indented `return` exists. Spaces-indented should not match.
	out := callCitationHandler(t, s, map[string]any{
		"span":      "    return",
		"file_path": filepath.Join(dir, "foo.go"),
	})
	assert.Equal(t, false, out["verified"], "whitespace-significant match rejects spaces vs tabs")
}

// substringMatchInfo unit test — keeps the predicate honest without
// shelling out to git.
func TestSubstringMatchInfo(t *testing.T) {
	cases := []struct {
		haystack, needle string
		wantFirst, wantN int
	}{
		{"abc\ndef\nghi", "def", 2, 1},
		{"abc abc abc", "abc", 1, 3},
		{"line1\nline2\nline3", "line2\nline3", 2, 1},
		{"hello", "world", 0, 0},
		{"x", "", 0, 0},
		{"", "x", 0, 0},
	}
	for _, c := range cases {
		first, n := substringMatchInfo(c.haystack, c.needle)
		assert.Equal(t, c.wantFirst, first, "first line for needle=%q in %q", c.needle, c.haystack)
		assert.Equal(t, c.wantN, n, "match count for needle=%q in %q", c.needle, c.haystack)
	}
}
