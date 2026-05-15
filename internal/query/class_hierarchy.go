package query

import "github.com/zzet/gortex/internal/graph"

// HierarchyDirection picks which side of the class-hierarchy graph
// ClassHierarchy traverses from the seed.
type HierarchyDirection string

const (
	HierarchyUp   HierarchyDirection = "up"
	HierarchyDown HierarchyDirection = "down"
	HierarchyBoth HierarchyDirection = "both"
)

// typeHierarchyEdgeKinds is the set traversed when the visited node is
// a type / interface. EdgeExtends covers single + multiple inheritance,
// EdgeImplements bridges concrete type ↔ interface, EdgeComposes covers
// Go struct embedding / Rust trait bounds / Python multiple inheritance
// mixins.
var typeHierarchyEdgeKinds = map[graph.EdgeKind]bool{
	graph.EdgeExtends:    true,
	graph.EdgeImplements: true,
	graph.EdgeComposes:   true,
}

// methodHierarchyEdgeKinds is the set traversed when the visited node
// is a method. EdgeOverrides is method-level: child method → parent /
// interface method.
var methodHierarchyEdgeKinds = map[graph.EdgeKind]bool{
	graph.EdgeOverrides: true,
}

// ClassHierarchy returns the inheritance subgraph rooted at seedID.
//
// Walks the graph through EdgeExtends + EdgeImplements + EdgeComposes
// for type nodes and EdgeOverrides for method nodes. Direction picks
// the side(s) of the hierarchy:
//
//   - HierarchyUp   — outgoing edges (parents / interfaces a child
//     extends or implements; parent methods this method overrides).
//   - HierarchyDown — incoming edges (subclasses / implementers; methods
//     that override this one).
//   - HierarchyBoth — union of the two.
//
// When includeMethods is true and a type / interface node is reached,
// its methods (in-edges of EdgeMemberOf whose From side is a function
// or method node) are pulled into the result and override links from
// each method are walked in the same direction(s).
//
// Workspace / project scope is enforced via opts.ScopeAllows on every
// neighbour. opts.MinTier is applied as a post-pass over the collected
// edges (consistent with the rest of the engine surface).
func (e *Engine) ClassHierarchy(seedID string, direction HierarchyDirection, depth int, includeMethods bool, opts QueryOptions) *SubGraph {
	if direction == "" {
		direction = HierarchyBoth
	}
	if depth <= 0 {
		depth = 5
	}
	if depth > 64 {
		depth = 64
	}

	walkUp := direction == HierarchyUp || direction == HierarchyBoth
	walkDown := direction == HierarchyDown || direction == HierarchyBoth

	visitedNodes := make(map[string]bool)
	visitedEdges := make(map[string]bool)
	var resultNodes []*graph.Node
	var resultEdges []*graph.Edge

	addNode := func(n *graph.Node) {
		if n == nil || visitedNodes[n.ID] {
			return
		}
		visitedNodes[n.ID] = true
		resultNodes = append(resultNodes, n)
	}

	// Edges are deduped by their source pointer identity — the graph
	// store hands out stable pointers per edge, so a pointer key is
	// sufficient and avoids constructing a synthetic key per edge.
	edgeKey := func(ed *graph.Edge) string {
		return ed.From + "→" + ed.To + "::" + string(ed.Kind) + ":" + edgeMetaTag(ed)
	}
	addEdge := func(ed *graph.Edge) {
		if ed == nil {
			return
		}
		k := edgeKey(ed)
		if visitedEdges[k] {
			return
		}
		visitedEdges[k] = true
		resultEdges = append(resultEdges, ed)
	}

	seed := e.g.GetNode(seedID)
	if seed == nil {
		return &SubGraph{}
	}
	addNode(seed)

	type queued struct {
		id    string
		depth int
	}
	queue := []queued{{id: seedID, depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= depth {
			continue
		}

		curNode := e.g.GetNode(cur.id)
		if curNode == nil {
			continue
		}

		isType := curNode.Kind == graph.KindType || curNode.Kind == graph.KindInterface
		isMethod := curNode.Kind == graph.KindMethod || curNode.Kind == graph.KindFunction

		// Pull in member methods of type/interface nodes when requested.
		// This happens at the visit step (not as a hop), so methods land
		// in the result without consuming a depth budget — they're a
		// projection of the type, not a separate hierarchy hop.
		if includeMethods && isType {
			for _, mEdge := range e.g.GetInEdges(cur.id) {
				if mEdge.Kind != graph.EdgeMemberOf {
					continue
				}
				member := e.g.GetNode(mEdge.From)
				if member == nil {
					continue
				}
				if member.Kind != graph.KindMethod && member.Kind != graph.KindFunction {
					continue
				}
				if opts.WorkspaceID != "" && !opts.ScopeAllows(member) {
					continue
				}
				addNode(member)
				addEdge(mEdge)
				// Surface the method itself for the override walk in
				// the next iteration. Same depth budget as the parent
				// type so a method's overrides cost the same as walking
				// to a method-seed at this depth.
				queue = append(queue, queued{id: member.ID, depth: cur.depth})
			}
		}

		// Pick edge kinds based on what kind of node we're standing on.
		var kindSet map[graph.EdgeKind]bool
		switch {
		case isType:
			kindSet = typeHierarchyEdgeKinds
		case isMethod:
			kindSet = methodHierarchyEdgeKinds
		default:
			// Fields, params, files, etc. — nothing to walk.
			continue
		}

		if walkUp {
			for _, ed := range e.g.GetOutEdges(cur.id) {
				if !kindSet[ed.Kind] {
					continue
				}
				neighbor := e.g.GetNode(ed.To)
				if neighbor == nil {
					continue
				}
				if opts.WorkspaceID != "" && !opts.ScopeAllows(neighbor) {
					continue
				}
				addEdge(ed)
				if !visitedNodes[neighbor.ID] {
					addNode(neighbor)
					queue = append(queue, queued{id: neighbor.ID, depth: cur.depth + 1})
				}
			}
		}
		if walkDown {
			for _, ed := range e.g.GetInEdges(cur.id) {
				if !kindSet[ed.Kind] {
					continue
				}
				neighbor := e.g.GetNode(ed.From)
				if neighbor == nil {
					continue
				}
				if opts.WorkspaceID != "" && !opts.ScopeAllows(neighbor) {
					continue
				}
				addEdge(ed)
				if !visitedNodes[neighbor.ID] {
					addNode(neighbor)
					queue = append(queue, queued{id: neighbor.ID, depth: cur.depth + 1})
				}
			}
		}
	}

	sg := &SubGraph{
		Nodes:      resultNodes,
		Edges:      resultEdges,
		TotalNodes: len(resultNodes),
		TotalEdges: len(resultEdges),
	}
	if opts.MinTier != "" {
		sg.FilterByMinTier(opts.MinTier)
	}
	return sg
}

// edgeMetaTag is a small disambiguator for edges that share From / To /
// Kind but carry distinct metadata (e.g. multiple EdgeOverrides between
// the same method pair via different language sources). Falls back to
// the edge file:line when no semantic_source is set.
func edgeMetaTag(ed *graph.Edge) string {
	if ed.Meta != nil {
		if src, ok := ed.Meta["semantic_source"].(string); ok && src != "" {
			return src
		}
	}
	if ed.FilePath != "" {
		return ed.FilePath
	}
	return ""
}
