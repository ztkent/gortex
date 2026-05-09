// Edge-driven analyzers built on top of the coverage parser
// extensions: channel send/recv, goroutine spawns, field writes,
// annotations, config readers, event emitters, error throws. Each
// walks one (kind -> edge kind) family and groups call sites by
// target so producer/consumer mismatches and hotspots become a
// graph query rather than a grep run.
//
// Conventions match the older analyzers in tools_enhancements.go:
//   - typed `<kind>Row` struct, JSON output by default,
//   - optional `compact: true` for one-line text,
//   - optional `format: "gcx"` for the GCX1 wire format,
//   - stable sort orders so diffs are predictable across runs.
//
// Filters are intentionally narrow: each analyzer surfaces the
// dimensions the parser actually emits in meta. Adding new filters
// means new meta keys upstream; we don't fabricate dimensions
// here.
package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// ---------------------------------------------------------------------------
// channel_ops — list channels with their senders/receivers.
// ---------------------------------------------------------------------------

// handleAnalyzeChannelOps walks every EdgeSends and EdgeRecvs edge
// and groups by channel target. The resulting rows surface
// producer/consumer mismatches (channels with sends but no
// receivers, or vice-versa) and concurrency hotspots (channels with
// many senders or receivers fanning across the codebase).
//
// Channels in v1 are extracted as `unresolved::<name>` targets
// because Go's tree-sitter pass doesn't propagate channel typing.
// The analyzer therefore reports the synthetic target id, not a
// fully-resolved channel symbol. That's the same fidelity
// `find_usages` would give a caller today and lets the rows be
// diff-able across runs.
//
// Filters:
//
//   - path_prefix: scope to operations originating in a directory
//     subtree. Useful for "channel discipline in package X".
func (s *Server) handleAnalyzeChannelOps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(stringArg(req.GetArguments(), "path_prefix"))

	type channelRow struct {
		Channel   string   `json:"channel"`
		Sends     int      `json:"sends"`
		Recvs     int      `json:"recvs"`
		Senders   []string `json:"senders,omitempty"`
		Receivers []string `json:"receivers,omitempty"`
	}
	byChannel := map[string]*channelRow{}
	get := func(target string) *channelRow {
		row, ok := byChannel[target]
		if !ok {
			row = &channelRow{Channel: target}
			byChannel[target] = row
		}
		return row
	}

	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeSends && e.Kind != graph.EdgeRecvs {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		row := get(e.To)
		if e.Kind == graph.EdgeSends {
			row.Sends++
			row.Senders = appendUnique(row.Senders, e.From)
		} else {
			row.Recvs++
			row.Receivers = appendUnique(row.Receivers, e.From)
		}
	}

	rows := make([]*channelRow, 0, len(byChannel))
	for _, r := range byChannel {
		sort.Strings(r.Senders)
		sort.Strings(r.Receivers)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		// Total op count desc; tie-break by channel id for stability.
		ti := rows[i].Sends + rows[i].Recvs
		tj := rows[j].Sends + rows[j].Recvs
		if ti != tj {
			return ti > tj
		}
		return rows[i].Channel < rows[j].Channel
	})

	if s.isGCX(ctx, req) {
		items := make([]channelOpItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, channelOpItem{
				Channel:   r.Channel,
				Sends:     r.Sends,
				Recvs:     r.Recvs,
				Senders:   strings.Join(r.Senders, ","),
				Receivers: strings.Join(r.Receivers, ","),
			})
		}
		return gcxResponse(encodeAnalyze("channel_ops", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "send=%d recv=%d  %s\n", r.Sends, r.Recvs, r.Channel)
		}
		if len(rows) == 0 {
			b.WriteString("no channel ops\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"channels": rows,
		"total":    len(rows),
	})
}

// ---------------------------------------------------------------------------
// goroutine_spawns — list spawn sites grouped by spawned target.
// ---------------------------------------------------------------------------

