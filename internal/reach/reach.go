// Package reach precomputes per-depth incoming-reachability sets on
// every impact-seed node so blast-radius queries (AnalyzeImpact,
// explain_change_impact, simulate_chain step-impact, prompt
// SafeToChange / PreCommit, diff_context) answer in O(seeds × reach)
// map lookups instead of a live BFS.
//
// The package depends only on internal/graph; it is imported by the
// indexer (build site) and the analysis package (consumer) so the
// import graph stays acyclic — analysis already imports indexer in
// its bench tests.
package reach

import (
	"sort"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// Reachability index keys. Each value is a []string of node IDs that
// can reach the carrier node via incoming edges within the named
// number of hops. Tiers are per-depth (not cumulative) so they map
// 1:1 onto AnalyzeImpact's ByDepth tiers.
//
// Parallel `*_conf` and `*_label` keys carry the representative
// in-edge's Confidence and ConfidenceLabel for each ID, indexed by
// position. They turn the fast path into a pure lookup — no
// GetInEdges calls at query time — so a precomputed AnalyzeImpact
// stays sub-ms even on graphs with high fan-in.
//
// Stored on Node.Meta — gob-serialized into the daemon snapshot so
// warm starts keep O(1) impact lookups without paying the build cost.
const (
	MetaReachD1      = "reach_d1"
	MetaReachD2      = "reach_d2"
	MetaReachD3      = "reach_d3"
	MetaReachD1Conf  = "reach_d1_conf"
	MetaReachD2Conf  = "reach_d2_conf"
	MetaReachD3Conf  = "reach_d3_conf"
	MetaReachD1Label = "reach_d1_label"
	MetaReachD2Label = "reach_d2_label"
	MetaReachD3Label = "reach_d3_label"

	// MetaReachBuild is a monotonic build-generation counter stamped
	// on every node the indexer touched in the most recent reach pass.
	// Consumers compare it against the graph-level counter on
	// AnalyzeImpact entry to decide whether to trust the precomputed
	// sets or fall back to a live walk. The Meta value is a uint64.
	MetaReachBuild = "reach_build"
)

// ReachableEdge returns true when an edge participates in the impact
// graph. Mirrors AnalyzeImpact's filter exactly so the precomputed
// sets and the live walk agree on membership. Exported so the
// AnalyzeImpact live-walk path can share the same filter and tests
// can assert filter parity across the two code paths.
func ReachableEdge(k graph.EdgeKind) bool {
	return k != graph.EdgeDefines && k != graph.EdgeMemberOf
}

// ImpactSeedKind returns true for node kinds that are sensible impact
// seeds — the symbols a developer actually changes. Files, imports,
// parameters, and similar wiring kinds carry no useful blast radius,
// so we skip them to keep the index lean.
func ImpactSeedKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod,
		graph.KindType, graph.KindInterface,
		graph.KindField, graph.KindEnumMember,
		graph.KindConstant, graph.KindVariable:
		return true
	}
	return false
}

// Stats reports the work BuildIndex did.
type Stats struct {
	NodesIndexed int    // nodes that received reach_d* entries
	EntriesD1    int    // total reach_d1 IDs across all indexed nodes
	EntriesD2    int    // total reach_d2 IDs
	EntriesD3    int    // total reach_d3 IDs
	Build        uint64 // generation tag stamped on every indexed node
}

// buildCounter is a process-wide monotonic generation counter used to
// invalidate cached reach sets across snapshot reloads and
// incremental rebuilds. Bumped on every BuildIndex / ClearIndex call.
var buildCounter uint64

