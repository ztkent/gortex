package store_kuzu_test

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_kuzu"
	"github.com/zzet/gortex/internal/graph/storetest"
)

func TestKuzuStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) graph.Store {
		dir := t.TempDir()
		s, err := store_kuzu.Open(filepath.Join(dir, "test.kuzu"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
