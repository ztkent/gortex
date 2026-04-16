package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooksLikeGlob(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"pkg/foo", false},
		{"pkg/*.go", true},
		{"*.tmp", true},
		{"dir/", false},
		{"!keep", true},
		{"/abs", false}, // absolute paths handled by normalizePattern
		{"abc?def", true},
		{"[0-9]", true},
	}
	for _, tc := range cases {
		if got := looksLikeGlob(tc.in); got != tc.want {
			t.Errorf("looksLikeGlob(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizePattern_RawGlob(t *testing.T) {
	p, err := normalizePattern("*.log", "/some/repo")
	require.NoError(t, err)
	assert.Equal(t, "*.log", p)
}

func TestNormalizePattern_NoRepoRoot(t *testing.T) {
	// Without a repo root to anchor against, the arg is passed through.
	p, err := normalizePattern("something", "")
	require.NoError(t, err)
	assert.Equal(t, "something", p)
}

func TestNormalizePattern_ExistingDir(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	nested := filepath.Join(root, "pkg", "generated")
	require.NoError(t, os.MkdirAll(nested, 0755))

	require.NoError(t, os.Chdir(root))
	p, err := normalizePattern("pkg/generated", root)
	require.NoError(t, err)
	assert.Equal(t, "pkg/generated/", p)
}

func TestNormalizePattern_ExistingFile(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	f := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(f, []byte(""), 0644))

	require.NoError(t, os.Chdir(root))
	p, err := normalizePattern("notes.txt", root)
	require.NoError(t, err)
	assert.Equal(t, "notes.txt", p)
}

func TestNormalizePattern_OutsideRepoRejected(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	outside := resolvePath(t, t.TempDir())
	require.NoError(t, os.Chdir(outside))
	// Pass an absolute path under 'outside', matched against 'root'.
	_, err := normalizePattern(outside, root)
	require.Error(t, err)
}

func TestNormalizePattern_RepoRootItselfRejected(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	require.NoError(t, os.Chdir(root))
	_, err := normalizePattern(".", root)
	require.Error(t, err)
}

// resolvePath normalises temp-dir paths on macOS where /var is a symlink
// to /private/var; without this the test assertions flap between runs.
func resolvePath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return resolved
}

func TestFindWorkspaceConfigPath_ExistingFile(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	configPath := filepath.Join(root, ".gortex.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(""), 0644))

	sub := filepath.Join(root, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.Chdir(sub))

	path, wsRoot, err := findWorkspaceConfigPath()
	require.NoError(t, err)
	assert.Equal(t, configPath, path)
	assert.Equal(t, root, wsRoot)
}

func TestFindWorkspaceConfigPath_GitRootFallback(t *testing.T) {
	root := resolvePath(t, t.TempDir())
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	sub := filepath.Join(root, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0755))
	require.NoError(t, os.Chdir(sub))

	path, wsRoot, err := findWorkspaceConfigPath()
	require.NoError(t, err)
	// No .gortex.yaml exists, so we get a prospective path at git root.
	assert.Equal(t, filepath.Join(root, ".gortex.yaml"), path)
	assert.Equal(t, root, wsRoot)
}

func TestWorkspaceTarget_AddRemove(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	require.NoError(t, os.Chdir(root))

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)

	// Initial load: file doesn't exist → nil.
	patterns, err := tgt.load()
	require.NoError(t, err)
	assert.Empty(t, patterns)

	// Save creates the file.
	require.NoError(t, tgt.save([]string{"a/", "b/"}))
	patterns, err = tgt.load()
	require.NoError(t, err)
	assert.Equal(t, []string{"a/", "b/"}, patterns)

	// Round-trip preserves only exclude (legacy keys scrubbed).
	data, err := os.ReadFile(filepath.Join(root, ".gortex.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "exclude:")
	assert.NotContains(t, string(data), "index:")
	assert.NotContains(t, string(data), "watch:")
}

func TestWorkspaceTarget_LegacyFallback(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0755))
	require.NoError(t, os.Chdir(root))

	// Simulate an existing legacy-shape .gortex.yaml.
	legacyYAML := "index:\n  exclude:\n    - legacy-pat/**\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(legacyYAML), 0644))

	tgt, err := newWorkspaceTarget()
	require.NoError(t, err)
	patterns, err := tgt.load()
	require.NoError(t, err)
	// Legacy key surfaces through the workspace target's load().
	assert.Equal(t, []string{"legacy-pat/**"}, patterns)
}
