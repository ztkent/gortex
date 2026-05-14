package mcp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/tokens"
)

func (s *Server) registerCodingTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("get_editing_context",
			mcp.WithDescription("The primary tool to call before editing any file. Returns all symbols defined in the file, their signatures, direct dependencies, and immediate callers — everything needed to code without reading raw source lines."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithString("detail", mcp.Description("brief or full (default: brief)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
		),
		s.handleGetEditingContext,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_import_path",
			mcp.WithDescription("Given a symbol name you want to use in a file, returns the correct import path. Use instead of reading files or guessing package paths."),
			mcp.WithString("name", mcp.Required(), mcp.Description("Name of the symbol to import")),
			mcp.WithString("path", mcp.Required(), mcp.Description("File where you want to use the symbol (relative path)")),
		),
		s.handleFindImportPath,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("explain_change_impact",
			mcp.WithDescription("Given a list of symbols you plan to modify, returns risk-tiered blast radius: d=1 will break, d=2 likely affected, d=3 needs testing. Includes affected processes and communities."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs to modify")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleEnhancedChangeImpact,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_symbol_source",
			mcp.WithDescription("Returns the source code of a specific symbol (function, method, type) without reading the entire file. Use instead of Read when you know which symbol you need — saves 70-80% of tokens compared to reading the whole file."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID (e.g. pkg/server.go::HandleRequest)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below the symbol (default: 3)")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleGetSymbolSource,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("batch_symbols",
			mcp.WithDescription("Returns signatures, source code, callers, and callees for multiple symbols in one call. Use instead of calling get_symbol_source or get_symbol multiple times — saves 60% round-trip overhead."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for each symbol (default: false)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below source (default: 3, only if include_source)")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleBatchSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_test_targets",
			mcp.WithDescription("Given changed symbol IDs, traces the call graph to find test files and test functions that exercise those symbols. Use after editing to know exactly which tests to run — no guessing, no running the entire suite."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithNumber("depth", mcp.Description("Caller traversal depth (default: 3)")),
		),
		s.handleGetTestTargets,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("suggest_pattern",
			mcp.WithDescription("Given an existing symbol as an example, extracts the structural pattern for creating similar code. Returns the example source, sibling symbols with the same pattern, registration/wiring code, test patterns, and files to edit. Use when adding a new function/handler/extractor that follows an existing convention."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
		),
		s.handleSuggestPattern,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_edit_plan",
			mcp.WithDescription("Given symbols you plan to change, returns a dependency-ordered list of files and symbols to edit — definitions first, then implementations, then callers, then tests. Eliminates manual dependency reasoning. Use before any multi-file refactor."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs to change")),
			mcp.WithNumber("depth", mcp.Description("Dependent traversal depth (default: 3)")),
		),
		s.handleGetEditPlan,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("edit_symbol",
			mcp.WithDescription("Edit a symbol's source code directly by ID — no Read needed. Gortex resolves the file and line range, finds the old_source fragment, replaces it with new_source, and writes the file. Eliminates the Read→Edit roundtrip for ~80% of edits."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID (e.g. server.go::NewServer)")),
			mcp.WithString("old_source", mcp.Required(), mcp.Description("Exact source fragment to replace (must be unique within the symbol)")),
			mcp.WithString("new_source", mcp.Required(), mcp.Description("Replacement source fragment")),
		),
		s.handleEditSymbol,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("edit_file",
			mcp.WithDescription("Edit any file (markdown, config, spec, template, source) by exact string replacement — no Read needed. Accepts absolute paths or paths relative to the indexed repo root. Writes atomically (temp+rename) and re-indexes the file so the graph stays fresh. Pass dry_run=true to validate the replacement without writing. Complements edit_symbol for non-code files that have no symbol ID."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path, or repo-prefixed / repo-root-relative path")),
			mcp.WithString("old_string", mcp.Required(), mcp.Description("Exact text to replace (must be unique unless replace_all=true)")),
			mcp.WithString("new_string", mcp.Required(), mcp.Description("Replacement text")),
			mcp.WithBoolean("replace_all", mcp.Description("Replace every occurrence instead of requiring uniqueness (default: false)")),
			mcp.WithBoolean("dry_run", mcp.Description("Validate the replacement and report what would change without writing (default: false)")),
		),
		s.handleEditFile,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("write_file",
			mcp.WithDescription("Create a new file or overwrite an existing one with the given content — no Read needed. Accepts absolute paths or paths relative to the indexed repo root. Writes atomically (temp+rename) and re-indexes the file so the graph stays fresh. Pass dry_run=true to report what would happen without writing. Use for new docs, configs, specs, scaffolded files; prefer edit_symbol or edit_file when a symbol/string target exists."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path, or repo-prefixed / repo-root-relative path")),
			mcp.WithString("content", mcp.Required(), mcp.Description("Full file content")),
			mcp.WithBoolean("dry_run", mcp.Description("Report would_create / would_overwrite without writing (default: false)")),
		),
		s.handleWriteFile,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("rename_symbol",
			mcp.WithDescription("Generates coordinated multi-file edit instructions for renaming a symbol. Returns {file, line, old_text, new_text, confidence} for every reference. Use dry_run to preview, then apply edits with the Edit tool."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to rename (e.g. auth/token.go::validateToken)")),
			mcp.WithString("new_name", mcp.Required(), mcp.Description("New name for the symbol")),
		),
		s.handleRenameSymbol,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("smart_context",
			mcp.WithDescription("Assembles the minimal context needed for a task in one call. Searches for relevant symbols, gets their source and relationships, finds patterns to follow, and builds an edit plan. Replaces an entire exploration phase of 5-10 tool calls."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural language description of what you want to do (e.g. 'add a new MCP tool called list_files')")),
			mcp.WithString("entry_point", mcp.Description("Optional symbol ID or file path to start from")),
			mcp.WithNumber("max_symbols", mcp.Description("Max symbols to include source for (default: 5)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
		),
		s.handleSmartContext,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("plan_turn",
			mcp.WithDescription("Opening-move router. Returns a short ranked list of recommended tool calls (with pre-filled args) for a task — 'what should I call first?'. Use before smart_context when you want a cheaper routing decision; call smart_context directly when you're committed to exploring."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural-language description of the task")),
			mcp.WithNumber("max_calls", mcp.Description("Max recommended calls (default: 4)")),
		),
		s.handlePlanTurn,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_untested_symbols",
			mcp.WithDescription("Returns functions and methods in non-test files that no test file reaches via the call graph — the inverse of get_test_targets. Ranked by fan_in descending so the most-used untested symbols surface first. Use to prioritize where to add test coverage."),
			mcp.WithNumber("limit", mcp.Description("Max entries returned (default: 50)")),
			mcp.WithString("file_prefix", mcp.Description("Restrict to symbols whose file path starts with this prefix (e.g. 'internal/auth/')")),
			mcp.WithNumber("min_fan_in", mcp.Description("Only flag symbols with at least this many callers; filters trivial helpers (default: 0)")),
		),
		s.handleGetUntestedSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_recent_changes",
			mcp.WithDescription("Returns files and symbols that changed since the last call (watch mode only). Use to re-orient after the user edits files outside of Claude Code's view, without re-reading anything."),
			mcp.WithString("since", mcp.Description("ISO 8601 timestamp (omit for all changes since index)")),
		),
		s.handleGetRecentChanges,
	)
}

type editingContext struct {
	File     map[string]any   `json:"file"`
	Defines  []map[string]any `json:"defines"`
	Imports  []map[string]any `json:"imports"`
	CalledBy []map[string]any `json:"called_by"`
	Calls    []map[string]any `json:"calls"`
}

func (s *Server) handleGetEditingContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Auto re-index stale file before querying.
	s.ensureFresh([]string{fp})

	s.sessionFor(ctx).recordFile(fp)
	sg := s.engine.GetFileSymbols(fp)
	if len(sg.Nodes) == 0 {
		return mcp.NewToolResultError("no symbols found for file: " + fp), nil
	}
	// A file outside the session's workspace is reported as not found
	// — its symbols all share one repo, so the first node decides.
	if !s.nodeInSessionScope(ctx, sg.Nodes[0]) {
		return mcp.NewToolResultError("no symbols found for file: " + fp), nil
	}
	// Confine the caller/callee neighbourhoods below to the session
	// workspace so editing context never reaches across the boundary.
	sessWS, _, _ := s.sessionScope(ctx)
	// Frecency: a file-level editing context is effectively an access to
	// every symbol defined in that file. Credit each of them — this is
	// the signal that "the agent is working in this area right now."
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile {
			continue
		}
		s.frecency.Record(n.ID)
	}

	out := editingContext{}

	// File info.
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile {
			out.File = map[string]any{"id": n.ID, "language": n.Language}
			break
		}
	}

	// Defines: all non-file symbols in this file.
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile {
			continue
		}
		entry := map[string]any{
			"id":         n.ID,
			"kind":       n.Kind,
			"name":       n.Name,
			"start_line": n.StartLine,
		}
		if sig, ok := n.Meta["signature"]; ok {
			entry["signature"] = sig
		}
		out.Defines = append(out.Defines, entry)
	}

	// Imports: outgoing import edges from the file node.
	for _, e := range sg.Edges {
		if e.Kind == graph.EdgeImports {
			importInfo := map[string]any{
				"id":       e.To,
				"external": strings.HasPrefix(e.To, "external::"),
			}
			out.Imports = append(out.Imports, importInfo)
		}
	}

	// CalledBy: who calls symbols in this file (depth 1).
	callerSeen := make(map[string]bool)
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			callers := s.engine.GetCallers(n.ID, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief", WorkspaceID: sessWS})
			for _, cn := range callers.Nodes {
				if cn.FilePath != fp && !callerSeen[cn.ID] {
					callerSeen[cn.ID] = true
					out.CalledBy = append(out.CalledBy, map[string]any{
						"id":         cn.ID,
						"name":       cn.Name,
						"file_path":  cn.FilePath,
						"start_line": cn.StartLine,
					})
				}
			}
		}
	}

	// Calls: what symbols in this file call (depth 1).
	callSeen := make(map[string]bool)
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			chain := s.engine.GetCallChain(n.ID, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief", WorkspaceID: sessWS})
			for _, cn := range chain.Nodes {
				if cn.FilePath != fp && !callSeen[cn.ID] {
					callSeen[cn.ID] = true
					out.Calls = append(out.Calls, map[string]any{
						"id":         cn.ID,
						"name":       cn.Name,
						"file_path":  cn.FilePath,
						"start_line": cn.StartLine,
					})
				}
			}
		}
	}

	// ETag conditional fetch.
	etag := computeETag(out)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeEditingContext(out.File, out.Defines, out.Imports, out.CalledBy, out.Calls, etag))
	}

	// Add etag to response by marshaling to map.
	result := map[string]any{
		"file":      out.File,
		"defines":   out.Defines,
		"imports":   out.Imports,
		"called_by": out.CalledBy,
		"calls":     out.Calls,
		"etag":      etag,
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleFindImportPath(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolName, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError("name is required"), nil
	}
	targetFile, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	candidates := s.scopedNodeSlice(ctx, s.engine.FindSymbols(symbolName))
	if len(candidates) == 0 {
		return mcp.NewToolResultError("symbol not found: " + symbolName), nil
	}

	// Find the best match (prefer different directory from target).
	targetDir := filepath.Dir(targetFile)
	var best *graph.Node
	for _, c := range candidates {
		if c.Kind == graph.KindFile || c.Kind == graph.KindImport {
			continue
		}
		if best == nil {
			best = c
		}
		// Prefer symbols NOT in the same directory (actual imports).
		if filepath.Dir(c.FilePath) != targetDir {
			best = c
			break
		}
	}

	if best == nil {
		return mcp.NewToolResultError("no importable symbol found: " + symbolName), nil
	}

	// Check if already imported.
	alreadyImported := false
	fileSymbols := s.engine.GetFileSymbols(targetFile)
	if len(fileSymbols.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSymbols.Nodes[0]) {
		fileSymbols = nil
	}
	if fileSymbols != nil {
		for _, e := range fileSymbols.Edges {
			if e.Kind == graph.EdgeImports && strings.Contains(e.To, filepath.Dir(best.FilePath)) {
				alreadyImported = true
				break
			}
		}
	}

	// Defensive: if the matched node carries an absolute file path
	// (older snapshots produced before applyRepoPrefix was hardened
	// could land abs paths in the graph), fold it back to a
	// repo-relative form so the response stays consistent with every
	// other graph tool. Without this, agents see a leaked
	// `/Users/...` import_path that confuses code generation.
	importDir := filepath.Dir(best.FilePath)
	if filepath.IsAbs(importDir) {
		importDir = s.repoRelative(importDir)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbol_id":        best.ID,
		"import_path":      importDir,
		"already_imported": alreadyImported,
	})
}

func (s *Server) handleGetRecentChanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.watcher == nil {
		return mcp.NewToolResultError("watch mode is not active"), nil
	}

	sinceStr := req.GetString("since", "")
	var changes []map[string]any

	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return mcp.NewToolResultError("invalid timestamp: " + sinceStr), nil
		}
		for _, ev := range s.watcher.HistorySince(t) {
			changes = append(changes, map[string]any{
				"file":          ev.FilePath,
				"kind":          ev.Kind,
				"nodes_added":   ev.NodesAdded,
				"nodes_removed": ev.NodesRemoved,
				"timestamp":     ev.Timestamp.Format(time.RFC3339),
			})
		}
	} else {
		for _, ev := range s.watcher.History() {
			changes = append(changes, map[string]any{
				"file":          ev.FilePath,
				"kind":          ev.Kind,
				"nodes_added":   ev.NodesAdded,
				"nodes_removed": ev.NodesRemoved,
				"timestamp":     ev.Timestamp.Format(time.RFC3339),
			})
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"changes":             changes,
		"graph_current_as_of": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleGetSymbolSource(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	// Auto re-index stale file before querying.
	if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
		s.ensureFresh([]string{parts[0]})
	}

	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	// A by-id fetch must not cross the session's workspace boundary —
	// reported the same as a genuine miss so the boundary isn't
	// probeable.
	if !s.nodeInSessionScope(ctx, node) {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	sess := s.sessionFor(ctx)
	sess.recordSymbol(id)
	sess.recordFile(node.FilePath)
	// Credit this consume back to the most recent matching search_symbols,
	// if any; no-op when the combo tracker isn't initialised or no search
	// window is active.
	if q := sess.attributedQuery(id); q != "" {
		s.combo.Record(q, id)
	}
	// Unconditionally record the access for frecency — this is the "symbols
	// the agent actually reads" signal, useful even when no prior search
	// sourced it (agents also fetch symbols by ID from recent history).
	s.frecency.Record(id)

	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	contextLines := req.GetInt("context_lines", 3)

	// Resolve the file path against whichever indexer owns the repo.
	// Multi-repo mode is the source of truth — node.RepoPrefix names
	// the owning repo and MultiIndexer holds its RootPath. Single-repo
	// mode falls back to the lone indexer's RootPath. Bare-relative
	// paths must never reach readLines: os.Open would resolve them
	// against the daemon process cwd, which is unrelated to any repo
	// and silently produces wrong results.
	absPath, resolveErr := s.resolveNodePath(node)
	if resolveErr != nil {
		return mcp.NewToolResultError(resolveErr.Error()), nil
	}

	source, startLine, totalFileChars, err := readLines(absPath, node.StartLine, node.EndLine, contextLines)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read source: %v", err)), nil
	}

	// Server-side accounting only — the savings value isn't returned to
	// the caller (agents don't act on it and it burns tokens in every
	// response). Aggregated stats remain available via the `savings` tool.
	returned := tokens.CountInt64(source)
	fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
	s.tokenStatsFor(ctx).record(node, returned, fullFile)

	result := map[string]any{
		"id":         node.ID,
		"kind":       node.Kind,
		"name":       node.Name,
		"file_path":  node.FilePath,
		"start_line": node.StartLine,
		"end_line":   node.EndLine,
		"source":     source,
		"from_line":  startLine,
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}

	// ETag conditional fetch.
	etag := computeETag(result)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	result["etag"] = etag

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeGetSymbolSource(node, source, startLine, etag))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// readLines reads lines from a file, with optional context lines above/below.
// Returns the source text, the first line number, the total file size in characters
// (for token savings estimation), and any error.
func readLines(path string, startLine, endLine, contextLines int) (string, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer func() { _ = f.Close() }()

	from := startLine - contextLines
	if from < 1 {
		from = 1
	}
	to := endLine + contextLines

	var lines []string
	var totalChars int
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		text := scanner.Text()
		totalChars += len(text) + 1 // +1 for newline stripped by Scanner
		if lineNum >= from && lineNum <= to {
			lines = append(lines, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, 0, err
	}

	return strings.Join(lines, "\n"), from, totalChars, nil
}

func (s *Server) handleBatchSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	includeSource := false
	if v, ok := req.GetArguments()["include_source"].(bool); ok {
		includeSource = v
	}
	contextLines := req.GetInt("context_lines", 3)
	// Confine the caller/callee neighbourhoods below to the session
	// workspace.
	sessWS, _, _ := s.sessionScope(ctx)

	var results []map[string]any
	for _, id := range ids {
		node := s.engine.GetSymbol(id)
		// A node outside the session's workspace is reported as a
		// miss — identical to a genuinely absent ID so the boundary
		// stays opaque.
		if node == nil || !s.nodeInSessionScope(ctx, node) {
			results = append(results, map[string]any{
				"id":    id,
				"error": "symbol not found",
			})
			continue
		}

		entry := map[string]any{
			"id":         node.ID,
			"kind":       node.Kind,
			"name":       node.Name,
			"file_path":  node.FilePath,
			"start_line": node.StartLine,
			"end_line":   node.EndLine,
		}
		if sig, ok := node.Meta["signature"]; ok {
			entry["signature"] = sig
		}

		// Callers (depth 1).
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			callers := s.engine.GetCallers(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief", WorkspaceID: sessWS})
			var callerIDs []string
			for _, cn := range callers.Nodes {
				if cn.ID != node.ID {
					callerIDs = append(callerIDs, cn.ID)
				}
			}
			if len(callerIDs) > 0 {
				entry["callers"] = callerIDs
			}

			// Callees (depth 1).
			callees := s.engine.GetCallChain(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief", WorkspaceID: sessWS})
			var calleeIDs []string
			for _, cn := range callees.Nodes {
				if cn.ID != node.ID {
					calleeIDs = append(calleeIDs, cn.ID)
				}
			}
			if len(calleeIDs) > 0 {
				entry["callees"] = calleeIDs
			}
		}

		// Source code (optional).
		if includeSource && node.StartLine > 0 && node.EndLine > 0 {
			if absPath, err := s.resolveNodePath(node); err == nil {
				if source, fromLine, totalFileChars, err := readLines(absPath, node.StartLine, node.EndLine, contextLines); err == nil {
					entry["source"] = source
					entry["from_line"] = fromLine
					returned := tokens.CountInt64(source)
					fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
					s.tokenStatsFor(ctx).record(node, returned, fullFile)
				}
			}
		}

		results = append(results, entry)
	}

	batchResult := map[string]any{
		"symbols": results,
		"total":   len(results),
	}

	// ETag conditional fetch.
	etag := computeETag(batchResult)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}
	batchResult["etag"] = etag

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeBatchSymbols(results, includeSource))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(batchResult)
	}

	return s.respondJSONOrTOON(ctx, req, batchResult)
}

