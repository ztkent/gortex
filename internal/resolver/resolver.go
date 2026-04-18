package resolver

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedPrefix = "unresolved::"

// ResolveStats holds counts from a resolution pass.
type ResolveStats struct {
	Resolved   int `json:"resolved"`
	Unresolved int `json:"unresolved"`
	External   int `json:"external"`
}

// Resolver resolves unresolved edge targets to actual graph node IDs.
//
// dirIndex / lastDirIndex are scratch maps populated for the duration
// of a single ResolveAll/ResolveFile pass so resolveImport can look up
// candidate file nodes in O(1) instead of scanning the whole graph per
// import edge. On large repos (vscode ≈ 150k nodes / 5k imports) the
// old full scan made ResolveAll the dominant cost of a cold index
// (8m of a 9m wall-clock). Maps are cleared between passes.
//
// mu serializes ResolveAll and ResolveFile because both reset and
// repopulate the scratch maps as part of their first step. Without
// it, two concurrent file-watcher debounce goroutines firing on the
// same per-repo Indexer (each calls Resolver.ResolveFile via
// Indexer.IndexFile) crash the daemon with "concurrent map writes"
// in buildDirIndexes.
type Resolver struct {
	graph        *graph.Graph
	dirIndex     map[string][]*graph.Node
	lastDirIndex map[string][]*graph.Node
	// mu serialises resolution phases against the shared graph.
	// Pointer so every Resolver built from the same *graph.Graph
	// locks the same mutex — necessary for MultiIndexer's per-repo
	// goroutines, each of which spawns its own Resolver instance.
	// Without the shared lock, concurrent ResolveAll passes race on
	// edge mutations (resolveImport writes e.To while another
	// goroutine iterates via graph.AllEdges()).
	mu *sync.Mutex
}

// New creates a Resolver for the given graph. The returned Resolver
// shares graph.ResolveMutex() with every other Resolver built from
// the same Graph, so their ResolveAll / ResolveFile calls serialise
// end-to-end.
func New(g *graph.Graph) *Resolver {
	return &Resolver{graph: g, mu: g.ResolveMutex()}
}

// ResolveAll resolves all unresolved edges in the graph.
func (r *Resolver) ResolveAll() *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buildDirIndexes()
	defer r.clearDirIndexes()

	stats := &ResolveStats{}

	edges := r.graph.AllEdges()
	for _, e := range edges {
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			continue
		}
		r.resolveEdge(e, stats)
	}
	return stats
}

// buildDirIndexes builds two lookup maps for resolveImport. Populated
// once per ResolveAll / ResolveFile pass and torn down after.
//
//   - dirIndex     keys on filepath.Dir(file.FilePath) for exact
//     importPath == dir matches.
//   - lastDirIndex keys on the last path component of that directory
//     so an import of "logger" matches any file under .../logger/.
func (r *Resolver) buildDirIndexes() {
	nodes := r.graph.AllNodes()
	r.dirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	r.lastDirIndex = make(map[string][]*graph.Node, len(nodes)/4)
	for _, n := range nodes {
		if n.Kind != graph.KindFile {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		r.dirIndex[dir] = append(r.dirIndex[dir], n)
		last := lastPathComponent(dir)
		if last != "" && last != dir {
			r.lastDirIndex[last] = append(r.lastDirIndex[last], n)
		}
	}
}

func (r *Resolver) clearDirIndexes() {
	r.dirIndex = nil
	r.lastDirIndex = nil
}

// ResolveFile resolves unresolved edges originating from a specific file.
func (r *Resolver) ResolveFile(filePath string) *ResolveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.buildDirIndexes()
	defer r.clearDirIndexes()

	stats := &ResolveStats{}

	// Get all nodes in the file, then check their outgoing edges.
	nodes := r.graph.GetFileNodes(filePath)
	for _, n := range nodes {
		edges := r.graph.GetOutEdges(n.ID)
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			r.resolveEdge(e, stats)
		}
	}
	return stats
}

func (r *Resolver) resolveEdge(e *graph.Edge, stats *ResolveStats) {
	oldTo := e.To
	target := strings.TrimPrefix(e.To, unresolvedPrefix)

	switch {
	case strings.HasPrefix(target, "import::"):
		r.resolveImport(e, strings.TrimPrefix(target, "import::"), stats)
	case strings.HasPrefix(target, "*."):
		// Method call or method-value reference (e.g. h.handleHealth)
		r.resolveMethodCall(e, strings.TrimPrefix(target, "*."), stats)
	default:
		// For instantiates/references edges, try to resolve as a type first;
		// for calls edges, resolve as a function (original behavior).
		if e.Kind == graph.EdgeInstantiates || e.Kind == graph.EdgeReferences {
			r.resolveTypeOrFunc(e, target, stats)
		} else {
			r.resolveFunctionCall(e, target, stats)
		}
	}

	// Update inEdges index if the target changed during resolution.
	if e.To != oldTo {
		r.graph.ReindexEdge(e, oldTo)
	}
}

