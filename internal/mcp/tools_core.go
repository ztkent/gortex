package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// minTierParamDescription is the `min_tier` parameter description shared by
// every edge-returning tool. Mentioning the tier vocabulary inline lets agents
// pick an appropriate filter without consulting external docs.
const minTierParamDescription = "Filter edges by minimum confidence tier. " +
	"Values (highest to lowest): lsp_resolved (compiler-verified), " +
	"lsp_dispatch (interface→impl via semantic provider), " +
	"ast_resolved (tree-sitter direct match), " +
	"ast_inferred (type heuristic), " +
	"text_matched (name-only). Omit for no filter. " +
	"Use lsp_resolved for high-stakes refactors where false positives are expensive."

// isCompact checks if the compact flag is set in the request.
func isCompact(req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["compact"].(bool); ok {
		return v
	}
	return false
}

// isTOON reports whether the caller requested the TOON wire format
// for this tool call. Selection mirrors `Server.isGCX`:
//
//  1. Explicit `format` arg wins.
//  2. Otherwise the per-session default (driven by MCP `clientInfo`)
//     decides — TOON is the second-tier compact format used when a
//     client decodes TOON but not GCX. Today no shipping client is
//     known to be in this bucket; the helper exists for forward
//     compat as plugins evolve.
//  3. Default false — JSON wins.
func (s *Server) isTOON(ctx context.Context, req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok && v != "" {
		return v == "toon"
	}
	if s == nil {
		return false
	}
	return s.resolveSessionFormat(ctx) == "toon"
}

// toonNodeRow is a TOON-optimized flat representation of a graph node.
type toonNodeRow struct {
	ID        string `toon:"id"`
	Kind      string `toon:"kind"`
	Name      string `toon:"name"`
	FilePath  string `toon:"file_path"`
	StartLine int    `toon:"start_line"`
}

// toonEdgeRow is a TOON-optimized flat representation of a graph edge.
type toonEdgeRow struct {
	From       string  `toon:"from"`
	To         string  `toon:"to"`
	Kind       string  `toon:"kind"`
	Confidence float64 `toon:"confidence"`
	Label      string  `toon:"label"`
}

// toonSubGraphResult wraps nodes and edges for TOON tabular output.
type toonSubGraphResult struct {
	Nodes     []toonNodeRow `toon:"nodes"`
	Edges     []toonEdgeRow `toon:"edges"`
	Total     int           `toon:"total"`
	Truncated bool          `toon:"truncated"`
}

// toonSearchResult wraps search results for TOON tabular output.
type toonSearchResult struct {
	Results   []toonNodeRow `toon:"results"`
	Total     int           `toon:"total"`
	Truncated bool          `toon:"truncated"`
}

// nodesToTOONRows converts graph nodes to flat TOON rows.
func nodesToTOONRows(nodes []*graph.Node) []toonNodeRow {
	rows := make([]toonNodeRow, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		rows = append(rows, toonNodeRow{
			ID:        n.ID,
			Kind:      string(n.Kind),
			Name:      n.Name,
			FilePath:  n.FilePath,
			StartLine: n.StartLine,
		})
	}
	return rows
}

// returnSubGraph returns a SubGraph in the requested format (JSON, compact, GCX, or TOON).
// Method on Server so the format negotiation can consult per-session
// client identity (claude-code → gcx, etc.).
func (s *Server) returnSubGraph(ctx context.Context, req mcp.CallToolRequest, sg *query.SubGraph) (*mcp.CallToolResult, error) {
	if isCompact(req) {
		return mcp.NewToolResultText(compactSubGraph(sg)), nil
	}
	if s.isGCX(ctx, req) {
		tool := requestToolName(req)
		if tool == "" {
			tool = "subgraph"
		}
		return gcxResponse(encodeSubGraph(tool, sg))
	}
	if s.isTOON(ctx, req) {
		return subGraphToTOON(sg)
	}
	return s.respondJSONOrTOON(ctx, req, sg)
}

// requestToolName extracts the MCP tool name from a CallToolRequest.
// mcp-go surfaces the name on req.Params.Name so we can route multiple
// subgraph-returning tools through the same encoder with distinct
// header tags.
func requestToolName(req mcp.CallToolRequest) string {
	return req.Params.Name
}

