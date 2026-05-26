package query

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// SubGraph is a JSON-serializable result from a graph query.
type SubGraph struct {
	Nodes      []*graph.Node `json:"nodes"`
	Edges      []*graph.Edge `json:"edges"`
	TotalNodes int           `json:"total_nodes"`
	TotalEdges int           `json:"total_edges"`
	Truncated  bool          `json:"truncated"`
	// Caveat is attached only when an edge-returning query (find_usages,
	// get_callers) comes back with no edges, classifying whether the
	// empty result reflects genuinely unused code or an extraction gap.
	// Nil — and omitted from the response — for any non-empty result.
	Caveat *graph.ZeroEdgeCaveat `json:"caveat,omitempty"`
	// CallerNotes carries concurrency-safety annotations keyed by node
	// ID. Populated only by get_callers (which classifies each caller);
	// other traversal tools share this struct and leave it nil, so it
	// is omitted from their responses. A node appears here only when at
	// least one concurrency flag is set, so an absent entry means
	// "neither sync_guarded nor cross_concurrent".
	CallerNotes map[string]*graph.ConcurrencyAnnotation `json:"caller_notes,omitempty"`
	// BudgetHit is set by token-budgeted traversals (WalkBudgeted) when
	// the walk stopped because the estimated encoded size of the result
	// reached the caller's token budget. False — and omitted — for a
	// traversal that completed within budget or never imposed one.
	BudgetHit bool `json:"budget_hit,omitempty"`
	// StoppedAtDepth records the BFS depth the budgeted traversal had
	// reached when it stopped — either the deepest depth fully expanded,
	// or the depth at which the budget / depth cap halted expansion.
	// Zero — and omitted — for traversals that don't track depth.
	StoppedAtDepth int `json:"stopped_at_depth,omitempty"`
}

// QueryOptions controls traversal depth, result limits, and detail level.
type QueryOptions struct {
	Depth   int    `json:"depth"`
	Limit   int    `json:"limit"`
	Detail  string `json:"detail"`             // "brief" or "full"
	MinTier string `json:"min_tier,omitempty"` // see graph.Origin* constants; "" = no filter
	// WorkspaceID, when set, restricts traversal to nodes whose
	// effective workspace (Node.WorkspaceID || Node.RepoPrefix
	// fallback) equals this slug. Empty disables the filter —
	// preserves the legacy global-graph behaviour for callers that
	// don't care about the workspace boundary.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID applies the same scoping for the soft sub-boundary.
	// Honoured only when WorkspaceID is also set; on its own it would
	// be ambiguous (two workspaces could declare a project with the
	// same name).
	ProjectID string `json:"project_id,omitempty"`
	// ExcludeTests, when true, drops edges originating from a function
	// flagged as a test (Node.Meta["is_test"] = true) — set by the
	// indexer's test-edge pass. Lets find_usages / get_callers answer
	// "who depends on X *in production*" without test-noise dilution.
	ExcludeTests bool `json:"exclude_tests,omitempty"`

	// SearchTimings, when non-nil, is populated by the search hot path
	// (SearchSymbolsScoped → gatherBackendCandidates) with per-phase
	// wall-clock breakdowns. Used by the MCP search_symbols handler's
	// debug log line; nil disables instrumentation. Single-call: the
	// caller MUST hand a fresh struct per query (the engine does not
	// reset). Never serialised — `json:"-"` keeps the option struct
	// JSON shape stable.
	SearchTimings *SearchTimings `json:"-"`

	// RerankContext is the optional rerank context the engine uses when
	// gathering bundle candidates: each bundle's in/out edges are
	// seeded into the context's edge caches so the handler-side
	// rerank.Pipeline.Rerank can skip its own batched edge fetch on
	// the merged candidate set. Pass nil — the engine's gather path
	// still works, the bundle's edges are just discarded after the
	// per-call rerank. Never serialised.
	RerankContext *rerank.Context `json:"-"`

	// SkipInnerRerank, when true, makes SearchSymbolsRanked skip its
	// own per-call rerank.Pipeline.Rerank pass. Callers that fan a
	// search across N expansion terms and merge the results themselves
	// (the MCP search_symbols handler) re-run the rerank once on the
	// merged candidate set with the full session-aware context — the
	// inner per-call rerank is wasted work whose output is mostly
	// discarded by the merge. Flipping this on collapses N+1
	// engine-side rerank invocations to zero. The merge-side rerank
	// is the source of truth either way.
	SkipInnerRerank bool `json:"-"`

	// SkipVectorChannel, when true, makes gatherBackendCandidates skip
	// the vector channel entirely — no embedder call, no ANN search.
	// Set by the MCP search_symbols handler on identifier-shape queries
	// (QueryClassSymbol / QueryClassPath / QueryClassSignature) where
	// the rerank's classWeightTable already proves the semantic
	// channel contributes near-zero useful signal (multipliers 0.65 /
	// 0.45 / 0.80 vs the baseline 1.00 for concept). Saves the embed
	// + vector search round-trip on the common-case identifier lookup.
	// The bundle path's vector-only branch and the legacy
	// SearchChannels path both honour this flag.
	SkipVectorChannel bool `json:"-"`

	// SkipExactNameSplice, when true, makes gatherBackendCandidates
	// skip the FindNodesByName(query) splice-in. Set by callers that
	// know the query string cannot match any exact node name — the
	// fetchAndMergeBM25 fan-out's combined-OR call is the canonical
	// case: a concatenated bag of expansion terms ("NewServer
	// StartServer Server.Init …") can't be the literal Name of any
	// node, so the FindNodesByName Cypher round-trip is wasted work.
	// The primary query still runs the splice.
	SkipExactNameSplice bool `json:"-"`
}