// Test file patterns by language.
var testFilePatterns = []struct {
	suffix string
	lang   string
}{
	{"_test.go", "go"},
	{".test.ts", "typescript"},
	{".test.tsx", "typescript"},
	{".spec.ts", "typescript"},
	{".test.js", "javascript"},
	{".spec.js", "javascript"},
	{"_test.py", "python"},
	{"test_", "python"},
	{"_test.rs", "rust"},
	{"Test.java", "java"},
	{"_test.rb", "ruby"},
	{"_test.exs", "elixir"},
	{"_test.kt", "kotlin"},
	{"Tests.swift", "swift"},
	{"Test.scala", "scala"},
	{"Test.php", "php"},
	{"Test.cs", "csharp"},
}

func isTestFile(path string) bool {
	for _, p := range testFilePatterns {
		if strings.Contains(path, p.suffix) {
			return true
		}
	}
	return strings.Contains(path, "__tests__/") || strings.Contains(path, "/test/")
}

func (s *Server) handleGetTestTargets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	depth := req.GetInt("depth", 3)

	// For each symbol, trace callers and collect test nodes.
	type testTarget struct {
		File      string   `json:"file"`
		Functions []string `json:"functions"`
	}

	// Map: test file -> set of test function names.
	testFiles := make(map[string]map[string]bool)
	// Track which changed symbols are covered.
	coveredSymbols := make(map[string]bool)

	for _, id := range ids {
		node := s.engine.GetSymbol(id)
		if node == nil {
			continue
		}

		// Fast path: use the persistent EdgeTests edges that the
		// indexer's test-edge pass attached to the graph. A direct
		// inverse-edge walk is one hop instead of the BFS-on-EdgeCalls
		// that this tool used to do, and it's exact (no isTestFile
		// post-filter needed).
		if testers := s.engine.GetTesters(id); len(testers) > 0 {
			for _, tn := range testers {
				if tn == nil {
					continue
				}
				coveredSymbols[id] = true
				if testFiles[tn.FilePath] == nil {
					testFiles[tn.FilePath] = make(map[string]bool)
				}
				if tn.Kind == graph.KindFunction || tn.Kind == graph.KindMethod {
					testFiles[tn.FilePath][tn.Name] = true
				}
			}
			continue
		}

		// Fallback for graphs that haven't been re-indexed since the
		// EdgeTests pass shipped, or for indirect coverage (depth > 1).
		callers := s.engine.GetCallers(id, query.QueryOptions{Depth: depth, Limit: 100, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if !isTestFile(cn.FilePath) {
				continue
			}
			coveredSymbols[id] = true
			if testFiles[cn.FilePath] == nil {
				testFiles[cn.FilePath] = make(map[string]bool)
			}
			if cn.Kind == graph.KindFunction || cn.Kind == graph.KindMethod {
				testFiles[cn.FilePath][cn.Name] = true
			}
		}

		// Also check if the symbol itself is in a test file (e.g. test helper).
		if isTestFile(node.FilePath) {
			coveredSymbols[id] = true
			if testFiles[node.FilePath] == nil {
				testFiles[node.FilePath] = make(map[string]bool)
			}
		}
	}

	// Build result.
	var targets []testTarget
	for file, funcs := range testFiles {
		var names []string
		for name := range funcs {
			names = append(names, name)
		}
		targets = append(targets, testTarget{
			File:      file,
			Functions: names,
		})
	}

	// Build run commands (Go-specific for now, extensible later).
	var runCommands []string
	for _, t := range targets {
		if strings.HasSuffix(t.File, "_test.go") {
			dir := filepath.Dir(t.File)
			if len(t.Functions) > 0 {
				runCommands = append(runCommands,
					fmt.Sprintf("go test -run %s ./%s/", strings.Join(t.Functions, "|"), dir))
			} else {
				runCommands = append(runCommands,
					fmt.Sprintf("go test ./%s/", dir))
			}
		}
	}

	// Uncovered symbols (no test found).
	var uncovered []string
	for _, id := range ids {
		if !coveredSymbols[id] {
			uncovered = append(uncovered, id)
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"test_targets":  targets,
		"run_commands":  runCommands,
		"total_files":   len(targets),
		"uncovered":     uncovered,
		"coverage_note": fmt.Sprintf("%d/%d changed symbols have test coverage", len(coveredSymbols), len(ids)),
	})
}

