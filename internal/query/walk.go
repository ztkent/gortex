package query

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// knownEdgeKinds is the set of edge kinds parseEdgeKinds accepts. It is
// the queryable surface of internal/graph/edge.go — the kinds an agent
// is likely to traverse on. Synthetic / internal kinds the graph emits
// but no traversal tool should expose are intentionally omitted.
var knownEdgeKinds = map[string]graph.EdgeKind{
	"imports":        graph.EdgeImports,
	"defines":        graph.EdgeDefines,
	"calls":          graph.EdgeCalls,
	"instantiates":   graph.EdgeInstantiates,
	"implements":     graph.EdgeImplements,
	"extends":        graph.EdgeExtends,
	"references":     graph.EdgeReferences,
	"member_of":      graph.EdgeMemberOf,
	"provides":       graph.EdgeProvides,
	"consumes":       graph.EdgeConsumes,
	"matches":        graph.EdgeMatches,
	"annotated":      graph.EdgeAnnotated,
	"tests":          graph.EdgeTests,
	"reads":          graph.EdgeReads,
	"writes":         graph.EdgeWrites,
	"throws":         graph.EdgeThrows,
	"returns":        graph.EdgeReturns,
	"typed_as":       graph.EdgeTypedAs,
	"captures":       graph.EdgeCaptures,
	"spawns":         graph.EdgeSpawns,
	"sends":          graph.EdgeSends,
	"recvs":          graph.EdgeRecvs,
	"queries":        graph.EdgeQueries,
	"reads_config":   graph.EdgeReadsConfig,
	"emits":          graph.EdgeEmits,
	"overrides":      graph.EdgeOverrides,
	"depends_on":     graph.EdgeDependsOn,
	"composes":       graph.EdgeComposes,
	"produces_topic": graph.EdgeProducesTopic,
	"consumes_topic": graph.EdgeConsumesTopic,
}