// BuildIndex precomputes per-depth incoming reachability sets for
// every impact-seed node in g and stores them under Node.Meta as
// []string slices keyed reach_d1 / reach_d2 / reach_d3. Tiers are
// per-depth (a node appears in at most one tier per seed). The build
// generation is stamped under MetaReachBuild so consumers can detect
// stale entries after partial rebuilds.
//
// Cost: O(N · E_avg) where E_avg is the average reach-3 fan-in
// (typically <200 nodes per seed on real call graphs). Empirically
// completes in well under a second on 50k-node graphs. Run after all
// graph-shaping passes settle (resolver, semantic enrichment, cross-
// repo edges, gRPC stub resolution).
//
// Safe to call repeatedly: existing reach_d* entries are overwritten
// and the build counter advances each time so any consumer that read
// an entry from a prior generation will fall back to a live walk.
func BuildIndex(g *graph.Graph) *Stats {
	if g == nil {
		return &Stats{}
	}
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()
	build := atomic.AddUint64(&buildCounter, 1)
	stats := &Stats{Build: build}

	nodes := g.AllNodes()
	// Sort by ID so the deterministic iteration order produces stable
	// reach slices — important for snapshot determinism and for tests
	// that compare reach payloads across runs.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	for _, n := range nodes {
		if n == nil || !ImpactSeedKind(n.Kind) {
			continue
		}
		tiers := compute(g, n.ID)
		if n.Meta == nil {
			n.Meta = make(map[string]any, 10)
		}
		// Always stamp the build counter so a node with no reach
		// (sink with zero callers) still proves "we tried" — without
		// this, lookups for sink nodes would fall back to a live walk
		// on every call.
		n.Meta[MetaReachBuild] = build
		setOrDeleteStrings(n.Meta, MetaReachD1, tiers[0].IDs)
		setOrDeleteStrings(n.Meta, MetaReachD2, tiers[1].IDs)
		setOrDeleteStrings(n.Meta, MetaReachD3, tiers[2].IDs)
		setOrDeleteFloats(n.Meta, MetaReachD1Conf, tiers[0].Conf)
		setOrDeleteFloats(n.Meta, MetaReachD2Conf, tiers[1].Conf)
		setOrDeleteFloats(n.Meta, MetaReachD3Conf, tiers[2].Conf)
		setOrDeleteStrings(n.Meta, MetaReachD1Label, tiers[0].Labels)
		setOrDeleteStrings(n.Meta, MetaReachD2Label, tiers[1].Labels)
		setOrDeleteStrings(n.Meta, MetaReachD3Label, tiers[2].Labels)

		stats.NodesIndexed++
		stats.EntriesD1 += len(tiers[0].IDs)
		stats.EntriesD2 += len(tiers[1].IDs)
		stats.EntriesD3 += len(tiers[2].IDs)
	}
	return stats
}

// tier holds the per-depth precomputed payload: a parallel triple of
// (ID, edge-confidence, edge-confidence-label) so the fast path can
// hydrate an ImpactEntry without a single GetInEdges call at query
// time. Sorted by ID for stable snapshot output and test parity.
type tier struct {
	IDs    []string
	Conf   []float64
	Labels []string
}

// setOrDeleteStrings keeps Meta lean — empty tiers are removed rather
// than stored as []string{} so cold-start gob payloads stay small and
// downstream code can rely on "key absent" == "no callers at this tier".
func setOrDeleteStrings(m map[string]any, key string, value []string) {
	if len(value) == 0 {
		delete(m, key)
		return
	}
	m[key] = value
}

// setOrDeleteFloats mirrors setOrDeleteStrings for the parallel
// confidence arrays.
func setOrDeleteFloats(m map[string]any, key string, value []float64) {
	if len(value) == 0 {
		delete(m, key)
		return
	}
	m[key] = value
}

// compute walks incoming edges from seed up to depth 3 and returns
// per-depth tiers carrying every ID encountered plus the
// representative in-edge's confidence + label. Each ID appears in at
// most one tier (BFS visited set is shared across depths). Edges are
// filtered with ReachableEdge so the result matches AnalyzeImpact;
// file / import nodes are walked through for fan-out but excluded
// from the tier slices.
func compute(g *graph.Graph, seedID string) [3]tier {
	var result [3]tier
	visited := map[string]struct{}{seedID: {}}
	current := []string{seedID}
	for depth := 1; depth <= 3; depth++ {
		var next []string
		for _, id := range current {
			for _, e := range g.GetInEdges(id) {
				if !ReachableEdge(e.Kind) {
					continue
				}
				if _, seen := visited[e.From]; seen {
					continue
				}
				visited[e.From] = struct{}{}
				next = append(next, e.From)

				if n := g.GetNode(e.From); n == nil ||
					n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				slot := depth - 1
				result[slot].IDs = append(result[slot].IDs, e.From)
				result[slot].Conf = append(result[slot].Conf, e.Confidence)
				result[slot].Labels = append(result[slot].Labels,
					graph.ConfidenceLabelFor(e.Kind, e.Confidence))
			}
		}
		current = next
	}
	for i := range result {
		sortTierByID(&result[i])
	}
	return result
}