// SearchTimings carries per-phase wall-clock measurements collected
// by the BM25 retrieval pipeline. Zero-valued fields mean the phase
// didn't run on this call (e.g. FallbackMS is 0 when the BM25 result
// already saturated the limit).
type SearchTimings struct {
	BM25PrimaryMS   int64 // time spent in the primary BM25 backend call
	BM25ExpansionMS int64 // time spent across all expansion-term BM25 calls
	GetNodesMS      int64 // time spent materialising BM25/vector IDs via GetNodesByIDs
	FindNameMS      int64 // time spent on the FindNodesByName splice-in
	FallbackMS      int64 // time spent in the substring/name-contains fallback
	// Sub-buckets of the BM25*MS totals — proves which phase inside
	// the wrapper is actually slow. Accumulated across every
	// primary + expansion BM25 invocation.
	TextBackendMS  int64 // strictly inside Backend.Search / text channel
	EmbedMS        int64 // inside embedder.Embed (vector path only)
	VectorSearchMS int64 // inside vector.Search ANN call (vector path only)
	EngineRerankMS int64 // inside rerank.Pipeline.Rerank in SearchSymbolsRanked
	// BundleMS accumulates the wall-clock spent inside
	// SymbolBundleSearcherBackend.SearchSymbolBundles (one Cypher per
	// BM25 fan-out that returns Node + in/out edges in one bundle).
	// When the backend supports bundles, the bundle path replaces the
	// (TextBackend + GetNodes) sub-buckets; the bm25_backend_ms
	// derivation in the handler subtracts BundleMS so the existing
	// fields stay meaningful.
	BundleMS int64
	// CacheHitRate is the fraction of post-merge candidates whose
	// in/out edges were already in the rerank Context cache when the
	// handler-side prepare() ran. 1.0 means every candidate was
	// pre-seeded from a bundle; 0.0 means the rerank had to fetch
	// every candidate's edges itself. Populated by the handler when
	// the bundle path is active so the search_symbols debug log can
	// surface how often the seeding actually catches.
	CacheHitRate float64
}

// ScopeAllows reports whether a node passes the workspace/project
// scope expressed in opts. Empty WorkspaceID means "no scope" — every
// node passes. Same effective-fallback rule as the matcher: missing
// WorkspaceID on the node falls back to its RepoPrefix.
//
// Exported so the MCP layer can enforce the session's workspace
// boundary on by-id and whole-graph handlers that don't route through
// the engine's scoped traversal.
func (o QueryOptions) ScopeAllows(n *graph.Node) bool {
	if o.WorkspaceID == "" || n == nil {
		return true
	}
	ws := n.WorkspaceID
	if ws == "" {
		ws = n.RepoPrefix
	}
	if ws != o.WorkspaceID {
		return false
	}
	if o.ProjectID == "" {
		return true
	}
	proj := n.ProjectID
	if proj == "" {
		proj = n.RepoPrefix
	}
	return proj == o.ProjectID
}

