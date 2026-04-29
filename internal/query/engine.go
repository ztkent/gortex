package query

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// SearchProvider is a function that returns the current search backend.
// This allows the engine to always use the latest backend even when the
// indexer replaces it (e.g., wrapping BM25 in HybridBackend for embeddings).
type SearchProvider func() search.Backend

// Engine provides higher-level query operations over the graph.
type Engine struct {
	g              *graph.Graph
	searchProvider SearchProvider
}

// NewEngine creates a query engine wrapping the given graph.
func NewEngine(g *graph.Graph) *Engine {
	return &Engine{g: g}
}

// SetSearch sets a static search backend (for backward compatibility).
func (e *Engine) SetSearch(s search.Backend) {
	e.searchProvider = func() search.Backend { return s }
}

// SetSearchProvider sets a dynamic search provider that is called on every query.
func (e *Engine) SetSearchProvider(p SearchProvider) {
	e.searchProvider = p
}

// getSearch returns the current search backend.
func (e *Engine) getSearch() search.Backend {
	if e.searchProvider == nil {
		return nil
	}
	return e.searchProvider()
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

// GetCallChain traces the call graph forward from a function. Follows
// EdgeCalls for intra-service traversal and EdgeMatches to cross service
// boundaries — a consumer function's outbound HTTP/gRPC/topic call is
// linked to the provider's handler via a matcher-produced edge, so the
// same BFS walks straight through.
func (e *Engine) GetCallChain(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, true, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches})
}

// GetCallers returns all callers of a function. Traverses EdgeCalls and
// EdgeMatches in reverse: a provider handler's callers include every
// consumer (possibly in another repo) that resolves to it via the matcher.
func (e *Engine) GetCallers(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, false, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches})
}

// FindImplementations returns all types implementing an interface.
func (e *Engine) FindImplementations(interfaceID string) []*graph.Node {
	return e.FindImplementationsMinTier(interfaceID, "")
}

// FindImplementationsMinTier is FindImplementations filtered by the origin
// tier of the implements-edge. Pass "" for no filter; pass
// graph.OriginLSPDispatch (or higher) to restrict to compiler-verified
// interface dispatches.
func (e *Engine) FindImplementationsMinTier(interfaceID, minTier string) []*graph.Node {
	edges := e.g.GetInEdges(interfaceID)
	var impls []*graph.Node
	for _, edge := range edges {
		if edge.Kind != graph.EdgeImplements {
			continue
		}
		if minTier != "" {
			origin := edge.Origin
			if origin == "" {
				src, _ := edge.Meta["semantic_source"].(string)
				origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, src)
			}
			if !graph.MeetsMinTier(origin, minTier) {
				continue
			}
		}
		if n := e.g.GetNode(edge.From); n != nil {
			impls = append(impls, n)
		}
	}
	return impls
}

// FindUsages returns all nodes that reference a symbol.
func (e *Engine) FindUsages(nodeID string) *SubGraph {
	return e.FindUsagesScoped(nodeID, QueryOptions{})
}

