package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Cross-package name-match guard.
//
// The heuristic cascade in resolveFunctionCall / resolveMethodCall ends,
// for calls it can't pin precisely, in a name-only fallback: "the first
// function/method named X in the caller's repo". When the only candidate
// of that name lives in a package the caller never imports, that
// fallback manufactures a false `calls` edge — a JS/TS factory result
// `h.handle()` binding to an unrelated `handle`, or a `ns.foo()`
// namespace call binding to a free `foo` in some other module.
//
// This guard runs once after the main resolution pass. For every edge
// the pass resolved at one of the two weakest confidence tiers
// (text_matched / ast_inferred) it asks a single question: is the
// resolved target import-reachable from the call site? Reachable means
// the target sits in the caller's own directory (same package) or in a
// directory the caller's file imports. When it is not, the edge is
// reverted to its pre-resolution `unresolved::` target so a
// higher-evidence resolver (CrossRepoResolver, or a later LSP-backed
// pass) can have a clean attempt instead of inheriting a wrong binding.
//
// Genuine same-package and imported-target edges are never touched: the
// reachability set always contains the caller's own directory, and an
// imported package contributes its directory to the set. Edges resolved
// at ast_resolved or above are out of scope — those carry structural or
// compiler-grade evidence the name-only fallback never had.

// guardCrossPackageCallEdges inspects the edges mutated by the just-
// completed resolution pass and reverts any weak-tier call/reference
// edge whose resolved target is not import-reachable from the caller.
// jobs are the reindexJob records produced by ResolveAll's worker
// phase; each carries the edge's pre-resolution target in oldTo, so a
// reverted edge is restored exactly. closure is the import-reachability
// map from buildImportClosure. Returns the number of edges reverted.
func (r *Resolver) guardCrossPackageCallEdges(jobs []reindexJob, closure map[string]map[string]struct{}) int {
	if len(jobs) == 0 {
		return 0
	}
	// Collect both mutation lists across the whole pass and apply them
	// via the batched Store methods at the end. Per-edge
	// SetEdgeProvenance + ReindexEdge in the body would otherwise pay
	// two ACID round-trips per reverted edge against disk backends —
	// catastrophic on a 30k-job pass.
	var provBatch []graph.EdgeProvenanceUpdate
	var reindexBatch []graph.EdgeReindex
	for i := range jobs {
		j := &jobs[i]
		if !isCallLikeEdge(j.kind) {
			continue
		}
		// Only the two weakest tiers — a name-only guess — are in scope.
		// DefaultOriginFor backfills the tier for edges whose Origin the
		// resolver left unset (the heuristic fallbacks never stamp it).
		origin := j.origin
		if origin == "" {
			origin = graph.DefaultOriginFor(j.kind, j.confidence, "")
		}
		if origin != graph.OriginTextMatched && origin != graph.OriginASTInferred {
			continue
		}
		// The pre-resolution target must be a bare-name placeholder —
		// `unresolved::Foo` (function call) or `unresolved::*.foo`
		// (member call). Anything else carries evidence the name-only
		// fallback never had and is out of scope: `extern::` pins an
		// import path, `grpc::` / `pyrel::` / `import::` are owned by
		// dedicated passes, and a non-`unresolved::` target was never a
		// guess to begin with.
		if !isBareNameCallTarget(j.oldTo) {
			continue
		}
		callerFile := r.edgeCallerFile(j.edge)
		target := r.graph.GetNode(j.newTo)
		if callerFile == "" || target == nil {
			continue
		}
		if r.targetImportReachable(callerFile, target, closure) {
			continue
		}
		// Not reachable — revert to the unresolved placeholder and
		// re-index against the resolved target we are abandoning.
		// SetEdgeProvenance("") drops the resolution provenance so
		// the reverted edge's identity change is counted; the target
		// revert + re-bucket follows. Both go in their respective
		// batches so the whole pass commits in two chunks instead of
		// 2×N per-edge transactions.
		oldResolved := j.edge.To
		provBatch = append(provBatch, graph.EdgeProvenanceUpdate{Edge: j.edge, NewOrigin: ""})
		j.edge.To = j.oldTo
		j.edge.Confidence = 0
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: j.edge, OldTo: oldResolved})
	}
	if len(provBatch) > 0 {
		r.graph.SetEdgeProvenanceBatch(provBatch)
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
	return len(reindexBatch)
}

