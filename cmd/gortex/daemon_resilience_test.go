package main

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

func fakeRepoEntry(path string) config.RepoEntry {
	return config.RepoEntry{Path: path}
}

// TestSnapshot_RestartStability is the B1 regression guard at the
// daemon-snapshot layer: N save/load cycles on top of the existing
// graph must not grow node or edge counts. Before Phase 2, re-running
// a snapshot load on top of a warm graph doubled edges every cycle
// because AddNode/AddEdge appended blindly.
func TestSnapshot_RestartStability(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	orig := graph.New()
	orig.AddNode(&graph.Node{ID: "a.go::Foo", Name: "Foo", Kind: graph.KindFunction, FilePath: "a.go"})
	orig.AddNode(&graph.Node{ID: "a.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "a.go"})
	orig.AddEdge(&graph.Edge{
		From: "a.go::Foo", To: "a.go::Bar",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5,
	})
	want := orig.Stats()

	// Save once; then repeatedly load into the same graph. Each
	// load re-applies AddNode / AddEdge for every record. With
	// idempotent writes, stats must not drift.
	saveSnapshot(orig, nil, nil, "v-test", zap.NewNop())

	for cycle := 0; cycle < 5; cycle++ {
		result, err := loadSnapshot(orig, zap.NewNop())
		require.NoError(t, err, "cycle %d", cycle)
		require.True(t, result.Loaded, "cycle %d", cycle)
		got := orig.Stats()
		assert.Equal(t, want.TotalNodes, got.TotalNodes,
			"cycle %d: nodes drifted", cycle)
		assert.Equal(t, want.TotalEdges, got.TotalEdges,
			"cycle %d: edges drifted (B1 regression)", cycle)
	}
}

// TestSnapshot_RoundTripsRepos verifies the new per-repo FileMtimes
// section survives encode → decode. Without this, IncrementalReindex on
// warmup treats every file as stale (empty mtime map → IsStale=true for
// all), producing the same duplicate-writes problem we fixed.
func TestSnapshot_RoundTripsRepos(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go::X", Name: "X", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})

	repos := []snapshotRepo{{
		RepoPrefix: "repo",
		RootPath:   "/abs/path/to/repo",
		FileMtimes: map[string]int64{
			"a.go":     1000,
			"b/c.go":   2000,
			"x/y/z.go": 3000,
		},
	}}

	saveSnapshot(g, repos, nil, "v-test", zap.NewNop())

	restored := graph.New()
	result, err := loadSnapshot(restored, zap.NewNop())
	require.NoError(t, err)
	require.True(t, result.Loaded)
	require.NotNil(t, result.Repos, "repo section must round-trip")

	r, ok := result.Repos["repo"]
	require.True(t, ok, "repo prefix must be keyed correctly")
	assert.Equal(t, "/abs/path/to/repo", r.RootPath)
	assert.Equal(t, int64(1000), r.FileMtimes["a.go"])
	assert.Equal(t, int64(2000), r.FileMtimes["b/c.go"])
	assert.Equal(t, int64(3000), r.FileMtimes["x/y/z.go"])
}

// TestSnapshot_BackwardCompat_OldSnapshotLoads_NoRepos verifies that a
// snapshot written without repo metadata (the v2 layout, pre-Phase 1)
// loads cleanly on the new daemon — the RepoCount field decodes as
// zero, the repo section is empty, and Repos is nil. The new daemon
// falls back to TrackRepoCtx, which does a full re-index.
func TestSnapshot_BackwardCompat_OldSnapshotLoads_NoRepos(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::Foo", Name: "Foo", Kind: graph.KindFunction, FilePath: "a.go"})

	// No repos passed — simulates a snapshot from before the
	// RepoCount field existed. (The on-disk bytes are the same
	// either way: saveSnapshot writes zero repo records when the
	// slice is empty, matching what older daemons produced.)
	saveSnapshot(g, nil, nil, "v-test", zap.NewNop())

	restored := graph.New()
	result, err := loadSnapshot(restored, zap.NewNop())
	require.NoError(t, err)
	assert.True(t, result.Loaded)
	assert.False(t, result.Partial)
	assert.Empty(t, result.Repos, "empty repos must surface as nil/empty map, not an error")
	assert.Equal(t, 1, restored.NodeCount())
}

// TestSnapshot_DanglingEdgesDropped verifies the structural-validation
// pass: edges whose endpoints aren't in the loaded node set are
// dropped, not stored. A corrupt or truncated snapshot that landed
// with dangling refs used to surface later as "edge pointing at nil
// node" panics in traversal code; the loader's contract is to drop
// them at load time.
func TestSnapshot_DanglingEdgesDropped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(dir, "snap.gob.gz"))

	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "a.go"})
	// B is deliberately NOT added — this edge will be dangling on load.
	// AddEdge still stores it (the graph doesn't validate at insert
	// time), but loadSnapshot should reject it during the structural
	// validation pass after nodes are in place.
	g.AddEdge(&graph.Edge{
		From: "a.go::A", To: "b.go::B",
		Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1,
	})

	saveSnapshot(g, nil, nil, "v-test", zap.NewNop())

	restored := graph.New()
	result, err := loadSnapshot(restored, zap.NewNop())
	require.NoError(t, err)
	require.True(t, result.Loaded)
	assert.Equal(t, 1, restored.NodeCount(), "only the valid node should land")
	assert.Equal(t, 0, restored.EdgeCount(),
		"dangling edge must be dropped during load validation")
}

