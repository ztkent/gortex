package graph

import "sync"

// GraphStats holds summary counts of the graph contents.
type GraphStats struct {
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByKind     map[string]int `json:"by_kind"`
	ByLanguage map[string]int `json:"by_language"`
}

// Graph is a thread-safe in-memory knowledge graph.
type Graph struct {
	nodes    map[string]*Node
	outEdges map[string][]*Edge
	inEdges  map[string][]*Edge
	byFile   map[string][]*Node
	byName   map[string][]*Node
	byQual   map[string]*Node
	byRepo   map[string][]*Node // repoPrefix → nodes
	mu       sync.RWMutex
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{
		nodes:    make(map[string]*Node),
		outEdges: make(map[string][]*Edge),
		inEdges:  make(map[string][]*Edge),
		byFile:   make(map[string][]*Node),
		byName:   make(map[string][]*Node),
		byQual:   make(map[string]*Node),
		byRepo:   make(map[string][]*Node),
	}
}

// AddNode inserts a node into the graph and all indexes.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = n
	g.byFile[n.FilePath] = append(g.byFile[n.FilePath], n)
	g.byName[n.Name] = append(g.byName[n.Name], n)
	if n.QualName != "" {
		g.byQual[n.QualName] = n
	}
	if n.RepoPrefix != "" {
		g.byRepo[n.RepoPrefix] = append(g.byRepo[n.RepoPrefix], n)
	}
}

// AddEdge inserts a directed edge into the graph.
func (g *Graph) AddEdge(e *Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.outEdges[e.From] = append(g.outEdges[e.From], e)
	g.inEdges[e.To] = append(g.inEdges[e.To], e)
}

// ReindexEdge updates the inEdges index after an edge's To field has been mutated
// (e.g., by the resolver changing "unresolved::X" to a real target).
// oldTo is the previous value of e.To before mutation.
func (g *Graph) ReindexEdge(e *Edge, oldTo string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if oldTo == e.To {
		return
	}

	// Remove from old inEdges slot.
	inList := g.inEdges[oldTo]
	for i, ie := range inList {
		if ie == e {
			g.inEdges[oldTo] = append(inList[:i], inList[i+1:]...)
			break
		}
	}

	// Add to new inEdges slot.
	g.inEdges[e.To] = append(g.inEdges[e.To], e)
}

// GetNode returns a node by ID, or nil if not found.
func (g *Graph) GetNode(id string) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodes[id]
}

// GetNodeByQualName returns a node by fully-qualified name, or nil.
func (g *Graph) GetNodeByQualName(qualName string) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byQual[qualName]
}

// FindNodesByName returns all nodes matching the short name.
func (g *Graph) FindNodesByName(name string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.byName[name]
	out := make([]*Node, len(src))
	copy(out, src)
	return out
}

// GetFileNodes returns all nodes defined in the given file.
func (g *Graph) GetFileNodes(filePath string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.byFile[filePath]
	out := make([]*Node, len(src))
	copy(out, src)
	return out
}

// GetOutEdges returns outgoing edges for a node.
func (g *Graph) GetOutEdges(nodeID string) []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.outEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// GetInEdges returns incoming edges for a node.
func (g *Graph) GetInEdges(nodeID string) []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.inEdges[nodeID]
	out := make([]*Edge, len(src))
	copy(out, src)
	return out
}

// EvictFile removes all nodes and edges belonging to the given file path.
// Returns counts of removed nodes and edges.
func (g *Graph) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	nodes := g.byFile[filePath]
	if len(nodes) == 0 {
		return 0, 0
	}

	// Collect IDs of nodes being evicted for edge cleanup.
	evictedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		evictedIDs[n.ID] = true
	}

	// Remove nodes from all indexes.
	for _, n := range nodes {
		delete(g.nodes, n.ID)
		if n.QualName != "" {
			delete(g.byQual, n.QualName)
		}
		// Remove from byName index.
		g.byName[n.Name] = removeNode(g.byName[n.Name], n.ID)
		if len(g.byName[n.Name]) == 0 {
			delete(g.byName, n.Name)
		}
		// Remove from byRepo index.
		if n.RepoPrefix != "" {
			g.byRepo[n.RepoPrefix] = removeNode(g.byRepo[n.RepoPrefix], n.ID)
			if len(g.byRepo[n.RepoPrefix]) == 0 {
				delete(g.byRepo, n.RepoPrefix)
			}
		}
	}
	delete(g.byFile, filePath)
	nodesRemoved = len(nodes)

	// Remove edges that reference evicted nodes or originate from this file.
	edgesRemoved = g.evictEdges(evictedIDs, filePath)

	return nodesRemoved, edgesRemoved
}

