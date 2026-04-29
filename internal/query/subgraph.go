package query

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SubGraph is a JSON-serializable result from a graph query.
type SubGraph struct {
	Nodes      []*graph.Node `json:"nodes"`
	Edges      []*graph.Edge `json:"edges"`
	TotalNodes int           `json:"total_nodes"`
	TotalEdges int           `json:"total_edges"`
	Truncated  bool          `json:"truncated"`
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
}

// scopeAllows reports whether a node passes the workspace/project
// scope expressed in opts. Empty WorkspaceID means "no scope" — every
// node passes. Same effective-fallback rule as the matcher: missing
// WorkspaceID on the node falls back to its RepoPrefix.
func (o QueryOptions) scopeAllows(n *graph.Node) bool {
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
