package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Disable any global gpg signing or templates that might be
	// configured on the dev's machine — the test needs reproducible
	// commit behaviour.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, string(out))
}

func testCtx() context.Context { return context.Background() }

// TestGitWatcher_BranchSwitchReconciles is the end-to-end proof that
// the .git/HEAD watcher catches branch switches and dispatches the
// correct evict/index decisions per path. The test creates a real
// git repo with two branches differing in file content, indexes
// branch A, switches to branch B, and asserts the graph reflects B's
// files and not A's. Without this path, the per-file fsnotify watcher
// sees 500 Remove+Create events for a checkout and walks them through
// the per-file path (slow, wrong for renames).
func TestGitWatcher_BranchSwitchReconciles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	// Main branch: a.go defines Alpha.
	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Alpha")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("a.go"), "Alpha must be indexed on main")

	// Create a feature branch that replaces a.go with b.go.
	runGit(t, repoDir, "checkout", "-q", "-b", "feature")
	require.NoError(t, os.Remove(filepath.Join(repoDir, "a.go")))
	writeFile(t, filepath.Join(repoDir, "b.go"), "package main\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "feature: Beta replaces Alpha")

	// Return to main so the diff has a clean "old" reference, then
	// start the watcher there and switch to feature. Checking out
	// feature again will move HEAD from main→feature; the watcher
	// observes the move and reconciles.
	runGit(t, repoDir, "checkout", "-q", "main")

	// Ensure the graph reflects main again — simulate a "daemon
	// started on main" by re-indexing explicitly.
	g2 := graph.New()
	idx2 := New(g2, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx2.search = search.NewBM25()
	idx2.SetRootPath(repoDir)
	_, err = idx2.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NotEmpty(t, g2.GetFileNodes("a.go"), "main branch must have Alpha")
	require.Empty(t, g2.GetFileNodes("b.go"), "main branch must not have Beta")

	gw, err := NewGitWatcher(repoDir, idx2, zap.NewNop())
	require.NoError(t, err)
	// Shorter debounce so the test finishes quickly.
	gw.debounce = 50 * time.Millisecond

	drained := make(chan int, 1)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	// Switch branches — this is the signal the watcher is supposed
	// to act on.
	runGit(t, repoDir, "checkout", "-q", "feature")

	select {
	case n := <-drained:
		assert.GreaterOrEqual(t, n, 1, "drain must touch at least one file")
	case <-time.After(5 * time.Second):
		t.Fatal("git watcher did not reconcile within timeout")
	}

	// After the reconcile, the graph must reflect the feature
	// branch: Beta present, Alpha gone.
	assert.Empty(t, g2.GetFileNodes("a.go"),
		"after checkout feature, Alpha's file-nodes must be evicted")
	assert.NotEmpty(t, g2.GetFileNodes("b.go"),
		"after checkout feature, Beta must be indexed")
}

// TestGitWatcher_ReconcileSingleFlight proves overlapping reconciles
// coalesce instead of running concurrently from the same stale base:
// a reconcile arriving while one is in flight only marks a rerun, and
// the in-flight run's completion replays exactly one follow-up that
// converges on the latest HEAD (and no-ops when HEAD didn't move).
func TestGitWatcher_ReconcileSingleFlight(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Alpha")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.lastSHA, err = gw.currentSHA(testCtx())
	require.NoError(t, err)

	drained := make(chan int, 2)
	gw.drained = func(n int) { drained <- n }

	// A reconcile arriving while one is in flight must coalesce: it
	// leaves the graph and lastSHA untouched and records the rerun.
	gw.mu.Lock()
	gw.reconciling = true
	gw.mu.Unlock()
	gw.reconcile("overlapping")
	gw.mu.Lock()
	require.True(t, gw.rerun, "overlapping reconcile must coalesce into a rerun")
	gw.reconciling = false
	gw.mu.Unlock()
	select {
	case <-drained:
		t.Fatal("coalesced reconcile must not apply changes")
	default:
	}

	// Move HEAD, then run the real reconcile with the rerun flag still
	// set: it applies the branch switch, and its completion replays
	// exactly one follow-up that no-ops on the unchanged HEAD.
	runGit(t, repoDir, "checkout", "-q", "-b", "feature")
	require.NoError(t, os.Remove(filepath.Join(repoDir, "a.go")))
	writeFile(t, filepath.Join(repoDir, "b.go"), "package main\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "feature: Beta replaces Alpha")

	gw.reconcile("real")

	select {
	case n := <-drained:
		require.GreaterOrEqual(t, n, 1, "real reconcile must touch files")
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not complete")
	}
	assert.Empty(t, g.GetFileNodes("a.go"), "Alpha must be evicted")
	assert.NotEmpty(t, g.GetFileNodes("b.go"), "Beta must be indexed")

	// The replayed follow-up settles without applying anything.
	require.Eventually(t, func() bool {
		gw.mu.Lock()
		defer gw.mu.Unlock()
		return !gw.reconciling && !gw.rerun
	}, 5*time.Second, 10*time.Millisecond, "coalesced follow-up must settle")
	select {
	case <-drained:
		t.Fatal("follow-up reconcile on unchanged HEAD must not apply changes")
	default:
	}
}

// TestGitWatcher_NoopWhenHeadUnchanged covers the "ref file touched
// but commit unchanged" case — e.g., a git gc that packs refs without
// moving any branch. The watcher should read the current SHA, find
// it matches the last seen one, and skip the diff entirely. A naive
// "any event → reconcile" would call git diff with empty ranges and
// log noise without changing the graph.
func TestGitWatcher_NoopWhenHeadUnchanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "init")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	gw.drained = func(int) {
		t.Error("drained must not fire when HEAD didn't move")
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	// Trigger a reconcile manually without moving HEAD — simulates
	// a spurious ref-file touch.
	gw.reconcile("synthetic")

	// Give any unexpected drain a window to fire.
	time.Sleep(200 * time.Millisecond)
}

// TestGitWatcher_DeletedClassification checks the 'D' diff status is
// classified by what is on disk, not by what git tracks: a file gone
// from disk is evicted, but a file removed from tracking yet still on
// disk ("untracked but visible") must stay indexed.
func TestGitWatcher_DeletedClassification(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ondisk.go"), "package main\n\nfunc OnDisk() {}\n")
	writeFile(t, filepath.Join(dir, "gone.go"), "package main\n\nfunc Gone() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("OnDisk"))
	require.NotEmpty(t, g.FindNodesByName("Gone"))

	gw, err := NewGitWatcher(dir, idx, zap.NewNop())
	require.NoError(t, err)
	defer func() { _ = gw.Stop() }()

	// gone.go is genuinely removed from disk; ondisk.go merely left
	// git tracking. Both arrive as a 'D' in the diff.
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))
	gw.applyChanges([]gitChange{
		{Status: 'D', Path: "gone.go"},
		{Status: 'D', Path: "ondisk.go"},
	})

	assert.Empty(t, g.FindNodesByName("Gone"),
		"a file gone from disk must be evicted on a 'D'")
	assert.NotEmpty(t, g.FindNodesByName("OnDisk"),
		"a file un-tracked but still on disk must stay indexed on a 'D'")
}

// TestGitWatcher_UntrackedFileStaysIndexed is the end-to-end proof for
// the untracked-but-visible case: `git rm --cached` removes a file
// from tracking but leaves it on disk, the commit moves HEAD, and the
// watcher's diff reports the file as deleted. The reconcile must keep
// the still-present file indexed rather than evict it.
func TestGitWatcher_UntrackedFileStaysIndexed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(repoDir, "core.go"), "package main\nfunc Core() {}\n")
	writeFile(t, filepath.Join(repoDir, "generated.go"), "package main\nfunc Generated() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "init")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewBM25()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("generated.go"))

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 1)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	// Stop tracking generated.go but keep it on disk, then commit so
	// HEAD moves and the watcher fires. The diff reports a 'D'.
	runGit(t, repoDir, "rm", "--cached", "-q", "generated.go")
	runGit(t, repoDir, "commit", "-q", "-m", "untrack generated.go")

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("git watcher did not reconcile within timeout")
	}

	assert.NotEmpty(t, g.GetFileNodes("generated.go"),
		"a file removed from git tracking but still on disk must stay indexed")
	assert.NotEmpty(t, g.GetFileNodes("core.go"), "core.go is untouched")
}
