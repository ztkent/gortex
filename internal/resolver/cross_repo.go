package resolver

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// CrossRepoStats holds counts from a cross-repo resolution pass.
type CrossRepoStats struct {
	Resolved       int            `json:"resolved"`
	Unresolved     int            `json:"unresolved"`
	CrossRepoEdges int            `json:"cross_repo_edges"`
	ByRepo         map[string]int `json:"by_repo"`
}

// CrossWorkspaceDepRule names one allowed dependency from a source
// workspace into another. Mirrors config.CrossWorkspaceDep but lives
// here so the resolver doesn't import internal/config (avoids a cycle
// once future steps wire workspace plumbing through manager.go).
type CrossWorkspaceDepRule struct {
	// Workspace is the *target* workspace slug — the workspace whose
	// nodes are eligible to be referenced from the source workspace.
	Workspace string
	// Modules is the list of import-path prefixes that the source
	// workspace is allowed to follow into the target. Iteration 1
	// only supports prefix-style matches (longest prefix wins).
	Modules []string
}

// CrossWorkspaceDepLookup returns the list of declared cross-workspace
// dependencies for a *source* workspace. Empty / nil result means the
// source workspace has no declared cross-workspace deps and so the
// resolver must keep cross-workspace candidates ineligible.
type CrossWorkspaceDepLookup func(sourceWorkspaceID string) []CrossWorkspaceDepRule

// CrossRepoResolver resolves unresolved edges across repository boundaries.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single Resolve* pass — they let resolveImport look up candidate
// file nodes by directory in O(1) instead of scanning the whole graph
// (which is O(N) per import edge, O(N×M) total). Maps are nil between
// passes so we don't pay the memory cost while idle.
//
// mu is the graph-wide resolver lock shared with every Resolver built
// from the same Graph. Private to CrossRepoResolver wasn't enough:
// MultiWatcher.forwardEvents calls ResolveForRepo while the per-repo
// Watcher's debounce timer concurrently calls Resolver.ResolveFile,
// and both paths iterate graph.AllEdges() / AllNodes() and mutate
// Edge.To in place. Sharing g.ResolveMutex() serialises both resolver
// types against the same graph.
//
// crossWorkspaceLookup is the workspace-boundary check. Empty (nil)
// means the resolver is in legacy mode: cross-repo / cross-workspace
// candidates resolve as if no boundary existed — for callers that
// haven't plumbed config through yet. When set, candidates whose
// WorkspaceID differs from
// the caller's are accepted only when the source workspace declared
// the target workspace via `cross_workspace_deps` AND, for import
// edges, the import path has a declared-module prefix.
type CrossRepoResolver struct {
	graph                *graph.Graph
	dirIndex             map[string][]*graph.Node
	lastDirIndex         map[string][]*graph.Node
	mu                   *sync.Mutex
	crossWorkspaceLookup CrossWorkspaceDepLookup
}

// NewCrossRepo creates a CrossRepoResolver for the given graph.
func NewCrossRepo(g *graph.Graph) *CrossRepoResolver {
	return &CrossRepoResolver{graph: g, mu: g.ResolveMutex()}
}

// SetCrossWorkspaceDepLookup wires the boundary rule. After this
// call, the resolver will refuse cross-workspace candidates that
// aren't covered by an explicit declaration in the source workspace's
// `cross_workspace_deps`. Legacy graphs (no WorkspaceID on either
// side) keep working — when both From and To carry empty workspace
// slugs the boundary check trivially passes.
func (cr *CrossRepoResolver) SetCrossWorkspaceDepLookup(lookup CrossWorkspaceDepLookup) {
	cr.crossWorkspaceLookup = lookup
}

// callerWorkspaceID returns the workspace slug for the From-side of
// an edge. Falls back to RepoPrefix to match Contract.Effective-
// Workspace's "missing → repo-name" rule.
func (cr *CrossRepoResolver) callerWorkspaceID(e *graph.Edge) string {
	from := cr.graph.GetNode(e.From)
	if from == nil {
		return ""
	}
	if from.WorkspaceID != "" {
		return from.WorkspaceID
	}
	return from.RepoPrefix
}

