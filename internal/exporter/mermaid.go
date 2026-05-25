package exporter

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// MermaidOpts narrows what the Mermaid exporter renders.
type MermaidOpts struct {
	// Scope picks the diagram flavour: "architecture" (default),
	// "communities", "processes", or "all" (writes all three when
	// the caller is feeding WriteMermaidScoped via the multi-file
	// directory mode).
	Scope string
	// MaxCommunities caps how many communities to include in the
	// architecture and communities diagrams. Zero means 20.
	MaxCommunities int
	// MinCommunity drops communities below this size. Zero means 3.
	MinCommunity int
	// Kinds and Languages mirror the standard exporter Options for
	// filtering the underlying graph before community detection.
	Kinds     []graph.NodeKind
	Languages []string
}

func (o MermaidOpts) withDefaults() MermaidOpts {
	if o.Scope == "" {
		o.Scope = "architecture"
	}
	if o.MaxCommunities == 0 {
		o.MaxCommunities = 20
	}
	if o.MinCommunity == 0 {
		o.MinCommunity = 3
	}
	return o
}

// WriteMermaid emits a single Mermaid diagram for the chosen scope.
// Use this when the caller asks for one file. For multi-file output
// the CLI calls WriteMermaid once per scope into separate files.
func WriteMermaid(w io.Writer, g graph.Store, opts MermaidOpts) (Stats, error) {
	opts = opts.withDefaults()
	cw := &countingWriter{w: w}

	if g == nil {
		_, _ = io.WriteString(cw, "graph LR\n  empty[\"no graph\"]\n")
		return Stats{BytesWritten: cw.n}, nil
	}

	body, nodes, edges, err := renderForScope(g, opts)
	if err != nil {
		return Stats{}, err
	}
	if _, err := io.WriteString(cw, body); err != nil {
		return Stats{}, err
	}
	return Stats{NodesWritten: nodes, EdgesWritten: edges, BytesWritten: cw.n}, nil
}

// renderForScope dispatches the Scope to the right diagram builder and
// returns the rendered Mermaid plus a (nodes, edges) count that the
// caller surfaces in Stats.
func renderForScope(g graph.Store, opts MermaidOpts) (body string, nodes, edges int, err error) {
	switch strings.ToLower(opts.Scope) {
	case "architecture":
		body, nodes, edges = renderArchitecture(g, opts)
	case "communities":
		body, nodes, edges = renderCommunities(g, opts)
	case "processes":
		body, nodes, edges = renderProcesses(g, opts)
	case "all":
		// Emit a single document containing all three diagrams in
		// sequence, separated by `%% ---` markers. Useful for
		// quick preview without enabling --out-dir.
		var sb strings.Builder
		arch, an, ae := renderArchitecture(g, opts)
		comm, cn, ce := renderCommunities(g, opts)
		proc, pn, pe := renderProcesses(g, opts)
		sb.WriteString("%% gortex architecture diagram\n")
		sb.WriteString(arch)
		sb.WriteString("\n%% ---\n")
		sb.WriteString("%% gortex communities diagram\n")
		sb.WriteString(comm)
		sb.WriteString("\n%% ---\n")
		sb.WriteString("%% gortex processes diagram\n")
		sb.WriteString(proc)
		body = sb.String()
		nodes = an + cn + pn
		edges = ae + ce + pe
	default:
		return "", 0, 0, fmt.Errorf("unknown scope %q (expected architecture | communities | processes | all)", opts.Scope)
	}
	return body, nodes, edges, nil
}

// renderArchitecture builds a top-level community map with hub
// annotations. Mirrors the layout used by the wiki page.
func renderArchitecture(g graph.Store, opts MermaidOpts) (string, int, int) {
	comms := analysis.DetectCommunities(g)
	var sb strings.Builder
	sb.WriteString("graph TB\n")
	if comms == nil || len(comms.Communities) == 0 {
		sb.WriteString("  empty[\"no communities\"]\n")
		return sb.String(), 1, 0
	}
	type item struct {
		id    string
		label string
		size  int
		hub   string
	}
	var kept []item
	for _, c := range comms.Communities {
		if c.Size < opts.MinCommunity {
			continue
		}
		label := c.Label
		if label == "" {
			label = c.ID
		}
		kept = append(kept, item{id: c.ID, label: label, size: c.Size, hub: c.Hub})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].size > kept[j].size })
	if opts.MaxCommunities > 0 && len(kept) > opts.MaxCommunities {
		kept = kept[:opts.MaxCommunities]
	}
	keepSet := make(map[string]bool, len(kept))
	for _, k := range kept {
		keepSet[k.id] = true
		hub := k.hub
		if hub == "" {
			hub = "—"
		}
		label := fmt.Sprintf("%s\\n%d symbols\\nhub: %s", k.label, k.size, hub)
		fmt.Fprintf(&sb, "  %s[\"%s\"]\n", mermaidSafeID(k.id), mermaidEscape(label))
	}
	sb.WriteString("\n")
	edges := emitCrossCommEdges(&sb, g, comms, keepSet)
	return sb.String(), len(kept), edges
}

