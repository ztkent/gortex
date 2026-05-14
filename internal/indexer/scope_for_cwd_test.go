package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestScopeForCWD_And_ReposInWorkspace exercises the per-session
// workspace resolution that underpins workspace isolation: a cwd is
// resolved to the workspace/project of the tracked repo that contains
// it, sibling repos sharing a workspace slug resolve together, a repo
// with no declared workspace is its own singleton workspace, and a cwd
// outside every tracked repo fails closed.
func TestScopeForCWD_And_ReposInWorkspace(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	repoC := setupRepoDir(t, "repo-c") // no .gortex.yaml → singleton workspace

	// repo-a and repo-b share workspace "alpha"; repo-c declares none.
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: repoC, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "") // empty scope → index every configured repo
	require.NoError(t, err)

	// cwd inside repo-a → workspace "alpha", home repo "repo-a".
	ws, _, prefix, ok := mi.ScopeForCWD(repoA)
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-a", prefix)

	// A nested subdirectory of repo-b still resolves to "alpha". The
	// path need not exist on disk — it is the agent's cwd, matched by
	// prefix against the tracked repo root.
	ws, _, prefix, ok = mi.ScopeForCWD(filepath.Join(repoB, "internal", "deep"))
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-b", prefix)

	// repo-c has no declared workspace → singleton workspace keyed on
	// the repo prefix.
	ws, _, prefix, ok = mi.ScopeForCWD(repoC)
	require.True(t, ok)
	assert.Equal(t, "repo-c", ws)
	assert.Equal(t, "repo-c", prefix)

	// A cwd outside every tracked repo must fail closed.
	_, _, _, ok = mi.ScopeForCWD(t.TempDir())
	assert.False(t, ok, "cwd outside every tracked repo must not resolve")

	// An empty cwd fails closed too.
	_, _, _, ok = mi.ScopeForCWD("")
	assert.False(t, ok)

	// ReposInWorkspace("alpha") is exactly {repo-a, repo-b}.
	alpha := mi.ReposInWorkspace("alpha")
	assert.True(t, alpha["repo-a"])
	assert.True(t, alpha["repo-b"])
	assert.False(t, alpha["repo-c"], "repo-c is not in workspace alpha")
	assert.Len(t, alpha, 2)

	// The singleton workspace "repo-c" contains only repo-c.
	singleton := mi.ReposInWorkspace("repo-c")
	assert.Equal(t, map[string]bool{"repo-c": true}, singleton)

	// An unknown workspace resolves to the empty set.
	assert.Empty(t, mi.ReposInWorkspace("does-not-exist"))
}
