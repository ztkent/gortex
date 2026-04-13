package resolver

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// CrossRepoStats holds counts from a cross-repo resolution pass.
type CrossRepoStats struct {
	Resolved       int            `json:"resolved"`
	Unresolved     int            `json:"unresolved"`
	CrossRepoEdges int            `json:"cross_repo_edges"`
	ByRepo         map[string]int `json:"by_repo"`
}

// CrossRepoResolver resolves unresolved edges across repository boundaries.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single Resolve* pass — they let resolveImport look up candidate
// file nodes by directory in O(1) instead of scanning the whole graph
// (which is O(N) per import edge, O(N×M) total). Maps are nil between
// passes so we don't pay the memory cost while idle.
type CrossRepoResolver struct {
	graph        *graph.Graph
	dirIndex     map[string][]*graph.Node
	lastDirIndex map[string][]*graph.Node
}

// NewCrossRepo creates a CrossRepoResolver for the given graph.
func NewCrossRepo(g *graph.Graph) *CrossRepoResolver {
	return &CrossRepoResolver{graph: g}
}

// ResolveAll resolves all unresolved edges in the graph, trying same-repo
// matches first, then cross-repo search. Sets Edge.CrossRepo = true for
// cross-repo matches.
func (cr *CrossRepoResolver) ResolveAll() *CrossRepoStats {
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

	// 1. Prefer same-repo match.
	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// 2. Cross-repo fallback: first function/method match from any repo.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			e.CrossRepo = true
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	stats.Unresolved++
}

func (cr *CrossRepoResolver) resolveImport(e *graph.Edge, importPath string, stats *CrossRepoStats) {
	callerRepo := cr.callerRepoPrefix(e)

	// Look for a package node with matching qualified name.
	node := cr.graph.GetNodeByQualName(importPath)
	if node != nil {
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
		// Cross-repo + exact type.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod && nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.CrossRepo = true
				e.Confidence = 0.85
				stats.Resolved++
				stats.CrossRepoEdges++
				stats.ByRepo[c.RepoPrefix]++
				return
			}
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
		if c.Kind == graph.KindMethod {
			e.To = c.ID
			e.CrossRepo = true
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && c.RepoPrefix == callerRepo {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction {
			e.To = c.ID
			e.CrossRepo = true
			stats.Resolved++
			stats.CrossRepoEdges++
			stats.ByRepo[c.RepoPrefix]++
			return
		}
	}

	stats.Unresolved++
}
