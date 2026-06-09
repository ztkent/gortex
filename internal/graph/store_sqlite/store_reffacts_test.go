package store_sqlite_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func openRefFactStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "rf.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRefFacts_Roundtrip(t *testing.T) {
	s := openRefFactStore(t)
	facts := []graph.RefFact{
		{RepoPrefix: "", FromID: "a.go::A", ToID: "b.go::B", Kind: "calls", RefName: "B", Line: 7, Origin: "ast_resolved", Tier: "ast", FilePath: "a.go", Lang: "go", Candidates: []string{"b.go::B", "c.go::B"}},
		{RepoPrefix: "", FromID: "a.go::A", ToID: "d.go::D", Kind: "references", RefName: "D", Line: 9, Origin: "lsp_resolved", Tier: "lsp", FilePath: "a.go", Lang: "go"},
	}
	require.NoError(t, s.BulkSetRefFacts("", facts))

	got, err := s.LoadRefFactsByFiles("", []string{"a.go"})
	require.NoError(t, err)
	require.Len(t, got, 2)

	byTo := map[string]graph.RefFact{}
	for _, f := range got {
		byTo[f.ToID] = f
	}
	require.Equal(t, "ast_resolved", byTo["b.go::B"].Origin)
	require.Equal(t, []string{"b.go::B", "c.go::B"}, byTo["b.go::B"].Candidates)
	require.Equal(t, 7, byTo["b.go::B"].Line)
	require.Equal(t, "lsp_resolved", byTo["d.go::D"].Origin)

	// LoadRefFactsByFiles with empty file list returns all for the repo.
	all, err := s.LoadRefFactsByFiles("", nil)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestRefFacts_DeleteByFile(t *testing.T) {
	s := openRefFactStore(t)
	require.NoError(t, s.BulkSetRefFacts("", []graph.RefFact{
		{FromID: "a.go::A", ToID: "x", Kind: "calls", FilePath: "a.go"},
		{FromID: "b.go::B", ToID: "y", Kind: "calls", FilePath: "b.go"},
	}))
	require.NoError(t, s.DeleteRefFactsByFiles("", []string{"a.go"}))
	got, err := s.LoadRefFactsByFiles("", nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "b.go", got[0].FilePath)
}

func TestRefFacts_RepoScoping(t *testing.T) {
	s := openRefFactStore(t)
	require.NoError(t, s.BulkSetRefFacts("repoA", []graph.RefFact{{FromID: "f::A", ToID: "tA", Kind: "calls", FilePath: "f.go"}}))
	require.NoError(t, s.BulkSetRefFacts("repoB", []graph.RefFact{{FromID: "f::A", ToID: "tB", Kind: "calls", FilePath: "f.go"}}))

	a, err := s.LoadRefFactsByFiles("repoA", []string{"f.go"})
	require.NoError(t, err)
	require.Len(t, a, 1)
	require.Equal(t, "tA", a[0].ToID)

	// Deleting repoA's file must not touch repoB.
	require.NoError(t, s.DeleteRefFactsByFiles("repoA", []string{"f.go"}))
	b, err := s.LoadRefFactsByFiles("repoB", []string{"f.go"})
	require.NoError(t, err)
	require.Len(t, b, 1)
	require.Equal(t, "tB", b[0].ToID)
}

func TestRefFacts_Chunking(t *testing.T) {
	s := openRefFactStore(t)
	const n = 500 // > refFactChunk (80)
	facts := make([]graph.RefFact, n)
	for i := range facts {
		facts[i] = graph.RefFact{FromID: fmt.Sprintf("a.go::f%d", i), ToID: fmt.Sprintf("t%d", i), Kind: "calls", FilePath: "a.go"}
	}
	require.NoError(t, s.BulkSetRefFacts("", facts))
	got, err := s.LoadRefFactsByFiles("", []string{"a.go"})
	require.NoError(t, err)
	require.Len(t, got, n)
}

func TestRefFacts_EmptyNoop(t *testing.T) {
	s := openRefFactStore(t)
	require.NoError(t, s.BulkSetRefFacts("", nil))
	require.NoError(t, s.DeleteRefFactsByFiles("", nil))
	got, err := s.LoadRefFactsByFiles("", nil)
	require.NoError(t, err)
	require.Empty(t, got)
}
