package query

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ClosureOptions controls a multi-seed dependency-closure traversal
// (Engine.ImportClosure). It generalises WalkBudgeted from a single
// start node to a set of seeds, and ranks every reached node by its
// graph distance to the nearest seed rather than stopping on an
// encoded-size estimate.
type ClosureOptions struct {
	// EdgeKinds is the set of edge kinds the closure follows. An empty
	// slice falls back to dependencyEdgeKinds (imports / calls /
	// references / depends_on plus the infrastructure edges), the same
	// allowlist GetDependencies walks.
	EdgeKinds []graph.EdgeKind
	// MaxDepth caps how far the closure expands from the seed set. A
	// non-positive value falls back to a built-in default.
	MaxDepth int
	// MaxNodes caps the size of the returned closure (the seeds always
	// count toward it). A non-positive value falls back to a built-in
	// default so a pathological seed set can never expand without
	// bound. Nodes are admitted in breadth-first / nearest-first order,
	// so the cap keeps the closest nodes.
	MaxNodes int
	// WorkspaceID / ProjectID scope the traversal exactly as the
	// matching WalkOptions fields do — neighbours outside the scope are
	// dropped along with the edge that reached them.
	WorkspaceID string
	ProjectID   string
}

// ClosureNode is one node in a dependency closure, tagged with its
// graph distance to the nearest seed (0 for a seed itself).
type ClosureNode struct {
	Node     *graph.Node
	Distance int
}

// ClosureResult is the outcome of a multi-seed closure traversal.
type ClosureResult struct {
	// Nodes are the closure members sorted by (distance, ID) so the
	// result — and any pack built from it — is deterministic regardless
	// of the backend's edge enumeration order.
	Nodes []ClosureNode
	// Edges are the traversed edges that connect closure members. A
	// cross-edge between two already-visited members is still recorded.
	Edges []*graph.Edge
	// SeedIDs is the de-duplicated, in-scope seed set the traversal
	// actually started from (a seed outside scope or absent from the
	// graph is dropped before expansion).
	SeedIDs []string
	// Truncated reports whether the node cap stopped the expansion
	// before the closure was fully explored.
	Truncated bool
	// StoppedAtDepth is the deepest distance the traversal reached.
	StoppedAtDepth int
}

const (
	closureDefaultMaxDepth = 6
	closureDefaultMaxNodes = 400
)

// closureScopeAllows reuses the WalkOptions scope rule so a closure
// enforces the same workspace/project boundary without duplicating the
// fallback logic.
func (o ClosureOptions) closureScopeAllows(n *graph.Node) bool {
	return WalkOptions{WorkspaceID: o.WorkspaceID, ProjectID: o.ProjectID}.scopeAllows(n)
}

// ImportClosure walks the transitive dependency closure of a set of
// seeds. Starting from every seed at distance 0, it expands breadth
// first over the chosen edge kinds (default: the dependency allowlist —
// imports / calls / references / depends_on / infra), recording each
// reached node's distance to the NEAREST seed. Unresolved / external
// neighbours are skipped and the workspace/project scope is enforced
// exactly as WalkBudgeted does.
//
// Unlike WalkBudgeted (single seed, token-bounded) this is multi-seed
// and node-bounded: it is the substrate for a closure-context packer
// that ranks the closure by graph distance and hands it to the
// token-budgeted manifest. The returned nodes are sorted by
// (distance, ID) so the result is order-independent across backends.
func (e *Engine) ImportClosure(seedIDs []string, opts ClosureOptions) *ClosureResult {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = closureDefaultMaxDepth
	}
	maxNodes := opts.MaxNodes
	if maxNodes <= 0 {
		maxNodes = closureDefaultMaxNodes
	}

	kinds := opts.EdgeKinds
	if len(kinds) == 0 {
		kinds = dependencyEdgeKinds
	}
	kindSet := make(map[graph.EdgeKind]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}

	distance := make(map[string]int)
	nodeByID := make(map[string]*graph.Node)
	var edges []*graph.Edge
	seenEdge := make(map[string]bool)

	type item struct {
		id    string
		depth int
	}
	var queue []item
	var seeds []string

	// Seed the frontier. A seed that is out of scope, unresolved, or
	// absent from the graph never enters the closure — the caller's
	// distance ranking and budget should not be perturbed by a seed
	// that cannot contribute real context.
	seedSeen := make(map[string]bool)
	for _, id := range seedIDs {
		if id == "" || seedSeen[id] {
			continue
		}
		if graph.IsUnresolvedTarget(id) || strings.HasPrefix(id, "external::") {
			continue
		}
		n := e.g.GetNode(id)
		if n == nil || !opts.closureScopeAllows(n) {
			continue
		}
		seedSeen[id] = true
		seeds = append(seeds, id)
		distance[id] = 0
		nodeByID[id] = n
		queue = append(queue, item{id: id, depth: 0})
	}

	truncated := false
	stoppedAtDepth := 0

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > stoppedAtDepth {
			stoppedAtDepth = cur.depth
		}
		if cur.depth >= maxDepth {
			continue
		}

		// Closure is a dependency-direction walk: follow outgoing edges
		// (what the seed depends on). This mirrors GetDependencies, the
		// established "what does X need" traversal.
		for _, edge := range e.g.GetOutEdges(cur.id) {
			if !kindSet[edge.Kind] {
				continue
			}
			neighborID := edge.To
			if graph.IsUnresolvedTarget(neighborID) ||
				strings.HasPrefix(neighborID, "external::") {
				continue
			}
			n := e.g.GetNode(neighborID)
			if n == nil || !opts.closureScopeAllows(n) {
				continue
			}

			// The edge is part of the result regardless of whether the
			// neighbour is new — a cross-edge between two visited closure
			// members is still a real dependency. Dedup by identity so a
			// parallel edge pair is not double-counted.
			ekey := string(edge.Kind) + "\x00" + edge.From + "\x00" + edge.To
			if !seenEdge[ekey] {
				seenEdge[ekey] = true
				edges = append(edges, edge)
			}

			if _, seen := distance[neighborID]; seen {
				continue
			}
			// Node cap: stop admitting new nodes once the closure is
			// full. Already-queued nodes still drain so their connecting
			// edges are recorded, but no deeper frontier is added.
			if len(nodeByID) >= maxNodes {
				truncated = true
				continue
			}
			distance[neighborID] = cur.depth + 1
			nodeByID[neighborID] = n
			queue = append(queue, item{id: neighborID, depth: cur.depth + 1})
		}
	}

	nodes := make([]ClosureNode, 0, len(nodeByID))
	for id, n := range nodeByID {
		nodes = append(nodes, ClosureNode{Node: n, Distance: distance[id]})
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Distance != nodes[j].Distance {
			return nodes[i].Distance < nodes[j].Distance
		}
		return nodes[i].Node.ID < nodes[j].Node.ID
	})

	sort.Strings(seeds)
	return &ClosureResult{
		Nodes:          nodes,
		Edges:          edges,
		SeedIDs:        seeds,
		Truncated:      truncated,
		StoppedAtDepth: stoppedAtDepth,
	}
}