// evictEdges removes edges associated with evicted node IDs or file path.
// Must be called under write lock.
//
// Instead of scanning all edges in the graph (O(total_edges)), this uses
// targeted lookups: for each evicted node, remove its outEdges/inEdges entries
// and clean the corresponding reverse index entries. This is O(evicted_edges),
// which is critical for performance on large graphs and low-resource devices.
func (g *Graph) evictEdges(evictedIDs map[string]bool, _ string) int {
	removed := 0

	// Phase 1: Remove all outgoing edges from evicted nodes.
	// For each such edge, also remove it from the inEdges of the target node.
	for id := range evictedIDs {
		edges := g.outEdges[id]
		removed += len(edges)
		for _, e := range edges {
			if !evictedIDs[e.To] {
				// Target is not evicted — clean the inEdges entry.
				g.inEdges[e.To] = filterEdge(g.inEdges[e.To], e)
				if len(g.inEdges[e.To]) == 0 {
					delete(g.inEdges, e.To)
				}
			}
		}
		delete(g.outEdges, id)
	}

	// Phase 2: Remove all incoming edges to evicted nodes.
	// For each such edge from a non-evicted source, also clean outEdges of the source.
	for id := range evictedIDs {
		edges := g.inEdges[id]
		for _, e := range edges {
			if !evictedIDs[e.From] {
				// Source is not evicted — clean the outEdges entry.
				// These edges were not counted in phase 1.
				removed++
				g.outEdges[e.From] = filterEdge(g.outEdges[e.From], e)
				if len(g.outEdges[e.From]) == 0 {
					delete(g.outEdges, e.From)
				}
			}
			// If source IS evicted, the edge was already counted and removed in phase 1.
		}
		delete(g.inEdges, id)
	}

	return removed
}

// filterEdge removes a specific edge pointer from a slice.
func filterEdge(edges []*Edge, target *Edge) []*Edge {
	for i, e := range edges {
		if e == target {
			return append(edges[:i], edges[i+1:]...)
		}
	}
	return edges
}

// NodeCount returns the total number of nodes.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// EdgeCount returns the total number of edges.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	count := 0
	for _, edges := range g.outEdges {
		count += len(edges)
	}
	return count
}

// AllNodes returns a snapshot of all nodes.
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	return out
}

// AllEdges returns a snapshot of all edges.
func (g *Graph) AllEdges() []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Edge
	for _, edges := range g.outEdges {
		out = append(out, edges...)
	}
	return out
}

// Stats returns summary counts by kind and language.
func (g *Graph) Stats() GraphStats {
	g.mu.RLock()
	defer g.mu.RUnlock()

	byKind := make(map[string]int)
	byLang := make(map[string]int)
	for _, n := range g.nodes {
		byKind[string(n.Kind)]++
		if n.Language != "" {
			byLang[n.Language]++
		}
	}

	edgeCount := 0
	for _, edges := range g.outEdges {
		edgeCount += len(edges)
	}

	return GraphStats{
		TotalNodes: len(g.nodes),
		TotalEdges: edgeCount,
		ByKind:     byKind,
		ByLanguage: byLang,
	}
}

// GetRepoNodes returns all nodes belonging to the given repository prefix.
func (g *Graph) GetRepoNodes(repoPrefix string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.byRepo[repoPrefix]
	out := make([]*Node, len(src))
	copy(out, src)
	return out
}