// FilterByMinTier drops edges whose Origin rank is below minTier.
//
// Nodes are left untouched — a hop that gets filtered can leave an
// unreachable node in Nodes. That's acceptable for the current surface
// area (agents filter by tier mainly for one-hop questions like "who
// calls this?"), and pruning orphans would silently change the node set
// when a caller might still want to see them. Callers that care can
// post-prune themselves.
//
// Edges without Origin set fall back to graph.DefaultOriginFor (derived
// from kind + confidence + semantic_source meta) so filters work on
// edges produced before this field existed or by providers not yet
// updated.
func (sg *SubGraph) FilterByMinTier(minTier string) {
	if minTier == "" || sg == nil {
		return
	}
	kept := make([]*graph.Edge, 0, len(sg.Edges))
	for _, e := range sg.Edges {
		origin := e.Origin
		if origin == "" {
			src, _ := e.Meta["semantic_source"].(string)
			origin = graph.DefaultOriginFor(e.Kind, e.Confidence, src)
		}
		if graph.MeetsMinTier(origin, minTier) {
			kept = append(kept, e)
		}
	}
	sg.Edges = kept
}

// ToDot returns a Graphviz DOT representation of the subgraph.
func (sg *SubGraph) ToDot() string {
	var b strings.Builder
	b.WriteString("digraph gortex {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [fontname=\"monospace\" fontsize=10];\n")
	b.WriteString("  edge [fontname=\"monospace\" fontsize=8];\n\n")

	kindColors := map[graph.NodeKind]string{
		graph.KindFile:      "#607D8B",
		graph.KindPackage:   "#bb9af7",
		graph.KindFunction:  "#7aa2f7",
		graph.KindMethod:    "#7dcfff",
		graph.KindType:      "#9ece6a",
		graph.KindInterface: "#73daca",
		graph.KindVariable:  "#ff9e64",
		graph.KindImport:    "#795548",
	}

	kindShapes := map[graph.NodeKind]string{
		graph.KindFile:      "folder",
		graph.KindFunction:  "ellipse",
		graph.KindMethod:    "ellipse",
		graph.KindType:      "box",
		graph.KindInterface: "box",
		graph.KindVariable:  "triangle",
		graph.KindImport:    "note",
		graph.KindPackage:   "diamond",
	}

	for _, n := range sg.Nodes {
		color := kindColors[n.Kind]
		if color == "" {
			color = "#565f89"
		}
		shape := kindShapes[n.Kind]
		if shape == "" {
			shape = "ellipse"
		}
		label := fmt.Sprintf("%s\\n%s", n.Name, n.Kind)
		fmt.Fprintf(&b, "  %q [label=%q shape=%s style=filled fillcolor=%q fontcolor=white];\n",
			n.ID, label, shape, color)
	}

	b.WriteString("\n")

	edgeColors := map[graph.EdgeKind]string{
		graph.EdgeCalls:        "#7aa2f7",
		graph.EdgeImports:      "#565f89",
		graph.EdgeDefines:      "#414868",
		graph.EdgeImplements:   "#9ece6a",
		graph.EdgeExtends:      "#bb9af7",
		graph.EdgeOverrides:    "#f7768e",
		graph.EdgeReferences:   "#3b4261",
		graph.EdgeMemberOf:     "#3b4261",
		graph.EdgeInstantiates: "#e0af68",
	}

	for _, e := range sg.Edges {
		color := edgeColors[e.Kind]
		if color == "" {
			color = "#3b4261"
		}
		fmt.Fprintf(&b, "  %q -> %q [label=%q color=%q];\n",
			e.From, e.To, e.Kind, color)
	}

	b.WriteString("}\n")
	return b.String()
}

