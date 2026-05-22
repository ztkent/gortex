package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestWorktreeRootGone covers the liveness predicate the janitor's
// worktree GC keys on: a present directory is "live", a removed one is
// "gone", and an empty path is always "live" (nothing to GC).
func TestWorktreeRootGone(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, WorktreeRootGone(dir), "an existing directory is not gone")
	assert.False(t, WorktreeRootGone(""), "an empty path is never reported gone")

	gone := filepath.Join(dir, "removed")
	require.NoError(t, os.MkdirAll(gone, 0o755))
	assert.False(t, WorktreeRootGone(gone), "a freshly-created dir is live")
	require.NoError(t, os.RemoveAll(gone))
	assert.True(t, WorktreeRootGone(gone), "a removed directory must read as gone")
}

// TestGCVanishedWorktrees_EvictsRemovedWorktree is the core piece-1
// scenario: the daemon tracks a main checkout and one of its linked
// worktrees, the worktree directory is removed (`git worktree remove`
// or a manual delete), and the janitor's GC sweep must evict the
// vanished worktree's index — its graph nodes and its config entry —
// while leaving the still-present main checkout untouched.
func TestGCVanishedWorktrees_EvictsRemovedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	// Main checkout — a real git repo so `git worktree add` works.
	main := t.TempDir()
	runGit(t, main, "init", "-q", "-b", "main")
	runGit(t, main, "config", "user.email", "test@example.com")
	runGit(t, main, "config", "user.name", "Test")
	runGit(t, main, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(main, "main.go"), "package main\nfunc Main() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-q", "-m", "init")

	// A linked worktree on its own branch, holding its own file.
	wt := filepath.Join(t.TempDir(), "feature-wt")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", wt)
	writeFile(t, filepath.Join(wt, "feature.go"), "package main\nfunc Feature() {}\n")

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: main, Name: "main-repo"},
		{Path: wt, Name: "feature-wt"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// Both repos are tracked; the worktree must be classified as one.
	mainMeta := mi.GetMetadata("main-repo")
	wtMeta := mi.GetMetadata("feature-wt")
	require.NotNil(t, mainMeta)
	require.NotNil(t, wtMeta)
	assert.False(t, mainMeta.IsWorktree, "the main checkout is not a worktree")
	assert.True(t, wtMeta.IsWorktree, "the linked worktree must be flagged IsWorktree")
	require.NotEmpty(t, g.GetFileNodes("feature-wt/feature.go"),
		"the worktree's file must be indexed before removal")

	// Nothing has vanished yet — a GC sweep is a no-op.
	assert.Empty(t, mi.GCVanishedWorktrees(),
		"no worktree is gone yet, GC must evict nothing")

	// Remove the worktree directory — the `git worktree remove`
	// effect on disk. The main checkout stays.
	require.NoError(t, os.RemoveAll(wt))

	gced := mi.GCVanishedWorktrees()
	require.Len(t, gced, 1, "exactly the vanished worktree must be GC'd")
	assert.Equal(t, "feature-wt", gced[0].RepoPrefix)
	assert.Equal(t, wt, gced[0].RootPath)
	assert.Positive(t, gced[0].NodesRemoved,
		"the vanished worktree's graph nodes must be reported removed")

	// The worktree's index is gone: no metadata, no graph nodes.
	assert.Nil(t, mi.GetMetadata("feature-wt"),
		"the vanished worktree must be untracked")
	assert.Empty(t, g.GetFileNodes("feature-wt/feature.go"),
		"the vanished worktree's nodes must be evicted from the graph")

	// The still-present main checkout is untouched.
	assert.NotNil(t, mi.GetMetadata("main-repo"),
		"the live main checkout must remain tracked")
	assert.NotEmpty(t, g.GetFileNodes("main-repo/main.go"),
		"the live main checkout's nodes must survive the GC sweep")

	// A second sweep finds nothing left to collect — GC is idempotent.
	assert.Empty(t, mi.GCVanishedWorktrees(),
		"a repeated GC sweep must be a no-op once the worktree is gone")
}

// TestGCVanishedWorktrees_LeavesVanishedMainCheckout asserts the GC's
// deliberate restraint: a *main* checkout whose directory vanished is
// NOT collected. A missing main checkout is far more likely a transient
// mount problem than an intentional removal, and untracking it would
// also orphan every linked worktree that shares its .git.
func TestGCVanishedWorktrees_LeavesVanishedMainCheckout(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "plain-repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "plain"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	require.False(t, mi.GetMetadata("plain").IsWorktree,
		"a non-worktree directory must not be flagged IsWorktree")

	// Remove the whole repo directory.
	require.NoError(t, os.RemoveAll(repoPath))

	// GC must not touch it — only worktrees are eligible.
	assert.Empty(t, mi.GCVanishedWorktrees(),
		"a vanished main checkout must not be garbage-collected")
	assert.NotNil(t, mi.GetMetadata("plain"),
		"the non-worktree repo stays tracked despite a missing root")
}

// TestLinkedWorktreeRoots reports the on-disk worktree siblings of a
// repo — the lookup the edit-path resolver uses to re-root a write into
// the worktree the file belongs to.
func TestLinkedWorktreeRoots(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	main := t.TempDir()
	runGit(t, main, "init", "-q", "-b", "main")
	runGit(t, main, "config", "user.email", "test@example.com")
	runGit(t, main, "config", "user.name", "Test")
	runGit(t, main, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(main, "main.go"), "package main\nfunc Main() {}\n")
	runGit(t, main, "add", ".")
	runGit(t, main, "commit", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", wt)
	writeFile(t, filepath.Join(wt, "feature.go"), "package main\nfunc Feature() {}\n")

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: main, Name: "main-repo"},
		{Path: wt, Name: "wt"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// Queried from the main checkout, the linked worktree is listed.
	fromMain := mi.LinkedWorktreeRoots(main)
	require.Len(t, fromMain, 1)
	assert.Equal(t, realpath(t, wt), realpath(t, fromMain[0]))

	// Queried from the worktree itself, the same set comes back —
	// the lookup keys on the shared MainRepoPath, not the argument.
	fromWt := mi.LinkedWorktreeRoots(wt)
	require.Len(t, fromWt, 1)
	assert.Equal(t, realpath(t, wt), realpath(t, fromWt[0]))

	// An unrelated path resolves to no worktree siblings.
	assert.Empty(t, mi.LinkedWorktreeRoots(t.TempDir()))
}
