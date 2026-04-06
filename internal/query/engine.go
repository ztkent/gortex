package query

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Engine provides higher-level query operations over the graph.
type Engine struct {
	g *graph.Graph
}

// NewEngine creates a query engine wrapping the given graph.
func NewEngine(g *graph.Graph) *Engine {
	return &Engine{g: g}
}

// GetSymbol returns a node by ID.
func (e *Engine) GetSymbol(id string) *graph.Node {
	return e.g.GetNode(id)
}

// GetOutEdges returns outgoing edges for a node.
func (e *Engine) GetOutEdges(nodeID string) []*graph.Edge {
	return e.g.GetOutEdges(nodeID)
}

// GetInEdges returns incoming edges for a node.
func (e *Engine) GetInEdges(nodeID string) []*graph.Edge {
	return e.g.GetInEdges(nodeID)
}

// FindSymbols returns nodes matching the name, optionally filtered by kind.
func (e *Engine) FindSymbols(name string, kinds ...graph.NodeKind) []*graph.Node {
	candidates := e.g.FindNodesByName(name)
	if len(kinds) == 0 {
		return candidates
	}
	kindSet := make(map[graph.NodeKind]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}
	var filtered []*graph.Node
	for _, n := range candidates {
		if kindSet[n.Kind] {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

// GetFileSymbols returns all symbols defined in a file.
func (e *Engine) GetFileSymbols(filePath string) *SubGraph {
	nodes := e.g.GetFileNodes(filePath)
	var edges []*graph.Edge
	for _, n := range nodes {
		edges = append(edges, e.g.GetOutEdges(n.ID)...)
		edges = append(edges, e.g.GetInEdges(n.ID)...)
	}
	return &SubGraph{
		Nodes: nodes, Edges: dedup(edges),
		TotalNodes: len(nodes), TotalEdges: len(edges),
	}
}

// GetDependencies returns outgoing dependencies (imports, calls, references) up to depth hops.
func (e *Engine) GetDependencies(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, true, []graph.EdgeKind{graph.EdgeImports, graph.EdgeCalls, graph.EdgeReferences})
}

// GetDependents returns incoming dependents (blast radius) up to depth hops.
func (e *Engine) GetDependents(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, false, []graph.EdgeKind{graph.EdgeImports, graph.EdgeCalls, graph.EdgeReferences})
}

// GetCallChain traces the call graph forward from a function.
func (e *Engine) GetCallChain(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, true, []graph.EdgeKind{graph.EdgeCalls})
}

// GetCallers returns all callers of a function.
func (e *Engine) GetCallers(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, false, []graph.EdgeKind{graph.EdgeCalls})
}

// FindImplementations returns all types implementing an interface.
func (e *Engine) FindImplementations(interfaceID string) []*graph.Node {
	edges := e.g.GetInEdges(interfaceID)
	var impls []*graph.Node
	for _, edge := range edges {
		if edge.Kind == graph.EdgeImplements {
			if n := e.g.GetNode(edge.From); n != nil {
				impls = append(impls, n)
			}
		}
	}
	return impls
}

// FindUsages returns all nodes that reference a symbol.
func (e *Engine) FindUsages(nodeID string) *SubGraph {
	edges := e.g.GetInEdges(nodeID)
	nodeMap := make(map[string]*graph.Node)
	var filtered []*graph.Edge
	for _, edge := range edges {
		if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeReferences ||
			edge.Kind == graph.EdgeInstantiates {
			filtered = append(filtered, edge)
			if n := e.g.GetNode(edge.From); n != nil {
				nodeMap[n.ID] = n
			}
		}
	}
	// Include the target node itself.
	if n := e.g.GetNode(nodeID); n != nil {
		nodeMap[n.ID] = n
	}
	nodes := make([]*graph.Node, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}
	return &SubGraph{
		Nodes: nodes, Edges: filtered,
		TotalNodes: len(nodes), TotalEdges: len(filtered),
	}
}

// GetCluster returns the immediate neighbourhood within radius hops (bidirectional).
func (e *Engine) GetCluster(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, true, nil) // nil = all edge kinds, bidirectional
}