// candidateWorkspaceID extracts the same slug from a candidate node.
func candidateWorkspaceID(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if n.WorkspaceID != "" {
		return n.WorkspaceID
	}
	return n.RepoPrefix
}

// crossWorkspaceEligible reports whether sourceWS is permitted to
// reach a candidate in targetWS, optionally constrained by the
// candidate's import path. importPath == "" means "any module"
// (function/method calls — they don't carry an import path so the
// only check is workspace-pair declaration).
func (cr *CrossRepoResolver) crossWorkspaceEligible(sourceWS, targetWS, importPath string) bool {
	if sourceWS == targetWS {
		return true
	}
	if cr.crossWorkspaceLookup == nil {
		// Legacy / unwired callers: no boundary enforcement.
		return true
	}
	rules := cr.crossWorkspaceLookup(sourceWS)
	for _, rule := range rules {
		if rule.Workspace != targetWS {
			continue
		}
		if importPath == "" {
			// Function/method call into a declared cross-workspace
			// dep is allowed once the workspace pair is declared —
			// iteration 1 doesn't try to require an import-path
			// match for non-import edges.
			return true
		}
		for _, m := range rule.Modules {
			if m == importPath || strings.HasPrefix(importPath, m+"/") {
				return true
			}
		}
	}
	return false
}

// ResolveAll resolves all unresolved edges in the graph, trying same-repo
// matches first, then cross-repo search. Sets Edge.CrossRepo = true for
// cross-repo matches.
func (cr *CrossRepoResolver) ResolveAll() *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	cr.buildDirIndexes()
	defer cr.clearDirIndexes()

	stats := &CrossRepoStats{ByRepo: make(map[string]int)}

	edges := cr.graph.AllEdges()
	for _, e := range edges {
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			continue
		}
		cr.resolveEdge(e, stats)
	}
	return stats
}

// ResolveForRepo resolves only unresolved edges originating from nodes
// in the specified repository.
func (cr *CrossRepoResolver) ResolveForRepo(repoPrefix string) *CrossRepoStats {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	cr.buildDirIndexes()
	defer cr.clearDirIndexes()

	stats := &CrossRepoStats{ByRepo: make(map[string]int)}

	nodes := cr.graph.GetRepoNodes(repoPrefix)
	for _, n := range nodes {
		edges := cr.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			cr.resolveEdge(e, stats)
		}
	}
	return stats
}

// buildDirIndexes walks the graph once and populates two lookup maps
// used by resolveImport — the only resolution path that previously
// scanned every node per edge.
//
//   - dirIndex     keys on filepath.Dir(file.FilePath) for exact matches
//     (importPath equal to the file's directory).
//   - lastDirIndex keys on the last path component of that directory,
//     covering the common case where an import path is a single name
//     like "logger" and we want any file under .../logger/.
//
// These maps are torn down via clearDirIndexes when the pass completes
// so we don't keep ~N pointers alive between resolves.
func (cr *CrossRepoResolver) buildDirIndexes() {
	nodes := cr.graph.AllNodes()
	cr.dirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	cr.lastDirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	for _, n := range nodes {
		if n.Kind != graph.KindFile {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		cr.dirIndex[dir] = append(cr.dirIndex[dir], n)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			cr.lastDirIndex[last] = append(cr.lastDirIndex[last], n)
		}
	}
}

func (cr *CrossRepoResolver) clearDirIndexes() {
	cr.dirIndex = nil
	cr.lastDirIndex = nil
}

