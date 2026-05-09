package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// handleGetUntestedSymbols returns functions and methods in non-test files
// that no test file reaches via the call graph — the inverse of
// get_test_targets. Test reachability is computed in a single forward BFS
// from every symbol defined in a test file, walking `calls` and
// `references` edges; any non-test function/method not visited is
// considered uncovered.
//
// This is the "where should we add tests first" tool: results are sorted by
// fan_in descending so the most-used untested symbols surface at the top —
// they're the ones most likely to break something when changed without a
// safety net.
func (s *Server) handleGetUntestedSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 50)
	if limit <= 0 {
		limit = 50
	}
	filePrefix := req.GetString("file_prefix", "")
	minFanIn := req.GetInt("min_fan_in", 0)

	covered := reachableFromTests(s.graph)

	// Fan-in map for ranking — incoming calls/references only; imports and
	// defines would flood every exported symbol with meaningless coverage.
	fanIn := make(map[string]int)
	for _, e := range s.graph.AllEdges() {
		if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences {
			fanIn[e.To]++
		}
	}

	type untestedEntry struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		FilePath string `json:"file_path"`
		Line     int    `json:"line"`
		FanIn    int    `json:"fan_in"`
	}

	var entries []untestedEntry
	totalCandidates := 0
	for _, n := range s.graph.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		// Skip symbols defined inside test files — those ARE test code.
		if isTestFile(n.FilePath) {
			continue
		}
		if filePrefix != "" && !strings.HasPrefix(n.FilePath, filePrefix) {
			continue
		}
		totalCandidates++
		if covered[n.ID] {
			continue
		}
		fi := fanIn[n.ID]
		if fi < minFanIn {
			continue
		}
		entries = append(entries, untestedEntry{
			ID:       n.ID,
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
			FanIn:    fi,
		})
	}

	// Rank by fan_in desc, then file_path asc for stable ordering.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].FanIn != entries[j].FanIn {
			return entries[i].FanIn > entries[j].FanIn
		}
		if entries[i].FilePath != entries[j].FilePath {
			return entries[i].FilePath < entries[j].FilePath
		}
		return entries[i].ID < entries[j].ID
	})
	totalUncovered := len(entries)
	if len(entries) > limit {
		entries = entries[:limit]
	}

	var coverageRatio float64
	if totalCandidates > 0 {
		coverageRatio = float64(totalCandidates-totalUncovered) / float64(totalCandidates)
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"untested":         entries,
		"total_candidates": totalCandidates,
		"total_uncovered":  totalUncovered,
		"coverage_ratio":   coverageRatio,
		"truncated":        totalUncovered > len(entries),
	})
}

// reachableFromTests computes the set of symbol IDs reachable from any
// symbol defined in a test file, following outgoing `calls` and
// `references` edges. One pass over the graph at O(V+E) beats
// per-symbol BFS which would be O(V·(V+E)).
//
// Test files are detected via isTestFile so this works across languages
// (Go _test.go, Python test_*.py, JS .spec.ts, etc.) without per-language
// special-casing here.
func reachableFromTests(g *graph.Graph) map[string]bool {
	covered := make(map[string]bool)

	// Seed: every function/method defined in a test file.
	var frontier []string
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if !isTestFile(n.FilePath) {
			continue
		}
		if !covered[n.ID] {
			covered[n.ID] = true
			frontier = append(frontier, n.ID)
		}
	}

	// Forward BFS along calls + references. A test function that calls X
	// covers X; X transitively covers whatever X calls, etc.
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, id := range frontier {
			for _, e := range g.GetOutEdges(id) {
				if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
					continue
				}
				if !covered[e.To] {
					covered[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		frontier = next
	}
	return covered
}
