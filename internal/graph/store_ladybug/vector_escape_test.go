//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestVectorSearcher_BulkUpsertSanitizesDirtyID guards the SymbolVec
// bulk COPY against node IDs containing a tab or newline (e.g.
// string-literal-derived nodes). Unescaped, such an ID split the TSV
// row and aborted the whole COPY with "expected 2 values per row, but
// got 1". The ID is sanitized the same way writeNodesTSV sanitizes the
// Node table, so the SymbolVec id stays consistent with the persisted
// Node id (the join key).
func TestVectorSearcher_BulkUpsertSanitizesDirtyID(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-vec-dirty-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := Open(filepath.Join(dir, "store.lbug"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	const dirtyID = "pkg/x.go::str\twith\ttab\nand\nnewline"
	items := []graph.VectorItem{
		{NodeID: dirtyID, Vec: []float32{1, 0, 0, 0}},
		{NodeID: "clean", Vec: []float32{0, 1, 0, 0}},
	}
	// Pre-fix this returned: copy SymbolVec: ... expected 2 values per
	// row, but got 1.
	require.NoError(t, s.BulkUpsertEmbeddings(items), "a dirty id must not abort the bulk COPY")
	require.NoError(t, s.BuildVectorIndex(4))

	hits, err := s.SimilarTo([]float32{1, 0, 0, 0}, 2)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	// The row is retrievable under the sanitized id (tab/newline -> space),
	// matching how the Node table stores the same id.
	want := sanitizeTSV(dirtyID)
	assert.Equal(t, want, hits[0].NodeID, "top hit must be the (sanitized) dirty id")
	assert.NotContains(t, hits[0].NodeID, "\t", "stored id must not contain a tab")
	assert.NotContains(t, hits[0].NodeID, "\n", "stored id must not contain a newline")
}
