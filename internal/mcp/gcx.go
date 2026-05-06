package mcp

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// isGCX reports whether the caller requested the GCX1 compact wire
// format. The selection order mirrors ParseFormat: the `format` arg
// wins, otherwise legacy `compact: true` falls through to text (not
// GCX), and the absence of either keeps JSON as the default.
func isGCX(req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok {
		return wire.ParseFormat(v) == wire.FormatGCX
	}
	return false
}

// gcxResponse wraps a GCX byte payload into an MCP text-result. If the
// encoder returned an error, the caller gets a structured MCP error
// result instead of a half-written payload.
func gcxResponse(payload []byte, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("wire encode failed: %v", err)), nil
	}
	return mcp.NewToolResultText(string(payload)), nil
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
func encodeWinnowSymbols(rows []winnowResult, total, limit int) ([]byte, error) {
	truncated := total > limit
	var buf bytes.Buffer
	enc := newGCX(&buf, "winnow_symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "score", "fan_in", "fan_out", "churn", "community", "contributions"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
		"weights", formatAxisWeights(winnowAxisWeights),
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
		[]string{"id", "kind", "name", "path", "line", "sig"},
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
			n.StartLine,
			nodeSig(n),
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

// encodeFindUsages emits one row per usage edge. Each row names the
// caller symbol, its location, the edge kind, and the origin tier so
// agents can filter without a second call.
func encodeFindUsages(sg *query.SubGraph) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "find_usages",
		[]string{"from", "to", "edge_kind", "origin", "confidence", "from_name", "from_path", "from_line"},
		"edges", fmt.Sprintf("%d", len(sg.Edges)),
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
		if err := enc.WriteRow(
			e.From, e.To, string(e.Kind), e.Origin, e.Confidence,
			fname, fpath, fline,
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeSubGraph is a shared encoder for the edge-returning traversal
// tools (get_callers, get_call_chain, get_dependencies, get_dependents,
// find_implementations). Emits two sections: nodes then edges.
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
		[]string{"id", "kind", "name", "path", "line"},
		"total", fmt.Sprintf("%d", sg.TotalNodes),
		"truncated", boolString(sg.Truncated),
	)
	for _, n := range nodes {
		if err := nodeEnc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.FilePath, n.StartLine); err != nil {
			return nil, err
		}
	}
	if err := nodeEnc.Close(); err != nil {
		return nil, err
	}
	edgeEnc := newGCX(&buf, tool+".edges",
		[]string{"from", "to", "kind", "origin", "confidence", "label"},
		"count", fmt.Sprintf("%d", len(sg.Edges)),
	)
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		if err := edgeEnc.WriteRow(e.From, e.To, string(e.Kind), e.Origin, e.Confidence, label); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), edgeEnc.Close()
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
			[]string{"id", "name", "path", "line", "fan_in", "fan_out", "cross_cut", "score"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Path, it.Line, it.FanIn, it.FanOut, it.CrossCommunity, it.Score); err != nil {
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
			[]string{"target", "mode", "spawns", "spawners"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Target, it.Mode, it.Spawns, it.Spawners); err != nil {
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
			[]string{"symbol", "file", "line", "throws", "errors"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.Symbol, it.File, it.Line, it.Throws, it.Errors); err != nil {
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
	Target   string
	Mode     string
	Spawns   int
	Spawners string
}

type fieldWriterItem struct {
	Field   string
	Writes  int
	Writers string
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

type errorSurfaceItem struct {
	Symbol string
	File   string
	Line   int
	Throws int
	Errors string
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
