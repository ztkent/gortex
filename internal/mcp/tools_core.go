package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// isCompact checks if the compact flag is set in the request.
func isCompact(req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["compact"].(bool); ok {
		return v
	}
	return false
}

// resolveRepoFilter resolves the optional repo/project/ref params into a set
// of allowed repo prefixes. Returns nil when no filtering is needed (all repos).
// When an active project is set and no explicit filter is provided, the active
// project scope is applied as the default.
func (s *Server) resolveRepoFilter(req mcp.CallToolRequest) (map[string]bool, error) {
	repo := req.GetString("repo", "")
	project := req.GetString("project", "")
	ref := req.GetString("ref", "")

	// Apply active project as default scope when no explicit filter is provided.
	if repo == "" && project == "" && ref == "" && s.activeProject != "" {
		project = s.activeProject
	}

	if repo == "" && project == "" && ref == "" {
		return nil, nil // no filter — search all repos
	}

	// Direct repo filter — just that one prefix.
	if repo != "" {
		return map[string]bool{repo: true}, nil
	}

	// Resolve project/ref via ConfigManager.
	if s.configManager == nil {
		return nil, fmt.Errorf("configuration manager is not available")
	}

	gc := s.configManager.Global()

	var entries []config.RepoEntry
	if project != "" {
		repos, err := gc.ResolveRepos(project)
		if err != nil {
			return nil, err
		}
		entries = repos
	} else {
		// ref without project — collect all repos from all projects.
		for _, proj := range gc.Projects {
			entries = append(entries, proj.Repos...)
		}
		// Also include top-level repos.
		entries = append(entries, gc.Repos...)
	}

	// Apply ref filter if set.
	allowed := make(map[string]bool)
	for _, e := range entries {
		if ref != "" && e.Ref != ref {
			continue
		}
		allowed[config.ResolvePrefix(e)] = true
	}

	return allowed, nil
}

// filterNodes returns only nodes whose RepoPrefix is in the allowed set.
// If allowed is nil, returns the original slice unchanged.
func filterNodes(nodes []*graph.Node, allowed map[string]bool) []*graph.Node {
	if allowed == nil {
		return nodes
	}
	var out []*graph.Node
	for _, n := range nodes {
		// In single-repo mode, nodes have empty RepoPrefix — always include them.
		if n.RepoPrefix == "" || allowed[n.RepoPrefix] {
			out = append(out, n)
		}
	}
	return out
}

// filterSubGraph returns a new SubGraph with only nodes/edges whose endpoints
// are in the allowed repo set. If allowed is nil, returns sg unchanged.
func filterSubGraph(sg *query.SubGraph, allowed map[string]bool) *query.SubGraph {
	if allowed == nil {
		return sg
	}
	nodeIDs := make(map[string]bool)
	var nodes []*graph.Node
	for _, n := range sg.Nodes {
		if n.RepoPrefix == "" || allowed[n.RepoPrefix] {
			nodes = append(nodes, n)
			nodeIDs[n.ID] = true
		}
	}
	var edges []*graph.Edge
	for _, e := range sg.Edges {
		if nodeIDs[e.From] || nodeIDs[e.To] {
			edges = append(edges, e)
		}
	}
	return &query.SubGraph{
		Nodes:      nodes,
		Edges:      edges,
		TotalNodes: len(nodes),
		TotalEdges: len(edges),
		Truncated:  sg.Truncated,
	}
}

// compactNodes formats nodes as one-line-per-symbol text.
// Format: "kind qualifiedName file_path:start_line"
// For methods, qualifiedName includes the receiver (e.g., "Indexer.Index")
// so the output can be combined with file_path to reconstruct the full node ID.
func compactNodes(nodes []*graph.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s:%d\n", n.Kind, qualifiedName(n), n.FilePath, n.StartLine)
	}
	return b.String()
}

// qualifiedName returns the symbol part of a node ID (after "::").
// For methods this includes the receiver type (e.g., "Indexer.Index"),
// for functions/types it's the plain name.
func qualifiedName(n *graph.Node) string {
	if idx := strings.LastIndex(n.ID, "::"); idx >= 0 {
		return n.ID[idx+2:]
	}
	return n.Name
}