func (s *Server) handleSuggestPattern(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	node := s.engine.GetSymbol(exampleID)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + exampleID), nil
	}

	result := map[string]any{
		"example": map[string]any{
			"id":        node.ID,
			"kind":      node.Kind,
			"name":      node.Name,
			"file_path": node.FilePath,
		},
	}

	// 1. Get the example source.
	if node.StartLine > 0 && node.EndLine > 0 {
		if absPath, err := s.resolveNodePath(node); err == nil {
			if source, _, _, err := readLines(absPath, node.StartLine, node.EndLine, 0); err == nil {
				result["example_source"] = source
			}
		}
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}

	// 2. Find siblings — same kind, same file, similar naming pattern.
	fileSymbols := s.engine.GetFileSymbols(node.FilePath)
	if len(fileSymbols.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSymbols.Nodes[0]) {
		fileSymbols = &query.SubGraph{}
	}
	var siblings []map[string]any
	prefix := extractPrefix(node.Name)
	for _, sn := range fileSymbols.Nodes {
		if sn.ID == node.ID || sn.Kind != node.Kind {
			continue
		}
		siblings = append(siblings, map[string]any{
			"id":         sn.ID,
			"name":       sn.Name,
			"start_line": sn.StartLine,
		})
	}
	if len(siblings) > 10 {
		siblings = siblings[:10]
	}
	result["siblings"] = siblings
	result["siblings_count"] = len(fileSymbols.Nodes) - 1 // exclude file node

	// 3. Find how the example is wired/registered (callers at depth 1).
	callers := s.engine.GetCallers(exampleID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
	var registration []map[string]any
	for _, cn := range callers.Nodes {
		if cn.ID == exampleID {
			continue
		}
		entry := map[string]any{
			"id":         cn.ID,
			"name":       cn.Name,
			"file_path":  cn.FilePath,
			"start_line": cn.StartLine,
		}
		// Get the registration source (the caller function that wires this symbol).
		if cn.StartLine > 0 && cn.EndLine > 0 {
			if absPath, err := s.resolveNodePath(cn); err == nil {
				if source, _, _, err := readLines(absPath, cn.StartLine, cn.EndLine, 0); err == nil {
					entry["source"] = source
				}
			}
		}
		registration = append(registration, entry)
	}
	result["registration"] = registration

	// 4. Find test patterns — look for test symbols with matching name prefix.
	var testPatterns []map[string]any
	if prefix != "" {
		// Search for test functions that match the example name.
		testSearch := s.scopedNodeSlice(ctx, s.engine.SearchSymbols(node.Name, 20))
		for _, tn := range testSearch {
			if !isTestFile(tn.FilePath) {
				continue
			}
			if tn.Kind != graph.KindFunction && tn.Kind != graph.KindMethod {
				continue
			}
			entry := map[string]any{
				"id":         tn.ID,
				"name":       tn.Name,
				"file_path":  tn.FilePath,
				"start_line": tn.StartLine,
			}
			// Get test source.
			if tn.StartLine > 0 && tn.EndLine > 0 {
				if absPath, err := s.resolveNodePath(tn); err == nil {
					if source, _, _, err := readLines(absPath, tn.StartLine, tn.EndLine, 0); err == nil {
						entry["source"] = source
					}
				}
			}
			testPatterns = append(testPatterns, entry)
			if len(testPatterns) >= 3 {
				break
			}
		}
	}
	result["test_patterns"] = testPatterns

	// 5. Files to edit — where would you add a new instance of this pattern?
	filesToEdit := []map[string]any{
		{"file": node.FilePath, "reason": "add new symbol here (same file as example)"},
	}
	for _, reg := range registration {
		if fp, ok := reg["file_path"].(string); ok && fp != node.FilePath {
			filesToEdit = append(filesToEdit, map[string]any{
				"file":   fp,
				"reason": "update registration/wiring",
			})
		}
	}
	for _, tp := range testPatterns {
		if fp, ok := tp["file_path"].(string); ok {
			filesToEdit = append(filesToEdit, map[string]any{
				"file":   fp,
				"reason": "add test for new symbol",
			})
			break // one test file is enough
		}
	}
	result["files_to_edit"] = filesToEdit

	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleGetEditPlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	depth := req.GetInt("depth", 3)

	type editStep struct {
		File    string   `json:"file"`
		Symbols []string `json:"symbols"`
		Reason  string   `json:"reason"`
		Order   int      `json:"order"`
	}

	// Track files by category and depth.
	type fileInfo struct {
		symbols map[string]bool
		reason  string
		order   int // lower = edit first
	}
	files := make(map[string]*fileInfo)

	addFile := func(filePath, symbol, reason string, order int) {
		if fi, ok := files[filePath]; ok {
			fi.symbols[symbol] = true
			// Keep the lowest (highest priority) order.
			if order < fi.order {
				fi.order = order
				fi.reason = reason
			}
		} else {
			files[filePath] = &fileInfo{
				symbols: map[string]bool{symbol: true},
				reason:  reason,
				order:   order,
			}
		}
	}

	changedFiles := make(map[string]bool)

	// Order 0: The changed symbols themselves (definitions).
	for _, id := range ids {
		node := s.engine.GetSymbol(id)
		if node == nil {
			continue
		}
		addFile(node.FilePath, node.Name, "definition — change starts here", 0)
		changedFiles[node.FilePath] = true

		// Check if symbol is an interface — implementations need updating.
		if node.Kind == graph.KindInterface {
			impls := s.scopedNodeSlice(ctx, s.engine.FindImplementations(id))
			for _, impl := range impls {
				addFile(impl.FilePath, impl.Name, "implements "+node.Name+" — must conform to changes", 1)
			}
		}

		// Check MemberOf — if changing a type, its methods may need updating.
		if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
			inEdges := s.engine.GetInEdges(id)
			for _, e := range inEdges {
				if e.Kind == graph.EdgeMemberOf {
					memberNode := s.engine.GetSymbol(e.From)
					if memberNode != nil {
						addFile(memberNode.FilePath, memberNode.Name, "member of "+node.Name, 1)
					}
				}
			}
		}
	}

	// Order 2-N: Dependents at increasing depth (callers/importers).
	for _, id := range ids {
		dependents := s.engine.GetDependents(id, query.QueryOptions{Depth: depth, Limit: 100, Detail: "brief"})
		for _, dn := range dependents.Nodes {
			if dn.Kind == graph.KindFile {
				continue
			}
			// Skip the changed symbols themselves.
			isChanged := false
			for _, cid := range ids {
				if dn.ID == cid {
					isChanged = true
					break
				}
			}
			if isChanged {
				continue
			}

			if isTestFile(dn.FilePath) {
				addFile(dn.FilePath, dn.Name, "test — verify after changes", 100)
			} else if changedFiles[dn.FilePath] {
				// Same file as a changed symbol, already covered.
				addFile(dn.FilePath, dn.Name, "definition — change starts here", 0)
			} else {
				addFile(dn.FilePath, dn.Name, "dependent — may need updating", 2)
			}
		}
	}

	// Sort by order, then by file path.
	type sortableStep struct {
		filePath string
		info     *fileInfo
	}
	var sorted []sortableStep
	for fp, fi := range files {
		sorted = append(sorted, sortableStep{fp, fi})
	}
	// Stable sort: order first, then alphabetical.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].info.order < sorted[i].info.order ||
				(sorted[j].info.order == sorted[i].info.order && sorted[j].filePath < sorted[i].filePath) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var steps []editStep
	for _, s := range sorted {
		var symbols []string
		for sym := range s.info.symbols {
			symbols = append(symbols, sym)
		}
		steps = append(steps, editStep{
			File:    s.filePath,
			Symbols: symbols,
			Reason:  s.info.reason,
			Order:   s.info.order,
		})
	}

	// Separate test files.
	var editSteps, testSteps []editStep
	for _, step := range steps {
		if isTestFile(step.File) {
			testSteps = append(testSteps, step)
		} else {
			editSteps = append(editSteps, step)
		}
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"edit_order":  editSteps,
		"test_after":  testSteps,
		"total_files": len(steps),
		"summary":     fmt.Sprintf("%d files to edit, %d test files to verify", len(editSteps), len(testSteps)),
	})
}