// isBareNameCallTarget reports whether an unresolved edge target is a
// bare-name call placeholder — `unresolved::Foo` for a free-function
// call or `unresolved::*.foo` for a member call. These are the only
// shapes the name-only resolution fallback acts on. Targets that embed
// further structure (`unresolved::extern::path::sym`, `grpc::`,
// `pyrel::`, `import::`) carry evidence the fallback never had and are
// resolved by other code paths, so the guard leaves them alone.
func isBareNameCallTarget(target string) bool {
	rest, ok := strings.CutPrefix(target, unresolvedPrefix)
	if !ok || rest == "" {
		return false
	}
	rest = strings.TrimPrefix(rest, "*.")
	if rest == "" {
		return false
	}
	// A remaining `::` means the placeholder is one of the structured
	// forms (extern::, grpc::, pyrel::, import::), not a bare name.
	return !strings.Contains(rest, "::")
}

// isCallLikeEdge reports whether an edge kind is one the guard polices.
// EdgeCalls is the obvious case; EdgeReferences is included because the
// resolver promotes a call-shaped EdgeReads to EdgeReferences once it
// learns the target is a function/method, and that promotion runs
// through the very same name-only fallback.
func isCallLikeEdge(k graph.EdgeKind) bool {
	return k == graph.EdgeCalls || k == graph.EdgeReferences
}

// edgeCallerFile returns the file path of the node that owns the edge's
// From end. Empty when the caller node is unknown.
func (r *Resolver) edgeCallerFile(e *graph.Edge) string {
	if n := r.graph.GetNode(e.From); n != nil && n.FilePath != "" {
		return n.FilePath
	}
	return e.FilePath
}

// targetImportReachable reports whether target sits in a package the
// caller's file can see: the caller's own directory (same package), or
// a directory present in the caller's import closure.
func (r *Resolver) targetImportReachable(callerFile string, target *graph.Node, closure map[string]map[string]struct{}) bool {
	if target.FilePath == "" {
		// A target with no file (synthetic / external stub) can't be
		// shown unreachable — leave the edge alone.
		return true
	}
	callerDir := filepath.Dir(callerFile)
	targetDir := filepath.Dir(target.FilePath)
	if targetDir == callerDir {
		return true
	}
	dirs, ok := closure[callerFile]
	if !ok {
		// No closure entry for the caller (its file node or imports were
		// not indexed). Be conservative: without evidence of isolation
		// we keep the edge rather than risk dropping a real one.
		return true
	}
	_, reachable := dirs[targetDir]
	return reachable
}

// buildImportClosure maps each caller file path to the set of directories
// it can reach by import. The set is seeded with the file's own directory
// and extended with the directory of every node its resolved EdgeImports
// edges point at. It is built from the post-resolution graph — by the
// time the guard runs, import edges have been resolved to real file /
// package nodes, so this closure captures JS/TS relative-file imports
// that the pre-resolution reachability index (keyed on directory-shaped
// import paths) structurally misses.
func (r *Resolver) buildImportClosure() map[string]map[string]struct{} {
	closure := make(map[string]map[string]struct{})
	add := func(file, dir string) {
		if file == "" || dir == "" {
			return
		}
		set := closure[file]
		if set == nil {
			set = make(map[string]struct{})
			closure[file] = set
		}
		set[dir] = struct{}{}
	}
	for n := range r.graph.NodesByKind(graph.KindFile) {
		if n.FilePath != "" {
			add(n.FilePath, filepath.Dir(n.FilePath))
		}
	}
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		// Skip imports still pointing at an unresolved placeholder or an
		// out-of-repo stub — neither names an in-repo directory that a
		// name-only call candidate could legitimately live in.
		if strings.HasPrefix(e.To, unresolvedPrefix) ||
			strings.HasPrefix(e.To, "external::") ||
			strings.HasPrefix(e.To, "stdlib::") ||
			strings.HasPrefix(e.To, "dep::") {
			continue
		}
		callerFile := r.edgeCallerFile(e)
		if target := r.graph.GetNode(e.To); target != nil && target.FilePath != "" {
			add(callerFile, filepath.Dir(target.FilePath))
		}
	}
	return closure
}
