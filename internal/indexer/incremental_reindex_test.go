package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
)

// TestIncrementalReindex_PreservesExcludedFiles is the regression test for
// the deletion-classification bug: a file that's tracked in fileMtimes but
// absent from the discovery walk must NOT be purged unless it's truly gone
// from disk. Otherwise an exclude-list change between passes silently
// destroys legitimate graph state.
func TestIncrementalReindex_PreservesExcludedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.go"), `package main

func Kept() {}
`)
	writeFile(t, filepath.Join(dir, "drop.go"), `package main

func Dropped() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Kept"))
	require.NotEmpty(t, g.FindNodesByName("Dropped"))

	// Mid-flight exclusion change: future discoveries will not visit
	// drop.go, but it's still on disk. The old code would treat it as
	// deleted and purge its nodes; the new code stats it and preserves.
	idx.config.Exclude = append(append([]string{}, excludes.Builtin...), "drop.go")
	idx.excludes = nil
	idx.excludeOnce = sync.Once{}

	_, err = idx.IncrementalReindex(dir)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("Kept"), "kept.go was not excluded; nodes must survive")
	assert.NotEmpty(t, g.FindNodesByName("Dropped"), "drop.go was excluded but still on disk; nodes must be preserved")
}

// TestIncrementalReindex_EvictsTrulyDeletedFiles is the control case: a
// file that has actually been removed from disk should still be evicted.
func TestIncrementalReindex_EvictsTrulyDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keep.go"), `package main

func Kept() {}
`)
	gonePath := filepath.Join(dir, "gone.go")
	writeFile(t, gonePath, `package main

func Gone() {}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Gone"))

	require.NoError(t, os.Remove(gonePath))

	_, err = idx.IncrementalReindex(dir)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("Kept"))
	assert.Empty(t, g.FindNodesByName("Gone"), "gone.go was deleted from disk; nodes must be evicted")
}

// canonicalGraph renders a graph as a deterministic, sorted projection
// of its structural identity (node identities + edge triples). Two
// graphs with an equal projection are byte-identical for every query
// the engine can answer.
func canonicalGraph(g *graph.Graph) string {
	var lines []string
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("N|%s|%s|%s|%s|%d|%d|%s",
			n.ID, n.Kind, n.Name, n.FilePath, n.StartLine, n.EndLine, n.Language))
	}
	for _, e := range g.AllEdges() {
		if e == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("E|%s|%s|%s", e.From, e.To, e.Kind))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// bumpMtime rewrites a file and pushes its mtime forward so the
// mtime-keyed staleness check always classifies it as changed,
// regardless of filesystem timestamp resolution.
func bumpMtime(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
}

// TestIncrementalReindex_ConvergesToFullIndex is the consistency
// invariant: a graph built incrementally — a cold index followed by
// per-file edits each reconciled with IncrementalReindex — must equal
// a single cold index of the same final disk state. Incremental
// reindex that drifted from a full index would silently serve a stale
// or wrong graph.
func TestIncrementalReindex_ConvergesToFullIndex(t *testing.T) {
	build := func(dir string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
		writeFile(t, filepath.Join(dir, "main.go"),
			"package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
		writeFile(t, filepath.Join(dir, "pkg", "util.go"),
			"package pkg\n\ntype Config struct{ Port int }\n\nfunc New() *Config { return &Config{} }\n")
		writeFile(t, filepath.Join(dir, "extra.go"),
			"package main\n\nfunc Extra() {}\n")
	}

	// Path A: incremental — a cold index, then a sequence of edits
	// each reconciled with IncrementalReindex.
	dir := t.TempDir()
	build(dir)
	gA := graph.New()
	idxA := newTestIndexer(gA)
	_, err := idxA.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() { helper(); helper() }\n\nfunc helper() {}\n")
	_, err = idxA.IncrementalReindex(dir)
	require.NoError(t, err)

	bumpMtime(t, filepath.Join(dir, "pkg", "util.go"),
		"package pkg\n\ntype Config struct{ Port int }\n\nfunc New() *Config { return &Config{} }\n\nfunc Reset(c *Config) {}\n")
	_, err = idxA.IncrementalReindex(dir)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(dir, "extra.go")))
	_, err = idxA.IncrementalReindex(dir)
	require.NoError(t, err)

	// Path B: a single cold index of the same final disk state.
	gB := graph.New()
	idxB := newTestIndexer(gB)
	_, err = idxB.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, canonicalGraph(gB), canonicalGraph(gA),
		"incremental reindex must converge to the same graph as a full index")
}

// TestIncrementalReindex_FailedFileSurfacedAndRetried checks the
// failed-chunk replay surface: a stale file that cannot be indexed is
// reported on IndexResult.FailedFiles (after one in-pass retry), its
// mtime is left unrecorded so it stays stale, and a later pass
// recovers it once the obstruction clears.
func TestIncrementalReindex_FailedFileSurfacedAndRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable-file test is meaningless as root")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.go"), "package main\n\nfunc OK() {}\n")
	bad := filepath.Join(dir, "bad.go")
	writeFile(t, bad, "package main\n\nfunc Bad() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Bad"))

	// Make bad.go unreadable and stale: the incremental pass discovers
	// it (stat works) but fails to read its content.
	require.NoError(t, os.Chmod(bad, 0o000))
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(bad, future, future))

	res, err := idx.IncrementalReindex(dir)
	require.NoError(t, err)
	assert.Contains(t, res.FailedFiles, bad,
		"an unreadable stale file must be surfaced on FailedFiles")

	// Readable again: the file is still stale (its failed pass never
	// recorded an mtime), so the next incremental pass recovers it.
	require.NoError(t, os.Chmod(bad, 0o644))
	res2, err := idx.IncrementalReindex(dir)
	require.NoError(t, err)
	assert.Empty(t, res2.FailedFiles, "the file indexes cleanly once readable")
	assert.NotEmpty(t, g.FindNodesByName("Bad"))
}

// TestIncrementalReindex_MerkleMode exercises the BLAKE3 Merkle change
// detector: a content edit is re-indexed, but a file merely touched
// (new mtime, identical content) is not — the content-addressed tree
// ignores the mtime false positive that the bare-mtime path would
// re-index needlessly.
func TestIncrementalReindex_MerkleMode(t *testing.T) {
	t.Setenv("GORTEX_MERKLE", "1")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "edited.go"), "package main\n\nfunc Edited() {}\n")
	writeFile(t, filepath.Join(dir, "touched.go"), "package main\n\nfunc Touched() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Edited"))
	require.NotEmpty(t, g.FindNodesByName("Touched"))
	require.FileExists(t, filepath.Join(dir, ".gortex", "merkle.json"),
		"a full index in Merkle mode must persist a baseline tree")

	// Edit one file's content; touch the other without changing it.
	bumpMtime(t, filepath.Join(dir, "edited.go"),
		"package main\n\nfunc Edited() {}\n\nfunc AlsoEdited() {}\n")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "touched.go"), future, future))

	res, err := idx.IncrementalReindex(dir)
	require.NoError(t, err)

	assert.NotEmpty(t, g.FindNodesByName("AlsoEdited"),
		"a content edit must be re-indexed under Merkle mode")
	assert.Equal(t, 1, res.StaleFileCount,
		"only the content-changed file is stale; a bare touch is not")
}