// extractPrefix returns the common prefix of a camelCase/PascalCase name.
// e.g. "handleGetSymbol" -> "handle", "TestNewServer" -> "Test"
func (s *Server) handleSmartContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, err := req.RequireString("task")
	if err != nil {
		return mcp.NewToolResultError("task is required"), nil
	}

	entryPoint := req.GetString("entry_point", "")
	maxSymbols := req.GetInt("max_symbols", 5)

	result := map[string]any{
		"task": task,
	}

	// 1. Extract keywords from task description. Kept internal — the
	// caller doesn't need the derivation to act on the result, and
	// echoing 5-10 keywords per response is pure token bloat.
	keywords := extractKeywords(task)

	// 2. Search for relevant symbols using each keyword.
	seen := make(map[string]bool)
	var relevantSymbols []*graph.Node
	for _, kw := range keywords {
		if len(kw) < 3 {
			continue
		}
		matches := s.scopedNodeSlice(ctx, s.engine.SearchSymbols(kw, 10))
		for _, m := range matches {
			if m.Kind == graph.KindFile || m.Kind == graph.KindImport {
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				relevantSymbols = append(relevantSymbols, m)
			}
		}
	}

	// 3. If entry point given, resolve it and prioritize.
	var entryNode *graph.Node
	if entryPoint != "" {
		// Try as symbol ID first.
		entryNode = s.engine.GetSymbol(entryPoint)
		if entryNode == nil {
			// Try as file path — get the most important symbol in the file.
			fileSym := s.engine.GetFileSymbols(entryPoint)
			if len(fileSym.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileSym.Nodes[0]) {
				fileSym = &query.SubGraph{}
			}
			if len(fileSym.Nodes) > 0 {
				for _, n := range fileSym.Nodes {
					if n.Kind != graph.KindFile {
						entryNode = n
						break
					}
				}
			}
		}
		if entryNode != nil && !seen[entryNode.ID] {
			relevantSymbols = append([]*graph.Node{entryNode}, relevantSymbols...)
			seen[entryNode.ID] = true
		}
	}

	// 3b. Apply repo/project filter to relevant symbols.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	relevantSymbols = filterNodes(relevantSymbols, allowed)

	// 3c. Feedback-aware reranking (when feedback data exists).
	if s.feedback != nil && s.feedback.HasData() && len(relevantSymbols) > 0 {
		type scored struct {
			node  *graph.Node
			score float64
		}
		scoredSyms := make([]scored, len(relevantSymbols))
		for i, sym := range relevantSymbols {
			baseScore := 1.0 / float64(i+1) // BM25 rank-based score
			fbScore := s.feedback.GetSymbolScore(sym.ID)
			scoredSyms[i] = scored{node: sym, score: baseScore + fbScore*0.3}
		}
		sort.Slice(scoredSyms, func(i, j int) bool {
			return scoredSyms[i].score > scoredSyms[j].score
		})
		for i, ss := range scoredSyms {
			relevantSymbols[i] = ss.node
		}

		// Inject frequently-missed symbols that match task keywords.
		missed := s.feedback.MissedSymbols(3)
		injected := 0
		for _, missedID := range missed {
			if injected >= 2 {
				break
			}
			if seen[missedID] {
				continue
			}
			missedNode := s.graph.GetNode(missedID)
			if missedNode == nil {
				continue
			}
			// Check if the missed symbol name matches any keyword.
			nameLower := strings.ToLower(missedNode.Name)
			for _, kw := range keywords {
				if strings.Contains(nameLower, strings.ToLower(kw)) {
					relevantSymbols = append(relevantSymbols, missedNode)
					seen[missedID] = true
					injected++
					break
				}
			}
		}
	}

	// 4. Limit to top N most relevant symbols.
	if len(relevantSymbols) > maxSymbols {
		relevantSymbols = relevantSymbols[:maxSymbols]
	}

	// 5. Get source and signatures for relevant symbols. Source is
	// only embedded for the top maxSource functions/methods — signatures
	// alone cover 80% of agent decision-making and each full source
	// snippet adds several hundred tokens. Callers that need more can
	// follow up with get_symbol_source for specific IDs.
	const maxSource = 3
	sourcesEmbedded := 0
	var symbolContexts []map[string]any
	for _, sym := range relevantSymbols {
		entry := map[string]any{
			"id":         sym.ID,
			"kind":       sym.Kind,
			"name":       sym.Name,
			"file_path":  sym.FilePath,
			"start_line": sym.StartLine,
		}
		if sig, ok := sym.Meta["signature"]; ok {
			entry["signature"] = sig
		}
		if sourcesEmbedded < maxSource &&
			(sym.Kind == graph.KindFunction || sym.Kind == graph.KindMethod) &&
			sym.StartLine > 0 && sym.EndLine > 0 {
			if absPath, err := s.resolveNodePath(sym); err == nil {
				if source, _, totalFileChars, err := readLines(absPath, sym.StartLine, sym.EndLine, 0); err == nil {
					entry["source"] = source
					sourcesEmbedded++
					returned := tokens.CountInt64(source)
					fullFile := int64(tokens.EstimateFromSample(totalFileChars, source))
					s.tokenStatsFor(ctx).record(sym, returned, fullFile)
				}
			}
		}
		symbolContexts = append(symbolContexts, entry)
	}
	result["relevant_symbols"] = symbolContexts

	// 5b. Include cross-repo dependencies when in multi-repo mode.
	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		var crossRepoDeps []map[string]any
		crossSeen := make(map[string]bool)
		for _, sym := range relevantSymbols {
			// Check outgoing edges for cross-repo references.
			outEdges := s.engine.GetOutEdges(sym.ID)
			for _, e := range outEdges {
				if !e.CrossRepo || crossSeen[e.To] {
					continue
				}
				crossSeen[e.To] = true
				targetNode := s.engine.GetSymbol(e.To)
				if targetNode == nil {
					continue
				}
				dep := map[string]any{
					"id":          targetNode.ID,
					"kind":        targetNode.Kind,
					"name":        targetNode.Name,
					"file_path":   targetNode.FilePath,
					"repo_prefix": targetNode.RepoPrefix,
					"edge_kind":   e.Kind,
				}
				if sig, ok := targetNode.Meta["signature"]; ok {
					dep["signature"] = sig
				}
				crossRepoDeps = append(crossRepoDeps, dep)
			}
		}
		if len(crossRepoDeps) > 0 {
			result["cross_repo_dependencies"] = crossRepoDeps
		}
	}

	// 6. If we have an entry point, get its pattern (registration, siblings, tests).
	if entryNode != nil {
		// File context: imports and structure.
		fileCtx := s.engine.GetFileSymbols(entryNode.FilePath)
		if len(fileCtx.Nodes) > 0 && !s.nodeInSessionScope(ctx, fileCtx.Nodes[0]) {
			fileCtx = &query.SubGraph{}
		}
		var fileSymbols []string
		for _, n := range fileCtx.Nodes {
			if n.Kind != graph.KindFile {
				fileSymbols = append(fileSymbols, fmt.Sprintf("%s %s (line %d)", n.Kind, n.Name, n.StartLine))
			}
		}
		result["entry_file_symbols"] = fileSymbols

		// Callers and callees.
		callers := s.engine.GetCallers(entryNode.ID, query.QueryOptions{Depth: 1, Limit: 5, Detail: "brief"})
		var callerIDs []string
		for _, cn := range callers.Nodes {
			if cn.ID != entryNode.ID {
				callerIDs = append(callerIDs, cn.ID)
			}
		}
		if len(callerIDs) > 0 {
			result["callers"] = callerIDs
		}

		callees := s.engine.GetCallChain(entryNode.ID, query.QueryOptions{Depth: 1, Limit: 5, Detail: "brief"})
		var calleeIDs []string
		for _, cn := range callees.Nodes {
			if cn.ID != entryNode.ID {
				calleeIDs = append(calleeIDs, cn.ID)
			}
		}
		if len(calleeIDs) > 0 {
			result["callees"] = calleeIDs
		}
	}

	// 7. Find test files related to the keywords.
	var testFiles []string
	testSeen := make(map[string]bool)
	for _, sym := range relevantSymbols {
		callers := s.engine.GetCallers(sym.ID, query.QueryOptions{Depth: 2, Limit: 20, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if isTestFile(cn.FilePath) && !testSeen[cn.FilePath] {
				testSeen[cn.FilePath] = true
				testFiles = append(testFiles, cn.FilePath)
			}
		}
	}
	if len(testFiles) > 5 {
		testFiles = testFiles[:5]
	}
	result["related_test_files"] = testFiles

	// 8. Files likely to edit.
	fileSet := make(map[string]bool)
	for _, sym := range relevantSymbols {
		fileSet[sym.FilePath] = true
	}
	var filesToEdit []string
	for f := range fileSet {
		filesToEdit = append(filesToEdit, f)
	}
	for _, tf := range testFiles {
		if !fileSet[tf] {
			filesToEdit = append(filesToEdit, tf)
		}
	}
	result["files_to_edit"] = filesToEdit

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeSmartContext(result))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// extractKeywords splits a task description into searchable keywords.
// Filters out common stop words and short words.
func extractKeywords(task string) []string {
	stopWords := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "shall": true, "can": true,
		"for": true, "and": true, "but": true, "or": true, "nor": true,
		"not": true, "so": true, "yet": true, "both": true,
		"to": true, "of": true, "in": true, "on": true, "at": true,
		"by": true, "with": true, "from": true, "into": true, "that": true,
		"this": true, "it": true, "its": true, "as": true, "if": true,
		"add": true, "new": true, "create": true, "make": true, "called": true,
		"like": true, "use": true, "using": true, "how": true, "what": true,
		"want": true, "need": true, "all": true, "each": true, "which": true,
	}

	// Split on whitespace and punctuation.
	words := strings.FieldsFunc(task, func(r rune) bool {
		if r >= 'a' && r <= 'z' {
			return false
		}
		if r >= 'A' && r <= 'Z' {
			return false
		}
		if r >= '0' && r <= '9' {
			return false
		}
		return r != '_'
	})

	seen := make(map[string]bool)
	var keywords []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if len(lower) < 3 || stopWords[lower] || seen[lower] {
			continue
		}
		seen[lower] = true
		keywords = append(keywords, w) // keep original case for search
	}
	return keywords
}

