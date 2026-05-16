package mcp

import (
	"bytes"
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/dataflow"
	"github.com/zzet/gortex/internal/graph"
)

// registerDataflowTools wires the CPG-lite dataflow MCP surface.
// Two tools ship today:
//
//   - flow_between(source_id, sink_id, max_depth?, max_paths?) →
//     ranked dataflow paths between a specific pair of symbols.
//   - taint_paths(source_pattern, sink_pattern, max_depth?,
//     limit?) → pattern-driven sweep returning every flow from a
//     matching source to a matching sink.
//
// Both tools accept format=gcx for the GCX1 wire format; the
// per-tool encoders live in this file alongside the handlers.
func (s *Server) registerDataflowTools() {
	s.addTool(
		mcp.NewTool("flow_between",
			mcp.WithDescription("Returns ranked dataflow paths between two symbols. Walks EdgeValueFlow / EdgeArgOf / EdgeReturnsTo forward from source to sink — the CPG-lite primitive that answers \"where does this value flow?\". Pairs with taint_paths for pattern-driven sweeps. Every EdgeStep carries an origin tier (lsp_resolved / ast_resolved / …) and a coarse `tier` label (lsp / ast / heuristic); pass `min_tier` to prune edges below a threshold during traversal."),
			mcp.WithString("source_id", mcp.Required(), mcp.Description("Source symbol node ID — typically a function, method, or parameter")),
			mcp.WithString("sink_id", mcp.Required(), mcp.Description("Sink symbol node ID — function/method/param/field")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum BFS hops (default: 8)")),
			mcp.WithNumber("max_paths", mcp.Description("Maximum number of paths to return (default: 10)")),
			mcp.WithString("min_tier", mcp.Description("Minimum per-edge Origin tier to traverse. Accepts one of: lsp_resolved, lsp_dispatch, ast_resolved, ast_inferred, text_matched. Empty (default) disables the filter.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleFlowBetween,
	)

	s.addTool(
		mcp.NewTool("taint_paths",
			mcp.WithDescription("Pattern-driven dataflow sweep — resolves every symbol matching `source_pattern` and `sink_pattern`, then walks the dataflow graph to find paths between each pair. Use for security-style queries (\"every flow from os.Getenv to db.Query\") and architectural audits. Pattern syntax: bare token = case-insensitive substring on symbol name; `exact:Foo` = exact name; `path:dir/` = file-path prefix; `kind:method` = restrict node kind. Combine clauses with spaces. Pass `min_tier` to prune per-edge traversal below a provenance threshold."),
			mcp.WithString("source_pattern", mcp.Required(), mcp.Description("Source pattern — see description for syntax")),
			mcp.WithString("sink_pattern", mcp.Required(), mcp.Description("Sink pattern — see description for syntax")),
			mcp.WithNumber("max_depth", mcp.Description("Maximum BFS hops per (source,sink) pair (default: 8)")),
			mcp.WithNumber("limit", mcp.Description("Maximum findings to return (default: 20)")),
			mcp.WithString("min_tier", mcp.Description("Minimum per-edge Origin tier to traverse. Accepts one of: lsp_resolved, lsp_dispatch, ast_resolved, ast_inferred, text_matched. Empty (default) disables the filter.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleTaintPaths,
	)
}

func (s *Server) handleFlowBetween(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	source, err := req.RequireString("source_id")
	if err != nil {
		return mcp.NewToolResultError("source_id is required"), nil
	}
	sink, err := req.RequireString("sink_id")
	if err != nil {
		return mcp.NewToolResultError("sink_id is required"), nil
	}
	maxDepth := req.GetInt("max_depth", dataflow.DefaultMaxDepth)
	maxPaths := req.GetInt("max_paths", dataflow.DefaultMaxPaths)
	minTier := req.GetString("min_tier", "")

	engine := dataflow.New(s.graph)
	paths := engine.FlowBetweenWithTier(source, sink, maxDepth, maxPaths, minTier)

	if s.isGCX(ctx, req) {
		payload, err := encodeFlowBetween(source, sink, paths)
		return s.gcxResponseWithBudget(req)(payload, err)
	}

	result := map[string]any{
		"source_id": source,
		"sink_id":   sink,
		"paths":     paths,
		"total":     len(paths),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleTaintPaths(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	srcRaw, err := req.RequireString("source_pattern")
	if err != nil {
		return mcp.NewToolResultError("source_pattern is required"), nil
	}
	sinkRaw, err := req.RequireString("sink_pattern")
	if err != nil {
		return mcp.NewToolResultError("sink_pattern is required"), nil
	}
	maxDepth := req.GetInt("max_depth", dataflow.DefaultMaxDepth)
	limit := req.GetInt("limit", 20)
	minTier := req.GetString("min_tier", "")

	src := dataflow.ParsePattern(srcRaw)
	if src.Empty() {
		return mcp.NewToolResultError("source_pattern matched no clauses"), nil
	}
	sink := dataflow.ParsePattern(sinkRaw)
	if sink.Empty() {
		return mcp.NewToolResultError("sink_pattern matched no clauses"), nil
	}

	engine := dataflow.New(s.graph)
	findings := engine.TaintPathsWithTier(src, sink, maxDepth, limit, minTier)

	if s.isGCX(ctx, req) {
		payload, err := encodeTaintPaths(srcRaw, sinkRaw, findings)
		return s.gcxResponseWithBudget(req)(payload, err)
	}

	rows := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		rows = append(rows, map[string]any{
			"source": describeNode(f.Source),
			"sink":   describeNode(f.Sink),
			"paths":  f.Paths,
		})
	}
	result := map[string]any{
		"source_pattern": srcRaw,
		"sink_pattern":   sinkRaw,
		"findings":       rows,
		"total":          len(findings),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// describeNode returns a JSON-shaped summary of a graph node for
// taint findings.
func describeNode(n *graph.Node) map[string]any {
	if n == nil {
		return nil
	}
	return map[string]any{
		"id":         n.ID,
		"kind":       string(n.Kind),
		"name":       n.Name,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
}

// encodeFlowBetween emits a GCX1 envelope with two sections:
// `flow_between.summary` (one row carrying source, sink, totals and
// the weakest tier seen across all returned paths) and
// `flow_between.paths` (one row per path with the flattened node
// sequence, edge kind sequence, per-step origin + tier sequences,
// and the weakest tier on the path).
func encodeFlowBetween(source, sink string, paths []dataflow.Path) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "flow_between.summary",
		Fields: []string{"source", "sink", "paths", "shortest", "worst_tier"},
	})
	shortest := 0
	worstAll := ""
	if len(paths) > 0 {
		shortest = paths[0].Length()
		for _, p := range paths {
			worstAll = mergeWorstTier(worstAll, worstTierOnPath(p.Edges))
		}
	}
	if err := sumEnc.WriteRow(source, sink, len(paths), shortest, worstAll); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}
	pathEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "flow_between.paths",
		Fields: []string{"length", "confidence", "worst_tier", "ids", "kinds", "origins", "tiers"},
		Meta: map[string]string{
			"count": fmt.Sprintf("%d", len(paths)),
		},
	})
	for _, p := range paths {
		ids := joinPathIDs(p.IDs)
		kinds := joinEdgeKinds(p.Edges)
		origins := joinEdgeOrigins(p.Edges)
		tiers := joinEdgeTiers(p.Edges)
		if err := pathEnc.WriteRow(p.Length(), p.Confidence, worstTierOnPath(p.Edges), ids, kinds, origins, tiers); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), pathEnc.Close()
}

// encodeTaintPaths emits a GCX1 envelope with two sections:
// `taint_paths.summary` and `taint_paths.findings`. Each finding
// row carries the best (shortest, highest-confidence) path; the
// other paths are joined into a parallel field for offline drill-
// down without repeating the source/sink columns per path.
func encodeTaintPaths(srcPattern, sinkPattern string, findings []dataflow.TaintFinding) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "taint_paths.summary",
		Fields: []string{"source_pattern", "sink_pattern", "findings"},
	})
	if err := sumEnc.WriteRow(srcPattern, sinkPattern, len(findings)); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}
	findEnc := wire.NewEncoder(&buf, wire.Header{
		Tool: "taint_paths.findings",
		Fields: []string{
			"source_id", "source_name", "sink_id", "sink_name",
			"best_length", "best_confidence", "best_worst_tier",
			"paths", "best_ids", "best_kinds", "best_origins", "best_tiers",
		},
	})
	for _, f := range findings {
		best := dataflow.Path{}
		if len(f.Paths) > 0 {
			best = f.Paths[0]
		}
		row := []any{
			nodeIDOf(f.Source),
			nodeNameOf(f.Source),
			nodeIDOf(f.Sink),
			nodeNameOf(f.Sink),
			best.Length(),
			best.Confidence,
			worstTierOnPath(best.Edges),
			len(f.Paths),
			joinPathIDs(best.IDs),
			joinEdgeKinds(best.Edges),
			joinEdgeOrigins(best.Edges),
			joinEdgeTiers(best.Edges),
		}
		if err := findEnc.WriteRow(row...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), findEnc.Close()
}

func nodeIDOf(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return n.ID
}

func nodeNameOf(n *graph.Node) string {
	if n == nil {
		return ""
	}
	return n.Name
}

func joinPathIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(id)
	}
	return b.String()
}

func joinEdgeKinds(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(e.Kind)
	}
	return b.String()
}

func joinEdgeOrigins(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(e.Origin)
	}
	return b.String()
}

func joinEdgeTiers(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	var b bytes.Buffer
	for i, e := range edges {
		if i > 0 {
			b.WriteString(",")
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		b.WriteString(tier)
	}
	return b.String()
}

// worstTierOnPath returns the lowest-confidence tier label across
// every step. The mapping is symmetric with graph.ResolvedBy:
// "lsp" > "ast" > "heuristic". An empty step list returns "".
func worstTierOnPath(edges []dataflow.EdgeStep) string {
	if len(edges) == 0 {
		return ""
	}
	worst := ""
	for _, e := range edges {
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		worst = mergeWorstTier(worst, tier)
	}
	return worst
}

// mergeWorstTier collapses two tier labels into the lower-confidence
// one. Used by both per-path and across-path summaries so callers
// see the weakest link without recomputing the mapping.
func mergeWorstTier(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if tierRank(a) <= tierRank(b) {
		return a
	}
	return b
}

func tierRank(tier string) int {
	switch tier {
	case "lsp":
		return 3
	case "ast":
		return 2
	case "heuristic":
		return 1
	}
	return 0
}
