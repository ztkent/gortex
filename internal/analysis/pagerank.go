package analysis

import "github.com/zzet/gortex/internal/graph"

// PageRankResult holds per-node PageRank centrality scores.
type PageRankResult struct {
	// Scores maps node ID to its PageRank value. The values sum to
	// ~1 across all nodes; individual scores are small and best read
	// relative to Max.
	Scores map[string]float64
	// Max is the largest score in Scores — the normaliser callers use
	// to project centrality onto a 0..1 / 0..100 scale.
	Max float64
}

// ScoreOf returns the PageRank score for a node, or 0 when absent.
func (r *PageRankResult) ScoreOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Scores[id]
}

// PageRank tuning. Damping 0.85 is the canonical web-graph value;
// iterations are fixed rather than convergence-tested because the
// graph is small enough that 40 power-iteration steps are well past
// the point the ranking order stabilises.
const (
	pageRankDamping    = 0.85
	pageRankIterations = 40
)

// ComputePageRank runs PageRank centrality over the call / reference
// graph. Rank flows backwards along call edges: a function is central
// when central functions call it, so a heavily-depended-on symbol
// accumulates score. Only EdgeCalls and EdgeReferences participate —
// structural edges (defines, member_of, imports) would drown the
// dependency signal.
//
// Dangling nodes (no outgoing call/reference edge — leaf utilities)
// redistribute their mass uniformly each iteration so the scores stay
// a proper probability distribution.
func ComputePageRank(g graph.Store) *PageRankResult {
	if g == nil {
		return &PageRankResult{Scores: map[string]float64{}}
	}
	nodes := g.AllNodes()
	n := len(nodes)
	if n == 0 {
		return &PageRankResult{Scores: map[string]float64{}}
	}

	outDegree := make(map[string]int, n)
	inLinks := make(map[string][]string)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		outDegree[e.From]++
		inLinks[e.To] = append(inLinks[e.To], e.From)
	}

	score := make(map[string]float64, n)
	initial := 1.0 / float64(n)
	for _, nd := range nodes {
		score[nd.ID] = initial
	}

	base := (1 - pageRankDamping) / float64(n)
	for iter := 0; iter < pageRankIterations; iter++ {
		// Dangling nodes have nowhere to send their score; pool it
		// and spread it across every node so no mass leaks.
		var dangling float64
		for _, nd := range nodes {
			if outDegree[nd.ID] == 0 {
				dangling += score[nd.ID]
			}
		}
		danglingShare := pageRankDamping * dangling / float64(n)

		next := make(map[string]float64, n)
		for _, nd := range nodes {
			var sum float64
			for _, src := range inLinks[nd.ID] {
				if d := outDegree[src]; d > 0 {
					sum += score[src] / float64(d)
				}
			}
			next[nd.ID] = base + danglingShare + pageRankDamping*sum
		}
		score = next
	}

	var max float64
	for _, v := range score {
		if v > max {
			max = v
		}
	}
	return &PageRankResult{Scores: score, Max: max}
}