func (r *Resolver) resolveImport(e *graph.Edge, importPath string, stats *ResolveStats) {
	callerRepo := r.callerRepoPrefix(e)

	// Look for a package node with matching qualified name.
	node := r.graph.GetNodeByQualName(importPath)
	if node != nil {
		e.To = node.ID
		if callerRepo != "" && node.RepoPrefix != "" && node.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return
	}

	// Inverted-index lookup instead of a per-edge AllNodes() scan —
	// the old scan was O(N) per import and the dominant cost of
	// ResolveAll on large repos (e.g. vscode: 5k imports × 150k nodes
	// = 750M comparisons per cold index). Falls back to a scan only
	// when the indexes aren't populated (ResolveEdge invoked outside
	// of ResolveAll/ResolveFile).
	var sameRepo, crossRepoNode *graph.Node
	consider := func(n *graph.Node) {
		if n.Kind != graph.KindFile {
			return
		}
		if callerRepo == "" || n.RepoPrefix == callerRepo {
			if sameRepo == nil {
				sameRepo = n
			}
			return
		}
		if crossRepoNode == nil {
			crossRepoNode = n
		}
	}
	if r.dirIndex != nil {
		for _, n := range r.dirIndex[importPath] {
			consider(n)
			if sameRepo != nil {
				break
			}
		}
		if sameRepo == nil {
			for _, n := range r.lastDirIndex[lastPathComponent(importPath)] {
				consider(n)
				if sameRepo != nil {
					break
				}
			}
		}
	} else {
		for _, n := range r.graph.AllNodes() {
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
	if crossRepoNode != nil {
		e.To = crossRepoNode.ID
		if callerRepo != "" && crossRepoNode.RepoPrefix != "" && crossRepoNode.RepoPrefix != callerRepo {
			e.CrossRepo = true
		}
		stats.Resolved++
		return
	}

	// External/unresolvable import — create a stub target ID.
	e.To = "external::" + importPath
	stats.External++
}

func (r *Resolver) resolveFunctionCall(e *graph.Edge, funcName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(funcName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	// Prefer same-package (same directory) match.
	callerFile := e.FilePath
	callerDir := filepath.Dir(callerFile)

	for _, c := range candidates {
		if (c.Kind == graph.KindFunction || c.Kind == graph.KindMethod) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to first function match (may be cross-repo).
	callerRepo := r.callerRepoPrefix(e)
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// resolveTypeOrFunc resolves unresolved edges that could be either a type
// reference (composite literal, type assertion) or a function reference.
// It first tries to match a type/interface node, then falls back to functions.
func (r *Resolver) resolveTypeOrFunc(e *graph.Edge, name string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(name)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerFile := e.FilePath
	callerDir := filepath.Dir(callerFile)

	// Prefer same-package type match.
	for _, c := range candidates {
		if (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
			filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	// Fall back to any type match.
	callerRepo := r.callerRepoPrefix(e)
	for _, c := range candidates {
		if c.Kind == graph.KindType || c.Kind == graph.KindInterface {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	// If no type found, try as function (e.g., bare function name passed as value).
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			if filepath.Dir(c.FilePath) == callerDir {
				e.To = c.ID
				stats.Resolved++
				return
			}
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction || c.Kind == graph.KindMethod {
			e.To = c.ID
			if callerRepo != "" && c.RepoPrefix != "" && c.RepoPrefix != callerRepo {
				e.CrossRepo = true
			}
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

func (r *Resolver) resolveMethodCall(e *graph.Edge, methodName string, stats *ResolveStats) {
	candidates := r.graph.FindNodesByName(methodName)
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}

	callerDir := filepath.Dir(e.FilePath)
	receiverType := edgeReceiverType(e)

	// If we have a type hint, try exact type match first.
	if receiverType != "" {
		// Pass 1: same-directory + exact type match (highest confidence).
		for _, c := range candidates {
			if c.Kind == graph.KindMethod &&
				filepath.Dir(c.FilePath) == callerDir &&
				nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.95
				stats.Resolved++
				return
			}
		}
		// Pass 2: exact type match, any directory.
		for _, c := range candidates {
			if c.Kind == graph.KindMethod && nodeReceiverType(c) == receiverType {
				e.To = c.ID
				e.Confidence = 0.85
				stats.Resolved++
				return
			}
		}
	}

	// Fallback: infer receiver type from the caller node.
	// If the caller is a method on type X and there's a candidate method on
	// type X with the same name, prefer it.  This handles e.extractFunctions()
	// where the type env doesn't have a hint for parameter-bound receivers.
	callerNode := r.graph.GetNode(e.From)
	if callerNode != nil && callerNode.Kind == graph.KindMethod {
		callerRecv := nodeReceiverType(callerNode)
		if callerRecv != "" {
			// Same receiver type + same directory = very high confidence.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod &&
					filepath.Dir(c.FilePath) == callerDir &&
					nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.9
					stats.Resolved++
					return
				}
			}
			// Same receiver type, any directory.
			for _, c := range candidates {
				if c.Kind == graph.KindMethod && nodeReceiverType(c) == callerRecv {
					e.To = c.ID
					e.Confidence = 0.8
					stats.Resolved++
					return
				}
			}
		}
	}

	// Final fallback: name-only heuristic (methods first, then functions for pkg.Func() calls).
	for _, c := range candidates {
		if c.Kind == graph.KindMethod && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindMethod {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	// Package-qualified function calls (e.g. parser.ParseFile) arrive here
	// because the extractor sees "pkg.Func()" as a selector call with "*." prefix.
	for _, c := range candidates {
		if c.Kind == graph.KindFunction && filepath.Dir(c.FilePath) == callerDir {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}
	for _, c := range candidates {
		if c.Kind == graph.KindFunction {
			e.To = c.ID
			stats.Resolved++
			return
		}
	}

	stats.Unresolved++
}

// edgeReceiverType extracts the receiver_type from Edge.Meta, if present.
func edgeReceiverType(e *graph.Edge) string {
	if e.Meta == nil {
		return ""
	}
	if rt, ok := e.Meta["receiver_type"].(string); ok {
		return rt
	}
	return ""
}

// nodeReceiverType extracts the receiver type from a method Node.Meta.
func nodeReceiverType(n *graph.Node) string {
	if n.Meta == nil {
		return ""
	}
	if rt, ok := n.Meta["receiver"].(string); ok {
		return rt
	}
	return ""
}

// InferImplements detects structural interface satisfaction by comparing
// method sets and adds EdgeImplements edges from types to interfaces.
// Returns the number of edges added.
func (r *Resolver) InferImplements() int {
	// Step 1: Collect all interfaces with their required method names.
	type ifaceInfo struct {
		id      string
		methods map[string]bool
	}
	var ifaces []ifaceInfo

	allNodes := r.graph.AllNodes()
	for _, n := range allNodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if n.Meta == nil {
			continue
		}
		raw, ok := n.Meta["methods"]
		if !ok {
			continue
		}
		// Meta["methods"] may be []string or []any (after JSON round-trip).
		methodSet := make(map[string]bool)
		switch v := raw.(type) {
		case []string:
			for _, m := range v {
				methodSet[m] = true
			}
		case []any:
			for _, m := range v {
				if s, ok := m.(string); ok {
					methodSet[s] = true
				}
			}
		}
		if len(methodSet) == 0 {
			continue
		}
		ifaces = append(ifaces, ifaceInfo{id: n.ID, methods: methodSet})
	}

	if len(ifaces) == 0 {
		return 0
	}

	// Step 2: Build map of type ID -> set of method names via EdgeMemberOf edges.
	typeMethods := make(map[string]map[string]bool)
	allEdges := r.graph.AllEdges()
	for _, e := range allEdges {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		// EdgeMemberOf: From=method, To=type
		methodNode := r.graph.GetNode(e.From)
		if methodNode == nil || methodNode.Kind != graph.KindMethod {
			continue
		}
		typeID := e.To
		if typeMethods[typeID] == nil {
			typeMethods[typeID] = make(map[string]bool)
		}
		typeMethods[typeID][methodNode.Name] = true
	}

	// Step 3: For each type, check if its method set satisfies each interface.
	added := 0
	for typeID, methods := range typeMethods {
		typeNode := r.graph.GetNode(typeID)
		if typeNode == nil || (typeNode.Kind != graph.KindType && typeNode.Kind != graph.KindInterface) {
			continue
		}
		// Don't let a type implement itself.
		for _, iface := range ifaces {
			if iface.id == typeID {
				continue
			}
			// Check if all required methods are present.
			satisfies := true
			for m := range iface.methods {
				if !methods[m] {
					satisfies = false
					break
				}
			}
			if satisfies {
				r.graph.AddEdge(&graph.Edge{
					From:     typeID,
					To:       iface.id,
					Kind:     graph.EdgeImplements,
					FilePath: typeNode.FilePath,
					Line:     typeNode.StartLine,
				})
				added++
			}
		}
	}

	return added
}

func lastPathComponent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return path
	}
	return parts[len(parts)-1]
}

// callerRepoPrefix returns the RepoPrefix of the node that owns the edge's From field.
func (r *Resolver) callerRepoPrefix(e *graph.Edge) string {
	fromNode := r.graph.GetNode(e.From)
	if fromNode != nil {
		return fromNode.RepoPrefix
	}
	return ""
}
