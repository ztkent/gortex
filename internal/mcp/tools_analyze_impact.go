package mcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
)

// ---------------------------------------------------------------------------
// analyze kind=impact
// ---------------------------------------------------------------------------
//
// Composite per-symbol change-impact score. Where health_score grades
// code *quality*, impact grades *blast radius* — how much breaks, and
// how hard it is to change safely. Five axes, each normalised to
// 0..100 (higher = more impactful), then combined into one weighted
// composite:
//
//   - centrality  — PageRank over the call graph. A symbol central
//                   nodes depend on carries architectural weight.
//   - reach       — transitive dependent count (precomputed reach
//                   index when available, else direct fan-in).
//   - complexity  — McCabe cyclomatic complexity. Branchy bodies are
//                   harder to change without regressions.
//   - co_change   — git-history logical coupling of the symbol's file.
//                   Strong coupling means a change ripples to files
//                   the import graph never connects.
//   - community   — how many distinct graph communities the symbol's
//                   direct neighbours span. Cross-community symbols
//                   are architectural seams.
//
// The composite is a single ranked number so an agent can ask "what
// are the highest-impact symbols" or "how risky is changing X" with
// one call instead of fusing hotspots + explain_change_impact +
// find_co_changing_symbols by hand.

// Per-axis composite weights. The formula is the user-visible
// contract — named constants so a change is deliberate. Centrality
// and reach dominate: they answer "how much depends on this".
const (
	impactWeightCentrality = 2.5
	impactWeightReach      = 2.5
	impactWeightComplexity = 1.5
	impactWeightCoChange   = 1.5
	impactWeightCommunity  = 1.0
)

// Axis saturation constants — the half-saturation point of each
// 100*x/(x+k) curve, picked so a typical "notable" value lands near
// the 60-75 band rather than pinning the axis at 100.
const (
	impactReachK        = 30.0
	impactComplexityK   = 8.0
	impactCoChangeK     = 2.0
	impactCommunityStep = 25.0 // neighbours spanning 4 communities = 100
)

// impactRow is the per-symbol breakdown returned by analyze impact.
type impactRow struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	File  string  `json:"file"`
	Line  int     `json:"line"`
	Score float64 `json:"score"`
	Risk  string  `json:"risk"`

	// Axes — each a 0..100 impact value.
	Centrality float64 `json:"centrality"`
	Reach      float64 `json:"reach"`
	Complexity float64 `json:"complexity"`
	CoChange   float64 `json:"co_change"`
	Community  float64 `json:"community"`

	// Raw inputs behind the axes, for explainability.
	PageRank      float64 `json:"pagerank"`
	ReachCount    int     `json:"reach_count"`
	Cyclomatic    int     `json:"cyclomatic"`
	CoChangeFiles int     `json:"co_change_files"`
	CommunitySpan int     `json:"community_span"`
	FanIn         int     `json:"fan_in"`
}

