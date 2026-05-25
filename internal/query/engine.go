package query

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// SearchProvider is a function that returns the current search backend.
// This allows the engine to always use the latest backend even when the
// indexer replaces it (e.g., wrapping BM25 in HybridBackend for embeddings).
type SearchProvider func() search.Backend

// Engine provides higher-level query operations over the graph.
//
// The graph is held as a `graph.Reader` rather than a concrete
// `*graph.Graph` so the same engine instance can serve both base-
// graph queries and overlay-aware queries (an `*graph.OverlaidView`
// also implements `graph.Reader`). `WithReader` returns a shallow
// clone that swaps the reader; the MCP overlay middleware uses it
// to scope a tool call to the calling session's shadow view without
// constructing a fresh Engine per request.
type Engine struct {
	g              graph.Reader
	searchProvider SearchProvider
	rerank         *rerank.Pipeline
}

// WithReader returns a shallow clone of the engine that reads
// through r instead of the original graph. The search provider and
// rerank pipeline are shared with the source engine. Pass the
// base graph reader to undo a previous swap.
func (e *Engine) WithReader(r graph.Reader) *Engine {
	if e == nil {
		return nil
	}
	clone := *e
	clone.g = r
	return &clone
}

// Reader returns the engine's currently-bound graph reader. Tool
// handlers that need to walk the same view the engine sees use this
// to keep their direct-graph reads consistent with the engine's
// internal walks.
func (e *Engine) Reader() graph.Reader { return e.g }

// NewEngine creates a query engine wrapping the given graph. The
// default 11-signal rerank.Pipeline is wired in; callers wanting a
// custom signal set / weights override via SetRerank.
func NewEngine(g graph.Store) *Engine {
	return &Engine{g: g, rerank: rerank.NewDefault()}
}

// SetRerank installs a custom rerank pipeline. Pass nil to disable
// the 11-signal pass and fall back to the BM25-rank-only ordering.
func (e *Engine) SetRerank(p *rerank.Pipeline) { e.rerank = p }

// Rerank returns the installed pipeline. May be nil.
func (e *Engine) Rerank() *rerank.Pipeline { return e.rerank }

// ApplyRerankWeights overlays a per-signal weight map (typically
// loaded from `.gortex.yaml::search::weights`) onto the engine's
// rerank pipeline. Keys not present in the map keep their default
// weight; setting a key to 0 disables that signal. No-op when the
// engine has no pipeline or the map is empty.
func (e *Engine) ApplyRerankWeights(weights map[string]float64) {
	if e.rerank == nil || len(weights) == 0 {
		return
	}
	for name, w := range weights {
		e.rerank.SetWeight(name, w)
	}
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

// dependencyEdgeKinds is the allowlist BFS follows for both
// GetDependencies (outgoing) and GetDependents (incoming). It covers
// the call-graph triple (imports/calls/references) plus the
// infrastructure edges (depends_on / configures / mounts / exposes /
// uses_env) so that "what does this Resource depend on" and "what
// depends on this ConfigMap" walks land on the manifest surface,
// not just the code surface.
var dependencyEdgeKinds = []graph.EdgeKind{
	graph.EdgeImports, graph.EdgeCalls, graph.EdgeReferences,
	graph.EdgeDependsOn, graph.EdgeConfigures, graph.EdgeMounts,
	graph.EdgeExposes, graph.EdgeUsesEnv,
}

// GetDependencies returns outgoing dependencies (imports, calls,
// references, plus infrastructure edges) up to depth hops.
func (e *Engine) GetDependencies(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, true, dependencyEdgeKinds)
}

// GetDependents returns incoming dependents (blast radius) up to depth hops.
func (e *Engine) GetDependents(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, false, dependencyEdgeKinds)
}

// GetCallChain traces the call graph forward from a function. Follows
// EdgeCalls for intra-service traversal and EdgeMatches to cross service
// boundaries — a consumer function's outbound HTTP/gRPC/topic call is
// linked to the provider's handler via a matcher-produced edge, so the
// same BFS walks straight through.
func (e *Engine) GetCallChain(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, true, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches})
}

// GetCallers returns all callers of a function. Traverses EdgeCalls,
// EdgeMatches, and EdgeReferences in reverse:
//   - EdgeCalls: direct `foo()` invocations.
//   - EdgeMatches: cross-service producer/consumer pairing from the matcher
//     (HTTP / gRPC / topic) — a provider handler's callers include every
//     consumer (possibly in another repo) that resolves to it.
//   - EdgeReferences: method-value references (`mux.HandleFunc("/p", h.foo)`,
//     command tables, callback maps, `defer x.Cleanup`). The handler isn't
//     called *at this site*, but it's wired in here — semantically a caller.
//     Without this kind, every routing-style codebase looks like its handlers
//     have zero callers.
func (e *Engine) GetCallers(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, false, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches, graph.EdgeReferences})
}

