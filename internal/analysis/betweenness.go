package analysis

import (
	"math/rand"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// BetweennessResult holds per-node betweenness-centrality scores.
//
// Betweenness measures how often a node lies on a shortest path
// between two other nodes — a high score marks a structural
// bottleneck that load flows through. It complements PageRank
// (which measures being depended-on) by measuring being a conduit.
type BetweennessResult struct {
	// Scores maps node ID to its betweenness value. Larger means the
	// node sits on more shortest paths; values are best read relative
	// to Max.
	Scores map[string]float64
	// Max is the largest score in Scores — the normaliser callers use
	// to project centrality onto a 0..1 / 0..100 scale.
	Max float64
	// Sampled reports whether the sampled-pivot fast path was used.
	// False means every node was a source (exact Brandes').
	Sampled bool
	// Pivots is the number of source nodes the accumulation ran from.
	// Equals the node count on the exact path.
	Pivots int
}

// ScoreOf returns the betweenness score for a node, or 0 when absent.
func (r *BetweennessResult) ScoreOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Scores[id]
}

// Betweenness tuning.
//
// betweennessExactThreshold is the node-count cutoff between the two
// paths: at or below it every node is a source (exact Brandes',
// O(V*E)); above it the sampled fast path runs from a bounded number
// of pivots (O(k*E)) and rescales by V/k.
//
// betweennessPivots bounds k on the sampled path. ~256 source pivots
// give a stable ranking on graphs into the hundreds of thousands of
// nodes — betweenness is needed for ordering hotspots, not for an
// exact path count, and the sampling error shrinks with graph size.
//
// betweennessSeed fixes the pivot-sampling RNG so a given graph
// yields the same scores on every run.
const (
	betweennessExactThreshold = 2000
	betweennessPivots         = 256
	betweennessSeed           = 0x6f7274
)

// ComputeBetweenness runs betweenness centrality over the call /
// reference graph. It adapts to graph size: small graphs get exact
// Brandes' (every node a source); large graphs switch to the
// sampled-pivot fast path — accumulate from k randomly chosen
// sources and scale the result by V/k. Both paths share the same
// single-source accumulation kernel, so the only difference is which
// sources feed it.
//
// Only EdgeCalls and EdgeReferences participate, matching
// ComputePageRank: structural edges (defines, member_of, imports)
// would swamp the dependency signal. Edges are treated as unweighted
// and directed — shortest paths are hop counts found by BFS.
//
// Pivot sampling is seeded with a fixed seed, so results are
// reproducible run to run.
func ComputeBetweenness(g graph.Store) *BetweennessResult {
	if g == nil {
		return &BetweennessResult{Scores: map[string]float64{}}
	}
	nodes := g.AllNodes()
	n := len(nodes)
	if n == 0 {
		return &BetweennessResult{Scores: map[string]float64{}}
	}

	// Stable node ordering: betweenness itself is order-independent,
	// but a deterministic order makes the sampled pivot pick
	// reproducible regardless of the map-iteration order AllNodes
	// happens to return.
	ids := make([]string, n)
	for i, nd := range nodes {
		ids[i] = nd.ID
	}
	sort.Strings(ids)

	// Forward adjacency over the call / reference subgraph.
	adj := make(map[string][]string, n)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
	}

	score := make(map[string]float64, n)
	for _, id := range ids {
		score[id] = 0
	}

	// Adaptive fast path: exact for small graphs, sampled otherwise.
	sources := ids
	sampled := false
	if n > betweennessExactThreshold {
		sampled = true
		sources = samplePivots(ids, betweennessPivots)
	}

	for _, src := range sources {
		brandesAccumulate(src, ids, adj, score)
	}

	// Rescale the sampled estimate up to a full-source equivalent so
	// the magnitude is comparable to the exact path.
	if sampled && len(sources) > 0 {
		scale := float64(n) / float64(len(sources))
		for id := range score {
			score[id] *= scale
		}
	}

	var max float64
	for _, v := range score {
		if v > max {
			max = v
		}
	}
	return &BetweennessResult{
		Scores:  score,
		Max:     max,
		Sampled: sampled,
		Pivots:  len(sources),
	}
}

// samplePivots picks k distinct source nodes from ids using a
// fixed-seed RNG, so the chosen pivots — and therefore the resulting
// scores — are identical on every run. When k is at or above the
// node count the whole set is returned (the caller would not be on
// the sampled path in that case, but the guard keeps samplePivots
// total).
func samplePivots(ids []string, k int) []string {
	if k >= len(ids) {
		out := make([]string, len(ids))
		copy(out, ids)
		return out
	}
	rng := rand.New(rand.NewSource(betweennessSeed))
	perm := rng.Perm(len(ids))
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = ids[perm[i]]
	}
	return out
}

// brandesAccumulate runs one single-source pass of Brandes'
// algorithm: a BFS from src counts shortest paths, then a reverse
// dependency-accumulation sweep folds this source's contribution
// into score. Intermediate vertices (everything but src and the
// target) collect the credit, which is exactly betweenness.
//
// The graph is unweighted, so a plain BFS yields shortest paths and
// the per-source cost is O(V+E); summed over all sources this is the
// O(V*E) exact bound, or O(k*E) when only k sources feed it.
func brandesAccumulate(src string, ids []string, adj map[string][]string, score map[string]float64) {
	// sigma: number of shortest paths from src to a node.
	// dist:  hop distance from src (-1 = unreached).
	// preds: shortest-path predecessors of a node.
	// order: nodes in non-decreasing distance — the BFS visitation
	//        order, replayed in reverse for the accumulation sweep.
	sigma := make(map[string]float64, len(ids))
	dist := make(map[string]int, len(ids))
	preds := make(map[string][]string, len(ids))
	for _, id := range ids {
		dist[id] = -1
	}
	sigma[src] = 1
	dist[src] = 0

	queue := []string{src}
	order := make([]string, 0, len(ids))
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		order = append(order, v)
		for _, w := range adj[v] {
			// First time w is reached — record its distance and
			// enqueue it.
			if dist[w] < 0 {
				dist[w] = dist[v] + 1
				queue = append(queue, w)
			}
			// w found again along another shortest path — add v's
			// path count and register v as a predecessor.
			if dist[w] == dist[v]+1 {
				sigma[w] += sigma[v]
				preds[w] = append(preds[w], v)
			}
		}
	}

	// Reverse sweep: pop nodes farthest-first and push their
	// accumulated dependency back onto their predecessors.
	delta := make(map[string]float64, len(ids))
	for i := len(order) - 1; i >= 0; i-- {
		w := order[i]
		for _, v := range preds[w] {
			if sigma[w] != 0 {
				delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
			}
		}
		// The source itself is never an intermediate vertex.
		if w != src {
			score[w] += delta[w]
		}
	}
}
