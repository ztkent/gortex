package indexer

import (
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// scopedGlobalPassesEnabled reports whether incremental reindex should scope
// the global inference passes (InferImplements / InferOverrides) to the
// changed-affected type set instead of re-running them over the whole graph.
// GORTEX_INDEX_SCOPED_GLOBAL_PASSES overrides the config key. ON by default.
func (idx *Indexer) scopedGlobalPassesEnabled() bool {
	if v := os.Getenv("GORTEX_INDEX_SCOPED_GLOBAL_PASSES"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.ScopedGlobalPassesEnabledOrDefault()
}

// affectedTypeSet computes the type/interface IDs whose inferred
// implements/override edges a set of changed (stale) files can affect:
//   - every KindType / KindInterface the changed files define (their inferred
//     out/in edges were dropped on eviction), and
//   - the owning type of every KindMethod in a changed file (a method add/remove
//     changes the type's method-set, which can newly satisfy / break an
//     interface defined in an unchanged file).
//
// types is the full re-check set; ifaces is the subset of changed interfaces
// (every type must be re-checked against a changed interface). Re-checking
// every (type, interface) pair with an endpoint in these sets re-lands exactly
// the edges eviction dropped — add-parity with the full pass.
func (idx *Indexer) affectedTypeSet(graphPaths []string) (types, ifaces map[string]bool) {
	types = map[string]bool{}
	ifaces = map[string]bool{}
	for _, p := range graphPaths {
		for _, n := range idx.graph.GetFileNodes(p) {
			if n == nil {
				continue
			}
			switch n.Kind {
			case graph.KindType, graph.KindInterface:
				types[n.ID] = true
				if n.Kind == graph.KindInterface {
					ifaces[n.ID] = true
				}
			case graph.KindMethod:
				for _, e := range idx.graph.GetOutEdges(n.ID) {
					if e.Kind == graph.EdgeMemberOf {
						types[e.To] = true
					}
				}
			}
		}
	}
	return types, ifaces
}

// runScopedInferencePasses runs the implements/override inference passes scoped
// to the types/interfaces a set of stale files can affect. Returns false when
// scoping is disabled (caller should run the full passes). When nothing
// type/interface-shaped changed, the passes are skipped entirely (the common
// case for a function-body edit).
func (idx *Indexer) runScopedInferencePasses(staleFiles []string) bool {
	if !idx.scopedGlobalPassesEnabled() {
		return false
	}
	types, ifaces := idx.affectedTypeSet(idx.graphFilePaths(staleFiles))
	if len(types) == 0 && len(ifaces) == 0 {
		return true // no type/interface change → no inferred edges to re-derive
	}
	idx.resolver.InferImplementsScoped(types, ifaces)
	idx.resolver.InferOverridesScoped(types)
	return true
}
