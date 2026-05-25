// Package dataflow implements the CPG-lite dataflow primitives
// surfaced by the flow_between and taint_paths MCP tools. It walks
// EdgeValueFlow / EdgeArgOf / EdgeReturnsTo edges over the live
// graph to answer two questions agents care about:
//
//  1. flow_between(source_id, sink_id, max_depth) — list every
//     ranked path that connects two specific symbols. Used for
//     refactor-safety questions ("if I change f's return type,
//     every site that ultimately consumes that value") and bug
//     investigation ("trace where this wrong value originated").
//
//  2. taint_paths(source_pattern, sink_pattern) — pattern-driven
//     resolution of source / sink sets, then a flow_between for
//     each candidate pair. Used for security-style queries
//     ("every flow from os.Getenv to db.Query") and architectural
//     audits ("every flow from request.Body to a logger").
//
// The traversal forms the smallest useful subset of Joern's CPG
// reachability primitives that the segment can ship inside an MCP
// surface — intra-procedural data dependence, captured at
// extraction time as EdgeValueFlow, plus inter-procedural binding
// at every call site, captured as EdgeArgOf / EdgeReturnsTo.
//
// Direction. flow_between always walks forward — out-edges of the
// current frontier — because every dataflow edge points in the
// direction of value movement: a value_flow goes source→consumer,
// an arg_of goes argument→callee param, and a returns_to goes
// callee→assignment. BFS over OutEdges therefore traces "where
// does this value go". Reverse flow ("where did this value come
// from") is a future addition; today the user can swap source
// and sink to walk the same edges from the other end.
package dataflow

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// DefaultMaxDepth bounds how far BFS will look — each hop on a
// dataflow edge counts. Eight is empirically wide enough to cover
// most real handlers and security-relevant flows while keeping
// pathological "fully connected" graphs from blowing the response
// budget.
const DefaultMaxDepth = 8

// DefaultMaxPaths bounds how many distinct paths flow_between
// will return for a single (source, sink) pair. The handler ranks
// by length first, then by edge-confidence sum, so the user gets
// the most plausible paths first.
const DefaultMaxPaths = 10

// EdgeStep is one hop along a flow path. It carries the edge kind,
// origin tier, and coarse tier label so the caller can distinguish a
// strong intra-procedural chain from a heuristic inter-procedural
// binding without recomputing the origin → tier mapping.
type EdgeStep struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Origin string `json:"origin,omitempty"`
	Tier   string `json:"tier,omitempty"`
}

// Path is an ordered sequence of edge hops from a source node to
// a sink node. The IDs slice is the source-first node sequence and
// is always one longer than Edges. Confidence is a normalised
// 0-1 score derived from edge origin tiers — higher means a
// stronger end-to-end binding.
type Path struct {
	IDs        []string   `json:"ids"`
	Edges      []EdgeStep `json:"edges"`
	Confidence float64    `json:"confidence"`
}

// Length returns the number of hops in the path.
func (p Path) Length() int { return len(p.Edges) }

// Engine is the dataflow query backend. It holds a reference to
// the graph and exposes the two MCP-ready primitives. Concurrency-
// safe by virtue of relying only on graph.Store's read methods.
type Engine struct {
	g graph.Store
}

// New returns an engine backed by the given graph.
func New(g graph.Store) *Engine { return &Engine{g: g} }

// IsDataflowKind returns true for the three edge kinds the BFS
// traverses.
func IsDataflowKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeValueFlow, graph.EdgeArgOf, graph.EdgeReturnsTo:
		return true
	}
	return false
}

// FlowBetween returns up to maxPaths shortest paths from sourceID
// to sinkID, walking forward over dataflow edges. Returns nil when
// no path exists within maxDepth hops.
//
// maxDepth and maxPaths are clamped to safe defaults when zero.
func (e *Engine) FlowBetween(sourceID, sinkID string, maxDepth, maxPaths int) []Path {
	return e.FlowBetweenWithTier(sourceID, sinkID, maxDepth, maxPaths, "")
}