// GetTesters returns the test functions that exercise a symbol via
// the persistent EdgeTests edges baked at index time. Direct
// inverse-edge walk; one hop, no BFS. Returns an empty slice when
// the symbol has no test coverage or when the index pre-dates the
// EdgeTests pass.
func (e *Engine) GetTesters(symbolID string) []*graph.Node {
	edges := e.g.GetInEdges(symbolID)
	var out []*graph.Node
	for _, edge := range edges {
		if edge.Kind != graph.EdgeTests {
			continue
		}
		if n := e.g.GetNode(edge.From); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// FindImplementations returns all types implementing an interface.
func (e *Engine) FindImplementations(interfaceID string) []*graph.Node {
	return e.FindImplementationsMinTier(interfaceID, "")
}

// FindOverrides returns the methods that override the given method
// (i.e. children with EdgeOverrides → methodID). One-hop walk over
// the type-hierarchy edges.
func (e *Engine) FindOverrides(methodID string) []*graph.Node {
	return e.FindOverridesMinTier(methodID, "")
}

// FindOverridesMinTier filters override edges by minimum origin tier.
// Pass graph.OriginLSPDispatch to restrict to LSP-confirmed overrides.
func (e *Engine) FindOverridesMinTier(methodID, minTier string) []*graph.Node {
	edges := e.g.GetInEdges(methodID)
	out := make([]*graph.Node, 0, len(edges))
	for _, edge := range edges {
		if edge.Kind != graph.EdgeOverrides {
			continue
		}
		if minTier != "" {
			origin := edge.Origin
			if origin == "" {
				origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, "")
			}
			if !graph.MeetsMinTier(origin, minTier) {
				continue
			}
		}
		if n := e.g.GetNode(edge.From); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// FindOverridden returns the parent-class / interface methods that
// the given method overrides (i.e. methodID -EdgeOverrides-> targets).
func (e *Engine) FindOverridden(methodID string) []*graph.Node {
	edges := e.g.GetOutEdges(methodID)
	out := make([]*graph.Node, 0, len(edges))
	for _, edge := range edges {
		if edge.Kind != graph.EdgeOverrides {
			continue
		}
		if n := e.g.GetNode(edge.To); n != nil {
			out = append(out, n)
		}
	}
	return out
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
		//
		// Infrastructure edges complete the picture: find_usages
		// on a ConfigMap returns workloads that consume it via
		// `envFrom` (EdgeConfigures) or mount it (EdgeMounts);
		// find_usages on a config_key returns workloads / Dockerfile
		// stages that declare they use it (EdgeUsesEnv) plus code
		// callers via the legacy reads_config path; find_usages on a
		// Service returns Ingresses routing to it (EdgeDependsOn);
		// find_usages on an Image returns workloads pulling it.
		if edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeReferences ||
			edge.Kind == graph.EdgeInstantiates ||
			edge.Kind == graph.EdgeReturns || edge.Kind == graph.EdgeTypedAs ||
			edge.Kind == graph.EdgeImplements || edge.Kind == graph.EdgeExtends ||
			edge.Kind == graph.EdgeComposes ||
			edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes ||
			edge.Kind == graph.EdgeReadsConfig || edge.Kind == graph.EdgeWritesConfig ||
			edge.Kind == graph.EdgeUsesEnv || edge.Kind == graph.EdgeConfigures ||
			edge.Kind == graph.EdgeMounts || edge.Kind == graph.EdgeExposes ||
			edge.Kind == graph.EdgeDependsOn {
			from := e.g.GetNode(edge.From)
			if opts.WorkspaceID != "" && !opts.ScopeAllows(from) {
				continue
			}
			if opts.ExcludeTests && isTestSource(from) {
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
	// Sort by ID — nodeMap is a map, so the extraction order is
	// otherwise randomised per call and leaks into the result set.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
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

// SearchSymbolsRanked is SearchSymbolsScoped that returns the full
// rerank.Candidate slice instead of just the nodes — callers can read
// the per-signal contributions and the final score off each candidate.
// rctx is optional session context (frecency / combo / feedback /
// repo + project locality); pass nil to score with structural signals
// only.
func (e *Engine) SearchSymbolsRanked(query string, limit int, opts QueryOptions, rctx *rerank.Context) []*rerank.Candidate {
	if limit <= 0 {
		limit = 20
	}
	fetchLimit := limit
	if opts.WorkspaceID != "" {
		fetchLimit = limit * 4
		if fetchLimit > 200 {
			fetchLimit = 200
		}
	}

	var cands []*rerank.Candidate
	if s := e.getSearch(); s != nil && s.Count() > 0 {
		cands = e.gatherBackendCandidates(query, fetchLimit)
	} else {
		nodes := e.searchSubstring(query, fetchLimit)
		cands = make([]*rerank.Candidate, 0, len(nodes))
		for i, n := range nodes {
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: i, VectorRank: -1})
		}
	}

	if opts.WorkspaceID != "" {
		kept := cands[:0]
		for _, c := range cands {
			if !opts.ScopeAllows(c.Node) {
				continue
			}
			kept = append(kept, c)
		}
		cands = kept
	}

	// Cross-repo RRF: when the candidate set spans repositories, the
	// per-channel ranks are reassigned repo by repo so each repo's
	// strongest hits compete on even footing. The rerank's RRF-kernel
	// bm25 and semantic signals then fuse across repos rather than
	// ranking within one merged corpus. No-op for a single-repo set.
	crossRepoRerank(cands)

	if e.rerank != nil {
		ctx := rctx
		if ctx == nil {
			ctx = &rerank.Context{}
		}
		ctx.Graph = e.g
		e.rerank.Rerank(query, cands, ctx)
	}

	if len(cands) > limit {
		cands = cands[:limit]
	}
	return cands
}

// SearchSymbolsScoped is SearchSymbols with the optional
// workspace/project scope. When opts.WorkspaceID is set, results
// outside that scope are filtered out and the search re-fetches as
// needed to fill the requested limit. Empty scope preserves the
// legacy global behaviour.
func (e *Engine) SearchSymbolsScoped(query string, limit int, opts QueryOptions) []*graph.Node {
	cands := e.SearchSymbolsRanked(query, limit, opts, nil)
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Node)
	}
	return out
}

// gatherBackendCandidates fetches BM25 + (optional) vector results,
// dedups them across channels, and supplements with exact-name /
// substring / bigram-rescue matches. Each candidate carries its
// 0-based TextRank and VectorRank (or -1 when the channel didn't
// return it) so the rerank pipeline can score per channel.
//
// The BM25 / vector / bigram tiers all return raw node IDs; the
// implementation materialises them through a single batched
// GetNodesByIDs call instead of per-id GetNode. On disk backends
// (Ladybug) that collapses 60+ cgo Cypher round-trips per query
// into one — the dominant cost on the search hot path before this
// changed.
func (e *Engine) gatherBackendCandidates(query string, limit int) []*rerank.Candidate {
	backend := e.getSearch()

	// Pull text + vector channels separately when the backend exposes
	// them (HybridBackend). Otherwise treat plain Search() output as
	// text-only.
	var (
		textResults []search.SearchResult
		vectorIDs   []string
	)
	if cs, ok := backend.(search.ChannelSearcher); ok {
		textResults, vectorIDs = cs.SearchChannels(query, limit*2)
	} else {
		textResults = backend.Search(query, limit*2)
	}

	// Collect every ID surfaced by the backend tiers up front, then
	// materialise them with one batched fetch. Empty IDs are tolerated
	// — the batch lookup ignores them and the per-id insert short-
	// circuits below.
	idBatch := make([]string, 0, len(textResults)+len(vectorIDs))
	for _, r := range textResults {
		if r.ID != "" {
			idBatch = append(idBatch, r.ID)
		}
	}
	for _, id := range vectorIDs {
		if id != "" {
			idBatch = append(idBatch, id)
		}
	}
	nodeByID := e.g.GetNodesByIDs(idBatch)

	idx := make(map[string]int) // node ID → slice index for dedup
	cands := make([]*rerank.Candidate, 0, len(textResults)+len(vectorIDs))

	insert := func(id string, textRank, vectorRank int) {
		if id == "" {
			return
		}
		node := nodeByID[id]
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			return
		}
		if pos, ok := idx[id]; ok {
			c := cands[pos]
			if textRank >= 0 && (c.TextRank < 0 || textRank < c.TextRank) {
				c.TextRank = textRank
			}
			if vectorRank >= 0 && (c.VectorRank < 0 || vectorRank < c.VectorRank) {
				c.VectorRank = vectorRank
			}
			return
		}
		idx[id] = len(cands)
		cands = append(cands, &rerank.Candidate{
			Node: node, TextRank: textRank, VectorRank: vectorRank,
		})
	}

	for rank, r := range textResults {
		insert(r.ID, rank, -1)
	}
	for rank, id := range vectorIDs {
		insert(id, -1, rank)
	}

	// Stop early when the BM25 + vector union has already exceeded the
	// requested width; the supplementary tiers below are a fill, not a
	// boost.
	if len(cands) >= limit*2 {
		return cands
	}

	// Exact-name matches that BM25 might rank low — splice them in at
	// the tail of the text channel so they're still text-ranked.
	for _, n := range e.g.FindNodesByName(query) {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if _, seen := idx[n.ID]; seen {
			continue
		}
		idx[n.ID] = len(cands)
		cands = append(cands, &rerank.Candidate{Node: n, TextRank: len(textResults), VectorRank: -1})
	}

	// Substring fallback for remaining slots — strictly TextRank=-1
	// (the rerank pipeline still considers them via signature/recency
	// signals, but BM25 can't speak to them). Matches are collected,
	// sorted by ID, then truncated, so the candidate set does not
	// depend on the randomised map-iteration order of AllNodes().
	if len(cands) < limit {
		lower := strings.ToLower(query)
		var subMatches []*graph.Node
		for _, n := range e.g.AllNodes() {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			if _, seen := idx[n.ID]; seen {
				continue
			}
			if strings.Contains(strings.ToLower(n.Name), lower) {
				subMatches = append(subMatches, n)
			}
		}
		sort.Slice(subMatches, func(i, j int) bool { return subMatches[i].ID < subMatches[j].ID })
		for _, n := range subMatches {
			idx[n.ID] = len(cands)
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: -1, VectorRank: -1})
			if len(cands) >= limit {
				break
			}
		}
	}

	// Bigram-overlap typo rescue. Same gates as the legacy path:
	// nothing else surfaced, query is one indivisible 4+ char token,
	// backend can provide candidates. The bigram backend also returns
	// raw IDs — batch-materialise them too rather than fall back to
	// per-id GetNode.
	if len(cands) == 0 && len(query) >= 4 && !strings.ContainsAny(query, " /.:_-") {
		if bg, ok := backend.(bigramProvider); ok {
			keys := len(query) - 1
			minOverlap := (keys + 1) / 2
			if minOverlap < 3 {
				minOverlap = 3
			}
			bigramIDs := bg.BigramCandidates(query, minOverlap)
			// Skip the batch fetch entirely when the bigram backend
			// returned nothing — otherwise we'd issue an empty Cypher
			// round-trip.
			if len(bigramIDs) > 0 {
				bigramNodes := e.g.GetNodesByIDs(bigramIDs)
				for _, id := range bigramIDs {
					if _, seen := idx[id]; seen {
						continue
					}
					node := bigramNodes[id]
					if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
						continue
					}
					idx[id] = len(cands)
					cands = append(cands, &rerank.Candidate{Node: node, TextRank: -1, VectorRank: -1})
					if len(cands) >= limit {
						break
					}
				}
			}
		}
	}

	return cands
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
		if len(results[i].node.Name) != len(results[j].node.Name) {
			return len(results[i].node.Name) < len(results[j].node.Name)
		}
		// Final tie-break on node ID — equal (score, name-length)
		// pairs would otherwise resolve in random map-iteration order.
		return results[i].node.ID < results[j].node.ID
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

			// ExcludeTests drops neighbours flagged as tests during a
			// reverse traversal — for forward traversals it's a no-op
			// because callers asking "who depends on X" (reverse) are
			// the only consumers of this filter today.
			if opts.ExcludeTests && !forward && !bidir {
				if n := e.g.GetNode(neighborID); isTestSource(n) {
					continue
				}
			}

			// Workspace/project scope. When opts.WorkspaceID is set,
			// neighbours outside that scope are dropped along with the
			// edge that pointed at them. Cross-workspace edges produced
			// by the resolver only exist when an explicit
			// cross_workspace_dep allows them, so this filter also
			// acts as the query-time enforcement of "find_usages on a
			// tuck symbol returns hits only from tuck".
			if opts.WorkspaceID != "" {
				if n := e.g.GetNode(neighborID); n != nil && !opts.ScopeAllows(n) {
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

// isTestSource reports whether a node was flagged as a test by the
// indexer's test-edge pass. Used by QueryOptions.ExcludeTests to drop
// callers/users that originate in tests, leaving production callers.
func isTestSource(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["is_test"].(bool)
	return v
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