func (s *Server) handleRenameSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return mcp.NewToolResultError("new_name is required"), nil
	}

	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}

	oldName := node.Name

	if oldName == newName {
		return mcp.NewToolResultError("new_name is the same as the current name"), nil
	}

	// Resolve abs paths per node/edge — single rootPath is wrong in
	// multi-repo mode where each repo has its own root.
	resolvePath := func(graphPath string) string {
		abs, err := s.resolveGraphPath(graphPath)
		if err != nil {
			return ""
		}
		return abs
	}

	type renameEdit struct {
		File       string `json:"file"`
		Line       int    `json:"line"`
		OldText    string `json:"old_text"`
		NewText    string `json:"new_text"`
		Confidence string `json:"confidence"`
		Reason     string `json:"reason"`
	}

	var edits []renameEdit
	editSeen := make(map[string]bool) // file:line dedup

	// 1. The definition itself.
	defLine := readSingleLineAt(resolvePath(node.FilePath), node.StartLine)
	if defLine != "" && strings.Contains(defLine, oldName) {
		key := fmt.Sprintf("%s:%d", node.FilePath, node.StartLine)
		if !editSeen[key] {
			editSeen[key] = true
			edits = append(edits, renameEdit{
				File:       node.FilePath,
				Line:       node.StartLine,
				OldText:    defLine,
				NewText:    strings.Replace(defLine, oldName, newName, 1),
				Confidence: "high",
				Reason:     "definition",
			})
		}
	}

	// 2. All graph usages (calls, references, instantiates).
	usages := s.engine.FindUsages(id)
	for _, edge := range usages.Edges {
		if edge.Line == 0 {
			continue
		}
		// Read the source line at the reference.
		srcLine := readSingleLineAt(resolvePath(edge.FilePath), edge.Line)
		if srcLine == "" || !strings.Contains(srcLine, oldName) {
			continue
		}
		key := fmt.Sprintf("%s:%d", edge.FilePath, edge.Line)
		if editSeen[key] {
			continue
		}
		editSeen[key] = true
		edits = append(edits, renameEdit{
			File:       edge.FilePath,
			Line:       edge.Line,
			OldText:    srcLine,
			NewText:    strings.Replace(srcLine, oldName, newName, 1),
			Confidence: "high",
			Reason:     string(edge.Kind),
		})
	}

	// 3. MemberOf edges — if renaming a type, its methods' receiver annotations may reference it.
	if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
		inEdges := s.engine.GetInEdges(id)
		for _, edge := range inEdges {
			if edge.Kind != graph.EdgeMemberOf {
				continue
			}
			memberNode := s.engine.GetSymbol(edge.From)
			if memberNode == nil {
				continue
			}
			// Check if the member's ID contains the old type name (e.g. "file.go::TypeName.MethodName").
			if strings.Contains(memberNode.ID, oldName+".") {
				// The receiver line may mention the type name.
				srcLine := readSingleLineAt(resolvePath(memberNode.FilePath), memberNode.StartLine)
				if srcLine != "" && strings.Contains(srcLine, oldName) {
					key := fmt.Sprintf("%s:%d", memberNode.FilePath, memberNode.StartLine)
					if !editSeen[key] {
						editSeen[key] = true
						edits = append(edits, renameEdit{
							File:       memberNode.FilePath,
							Line:       memberNode.StartLine,
							OldText:    srcLine,
							NewText:    strings.Replace(srcLine, oldName, newName, 1),
							Confidence: "high",
							Reason:     "member receiver",
						})
					}
				}
			}
		}
	}

	// 4. Test files that reference the old name (text search fallback).
	for _, edge := range usages.Edges {
		if !isTestFile(edge.FilePath) {
			continue
		}
		// Already covered by graph edges above, but check for test function names
		// like "TestValidateToken" that contain the old name.
		for _, n := range usages.Nodes {
			if n.FilePath == edge.FilePath && strings.Contains(n.Name, oldName) {
				srcLine := readSingleLineAt(resolvePath(n.FilePath), n.StartLine)
				if srcLine == "" {
					continue
				}
				key := fmt.Sprintf("%s:%d", n.FilePath, n.StartLine)
				if editSeen[key] {
					continue
				}
				editSeen[key] = true
				edits = append(edits, renameEdit{
					File:       n.FilePath,
					Line:       n.StartLine,
					OldText:    srcLine,
					NewText:    strings.Replace(srcLine, oldName, newName, 1),
					Confidence: "medium",
					Reason:     "test function name",
				})
			}
		}
	}

	// Collect affected files.
	fileSet := make(map[string]bool)
	for _, e := range edits {
		fileSet[e.File] = true
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"old_name":       oldName,
		"new_name":       newName,
		"edits":          edits,
		"total_edits":    len(edits),
		"files_affected": len(fileSet),
	})
}

