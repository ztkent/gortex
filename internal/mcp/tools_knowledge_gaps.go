package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// registerKnowledgeGapsTool wires get_knowledge_gaps — a cold-start
// audit composing four signals the graph already carries:
// disconnected nodes, thin communities, single-file communities, and
// untested hotspots. Returns the bundled view so an agent can decide
// where to invest test-writing / refactor effort before opening
// individual analyze calls.
func (s *Server) registerKnowledgeGapsTool() {
	s.addTool(
		mcp.NewTool("get_knowledge_gaps",
			mcp.WithDescription("Surface places the codebase under-documents itself. Composes disconnected nodes (zero in/out edges), thin communities (<thin_community_size members), single-file communities, and untested hotspots (high fan-in, low coverage_pct). Returns a bundled rollup so callers can rank where to invest test-writing / refactor effort. Cold-start audit aid: complements get_repo_outline by pointing at the weak spots, not the load-bearing ones."),
			mcp.WithNumber("thin_community_size", mcp.Description("Communities with fewer members are flagged 'thin' (default: 3).")),
			mcp.WithNumber("min_coverage_pct", mcp.Description("Hotspots with coverage_pct below this are 'untested' (default: 50). Hotspots without any coverage data are always included.")),
			mcp.WithNumber("hotspot_limit", mcp.Description("Top-N highest-fan-in nodes to evaluate against the coverage threshold (default: 20).")),
			mcp.WithNumber("limit_per_category", mcp.Description("Cap each rollup category (default: 20).")),
			mcp.WithString("path_prefix", mcp.Description("Scope analysis to nodes under this file-path prefix — e.g. 'internal/auth/'.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetKnowledgeGaps,
	)
}

// gapDisconnected — function/method with zero incoming and outgoing
// edges. Almost always either dead code or an isolated utility
// nobody wired up.
type gapDisconnected struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// gapCommunity — for thin and single-file communities the caller
// needs the same fields, so they share a row type.
type gapCommunity struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Size  int      `json:"size"`
	Files []string `json:"files"`
}

// gapUntestedHotspot — high-fan-in node whose coverage_pct is below
// the threshold, or absent entirely. fan_in is the in-edge count
// computed locally — independent of the hotspots analyzer's mean+2σ
// gate so we surface load-bearing nodes even in small repos where
// the analyzer is conservative.
type gapUntestedHotspot struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
	FanIn      int     `json:"fan_in"`
	Coverage   float64 `json:"coverage_pct"`
	HasCoverage bool   `json:"has_coverage"`
}

func (s *Server) handleGetKnowledgeGaps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	thinSize := req.GetInt("thin_community_size", 3)
	if thinSize < 1 {
		thinSize = 3
	}
	minCov := req.GetFloat("min_coverage_pct", 50.0)
	if minCov < 0 {
		minCov = 0
	}
	hotspotLimit := max(req.GetInt("hotspot_limit", 20), 1)
	perCategoryLimit := max(req.GetInt("limit_per_category", 20), 1)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))

	scoped := s.scopedNodes(ctx)

	disconnected := s.collectDisconnected(scoped, pathPrefix, perCategoryLimit)
	thin, singleFile := s.collectCommunityGaps(thinSize, pathPrefix, perCategoryLimit)
	untested := s.collectUntestedHotspots(scoped, pathPrefix, hotspotLimit, minCov, perCategoryLimit)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"disconnected_nodes":      disconnected,
		"thin_communities":        thin,
		"single_file_communities": singleFile,
		"untested_hotspots":       untested,
		"summary": map[string]any{
			"disconnected_count": len(disconnected),
			"thin_count":         len(thin),
			"single_file_count":  len(singleFile),
			"untested_count":     len(untested),
		},
		"thresholds": map[string]any{
			"thin_community_size": thinSize,
			"min_coverage_pct":    minCov,
			"hotspot_limit":       hotspotLimit,
			"limit_per_category":  perCategoryLimit,
		},
	})
}

