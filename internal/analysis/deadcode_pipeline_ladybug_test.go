package analysis_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph/store_ladybug"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
)

// TestDeadCode_RealPipeline_LadybugResolve is the end-to-end guard for the
// reported bug: real, clearly-called Go functions were flagged as dead code
// because the ladybug backend resolver left their incoming call edges on an
// `unresolved::` stub (ResolveUniqueNames counted the stub as its own
// candidate) — so dead_code saw zero incoming usage edges.
//
// It drives the REAL pipeline against the REAL ladybug backend resolver:
//
//	extract internal/analysis/deadcode.go  ->  store.AddBatch
//	resolver.New(store).ResolveAll()       (runs ResolveAllBulk in-engine)
//	analysis.FindDeadCode(store)           ->  assertions
//
// isExportedSymbol and collectDeadCodeCandidates are both called by
// FindDeadCode inside this same file, so after resolution they MUST have an
// incoming calls edge and MUST NOT be reported dead. Synthetic stub /
// external nodes must never be reported either.
func TestDeadCode_RealPipeline_LadybugResolve(t *testing.T) {
	src, err := os.ReadFile("deadcode.go")
	if err != nil {
		t.Fatalf("read deadcode.go: %v", err)
	}
	res, err := languages.NewGoExtractor().Extract("internal/analysis/deadcode.go", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	store, err := store_ladybug.Open(filepath.Join(t.TempDir(), "dc.kuzu"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch(res.Nodes, res.Edges)
	resolver.New(store).ResolveAll()

	dead := analysis.FindDeadCode(store, nil, nil)
	flagged := make(map[string]bool, len(dead))
	for _, d := range dead {
		flagged[d.ID] = true
		// Cause A: no synthetic external/stub node may ever be reported.
		if isSyntheticID(d.ID) {
			t.Errorf("synthetic stub/external node reported as dead: %s (kind=%s)", d.ID, d.Kind)
		}
	}

	// These are unexported helpers that FindDeadCode calls within
	// deadcode.go — they have a real intra-file caller and must resolve.
	calledInFile := []string{
		"internal/analysis/deadcode.go::isExportedSymbol",
		"internal/analysis/deadcode.go::collectDeadCodeCandidates",
	}
	for _, id := range calledInFile {
		if flagged[id] {
			t.Errorf("FALSE POSITIVE: %s is called by FindDeadCode in-file but was flagged dead "+
				"(its incoming calls edge was not resolved)", id)
		}
	}
}

// isSyntheticID reports whether id is a resolver-minted external/stub target
// (stdlib::* / dep::* / external::* / external_call::* / builtin::* /
// module::*, with or without a repo prefix) rather than first-party code.
func isSyntheticID(id string) bool {
	for _, p := range []string{"stdlib::", "dep::", "external::", "external_call::", "builtin::", "module::", "unresolved::"} {
		if hasSeg(id, p) {
			return true
		}
	}
	return false
}

func hasSeg(id, prefix string) bool {
	if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
		return true
	}
	// repo-prefixed: <repo>::<prefix>...
	if i := indexOf(id, "::"+prefix); i >= 0 {
		return true
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
