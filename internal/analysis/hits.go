package analysis

import (
	"math"

	"github.com/zzet/gortex/internal/graph"
)

// HITSResult holds per-node HITS scores -- the authority and hub
// values from Kleinberg's Hyperlink-Induced Topic Search.
//
// In a call/reference graph the two roles read as:
//   - Authority: a symbol called by many *hubs* -- a load-bearing
//     definition that coordinator code converges on.
//   - Hub: a symbol that calls many *authorities* -- an orchestrator
//     / dispatcher that pulls many load-bearing pieces together.
//
// A pure utility called by everything scores HIGH on the in-degree
// count fan-in already captures, but only MODERATELY on authority
// unless its callers are themselves authoritative -- which is exactly
// the discrimination the rerank pipeline wants on top of raw fan-in.
type HITSResult struct {
	// Authorities maps node ID to its authority score.
	Authorities map[string]float64
	// Hubs maps node ID to its hub score.
	Hubs map[string]float64
	// MaxAuth / MaxHub are the largest values in each map -- the
	// normalisers callers use to project onto a 0..1 scale.
	MaxAuth float64
	MaxHub  float64
}

// AuthorityOf returns the authority score for a node, or 0 when
// absent or the result is nil.
func (r *HITSResult) AuthorityOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Authorities[id]
}

// HubOf returns the hub score for a node, or 0 when absent or the
// result is nil.
func (r *HITSResult) HubOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Hubs[id]
}

// hitsIterations fixes the power-iteration count. HITS converges
// fast; 40 steps mirror ComputePageRank and are well past the point
// the authority/hub ordering stabilises on a graph of this size.
const hitsIterations = 40

// ComputeHITS runs the HITS algorithm over the call / reference
// graph. Only EdgeCalls and EdgeReferences participate -- the same
// edge set ComputePageRank uses, so structural edges (defines,
// member_of, imports) do not drown the dependency signal.
//
// Each iteration applies the mutual recursion
//
//	auth(p) = sum over q->p of hub(q)
//	hub(p)  = sum over p->q of auth(q)
//
// then L2-normalises both vectors so the scores stay bounded. A nil
// or empty graph yields an empty, safe-to-query result.
func ComputeHITS(g graph.Store) *HITSResult {
	if g == nil {
		return &HITSResult{Authorities: map[string]float64{}, Hubs: map[string]float64{}}
	}
	nodes := g.AllNodes()
	n := len(nodes)
	if n == 0 {
		return &HITSResult{Authorities: map[string]float64{}, Hubs: map[string]float64{}}
	}

	// Adjacency restricted to the call/reference edge set. outLinks
	// drives the hub update; inLinks drives the authority update.
	outLinks := make(map[string][]string)
	inLinks := make(map[string][]string)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		outLinks[e.From] = append(outLinks[e.From], e.To)
		inLinks[e.To] = append(inLinks[e.To], e.From)
	}

	auth := make(map[string]float64, n)
	hub := make(map[string]float64, n)
	for _, nd := range nodes {
		auth[nd.ID] = 1.0
		hub[nd.ID] = 1.0
	}

	for iter := 0; iter < hitsIterations; iter++ {
		// Authority update: a node's authority is the sum of the hub
		// scores of the nodes pointing at it.
		nextAuth := make(map[string]float64, n)
		for _, nd := range nodes {
			var sum float64
			for _, src := range inLinks[nd.ID] {
				sum += hub[src]
			}
			nextAuth[nd.ID] = sum
		}
		// Hub update: a node's hub score is the sum of the (just
		// updated) authority scores of the nodes it points at.
		nextHub := make(map[string]float64, n)
		for _, nd := range nodes {
			var sum float64
			for _, dst := range outLinks[nd.ID] {
				sum += nextAuth[dst]
			}
			nextHub[nd.ID] = sum
		}
		normalizeL2(nextAuth)
		normalizeL2(nextHub)
		auth, hub = nextAuth, nextHub
	}

	res := &HITSResult{Authorities: auth, Hubs: hub}
	for _, v := range auth {
		if v > res.MaxAuth {
			res.MaxAuth = v
		}
	}
	for _, v := range hub {
		if v > res.MaxHub {
			res.MaxHub = v
		}
	}
	return res
}

// normalizeL2 scales a score vector in place to unit L2 norm. A
// zero vector (no edges in the participating set) is left untouched
// so the next iteration starts from a defined state.
func normalizeL2(m map[string]float64) {
	var sumSq float64
	for _, v := range m {
		sumSq += v * v
	}
	if sumSq == 0 {
		return
	}
	norm := math.Sqrt(sumSq)
	for k := range m {
		m[k] /= norm
	}
}