// handleAnalyzeGoroutineSpawns walks every EdgeSpawns edge and
// groups by spawned target. The mode meta (goroutine / async /
// promise / worker_pool) is surfaced verbatim so cross-language
// concurrency hotspots stay separable. Useful for spotting leaks
// (a single function with many spawn sites), unowned background
// work, and codebase-wide concurrency hygiene reviews.
func (s *Server) handleAnalyzeGoroutineSpawns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type spawnRow struct {
		Target    string   `json:"target"`
		Mode      string   `json:"mode,omitempty"`
		Spawns    int      `json:"spawns"`
		Spawners  []string `json:"spawners,omitempty"`
	}
	byTarget := map[string]*spawnRow{}

	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeSpawns {
			continue
		}
		mode, _ := e.Meta["mode"].(string)
		key := e.To + "|" + mode
		row, ok := byTarget[key]
		if !ok {
			row = &spawnRow{Target: e.To, Mode: mode}
			byTarget[key] = row
		}
		row.Spawns++
		row.Spawners = appendUnique(row.Spawners, e.From)
	}

	rows := make([]*spawnRow, 0, len(byTarget))
	for _, r := range byTarget {
		sort.Strings(r.Spawners)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Spawns != rows[j].Spawns {
			return rows[i].Spawns > rows[j].Spawns
		}
		if rows[i].Target != rows[j].Target {
			return rows[i].Target < rows[j].Target
		}
		return rows[i].Mode < rows[j].Mode
	})

	if s.isGCX(ctx, req) {
		items := make([]spawnItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, spawnItem{
				Target:   r.Target,
				Mode:     r.Mode,
				Spawns:   r.Spawns,
				Spawners: strings.Join(r.Spawners, ","),
			})
		}
		return gcxResponse(encodeAnalyze("goroutine_spawns", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			mode := r.Mode
			if mode == "" {
				mode = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Spawns, mode, r.Target)
		}
		if len(rows) == 0 {
			b.WriteString("no spawn sites\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"spawns": rows,
		"total":  len(rows),
	})
}

// ---------------------------------------------------------------------------
// field_writers — for a given field id (or top-N most-written
// fields when no id given), list writer functions.
// ---------------------------------------------------------------------------