// sortTierByID sorts a tier's parallel arrays in lock-step by ID so
// repeated builds produce identical snapshots and consumers can
// binary-search for membership.
func sortTierByID(t *tier) {
	n := len(t.IDs)
	if n <= 1 {
		return
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return t.IDs[idx[a]] < t.IDs[idx[b]] })
	ids := make([]string, n)
	conf := make([]float64, n)
	labels := make([]string, n)
	for newPos, oldPos := range idx {
		ids[newPos] = t.IDs[oldPos]
		conf[newPos] = t.Conf[oldPos]
		labels[newPos] = t.Labels[oldPos]
	}
	t.IDs = ids
	t.Conf = conf
	t.Labels = labels
}

// ClearIndex removes reach_d* and reach_build entries from every node
// and bumps the build counter so any cached lookups dated to a prior
// generation are invalidated. Use when the graph topology has shifted
// so far that a full rebuild is cheaper than incremental invalidation.
func ClearIndex(g *graph.Graph) {
	if g == nil {
		return
	}
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()
	atomic.AddUint64(&buildCounter, 1)
	for _, n := range g.AllNodes() {
		if n == nil || n.Meta == nil {
			continue
		}
		for _, k := range []string{
			MetaReachD1, MetaReachD2, MetaReachD3,
			MetaReachD1Conf, MetaReachD2Conf, MetaReachD3Conf,
			MetaReachD1Label, MetaReachD2Label, MetaReachD3Label,
			MetaReachBuild,
		} {
			delete(n.Meta, k)
		}
	}
}

// Entry is one precomputed reach record: a node ID and the
// representative in-edge's confidence + confidence-label so the
// AnalyzeImpact fast path can hydrate an ImpactEntry with zero
// GetInEdges calls.
type Entry struct {
	ID    string
	Conf  float64
	Label string
}

// Lookup returns the precomputed per-depth reach for seedID and a
// hit boolean. hit=false means the seed lacks a build stamp — either
// the index has not been built yet, the node was added after the
// last build, or its kind is not an impact seed. Callers must fall
// back to a live walk in that case.
func Lookup(g *graph.Graph, seedID string) (d1, d2, d3 []Entry, hit bool) {
	if g == nil {
		return nil, nil, nil, false
	}
	n := g.GetNode(seedID)
	if n == nil || n.Meta == nil {
		return nil, nil, nil, false
	}
	if _, ok := n.Meta[MetaReachBuild]; !ok {
		return nil, nil, nil, false
	}
	d1 = readTier(n.Meta, MetaReachD1, MetaReachD1Conf, MetaReachD1Label)
	d2 = readTier(n.Meta, MetaReachD2, MetaReachD2Conf, MetaReachD2Label)
	d3 = readTier(n.Meta, MetaReachD3, MetaReachD3Conf, MetaReachD3Label)
	return d1, d2, d3, true
}

// readTier reconstructs an []Entry from the parallel arrays. Missing
// confidence / label keys (or shorter slices) zero-fill so older
// snapshots that lack the parallel data degrade gracefully — the
// caller still sees the ID set, just with zero confidence.
func readTier(meta map[string]any, idsKey, confKey, labelKey string) []Entry {
	ids, _ := meta[idsKey].([]string)
	if len(ids) == 0 {
		return nil
	}
	conf, _ := meta[confKey].([]float64)
	labels, _ := meta[labelKey].([]string)
	out := make([]Entry, len(ids))
	for i, id := range ids {
		out[i].ID = id
		if i < len(conf) {
			out[i].Conf = conf[i]
		}
		if i < len(labels) {
			out[i].Label = labels[i]
		}
	}
	return out
}

// BuildCounter returns the current generation tag. Tests use it to
// assert that a rebuild actually bumped the counter.
func BuildCounter() uint64 {
	return atomic.LoadUint64(&buildCounter)
}