// renderCommunities is identical to architecture today but exposes
// `graph LR` for a wider canvas. Caller picks via Scope.
func renderCommunities(g graph.Store, opts MermaidOpts) (string, int, int) {
	comms := analysis.DetectCommunities(g)
	var sb strings.Builder
	sb.WriteString("graph LR\n")
	if comms == nil || len(comms.Communities) == 0 {
		sb.WriteString("  empty[\"no communities\"]\n")
		return sb.String(), 1, 0
	}
	type item struct {
		id    string
		label string
		size  int
	}
	var kept []item
	for _, c := range comms.Communities {
		if c.Size < opts.MinCommunity {
			continue
		}
		label := c.Label
		if label == "" {
			label = c.ID
		}
		kept = append(kept, item{id: c.ID, label: label, size: c.Size})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].size > kept[j].size })
	if opts.MaxCommunities > 0 && len(kept) > opts.MaxCommunities {
		kept = kept[:opts.MaxCommunities]
	}
	keepSet := make(map[string]bool, len(kept))
	for _, k := range kept {
		keepSet[k.id] = true
		fmt.Fprintf(&sb, "  %s([\"%s\\n%d\"])\n", mermaidSafeID(k.id), mermaidEscape(k.label), k.size)
	}
	sb.WriteString("\n")
	edges := emitCrossCommEdges(&sb, g, comms, keepSet)
	return sb.String(), len(kept), edges
}

// renderProcesses lists every process as a small flowchart of
// caller→callee pairs, capped to keep the rendering responsive.
func renderProcesses(g graph.Store, _ MermaidOpts) (string, int, int) {
	procs := analysis.DiscoverProcesses(g)
	var sb strings.Builder
	sb.WriteString("graph LR\n")
	if procs == nil || len(procs.Processes) == 0 {
		sb.WriteString("  empty[\"no processes\"]\n")
		return sb.String(), 1, 0
	}
	maxProcesses := 12
	if len(procs.Processes) < maxProcesses {
		maxProcesses = len(procs.Processes)
	}
	nodeCount, edgeCount := 0, 0
	nodeByID := make(map[string]*graph.Node)
	for _, n := range g.AllNodes() {
		nodeByID[n.ID] = n
	}
	for i := 0; i < maxProcesses; i++ {
		p := procs.Processes[i]
		fmt.Fprintf(&sb, "  subgraph %s [%s]\n", mermaidSafeID("p_"+p.ID), mermaidEscape(p.Name))
		// Walk steps in order and emit caller→callee using the
		// nearest preceding step with smaller depth.
		parentStack := make([]int, 0, len(p.Steps))
		for j, step := range p.Steps {
			for len(parentStack) > 0 {
				top := parentStack[len(parentStack)-1]
				if p.Steps[top].Depth < step.Depth {
					break
				}
				parentStack = parentStack[:len(parentStack)-1]
			}
			parent := -1
			if len(parentStack) > 0 {
				parent = parentStack[len(parentStack)-1]
			}
			parentStack = append(parentStack, j)

			name := step.ID
			if n := nodeByID[step.ID]; n != nil && n.Name != "" {
				name = n.Name
			}
			id := mermaidSafeID(step.ID + "_" + p.ID)
			fmt.Fprintf(&sb, "    %s[\"%s\"]\n", id, mermaidEscape(name))
			nodeCount++
			if parent >= 0 {
				parentID := mermaidSafeID(p.Steps[parent].ID + "_" + p.ID)
				fmt.Fprintf(&sb, "    %s --> %s\n", parentID, id)
				edgeCount++
			}
		}
		sb.WriteString("  end\n")
	}
	return sb.String(), nodeCount, edgeCount
}

// emitCrossCommEdges writes EdgeCalls between communities (filtered
// to the kept set) and returns the edge count.
func emitCrossCommEdges(sb *strings.Builder, g graph.Store, comms *analysis.CommunityResult, keep map[string]bool) int {
	type edge struct {
		from, to string
		count    int
	}
	edges := make(map[string]*edge)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		from := comms.NodeToComm[e.From]
		to := comms.NodeToComm[e.To]
		if from == "" || to == "" || from == to {
			continue
		}
		if !keep[from] || !keep[to] {
			continue
		}
		k := from + "→" + to
		if x, ok := edges[k]; ok {
			x.count++
		} else {
			edges[k] = &edge{from: from, to: to, count: 1}
		}
	}
	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ed := edges[k]
		fmt.Fprintf(sb, "  %s -->|%d| %s\n", mermaidSafeID(ed.from), ed.count, mermaidSafeID(ed.to))
	}
	return len(edges)
}

// mermaidSafeID — mirror of the one in internal/wiki/mermaid.go. Kept
// duplicated so the exporter package has no cross-coupling to wiki.
func mermaidSafeID(id string) string {
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
		"*", "_",
		"#", "_",
		"@", "_",
		":", "_",
	)
	return r.Replace(id)
}

func mermaidEscape(s string) string {
	return strings.ReplaceAll(s, `"`, `#quot;`)
}