// handleAnalyzeFieldWriters walks EdgeWrites edges with a field
// target and groups by field. With `id` the analyzer scopes to one
// field — useful for "who mutates Server.config?". Without `id` it
// surfaces the top-N most-written fields, the mutability hotspot
// view.
//
// Filters:
//
//   - id: a specific field node id. When set, only that field is
//     reported. Useful for targeted review of a single field's
//     write surface.
//   - limit: max rows when no id is set (default 20).
func (s *Server) handleAnalyzeFieldWriters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type writerRow struct {
		Field   string   `json:"field"`
		Writes  int      `json:"writes"`
		Writers []string `json:"writers,omitempty"`
	}
	byField := map[string]*writerRow{}

	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeWrites {
			continue
		}
		if idFilter != "" && e.To != idFilter {
			continue
		}
		// Only count writes whose target resolves to a field node.
		// Pre-resolution edges land on `unresolved::*.foo` and would
		// muddy the per-field rollup; the resolver post-pass
		// rewrites the To, so any unresolved edges left at query
		// time are a different problem.
		if idFilter == "" {
			target := s.graph.GetNode(e.To)
			if target == nil || target.Kind != graph.KindField {
				continue
			}
		}
		row, ok := byField[e.To]
		if !ok {
			row = &writerRow{Field: e.To}
			byField[e.To] = row
		}
		row.Writes++
		row.Writers = appendUnique(row.Writers, e.From)
	}

	rows := make([]*writerRow, 0, len(byField))
	for _, r := range byField {
		sort.Strings(r.Writers)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Writes != rows[j].Writes {
			return rows[i].Writes > rows[j].Writes
		}
		return rows[i].Field < rows[j].Field
	})
	truncated := false
	if idFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]fieldWriterItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, fieldWriterItem{
				Field:   r.Field,
				Writes:  r.Writes,
				Writers: strings.Join(r.Writers, ","),
			})
		}
		return gcxResponse(encodeAnalyze("field_writers", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s\n", r.Writes, r.Field)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no field writes\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	resp := map[string]any{
		"fields":    rows,
		"total":     len(rows),
		"truncated": truncated,
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// annotation_users — for an annotation node id (or list all when no
// id), surface annotated symbols.
// ---------------------------------------------------------------------------

// handleAnalyzeAnnotationUsers walks EdgeAnnotated edges. With `id`
// the analyzer scopes to one annotation target — "every symbol
// annotated `@Deprecated`". Without `id` it lists every distinct
// annotation found, with annotated count, so the agent can decide
// which one to dive into. The `name` filter narrows the
// annotation-name match without requiring a synthetic id.
//
// Filters:
//
//   - id: annotation node id (e.g. `annotation::java::Deprecated`).
//     Returns one row per annotated symbol.
//   - name: annotation bare name (case-insensitive). Returns one
//     row per matching annotation grouped by id.
func (s *Server) handleAnalyzeAnnotationUsers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	idFilter := strings.TrimSpace(stringArg(args, "id"))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	if idFilter != "" {
		type annotatedRow struct {
			Symbol string `json:"symbol"`
			File   string `json:"file"`
			Line   int    `json:"line"`
			Args   string `json:"args,omitempty"`
		}
		var rows []annotatedRow
		for _, e := range s.graph.AllEdges() {
			if e.Kind != graph.EdgeAnnotated || e.To != idFilter {
				continue
			}
			argsStr, _ := e.Meta["args"].(string)
			rows = append(rows, annotatedRow{
				Symbol: e.From,
				File:   e.FilePath,
				Line:   e.Line,
				Args:   argsStr,
			})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].File != rows[j].File {
				return rows[i].File < rows[j].File
			}
			return rows[i].Line < rows[j].Line
		})

		if s.isGCX(ctx, req) {
			items := make([]annotatedItem, 0, len(rows))
			for _, r := range rows {
				items = append(items, annotatedItem(r))
			}
			return gcxResponse(encodeAnalyze("annotation_users", items))
		}
		if isCompact(req) {
			var b strings.Builder
			for _, r := range rows {
				if r.Args != "" {
					fmt.Fprintf(&b, "%s:%d  %s  (%s)\n", r.File, r.Line, r.Symbol, r.Args)
				} else {
					fmt.Fprintf(&b, "%s:%d  %s\n", r.File, r.Line, r.Symbol)
				}
			}
			if len(rows) == 0 {
				b.WriteString("no users for that annotation\n")
			}
			return mcp.NewToolResultText(b.String()), nil
		}
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"annotation": idFilter,
			"users":      rows,
			"total":      len(rows),
		})
	}

	// No id — group annotations by id, count usages.
	type annoRow struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Users int    `json:"users"`
	}
	byID := map[string]*annoRow{}
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		row, ok := byID[e.To]
		if !ok {
			n := s.graph.GetNode(e.To)
			name := ""
			if n != nil {
				name = n.Name
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &annoRow{ID: e.To, Name: name}
			byID[e.To] = row
		}
		row.Users++
	}
	rows := make([]*annoRow, 0, len(byID))
	for _, r := range byID {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Users != rows[j].Users {
			return rows[i].Users > rows[j].Users
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]annotationItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, annotationItem{
				ID:    r.ID,
				Name:  r.Name,
				Users: r.Users,
			})
		}
		return gcxResponse(encodeAnalyze("annotation_users.list", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-4d  %s  (%s)\n", r.Users, r.Name, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no annotations\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"annotations": rows,
		"total":       len(rows),
	})
}

// ---------------------------------------------------------------------------
// config_readers — list config_key nodes with their readers.
// ---------------------------------------------------------------------------

// handleAnalyzeConfigReaders walks EdgeReadsConfig edges and
// groups by config-key target. Each row carries the config-key id,
// its surface name, the source (env/viper/etc — kept verbatim from
// node meta), and the reader symbol list. Useful for tracing
// configuration drift ("which functions read DATABASE_URL?") and
// finding hot config keys with many readers.
//
// Filters:
//
//   - name: config-key bare name (case-insensitive). Returns the
//     readers of that single key.
//   - limit: max rows when no name filter is set (default 20).
func (s *Server) handleAnalyzeConfigReaders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	type configRow struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Source  string   `json:"source,omitempty"`
		Readers []string `json:"readers,omitempty"`
		Reads   int      `json:"reads"`
	}
	byKey := map[string]*configRow{}
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeReadsConfig {
			continue
		}
		row, ok := byKey[e.To]
		if !ok {
			n := s.graph.GetNode(e.To)
			name := ""
			source := ""
			if n != nil {
				name = n.Name
				source, _ = n.Meta["source"].(string)
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &configRow{ID: e.To, Name: name, Source: source}
			byKey[e.To] = row
		}
		row.Reads++
		row.Readers = appendUnique(row.Readers, e.From)
	}

	rows := make([]*configRow, 0, len(byKey))
	for _, r := range byKey {
		sort.Strings(r.Readers)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Reads != rows[j].Reads {
			return rows[i].Reads > rows[j].Reads
		}
		return rows[i].ID < rows[j].ID
	})
	truncated := false
	if nameFilter == "" && len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]configReaderItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, configReaderItem{
				ID:      r.ID,
				Name:    r.Name,
				Source:  r.Source,
				Reads:   r.Reads,
				Readers: strings.Join(r.Readers, ","),
			})
		}
		return gcxResponse(encodeAnalyze("config_readers", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			source := r.Source
			if source == "" {
				source = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Reads, source, r.Name)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated to %d\n", limit)
		}
		if len(rows) == 0 {
			b.WriteString("no config readers\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"config_keys": rows,
		"total":       len(rows),
		"truncated":   truncated,
	})
}