// handleAnalyzeImpactComposite ranks scoped symbols by composite
// change impact.
//
// Filters:
//   - ids         — comma-separated symbol IDs; score only these
//     (the "blast radius of changing X" use).
//   - path_prefix — keep only symbols whose file path starts here.
//   - kinds       — comma-separated (default function,method); "all"
//     keeps every kind.
//   - min_score / max_score — composite-score band filter.
//   - limit       — cap rows (default 100); total reports the
//     pre-truncation count.
func (s *Server) handleAnalyzeImpactComposite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	idFilter := splitIDSet(stringArg(args, "ids"))
	limit := intArg(args, "limit", 100)
	minScore := -1.0
	if v, ok := args["min_score"].(float64); ok {
		minScore = v
	}
	maxScore := -1.0
	if v, ok := args["max_score"].(float64); ok {
		maxScore = v
	}

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	// Co-change feeds one axis — make sure the git mine has run.
	s.ensureCoChange()

	pr := s.getPageRank()
	maxPR := 0.0
	if pr != nil {
		maxPR = pr.Max
	}
	nodeToComm := map[string]string{}
	if c := s.getCommunities(); c != nil && c.NodeToComm != nil {
		nodeToComm = c.NodeToComm
	}

	// Build the candidate id set up front so both the fan-in
	// aggregator and the per-edge community walk stay bounded by
	// the kinds / path / ids the caller actually asked for. Without
	// this, the analyzer paid for an unfiltered AllEdges()
	// materialisation per call -- ~500k edges over cgo on the gortex
	// workspace, the bulk of the wall-clock cost on Ladybug.
	scoped := s.scopedNodes(ctx)
	candidateIDs := make([]string, 0, len(scoped))
	candidateSet := make(map[string]struct{}, len(scoped))
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if allowedKinds != nil {
			if _, ok := allowedKinds[n.Kind]; !ok {
				continue
			}
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		if len(idFilter) > 0 {
			if _, ok := idFilter[n.ID]; !ok {
				continue
			}
		}
		candidateIDs = append(candidateIDs, n.ID)
		candidateSet[n.ID] = struct{}{}
	}

	// fan-in: uses the NodeFanAggregator capability when the
	// backend supports it (one bulk Cypher per direction over the
	// candidate id set) and falls back to a per-kind EdgesByKind
	// stream otherwise. fanOutKinds is empty -- impact only reads
	// fan-in.
	fanIn, _ := analysis.CollectFanCounts(s.graph, candidateIDs,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		nil,
	)

	// neighborComms[n] = set of distinct communities of n's call /
	// reference neighbours (both directions). Streamed via
	// EdgesByKind per kind so neither backend pays for an
	// unfiltered AllEdges walk; the per-kind MATCH on disk backends
	// is the same plan EdgesByKind feeds every other analyzer.
	// Membership is restricted to candidate ids -- a node outside
	// the result set has nowhere to receive a span count.
	neighborComms := map[string]map[string]struct{}{}
	addComm := func(node, comm string) {
		if comm == "" {
			return
		}
		if _, ok := candidateSet[node]; !ok {
			return
		}
		set := neighborComms[node]
		if set == nil {
			set = map[string]struct{}{}
			neighborComms[node] = set
		}
		set[comm] = struct{}{}
	}
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range s.graph.EdgesByKind(kind) {
			if e == nil {
				continue
			}
			addComm(e.From, nodeToComm[e.To])
			addComm(e.To, nodeToComm[e.From])
		}
	}

	rows := make([]impactRow, 0, len(candidateIDs))
	for _, n := range scoped {
		if n == nil {
			continue
		}
		if _, ok := candidateSet[n.ID]; !ok {
			continue
		}

		prVal := pr.ScoreOf(n.ID)
		centrality := 0.0
		if maxPR > 0 {
			centrality = 100 * prVal / maxPR
		}

		// Reach: precomputed transitive set when the index is built,
		// otherwise direct fan-in as the depth-1 proxy.
		reachCount := fanIn[n.ID]
		if d1, d2, d3, hit := reach.Lookup(s.graph, n.ID); hit {
			reachCount = len(d1) + len(d2) + len(d3)
		}
		reachScore := saturate(float64(reachCount), impactReachK)

		cyc := cyclomaticOf(n)
		complexityScore := saturate(float64(cyc-1), impactComplexityK)

		var ccSum float64
		ccFiles := 0
		for _, sc := range s.coChangeScores(n.FilePath) {
			ccSum += sc
			ccFiles++
		}
		coChangeScore := saturate(ccSum, impactCoChangeK)

		span := len(neighborComms[n.ID])
		communityScore := math.Min(100, float64(span)*impactCommunityStep)

		composite := (centrality*impactWeightCentrality +
			reachScore*impactWeightReach +
			complexityScore*impactWeightComplexity +
			coChangeScore*impactWeightCoChange +
			communityScore*impactWeightCommunity) /
			(impactWeightCentrality + impactWeightReach + impactWeightComplexity +
				impactWeightCoChange + impactWeightCommunity)

		row := impactRow{
			ID:            n.ID,
			Name:          n.Name,
			Kind:          string(n.Kind),
			File:          n.FilePath,
			Line:          n.StartLine,
			Score:         roundTo(composite, 2),
			Risk:          impactRisk(composite),
			Centrality:    roundTo(centrality, 2),
			Reach:         roundTo(reachScore, 2),
			Complexity:    roundTo(complexityScore, 2),
			CoChange:      roundTo(coChangeScore, 2),
			Community:     roundTo(communityScore, 2),
			PageRank:      prVal,
			ReachCount:    reachCount,
			Cyclomatic:    cyc,
			CoChangeFiles: ccFiles,
			CommunitySpan: span,
			FanIn:         fanIn[n.ID],
		}
		if minScore >= 0 && row.Score < minScore {
			continue
		}
		if maxScore >= 0 && row.Score > maxScore {
			continue
		}
		rows = append(rows, row)
	}

	// Rank by composite descending — the highest-impact symbols, the
	// ones to change with the most care, surface first.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].ID < rows[j].ID
	})

	total := len(rows)
	truncated := false
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("impact", rows))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-8s %6.2f  %s:%d  %s\n", r.Risk, r.Score, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no symbols scored\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	resp := map[string]any{
		"symbols":   rows,
		"total":     total,
		"truncated": truncated,
		"weights": map[string]float64{
			"centrality": impactWeightCentrality,
			"reach":      impactWeightReach,
			"complexity": impactWeightComplexity,
			"co_change":  impactWeightCoChange,
			"community":  impactWeightCommunity,
		},
	}
	if truncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// saturate maps a non-negative raw value onto 0..100 with a
// half-saturation point of k: x==k yields 50, x==3k yields 75.
func saturate(x, k float64) float64 {
	if x <= 0 || k <= 0 {
		return 0
	}
	return 100 * x / (x + k)
}

// impactRisk buckets a composite score into a coarse risk label.
func impactRisk(score float64) string {
	switch {
	case score >= 75:
		return "CRITICAL"
	case score >= 55:
		return "HIGH"
	case score >= 35:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

// cyclomaticOf reads the McCabe complexity stamped on a function node
// by the language extractors. The extractors only stamp values above
// 1, so an absent key means the canonical "no branches" score of 1.
func cyclomaticOf(n *graph.Node) int {
	if n == nil || n.Meta == nil {
		return 1
	}
	switch v := n.Meta["complexity"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return 1
}

// splitIDSet parses a comma-separated symbol-ID list into a set.
// Unlike parseCSVSet it preserves case — symbol IDs are case-sensitive.
func splitIDSet(in string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(in, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = struct{}{}
		}
	}
	return out
}