// EvictRepo removes all nodes with matching RepoPrefix and all edges
// referencing those nodes. Returns counts of removed nodes and edges.
func (g *Graph) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	nodes := g.byRepo[repoPrefix]
	if len(nodes) == 0 {
		return 0, 0
	}

	// Collect IDs and file paths of nodes being evicted.
	evictedIDs := make(map[string]bool, len(nodes))
	evictedFiles := make(map[string]bool)
	for _, n := range nodes {
		evictedIDs[n.ID] = true
		evictedFiles[n.FilePath] = true
	}

	// Remove nodes from all indexes.
	for _, n := range nodes {
		delete(g.nodes, n.ID)
		if n.QualName != "" {
			delete(g.byQual, n.QualName)
		}
		g.byName[n.Name] = removeNode(g.byName[n.Name], n.ID)
		if len(g.byName[n.Name]) == 0 {
			delete(g.byName, n.Name)
		}
		g.byFile[n.FilePath] = removeNode(g.byFile[n.FilePath], n.ID)
		if len(g.byFile[n.FilePath]) == 0 {
			delete(g.byFile, n.FilePath)
		}
	}
	delete(g.byRepo, repoPrefix)
	nodesRemoved = len(nodes)

	// Remove edges that reference evicted nodes.
	edgesRemoved = g.evictEdgesForNodes(evictedIDs)

	return nodesRemoved, edgesRemoved
}

// evictEdgesForNodes removes edges where either endpoint is in evictedIDs.
// Must be called under write lock.
// Uses targeted lookups instead of full graph scan for O(evicted_edges) performance.
func (g *Graph) evictEdgesForNodes(evictedIDs map[string]bool) int {
	removed := 0

	// Phase 1: Remove all outgoing edges from evicted nodes.
	for id := range evictedIDs {
		edges := g.outEdges[id]
		removed += len(edges)
		for _, e := range edges {
			if !evictedIDs[e.To] {
				g.inEdges[e.To] = filterEdge(g.inEdges[e.To], e)
				if len(g.inEdges[e.To]) == 0 {
					delete(g.inEdges, e.To)
				}
			}
		}
		delete(g.outEdges, id)
	}

	// Phase 2: Remove all incoming edges to evicted nodes from non-evicted sources.
	for id := range evictedIDs {
		edges := g.inEdges[id]
		for _, e := range edges {
			if !evictedIDs[e.From] {
				removed++
				g.outEdges[e.From] = filterEdge(g.outEdges[e.From], e)
				if len(g.outEdges[e.From]) == 0 {
					delete(g.outEdges, e.From)
				}
			}
		}
		delete(g.inEdges, id)
	}

	return removed
}

// RepoStats returns per-repository node and edge counts.
func (g *Graph) RepoStats() map[string]GraphStats {
	g.mu.RLock()
	defer g.mu.RUnlock()

	stats := make(map[string]GraphStats, len(g.byRepo))

	// Count nodes per repo.
	repoNodes := make(map[string]map[string]int)  // repo → byKind
	repoLangs := make(map[string]map[string]int)   // repo → byLanguage
	repoNodeCount := make(map[string]int)
	for prefix, nodes := range g.byRepo {
		repoNodeCount[prefix] = len(nodes)
		byKind := make(map[string]int)
		byLang := make(map[string]int)
		for _, n := range nodes {
			byKind[string(n.Kind)]++
			if n.Language != "" {
				byLang[n.Language]++
			}
		}
		repoNodes[prefix] = byKind
		repoLangs[prefix] = byLang
	}

	// Count edges per repo (by the From node's repo).
	repoEdgeCount := make(map[string]int)
	for _, edges := range g.outEdges {
		for _, e := range edges {
			if fromNode, ok := g.nodes[e.From]; ok && fromNode.RepoPrefix != "" {
				repoEdgeCount[fromNode.RepoPrefix]++
			}
		}
	}

	for prefix := range g.byRepo {
		stats[prefix] = GraphStats{
			TotalNodes: repoNodeCount[prefix],
			TotalEdges: repoEdgeCount[prefix],
			ByKind:     repoNodes[prefix],
			ByLanguage: repoLangs[prefix],
		}
	}

	return stats
}

// RepoPrefixes returns a list of unique repository prefixes in the graph.
func (g *Graph) RepoPrefixes() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	prefixes := make([]string, 0, len(g.byRepo))
	for prefix := range g.byRepo {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func removeNode(nodes []*Node, id string) []*Node {
	for i, n := range nodes {
		if n.ID == id {
			return append(nodes[:i], nodes[i+1:]...)
		}
	}
	return nodes
}
