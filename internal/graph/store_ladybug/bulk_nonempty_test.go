package store_ladybug_test

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	store_ladybug "github.com/zzet/gortex/internal/graph/store_ladybug"
)

// TestCopyBulk_SecondLoadIntoNonEmpty reproduces the fresh-cold-load
// failure: each per-repo Indexer drains to the shared store via its own
// BeginBulkLoad/FlushBulk. The first repo COPYs into an empty Node
// table (fine); every subsequent repo COPYs into a non-empty Node table
// and Kuzu rejects it with "COPY into a non-empty primary-key node
// table without a hash index is not supported" — so on a fresh store
// only the first repo persists.
func TestCopyBulk_SecondLoadIntoNonEmpty(t *testing.T) {
	s, err := store_ladybug.Open(filepath.Join(t.TempDir(), "x.kuzu"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	load := func(prefix, file, name string) error {
		s.BeginBulkLoad()
		s.AddBatch(
			[]*graph.Node{{
				ID: file + "::" + name, Name: name, Kind: graph.KindFunction,
				FilePath: file, RepoPrefix: prefix, StartLine: 1, EndLine: 2,
				Meta: map[string]any{"k": "v"},
			}},
			[]*graph.Edge{{
				From: file + "::" + name, To: "unresolved::Other",
				Kind: graph.EdgeCalls, FilePath: file, Line: 1,
			}},
		)
		return s.FlushBulk()
	}

	if err := load("repoA", "a/x.go", "Alpha"); err != nil {
		t.Fatalf("first bulk load (empty table): %v", err)
	}
	// Second load: the Node table is now non-empty.
	if err := load("repoB", "b/y.go", "Beta"); err != nil {
		t.Fatalf("second bulk load (non-empty table): %v", err)
	}

	if s.GetNode("a/x.go::Alpha") == nil {
		t.Error("Alpha (repo A) missing after second load")
	}
	if s.GetNode("b/y.go::Beta") == nil {
		t.Error("Beta (repo B) missing — its COPY into the non-empty table was dropped")
	}
}
