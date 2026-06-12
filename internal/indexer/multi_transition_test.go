package indexer

import (
	"context"
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

// Untracking a single-mode repo must actually evict its nodes. They
// carry RepoPrefix="" and never enter the byRepo bucket EvictRepo
// walks, so before the file-set eviction they lingered forever — and
// the lone-repo fallback would then resolve them against whichever
// repo was tracked next.
func TestUntrackRepo_SingleModeEvictsUnprefixedNodes(t *testing.T) {
	dir := setupRepoDir(t, "repo-a")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dir, Name: "repo-a"})
	require.NoError(t, err)
	require.NotNil(t, g.GetNode("main.go::Hello"), "single-mode node is unprefixed")

	nodes, _ := mi.UntrackRepo("repo-a")
	assert.Greater(t, nodes, 0, "untrack must evict the unprefixed nodes")
	assert.Nil(t, g.GetNode("main.go::Hello"), "stale unprefixed node must not survive untrack")

	// A different repo tracked afterwards (single mode again) must not
	// inherit any of repo-a's identity.
	dirB := setupRepoDir(t, "repo-b")
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)
	root, ok := mi.RepoRoot("")
	require.True(t, ok)
	assert.Equal(t, dirB, root)
}

// Tracking a second repo into a live single-repo daemon re-mints the
// first repo's nodes with its prefix, so they stay reachable when the
// empty-prefix fallback disarms.
func TestTrackRepo_SecondRepoRemintsLoneUnprefixedRepo(t *testing.T) {
	dirA := setupRepoDir(t, "repo-a")
	dirB := setupRepoDir(t, "repo-b")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirA, Name: "repo-a"})
	require.NoError(t, err)
	require.NotNil(t, g.GetNode("main.go::Hello"), "lone repo indexes unprefixed")

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)

	// repo-a's nodes are re-minted under its prefix; the unprefixed
	// originals are gone.
	assert.Nil(t, g.GetNode("main.go::Hello"), "unprefixed originals must be evicted on transition")
	require.NotNil(t, g.GetNode("repo-a/main.go::Hello"), "repo-a re-minted with prefix")
	require.NotNil(t, g.GetNode("repo-b/main.go::Hello"), "repo-b indexed with prefix")

	// With two repos the empty-prefix fallback stays ambiguous.
	_, ok := mi.RepoRoot("")
	assert.False(t, ok)

	// Both repos resolve through their prefixes.
	root, ok := mi.RepoRoot("repo-a")
	require.True(t, ok)
	assert.Equal(t, dirA, root)
}

// After a 1→2→1 sequence the lone survivor is a *prefixed* repo; the
// empty-prefix fallback must keep failing closed for it so any stale
// unprefixed ID cannot resolve against the wrong checkout.
func TestRepoRoot_EmptyPrefixFailsClosedForLonePrefixedRepo(t *testing.T) {
	dirA := setupRepoDir(t, "repo-a")
	dirB := setupRepoDir(t, "repo-b")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	ctx := context.Background()
	_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirA, Name: "repo-a"})
	require.NoError(t, err)
	_, err = mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)
	mi.UntrackRepo("repo-a")

	// repo-b is the lone survivor but was minted prefixed: the empty
	// prefix must not resolve to it.
	_, ok := mi.RepoRoot("")
	assert.False(t, ok, "lone prefixed repo must not satisfy the empty-prefix fallback")
	assert.Empty(t, mi.ResolveFilePath("main.go"))
}

// A repo whose directory name collides with one of its own top-level
// directories: single-repo graph paths are raw relative paths, so the
// prefix-matching join must not hijack them.
func TestResolveFilePath_LoneRepoOwnPrefixCollision(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "api")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o755))
	writeFile(t, filepath.Join(dir, "api", "handlers.go"), "package api\n\nfunc Handle() {}\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Hello() {}\n")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dir, Name: "api"})
	require.NoError(t, err)

	// The graph path "api/handlers.go" names <root>/api/handlers.go —
	// not <root>/handlers.go via prefix stripping.
	assert.Equal(t, filepath.Join(dir, "api", "handlers.go"), mi.ResolveFilePath("api/handlers.go"))
	// Plain unprefixed paths still anchor to the root.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("main.go"))
}
