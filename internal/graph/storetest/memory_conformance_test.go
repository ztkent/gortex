package storetest_test

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/storetest"
)

// TestMemoryStoreConformance proves the in-memory *graph.Graph (the
// only Store impl that exists today) satisfies the conformance suite.
// This is the canonical baseline; new backends must pass the same
// battery.
func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) graph.Store {
		return graph.New()
	})
}