// KnownEdgeKinds returns the sorted list of edge-kind names that
// parseEdgeKinds accepts. Used to build tool-description text so the
// documented surface can never drift from the parser.
func KnownEdgeKinds() []string {
	out := make([]string, 0, len(knownEdgeKinds))
	for k := range knownEdgeKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseEdgeKindsCSV parses a comma-separated list of edge-kind names
// into graph.EdgeKind values. Whitespace around each token is trimmed
// and empty tokens are skipped, so "calls, references" and
// "calls,,references" both parse. An empty (or all-empty) input returns
// a nil slice with no error — callers treat nil as "default" or "every
// kind" per their own semantics. An unrecognised token is a hard error
// naming the offender. Shared by the walk_graph, graph_query, and nav
// MCP tools so their accepted edge-kind surface can never diverge.
func ParseEdgeKindsCSV(csv string) ([]graph.EdgeKind, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	var out []graph.EdgeKind
	seen := make(map[graph.EdgeKind]bool)
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		kind, ok := knownEdgeKinds[strings.ToLower(tok)]
		if !ok {
			return nil, fmt.Errorf("unknown edge kind %q (valid: %s)",
				tok, strings.Join(KnownEdgeKinds(), ", "))
		}
		if seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	return out, nil
}

// walkTokenEstimate is the per-node contribution to the running encoded-
// size estimate used by WalkBudgeted. The encoder emits one row per node
// (id, kind, name, path, line, …) and roughly one row per edge; this
// constant approximates the token cost of a node row plus its incident
// edge row at the GCX wire format's density. It is deliberately
// conservative — over-estimating stops the walk a little early, which is
// the safe direction for a budget.
const walkTokenEstimate = 28

// walkBudgetTokens converts a running byte estimate into tokens at the
// ~3.5 bytes/token heuristic used elsewhere in the codebase.
func walkBudgetTokens(bytesEstimate int) int {
	return bytesEstimate * 10 / 35
}

// WalkBudgeted performs a token-budgeted breadth-first traversal from
// startID. It generalises bfs: the caller picks the edge kinds and the
// direction, and the walk stops appending nodes once the estimated
// encoded size of the result would exceed opts.TokenBudget (rather than
// on a fixed node count). opts.MaxDepth is a hard safety cap applied
// regardless of the budget.
//
// The returned SubGraph carries BudgetHit (true when the token budget
// stopped the walk) and StoppedAtDepth (the deepest BFS depth reached).
// When opts.EdgeKinds is empty the walk follows every known edge kind;
// combined with Direction "both" that is an undirected neighbourhood
// expansion. Unresolved / external neighbours are skipped, and the
// workspace/project scope in opts is enforced exactly as bfs does.
func (e *Engine) WalkBudgeted(startID string, opts WalkOptions) *SubGraph {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 8
	}

	direction := strings.ToLower(strings.TrimSpace(opts.Direction))
	if direction == "" {
		direction = "out"
	}
	both := direction == "both"
	forward := direction != "in"

	// An empty kind set means "follow every known kind". Building the
	// set explicitly (rather than a bidir==nil sentinel like bfs) keeps
	// the direction and kind axes independent.
	allKinds := len(opts.EdgeKinds) == 0
	kindSet := make(map[graph.EdgeKind]bool, len(opts.EdgeKinds))
	for _, k := range opts.EdgeKinds {
		kindSet[k] = true
	}

	visited := make(map[string]bool)
	var allNodes []*graph.Node
	var allEdges []*graph.Edge
	budgetHit := false
	stoppedAtDepth := 0

	type item struct {
		id    string
		depth int
	}

	visited[startID] = true
	// byteEstimate tracks the running encoded size. The seed always
	// enters the result, so it is counted up front.
	byteEstimate := 0
	if n := e.g.GetNode(startID); n != nil {
		allNodes = append(allNodes, n)
		byteEstimate += walkTokenEstimate
	}
	queue := []item{{id: startID, depth: 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth > stoppedAtDepth {
			stoppedAtDepth = cur.depth
		}
		if cur.depth >= maxDepth {
			continue
		}

		var edges []*graph.Edge
		if both {
			edges = append(e.g.GetOutEdges(cur.id), e.g.GetInEdges(cur.id)...)
		} else if forward {
			edges = e.g.GetOutEdges(cur.id)
		} else {
			edges = e.g.GetInEdges(cur.id)
		}

		for _, edge := range edges {
			if !allKinds && !kindSet[edge.Kind] {
				continue
			}

			var neighborID string
			if both {
				if edge.From == cur.id {
					neighborID = edge.To
				} else {
					neighborID = edge.From
				}
			} else if forward {
				if edge.From != cur.id {
					continue
				}
				neighborID = edge.To
			} else {
				if edge.To != cur.id {
					continue
				}
				neighborID = edge.From
			}

			if strings.HasPrefix(neighborID, "unresolved::") ||
				strings.HasPrefix(neighborID, "external::") {
				continue
			}

			n := e.g.GetNode(neighborID)
			if n == nil {
				continue
			}
			if !opts.scopeAllows(n) {
				continue
			}

			// The edge is part of the result regardless of whether its
			// target node is new — a cross-edge between two visited
			// nodes is still a real relationship.
			allEdges = append(allEdges, edge)

			if visited[neighborID] {
				continue
			}

			// Token budget: stop appending nodes once the running
			// estimate would exceed the budget. Already-queued nodes
			// still drain so their edges are recorded, but no deeper
			// frontier is added.
			if opts.TokenBudget > 0 &&
				walkBudgetTokens(byteEstimate+walkTokenEstimate) > opts.TokenBudget {
				budgetHit = true
				continue
			}

			visited[neighborID] = true
			allNodes = append(allNodes, n)
			byteEstimate += walkTokenEstimate
			queue = append(queue, item{id: neighborID, depth: cur.depth + 1})
		}
	}

	return &SubGraph{
		Nodes:          allNodes,
		Edges:          allEdges,
		TotalNodes:     len(allNodes),
		TotalEdges:     len(allEdges),
		Truncated:      budgetHit,
		BudgetHit:      budgetHit,
		StoppedAtDepth: stoppedAtDepth,
	}
}
