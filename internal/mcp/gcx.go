package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// isGCX reports whether the caller requested the GCX1 compact wire
// format for this tool call. Selection order:
//
//  1. Explicit `format` arg on the request — wins unconditionally.
//     `format: "json"` returns false even when the session default is
//     gcx, so an agent can opt out per-call when it needs a JSON
//     payload (e.g. piping to a non-GCX consumer).
//  2. Per-session default driven by the MCP `clientInfo.name` snooped
//     at handshake — claude-code / cursor / vscode / zed / etc. ship
//     with the @gortex/wire (or gcx-go) decoder so the daemon emits
//     gcx by default for them. Other clients fall through.
//  3. JSON fallback for unknown clients.
//
// The Server receiver is needed only for the session lookup; nil
// receiver collapses to "explicit arg only" semantics, matching the
// pre-session-default behaviour.
func (s *Server) isGCX(ctx context.Context, req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok && v != "" {
		return wire.ParseFormat(v) == wire.FormatGCX
	}
	if s == nil {
		return false
	}
	return s.resolveSessionFormat(ctx) == "gcx"
}

// gcxResponseWithBudget binds a request to the gcx-response builder.
// Returns a 2-arg function so it can be called inline with the
// encoder's `(payload []byte, err error)` return tuple — the same
// invocation shape as the legacy gcxResponse. Byte-level row-tail
// trimming kicks in when the caller opted into a budget (`max_bytes`
// or `paginate: true`); without opt-in the payload is forwarded
// untouched, so non-budgeted callers retain pre-budgeting behaviour
// (full result inline, transport-spilled if the harness cap fires).
func (s *Server) gcxResponseWithBudget(req mcp.CallToolRequest) func([]byte, error) (*mcp.CallToolResult, error) {
	budget := effectiveBudget(req)
	return func(payload []byte, err error) (*mcp.CallToolResult, error) {
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("wire encode failed: %v", err)), nil
		}
		if budget > 0 {
			if trimmed, didTrim := trimGCXBytes(payload, budget); trimmed != nil {
				payload = trimmed
				if didTrim {
					payload = decorateTokenBudgetGCX(payload, req)
				}
			}
		}
		return mcp.NewToolResultText(string(payload)), nil
	}
}

// newGCX creates an encoder writing to w with the given tool + fields.
// Encoders own their header so section layout stays visible at the
// call site.
func newGCX(w *bytes.Buffer, tool string, fields []string, metaKV ...string) *wire.Encoder {
	meta := map[string]string{}
	for i := 0; i+1 < len(metaKV); i += 2 {
		meta[metaKV[i]] = metaKV[i+1]
	}
	return wire.NewEncoder(w, wire.Header{
		Tool:   tool,
		Fields: fields,
		Meta:   meta,
	})
}

// nodeShort returns the short form of a node ID — whatever follows
// the last "::" separator. For methods this carries the receiver; for
// functions / types it is the plain name.
func nodeShort(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if idx := strings.LastIndex(n.ID, "::"); idx >= 0 {
		return n.ID[idx+2:]
	}
	return n.Name
}

// nodeSig returns the rendered signature string for a node, falling
// back to "" when no signature was extracted.
func nodeSig(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	if s, ok := n.Meta["signature"].(string); ok {
		return s
	}
	return ""
}

// nodeIsTest reports whether a node was flagged as a test by the
// indexer's test-edge pass (Meta["is_test"]).
func nodeIsTest(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["is_test"].(bool)
	return v
}

// nodeTestRole returns the node's specific test role — "test",
// "benchmark", "fuzz", or "example" — or "" for non-test nodes.
func nodeTestRole(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	r, _ := n.Meta["test_role"].(string)
	return r
}

// nodeTestRunner returns the node's resolved test-runner identifier —
// "mocha" / "bun-test" / "jest" / "vitest" / "node-test" / "playwright"
// / "cypress" / "gotest" / "pytest" / "unittest" / "rspec" / "minitest"
// — or "" when the test-edge pass found no signal. Surfaced on the
// listing rows alongside is_test / test_role so agents can scope test
// pickers per runner (e.g. `--testNamePattern` vs `--grep`).
func nodeTestRunner(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	r, _ := n.Meta["test_runner"].(string)
	return r
}

// shouldSkipGraphNode filters File and Import pseudo-nodes the way the
// legacy compact / TOON formatters do — they add noise without
// informational value in symbol-oriented outputs.
func shouldSkipGraphNode(n *graph.Node) bool {
	if n == nil {
		return true
	}
	return n.Kind == graph.KindFile || n.Kind == graph.KindImport
}

// --------------------------------------------------------------------
// Hand-tuned encoders for the top-10 hot-path tools.
// --------------------------------------------------------------------

// encodeWinnowSymbols emits one row per ranked hit with per-axis score
// contributions. The contributions column is a pipe-separated list of
// `axis=value` pairs so decoders can recover the attribution without a
// nested structure.
func encodeWinnowSymbols(rows []winnowResult, total, limit int, weights map[string]float64) ([]byte, error) {
	truncated := total > limit
	if weights == nil {
		weights = winnowAxisWeights
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "winnow_symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "score", "fan_in", "fan_out", "churn", "community", "contributions", "is_test", "test_role", "test_runner"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
		"weights", formatAxisWeights(weights),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(rows))); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if shouldSkipGraphNode(r.Node) {
			continue
		}
		if err := enc.WriteRow(
			r.Node.ID,
			string(r.Node.Kind),
			nodeShort(r.Node),
			r.Node.FilePath,
			r.Node.StartLine,
			nodeSig(r.Node),
			roundFloat(r.Score),
			r.FanIn,
			r.FanOut,
			r.Churn,
			r.Community,
			formatContributions(r.Contributions),
			nodeIsTest(r.Node),
			nodeTestRole(r.Node),
			nodeTestRunner(r.Node),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// formatContributions renders the per-axis attribution map as a stable
// pipe-separated key=value list (sorted by key for determinism).
func formatContributions(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%s=%.3f", k, m[k])
	}
	return b.String()
}