// compactSubGraph formats a SubGraph as compact text.
func compactSubGraph(sg *query.SubGraph) string {
	var b strings.Builder
	for _, n := range sg.Nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s:%d\n", n.Kind, qualifiedName(n), n.FilePath, n.StartLine)
	}
	if sg.Truncated {
		fmt.Fprintf(&b, "... truncated (%d total)\n", sg.TotalNodes)
	}
	return b.String()
}

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
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleGetSymbol,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("search_symbols",
			mcp.WithDescription("Use instead of Grep to find symbols across the whole codebase. Supports natural language queries with camelCase-aware tokenization and BM25 ranking — 'validate token auth' finds validateToken, AuthMiddleware, parseJWT."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query — can be symbol name, concept, or multiple keywords")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
			mcp.WithBoolean("compact", mcp.Description("Return one-line-per-result text instead of JSON objects (saves 50-70% tokens)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleSearchSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_file_summary",
			mcp.WithDescription("Use instead of Read to understand a file's role: returns all its symbols and imports without reading source lines."),
			mcp.WithString("file_path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleGetFileSummary,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_dependencies",
			mcp.WithDescription("Returns what a symbol or file depends on — imports, calls, type references — without reading any files. Use before editing to understand incoming contracts."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
		),
		s.handleGetDependencies,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_dependents",
			mcp.WithDescription("Returns everything that depends on this symbol (blast radius). Call before changing a function or type to know what else must be updated."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 3)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
		),
		s.handleGetDependents,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_call_chain",
			mcp.WithDescription("Traces the call graph forward from a function without reading source. Use to understand what a function ultimately triggers."),
			mcp.WithString("function_id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 4)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleGetCallChain,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_callers",
			mcp.WithDescription("Returns all callers of a function without reading source. Use instead of Grep when you need to know who calls a function."),
			mcp.WithString("function_id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
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
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleFindUsages,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_cluster",
			mcp.WithDescription("Returns the immediate neighbourhood around a node — all symbols it touches and that touch it. Useful for understanding a module's coupling before refactoring."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("radius", mcp.Description("Bidirectional hops (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
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
	s.RunAnalysis()
	return mcp.NewToolResultJSON(result)
}

func (s *Server) handleGetSymbol(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	if allowed != nil && node.RepoPrefix != "" && !allowed[node.RepoPrefix] {
		return mcp.NewToolResultError("symbol not found in specified scope: " + id), nil
	}

	s.session.recordSymbol(id)

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

	s.session.recordSearch(q)
	nodes := s.engine.SearchSymbols(q, limit+10)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	nodes = filterNodes(nodes, allowed)

	if isCompact(req) {
		if len(nodes) > limit {
			nodes = nodes[:limit]
		}
		return mcp.NewToolResultText(compactNodes(nodes)), nil
	}

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

	// Auto re-index stale file before querying.
	s.ensureFresh([]string{fp})

	sg := s.engine.GetFileSymbols(fp)
	if len(sg.Nodes) == 0 {
		return mcp.NewToolResultError("no symbols found for file: " + fp), nil
	}

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	if len(sg.Nodes) == 0 {
		return mcp.NewToolResultError("no symbols found for file in specified scope: " + fp), nil
	}

	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
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
	sg := s.engine.GetDependencies(id, opts)
	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	return mcp.NewToolResultJSON(sg)
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
	sg := s.engine.GetDependents(id, opts)
	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	return mcp.NewToolResultJSON(sg)
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
	sg := s.engine.GetCallChain(id, opts)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)

	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	return mcp.NewToolResultJSON(sg)
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
	sg := s.engine.GetCallers(id, opts)
	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	return mcp.NewToolResultJSON(sg)
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
	return mcp.NewToolResultJSON(map[string]any{
		"implementations": results,
		"total":           len(results),
	})
}

func (s *Server) handleFindUsages(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	sg := s.engine.FindUsages(id)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)

	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
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
	sg := s.engine.GetCluster(id, opts)
	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	return mcp.NewToolResultJSON(sg)
}

func (s *Server) handleGraphStats(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stats := s.engine.Stats()
	result := map[string]any{
		"total_nodes": stats.TotalNodes,
		"total_edges": stats.TotalEdges,
		"by_kind":     stats.ByKind,
		"by_language": stats.ByLanguage,
	}

	// Include per-repo stats when in multi-repo mode.
	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		result["per_repo"] = s.graph.RepoStats()
	}

	// Include session-level token savings.
	result["token_savings"] = s.tokenStats.snapshot()

	return mcp.NewToolResultJSON(result)
}
