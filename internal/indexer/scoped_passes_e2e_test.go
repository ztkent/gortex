package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func countImplements(g *graph.Graph) int {
	n := 0
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeImplements {
			n++
		}
	}
	return n
}

// TestScopedPasses_InterfaceChangeParity is the EvictFile regression guard:
// when an interface file is incrementally reindexed, EvictFile drops the
// inferred A->I / B->I edges whose source types live in UNCHANGED files. The
// scoped pass must re-derive them (because the interface is in the affected
// set), so the implements-edge count is unchanged — identical to the full pass.
func TestScopedPasses_InterfaceChangeParity(t *testing.T) {
	run := func(t *testing.T, scoped bool) int {
		if !scoped {
			t.Setenv("GORTEX_INDEX_SCOPED_GLOBAL_PASSES", "0")
		}
		dir := t.TempDir()
		write := func(name, body string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
		}
		write("iface.go", "package p\n\ntype I interface{ M() }\n")
		write("a.go", "package p\n\ntype A struct{}\n\nfunc (a A) M() {}\n")
		write("b.go", "package p\n\ntype B struct{}\n\nfunc (b B) M() {}\n")

		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		idx := New(g, reg, config.Default().Index, zap.NewNop())
		_, err := idx.Index(dir)
		require.NoError(t, err)
		idx.ResolveAll()

		before := countImplements(g)
		require.GreaterOrEqual(t, before, 2, "expected A->I and B->I before reindex")

		// Change the interface file → its node + the inferred in-edges from
		// a.go/b.go are evicted.
		write("iface.go", "package p\n\n// touched\ntype I interface{ M() }\n")
		_, err = idx.IncrementalReindexPaths(dir, []string{"iface.go"})
		require.NoError(t, err)

		after := countImplements(g)
		require.Equal(t, before, after,
			"implements edges must survive an interface-file reindex (scoped=%v)", scoped)
		return after
	}

	scopedCount := run(t, true)
	fullCount := run(t, false)
	require.Equal(t, fullCount, scopedCount, "scoped and full passes must produce the same implements-edge count")
}