// returnTOON marshals payload as TOON and returns a text result. It
// goes JSON-first so the on-wire field names match the JSON schema
// every tool already advertises: toon-go honours only `toon:` tags
// and rejects map[int]X / non-string keys outright, but every Gortex
// payload tags its fields with `json:` (we don't double-tag with
// `toon:`). Round-tripping through JSON gives us tag-driven naming
// and string-key normalisation (Go's encoding/json stringifies int
// keys) for free, with a single allocation we can amortise across
// the tool surface.
//
// Falls back to JSON on encoder error so a malformed payload can
// never take down the response — the caller never sees a half-
// written document.
func returnTOON(payload any) (*mcp.CallToolResult, error) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	data, err := toon.Marshal(generic)
	if err != nil {
		return mcp.NewToolResultJSON(payload)
	}
	return mcp.NewToolResultText(string(data)), nil
}

// respondJSONOrTOON is the bottom-of-the-handler decision shared by
// every tool that advertises `format` in its schema and lets a
// per-tool GCX encoder run ahead of it. It returns TOON when the
// caller (or the per-session default) asks for it and JSON otherwise.
// GCX is handled inline at the call site because GCX uses hand-tuned
// per-tool encoders rather than reusing the JSON shape.
func (s *Server) respondJSONOrTOON(ctx context.Context, req mcp.CallToolRequest, payload any) (*mcp.CallToolResult, error) {
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return mcp.NewToolResultJSON(payload)
}

// subGraphToTOON converts a SubGraph to a TOON-encoded text result.
func subGraphToTOON(sg *query.SubGraph) (*mcp.CallToolResult, error) {
	var edgeRows []toonEdgeRow
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		edgeRows = append(edgeRows, toonEdgeRow{
			From:       e.From,
			To:         e.To,
			Kind:       string(e.Kind),
			Confidence: e.Confidence,
			Label:      label,
		})
	}
	result := toonSubGraphResult{
		Nodes:     nodesToTOONRows(sg.Nodes),
		Edges:     edgeRows,
		Total:     sg.TotalNodes,
		Truncated: sg.Truncated,
	}
	data, err := toon.Marshal(result)
	if err != nil {
		return mcp.NewToolResultJSON(sg)
	}
	return mcp.NewToolResultText(string(data)), nil
}

