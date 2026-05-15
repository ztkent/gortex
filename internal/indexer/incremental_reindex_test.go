package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

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
