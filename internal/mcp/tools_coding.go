package mcp

import (
	"context"
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
		"changes":              changes,
		"graph_current_as_of": time.Now().Format(time.RFC3339),
	})
}

