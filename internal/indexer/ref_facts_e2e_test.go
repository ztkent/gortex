package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestRefFacts_PersistedOnIndex indexes a Go fixture into a sqlite-backed graph
// and asserts the resolved A->B call is persisted as a durable reference fact
// (with provenance) in the ref_facts sidecar.
func TestRefFacts_PersistedOnIndex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "m.go"), []byte(
		"package p\n\nfunc B() {}\n\nfunc A() { B() }\n"), 0o644))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(store, reg, config.Default().Index, zap.NewNop())
	_, err = idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	facts, err := store.LoadRefFactsByFiles("", nil)
	require.NoError(t, err)
	require.NotEmpty(t, facts, "expected resolved reference facts persisted to the sidecar")

	var found bool
	for _, f := range facts {
		if f.Kind == "calls" && f.RefName == "B" {
			found = true
			require.NotEmpty(t, f.Origin, "fact must carry a provenance origin")
			require.NotEmpty(t, f.Tier, "fact must carry a coarse tier")
		}
	}
	require.True(t, found, "expected an A->B calls fact, got %+v", facts)
}
