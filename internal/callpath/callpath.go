// Package callpath answers targeted A→B reachability questions over the
// CALLS-class call graph. It is the sibling of internal/dataflow (which walks
// the dataflow edges value_flow/arg_of/returns_to) and of query.Engine's
// single-source call BFS (get_call_chain): callpath traces a *targeted*
// shortest path from one symbol to another and, when no path exists, reports a
// structured why-unreachable diagnosis naming the dynamic-dispatch / external
// boundary where the chain dies.
//
// The engine uses balanced bidirectional BFS: it alternately expands the
// smaller of the two frontiers (one growing forward from the source over OUT
// edges, one growing backward from the sink over IN edges) so it touches
// O(b^(d/2)) nodes instead of O(b^d). BFS on the unweighted call graph yields a
// genuinely shortest path; the level-synchronised meeting test keeps that
// guarantee while collecting every equal-length route for the K-shortest case.
package callpath

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Bounds for the bidirectional search. These mirror the spirit of
// dataflow.DefaultMaxDepth/DefaultMaxPaths: the call-graph diameter is small,
// so the depth cap is a safety valve rather than a tuning knob.
const (
	DefaultMaxDepth    = 24
	DefaultMaxNodes    = 50000
	DefaultMaxFrontier = 25
	DefaultK           = 1

	// maxBoundaryHits caps the boundary-hit list so a hub source that calls
	// hundreds of unresolved targets cannot bloat the gap report.
	maxBoundaryHits = 64
)

// callEdgeKinds is the CALLS-class edge set the engine traces, mirroring
// get_callers/get_call_chain so the answer matches what agents already
// understand: EdgeCalls (direct invocation), EdgeMatches (cross-service
// producer/consumer bridge — lets a path cross repo/service boundaries) and
// EdgeReferences (method-value wiring: mux.HandleFunc, command tables, defer
// x.Cleanup — without it routing codebases look disconnected).
var callEdgeKinds = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches, graph.EdgeReferences}

// ReachReason classifies, for an unreachable pair, *why* the call graph fails
// to connect source→sink. It mirrors the structured-reason pattern of
// graph.ClassifyZeroEdge.
type ReachReason string

const (
	ReasonSrcNotFound      ReachReason = "src_not_found"
	ReasonSinkNotFound     ReachReason = "sink_not_found"
	ReasonSrcNoOut         ReachReason = "src_no_out_edges"
	ReasonSinkNoIn         ReachReason = "sink_no_in_edges"
	ReasonDynamicDispatch  ReachReason = "crosses_dynamic_dispatch"
	ReasonExternalBoundary ReachReason = "crosses_external_boundary"
	ReasonDepthExceeded    ReachReason = "depth_exceeded"
	ReasonDisconnected     ReachReason = "disconnected"
)

// Options tunes a ShortestPath query. The zero value is valid: empty fields
// fall back to the package defaults / the full CALLS-class edge set.
type Options struct {
	// EdgeKinds overrides the traced edge set. Empty uses callEdgeKinds.
	EdgeKinds []graph.EdgeKind
	// IncludeReferences, when false, drops EdgeReferences from the default
	// edge set for a pure direct-call path. Ignored when EdgeKinds is set.
	IncludeReferences bool
	MaxDepth          int
	MaxNodes          int
	// K is the number of distinct shortest-length paths to return (default 1).
	K           int
	MaxFrontier int
	// MinTier prunes edges whose Origin tier is below the threshold during
	// traversal (same semantics as flow_between's min_tier).
	MinTier string
	// WorkspaceID, when set, confines traversal to nodes in the same
	// workspace, so cross-workspace noise does not leak into the gap report.
	WorkspaceID string
}

// PathEdge is one hop on a returned path, carrying provenance so the agent
// sees whether the hop was compiler-verified (lsp), tree-sitter resolved (ast)
// or heuristic.
type PathEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Origin string `json:"origin,omitempty"`
	Tier   string `json:"tier,omitempty"`
}

// Path is one source→sink route. Length is the hop count (edges); a 0-length
// path means source == sink.
type Path struct {
	Nodes      []string   `json:"nodes"`
	Edges      []PathEdge `json:"edges,omitempty"`
	Length     int        `json:"length"`
	Confidence float64    `json:"confidence"`
	WorstTier  string     `json:"worst_tier,omitempty"`
}

// FrontierNode is one node on a search frontier, tagged with its BFS depth.
type FrontierNode struct {
	ID    string `json:"id"`
	Depth int    `json:"depth"`
}

