package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/query"
)

func (s *Server) registerCoreTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("index_repository",
			mcp.WithDescription("Index or re-index a local repository path into Gortex. Call once at session start if not already running with --watch."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
		),
		s.handleIndexRepository,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_symbol",
			mcp.WithDescription("Use instead of Read to locate a function, type, interface, or variable definition. Returns location and signature without reading the whole file."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID (e.g. pkg/server.go::HandleRequest)")),
			mcp.WithString("detail", mcp.Description("brief or full (default: brief)")),
		),
		s.handleGetSymbol,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("search_symbols",
			mcp.WithDescription("Use instead of Grep to find symbols by name across the whole codebase. Faster and token-free compared to searching file contents."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Symbol name to search for")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
		),
		s.handleSearchSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_file_summary",
			mcp.WithDescription("Use instead of Read to understand a file's role: returns all its symbols and imports without reading source lines."),
			mcp.WithString("file_path", mcp.Required(), mcp.Description("Relative file path")),
		),
		s.handleGetFileSummary,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_dependencies",
			mcp.WithDescription("Returns what a symbol or file depends on — imports, calls, type references — without reading any files. Use before editing to understand incoming contracts."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleGetDependencies,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_dependents",
			mcp.WithDescription("Returns everything that depends on this symbol (blast radius). Call before changing a function or type to know what else must be updated."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 3)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleGetDependents,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_call_chain",
			mcp.WithDescription("Traces the call graph forward from a function without reading source. Use to understand what a function ultimately triggers."),
			mcp.WithString("function_id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 4)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleGetCallChain,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_callers",
			mcp.WithDescription("Returns all callers of a function without reading source. Use instead of Grep when you need to know who calls a function."),
			mcp.WithString("function_id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleGetCallers,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_implementations",
			mcp.WithDescription("Finds all concrete types that implement an interface. Use before changing an interface to identify all types that will be affected."),
			mcp.WithString("interface_id", mcp.Required(), mcp.Description("Interface node ID")),
		),
		s.handleFindImplementations,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_usages",
			mcp.WithDescription("Use instead of Grep to find every reference to a symbol across the codebase. Returns precise locations with zero false positives."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleFindUsages,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_cluster",
			mcp.WithDescription("Returns the immediate neighbourhood around a node — all symbols it touches and that touch it. Useful for understanding a module's coupling before refactoring."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("radius", mcp.Description("Bidirectional hops (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
		),
		s.handleGetCluster,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("graph_stats",
			mcp.WithDescription("Returns a compact summary of the indexed codebase: node/edge counts by kind and language. Call at session start to orient Claude in an unfamiliar repo."),
		),
		s.handleGraphStats,
	)
}

func (s *Server) handleIndexRepository(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}
	result, err := s.indexer.Index(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultJSON(result)
}

func (s *Server) handleGetSymbol(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	node := s.engine.GetSymbol(id)
	if node == nil {
		return mcp.NewToolResultError("symbol not found: " + id), nil
	}

	detail := req.GetString("detail", "brief")
	if detail == "brief" {
		return mcp.NewToolResultJSON(node.Brief())
	}

	// Full: include node + direct edges.
	out := s.engine.GetOutEdges(node.ID)
	in := s.engine.GetInEdges(node.ID)
	return mcp.NewToolResultJSON(map[string]any{
		"node":      node,
		"out_edges": out,
		"in_edges":  in,
	})
}

func (s *Server) handleSearchSymbols(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := req.GetInt("limit", 20)

	// Use fuzzy/substring search with relevance ranking
	nodes := s.engine.SearchSymbols(q, limit+10) // fetch extra for total count

	var results []map[string]any
	total := len(nodes)
	for i, n := range nodes {
		if i >= limit {
			break
		}
		results = append(results, n.Brief())
	}
	return mcp.NewToolResultJSON(map[string]any{
		"results":   results,
		"total":     total,
		"truncated": total > limit,
	})
}

func (s *Server) handleGetFileSummary(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("file_path")
	if err != nil {
		return mcp.NewToolResultError("file_path is required"), nil
	}
	sg := s.engine.GetFileSymbols(fp)
	if len(sg.Nodes) == 0 {
		return mcp.NewToolResultError("no symbols found for file: " + fp), nil
	}
	return mcp.NewToolResultJSON(sg)
}

func (s *Server) handleGetDependencies(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	opts := query.QueryOptions{
		Depth:  req.GetInt("depth", 2),
		Limit:  req.GetInt("limit", 50),
		Detail: "brief",
	}
	return mcp.NewToolResultJSON(s.engine.GetDependencies(id, opts))
}

func (s *Server) handleGetDependents(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	opts := query.QueryOptions{
		Depth:  req.GetInt("depth", 3),
		Limit:  req.GetInt("limit", 50),
		Detail: "brief",
	}
	return mcp.NewToolResultJSON(s.engine.GetDependents(id, opts))
}

func (s *Server) handleGetCallChain(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("function_id")
	if err != nil {
		return mcp.NewToolResultError("function_id is required"), nil
	}
	opts := query.QueryOptions{
		Depth:  req.GetInt("depth", 4),
		Limit:  req.GetInt("limit", 50),
		Detail: "brief",
	}
	return mcp.NewToolResultJSON(s.engine.GetCallChain(id, opts))
}

func (s *Server) handleGetCallers(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("function_id")
	if err != nil {
		return mcp.NewToolResultError("function_id is required"), nil
	}
	opts := query.QueryOptions{
		Depth:  req.GetInt("depth", 2),
		Limit:  req.GetInt("limit", 50),
		Detail: "brief",
	}
	return mcp.NewToolResultJSON(s.engine.GetCallers(id, opts))
}

func (s *Server) handleFindImplementations(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("interface_id")
	if err != nil {
		return mcp.NewToolResultError("interface_id is required"), nil
	}
	impls := s.engine.FindImplementations(id)
	var results []map[string]any
	for _, n := range impls {
		results = append(results, n.Brief())
	}
	return mcp.NewToolResultJSON(results)
}

func (s *Server) handleFindUsages(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	sg := s.engine.FindUsages(id)
	return mcp.NewToolResultJSON(sg)
}

func (s *Server) handleGetCluster(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	opts := query.QueryOptions{
		Depth:  req.GetInt("radius", 2),
		Limit:  req.GetInt("limit", 50),
		Detail: "brief",
	}
	return mcp.NewToolResultJSON(s.engine.GetCluster(id, opts))
}

func (s *Server) handleGraphStats(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultJSON(s.engine.Stats())
}

