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
	"context"
	"sort"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/progress"
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
func BuildIndex(g graph.Store) *Stats {
	return BuildIndexCtx(context.Background(), g)
}

// BuildIndexCtx is BuildIndex with intra-stage progress reporting.
// Pulls a progress.Reporter from ctx (no-op when none is attached) and
// emits per-seed progress every reachProgressEvery seeds — the pass
// otherwise looks hung from the outside, since "reach" is one of the
// longest stages on monorepo-scale graphs (~200 s on k8s with 150 k
// impact seeds). Pure operator-visibility instrumentation: the per-
// report call is cheap (no I/O when the reporter is the default no-op).
func BuildIndexCtx(ctx context.Context, g graph.Store) *Stats {
	if g == nil {
		return &Stats{}
	}
	reporter := progress.FromContext(ctx)
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

	// Pre-count impact seeds so the progress denominator is real, not
	// the total node count (the loop skips ~80% of nodes — files,
	// imports, params, vars, …).
	var seedTotal int
	for _, n := range nodes {
		if n != nil && ImpactSeedKind(n.Kind) {
			seedTotal++
		}
	}
	reporter.Report("reachability index", 0, seedTotal)

	const reachProgressEvery = 1000
	seedsDone := 0
	// Collect the seed nodes we stamp so we can persist the Meta back
	// through the store in one batch at the end. On the in-memory
	// backend the in-place stamp already persists (n is canonical); on
	// disk backends (Ladybug) n is a GetNode reconstruction, so without
	// the write-back the whole reach index would be computed and then
	// thrown away. Mirrors the per-seed AddNode in Lookup's slow path.
	stamped := make([]*graph.Node, 0, seedTotal)
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

		stamped = append(stamped, n)
		stats.NodesIndexed++
		stats.EntriesD1 += len(tiers[0].IDs)
		stats.EntriesD2 += len(tiers[1].IDs)
		stats.EntriesD3 += len(tiers[2].IDs)

		seedsDone++
		if seedsDone%reachProgressEvery == 0 {
			reporter.Report("reachability index", seedsDone, seedTotal)
		}
	}
	// Persist every stamped node's Meta back through the store in one
	// batch (no-op-ish on the in-memory backend, the durable write on
	// disk backends). AddBatch with no edges only upserts the nodes.
	if len(stamped) > 0 {
		g.AddBatch(stamped, nil)
	}
	reporter.Report("reachability index", seedsDone, seedTotal)
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
func compute(g graph.Store, seedID string) [3]tier {
	var result [3]tier
	visited := map[string]struct{}{seedID: {}}
	current := []string{seedID}
	for depth := 1; depth <= 3 && len(current) > 0; depth++ {
		// Batch the whole BFS level's incoming-edge fetch into one
		// backend round-trip. The per-node g.GetInEdges(id) form issued
		// one Cypher query + cgo crossing per node on disk backends — an
		// O(reachable-nodes) query storm that turned a single
		// AnalyzeImpact live walk into a multi-minute (timeout) call on
		// Ladybug. GetInEdgesByNodeIDs collapses it to one query per depth.
		inEdges := g.GetInEdgesByNodeIDs(current)

		// First pass: discover this level's new From-nodes in
		// deterministic (current-order, edge-order) order, recording the
		// representative in-edge for each.
		type cand struct {
			from string
			conf float64
			kind graph.EdgeKind
		}
		var next []string
		var cands []cand
		for _, id := range current {
			for _, e := range inEdges[id] {
				if !ReachableEdge(e.Kind) {
					continue
				}
				if _, seen := visited[e.From]; seen {
					continue
				}
				visited[e.From] = struct{}{}
				next = append(next, e.From)
				cands = append(cands, cand{from: e.From, conf: e.Confidence, kind: e.Kind})
			}
		}

		// Batch the node-kind lookups too — the original called
		// g.GetNode(e.From) once per discovered node (a second per-node
		// query storm on disk backends). File / import nodes are still
		// walked through for fan-out (they stay in `next`) but excluded
		// from the result tiers, exactly as before.
		ids := make([]string, len(cands))
		for i := range cands {
			ids[i] = cands[i].from
		}
		nodes := g.GetNodesByIDs(ids)
		slot := depth - 1
		for _, c := range cands {
			n := nodes[c.from]
			if n == nil || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			result[slot].IDs = append(result[slot].IDs, c.from)
			result[slot].Conf = append(result[slot].Conf, c.conf)
			result[slot].Labels = append(result[slot].Labels,
				graph.ConfidenceLabelFor(c.kind, c.conf))
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
func ClearIndex(g graph.Store) {
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

// Lookup returns the per-depth reach for seedID. On a fresh cache hit
// (build counter matches current generation) it returns the cached
// tiers in sub-millisecond. On a miss — first call for this seed, or
// the global build counter has advanced past the stamped value
// because the graph mutated — it runs the BFS on demand under
// g.ResolveMutex(), caches the result onto n.Meta, and returns the
// fresh tiers. Returns hit=false only when seedID names no node or
// names a node whose kind is not an impact seed (KindFunction,
// KindMethod, KindType, KindInterface).
//
// This is the "lazy reach index" — the eager BuildIndex pass that
// used to walk every impact seed during cold-index has been removed
// from the IndexCtx hot path because the breakeven was untenable on
// monorepo graphs: ~2000 s of cold-index work on k8s to save ~10 ms
// per query, requiring ~200 k queries to break even. The lazy form
// pays the 10 ms only on the first AnalyzeImpact call that names a
// given seed, then caches forever. BuildIndex remains available for
// `gortex enrich reach` (explicit prebuild) and for callers that
// want to pay the cost up front under controlled conditions.
func Lookup(g graph.Store, seedID string) (d1, d2, d3 []Entry, hit bool) {
	if g == nil {
		return nil, nil, nil, false
	}
	n := g.GetNode(seedID)
	if n == nil {
		return nil, nil, nil, false
	}
	if !ImpactSeedKind(n.Kind) {
		return nil, nil, nil, false
	}

	currentBuild := atomic.LoadUint64(&buildCounter)
	// Fast path: existing stamp matches the current build generation.
	if d1, d2, d3, ok := readCached(n, currentBuild); ok {
		return d1, d2, d3, true
	}

	// Slow path: compute the tiers and cache them. Acquire the resolve
	// mutex so the Meta writes don't race other graph-wide passes that
	// already serialise on it (markTestSymbolsAndEmitEdges, clone
	// detection, ResolveTemporalCalls).
	mu := g.ResolveMutex()
	mu.Lock()
	defer mu.Unlock()

	// Re-check after acquiring the lock: another goroutine may have
	// computed and cached this seed while we were waiting.
	if d1, d2, d3, ok := readCached(n, currentBuild); ok {
		return d1, d2, d3, true
	}

	tiers := compute(g, seedID)
	if n.Meta == nil {
		n.Meta = make(map[string]any, 10)
	}
	n.Meta[MetaReachBuild] = currentBuild
	setOrDeleteStrings(n.Meta, MetaReachD1, tiers[0].IDs)
	setOrDeleteStrings(n.Meta, MetaReachD2, tiers[1].IDs)
	setOrDeleteStrings(n.Meta, MetaReachD3, tiers[2].IDs)
	setOrDeleteFloats(n.Meta, MetaReachD1Conf, tiers[0].Conf)
	setOrDeleteFloats(n.Meta, MetaReachD2Conf, tiers[1].Conf)
	setOrDeleteFloats(n.Meta, MetaReachD3Conf, tiers[2].Conf)
	setOrDeleteStrings(n.Meta, MetaReachD1Label, tiers[0].Labels)
	setOrDeleteStrings(n.Meta, MetaReachD2Label, tiers[1].Labels)
	setOrDeleteStrings(n.Meta, MetaReachD3Label, tiers[2].Labels)

	// Persist the freshly-stamped Meta through the store. On the
	// in-memory backend n is the canonical node, so the mutations above
	// already stuck — AddNode re-inserts the same pointer idempotently.
	// On disk backends (Ladybug) n is a per-call reconstruction returned
	// by GetNode, so the in-place stamp would otherwise be discarded the
	// moment this function returns: the lazy reach cache would never
	// survive a single query, forcing a full recompute on every
	// AnalyzeImpact / explain_change_impact / get_callers call. AddNode
	// upserts the Meta column so the cache actually sticks.
	g.AddNode(n)

	d1 = readTier(n.Meta, MetaReachD1, MetaReachD1Conf, MetaReachD1Label)
	d2 = readTier(n.Meta, MetaReachD2, MetaReachD2Conf, MetaReachD2Label)
	d3 = readTier(n.Meta, MetaReachD3, MetaReachD3Conf, MetaReachD3Label)
	return d1, d2, d3, true
}

// readCached reads the stamped reach tiers off n.Meta when the stamp
// matches currentBuild. Returns ok=false when the stamp is missing
// (never built), stale (graph has changed since), or has the wrong
// Go type (snapshot from an older format).
func readCached(n *graph.Node, currentBuild uint64) (d1, d2, d3 []Entry, ok bool) {
	if n.Meta == nil {
		return nil, nil, nil, false
	}
	raw, present := n.Meta[MetaReachBuild]
	if !present {
		return nil, nil, nil, false
	}
	var stamped uint64
	switch v := raw.(type) {
	case uint64:
		stamped = v
	case uint32:
		stamped = uint64(v)
	case int:
		stamped = uint64(v)
	case int64:
		stamped = uint64(v)
	default:
		return nil, nil, nil, false
	}
	if stamped != currentBuild {
		return nil, nil, nil, false
	}
	d1 = readTier(n.Meta, MetaReachD1, MetaReachD1Conf, MetaReachD1Label)
	d2 = readTier(n.Meta, MetaReachD2, MetaReachD2Conf, MetaReachD2Label)
	d3 = readTier(n.Meta, MetaReachD3, MetaReachD3Conf, MetaReachD3Label)
	return d1, d2, d3, true
}

// InvalidateIndex advances the global build counter so every future
// Lookup recomputes against the new graph state. Call this whenever
// the graph mutates in a way that could change reach sets — at the
// end of every IndexCtx / IncrementalReindex / global-pass run.
//
// The cached Meta entries on nodes that survived the mutation are
// not deleted; they're simply tagged with a stale build counter, so
// the next Lookup on each falls through to a fresh compute. This is
// strictly cheaper than walking all nodes to clear Meta — the
// invalidation is O(1) and only the seeds actually queried pay the
// recompute cost.
func InvalidateIndex() {
	atomic.AddUint64(&buildCounter, 1)
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
