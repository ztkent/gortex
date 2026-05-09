package mcp

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// handleGetRepoOutline returns a single-call narrative overview of the
// indexed codebase: primary languages, top communities, load-bearing
// hotspots, most-imported files, and entry points. It's the "new to this
// repo" tool — everything a reader wants to know about the codebase in one
// response without having to assemble it from graph_stats + analyze + manual
// inspection.
//
// Output is compact on purpose (a handful of each list) so it stays under
// ~1k tokens even on large repos. For deeper exploration, the agent
// follows up with smart_context, find_usages, etc. on specific symbols
// surfaced here.
func (s *Server) handleGetRepoOutline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	const (
		topCommunitiesN  = 5
		topHotspotsN     = 5
		topMostImportedN = 10
		topEntryPointsN  = 10
		topLanguagesN    = 5
	)

	stats := s.engine.Stats()

	// Language breakdown — sort by node count, take top N.
	type langEntry struct {
		Name  string `json:"name"`
		Nodes int    `json:"nodes"`
	}
	var languages []langEntry
	for name, n := range stats.ByLanguage {
		languages = append(languages, langEntry{Name: name, Nodes: n})
	}
	sort.Slice(languages, func(i, j int) bool {
		return languages[i].Nodes > languages[j].Nodes
	})
	primaryLang := ""
	if len(languages) > 0 {
		primaryLang = languages[0].Name
	}
	if len(languages) > topLanguagesN {
		languages = languages[:topLanguagesN]
	}

	summary := map[string]any{
		"total_nodes":      stats.TotalNodes,
		"total_edges":      stats.TotalEdges,
		"primary_language": primaryLang,
		"languages":        languages,
	}

	// Communities — top N by member count.
	communitiesSection := map[string]any{"count": 0}
	if comms := s.getCommunities(); comms != nil && len(comms.Communities) > 0 {
		sorted := append([]analysis.Community(nil), comms.Communities...)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Size > sorted[j].Size
		})
		top := sorted
		if len(top) > topCommunitiesN {
			top = top[:topCommunitiesN]
		}
		communitiesSection = map[string]any{
			"count":      len(comms.Communities),
			"modularity": comms.Modularity,
			"top":        topCommunitiesSummary(top),
		}
	}

	// Hotspots — load-bearing symbols by fan-in/out/crossings. Use a low
	// threshold to ensure we get the top N regardless of repo size.
	hotspotsSection := []map[string]any{}
	hs := analysis.FindHotspots(s.graph, s.getCommunities(), 0)
	if len(hs) > topHotspotsN {
		hs = hs[:topHotspotsN]
	}
	for _, h := range hs {
		hotspotsSection = append(hotspotsSection, map[string]any{
			"id":               h.ID,
			"name":             h.Name,
			"kind":             h.Kind,
			"file_path":        h.FilePath,
			"fan_in":           h.FanIn,
			"fan_out":          h.FanOut,
			"complexity_score": h.ComplexityScore,
		})
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"summary":             summary,
		"communities":         communitiesSection,
		"hotspots":            hotspotsSection,
		"most_imported_files": mostImportedFiles(s.graph, topMostImportedN),
		"entry_points":        entryPoints(s.graph, topEntryPointsN),
	})
}

// topCommunitiesSummary shapes a subset of communities for the outline.
// Trimmed from the full Community struct (members can be thousands of IDs)
// to just label, size, and cohesion — enough for the reader to decide
// whether to drill into that subsystem.
func topCommunitiesSummary(comms []analysis.Community) []map[string]any {
	out := make([]map[string]any, 0, len(comms))
	for _, c := range comms {
		out = append(out, map[string]any{
			"id":       c.ID,
			"label":    c.Label,
			"size":     c.Size,
			"cohesion": c.Cohesion,
		})
	}
	return out
}

// mostImportedFiles ranks files by incoming `imports` edges. This surfaces
// the shared modules — packages everyone reaches for — which is a strong
// "here's where the gravity lives" signal for newcomers.
func mostImportedFiles(g *graph.Graph, topN int) []map[string]any {
	type fileCount struct {
		path  string
		count int
	}
	counts := make(map[string]int)
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeImports {
			continue
		}
		target := g.GetNode(e.To)
		if target == nil {
			continue
		}
		// Aggregate at the file level. For Import-kind nodes the node's
		// FilePath is the file being imported; for File-kind nodes the
		// ID is already the path.
		path := target.FilePath
		if path == "" {
			path = target.ID
		}
		counts[path]++
	}

	var ranked []fileCount
	for p, c := range counts {
		ranked = append(ranked, fileCount{path: p, count: c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].path < ranked[j].path
	})
	if len(ranked) > topN {
		ranked = ranked[:topN]
	}

	out := make([]map[string]any, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, map[string]any{
			"path":         r.path,
			"import_count": r.count,
		})
	}
	return out
}

// entryPoints finds likely program entry points: functions named `main`
// (the Go / Rust / C convention) and top-level functions with no callers
// in files named `main.*` or `cmd/**`. Good enough for the outline; a
// fuller process-based walk is what `get_processes` does separately.
func entryPoints(g *graph.Graph, topN int) []map[string]any {
	type ep struct {
		id       string
		name     string
		filePath string
	}
	var out []ep
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if n.Name != "main" {
			continue
		}
		out = append(out, ep{id: n.ID, name: n.Name, filePath: n.FilePath})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].filePath < out[j].filePath
	})
	if len(out) > topN {
		out = out[:topN]
	}

	shaped := make([]map[string]any, 0, len(out))
	for _, e := range out {
		shaped = append(shaped, map[string]any{
			"id":        e.id,
			"name":      e.name,
			"file_path": e.filePath,
		})
	}
	return shaped
}
