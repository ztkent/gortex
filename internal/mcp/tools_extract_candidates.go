package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// registerExtractionCandidatesTool wires get_extraction_candidates
// — a ranked list of function/method nodes where an extract-function
// refactor would plausibly pay off. Composes three signals already
// in the graph (size, caller count, internal fan-out) into a
// single score and a per-candidate rationale.
//
// Heuristic: a function is a good extraction candidate when it's
// long, called from multiple places, and internally complex (many
// distinct callees). Long-single-caller-simple functions don't
// benefit; short-many-caller-simple functions are already utility
// shapes nobody benefits from breaking up.
func (s *Server) registerExtractionCandidatesTool() {
	s.addTool(
		mcp.NewTool("get_extraction_candidates",
			mcp.WithDescription("Rank function/method nodes by extract-function value. Score composes size (log line_count), caller count (log fan-in), and internal complexity (log fan-out). Returns top-N with {symbol_id, name, file, line_count, caller_count, fan_out, score, rationale}. Filter via min_lines / min_callers / min_fan_out / path_prefix. Pairs with /gortex-extract-function skill — that enforces the LSP-based refactor path; this picks where to apply it."),
			mcp.WithNumber("min_lines", mcp.Description("Skip functions shorter than this many lines (default: 20).")),
			mcp.WithNumber("min_callers", mcp.Description("Skip functions with fewer callers (default: 2).")),
			mcp.WithNumber("min_fan_out", mcp.Description("Skip functions with fewer distinct callees (default: 5).")),
			mcp.WithString("path_prefix", mcp.Description("Scope to nodes under this file-path prefix.")),
			mcp.WithNumber("limit", mcp.Description("Cap the result set (default: 25).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleGetExtractionCandidates,
	)
}

type extractCandidateRow struct {
	ID           string  `json:"symbol_id"`
	Name         string  `json:"name"`
	File         string  `json:"file"`
	StartLine    int     `json:"start_line"`
	EndLine      int     `json:"end_line"`
	LineCount    int     `json:"line_count"`
	CallerCount  int     `json:"caller_count"`
	FanOut       int     `json:"fan_out"`
	Score        float64 `json:"score"`
	Rationale    string  `json:"rationale"`
}

func (s *Server) handleGetExtractionCandidates(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	minLines := max(req.GetInt("min_lines", 20), 1)
	minCallers := max(req.GetInt("min_callers", 2), 0)
	minFanOut := max(req.GetInt("min_fan_out", 5), 0)
	pathPrefix := strings.TrimSpace(req.GetString("path_prefix", ""))
	limit := max(req.GetInt("limit", 25), 1)

	scoped := s.scopedNodes(ctx)
	rows := make([]extractCandidateRow, 0, len(scoped))
	for _, n := range scoped {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		if n.StartLine == 0 || n.EndLine == 0 {
			continue
		}
		lineCount := n.EndLine - n.StartLine + 1
		if lineCount < minLines {
			continue
		}

		callers := callerCount(s.graph, n.ID)
		if callers < minCallers {
			continue
		}
		fanOut := distinctCalleeCount(s.graph, n.ID)
		if fanOut < minFanOut {
			continue
		}

		// Log-scaled composite — long-tail values don't dominate the
		// short-tail. Adding 1 inside each log keeps the score >= 0.
		score := math.Log1p(float64(lineCount)) *
			math.Log1p(float64(callers)) *
			math.Log1p(float64(fanOut))

		rows = append(rows, extractCandidateRow{
			ID: n.ID, Name: n.Name, File: n.FilePath,
			StartLine: n.StartLine, EndLine: n.EndLine,
			LineCount:   lineCount,
			CallerCount: callers,
			FanOut:      fanOut,
			Score:       roundScore(score),
			Rationale:   buildExtractRationale(lineCount, callers, fanOut),
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"candidates":  rows,
		"total":       len(rows),
		"truncated":   truncated,
		"thresholds": map[string]any{
			"min_lines":   minLines,
			"min_callers": minCallers,
			"min_fan_out": minFanOut,
		},
	})
}

// callerCount returns the number of distinct call-site origins for
// the given node. Counts EdgeCalls and the cross-repo call variant.
func callerCount(g graph.Store, id string) int {
	seen := map[string]bool{}
	for _, e := range g.GetInEdges(id) {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeCrossRepoCalls {
			continue
		}
		seen[e.From] = true
	}
	return len(seen)
}

// distinctCalleeCount returns how many distinct functions/methods
// the node calls. Proxy for internal complexity — a function that
// orchestrates 20 different callees is probably doing too much.
func distinctCalleeCount(g graph.Store, id string) int {
	seen := map[string]bool{}
	for _, e := range g.GetOutEdges(id) {
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeCrossRepoCalls {
			continue
		}
		seen[e.To] = true
	}
	return len(seen)
}

// buildExtractRationale produces a human-readable explanation of
// which signals fired. Lets the agent (and the user) understand
// why each candidate ranked where it did.
func buildExtractRationale(lineCount, callers, fanOut int) string {
	parts := []string{}
	if lineCount >= 50 {
		parts = append(parts, fmt.Sprintf("very long (%d lines)", lineCount))
	} else if lineCount >= 20 {
		parts = append(parts, fmt.Sprintf("long (%d lines)", lineCount))
	}
	if callers >= 10 {
		parts = append(parts, fmt.Sprintf("widely called (%d callers)", callers))
	} else if callers >= 2 {
		parts = append(parts, fmt.Sprintf("multi-caller (%d)", callers))
	}
	if fanOut >= 15 {
		parts = append(parts, fmt.Sprintf("orchestration shape (%d callees)", fanOut))
	} else if fanOut >= 5 {
		parts = append(parts, fmt.Sprintf("complex body (%d callees)", fanOut))
	}
	if len(parts) == 0 {
		return "meets minimum thresholds"
	}
	return strings.Join(parts, ", ")
}