func (s *Server) handleEditSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	oldSource, err := req.RequireString("old_source")
	if err != nil {
		return mcp.NewToolResultError("old_source is required"), nil
	}
	newSource, err := req.RequireString("new_source")
	if err != nil {
		return mcp.NewToolResultError("new_source is required"), nil
	}

	if oldSource == newSource {
		return mcp.NewToolResultError("old_source and new_source are identical"), nil
	}

	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}

	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	// Resolve file path.
	absPath, err := s.resolveNodePath(node)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Read the entire file.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read file: %v", err)), nil
	}

	fileStr := string(content)

	// Extract the symbol's source lines to verify old_source is within them.
	lines := strings.Split(fileStr, "\n")
	if node.StartLine > len(lines) || node.EndLine > len(lines) {
		return mcp.NewToolResultError("symbol line range exceeds file length"), nil
	}

	symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")

	if !strings.Contains(symbolSource, oldSource) {
		// Expand search to include preceding doc comments (agents often include
		// them because get_symbol_source returns context_lines above the symbol).
		expandedStart := node.StartLine - 1
		for expandedStart > 0 {
			trimmed := strings.TrimSpace(lines[expandedStart-1])
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || trimmed == "" {
				expandedStart--
			} else {
				break
			}
		}
		if expandedStart < node.StartLine-1 {
			symbolSource = strings.Join(lines[expandedStart:node.EndLine], "\n")
		}
	}

	if !strings.Contains(symbolSource, oldSource) {
		return mcp.NewToolResultError(fmt.Sprintf(
			"old_source not found within symbol %s (lines %d-%d). Use get_symbol_source to see the current code.",
			id, node.StartLine, node.EndLine)), nil
	}

	// Verify old_source is unique within the symbol.
	if strings.Count(symbolSource, oldSource) > 1 {
		return mcp.NewToolResultError(
			"old_source appears multiple times within the symbol. Provide a larger fragment to make it unique."), nil
	}

	// Apply the edit to the full file content.
	// Find old_source within the symbol's line range only (not the whole file).
	// Use the expanded start if doc comments were included.
	effectiveStart := node.StartLine
	if !strings.Contains(strings.Join(lines[node.StartLine-1:node.EndLine], "\n"), oldSource) {
		// Recalculate expanded start for offset computation.
		expandedStart := node.StartLine - 1
		for expandedStart > 0 {
			trimmed := strings.TrimSpace(lines[expandedStart-1])
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || trimmed == "" {
				expandedStart--
			} else {
				break
			}
		}
		effectiveStart = expandedStart + 1
	}

	symbolStart := 0
	for i := 0; i < effectiveStart-1 && i < len(lines); i++ {
		symbolStart += len(lines[i]) + 1 // +1 for newline
	}

	symbolEnd := symbolStart + len(symbolSource)
	if symbolEnd > len(fileStr) {
		symbolEnd = len(fileStr)
	}

	// Find old_source within the symbol region.
	offset := strings.Index(fileStr[symbolStart:symbolEnd], oldSource)
	if offset < 0 {
		return mcp.NewToolResultError("old_source not found in symbol region"), nil
	}

	// Build the new file content.
	editStart := symbolStart + offset
	editEnd := editStart + len(oldSource)
	newContent := fileStr[:editStart] + newSource + fileStr[editEnd:]

	// Write the file.
	if err := os.WriteFile(absPath, []byte(newContent), 0o644); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not write file: %v", err)), nil
	}
	sess := s.sessionFor(ctx)
	sess.recordModified(node.FilePath)
	sess.recordSymbol(id)

	// Count lines changed.
	oldLines := strings.Count(oldSource, "\n") + 1
	newLines := strings.Count(newSource, "\n") + 1

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"file":         node.FilePath,
		"symbol":       id,
		"lines_before": oldLines,
		"lines_after":  newLines,
		"start_line":   node.StartLine,
		"status":       "applied",
	})
}

// readSingleLineAt reads a single line from an absolute filesystem path.
// Returns "" on error. Caller is responsible for resolving relative graph
// paths to abs first (via Server.resolveGraphPath / resolveNodePath) so a
// missing repo root surfaces as an error instead of silently opening the
// wrong file relative to the daemon process CWD.
func readSingleLineAt(absPath string, lineNum int) string {
	if absPath == "" {
		return ""
	}
	f, err := os.Open(absPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	current := 0
	for scanner.Scan() {
		current++
		if current == lineNum {
			return scanner.Text()
		}
	}
	return ""
}

// extractPrefix returns the common prefix of a camelCase/PascalCase name.
// e.g. "handleGetSymbol" -> "handle", "TestNewServer" -> "Test"
func extractPrefix(name string) string {
	for i := 1; i < len(name); i++ {
		if name[i] >= 'A' && name[i] <= 'Z' {
			return name[:i]
		}
	}
	return name
}