// ---------------------------------------------------------------------------
// event_emitters — list event nodes with their emitters.
// ---------------------------------------------------------------------------

// handleAnalyzeEventEmitters walks EdgeEmits edges and groups by
// event target. The `level` filter narrows by log level when meta
// carries it (error/warn/info/debug); without a level filter every
// event is included. Useful for "every site that logs an error" or
// "what does this metric get emitted from".
//
// Filters:
//
//   - level: log/event level — error, warn, warning, info, debug,
//     trace, fatal. Case-insensitive. The matcher checks both
//     `level` and the `event_kind`/`method` meta keys so it works
//     across the parsers that use different conventions.
//   - name: event name (case-insensitive). Returns the emitters
//     for that single event.
func (s *Server) handleAnalyzeEventEmitters(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	levelFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "level")))
	nameFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "name")))

	type eventRow struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Kind     string   `json:"event_kind,omitempty"`
		Emits    int      `json:"emits"`
		Emitters []string `json:"emitters,omitempty"`
	}
	byEvent := map[string]*eventRow{}
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeEmits {
			continue
		}
		// Level filter: an emit edge stores the method on the edge
		// (e.g. "Errorf"); the event node may carry an event_kind.
		// We accept either source so both per-event and per-call
		// taxonomies match.
		if levelFilter != "" {
			method, _ := e.Meta["method"].(string)
			if !levelMatches(levelFilter, method) {
				n := s.graph.GetNode(e.To)
				if n == nil {
					continue
				}
				kind, _ := n.Meta["event_kind"].(string)
				if !levelMatches(levelFilter, kind) {
					continue
				}
			}
		}
		row, ok := byEvent[e.To]
		if !ok {
			n := s.graph.GetNode(e.To)
			name := ""
			kind := ""
			if n != nil {
				name = n.Name
				kind, _ = n.Meta["event_kind"].(string)
			}
			if nameFilter != "" && strings.ToLower(name) != nameFilter {
				continue
			}
			row = &eventRow{ID: e.To, Name: name, Kind: kind}
			byEvent[e.To] = row
		}
		row.Emits++
		row.Emitters = appendUnique(row.Emitters, e.From)
	}
	rows := make([]*eventRow, 0, len(byEvent))
	for _, r := range byEvent {
		sort.Strings(r.Emitters)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Emits != rows[j].Emits {
			return rows[i].Emits > rows[j].Emits
		}
		return rows[i].ID < rows[j].ID
	})

	if s.isGCX(ctx, req) {
		items := make([]eventEmitterItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, eventEmitterItem{
				ID:       r.ID,
				Name:     r.Name,
				Kind:     r.Kind,
				Emits:    r.Emits,
				Emitters: strings.Join(r.Emitters, ","),
			})
		}
		return gcxResponse(encodeAnalyze("event_emitters", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			kind := r.Kind
			if kind == "" {
				kind = "?"
			}
			fmt.Fprintf(&b, "%-3d [%s] %s\n", r.Emits, kind, r.Name)
		}
		if len(rows) == 0 {
			b.WriteString("no event emitters\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"events": rows,
		"total":  len(rows),
	})
}

// ---------------------------------------------------------------------------
// error_surface — function/method nodes with their EdgeThrows
// targets.
// ---------------------------------------------------------------------------