// BoundaryHit records a neighbour the forward search refused to traverse
// because it was an unresolved/dynamic-dispatch target or an external/stub
// boundary — precisely the sites that make the call graph un-connectable.
type BoundaryHit struct {
	From     string `json:"from"`
	Target   string `json:"target"`
	Reason   string `json:"reason"`
	EdgeKind string `json:"edge_kind"`
}

// Gap is the why-unreachable diagnosis returned when no path exists.
type Gap struct {
	Reason             ReachReason    `json:"reason"`
	Message            string         `json:"message"`
	FurthestFromSource []FrontierNode `json:"furthest_from_source,omitempty"`
	NearestToSink      []FrontierNode `json:"nearest_to_sink,omitempty"`
	BoundaryHits       []BoundaryHit  `json:"boundary_hits,omitempty"`
	ForwardReached     int            `json:"forward_reached"`
	BackwardReached    int            `json:"backward_reached"`
}

// Result is the engine's answer. Exactly one of Paths / Gap is populated.
type Result struct {
	Found     bool   `json:"found"`
	SrcID     string `json:"source_id"`
	SinkID    string `json:"sink_id"`
	Paths     []Path `json:"paths,omitempty"`
	Gap       *Gap   `json:"gap,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Engine is the call-path query backend. It holds a reference to the graph and
// is concurrency-safe by relying only on graph.Store's read methods.
type Engine struct {
	g graph.Store
}

// New returns an engine backed by the given graph.
func New(g graph.Store) *Engine { return &Engine{g: g} }

// ShortestPath returns the shortest A→B path over the CALLS-class call graph
// (and up to opts.K equal-length alternates), or a structured Gap diagnosing
// why none exists.
func (e *Engine) ShortestPath(src, sink string, opts Options) Result {
	res := Result{SrcID: src, SinkID: sink}
	if e == nil || e.g == nil || src == "" {
		res.Gap = &Gap{Reason: ReasonSrcNotFound, Message: "source id is empty"}
		return res
	}
	if sink == "" {
		res.Gap = &Gap{Reason: ReasonSinkNotFound, Message: "sink id is empty"}
		return res
	}

	maxDepth := orDefault(opts.MaxDepth, DefaultMaxDepth)
	maxNodes := orDefault(opts.MaxNodes, DefaultMaxNodes)
	k := orDefault(opts.K, DefaultK)
	maxFrontier := orDefault(opts.MaxFrontier, DefaultMaxFrontier)
	kindSet := e.kindSet(opts)

	if e.g.GetNode(src) == nil {
		res.Gap = &Gap{Reason: ReasonSrcNotFound, Message: fmt.Sprintf("%s is not in the graph", src)}
		return res
	}
	if e.g.GetNode(sink) == nil {
		res.Gap = &Gap{Reason: ReasonSinkNotFound, Message: fmt.Sprintf("%s is not in the graph", sink)}
		return res
	}
	if src == sink {
		res.Found = true
		res.Paths = []Path{{Nodes: []string{src}, Length: 0, Confidence: 1}}
		return res
	}

	// Pre-checks that make a path impossible regardless of search effort.
	if !e.hasEdge(src, true, kindSet) {
		res.Gap = e.simpleGap(ReasonSrcNoOut,
			fmt.Sprintf("%s makes no calls — it is a leaf of the call graph, so nothing is reachable from it", src))
		return res
	}
	if !e.hasEdge(sink, false, kindSet) {
		res.Gap = e.simpleGap(ReasonSinkNoIn,
			fmt.Sprintf("nothing calls %s — it has no incoming call edges, so it is unreachable from any source", sink))
		return res
	}

	st := &search{
		eng:         e,
		opts:        opts,
		kindSet:     kindSet,
		maxNodes:    maxNodes,
		fwdParent:   map[string]*graph.Edge{src: nil},
		bwdChild:    map[string]*graph.Edge{sink: nil},
		fwdDepth:    map[string]int{src: 0},
		bwdDepth:    map[string]int{sink: 0},
		boundarySee: map[string]bool{},
	}
	st.fwdFrontier = []string{src}
	st.bwdFrontier = []string{sink}
	st.lastFwd = st.fwdFrontier
	st.lastBwd = st.bwdFrontier

	fLevel, bLevel := 0, 0
	depthExceeded := false
	for len(st.fwdFrontier) > 0 && len(st.bwdFrontier) > 0 {
		if fLevel+bLevel >= maxDepth {
			depthExceeded = true
			break
		}
		if len(st.fwdParent)+len(st.bwdChild) >= maxNodes {
			st.truncated = true
			break
		}

		var meetings []string
		if len(st.fwdFrontier) <= len(st.bwdFrontier) {
			st.fwdFrontier, meetings = st.expandForward(fLevel)
			fLevel++
			if len(st.fwdFrontier) > 0 {
				st.lastFwd = st.fwdFrontier
			}
		} else {
			st.bwdFrontier, meetings = st.expandBackward(bLevel)
			bLevel++
			if len(st.bwdFrontier) > 0 {
				st.lastBwd = st.bwdFrontier
			}
		}

		if len(meetings) > 0 {
			paths := st.buildPaths(src, meetings, k)
			if len(paths) > 0 {
				res.Found = true
				res.Paths = paths
				res.Truncated = st.truncated
				return res
			}
		}
	}

	// No meeting: the pair is unreachable. The balanced search stops as soon
	// as either frontier empties, which can leave the *other* side barely
	// explored — so the gap report would carry a one-sided picture. Complete
	// both reach cones (bounded by depth/node caps) purely to populate a
	// symmetric "A reaches … / … reaches B" diagnosis. This cannot surface a
	// missed path: forward and backward apply identical boundary/tier/scope
	// pruning, so any node reachable both ways would already have met.
	if !depthExceeded && !st.truncated {
		for len(st.fwdFrontier) > 0 && fLevel < maxDepth && len(st.fwdParent)+len(st.bwdChild) < maxNodes {
			st.fwdFrontier, _ = st.expandForward(fLevel)
			fLevel++
			if len(st.fwdFrontier) > 0 {
				st.lastFwd = st.fwdFrontier
			}
		}
		for len(st.bwdFrontier) > 0 && bLevel < maxDepth && len(st.fwdParent)+len(st.bwdChild) < maxNodes {
			st.bwdFrontier, _ = st.expandBackward(bLevel)
			bLevel++
			if len(st.bwdFrontier) > 0 {
				st.lastBwd = st.bwdFrontier
			}
		}
	}

	res.Gap = st.buildGap(maxFrontier, depthExceeded)
	res.Truncated = st.truncated
	return res
}

// search holds the mutable state of one bidirectional sweep.
type search struct {
	eng      *Engine
	opts     Options
	kindSet  map[graph.EdgeKind]bool
	maxNodes int

	fwdParent map[string]*graph.Edge // node → edge that discovered it forward (edge.To == node)
	bwdChild  map[string]*graph.Edge // node → edge toward the sink (edge.From == node)
	fwdDepth  map[string]int
	bwdDepth  map[string]int

	fwdFrontier []string
	bwdFrontier []string
	lastFwd     []string
	lastBwd     []string

	boundary    []BoundaryHit
	boundarySee map[string]bool
	truncated   bool
}

func (s *search) expandForward(level int) (next []string, meetings []string) {
	for _, u := range s.fwdFrontier {
		for _, ed := range s.eng.g.GetOutEdges(u) {
			if !s.kindSet[ed.Kind] {
				continue
			}
			v := ed.To
			if _, seen := s.fwdParent[v]; seen {
				continue
			}
			if reason, isB := classifyBoundary(v); isB {
				s.addBoundary(u, v, reason, string(ed.Kind))
				continue
			}
			if !s.tierOK(ed) || !s.eng.scopeOK(v, s.opts) {
				continue
			}
			s.fwdParent[v] = ed
			s.fwdDepth[v] = level + 1
			next = append(next, v)
			if _, ok := s.bwdChild[v]; ok {
				meetings = append(meetings, v)
			}
		}
	}
	return next, meetings
}

func (s *search) expandBackward(level int) (next []string, meetings []string) {
	for _, u := range s.bwdFrontier {
		for _, ed := range s.eng.g.GetInEdges(u) {
			if !s.kindSet[ed.Kind] {
				continue
			}
			w := ed.From
			if _, seen := s.bwdChild[w]; seen {
				continue
			}
			// A boundary node is never a real caller; skip without recording
			// (boundary hits are a forward-search concept — they name the
			// dynamic-dispatch sites the source's reach terminates at).
			if _, isB := classifyBoundary(w); isB {
				continue
			}
			if !s.tierOK(ed) || !s.eng.scopeOK(w, s.opts) {
				continue
			}
			s.bwdChild[w] = ed
			s.bwdDepth[w] = level + 1
			next = append(next, w)
			if _, ok := s.fwdParent[w]; ok {
				meetings = append(meetings, w)
			}
		}
	}
	return next, meetings
}

func (s *search) tierOK(ed *graph.Edge) bool {
	if s.opts.MinTier == "" {
		return true
	}
	return graph.MeetsMinTier(edgeOriginOf(ed), s.opts.MinTier)
}

func (s *search) addBoundary(from, target, reason, edgeKind string) {
	if s.boundarySee[target] || len(s.boundary) >= maxBoundaryHits {
		return
	}
	s.boundarySee[target] = true
	s.boundary = append(s.boundary, BoundaryHit{From: from, Target: target, Reason: reason, EdgeKind: edgeKind})
}

// buildPaths reconstructs distinct shortest-length paths through the supplied
// meeting nodes. Among the meetings discovered in the meeting level it keeps
// only those whose total length equals the minimum, then dedupes by node
// sequence, ranks by (length asc, confidence desc) and returns up to k.
func (s *search) buildPaths(src string, meetings []string, k int) []Path {
	type cand struct {
		total int
		node  string
	}
	cands := make([]cand, 0, len(meetings))
	minTotal := 1 << 30
	for _, m := range meetings {
		t := s.fwdDepth[m] + s.bwdDepth[m]
		cands = append(cands, cand{total: t, node: m})
		if t < minTotal {
			minTotal = t
		}
	}
	seen := map[string]bool{}
	var paths []Path
	for _, c := range cands {
		if c.total != minTotal {
			continue
		}
		p := s.reconstruct(src, c.node)
		key := strings.Join(p.Nodes, ">")
		if seen[key] {
			continue
		}
		seen[key] = true
		paths = append(paths, p)
	}
	sort.SliceStable(paths, func(i, j int) bool {
		if paths[i].Length != paths[j].Length {
			return paths[i].Length < paths[j].Length
		}
		return paths[i].Confidence > paths[j].Confidence
	})
	if k > 0 && len(paths) > k {
		paths = paths[:k]
	}
	return paths
}

// reconstruct splices the forward chain (src→meet) and the backward chain
// (meet→sink) into one path.
func (s *search) reconstruct(src, meet string) Path {
	var fwd []*graph.Edge
	for cur := meet; ; {
		ed := s.fwdParent[cur]
		if ed == nil {
			break
		}
		fwd = append(fwd, ed)
		cur = ed.From
	}
	// fwd is meet→…→src; reverse to src→…→meet.
	for i, j := 0, len(fwd)-1; i < j; i, j = i+1, j-1 {
		fwd[i], fwd[j] = fwd[j], fwd[i]
	}
	var bwd []*graph.Edge
	for cur := meet; ; {
		ed := s.bwdChild[cur]
		if ed == nil {
			break
		}
		bwd = append(bwd, ed)
		cur = ed.To
	}
	all := append(fwd, bwd...)

	nodes := make([]string, 0, len(all)+1)
	nodes = append(nodes, src)
	edges := make([]PathEdge, 0, len(all))
	score := 1.0
	worst := ""
	for _, ed := range all {
		nodes = append(nodes, ed.To)
		origin := edgeOriginOf(ed)
		tier := graph.ResolvedBy(origin)
		edges = append(edges, PathEdge{From: ed.From, To: ed.To, Kind: string(ed.Kind), Origin: origin, Tier: tier})
		score *= graph.EdgeTierScore(origin, ed.Kind)
		worst = mergeWorstTier(worst, tier)
	}
	return Path{Nodes: nodes, Edges: edges, Length: len(all), Confidence: score, WorstTier: worst}
}

func (s *search) buildGap(maxFrontier int, depthExceeded bool) *Gap {
	g := &Gap{
		FurthestFromSource: s.frontierNodes(s.lastFwd, s.fwdDepth, maxFrontier),
		NearestToSink:      s.frontierNodes(s.lastBwd, s.bwdDepth, maxFrontier),
		BoundaryHits:       s.boundary,
		ForwardReached:     len(s.fwdParent) - 1,
		BackwardReached:    len(s.bwdChild) - 1,
	}
	hasDyn, hasExt := false, false
	var dynNames []string
	for _, b := range s.boundary {
		if b.Reason == boundaryDynamicDispatch {
			hasDyn = true
			if len(dynNames) < 3 {
				dynNames = append(dynNames, graph.UnresolvedName(b.Target))
			}
		} else {
			hasExt = true
		}
	}
	switch {
	case depthExceeded:
		g.Reason = ReasonDepthExceeded
		g.Message = fmt.Sprintf("the search hit the depth bound before the forward reach (%d) and backward reach (%d) met — raise --depth or narrow the endpoints",
			g.ForwardReached, g.BackwardReached)
	case hasDyn:
		g.Reason = ReasonDynamicDispatch
		g.Message = fmt.Sprintf("%s reaches %d functions and %s is reachable from %d, but the forward reach terminates at %d dynamic-dispatch call site(s) (e.g. %s) that the resolver never bound — try find_implementations on the interface, or rerun with a lower min_tier",
			s.srcLabel(), g.ForwardReached, s.sinkLabel(), g.BackwardReached, len(dynNames), strings.Join(dynNames, ", "))
	case hasExt:
		g.Reason = ReasonExternalBoundary
		g.Message = fmt.Sprintf("%s reaches %d functions and %s is reachable from %d, but the chain leaves the indexed code at an external/stdlib boundary — the call graph cannot connect them through resolved code",
			s.srcLabel(), g.ForwardReached, s.sinkLabel(), g.BackwardReached)
	default:
		g.Reason = ReasonDisconnected
		g.Message = fmt.Sprintf("%s reaches %d functions and %s is reachable from %d, but the two reachable sets are disjoint — no call path connects them",
			s.srcLabel(), g.ForwardReached, s.sinkLabel(), g.BackwardReached)
	}
	return g
}

func (s *search) srcLabel() string  { return shortLabel(s.fwdRoot()) }
func (s *search) sinkLabel() string { return shortLabel(s.bwdRoot()) }

func (s *search) fwdRoot() string {
	for id, e := range s.fwdParent {
		if e == nil {
			return id
		}
	}
	return ""
}
func (s *search) bwdRoot() string {
	for id, e := range s.bwdChild {
		if e == nil {
			return id
		}
	}
	return ""
}

func (s *search) frontierNodes(ids []string, depth map[string]int, max int) []FrontierNode {
	if len(ids) == 0 {
		return nil
	}
	out := make([]FrontierNode, 0, len(ids))
	for _, id := range ids {
		out = append(out, FrontierNode{ID: id, Depth: depth[id]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// simpleGap builds a one-line gap for the trivial impossible cases.
func (e *Engine) simpleGap(reason ReachReason, msg string) *Gap {
	return &Gap{Reason: reason, Message: msg}
}

func (e *Engine) kindSet(opts Options) map[graph.EdgeKind]bool {
	kinds := opts.EdgeKinds
	if len(kinds) == 0 {
		// The default edge set mirrors get_callers (calls + matches +
		// references). Callers wanting a pure direct-call path pass
		// IncludeReferences=false, which drops the method-value wiring edges.
		kinds = callEdgeKinds
		if !opts.IncludeReferences {
			kinds = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches}
		}
	}
	set := make(map[graph.EdgeKind]bool, len(kinds))
	for _, k := range kinds {
		set[k] = true
	}
	return set
}

// hasEdge reports whether id has at least one edge of a traced kind in the
// given direction (forward = out edges).
func (e *Engine) hasEdge(id string, forward bool, kindSet map[graph.EdgeKind]bool) bool {
	var edges []*graph.Edge
	if forward {
		edges = e.g.GetOutEdges(id)
	} else {
		edges = e.g.GetInEdges(id)
	}
	for _, ed := range edges {
		if kindSet[ed.Kind] {
			return true
		}
	}
	return false
}

func (e *Engine) scopeOK(id string, opts Options) bool {
	if opts.WorkspaceID == "" {
		return true
	}
	n := e.g.GetNode(id)
	if n == nil || n.WorkspaceID == "" {
		return true
	}
	return n.WorkspaceID == opts.WorkspaceID
}

// boundary reason labels. dynamic_dispatch is special-cased in the gap
// classifier; the rest come straight from graph.StubKind.
const boundaryDynamicDispatch = "dynamic_dispatch"

// classifyBoundary maps a neighbour id to a boundary reason, or returns
// isBoundary=false for an ordinary in-graph node.
func classifyBoundary(id string) (reason string, isBoundary bool) {
	if graph.IsUnresolvedTarget(id) {
		return boundaryDynamicDispatch, true
	}
	if strings.HasPrefix(id, "external::") {
		return "external_namespace", true
	}
	if k := graph.StubKind(id); k != "" {
		return k, true
	}
	return "", false
}

// edgeOriginOf returns the stamped Origin, falling back to DefaultOriginFor so
// back-compat graphs classify cleanly (identical to dataflow.edgeOrigin).
func edgeOriginOf(e *graph.Edge) string {
	if e.Origin != "" {
		return e.Origin
	}
	src, _ := e.Meta["semantic_source"].(string)
	return graph.DefaultOriginFor(e.Kind, e.Confidence, src)
}

// tierRank orders the coarse tier labels worst→best for mergeWorstTier.
var tierRank = map[string]int{"heuristic": 0, "ast": 1, "lsp": 2}

// mergeWorstTier returns the weaker of two coarse tier labels.
func mergeWorstTier(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if tierRank[b] < tierRank[a] {
		return b
	}
	return a
}

func shortLabel(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[i+2:]
	}
	return id
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