// formatAxisWeights mirrors formatContributions for the meta header.
func formatAxisWeights(m map[string]float64) string {
	return formatContributions(m)
}

// encodeSearchSymbols emits one row per search hit with the minimum
// fields an agent needs to decide whether to fetch more detail.
func encodeSearchSymbols(nodes []*graph.Node, total, limit int) ([]byte, error) {
	truncated := total > limit
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "search_symbols",
		[]string{"id", "kind", "name", "path", "path_abs", "line", "sig", "is_test", "test_role", "test_runner"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(nodes))); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		if err := enc.WriteRow(
			n.ID,
			string(n.Kind),
			nodeShort(n),
			n.FilePath,
			n.AbsoluteFilePath,
			n.StartLine,
			nodeSig(n),
			nodeIsTest(n),
			nodeTestRole(n),
			nodeTestRunner(n),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeGetSymbolSource emits a single row carrying the node
// metadata plus the full source. Source is escaped line-by-line.
func encodeGetSymbolSource(node *graph.Node, source string, fromLine int, etag string) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "get_symbol_source",
		[]string{"id", "kind", "name", "path", "start_line", "end_line", "from_line", "sig", "etag", "source"},
		"etag", etag,
	)
	if err := enc.WriteRow(
		node.ID,
		string(node.Kind),
		node.Name,
		node.FilePath,
		node.StartLine,
		node.EndLine,
		fromLine,
		nodeSig(node),
		etag,
		source,
	); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// encodeBatchSymbols emits one row per requested symbol; missing
