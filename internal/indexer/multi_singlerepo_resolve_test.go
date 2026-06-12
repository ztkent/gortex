package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func indexSingleRepoForTest(t *testing.T) (*MultiIndexer, string) {
	t.Helper()
	dir := setupRepoDir(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	return mi, dir
}

func indexTwoReposForTest(t *testing.T) (*MultiIndexer, string, string) {
	t.Helper()
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	return mi, repoA, repoB
}

// Single-repo mode indexes nodes and file paths without a repo prefix
// while registering the repo's metadata under its real prefix. The empty
// prefix must therefore resolve to the lone repo — otherwise every node
// the single-repo indexer mints is unresolvable (no source reads, no
// savings recording, no editing by graph path).
func TestMultiIndexer_RepoRoot_EmptyPrefixResolvesLoneRepo(t *testing.T) {
	mi, dir := indexSingleRepoForTest(t)

	root, ok := mi.RepoRoot("")
	require.True(t, ok, "empty prefix must resolve when exactly one repo is tracked")
	assert.Equal(t, dir, root)

	// The real prefix keeps working.
	root, ok = mi.RepoRoot("myrepo")
	require.True(t, ok)
	assert.Equal(t, dir, root)

	// Unknown prefixes still miss.
	_, ok = mi.RepoRoot("nope")
	assert.False(t, ok)
}

func TestMultiIndexer_RepoRoot_EmptyPrefixAmbiguousInMultiRepo(t *testing.T) {
	mi, _, _ := indexTwoReposForTest(t)

	_, ok := mi.RepoRoot("")
	assert.False(t, ok, "empty prefix is ambiguous with two tracked repos")
}

func TestMultiIndexer_ResolveFilePath_UnprefixedSingleRepo(t *testing.T) {
	mi, dir := indexSingleRepoForTest(t)

	// Unprefixed path (the form single-repo nodes carry) anchors to the
	// lone repo root.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("main.go"))

	// The prefixed form keeps working too.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("myrepo/main.go"))
}

func TestMultiIndexer_ResolveFilePath_UnprefixedMultiRepoStaysEmpty(t *testing.T) {
	mi, _, _ := indexTwoReposForTest(t)

	assert.Empty(t, mi.ResolveFilePath("main.go"),
		"bare path with two tracked repos is ambiguous and must not resolve")
}