// TestCanMigrate covers the migration-chain guard that gates between
// "attempt to migrate" and "discard cache on schema mismatch" paths.
// The registry ships empty (Phase 4 is scaffolding) so every answer
// today is `false`, but the shape of the lookup matters — a future
// PR that lands a real migration needs this to return `true` only
// when the chain is complete.
func TestCanMigrate(t *testing.T) {
	// from >= to → no migration needed.
	assert.False(t, canMigrate(3, 3))
	assert.False(t, canMigrate(5, 2))

	// Empty registry — no migrations are available.
	assert.False(t, canMigrate(1, 2))

	// Install a fake v1→v2 migration. A complete chain reports true;
	// a missing link (v2→v3 never registered) reports false.
	orig := snapshotMigrations
	t.Cleanup(func() { snapshotMigrations = orig })
	snapshotMigrations = map[int]snapshotMigration{
		1: func(in io.Reader, out io.Writer) error { return nil },
	}
	assert.True(t, canMigrate(1, 2))
	assert.False(t, canMigrate(1, 3), "missing v2→v3 link breaks the chain")
}

// TestPriorMtimesForEntry_MatchesByAbsRootPath is the core of the
// warmup decision: given a snapshot's per-repo metadata and a config
// entry, priorMtimesForEntry must resolve the right FileMtimes for
// incremental reconciliation. A miss here causes warmup to fall back
// to full re-indexing — not a correctness bug, but the whole point of
// Phase 1 is to avoid the full pass on routine restart.
func TestPriorMtimesForEntry_MatchesByAbsRootPath(t *testing.T) {
	absPath := filepath.Join(t.TempDir(), "repo")

	repos := map[string]*snapshotRepo{
		"repo": {
			RepoPrefix: "repo",
			RootPath:   absPath,
			FileMtimes: map[string]int64{"a.go": 42},
		},
	}

	// Exact absolute-path match.
	entry := fakeRepoEntry(absPath)
	got := priorMtimesForEntry(repos, entry)
	assert.Equal(t, int64(42), got["a.go"])

	// No match → nil.
	miss := fakeRepoEntry(filepath.Join(t.TempDir(), "nope"))
	assert.Nil(t, priorMtimesForEntry(repos, miss))

	// Empty repos map is safe — no panic, returns nil.
	assert.Nil(t, priorMtimesForEntry(nil, entry))
}
