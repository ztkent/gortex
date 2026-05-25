//go:build ladybug

package store_ladybug

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/search"
)

// TestSymbolSearcher_EndToEnd is the conformance check for the
// Ladybug FTS path. Seeds three "symbols" via UpsertSymbolFTS with
// pre-tokenised text, builds the index, then exercises queries that
// the existing BM25 backend recall contract requires to work:
//
//   - exact identifier ("ValidateToken" tokenises to "validate token")
//   - mid-word camelCase ("validate" / "token" alone)
//   - qualifier hop ("auth")
//   - control case ("PrettyPrint" / "pretty")
//
// The probe in fts_probe_test.go proved the raw CALL surface works
// but couldn't camelCase-split — the tokenizer bridge here is what
// closes that recall gap.
func TestSymbolSearcher_EndToEnd(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-fts-e2e-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Pre-tokenise the symbol names exactly as the indexer will at
	// production time — search.Tokenize handles camelCase and
	// snake_case + path separators.
	upsert := func(id, raw string) {
		toks := search.Tokenize(raw)
		joined := ""
		for i, t := range toks {
			if i > 0 {
				joined += " "
			}
			joined += t
		}
		require.NoError(t, s.UpsertSymbolFTS(id, joined))
	}
	upsert("pkg/auth.go::ValidateToken", "ValidateToken auth.ValidateToken")
	upsert("pkg/auth.go::ValidateSession", "ValidateSession auth.ValidateSession")
	upsert("pkg/format.go::PrettyPrint", "PrettyPrint format.PrettyPrint")

	require.NoError(t, s.BuildSymbolIndex())

	cases := []struct {
		name      string
		query     string
		wantTopID string
		minHits   int
	}{
		{"exact identifier", "ValidateToken", "pkg/auth.go::ValidateToken", 1},
		{"camelCase head", "validate", "", 2},
		{"camelCase tail", "token", "pkg/auth.go::ValidateToken", 1},
		{"two-word query", "validate token", "pkg/auth.go::ValidateToken", 1},
		{"qualifier", "auth", "", 2},
		{"control", "pretty", "pkg/format.go::PrettyPrint", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits, err := s.SearchSymbols(c.query, 10)
			require.NoError(t, err)
			t.Logf("query %q → %d hits: %v", c.query, len(hits), hits)
			assert.GreaterOrEqual(t, len(hits), c.minHits,
				"query %q must return at least %d hits", c.query, c.minHits)
			if c.wantTopID != "" && len(hits) > 0 {
				assert.Equal(t, c.wantTopID, hits[0].NodeID,
					"top hit for %q must be %s", c.query, c.wantTopID)
			}
		})
	}
}

// TestSymbolSearcher_AutoUpdate verifies the FTS index reflects
// rows added after CREATE_FTS_INDEX. Critical for incremental
// reindexing — a file change re-triggers UpsertSymbolFTS and the
// new row must be findable without re-running BuildSymbolIndex.
func TestSymbolSearcher_AutoUpdate(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-fts-auto-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.UpsertSymbolFTS("pkg/a.go::Original", "original a.original"))
	require.NoError(t, s.BuildSymbolIndex())

	// First query — only the original row exists.
	hits, err := s.SearchSymbols("original", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// Upsert a new row AFTER index creation.
	require.NoError(t, s.UpsertSymbolFTS("pkg/b.go::PostAdd", "post add b.postadd"))
	hits, err = s.SearchSymbols("postadd", 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(hits), 1,
		"post-create insert must be findable without rebuilding the index")
}

// TestSymbolSearcher_IdempotentUpsert verifies that replacing a row's
// text via a second UpsertSymbolFTS call updates the FTS hit in
// place instead of producing a duplicate. Matches the indexer's
// re-parse contract.
func TestSymbolSearcher_IdempotentUpsert(t *testing.T) {
	dir, err := os.MkdirTemp("", "lbug-fts-idem-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	s, err := Open(filepath.Join(dir, "store.lbug"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	id := "pkg/foo.go::Method"
	require.NoError(t, s.UpsertSymbolFTS(id, "originalname"))
	require.NoError(t, s.BuildSymbolIndex())
	require.NoError(t, s.UpsertSymbolFTS(id, "renamedmethod"))

	// Old name should miss; new name should hit. Only one row total.
	missHits, err := s.SearchSymbols("originalname", 10)
	require.NoError(t, err)
	for _, h := range missHits {
		assert.NotEqual(t, id, h.NodeID, "old text must no longer match after upsert replacement")
	}
	freshHits, err := s.SearchSymbols("renamedmethod", 10)
	require.NoError(t, err)
	require.NotEmpty(t, freshHits)
	assert.Equal(t, id, freshHits[0].NodeID)
}