// resolveRepoFilter resolves the optional repo/project/ref params into a set
// of allowed repo prefixes. Returns nil when no filtering is needed (all repos).
// When an active project is set and no explicit filter is provided, the active
// project scope is applied as the default. If the active project cannot be
// resolved (typically a stale name in config, no matching projects map), the
// fallback degrades to "no filter" with a warning log instead of a hard error,
// mirroring ConfigManager.ActiveRepos so tool calls stay usable.
func (s *Server) resolveRepoFilter(req mcp.CallToolRequest) (map[string]bool, error) {
	repo := req.GetString("repo", "")
	project := req.GetString("project", "")
	ref := req.GetString("ref", "")

	// Track whether `project` was set by the caller (explicit) or by the
	// active-project default. An explicit unknown project is still a hard
	// error (the caller asked for X by name, deserves to know X is wrong);
	// a stale active-project default falls back to "all repos" so a single
	// misconfigured config line does not break every per-repo MCP call.
	projectFromActive := false
	if repo == "" && project == "" && ref == "" && s.activeProject != "" {
		project = s.activeProject
		projectFromActive = true
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
			if projectFromActive {
				// Stale active-project default. Log and degrade to no
				// filter (all repos) so the call still succeeds. This
				// mirrors ConfigManager.ActiveRepos behavior.
				if s.logger != nil {
					s.logger.Warn("active project not resolvable, falling back to all repos",
						zap.String("active_project", project),
						zap.Error(err))
				}
				return nil, nil
			}
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

// filterNodesByKind keeps only nodes whose Kind is in the comma-
// separated list. Empty / unknown kinds in the input are ignored
// (treated as "no constraint of this name") so a typo is graceful
// rather than silently empty. Case-insensitive.
//
// Used by search_symbols' `kind` argument — lets callers scope a
// query to one of the domain-specific node kinds (todo, license,
// team, …) without paying the cost of an unrelated BM25 prefix
// match.
func filterNodesByKind(nodes []*graph.Node, kindArg string) []*graph.Node {
	want := make(map[string]struct{})
	for k := range strings.SplitSeq(kindArg, ",") {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" {
			continue
		}
		want[k] = struct{}{}
	}
	if len(want) == 0 {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if _, ok := want[strings.ToLower(string(n.Kind))]; ok {
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

// enrichSubGraphEdges populates ConfidenceLabel and Origin on every edge in
// a SubGraph. Origin is backfilled from kind + confidence + semantic_source
// meta when unset so clients see a tier on every edge.
func enrichSubGraphEdges(sg *query.SubGraph) {
	for _, e := range sg.Edges {
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		if e.Origin == "" {
			src, _ := e.Meta["semantic_source"].(string)
			e.Origin = graph.DefaultOriginFor(e.Kind, e.Confidence, src)
		}
	}
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
	// Append edge confidence distribution.
	if len(sg.Edges) > 0 {
		counts := map[string]int{}
		for _, e := range sg.Edges {
			label := e.ConfidenceLabel
			if label == "" {
				label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
			}
			counts[label]++
		}
		fmt.Fprintf(&b, "edges: %d total", len(sg.Edges))
		for _, label := range []string{"EXTRACTED", "INFERRED", "AMBIGUOUS"} {
			if c := counts[label]; c > 0 {
				fmt.Fprintf(&b, ", %d %s", c, label)
			}
		}
		b.WriteByte('\n')
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
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format — round-trippable, ~40% fewer tokens), or toon")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("kind", mcp.Description("Filter to one or more node kinds (comma-separated). Standard kinds: function, method, type, interface, variable, constant, field, file, package, import, contract. Coverage kinds: param, closure, enum_member, generic_param, module, table, column, config_key, flag, event, migration, fixture, todo, team, license, release.")),
		),
		s.handleSearchSymbols,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_file_summary",
			mcp.WithDescription("Use instead of Read to understand a file's role: returns all its symbols and imports without reading source lines."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
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
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
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
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleGetDependents,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_call_chain",
			mcp.WithDescription("Traces the call graph forward from a function without reading source. Use to understand what a function ultimately triggers."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 4)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleGetCallChain,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_callers",
			mcp.WithDescription("Returns all callers of a function without reading source. Use instead of Grep when you need to know who calls a function."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 2)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop callers originating in test functions (set true when you want production callers only)")),
		),
		s.handleGetCallers,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_implementations",
			mcp.WithDescription("Finds all concrete types that implement an interface. Use before changing an interface to identify all types that will be affected."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Interface node ID")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleFindImplementations,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_overrides",
			mcp.WithDescription("Finds all methods that override the given method (children) or the parent methods it overrides. Backed by EdgeOverrides materialised at index time and promoted to lsp_dispatch when an LSP is available."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Method node ID")),
			mcp.WithString("direction", mcp.Description("'children' (default — overriders) or 'parents' (overridden methods)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleFindOverrides,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("find_usages",
			mcp.WithDescription("Use instead of Grep to find every reference to a symbol across the codebase. Returns precise locations with zero false positives."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop references originating in test functions (set true to see only production usages)")),
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
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleGetCluster,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_repo_outline",
			mcp.WithDescription("Narrative single-call overview of the indexed codebase: primary languages, top communities, load-bearing hotspots, most-imported files, and entry points. Use at session start (or when onboarding to an unfamiliar repo) instead of assembling this from graph_stats + analyze + manual inspection. Output stays under ~1k tokens."),
		),
		s.handleGetRepoOutline,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("graph_stats",
			mcp.WithDescription("Returns a compact summary of the indexed codebase: node/edge counts by kind and language. Call at session start to orient Claude in an unfamiliar repo."),
		),
		s.handleGraphStats,
	)
}

func (s *Server) handleIndexRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// In multi-repo mode, route through multiIndexer so nodes get the correct
	// RepoPrefix and byRepo stays consistent. Using the shared singleton
	// indexer here produces unprefixed nodes that corrupt per-repo stats.
	if s.multiIndexer != nil {
		// Accept either a tracked prefix directly or a filesystem path.
		// Falls back to reconciling from persisted config so users don't
		// have to re-track repos the daemon dropped across warmup (T0.3).
		prefix := s.resolveRepoPrefixOrReconcile(ctx, path)
		if prefix == "" {
			return mcp.NewToolResultError(fmt.Sprintf(
				"path %q is not a tracked repository; use track_repository to add it",
				path)), nil
		}
		result, err := s.multiIndexer.IndexRepo(prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		s.RunAnalysis()
		return s.respondJSONOrTOON(ctx, req, result)
	}

	result, err := s.indexer.IndexCtx(s.progressCtx(ctx, req), path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	s.RunAnalysis()
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleGetSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	s.sessionFor(ctx).recordSymbol(id)

	detail := req.GetString("detail", "brief")
	if detail == "brief" {
		return s.respondJSONOrTOON(ctx, req, node.Brief())
	}

	// Full: include node + direct edges.
	out := s.engine.GetOutEdges(node.ID)
	in := s.engine.GetInEdges(node.ID)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"node":      node,
		"out_edges": out,
		"in_edges":  in,
	})
}

func (s *Server) handleSearchSymbols(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	limit := req.GetInt("limit", 20)

	sess := s.sessionFor(ctx)
	sess.recordSearch(q)

	// Apply server-default scope merged with caller args. `workspace`
	// / `project` args win per-field; empty falls through to the
	// server's --workspace flag. SearchSymbolsScoped over-fetches and
	// post-filters, so ranking is preserved while results stay inside
	// the boundary.
	workspaceArg := req.GetString("workspace", "")
	projectArg := req.GetString("project", "")
	scopeWS, scopeProj := s.resolveQueryScope(workspaceArg, projectArg)
	scope := query.QueryOptions{WorkspaceID: scopeWS, ProjectID: scopeProj}
	nodes := s.engine.SearchSymbolsScoped(q, limit+10, scope)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	nodes = filterNodes(nodes, allowed)

	// kind filter so callers can scope to a single new node kind
	// (todo, license, team, module, …). Comma-separated list —
	// case-insensitive — applied post-search so BM25 ranking is
	// preserved within the kept set.
	if kindArg := strings.TrimSpace(req.GetString("kind", "")); kindArg != "" {
		nodes = filterNodesByKind(nodes, kindArg)
	}

	// Rerank: fold combo + frecency signals over the backend's BM25 order.
	// Both signals are per-repo and zero-valued until the agent has spent
	// some time in the codebase, so cold queries return BM25 order verbatim.
	nodes = applyRerankBoosts(nodes, s.combo, s.frecency, q)

	// Remember the returned IDs for attribution on later consume calls.
	// Cap at top limit so unseen "overflow" results don't get credited.
	recordLastSearchFromNodes(sess, q, nodes, limit)

	if isCompact(req) {
		if len(nodes) > limit {
			nodes = nodes[:limit]
		}
		return mcp.NewToolResultText(compactNodes(nodes)), nil
	}

	total := len(nodes)

	if s.isGCX(ctx, req) {
		return gcxResponse(encodeSearchSymbols(nodes, total, limit))
	}

	if len(nodes) > limit {
		nodes = nodes[:limit]
	}

	if s.isTOON(ctx, req) {
		result := toonSearchResult{
			Results:   nodesToTOONRows(nodes),
			Total:     total,
			Truncated: total > limit,
		}
		data, err := toon.Marshal(result)
		if err == nil {
			return mcp.NewToolResultText(string(data)), nil
		}
	}

	var results []map[string]any
	for _, n := range nodes {
		results = append(results, n.Brief())
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"results":   results,
		"total":     total,
		"truncated": total > limit,
	})
}

func (s *Server) handleGetFileSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
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

	// ETag conditional fetch.
	etag := computeETag(sg)
	if ifNoneMatch := req.GetString("if_none_match", ""); ifNoneMatch != "" && ifNoneMatch == etag {
		return notModifiedResult(etag), nil
	}

	if s.isGCX(ctx, req) {
		return gcxResponse(encodeFileSummary(sg, etag))
	}

	// Wrap with etag in response.
	result := map[string]any{
		"nodes":       sg.Nodes,
		"edges":       sg.Edges,
		"total_nodes": len(sg.Nodes),
		"total_edges": len(sg.Edges),
		"truncated":   sg.Truncated,
		"etag":        etag,
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

func (s *Server) handleGetDependencies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	scopeWS, scopeProj := s.scopeFromRequest(&req)
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 2),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: scopeWS,
		ProjectID:   scopeProj,
	}
	sg := s.engine.GetDependencies(id, opts)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGetDependents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	scopeWS, scopeProj := s.scopeFromRequest(&req)
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 3),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: scopeWS,
		ProjectID:   scopeProj,
	}
	sg := s.engine.GetDependents(id, opts)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGetCallChain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	scopeWS, scopeProj := s.scopeFromRequest(&req)
	opts := query.QueryOptions{
		Depth:       req.GetInt("depth", 4),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		MinTier:     minTier,
		WorkspaceID: scopeWS,
		ProjectID:   scopeProj,
	}
	sg := s.engine.GetCallChain(id, opts)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGetCallers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	scopeWS, scopeProj := s.scopeFromRequest(&req)
	opts := query.QueryOptions{
		Depth:        req.GetInt("depth", 2),
		Limit:        req.GetInt("limit", 50),
		Detail:       "brief",
		MinTier:      minTier,
		WorkspaceID:  scopeWS,
		ProjectID:    scopeProj,
		ExcludeTests: req.GetBool("exclude_tests", false),
	}
	sg := s.engine.GetCallers(id, opts)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleFindOverrides(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	direction := req.GetString("direction", "children")
	minTier := req.GetString("min_tier", "")
	var nodes []*graph.Node
	switch direction {
	case "parents", "overridden":
		nodes = s.engine.FindOverridden(id)
	default:
		nodes = s.engine.FindOverridesMinTier(id, minTier)
	}

	if s.isGCX(ctx, req) {
		sg := &query.SubGraph{Nodes: nodes, TotalNodes: len(nodes)}
		return s.returnSubGraph(ctx, req, sg)
	}
	if s.isTOON(ctx, req) {
		result := struct {
			Overrides []toonNodeRow `toon:"overrides"`
			Total     int           `toon:"total"`
		}{
			Overrides: nodesToTOONRows(nodes),
			Total:     len(nodes),
		}
		if data, err := toon.Marshal(result); err == nil {
			return mcp.NewToolResultText(string(data)), nil
		}
	}
	results := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		results = append(results, n.Brief())
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"overrides": results,
		"total":     len(results),
		"direction": direction,
	})
}