// FindUsagesScoped is FindUsages with an optional workspace scope.
// When opts.WorkspaceID is set, only callers from that workspace are
// returned — i.e. find_usages on a tuck symbol returns hits only
// from tuck. Empty WorkspaceID preserves the legacy global-graph
// behaviour.
func (e *Engine) FindUsagesScoped(nodeID string, opts QueryOptions) *SubGraph {
	edges := e.g.GetInEdges(nodeID)
	nodeMap := make(map[string]*graph.Node)
	var filtered []*graph.Edge
	for _, edge := range edges {
		// EdgeProvides + EdgeConsumes carry DI token relationships —
		// `@Inject(TOKEN)` and `{ provide: TOKEN, useValue: ... }`
		// both resolve into one of these, so find_usages on a token
		// returns its providers and consumers alongside the usual
		// call/reference/instantiate edges.
		if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeReferences ||
			edge.Kind == graph.EdgeInstantiates ||
			edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
			from := e.g.GetNode(edge.From)
			if opts.WorkspaceID != "" && !opts.scopeAllows(from) {
				continue
			}
			filtered = append(filtered, edge)
			if from != nil {
				nodeMap[from.ID] = from
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

// SearchSymbols performs full-text search across all nodes.
// When a search backend is configured, uses BM25/Bleve ranking with
// camelCase-aware tokenization. Falls back to substring matching otherwise.
func (e *Engine) SearchSymbols(query string, limit int) []*graph.Node {
	return e.SearchSymbolsScoped(query, limit, QueryOptions{})
}

// SearchSymbolsScoped is SearchSymbols with the optional
// workspace/project scope. When opts.WorkspaceID is set, results
// outside that scope are filtered out and the search re-fetches as
// needed to fill the requested limit. Empty scope preserves the
// legacy global behaviour.
func (e *Engine) SearchSymbolsScoped(query string, limit int, opts QueryOptions) []*graph.Node {
	if limit <= 0 {
		limit = 20
	}

	// Workspace-scoped searches need to over-fetch from the backend
	// because the BM25 / substring layers don't know about the
	// workspace boundary; we filter post-hoc and may need to keep
	// going to fill
	// `limit`. The 4× factor is a heuristic — most workspace-bounded
	// users have an ~all-mine result distribution and so the first
	// page is usually enough; the extras get truncated cheaply. We
	// cap at 200 to keep the BM25 walk bounded.
	fetchLimit := limit
	if opts.WorkspaceID != "" {
		fetchLimit = limit * 4
		if fetchLimit > 200 {
			fetchLimit = 200
		}
	}

	var raw []*graph.Node
	if s := e.getSearch(); s != nil && s.Count() > 0 {
		raw = e.searchWithBackend(query, fetchLimit)
	} else {
		raw = e.searchSubstring(query, fetchLimit)
	}

	if opts.WorkspaceID == "" {
		return raw
	}
	out := make([]*graph.Node, 0, limit)
	for _, n := range raw {
		if !opts.scopeAllows(n) {
			continue
		}
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (e *Engine) searchWithBackend(query string, limit int) []*graph.Node {
	// Get BM25/Bleve results.
	results := e.getSearch().Search(query, limit*2) // fetch extra for dedup/filtering

	seen := make(map[string]bool)
	var out []*graph.Node

	// BM25 results first (ranked by relevance).
	for _, r := range results {
		node := e.g.GetNode(r.ID)
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			continue
		}
		if seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		out = append(out, node)
		if len(out) >= limit {
			return out
		}
	}

	// If BM25 didn't fill the limit, supplement with substring matches.
	// This catches exact name matches that BM25 might rank lower.
	lower := strings.ToLower(query)
	exact := e.g.FindNodesByName(query)
	for _, n := range exact {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport || seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		out = append(out, n)
		if len(out) >= limit {
			return out
		}
	}

	// Substring fallback for remaining slots.
	allNodes := e.g.AllNodes()
	for _, n := range allNodes {
		if seen[n.ID] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		nameLower := strings.ToLower(n.Name)
		if strings.Contains(nameLower, lower) {
			seen[n.ID] = true
			out = append(out, n)
			if len(out) >= limit {
				return out
			}
		}
	}

	// Final tier — bigram-overlap typo rescue. Strictly gated: the
	// preceding tiers must have produced ZERO results (true "nothing
	// plausibly matches"), the query must be a single indivisible word
	// of at least 4 chars (the shape of a typo), and a bigram-providing
	// backend must be available. Anything else (partial BM25 hits, short
	// queries, compound queries) skips straight past — bigram scanning
	// is expensive and noisy, so we pay for it only when we'd otherwise
	// return nothing at all.
	if len(out) == 0 && len(query) >= 4 && !strings.ContainsAny(query, " /.:_-") {
		if bg, ok := e.getSearch().(bigramProvider); ok {
			keys := len(query) - 1
			minOverlap := (keys + 1) / 2
			if minOverlap < 3 {
				minOverlap = 3
			}
			for _, id := range bg.BigramCandidates(query, minOverlap) {
				if seen[id] {
					continue
				}
				node := e.g.GetNode(id)
				if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
					continue
				}
				seen[id] = true
				out = append(out, node)
				if len(out) >= limit {
					return out
				}
			}
		}
	}

	return out
}

// bigramProvider is satisfied by backends that expose a typo-tolerant
// rescue list. Declared here (not in search) so the engine can adopt
// rescue without the search interface changing; any backend that can
// provide bigram candidates just has to implement this method.
type bigramProvider interface {
	BigramCandidates(query string, minOverlap int) []string
}

func (e *Engine) searchSubstring(query string, limit int) []*graph.Node {
	lower := strings.ToLower(query)

	exact := e.g.FindNodesByName(query)

	type scored struct {
		node  *graph.Node
		score int
	}
	var results []scored
	seen := make(map[string]bool)

	for _, n := range exact {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		seen[n.ID] = true
		results = append(results, scored{n, 0})
	}

	allNodes := e.g.AllNodes()
	for _, n := range allNodes {
		if seen[n.ID] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		nameLower := strings.ToLower(n.Name)
		idLower := strings.ToLower(n.ID)

		if strings.HasPrefix(nameLower, lower) {
			results = append(results, scored{n, 1})
		} else if strings.Contains(nameLower, lower) {
			results = append(results, scored{n, 2})
		} else if strings.Contains(idLower, lower) {
			results = append(results, scored{n, 3})
		} else {
			continue
		}
		seen[n.ID] = true
	}

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

// SearchSymbolsInRepo performs full-text search filtered to a specific repository.
func (e *Engine) SearchSymbolsInRepo(query string, repoPrefix string, limit int) []*graph.Node {
	if limit <= 0 {
		limit = 20
	}
	// Fetch extra results since some will be filtered out.
	candidates := e.SearchSymbols(query, limit*2)
	var out []*graph.Node
	for _, n := range candidates {
		if n.RepoPrefix == repoPrefix {
			out = append(out, n)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// GetFileSymbolsInRepo returns all symbols defined in a file, scoped to a specific repository.
func (e *Engine) GetFileSymbolsInRepo(filePath string, repoPrefix string) *SubGraph {
	sg := e.GetFileSymbols(filePath)
	var nodes []*graph.Node
	for _, n := range sg.Nodes {
		if n.RepoPrefix == repoPrefix {
			nodes = append(nodes, n)
		}
	}
	var edges []*graph.Edge
	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	for _, edge := range sg.Edges {
		if nodeSet[edge.From] || nodeSet[edge.To] {
			edges = append(edges, edge)
		}
	}
	return &SubGraph{
		Nodes:      nodes,
		Edges:      dedup(edges),
		TotalNodes: len(nodes),
		TotalEdges: len(edges),
	}
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
		// The seed always enters the result, regardless of scope —
		// callers ask "what reaches X" with X already in mind. The
		// scope check applies to neighbours discovered by traversal.
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

			// Workspace/project scope. When opts.WorkspaceID is set,
			// neighbours outside that scope are dropped along with the
			// edge that pointed at them. Cross-workspace edges produced
			// by the resolver only exist when an explicit
			// cross_workspace_dep allows them, so this filter also
			// acts as the query-time enforcement of "find_usages on a
			// tuck symbol returns hits only from tuck".
			if opts.WorkspaceID != "" {
				if n := e.g.GetNode(neighborID); n != nil && !opts.scopeAllows(n) {
					continue
				}
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
