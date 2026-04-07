package mcp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func (s *Server) registerCodingTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("get_editing_context",
			mcp.WithDescription("The primary tool to call before editing any file. Returns all symbols defined in the file, their signatures, direct dependencies, and immediate callers — everything needed to code without reading raw source lines."),
			mcp.WithString("file_path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithString("detail", mcp.Description("brief or full (default: brief)")),
		),
		s.handleGetEditingContext,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_symbol_signature",
			mcp.WithDescription("Returns only the signature of a function, method, or type — not the body. Use to understand an API boundary without spending tokens on implementation details."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID")),
		),
		s.handleGetSymbolSignature,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_import_path",
			mcp.WithDescription("Given a symbol name you want to use in a file, returns the correct import path. Use instead of reading files or guessing package paths."),
			mcp.WithString("symbol_name", mcp.Required(), mcp.Description("Name of the symbol to import")),
			mcp.WithString("target_file", mcp.Required(), mcp.Description("File where you want to use the symbol")),
		),
		s.handleFindImportPath,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("explain_change_impact",
			mcp.WithDescription("Given a list of symbols you plan to modify, returns risk-tiered blast radius: d=1 will break, d=2 likely affected, d=3 needs testing. Includes affected processes and communities."),
			mcp.WithString("symbol_ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs to modify")),
		),
		s.handleEnhancedChangeImpact,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_symbol_source",
			mcp.WithDescription("Returns the source code of a specific symbol (function, method, type) without reading the entire file. Use instead of Read when you know which symbol you need — saves 70-80% of tokens compared to reading the whole file."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol node ID (e.g. pkg/server.go::HandleRequest)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below the symbol (default: 3)")),
		),
		s.handleGetSymbolSource,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("batch_symbols",
			mcp.WithDescription("Returns signatures, source code, callers, and callees for multiple symbols in one call. Use instead of calling get_symbol_source or get_symbol_signature multiple times — saves 60% round-trip overhead."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for each symbol (default: false)")),
			mcp.WithNumber("context_lines", mcp.Description("Extra lines above/below source (default: 3, only if include_source)")),
		),
		s.handleBatchSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_test_targets",
			mcp.WithDescription("Given changed symbol IDs, traces the call graph to find test files and test functions that exercise those symbols. Use after editing to know exactly which tests to run — no guessing, no running the entire suite."),
			mcp.WithString("symbol_ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithNumber("depth", mcp.Description("Caller traversal depth (default: 3)")),
		),
		s.handleGetTestTargets,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("suggest_pattern",
			mcp.WithDescription("Given an existing symbol as an example, extracts the structural pattern for creating similar code. Returns the example source, sibling symbols with the same pattern, registration/wiring code, test patterns, and files to edit. Use when adding a new function/handler/extractor that follows an existing convention."),
			mcp.WithString("example_id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
		),
		s.handleSuggestPattern,
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

func (s *Server) handleGetEditingContext(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("file_path")
	if err != nil {
		return mcp.NewToolResultError("file_path is required"), nil
	}

	sg := s.engine.GetFileSymbols(fp)
	if len(sg.Nodes) == 0 {
		return mcp.NewToolResultError("no symbols found for file: " + fp), nil
	}

	ctx := editingContext{}

	// File info.
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile {
			ctx.File = map[string]any{"id": n.ID, "language": n.Language}
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
		ctx.Defines = append(ctx.Defines, entry)
	}

	// Imports: outgoing import edges from the file node.
	for _, e := range sg.Edges {
		if e.Kind == graph.EdgeImports {
			importInfo := map[string]any{
				"id":       e.To,
				"external": strings.HasPrefix(e.To, "external::"),
			}
			ctx.Imports = append(ctx.Imports, importInfo)
		}
	}

	// CalledBy: who calls symbols in this file (depth 1).
	callerSeen := make(map[string]bool)
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			callers := s.engine.GetCallers(n.ID, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief"})
			for _, cn := range callers.Nodes {
				if cn.FilePath != fp && !callerSeen[cn.ID] {
					callerSeen[cn.ID] = true
					ctx.CalledBy = append(ctx.CalledBy, map[string]any{
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
			chain := s.engine.GetCallChain(n.ID, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief"})
			for _, cn := range chain.Nodes {
				if cn.FilePath != fp && !callSeen[cn.ID] {
					callSeen[cn.ID] = true
					ctx.Calls = append(ctx.Calls, map[string]any{
						"id":         cn.ID,
						"name":       cn.Name,
						"file_path":  cn.FilePath,
						"start_line": cn.StartLine,
					})
				}
			}
		}
	}

	return mcp.NewToolResultJSON(ctx)
}

func (s *Server) handleGetSymbolSignature(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}
	result := map[string]any{
		"id":         node.ID,
		"kind":       node.Kind,
		"name":       node.Name,
		"file_path":  node.FilePath,
		"start_line": node.StartLine,
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}
	return mcp.NewToolResultJSON(result)
}

func (s *Server) handleFindImportPath(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolName, err := req.RequireString("symbol_name")
	if err != nil {
		return mcp.NewToolResultError("symbol_name is required"), nil
	}
	targetFile, err := req.RequireString("target_file")
	if err != nil {
		return mcp.NewToolResultError("target_file is required"), nil
	}

	candidates := s.engine.FindSymbols(symbolName)
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
	for _, e := range fileSymbols.Edges {
		if e.Kind == graph.EdgeImports && strings.Contains(e.To, filepath.Dir(best.FilePath)) {
			alreadyImported = true
			break
		}
	}

	return mcp.NewToolResultJSON(map[string]any{
		"symbol_id":        best.ID,
		"import_path":      filepath.Dir(best.FilePath),
		"already_imported": alreadyImported,
	})
}

func (s *Server) handleGetRecentChanges(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	return mcp.NewToolResultJSON(map[string]any{
		"changes":             changes,
		"graph_current_as_of": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleGetSymbolSource(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}

	if node.StartLine == 0 || node.EndLine == 0 {
		return mcp.NewToolResultError("symbol has no line range: " + id), nil
	}

	contextLines := req.GetInt("context_lines", 3)

	// Resolve the file path against the indexer's root.
	absPath := node.FilePath
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			absPath = filepath.Join(root, node.FilePath)
		}
	}

	source, startLine, err := readLines(absPath, node.StartLine, node.EndLine, contextLines)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("could not read source: %v", err)), nil
	}

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
	return mcp.NewToolResultJSON(result)
}

// readLines reads lines from a file, with optional context lines above/below.
func readLines(path string, startLine, endLine, contextLines int) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	from := startLine - contextLines
	if from < 1 {
		from = 1
	}
	to := endLine + contextLines

	var lines []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < from {
			continue
		}
		if lineNum > to {
			break
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", 0, err
	}

	return strings.Join(lines, "\n"), from, nil
}

func (s *Server) handleBatchSymbols(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	var results []map[string]any
	for _, id := range ids {
		node := s.engine.GetSymbol(id)
		if node == nil {
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
			callers := s.engine.GetCallers(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
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
			callees := s.engine.GetCallChain(node.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
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
			absPath := node.FilePath
			if s.indexer != nil {
				if root := s.indexer.RootPath(); root != "" {
					absPath = filepath.Join(root, node.FilePath)
				}
			}
			if source, fromLine, err := readLines(absPath, node.StartLine, node.EndLine, contextLines); err == nil {
				entry["source"] = source
				entry["from_line"] = fromLine
			}
		}

		results = append(results, entry)
	}

	return mcp.NewToolResultJSON(map[string]any{
		"symbols": results,
		"total":   len(results),
	})
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

func (s *Server) handleGetTestTargets(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("symbol_ids")
	if err != nil {
		return mcp.NewToolResultError("symbol_ids is required"), nil
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

		// Get all callers up to depth.
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

	return mcp.NewToolResultJSON(map[string]any{
		"test_targets":  targets,
		"run_commands":  runCommands,
		"total_files":   len(targets),
		"uncovered":     uncovered,
		"coverage_note": fmt.Sprintf("%d/%d changed symbols have test coverage", len(coveredSymbols), len(ids)),
	})
}

func (s *Server) handleSuggestPattern(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("example_id")
	if err != nil {
		return mcp.NewToolResultError("example_id is required"), nil
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
		absPath := node.FilePath
		if s.indexer != nil {
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, node.FilePath)
			}
		}
		if source, _, err := readLines(absPath, node.StartLine, node.EndLine, 0); err == nil {
			result["example_source"] = source
		}
	}
	if sig, ok := node.Meta["signature"]; ok {
		result["signature"] = sig
	}

	// 2. Find siblings — same kind, same file, similar naming pattern.
	fileSymbols := s.engine.GetFileSymbols(node.FilePath)
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
			absPath := cn.FilePath
			if s.indexer != nil {
				if root := s.indexer.RootPath(); root != "" {
					absPath = filepath.Join(root, cn.FilePath)
				}
			}
			if source, _, err := readLines(absPath, cn.StartLine, cn.EndLine, 0); err == nil {
				entry["source"] = source
			}
		}
		registration = append(registration, entry)
	}
	result["registration"] = registration

	// 4. Find test patterns — look for test symbols with matching name prefix.
	var testPatterns []map[string]any
	if prefix != "" {
		// Search for test functions that match the example name.
		testSearch := s.engine.SearchSymbols(node.Name, 20)
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
				absPath := tn.FilePath
				if s.indexer != nil {
					if root := s.indexer.RootPath(); root != "" {
						absPath = filepath.Join(root, tn.FilePath)
					}
				}
				if source, _, err := readLines(absPath, tn.StartLine, tn.EndLine, 0); err == nil {
					entry["source"] = source
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

	return mcp.NewToolResultJSON(result)
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