func (cr *CrossRepoResolver) resolveEdge(e *graph.Edge, stats *CrossRepoStats) {
	oldTo := e.To
	target := strings.TrimPrefix(e.To, unresolvedPrefix)

	switch {
	case strings.HasPrefix(target, "import::"):
		cr.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		cr.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	default:
		cr.resolveFunctionCall(e, target, stats)
	}

	if e.To != oldTo {
		cr.graph.ReindexEdge(e, oldTo)
	}
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (cr *CrossRepoResolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := cr.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}

func (cr *CrossRepoResolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	candidates := cr.graph.FindNodesByName(funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback: first function/method match honoring the
	// workspace boundary. Same-workspace cross-repo is always
	// eligible; cross-workspace requires a declared
	// cross_workspace_deps entry covering the workspace pair.
	for _, c := range candidates {
		if c.Kind != graph.KindFunction && c.Kind != graph.KindMethod {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}

	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveImport(e *graph.Edge, importPath string, stats *CrossRepoStats) {
	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)

	// Look for a package node with matching qualified name.
	node := cr.graph.GetNodeByQualName(importPath)
	if node != nil {
		// Workspace boundary check: if the candidate is in a
		// different workspace, allow only when an explicit
		// cross_workspace_dep declares it.
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(node), importPath) {
			// Treat as external — the dep wasn't opted in.
			e.To = "external::" + importPath
			stats.Unresolved++
			return
		}
		e.To = node.ID
		if node.RepoPrefix != callerRepo {
			e.CrossRepo = true
			stats.CrossRepoEdges++
			stats.ByRepo[node.RepoPrefix]++
		}
		stats.Resolved++
		return
	}

	// Look for file nodes whose directory matches the import path. Two
	// inverted indexes (built once per Resolve* pass) replace what used
	// to be an O(N) scan of the entire graph per import edge.
	//
	// 1. Exact dir match — `dirIndex[importPath]` covers the case where
	//    the import literally equals a known directory.
	// 2. Last-component match — `lastDirIndex[lastPathComponent(...)]`
	//    covers the common case where an import path is just a name
	//    (e.g. "logger") and any file under .../logger/ is a candidate.
	//
	// Falls back to a full graph scan if the indexes are unset (defensive
	// — only happens when resolveImport is called outside a Resolve* pass).
	var sameRepo, crossRepo *graph.Node
	consider := func(n *graph.Node) {
		if n.Kind != graph.KindFile {
			return
		}
		if n.RepoPrefix == callerRepo {
			if sameRepo == nil {
				sameRepo = n
			}
			return
		}
		if crossRepo == nil {
			crossRepo = n
		}
	}
	if cr.dirIndex != nil {
		for _, n := range cr.dirIndex[importPath] {
			consider(n)
			if sameRepo != nil {
				break
			}
		}
		if sameRepo == nil {
			for _, n := range cr.lastDirIndex[lastPathComponent(importPath)] {
				consider(n)
				if sameRepo != nil {
					break
				}
			}
		}
	} else {
		for _, n := range cr.graph.AllNodes() {
			if n.Kind != graph.KindFile {
				continue
			}
			dir := filepath.Dir(n.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(n)
				if sameRepo != nil {
					break
				}
			}
		}
	}

	if sameRepo != nil {
		e.To = sameRepo.ID
		stats.Resolved++
		return
	}
	if crossRepo != nil {
		// Apply workspace boundary on the directory-match path too.
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(crossRepo), importPath) {
			e.To = "external::" + importPath
			stats.Unresolved++
			return
		}
		e.To = crossRepo.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[crossRepo.RepoPrefix]++
		return
	}

	// External/unresolvable import.
	e.To = "external::" + importPath
	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveMethodCall(e *graph.Edge, methodName string, stats *CrossRepoStats) {
	candidates := cr.graph.FindNodesByName(methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	receiverType := edgeReceiverType(e)

	// If we have a type hint, try exact type match first.
	if receiverType != "" {
		// Same-repo + exact type.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod &&
				c.RepoPrefix == callerRepo &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return
			}
		}
		// Cross-repo + exact type — bounded by workspace check.
		for _, c := range candidates {
			if c.Kind != graph.KindMethod || nodeReceiverType(c) != receiverType {
				continue
			}
			if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
				continue
			}
			e.To = c.ID
			e.CrossRepo = true
			e.Confidence = 0.85
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	// Fallback: name-only matching (methods first, then functions for pkg.Func() calls).
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindMethod {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind != graph.KindFunction {
			continue
		}
		if !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		e.To = c.ID
		e.CrossRepo = true
		stats.Resolved++
		stats.CrossRepoEdges++
		stats.ByRepo[c.RepoPrefix]++
		return
	}

	stats.Unresolved++
}