func (s *Server) handleFindImplementations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")
	impls := s.engine.FindImplementationsMinTier(id, minTier)

	if s.isGCX(ctx, req) {
		sg := &query.SubGraph{Nodes: impls, TotalNodes: len(impls)}
		return s.returnSubGraph(ctx, req, sg)
	}

	if s.isTOON(ctx, req) {
		result := struct {
			Implementations []toonNodeRow `toon:"implementations"`
			Total           int           `toon:"total"`
		}{
			Implementations: nodesToTOONRows(impls),
			Total:           len(impls),
		}
		if data, err := toon.Marshal(result); err == nil {
			return mcp.NewToolResultText(string(data)), nil
		}
	}

	var results []map[string]any
	for _, n := range impls {
		results = append(results, n.Brief())
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"implementations": results,
		"total":           len(results),
	})
}

func (s *Server) handleFindUsages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	minTier := req.GetString("min_tier", "")

	// find_usages on a tuck symbol returns hits only from tuck.
	// Server-level --workspace + caller `workspace` arg compose the
	// same way as on search_symbols.
	workspaceArg := req.GetString("workspace", "")
	projectArg := req.GetString("project", "")
	scopeWS, scopeProj := s.resolveQueryScope(workspaceArg, projectArg)
	sg := s.engine.FindUsagesScoped(id, query.QueryOptions{
		WorkspaceID:  scopeWS,
		ProjectID:    scopeProj,
		ExcludeTests: req.GetBool("exclude_tests", false),
	})

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	if s.isGCX(ctx, req) {
		return gcxResponse(encodeFindUsages(sg))
	}
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGetCluster(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	scopeWS, scopeProj := s.scopeFromRequest(&req)
	opts := query.QueryOptions{
		Depth:       req.GetInt("radius", 2),
		Limit:       req.GetInt("limit", 50),
		Detail:      "brief",
		WorkspaceID: scopeWS,
		ProjectID:   scopeProj,
	}
	sg := s.engine.GetCluster(id, opts)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGraphStats(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	// Include session-level token savings. Per-session when the caller
	// has a session ID (daemon path); shared default for embedded/stdio.
	result["token_savings"] = s.tokenStatsFor(ctx).snapshot()

	// Include cumulative cross-session savings when a persistent store is wired.
	if cs := s.cumulativeSavingsSnapshot(); cs != nil {
		result["cumulative_savings"] = cs
	}

	// Include semantic enrichment stats.
	if s.semanticMgr != nil && s.semanticMgr.Enabled() {
		result["semantic"] = map[string]any{
			"enabled":   true,
			"providers": s.semanticMgr.Stats(),
		}
	}

	return s.respondJSONOrTOON(ctx, req, result)
}
