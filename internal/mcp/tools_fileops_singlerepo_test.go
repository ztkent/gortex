package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// newSingleRepoServer indexes one repo through the MultiIndexer — the
// daemon/serverstack shape where multiIndexer is always non-nil — and
// returns the server plus the repo root. Nodes carry RepoPrefix=""
// (single-repo mode indexes without prefixing).
func newSingleRepoServer(t *testing.T) (*Server, *graph.Graph, string) {
	t.Helper()
	dir := setupMiniRepo(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})
	return srv, g, dir
}

// A single tracked repo mints unprefixed nodes; resolving their source
// path must anchor to the lone repo root rather than erroring with
// `could not resolve repo root (repo_prefix="")`. This is the gate in
// front of every source read — and of savings recording.
func TestResolveNodePath_SingleRepoUnprefixedNode(t *testing.T) {
	srv, g, dir := newSingleRepoServer(t)

	node := g.GetNode("main.go::Hello")
	require.NotNil(t, node, "single-repo node IDs are unprefixed")
	require.Empty(t, node.RepoPrefix)

	abs, err := srv.resolveNodePath(node)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
}

func TestResolveFilePath_SingleRepoBareRelative(t *testing.T) {
	srv, _, dir := newSingleRepoServer(t)

	abs, rel, err := srv.resolveFilePath("main.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
	assert.Equal(t, "main.go", rel)

	// Containment still enforced: escaping the lone root is refused.
	_, _, err = srv.resolveFilePath("../escape.go")
	require.Error(t, err)

	// The prefixed form keeps working.
	abs, _, err = srv.resolveFilePath("myrepo/main.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "main.go"), abs)
}

func TestResolveFilePath_MultiRepoBareRelativeStillAmbiguous(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

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

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	_, _, err = srv.resolveFilePath("main.go")
	require.Error(t, err, "bare-relative path with two tracked repos stays ambiguous")
}
