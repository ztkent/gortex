package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// registerWakeupTool wires gortex_wakeup — a ~500-token markdown
// codebase digest assembled from the same substrate get_repo_outline
// already exposes (language mix, top communities, hotspots, entry
// points), formatted as paste-ready markdown for users who *can't*
// run MCP at all (web ChatGPT, hosted Codex, raw API).
//
// Same builder also feeds the `gortex wakeup` CLI subcommand so the
// MCP and CLI outputs stay byte-identical.
func (s *Server) registerWakeupTool() {
	s.addTool(
		mcp.NewTool("gortex_wakeup",
			mcp.WithDescription("Paste-ready ~500-token codebase digest. Composes language mix + top communities + load-bearing hotspots + entry points into a single markdown blob the agent can paste into a chat session at startup. Targets users without an MCP transport (web ChatGPT, hosted Codex, raw API). Token cap is approximate — under 600 in typical use."),
			mcp.WithNumber("max_tokens", mcp.Description("Approximate output cap (default: 500). Bytes-per-token heuristic is 4; we trim to that budget after rendering.")),
			mcp.WithNumber("top_communities", mcp.Description("Communities to include (default: 4).")),
			mcp.WithNumber("top_hotspots", mcp.Description("Hotspots to include (default: 5).")),
			mcp.WithNumber("top_entry_points", mcp.Description("Entry points to include (default: 5).")),
			mcp.WithString("format", mcp.Description("Output format: markdown (default — primary use case) or json. JSON wraps the markdown in a {markdown, tokens_est, sections} envelope for callers that want to introspect.")),
		),
		s.handleGortexWakeup,
	)
}

// WakeupOptions controls BuildWakeup output. Exposed so the
// `gortex wakeup` CLI subcommand can reuse the identical renderer
// without duplicating defaults.
type WakeupOptions struct {
	MaxTokens      int
	TopCommunities int
	TopHotspots    int
	TopEntryPoints int
}

// DefaultWakeupOptions returns the defaults the MCP handler uses.
// Pulled out so the CLI subcommand renders the same output.
func DefaultWakeupOptions() WakeupOptions {
	return WakeupOptions{
		MaxTokens:      500,
		TopCommunities: 4,
		TopHotspots:    5,
		TopEntryPoints: 5,
	}
}

// BuildWakeup renders the wakeup digest from a graph + cached
// communities. Returns the markdown body and an approximate token
// count (bytes / 4). Exposed so CLI and MCP paths share one
// implementation.
func BuildWakeup(g graph.Store, communities *analysis.CommunityResult, opts WakeupOptions) (markdown string, tokensEst int) {
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 500
	}
	if opts.TopCommunities <= 0 {
		opts.TopCommunities = 4
	}
	if opts.TopHotspots <= 0 {
		opts.TopHotspots = 5
	}
	if opts.TopEntryPoints <= 0 {
		opts.TopEntryPoints = 5
	}

	nodes := g.AllNodes()
	var b strings.Builder
	b.WriteString("# Codebase wakeup\n\n")

	// Summary line: total nodes, top 3 languages.
	langCounts := map[string]int{}
	for _, n := range nodes {
		if n.Language != "" {
			langCounts[n.Language]++
		}
	}
	type langRow struct {
		name  string
		count int
	}
	langs := make([]langRow, 0, len(langCounts))
	for k, v := range langCounts {
		langs = append(langs, langRow{k, v})
	}
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].count != langs[j].count {
			return langs[i].count > langs[j].count
		}
		return langs[i].name < langs[j].name
	})
	topLangs := langs
	if len(topLangs) > 3 {
		topLangs = topLangs[:3]
	}
	langSummary := []string{}
	for _, l := range topLangs {
		langSummary = append(langSummary, fmt.Sprintf("%s (%d)", l.name, l.count))
	}
	fmt.Fprintf(&b, "**Scale.** %d indexed symbols across %d files. Primary: %s.\n\n",
		len(nodes), countFileNodes(nodes), strings.Join(langSummary, ", "))

	// Communities.
	if communities != nil && len(communities.Communities) > 0 {
		comms := append([]analysis.Community(nil), communities.Communities...)
		sort.Slice(comms, func(i, j int) bool { return comms[i].Size > comms[j].Size })
		if len(comms) > opts.TopCommunities {
			comms = comms[:opts.TopCommunities]
		}
		b.WriteString("**Communities.**\n")
		for _, c := range comms {
			label := c.Label
			if label == "" {
				label = c.ID
			}
			hub := ""
			if c.Hub != "" {
				hub = " · hub " + c.Hub
			}
			fmt.Fprintf(&b, "- %s (%d members%s)\n", label, c.Size, hub)
		}
		b.WriteString("\n")
	}

	// Hotspots.
	hotspots := analysis.FindHotspots(g, communities, 0)
	if len(hotspots) > opts.TopHotspots {
		hotspots = hotspots[:opts.TopHotspots]
	}
	if len(hotspots) > 0 {
		b.WriteString("**Load-bearing symbols.**\n")
		for _, h := range hotspots {
			fmt.Fprintf(&b, "- `%s` (in:%d, out:%d) — %s\n", h.Name, h.FanIn, h.FanOut, h.FilePath)
		}
		b.WriteString("\n")
	}

	// Entry points.
	entries := wakeupEntryPoints(nodes, g, opts.TopEntryPoints)
	if len(entries) > 0 {
		b.WriteString("**Entry points.**\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "- `%s` — %s\n", e.Name, e.FilePath)
		}
		b.WriteString("\n")
	}

	out := b.String()
	out = trimToTokens(out, opts.MaxTokens)
	return out, len(out) / 4
}