// handleAnalyzeErrorSurface walks EdgeThrows edges and groups by
// thrower (the From side — the function/method that returns or
// raises the error). Each row lists the error types it can produce.
// Useful for sketching the error surface of a package, for finding
// functions that throw too many distinct error kinds, and for
// confirming that a refactor didn't widen the error surface
// inadvertently.
//
// Filters:
//
//   - path_prefix: scope to throwers under a directory subtree.
func (s *Server) handleAnalyzeErrorSurface(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pathPrefix := strings.TrimSpace(stringArg(req.GetArguments(), "path_prefix"))

	type throwerRow struct {
		Symbol string   `json:"symbol"`
		File   string   `json:"file"`
		Line   int      `json:"line"`
		Throws int      `json:"throws"`
		Errors []string `json:"errors"`
	}
	byThrower := map[string]*throwerRow{}
	for _, e := range s.graph.AllEdges() {
		if e.Kind != graph.EdgeThrows {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(e.FilePath, pathPrefix) {
			continue
		}
		row, ok := byThrower[e.From]
		if !ok {
			n := s.graph.GetNode(e.From)
			file := e.FilePath
			line := e.Line
			if n != nil {
				if file == "" {
					file = n.FilePath
				}
				if line == 0 {
					line = n.StartLine
				}
			}
			row = &throwerRow{Symbol: e.From, File: file, Line: line}
			byThrower[e.From] = row
		}
		row.Throws++
		row.Errors = appendUnique(row.Errors, e.To)
	}

	rows := make([]*throwerRow, 0, len(byThrower))
	for _, r := range byThrower {
		sort.Strings(r.Errors)
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		// Throwers with the most distinct error targets surface
		// first — those are the highest-leverage refactor candidates.
		ai, aj := len(rows[i].Errors), len(rows[j].Errors)
		if ai != aj {
			return ai > aj
		}
		if rows[i].Throws != rows[j].Throws {
			return rows[i].Throws > rows[j].Throws
		}
		return rows[i].Symbol < rows[j].Symbol
	})

	// Cap response size — large repos blow past the MCP token cap on
	// the JSON shape. Default 200 keeps the response useful while
	// callers that need everything pass an explicit limit.
	limit := 200
	if v, ok := req.GetArguments()["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	totalRows := len(rows)
	rowsTruncated := false
	if len(rows) > limit {
		rows = rows[:limit]
		rowsTruncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]errorSurfaceItem, 0, len(rows))
		for _, r := range rows {
			items = append(items, errorSurfaceItem{
				Symbol: r.Symbol,
				File:   r.File,
				Line:   r.Line,
				Throws: r.Throws,
				Errors: strings.Join(r.Errors, ","),
			})
		}
		return gcxResponse(encodeAnalyze("error_surface", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s:%d  %s  -> %s\n",
				len(r.Errors), r.File, r.Line, r.Symbol, strings.Join(r.Errors, ","))
		}
		if len(rows) == 0 {
			b.WriteString("no error surface\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	resp := map[string]any{
		"throwers":  rows,
		"total":     totalRows,
		"truncated": rowsTruncated,
	}
	if rowsTruncated {
		resp["limit"] = limit
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// appendUnique returns dst with v added if not already present.
// Used by every analyzer above to dedupe the From-side caller list
// without falling back to a map (the lists are small per row, so a
// linear scan is cheaper than building a set per group).
func appendUnique(dst []string, v string) []string {
	if v == "" {
		return dst
	}
	for _, x := range dst {
		if x == v {
			return dst
		}
	}
	return append(dst, v)
}

// levelMatches returns true when the requested level matches the
// candidate string. The match is case-insensitive and accepts
// common aliases (warn/warning, debug/trace) so callers can pass
// either the canonical level or the parser's verbatim method/kind
// without ceremony.
func levelMatches(want, candidate string) bool {
	candidate = strings.ToLower(candidate)
	if candidate == "" {
		return false
	}
	if strings.Contains(candidate, want) {
		return true
	}
	switch want {
	case "warn", "warning":
		return strings.Contains(candidate, "warn")
	case "info":
		return strings.Contains(candidate, "info")
	case "debug", "trace":
		return strings.Contains(candidate, "debug") || strings.Contains(candidate, "trace")
	case "error":
		return strings.Contains(candidate, "error") || strings.Contains(candidate, "err")
	case "fatal":
		return strings.Contains(candidate, "fatal") || strings.Contains(candidate, "panic")
	}
	return false
}