// ToMermaid returns a Mermaid flowchart representation of the subgraph.
// Renders in GitHub, Notion, and most markdown viewers.
func (sg *SubGraph) ToMermaid() string {
	var b strings.Builder
	b.WriteString("graph LR\n")

	// Mermaid node shapes by kind.
	// [text] = rectangle, ([text]) = rounded, ((text)) = circle,
	// {text} = diamond, >text] = flag, [(text)] = stadium
	for _, n := range sg.Nodes {
		safeID := mermaidID(n.ID)
		label := fmt.Sprintf("%s\n%s", n.Name, n.Kind)

		switch n.Kind {
		case graph.KindFile:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindFunction, graph.KindMethod:
			fmt.Fprintf(&b, "  %s([\"%s\"])\n", safeID, mermaidEscape(label))
		case graph.KindType, graph.KindInterface:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindVariable:
			fmt.Fprintf(&b, "  %s>\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindPackage:
			fmt.Fprintf(&b, "  %s{\"%s\"}\n", safeID, mermaidEscape(label))
		default:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		}
	}

	b.WriteString("\n")

	// Mermaid edge styles by kind.
	edgeStyles := map[graph.EdgeKind]string{
		graph.EdgeCalls:        "-->",
		graph.EdgeImports:      "-.->",
		graph.EdgeDefines:      "-->",
		graph.EdgeImplements:   "-. implements .->",
		graph.EdgeExtends:      "-. extends .->",
		graph.EdgeOverrides:    "-. overrides .->",
		graph.EdgeReferences:   "-->",
		graph.EdgeMemberOf:     "-->",
		graph.EdgeInstantiates: "-. new .->",
	}

	for _, e := range sg.Edges {
		style := edgeStyles[e.Kind]
		if style == "" {
			style = "-->"
		}
		fromID := mermaidID(e.From)
		toID := mermaidID(e.To)

		// For simple arrow styles, add the edge kind as label.
		if style == "-->" || style == "-.->" {
			fmt.Fprintf(&b, "  %s %s|%s| %s\n", fromID, style, e.Kind, toID)
		} else {
			fmt.Fprintf(&b, "  %s %s %s\n", fromID, style, toID)
		}
	}

	// Style classes for node coloring.
	b.WriteString("\n")
	kindCSS := map[graph.NodeKind]string{
		graph.KindFile:      "fill:#607D8B,color:#fff",
		graph.KindPackage:   "fill:#bb9af7,color:#fff",
		graph.KindFunction:  "fill:#7aa2f7,color:#fff",
		graph.KindMethod:    "fill:#7dcfff,color:#fff",
		graph.KindType:      "fill:#9ece6a,color:#fff",
		graph.KindInterface: "fill:#73daca,color:#fff",
		graph.KindVariable:  "fill:#ff9e64,color:#fff",
		graph.KindImport:    "fill:#795548,color:#fff",
	}

	// Group nodes by kind for class assignment.
	byKind := make(map[graph.NodeKind][]string)
	for _, n := range sg.Nodes {
		byKind[n.Kind] = append(byKind[n.Kind], mermaidID(n.ID))
	}
	for kind, ids := range byKind {
		css := kindCSS[kind]
		if css == "" {
			continue
		}
		fmt.Fprintf(&b, "  classDef %s %s\n", kind, css)
		fmt.Fprintf(&b, "  class %s %s\n", strings.Join(ids, ","), kind)
	}

	return b.String()
}

// mermaidID converts a node ID to a Mermaid-safe identifier.
// Mermaid IDs can't contain ::, /, or dots.
func mermaidID(id string) string {
	r := strings.NewReplacer(
		"::", "_",
		"/", "_",
		".", "_",
		"-", "_",
		" ", "_",
		"<", "_",
		">", "_",
		"(", "_",
		")", "_",
	)
	return r.Replace(id)
}

// mermaidEscape escapes characters that break Mermaid labels.
func mermaidEscape(s string) string {
	s = strings.ReplaceAll(s, "\"", "#quot;")
	return s
}

// DefaultOptions returns options with sensible defaults.
func DefaultOptions() QueryOptions {
	return QueryOptions{
		Depth:  3,
		Limit:  50,
		Detail: "brief",
	}
}

// WalkOptions controls a token-budgeted free-form graph traversal
// (Engine.WalkBudgeted). It is deliberately a separate struct from
// QueryOptions: a budgeted walk stops on an encoded-size estimate
// rather than a node count, and lets the caller pick an arbitrary set
// of edge kinds and a traversal direction — neither of which the
// fixed-purpose QueryOptions traversals expose.
type WalkOptions struct {
	// EdgeKinds is the set of edge kinds the walk follows. An empty
	// slice means "every edge kind" and, combined with Direction
	// "both", reproduces an undirected neighbourhood walk.
	EdgeKinds []graph.EdgeKind
	// Direction is "out" (follow outgoing edges — the default when
	// empty), "in" (follow incoming edges), or "both" (undirected).
	Direction string
	// TokenBudget is the approximate token ceiling for the encoded
	// result. The walk stops appending nodes once the running estimate
	// would exceed it. A non-positive value disables the budget.
	TokenBudget int
	// MaxDepth is a hard safety cap on BFS depth, applied even when the
	// token budget would allow deeper expansion. A non-positive value
	// falls back to a built-in default.
	MaxDepth int
	// WorkspaceID / ProjectID scope the traversal exactly as the
	// matching QueryOptions fields do — neighbours outside the scope
	// are dropped along with the edge that reached them.
	WorkspaceID string
	ProjectID   string
}

// scopeAllows reports whether n passes this walk's workspace/project
// scope. Mirrors QueryOptions.ScopeAllows so budgeted walks enforce the
// same boundary without duplicating the fallback rules.
func (o WalkOptions) scopeAllows(n *graph.Node) bool {
	return QueryOptions{WorkspaceID: o.WorkspaceID, ProjectID: o.ProjectID}.ScopeAllows(n)
}