// collectDisconnected returns function/method nodes with zero
// incoming and zero outgoing edges in the scoped subgraph. The
// kind filter mirrors handleAnalyzeCoverageGaps' default — variables
// and constants always look disconnected, so including them would
// flood the result.
//
// Picks NodeDegreeAggregator when the backend implements it (one
// batched in/out count instead of 2N GetInEdges/GetOutEdges cgo
// round-trips on Ladybug).
func (s *Server) collectDisconnected(scoped []*graph.Node, pathPrefix string, limit int) []gapDisconnected {
	// Build the candidate list first — kind+prefix filters touch
	// only the in-memory scoped slice so they cost nothing.
	candidates := make([]*graph.Node, 0, len(scoped))
	for _, n := range scoped {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		candidates = append(candidates, n)
	}

	out := make([]gapDisconnected, 0)
	if agg, ok := s.graph.(graph.NodeDegreeAggregator); ok && len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		byID := make(map[string]*graph.Node, len(candidates))
		for _, n := range candidates {
			ids = append(ids, n.ID)
			byID[n.ID] = n
		}
		for _, r := range agg.NodeDegreeCounts(ids, nil) {
			if r.InCount > 0 || r.OutCount > 0 {
				continue
			}
			n := byID[r.NodeID]
			if n == nil {
				continue
			}
			out = append(out, gapDisconnected{
				ID: n.ID, Name: n.Name, Kind: string(n.Kind),
				File: n.FilePath, Line: n.StartLine,
			})
		}
	} else {
		for _, n := range candidates {
			if len(s.graph.GetInEdges(n.ID)) > 0 || len(s.graph.GetOutEdges(n.ID)) > 0 {
				continue
			}
			out = append(out, gapDisconnected{
				ID: n.ID, Name: n.Name, Kind: string(n.Kind),
				File: n.FilePath, Line: n.StartLine,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// collectCommunityGaps walks the cached community result and
// produces two parallel rollups in one pass: thin communities (under
// the size threshold) and single-file communities (every member from
// the same file — usually a sign the cluster never crossed a module
// boundary).
func (s *Server) collectCommunityGaps(thinSize int, pathPrefix string, limit int) (thin, singleFile []gapCommunity) {
	thin = make([]gapCommunity, 0)
	singleFile = make([]gapCommunity, 0)

	cr := s.getCommunities()
	if cr == nil {
		return thin, singleFile
	}

	for _, c := range cr.Communities {
		// Path-prefix scope: keep the community if at least one
		// file lies under the prefix. Empty prefix = no filter.
		if pathPrefix != "" {
			match := false
			for _, f := range c.Files {
				if strings.HasPrefix(f, pathPrefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		row := gapCommunity{ID: c.ID, Label: c.Label, Size: c.Size, Files: c.Files}
		if c.Size < thinSize {
			thin = append(thin, row)
		}
		if len(c.Files) == 1 {
			singleFile = append(singleFile, row)
		}
	}

	sort.Slice(thin, func(i, j int) bool { return thin[i].Size < thin[j].Size })
	sort.Slice(singleFile, func(i, j int) bool { return singleFile[i].Size > singleFile[j].Size })

	if len(thin) > limit {
		thin = thin[:limit]
	}
	if len(singleFile) > limit {
		singleFile = singleFile[:limit]
	}
	return thin, singleFile
}

// collectUntestedHotspots ranks scoped function/method nodes by
// in-edge count, takes the top `hotspotLimit`, and keeps those with
// coverage_pct < minCov or no coverage data at all. Independent of
// analyze hotspots (which gates on mean+2σ) so it still surfaces
// load-bearing nodes in small repos.
//
// Uses NodeDegreeAggregator when the backend implements it (one
// batched in-count instead of N per-node GetInEdges cgo round-trips
// on Ladybug).
func (s *Server) collectUntestedHotspots(scoped []*graph.Node, pathPrefix string, hotspotLimit int, minCov float64, limit int) []gapUntestedHotspot {
	type ranked struct {
		node  *graph.Node
		fanIn int
	}
	// Pre-filter on kind + prefix Go-side first — that touches only
	// the in-memory scoped slice. Then ask the storage layer for the
	// bulk in-degree count if it offers one.
	pool := make([]*graph.Node, 0, len(scoped))
	for _, n := range scoped {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pool = append(pool, n)
	}
	candidates := make([]ranked, 0, len(pool))
	if agg, ok := s.graph.(graph.NodeDegreeAggregator); ok && len(pool) > 0 {
		ids := make([]string, 0, len(pool))
		byID := make(map[string]*graph.Node, len(pool))
		for _, n := range pool {
			ids = append(ids, n.ID)
			byID[n.ID] = n
		}
		for _, r := range agg.NodeDegreeCounts(ids, nil) {
			n := byID[r.NodeID]
			if n == nil {
				continue
			}
			candidates = append(candidates, ranked{node: n, fanIn: r.InCount})
		}
	} else {
		for _, n := range pool {
			candidates = append(candidates, ranked{node: n, fanIn: len(s.graph.GetInEdges(n.ID))})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fanIn > candidates[j].fanIn
	})
	if len(candidates) > hotspotLimit {
		candidates = candidates[:hotspotLimit]
	}

	out := make([]gapUntestedHotspot, 0)
	for _, c := range candidates {
		// A "hotspot" with zero callers isn't a hotspot — drop it.
		// Disconnected functions are already covered by the
		// disconnected_nodes rollup.
		if c.fanIn == 0 {
			continue
		}
		pct, has := c.node.Meta["coverage_pct"].(float64)
		if has && pct >= minCov {
			continue
		}
		out = append(out, gapUntestedHotspot{
			ID:          c.node.ID,
			Name:        c.node.Name,
			File:        c.node.FilePath,
			Line:        c.node.StartLine,
			FanIn:       c.fanIn,
			Coverage:    pct,
			HasCoverage: has,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FanIn != out[j].FanIn {
			return out[i].FanIn > out[j].FanIn
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