// symbols carry an error cell instead of a real node.
func encodeBatchSymbols(rows []map[string]any, includeSource bool) ([]byte, error) {
	fields := []string{"id", "kind", "name", "path", "start_line", "end_line", "sig"}
	if includeSource {
		fields = append(fields, "source")
	}
	fields = append(fields, "error")
	var buf bytes.Buffer
	enc := newGCX(&buf, "batch_symbols", fields,
		"count", fmt.Sprintf("%d", len(rows)),
	)
	for _, r := range rows {
		values := []any{
			str(r["id"]),
			str(r["kind"]),
			str(r["name"]),
			str(r["file_path"]),
			r["start_line"],
			r["end_line"],
			str(r["signature"]),
		}
		if includeSource {
			values = append(values, str(r["source"]))
		}
		values = append(values, str(r["error"]))
		if err := enc.WriteRow(values...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// zeroEdgeCaveatMeta renders a zero-edge caveat as GCX header meta
// key/value pairs (caveat + caveat_message). Returns nil when the
// caveat is absent so a non-empty result carries no extra meta.
func zeroEdgeCaveatMeta(c *graph.ZeroEdgeCaveat) []string {
	if c == nil {
		return nil
	}
	return []string{"caveat", string(c.Class), "caveat_message", c.Message}
}

// encodeFindUsages emits one row per usage edge. Each row names the
// caller symbol, its location, the edge kind, and the origin tier so
// agents can filter without a second call.
func encodeFindUsages(sg *query.SubGraph) ([]byte, error) {
	var buf bytes.Buffer
	meta := []string{"edges", fmt.Sprintf("%d", len(sg.Edges))}
	meta = append(meta, zeroEdgeCaveatMeta(sg.Caveat)...)
	enc := newGCX(&buf, "find_usages",
		[]string{"from", "to", "edge_kind", "origin", "tier", "confidence", "from_name", "from_path", "from_line", "from_is_test", "from_test_role", "from_test_runner"},
		meta...,
	)
	nodeIdx := indexNodes(sg.Nodes)
	for _, e := range sg.Edges {
		fn := nodeIdx[e.From]
		var fname, fpath string
		var fline int
		if fn != nil {
			fname = nodeShort(fn)
			fpath = fn.FilePath
			fline = fn.StartLine
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		if err := enc.WriteRow(
			e.From, e.To, string(e.Kind), e.Origin, tier, e.Confidence,
			fname, fpath, fline, nodeIsTest(fn), nodeTestRole(fn), nodeTestRunner(fn),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeSubGraph is a shared encoder for the edge-returning traversal
// tools (get_callers, get_call_chain, get_dependencies, get_dependents,
// find_implementations). Emits two sections — nodes then edges — plus a
// third caller_notes section when get_callers attached concurrency
// annotations. The third section is omitted entirely when empty so the
// other traversal tools' wire output is byte-identical to before.
func encodeSubGraph(tool string, sg *query.SubGraph) ([]byte, error) {
	var buf bytes.Buffer
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		nodes = append(nodes, n)
	}
	nodeEnc := newGCX(&buf, tool+".nodes",
		[]string{"id", "kind", "name", "path", "path_abs", "line", "is_test", "test_role", "test_runner"},
		"total", fmt.Sprintf("%d", sg.TotalNodes),
		"truncated", boolString(sg.Truncated),
	)
	for _, n := range nodes {
		if err := nodeEnc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.FilePath, n.AbsoluteFilePath, n.StartLine, nodeIsTest(n), nodeTestRole(n), nodeTestRunner(n)); err != nil {
			return nil, err
		}
	}
	if err := nodeEnc.Close(); err != nil {
		return nil, err
	}
	edgeMeta := []string{"count", fmt.Sprintf("%d", len(sg.Edges))}
	edgeMeta = append(edgeMeta, zeroEdgeCaveatMeta(sg.Caveat)...)
	edgeEnc := newGCX(&buf, tool+".edges",
		[]string{"from", "to", "kind", "origin", "tier", "confidence", "label"},
		edgeMeta...,
	)
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		if err := edgeEnc.WriteRow(e.From, e.To, string(e.Kind), e.Origin, tier, e.Confidence, label); err != nil {
			return nil, err
		}
	}
	if err := edgeEnc.Close(); err != nil {
		return nil, err
	}
	if len(sg.CallerNotes) == 0 {
		return buf.Bytes(), nil
	}
	// caller_notes — one row per caller carrying a concurrency flag.
	// Rows are sorted by node ID so the wire output is deterministic.
	noteIDs := make([]string, 0, len(sg.CallerNotes))
	for id := range sg.CallerNotes {
		noteIDs = append(noteIDs, id)
	}
	sort.Strings(noteIDs)
	noteEnc := newGCX(&buf, tool+".caller_notes",
		[]string{"id", "sync_guarded", "sync_guarded_why", "cross_concurrent", "cross_concurrent_why"},
		"count", fmt.Sprintf("%d", len(noteIDs)),
	)
	for _, id := range noteIDs {
		a := sg.CallerNotes[id]
		if err := noteEnc.WriteRow(id, a.SyncGuarded, a.SyncGuardedWhy, a.CrossConcurrent, a.CrossConcurrentWhy); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), noteEnc.Close()
}

// encodeFileSummary emits one row per symbol in a file plus a trailing
// edge-distribution comment.
func encodeFileSummary(sg *query.SubGraph, etag string) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "get_file_summary",
		[]string{"id", "kind", "name", "line", "sig"},
		"total_nodes", fmt.Sprintf("%d", sg.TotalNodes),
		"total_edges", fmt.Sprintf("%d", len(sg.Edges)),
		"truncated", boolString(sg.Truncated),
		"etag", etag,
	)
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		if err := enc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.StartLine, nodeSig(n)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeAnalyze routes to per-kind encoders. The `kind` field is
// placed in the meta header rather than per-row so consumers can
// dispatch without inspecting every row.
func encodeAnalyze(kind string, payload any) ([]byte, error) {
	var buf bytes.Buffer
	switch kind {
	case "dead_code":
		items, _ := payload.([]deadCodeItem)
		enc := newGCX(&buf, "analyze.dead_code",
			[]string{"id", "kind", "name", "path", "line", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Kind, it.Name, it.Path, it.Line, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "hotspots":
		items, _ := payload.([]hotspotItem)
		enc := newGCX(&buf, "analyze.hotspots",
			[]string{"id", "name", "path", "line", "fan_in", "fan_out", "cross_cut", "betweenness", "score"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Path, it.Line, it.FanIn, it.FanOut, it.CrossCommunity, it.Betweenness, it.Score); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "cycles":
		cycles, _ := payload.([]cycleItem)
		enc := newGCX(&buf, "analyze.cycles",
			[]string{"size", "severity", "nodes"},
			"count", fmt.Sprintf("%d", len(cycles)),
		)
		for _, c := range cycles {
			if err := enc.WriteRow(c.Size, c.Severity, strings.Join(c.Nodes, ",")); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "channel_ops":
		items, _ := payload.([]channelOpItem)
		enc := newGCX(&buf, "analyze.channel_ops",
			[]string{"channel", "sends", "recvs", "senders", "receivers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Channel, it.Sends, it.Recvs, it.Senders, it.Receivers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "goroutine_spawns":
		items, _ := payload.([]spawnItem)
		enc := newGCX(&buf, "analyze.goroutine_spawns",
			[]string{"target", "mode", "spawns", "spawners", "sync_guarded", "sync_guarded_why", "cross_concurrent", "cross_concurrent_why"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Target, it.Mode, it.Spawns, it.Spawners, it.SyncGuarded, it.SyncGuardedWhy, it.CrossConcurrent, it.CrossConcurrentWhy); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "field_writers":
		items, _ := payload.([]fieldWriterItem)
		enc := newGCX(&buf, "analyze.field_writers",
			[]string{"field", "writes", "writers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Field, it.Writes, it.Writers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "race_writes":
		items, _ := payload.([]raceWriteItem)
		enc := newGCX(&buf, "analyze.race_writes",
			[]string{"field", "writer", "file", "line", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Field, it.Writer, it.FilePath, it.Line, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "unclosed_channels":
		items, _ := payload.([]unclosedChannelItem)
		enc := newGCX(&buf, "analyze.unclosed_channels",
			[]string{"channel", "file", "line", "sends", "recvs", "senders", "risk", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Channel, it.FilePath, it.Line, it.Sends, it.Recvs, it.Senders, it.Risk, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "annotation_users":
		items, _ := payload.([]annotatedItem)
		enc := newGCX(&buf, "analyze.annotation_users",
			[]string{"symbol", "file", "line", "args"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.File, it.Line, it.Args); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "annotation_users.list":
		items, _ := payload.([]annotationItem)
		enc := newGCX(&buf, "analyze.annotation_users.list",
			[]string{"id", "name", "users"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Users); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "config_readers":
		items, _ := payload.([]configReaderItem)
		enc := newGCX(&buf, "analyze.config_readers",
			[]string{"id", "name", "source", "reads", "readers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Source, it.Reads, it.Readers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "event_emitters":
		items, _ := payload.([]eventEmitterItem)
		enc := newGCX(&buf, "analyze.event_emitters",
			[]string{"id", "name", "event_kind", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Kind, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "pubsub":
		items, _ := payload.([]pubsubItem)
		enc := newGCX(&buf, "analyze.pubsub",
			[]string{"id", "name", "transport", "publishes", "subscribes", "publishers", "subscribers"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Transport, it.Publishes, it.Subscribes, it.Publishers, it.Subscribers); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "string_emitters":
		items, _ := payload.([]stringEmitterItem)
		enc := newGCX(&buf, "analyze.string_emitters",
			[]string{"id", "context", "value", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Context, it.Value, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "error_surface":
		items, _ := payload.([]errorSurfaceItem)
		enc := newGCX(&buf, "analyze.error_surface",
			[]string{"symbol", "file", "line", "throws", "errors", "error_msgs"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.File, it.Line, it.Throws, it.Errors, it.ErrorMsgs); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "log_events":
		items, _ := payload.([]logEventItem)
		enc := newGCX(&buf, "analyze.log_events",
			[]string{"id", "value", "level", "emits", "emitters"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Value, it.Level, it.Emits, it.Emitters); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "sql_rebuild":
		items, _ := payload.([]sqlRebuildItem)
		enc := newGCX(&buf, "analyze.sql_rebuild",
			[]string{"strings_visited", "tables_created", "columns_created", "query_edges", "reads_col_edges", "writes_col_edges", "emitters_linked", "skipped"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.StringsVisited, it.TablesCreated, it.ColumnsCreated, it.QueryEdges, it.ReadColEdges, it.WriteColEdges, it.EmittersLinked, it.Skipped); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "cross_repo":
		items, _ := payload.([]crossRepoItem)
		enc := newGCX(&buf, "analyze.cross_repo",
			[]string{"from_repo", "to_repo", "kind", "count", "samples"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.FromRepo, it.ToRepo, it.Kind, it.Count, it.Samples); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "unsafe_patterns":
		items, _ := payload.([]unsafePatternItem)
		enc := newGCX(&buf, "analyze.unsafe_patterns",
			[]string{"detector", "severity", "language", "file", "line", "symbol", "text"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Detector, it.Severity, it.Language, it.File, it.Line, it.Symbol, it.Text); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "health_score":
		items, _ := payload.([]healthScoreItem)
		enc := newGCX(&buf, "analyze.health_score",
			[]string{
				"id", "name", "kind", "file", "line",
				"score", "grade",
				"coverage_pct", "complexity_pct", "recency_pct", "churn_pct",
				"fan_in", "fan_out", "crossings",
				"age_days", "mods", "axes_used",
			},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(
				it.ID, it.Name, it.Kind, it.File, it.Line,
				it.Score, it.Grade,
				it.CoveragePct, it.ComplexityPct, it.RecencyPct, it.ChurnPct,
				it.FanIn, it.FanOut, it.Crossings,
				it.AgeDays, it.Mods, it.AxesUsed,
			); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "health_score.rollup":
		items, _ := payload.([]healthRollupItem)
		enc := newGCX(&buf, "analyze.health_score.rollup",
			[]string{
				"scope", "key",
				"avg_score", "min_score", "max_score",
				"symbols", "grade",
				"count_a", "count_b", "count_c", "count_d", "count_f",
			},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(
				it.Scope, it.Key,
				it.AvgScore, it.MinScore, it.MaxScore,
				it.Symbols, it.Grade,
				it.CountA, it.CountB, it.CountC, it.CountD, it.CountF,
			); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	default:
		// Fall back to generic so analyze variants without a hand-tuned
		// encoder still produce valid GCX instead of failing.
		if err := wire.EncodeAny(&buf, "analyze."+kind, payload); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// Narrow row contracts used by analyze.* encoders so the caller can
// build them from heterogeneous internal structs.
type deadCodeItem struct {
	ID, Kind, Name, Path, Reason string
	Line                         int
}

type hotspotItem struct {
	ID, Name, Path string
	Line, FanIn    int
	FanOut         int
	CrossCommunity int
	Betweenness    float64
	Score          float64
}

type cycleItem struct {
	Size     int
	Severity string
	Nodes    []string
}

// Row contracts for the edge-driven analyzers (see
// tools_analyze_edges.go). Values are pre-flattened (slices joined)
// so the GCX encoder writes one scalar column per field.
type channelOpItem struct {
	Channel   string
	Sends     int
	Recvs     int
	Senders   string
	Receivers string
}

type spawnItem struct {
	Target string
	Mode   string
	Spawns int
	// Spawners is a comma-joined list of caller node IDs.
	Spawners string
	// Concurrency columns carry the shared classification of the
	// spawned target — sync_guarded (goroutine body on a lock-holding
	// type) and cross_concurrent (reached across a concurrency
	// boundary). Empty when the target carries neither flag.
	SyncGuarded        bool
	SyncGuardedWhy     string
	CrossConcurrent    bool
	CrossConcurrentWhy string
}

type fieldWriterItem struct {
	Field   string
	Writes  int
	Writers string
}

// raceWriteItem is one row of the `race_writes` analyzer: a field
// write that fires from inside a goroutine-reachable function with
// no detected lock guard.
type raceWriteItem struct {
	Field    string
	Writer   string
	FilePath string
	Line     int
	Reason   string
}

// unclosedChannelItem is one row of the `unclosed_channels`
// analyzer: a channel that takes sends but nobody (sender or
// receiver) calls close() on.
type unclosedChannelItem struct {
	Channel  string
	FilePath string
	Line     int
	Sends    int
	Recvs    int
	Senders  int
	Risk     string
	Reason   string
}

type annotatedItem struct {
	Symbol string
	File   string
	Line   int
	Args   string
}

type annotationItem struct {
	ID    string
	Name  string
	Users int
}

type configReaderItem struct {
	ID      string
	Name    string
	Source  string
	Reads   int
	Readers string
}

type eventEmitterItem struct {
	ID       string
	Name     string
	Kind     string
	Emits    int
	Emitters string
}

type pubsubItem struct {
	ID          string
	Name        string
	Transport   string
	Publishes   int
	Subscribes  int
	Publishers  string
	Subscribers string
}

type errorSurfaceItem struct {
	Symbol    string
	File      string
	Line      int
	Throws    int
	Errors    string
	ErrorMsgs string
}

type logEventItem struct {
	ID       string
	Value    string
	Level    string
	Emits    int
	Emitters string
}

type sqlRebuildItem struct {
	StringsVisited int
	TablesCreated  int
	ColumnsCreated int
	QueryEdges     int
	ReadColEdges   int
	WriteColEdges  int
	EmittersLinked int
	Skipped        int
}

type crossRepoItem struct {
	FromRepo string
	ToRepo   string
	Kind     string
	Count    int
	Samples  string
}

// unsafePatternItem is one row of `analyze kind=unsafe_patterns`:
// a single match from one of the bundled unsafe-pattern detectors.
type unsafePatternItem struct {
	Detector string
	Severity string
	Language string
	File     string
	Line     int
	Symbol   string
	Text     string
}

// healthScoreItem is one row of `analyze kind=health_score`: a
// per-symbol composite health value with the per-axis breakdown.
// "_pct" fields are the 0..100 axis values; NaN encodes "no data
// for this axis" (the GCX encoder serializes NaN as empty so a
// downstream reader can distinguish missing from zero).
type healthScoreItem struct {
	ID            string
	Name          string
	Kind          string
	File          string
	Line          int
	Score         float64
	Grade         string
	CoveragePct   float64
	ComplexityPct float64
	RecencyPct    float64
	ChurnPct      float64
	FanIn         int
	FanOut        int
	Crossings     int
	AgeDays       int
	Mods          int
	AxesUsed      int
}

// healthRollupItem is one row of `analyze kind=health_score` when
// `roll_up` aggregates the per-symbol scores up to file or repo
// scope.
type healthRollupItem struct {
	Scope    string
	Key      string
	AvgScore float64
	MinScore float64
	MaxScore float64
	Symbols  int
	Grade    string
	CountA   int
	CountB   int
	CountC   int
	CountD   int
	CountF   int
}

// --------------------------------------------------------------------
// contracts
// --------------------------------------------------------------------

// contractFields is the fixed column layout for one contract row.
// method + path are promoted out of Meta so HTTP/gRPC filters don't
// have to parse the compact meta column.
var contractFields = []string{
	"type", "role", "repo", "method", "path",
	"file", "line", "id", "symbol_id", "confidence", "meta",
}

// writeContractRow emits one contract using contractFields.
func writeContractRow(enc *wire.Encoder, c contracts.Contract) error {
	method, _ := c.Meta["method"].(string)
	path, _ := c.Meta["path"].(string)
	return enc.WriteRow(
		string(c.Type), string(c.Role), c.RepoPrefix, method, path,
		c.FilePath, c.Line, c.ID, c.SymbolID, c.Confidence,
		formatContractMeta(c.Meta, "method", "path"),
	)
}

// formatContractMeta renders Meta as a stable semicolon-separated k=v
// list, dropping excluded keys already promoted to dedicated columns.
func formatContractMeta(m map[string]any, exclude ...string) string {
	if len(m) == 0 {
		return ""
	}
	skip := make(map[string]bool, len(exclude))
	for _, k := range exclude {
		skip[k] = true
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		fmt.Fprintf(&b, "%s=%v", k, m[k])
	}
	return b.String()
}

// encodeContractsList emits one row per contract. The by_repo grouping
// from the JSON payload is flattened — rows carry repo in-band so
// consumers can regroup without walking a tree.
func encodeContractsList(rows []contracts.Contract, total int, extraMeta ...string) ([]byte, error) {
	var buf bytes.Buffer
	meta := append([]string{"total", fmt.Sprintf("%d", total)}, extraMeta...)
	enc := newGCX(&buf, "contracts.list", contractFields, meta...)
	if err := enc.WriteComment(fmt.Sprintf("%d contract(s)", len(rows))); err != nil {
		return nil, err
	}
	for _, c := range rows {
		if err := writeContractRow(enc, c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeContractsCheck emits three sections: matched pairs, orphan
// providers, orphan consumers. Orphan sections reuse contractFields.
func encodeContractsCheck(result contracts.MatchResult) ([]byte, error) {
	var buf bytes.Buffer

	matchedFields := []string{
		"contract_id", "cross_repo",
		"provider_repo", "provider_file", "provider_line", "provider_symbol",
		"consumer_repo", "consumer_file", "consumer_line", "consumer_symbol",
	}
	matchedEnc := newGCX(&buf, "contracts.check.matched", matchedFields,
		"count", fmt.Sprintf("%d", len(result.Matched)),
	)
	for _, m := range result.Matched {
		if err := matchedEnc.WriteRow(
			m.ContractID, m.CrossRepo,
			m.Provider.RepoPrefix, m.Provider.FilePath, m.Provider.Line, m.Provider.SymbolID,
			m.Consumer.RepoPrefix, m.Consumer.FilePath, m.Consumer.Line, m.Consumer.SymbolID,
		); err != nil {
			return nil, err
		}
	}
	if err := matchedEnc.Close(); err != nil {
		return nil, err
	}

	provEnc := newGCX(&buf, "contracts.check.orphan_providers", contractFields,
		"count", fmt.Sprintf("%d", len(result.OrphanProviders)),
	)
	for _, c := range result.OrphanProviders {
		if err := writeContractRow(provEnc, c); err != nil {
			return nil, err
		}
	}
	if err := provEnc.Close(); err != nil {
		return nil, err
	}

	consEnc := newGCX(&buf, "contracts.check.orphan_consumers", contractFields,
		"count", fmt.Sprintf("%d", len(result.OrphanConsumers)),
	)
	for _, c := range result.OrphanConsumers {
		if err := writeContractRow(consEnc, c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), consEnc.Close()
}

// --------------------------------------------------------------------
// get_editing_context
// --------------------------------------------------------------------

// encodeEditingContext emits four sections: defines, imports,
// called_by, calls. File metadata (id, language, etag) lives in the
// defines-section header so there is no single-row wrapper section.
func encodeEditingContext(file map[string]any, defines, imports, calledBy, calls []map[string]any, etag string) ([]byte, error) {
	var buf bytes.Buffer

	// file_id (the path) is not echoed in the header: paths on macOS /
	// Windows can contain spaces, and the GCX header tokeniser splits on
	// raw spaces. Callers can recover the file path from any defines
	// row's ID prefix (`<path>::<symbol>`).
	var language string
	if v, ok := file["language"]; ok {
		language = fmt.Sprint(v)
	}

	defEnc := newGCX(&buf, "get_editing_context.defines",
		[]string{"id", "kind", "name", "line", "sig"},
		"etag", etag,
		"language", language,
		"count", fmt.Sprintf("%d", len(defines)),
	)
	for _, d := range defines {
		if err := defEnc.WriteRow(
			str(d["id"]),
			str(d["kind"]),
			str(d["name"]),
			d["start_line"],
			str(d["signature"]),
		); err != nil {
			return nil, err
		}
	}
	if err := defEnc.Close(); err != nil {
		return nil, err
	}

	impEnc := newGCX(&buf, "get_editing_context.imports",
		[]string{"id", "external"},
		"count", fmt.Sprintf("%d", len(imports)),
	)
	for _, im := range imports {
		if err := impEnc.WriteRow(str(im["id"]), im["external"]); err != nil {
			return nil, err
		}
	}
	if err := impEnc.Close(); err != nil {
		return nil, err
	}

	cbEnc := newGCX(&buf, "get_editing_context.called_by",
		[]string{"id", "name", "path", "line"},
		"count", fmt.Sprintf("%d", len(calledBy)),
	)
	for _, c := range calledBy {
		if err := cbEnc.WriteRow(str(c["id"]), str(c["name"]), str(c["file_path"]), c["start_line"]); err != nil {
			return nil, err
		}
	}
	if err := cbEnc.Close(); err != nil {
		return nil, err
	}

	callEnc := newGCX(&buf, "get_editing_context.calls",
		[]string{"id", "name", "path", "line"},
		"count", fmt.Sprintf("%d", len(calls)),
	)
	for _, c := range calls {
		if err := callEnc.WriteRow(str(c["id"]), str(c["name"]), str(c["file_path"]), c["start_line"]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), callEnc.Close()
}

// --------------------------------------------------------------------
// smart_context
// --------------------------------------------------------------------

// encodeSmartContext emits up to seven sections — symbols, cross_repo,
// entry_file, callers, callees, tests, files — in the order the
// orchestrator populates them. Only non-empty sections are written so
// small tasks stay small. Each section has its own field list to
// avoid wide rows padded with empty cells.
func encodeSmartContext(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	// The task string is not echoed in the header: the GCX header tokeniser
	// splits on spaces and escape() doesn't escape them, so a free-text
	// task would corrupt parsing. The caller already knows what they
	// asked for — no need to round-trip it.

	symbols, _ := result["relevant_symbols"].([]map[string]any)
	symEnc := newGCX(&buf, "smart_context.symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "source"},
		"count", fmt.Sprintf("%d", len(symbols)),
	)
	for _, s := range symbols {
		if err := symEnc.WriteRow(
			str(s["id"]),
			str(s["kind"]),
			str(s["name"]),
			str(s["file_path"]),
			s["start_line"],
			str(s["signature"]),
			str(s["source"]),
		); err != nil {
			return nil, err
		}
	}
	if err := symEnc.Close(); err != nil {
		return nil, err
	}

	if crossRepo, ok := result["cross_repo_dependencies"].([]map[string]any); ok && len(crossRepo) > 0 {
		enc := newGCX(&buf, "smart_context.cross_repo",
			[]string{"id", "kind", "name", "path", "repo", "edge_kind", "sig"},
			"count", fmt.Sprintf("%d", len(crossRepo)),
		)
		for _, d := range crossRepo {
			if err := enc.WriteRow(
				str(d["id"]),
				str(d["kind"]),
				str(d["name"]),
				str(d["file_path"]),
				str(d["repo_prefix"]),
				str(d["edge_kind"]),
				str(d["signature"]),
			); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if entryFile, ok := result["entry_file_symbols"].([]string); ok && len(entryFile) > 0 {
		enc := newGCX(&buf, "smart_context.entry_file",
			[]string{"desc"},
			"count", fmt.Sprintf("%d", len(entryFile)),
		)
		for _, d := range entryFile {
			if err := enc.WriteRow(d); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if callers, ok := result["callers"].([]string); ok && len(callers) > 0 {
		enc := newGCX(&buf, "smart_context.callers",
			[]string{"id"},
			"count", fmt.Sprintf("%d", len(callers)),
		)
		for _, id := range callers {
			if err := enc.WriteRow(id); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if callees, ok := result["callees"].([]string); ok && len(callees) > 0 {
		enc := newGCX(&buf, "smart_context.callees",
			[]string{"id"},
			"count", fmt.Sprintf("%d", len(callees)),
		)
		for _, id := range callees {
			if err := enc.WriteRow(id); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if tests, ok := result["related_test_files"].([]string); ok && len(tests) > 0 {
		enc := newGCX(&buf, "smart_context.tests",
			[]string{"path"},
			"count", fmt.Sprintf("%d", len(tests)),
		)
		for _, p := range tests {
			if err := enc.WriteRow(p); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	if files, ok := result["files_to_edit"].([]string); ok && len(files) > 0 {
		enc := newGCX(&buf, "smart_context.files",
			[]string{"path"},
			"count", fmt.Sprintf("%d", len(files)),
		)
		for _, p := range files {
			if err := enc.WriteRow(p); err != nil {
				return nil, err
			}
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// --------------------------------------------------------------------
// prefetch_context, explain_change_impact, check_guards, feedback.query
// --------------------------------------------------------------------

// encodePrefetchContext emits one row per ranked candidate with the
// per-axis score contributions inlined (search/proximity/community)
// so consumers can recover the attribution without a nested object.
// Source — when include_source was set on the request — rides as the
// last field, escaped via the wire encoder so newlines stay one row.
func encodePrefetchContext(candidates []prefetchCandidate, total int, truncated bool, includeSource bool) ([]byte, error) {
	fields := []string{"id", "kind", "name", "path", "line", "confidence", "search", "proximity", "community", "reason"}
	if includeSource {
		fields = append(fields, "source")
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "prefetch_context",
		fields,
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d candidate(s)", len(candidates))); err != nil {
		return nil, err
	}
	for _, c := range candidates {
		row := []any{
			c.ID,
			c.Kind,
			nodeShort(c.Node),
			c.FilePath,
			c.StartLine,
			roundFloat(c.Confidence),
			roundFloat(c.SearchRelevance),
			roundFloat(c.GraphProximity),
			roundFloat(c.CommunityBonus),
			c.Reason,
		}
		if includeSource {
			row = append(row, c.Source)
		}
		if err := enc.WriteRow(row...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeChangeImpact emits a multi-section envelope for the
// explain_change_impact tool. Section layout:
//
//   - explain_change_impact.summary (one row): risk, total, communities/processes
//     joined with `,` so the section stays one row regardless of fan-out.
//   - explain_change_impact.entries (one row per affected symbol): the
//     by_depth + by_repo maps are flattened into a single table — `repo`
//     is empty unless the change crosses repos.
//   - explain_change_impact.contracts (one row when contract_impact is set):
//     breaking/warning/info counters + the validate-pass risk upgrade flag.
func encodeChangeImpact(result map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	risk, _ := result["risk"].(analysis.RiskLevel)
	summaryStr, _ := result["summary"].(string)
	totalAffected, _ := result["total_affected"].(int)
	crossRepo, _ := result["cross_repo_impact"].(bool)
	processes, _ := result["affected_processes"].([]string)
	communities, _ := result["affected_communities"].([]string)
	testFiles, _ := result["test_files"].([]string)
	warning, _ := result["cross_community_warning"].(string)
	communityNote, _ := result["community_note"].(string)
	contractRiskUpgrade, _ := result["contract_risk_upgrade"].(string)

	sumEnc := newGCX(&buf, "explain_change_impact.summary",
		[]string{"risk", "summary", "total_affected", "cross_repo", "processes", "communities", "test_files", "warning", "note", "risk_upgrade"},
	)
	if err := sumEnc.WriteRow(
		string(risk),
		summaryStr,
		totalAffected,
		boolString(crossRepo),
		strings.Join(processes, ","),
		strings.Join(communities, ","),
		strings.Join(testFiles, ","),
		warning,
		communityNote,
		contractRiskUpgrade,
	); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	// Flatten by_depth (and by_repo when cross-repo) into a single
	// rows section. We carry depth + repo so consumers can regroup.
	entryFields := []string{"depth", "repo", "id", "name", "kind", "path", "line", "edge_confidence", "confidence_label"}
	entryEnc := newGCX(&buf, "explain_change_impact.entries", entryFields)
	totalRows := 0
	if byDepth, ok := result["by_depth"].(map[int][]analysis.ImpactEntry); ok {
		// Sort depths so output is deterministic.
		depths := make([]int, 0, len(byDepth))
		for d := range byDepth {
			depths = append(depths, d)
		}
		sort.Ints(depths)
		for _, d := range depths {
			for _, e := range byDepth[d] {
				if err := entryEnc.WriteRow(
					d,
					e.RepoPrefix,
					e.ID,
					e.Name,
					e.Kind,
					e.FilePath,
					e.Line,
					roundFloat(e.EdgeConfidence),
					e.ConfidenceLabel,
				); err != nil {
					return nil, err
				}
				totalRows++
			}
		}
	}
	if err := entryEnc.Close(); err != nil {
		return nil, err
	}

	if ci, ok := result["contract_impact"].(*contractImpact); ok && ci != nil {
		ciEnc := newGCX(&buf, "explain_change_impact.contracts",
			[]string{"breaking", "warning", "info", "affected"},
		)
		if err := ciEnc.WriteRow(ci.Breaking, ci.Warning, ci.Info, len(ci.Affected)); err != nil {
			return nil, err
		}
		if err := ciEnc.Close(); err != nil {
			return nil, err
		}
	}

	// zero_impact_caveat — one row per input symbol whose empty blast
	// radius could not be told apart from an extraction gap.
	if caveats, ok := result["zero_impact_caveat"].([]graph.ZeroImpactCaveat); ok && len(caveats) > 0 {
		cavEnc := newGCX(&buf, "explain_change_impact.caveats",
			[]string{"id", "class", "message"},
		)
		for _, c := range caveats {
			if err := cavEnc.WriteRow(c.ID, string(c.Class), c.Message); err != nil {
				return nil, err
			}
		}
		if err := cavEnc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// encodeCheckGuards emits one row per guard violation. The empty case
// (no rules / no violations) still produces a valid envelope so agents
// can rely on the GCX1 header to differentiate from an error payload —
// the meta `status=no_rules_configured` flag carries the "no rules"
// hint as a single token (the decoder splits the header on raw spaces,
// so multi-word meta values must stay tokenised).
func encodeCheckGuards(violations []analysis.GuardViolation, noRulesConfigured bool) ([]byte, error) {
	var buf bytes.Buffer
	meta := []string{"total", fmt.Sprintf("%d", len(violations))}
	if noRulesConfigured {
		meta = append(meta, "status", "no_rules_configured")
	}
	enc := newGCX(&buf, "check_guards",
		[]string{"rule_name", "kind", "description"},
		meta...,
	)
	for _, v := range violations {
		if err := enc.WriteRow(v.RuleName, v.Kind, v.Description); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeFeedbackQuery emits a multi-section envelope:
//
//   - feedback.summary (one row): total_entries, accuracy
//   - feedback.most_useful, feedback.most_missed, feedback.most_demoted
//     (one row per ranked symbol): id, score, count
//
// Empty lists still emit their section so consumers can iterate without
// branching on existence — the meta count is the source of truth.
func encodeFeedbackQuery(stats map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	totalEntries := 0
	if v, ok := stats["total_entries"].(int); ok {
		totalEntries = v
	}
	accuracy := 0.0
	if v, ok := stats["accuracy"].(float64); ok {
		accuracy = v
	}
	sumEnc := newGCX(&buf, "feedback.summary",
		[]string{"total_entries", "accuracy"},
	)
	if err := sumEnc.WriteRow(totalEntries, roundFloat(accuracy)); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	emit := func(section string, key string) error {
		enc := newGCX(&buf, section,
			[]string{"id", "score", "count"},
		)
		// AggregatedStats produces an unexported `ranked` slice we
		// can only see through reflection. Coerce via JSON shape:
		// the published schema is {id, score, count}, so we read
		// it through a generic any-decoder.
		rows, _ := stats[key].([]any)
		if rows == nil {
			// Strongly-typed path — re-marshal whatever AggregatedStats
			// returned and decode into a slice of {id, score, count}.
			if raw, err := json.Marshal(stats[key]); err == nil {
				var coerced []struct {
					ID    string  `json:"id"`
					Score float64 `json:"score"`
					Count int     `json:"count"`
				}
				_ = json.Unmarshal(raw, &coerced)
				for _, r := range coerced {
					if err := enc.WriteRow(r.ID, roundFloat(r.Score), r.Count); err != nil {
						return err
					}
				}
				return enc.Close()
			}
		}
		for _, row := range rows {
			m, _ := row.(map[string]any)
			id, _ := m["id"].(string)
			score, _ := m["score"].(float64)
			count := 0
			if v, ok := m["count"].(int); ok {
				count = v
			} else if v, ok := m["count"].(float64); ok {
				count = int(v)
			}
			if err := enc.WriteRow(id, roundFloat(score), count); err != nil {
				return err
			}
		}
		return enc.Close()
	}
	if err := emit("feedback.most_useful", "most_useful"); err != nil {
		return nil, err
	}
	if err := emit("feedback.most_missed", "most_missed"); err != nil {
		return nil, err
	}
	if err := emit("feedback.most_demoted", "most_demoted"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --------------------------------------------------------------------
// small utilities
// --------------------------------------------------------------------

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func indexNodes(nodes []*graph.Node) map[string]*graph.Node {
	m := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}