func countFileNodes(nodes []*graph.Node) int {
	n := 0
	for _, x := range nodes {
		if x.Kind == graph.KindFile {
			n++
		}
	}
	return n
}

func wakeupEntryPoints(nodes []*graph.Node, g graph.Store, top int) []*graph.Node {
	candidates := make([]*graph.Node, 0)
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if len(g.GetInEdges(n.ID)) > 0 {
			continue
		}
		if len(g.GetOutEdges(n.ID)) == 0 {
			continue
		}
		candidates = append(candidates, n)
	}
	sort.Slice(candidates, func(i, j int) bool {
		oi := len(g.GetOutEdges(candidates[i].ID))
		oj := len(g.GetOutEdges(candidates[j].ID))
		if oi != oj {
			return oi > oj
		}
		return candidates[i].ID < candidates[j].ID
	})
	if len(candidates) > top {
		candidates = candidates[:top]
	}
	return candidates
}

// trimToTokens caps the markdown to the requested approximate token
// budget. Heuristic: 4 bytes per token. Trims at a line boundary so
// the cut is visually clean.
func trimToTokens(s string, maxTokens int) string {
	limitBytes := maxTokens * 4
	if len(s) <= limitBytes {
		return s
	}
	cut := s[:limitBytes]
	if idx := strings.LastIndex(cut, "\n"); idx > limitBytes/2 {
		cut = cut[:idx]
	}
	return cut + "\n\n_… digest truncated to fit token budget …_\n"
}

func (s *Server) handleGortexWakeup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	opts := DefaultWakeupOptions()
	if v := req.GetInt("max_tokens", 0); v > 0 {
		opts.MaxTokens = v
	}
	if v := req.GetInt("top_communities", 0); v > 0 {
		opts.TopCommunities = v
	}
	if v := req.GetInt("top_hotspots", 0); v > 0 {
		opts.TopHotspots = v
	}
	if v := req.GetInt("top_entry_points", 0); v > 0 {
		opts.TopEntryPoints = v
	}

	md, est := BuildWakeup(s.graph, s.getCommunities(), opts)

	format := strings.ToLower(strings.TrimSpace(req.GetString("format", "markdown")))
	if format == "markdown" || format == "" {
		return mcp.NewToolResultText(md), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"markdown":   md,
		"tokens_est": est,
	})
}