// SearchSymbols performs fuzzy/substring search across all nodes.
// Results are ranked by relevance: exact match > prefix > substring, shorter names first.
func (e *Engine) SearchSymbols(query string, limit int) []*graph.Node {
	if limit <= 0 {
		limit = 20
	}
	lower := strings.ToLower(query)

	// First try exact name match
	exact := e.g.FindNodesByName(query)

	type scored struct {
		node  *graph.Node
		score int // lower = better
	}
	var results []scored
	seen := make(map[string]bool)

	for _, n := range exact {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		seen[n.ID] = true
		results = append(results, scored{n, 0}) // exact match = best
	}

	// Substring search across all nodes
	allNodes := e.g.AllNodes()
	for _, n := range allNodes {
		if seen[n.ID] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		nameLower := strings.ToLower(n.Name)
		idLower := strings.ToLower(n.ID)

		if strings.HasPrefix(nameLower, lower) {
			results = append(results, scored{n, 1}) // prefix match
		} else if strings.Contains(nameLower, lower) {
			results = append(results, scored{n, 2}) // substring of name
		} else if strings.Contains(idLower, lower) {
			results = append(results, scored{n, 3}) // substring of ID
		} else {
			continue
		}
		seen[n.ID] = true
	}

	// Sort: by score, then by name length (shorter = more relevant)
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score < results[j].score
		}
		return len(results[i].node.Name) < len(results[j].node.Name)
	})

	out := make([]*graph.Node, 0, limit)
	for i, r := range results {
		if i >= limit {
			break
		}
		out = append(out, r.node)
	}
	return out
}

// AllNodes returns all nodes in the graph.
func (e *Engine) AllNodes() []*graph.Node {
	return e.g.AllNodes()
}

// Stats returns summary statistics for the graph.
func (e *Engine) Stats() *graph.GraphStats {
	s := e.g.Stats()
	return &s
}

// bfs performs breadth-first traversal from nodeID.
// If forward is true, follows outgoing edges; if false, follows incoming.
// If edgeKinds is nil, follows all edge kinds bidirectionally (for cluster).
func (e *Engine) bfs(nodeID string, opts QueryOptions, forward bool, edgeKinds []graph.EdgeKind) *SubGraph {
	if opts.Depth <= 0 {
		opts.Depth = 3
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}

	bidir := edgeKinds == nil
	kindSet := make(map[graph.EdgeKind]bool, len(edgeKinds))
	for _, k := range edgeKinds {
		kindSet[k] = true
	}

	visited := make(map[string]bool)
	var allNodes []*graph.Node
	var allEdges []*graph.Edge
	truncated := false

	type item struct {
		id    string
		depth int
	}
	queue := []item{{id: nodeID, depth: 0}}
	visited[nodeID] = true

	if n := e.g.GetNode(nodeID); n != nil {
		allNodes = append(allNodes, n)
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur.depth >= opts.Depth {
			continue
		}

		var edges []*graph.Edge
		if bidir {
			edges = append(e.g.GetOutEdges(cur.id), e.g.GetInEdges(cur.id)...)
		} else if forward {
			edges = e.g.GetOutEdges(cur.id)
		} else {
			edges = e.g.GetInEdges(cur.id)
		}

		for _, edge := range edges {
			if !bidir && !kindSet[edge.Kind] {
				continue
			}

			var neighborID string
			if forward || bidir {
				if edge.From == cur.id {
					neighborID = edge.To
				} else if bidir {
					neighborID = edge.From
				} else {
					continue
				}
			} else {
				if edge.To == cur.id {
					neighborID = edge.From
				} else {
					continue
				}
			}

			// Skip unresolved/external targets.
			if strings.HasPrefix(neighborID, "unresolved::") || strings.HasPrefix(neighborID, "external::") {
				continue
			}

			allEdges = append(allEdges, edge)

			if visited[neighborID] {
				continue
			}
			visited[neighborID] = true

			n := e.g.GetNode(neighborID)
			if n == nil {
				continue
			}

			if len(allNodes) >= opts.Limit {
				truncated = true
				continue
			}

			allNodes = append(allNodes, n)
			queue = append(queue, item{id: neighborID, depth: cur.depth + 1})
		}
	}

	sg := &SubGraph{
		Nodes:      allNodes,
		Edges:      allEdges,
		TotalNodes: len(visited),
		TotalEdges: len(allEdges),
		Truncated:  truncated,
	}

	if opts.Detail == "brief" {
		stripMeta(sg)
	}
	return sg
}

func stripMeta(sg *SubGraph) {
	for _, n := range sg.Nodes {
		n.Meta = nil
	}
}

func dedup(edges []*graph.Edge) []*graph.Edge {
	seen := make(map[string]bool)
	var out []*graph.Edge
	for _, e := range edges {
		key := e.From + "->" + e.To + ":" + string(e.Kind)
		if !seen[key] {
			seen[key] = true
			out = append(out, e)
		}
	}
	return out
}