// FlowBetweenWithTier is FlowBetween with an additional provenance
// filter: edges whose backfilled Origin tier ranks below minTier are
// skipped during traversal, pruning entire branches that cannot
// produce a fully-resolved path. Empty minTier disables the filter
// (identical to FlowBetween). The per-step Tier label is stamped on
// every retained EdgeStep so callers do not need to recompute it.
func (e *Engine) FlowBetweenWithTier(sourceID, sinkID string, maxDepth, maxPaths int, minTier string) []Path {
	if e == nil || e.g == nil || sourceID == "" || sinkID == "" {
		return nil
	}
	if sourceID == sinkID {
		return []Path{{IDs: []string{sourceID}, Confidence: 1}}
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if maxPaths <= 0 {
		maxPaths = DefaultMaxPaths
	}

	// DFS with depth bound. We track the current path's node set
	// to prevent cycles. Memoisation by (node, depth) would help
	// pathological graphs but the maxDepth cap is already
	// effective on real codebases — the typical taint walk hits
	// 4-6 hops before either landing on the sink or running out
	// of frontier.
	var paths []Path
	visited := make(map[string]bool, 32)
	stack := []string{sourceID}
	steps := []EdgeStep{}
	visited[sourceID] = true

	var dfs func(nodeID string, depth int)
	dfs = func(nodeID string, depth int) {
		if len(paths) >= maxPaths {
			return
		}
		if depth >= maxDepth {
			return
		}
		out := e.g.GetOutEdges(nodeID)
		for _, ed := range out {
			if !IsDataflowKind(ed.Kind) {
				continue
			}
			if visited[ed.To] {
				continue
			}
			origin := edgeOrigin(ed)
			if minTier != "" && !graph.MeetsMinTier(origin, minTier) {
				continue
			}
			step := EdgeStep{
				From:   ed.From,
				To:     ed.To,
				Kind:   string(ed.Kind),
				Origin: origin,
				Tier:   graph.ResolvedBy(origin),
			}
			if ed.To == sinkID {
				ids := append([]string(nil), stack...)
				ids = append(ids, ed.To)
				edgeCopy := append([]EdgeStep(nil), steps...)
				edgeCopy = append(edgeCopy, step)
				paths = append(paths, Path{
					IDs:        ids,
					Edges:      edgeCopy,
					Confidence: confidenceFromEdges(edgeCopy),
				})
				if len(paths) >= maxPaths {
					return
				}
				continue
			}
			visited[ed.To] = true
			stack = append(stack, ed.To)
			steps = append(steps, step)
			dfs(ed.To, depth+1)
			stack = stack[:len(stack)-1]
			steps = steps[:len(steps)-1]
			delete(visited, ed.To)
		}
	}
	dfs(sourceID, 0)

	rankPaths(paths)
	if len(paths) > maxPaths {
		paths = paths[:maxPaths]
	}
	return paths
}

// edgeOrigin returns the stamped Origin on an edge, falling back to
// DefaultOriginFor when the field is empty so back-compat graphs
// (produced before Origin was a first-class field) still classify
// cleanly for filtering and tier surfacing.
func edgeOrigin(e *graph.Edge) string {
	if e.Origin != "" {
		return e.Origin
	}
	src, _ := e.Meta["semantic_source"].(string)
	return graph.DefaultOriginFor(e.Kind, e.Confidence, src)
}

// rankPaths sorts in-place by length asc, then by confidence desc.
// Shorter, higher-confidence paths sort first so the agent always
// sees the most plausible explanation before the more speculative
// chains.
func rankPaths(paths []Path) {
	sort.SliceStable(paths, func(i, j int) bool {
		if len(paths[i].Edges) != len(paths[j].Edges) {
			return len(paths[i].Edges) < len(paths[j].Edges)
		}
		return paths[i].Confidence > paths[j].Confidence
	})
}

// confidenceFromEdges computes a normalised path confidence from
// the per-edge origin tiers. Each edge contributes a 0-1 score
// based on how well-grounded its kind / origin are; the path's
// score is the geometric mean (product) so a single weak edge
// drags the whole chain down — matching the agent intuition that
// a dataflow path is only as strong as its weakest hop.
func confidenceFromEdges(edges []EdgeStep) float64 {
	if len(edges) == 0 {
		return 1
	}
	prod := 1.0
	for _, e := range edges {
		prod *= confidenceFromOrigin(e.Origin, e.Kind)
	}
	return prod
}

func confidenceFromOrigin(origin, kind string) float64 {
	switch origin {
	case graph.OriginLSPResolved:
		return 1
	case graph.OriginLSPDispatch:
		return 0.95
	case graph.OriginASTResolved:
		return 0.9
	case graph.OriginASTInferred:
		return 0.7
	case graph.OriginTextMatched:
		return 0.4
	}
	// Unstamped: fall back to the kind tier. value_flow is intra-
	// procedural and cheap to ground; arg_of / returns_to are
	// cross-call and start lower until the resolver lifts them.
	switch kind {
	case string(graph.EdgeValueFlow):
		return 0.85
	case string(graph.EdgeArgOf), string(graph.EdgeReturnsTo):
		return 0.7
	}
	return 0.5
}

// TaintPattern resolves a source / sink pattern against the live
// graph. Patterns are name-substrings (case-insensitive) by
// default; `exact:Name` switches to exact-name match; `path:dir/`
// scopes to nodes whose FilePath starts with that prefix combined
// with any name fragment after `::`.
type TaintPattern struct {
	Raw       string
	NameMatch func(string) bool
	PathMatch func(string) bool
	KindMatch func(graph.NodeKind) bool
}

// ParsePattern compiles a string into a TaintPattern. Supported
// syntaxes:
//
//   - "Name"            → case-insensitive substring match on node name.
//   - "exact:Name"      → exact name match.
//   - "path:dir/"       → file path prefix match (any name).
//   - "kind:function"   → restrict to a particular node kind.
//   - "name=… kind=…"   → multi-clause AND form (space-separated).
//
// The clauses combine with AND; an empty pattern matches nothing.
func ParsePattern(raw string) TaintPattern {
	p := TaintPattern{Raw: raw}
	if strings.TrimSpace(raw) == "" {
		return p
	}
	clauses := strings.Fields(raw)
	for _, c := range clauses {
		switch {
		case strings.HasPrefix(c, "exact:"):
			want := strings.TrimPrefix(c, "exact:")
			p.NameMatch = chainName(p.NameMatch, func(name string) bool {
				return name == want
			})
		case strings.HasPrefix(c, "name:"):
			want := strings.ToLower(strings.TrimPrefix(c, "name:"))
			p.NameMatch = chainName(p.NameMatch, func(name string) bool {
				return strings.Contains(strings.ToLower(name), want)
			})
		case strings.HasPrefix(c, "name="):
			want := strings.ToLower(strings.TrimPrefix(c, "name="))
			p.NameMatch = chainName(p.NameMatch, func(name string) bool {
				return strings.Contains(strings.ToLower(name), want)
			})
		case strings.HasPrefix(c, "path:"):
			want := strings.TrimPrefix(c, "path:")
			p.PathMatch = chainName(p.PathMatch, func(path string) bool {
				return strings.HasPrefix(path, want)
			})
		case strings.HasPrefix(c, "kind:"):
			wantKind := graph.NodeKind(strings.TrimPrefix(c, "kind:"))
			p.KindMatch = chainKind(p.KindMatch, func(k graph.NodeKind) bool {
				return k == wantKind
			})
		default:
			// Bare token — case-insensitive substring on name.
			want := strings.ToLower(c)
			p.NameMatch = chainName(p.NameMatch, func(name string) bool {
				return strings.Contains(strings.ToLower(name), want)
			})
		}
	}
	return p
}

func chainName(prev, next func(string) bool) func(string) bool {
	if prev == nil {
		return next
	}
	return func(s string) bool { return prev(s) && next(s) }
}

func chainKind(prev, next func(graph.NodeKind) bool) func(graph.NodeKind) bool {
	if prev == nil {
		return next
	}
	return func(k graph.NodeKind) bool { return prev(k) && next(k) }
}

// Empty reports whether the pattern matches nothing.
func (p TaintPattern) Empty() bool {
	return p.Raw == "" || (p.NameMatch == nil && p.PathMatch == nil && p.KindMatch == nil)
}

// matches reports whether n satisfies the compiled clauses. All
// configured matchers must pass; absent matchers are skipped.
func (p TaintPattern) matches(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if p.NameMatch != nil && !p.NameMatch(n.Name) {
		return false
	}
	if p.PathMatch != nil && !p.PathMatch(n.FilePath) {
		return false
	}
	if p.KindMatch != nil && !p.KindMatch(n.Kind) {
		return false
	}
	return true
}

// ResolveCandidates walks the graph and returns up to limit
// distinct symbol IDs whose nodes match the pattern. Returns the
// caller-friendly nodes themselves so MCP responses can include
// names + paths without a second lookup.
func (e *Engine) ResolveCandidates(p TaintPattern, limit int) []*graph.Node {
	if e == nil || e.g == nil || p.Empty() {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	out := make([]*graph.Node, 0, 16)
	for _, n := range e.g.AllNodes() {
		if !taintEligible(n) {
			continue
		}
		if !p.matches(n) {
			continue
		}
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// taintEligible filters the node universe to symbols that could
// plausibly be a dataflow source or sink. Files / imports / pkg
// markers don't carry value semantics, so excluding them up front
// keeps the candidate set focused.
func taintEligible(n *graph.Node) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case graph.KindFunction, graph.KindMethod, graph.KindParam,
		graph.KindField, graph.KindVariable, graph.KindConstant,
		graph.KindType, graph.KindInterface:
		return true
	}
	return false
}

// TaintFinding is one (source, sink) hit produced by TaintPaths.
// Paths is non-empty when at least one BFS path connects the two.
type TaintFinding struct {
	Source *graph.Node `json:"source"`
	Sink   *graph.Node `json:"sink"`
	Paths  []Path      `json:"paths"`
}

// TaintPaths resolves both patterns, then runs flow_between for
// each (source, sink) pair. Returns up to limit findings, sorted
// by best path confidence × shortest length.
//
// Role-aware expansion. Sources and sinks expand differently
// because the dataflow edges are directional. A "source" function
// produces values via its return — flow originates at the
// function node itself, which has incoming returns_to edges
// agents will walk forward from. A "sink" function consumes
// values via its parameters — flow terminates at the param nodes
// where arg_of lands. So when the sink pattern resolves to a
// function/method, we automatically include each declared
// parameter as an additional candidate. This matches the agent
// intuition that `name:DBQuery` for a sink means "every value
// that lands in any argument of DBQuery", not the function
// itself (which has no incoming dataflow).
func (e *Engine) TaintPaths(sourcePattern, sinkPattern TaintPattern, maxDepth, limit int) []TaintFinding {
	return e.TaintPathsWithTier(sourcePattern, sinkPattern, maxDepth, limit, "")
}

// TaintPathsWithTier is TaintPaths with the same per-edge provenance
// filter as FlowBetweenWithTier; empty minTier preserves the legacy
// behavior.
func (e *Engine) TaintPathsWithTier(sourcePattern, sinkPattern TaintPattern, maxDepth, limit int, minTier string) []TaintFinding {
	if e == nil || e.g == nil {
		return nil
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if limit <= 0 {
		limit = 20
	}
	sources := e.ResolveCandidates(sourcePattern, 0)
	rawSinks := e.ResolveCandidates(sinkPattern, 0)
	sinks := e.expandSinkCandidates(rawSinks)
	if len(sources) == 0 || len(sinks) == 0 {
		return nil
	}
	var findings []TaintFinding
	for _, src := range sources {
		for _, sink := range sinks {
			if src.ID == sink.ID {
				continue
			}
			paths := e.FlowBetweenWithTier(src.ID, sink.ID, maxDepth, DefaultMaxPaths, minTier)
			if len(paths) == 0 {
				continue
			}
			findings = append(findings, TaintFinding{
				Source: src,
				Sink:   sink,
				Paths:  paths,
			})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		bi, bj := bestPath(findings[i].Paths), bestPath(findings[j].Paths)
		if bi.Length() != bj.Length() {
			return bi.Length() < bj.Length()
		}
		return bi.Confidence > bj.Confidence
	})
	if len(findings) > limit {
		findings = findings[:limit]
	}
	return findings
}

// expandSinkCandidates appends every parameter node of every
// matched function/method, deduplicated by ID. Originals stay in
// the candidate set — sometimes a flow really does land on the
// function symbol itself (e.g., when a callee value is passed
// straight back through another return) and excluding it would
// hide that case.
func (e *Engine) expandSinkCandidates(raw []*graph.Node) []*graph.Node {
	if len(raw) == 0 || e == nil || e.g == nil {
		return raw
	}
	seen := make(map[string]struct{}, len(raw)*2)
	out := make([]*graph.Node, 0, len(raw)*2)
	add := func(n *graph.Node) {
		if n == nil {
			return
		}
		if _, ok := seen[n.ID]; ok {
			return
		}
		seen[n.ID] = struct{}{}
		out = append(out, n)
	}
	for _, n := range raw {
		add(n)
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		for _, edge := range e.g.GetInEdges(n.ID) {
			if edge.Kind != graph.EdgeParamOf {
				continue
			}
			pNode := e.g.GetNode(edge.From)
			if pNode == nil || pNode.Kind != graph.KindParam {
				continue
			}
			add(pNode)
		}
	}
	return out
}

// bestPath returns the lowest-cost path in a finding (assumed
// already rankPaths-sorted). Falls back to a zero-value when the
// finding has none.
func bestPath(paths []Path) Path {
	if len(paths) == 0 {
		return Path{}
	}
	return paths[0]
}
