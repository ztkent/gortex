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
	IsTest    bool   `toon:"is_test"`
	TestRole  string `toon:"test_role"`
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
		isTest, _ := n.Meta["is_test"].(bool)
		testRole, _ := n.Meta["test_role"].(string)
		rows = append(rows, toonNodeRow{
			ID:        n.ID,
			Kind:      string(n.Kind),
			Name:      n.Name,
			FilePath:  n.FilePath,
			StartLine: n.StartLine,
			IsTest:    isTest,
			TestRole:  testRole,
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
		return s.gcxResponseWithBudget(req)(encodeSubGraph(tool, sg))
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
//
// Three pipeline stages run before the format encoder:
//
//  1. Sparse-fieldsets filter: when the caller passes
//     `fields: "id,line"`, list rows are projected down to those keys.
//     Trims response size at the row level.
//  2. Graceful degradation: tools that registered a per-shape policy
//     (`get_file_summary`, `get_editing_context`, `find_usages`, …)
//     run a cascade — strip verbose meta, drop low-priority kinds,
//     last-resort tail-trim. Quality stays high under pressure.
//  3. Generic budget: tools without a registered shape fall back to
//     a "trim the longest list" heuristic. Always emits inline data
//     with `_truncated_by_budget` metadata; never falls through to
//     a transport spill that the agent has to re-read from disk.
//
// effectiveBudget defaults to defaultMaxBytes when the caller does
// not specify; pass `max_bytes: 0` to opt out of budgeting and get
// the full result in one shot (transport spill if oversized).
func (s *Server) respondJSONOrTOON(ctx context.Context, req mcp.CallToolRequest, payload any) (*mcp.CallToolResult, error) {
	payload = applyFieldsFilter(payload, parseFields(req.GetString("fields", "")))
	if budget := effectiveBudget(req); budget > 0 {
		if shape, ok := degradeShapes[req.Params.Name]; ok {
			payload, _ = applyDegradation(payload, shape, budget)
		} else {
			payload, _ = applyBudget(payload, budget)
		}
	}
	// TOON is the right fallback whenever the caller (or the
	// per-session default) asked for a compact format. That covers
	// two cases:
	//
	//  1. Explicit `format=toon` — return TOON.
	//  2. Session default is gcx but this tool does not have a
	//     hand-tuned GCX encoder (status-shape tools like graph_stats
	//     / index_health / list_repos go through this path). Falling
	//     back to TOON instead of JSON keeps the response compact —
	//     ~10–15% smaller than JSON for typical payloads — without
	//     forcing every status tool to ship a bespoke GCX encoder.
	//
	// Plain JSON is still the answer when neither toon nor gcx was
	// requested (unknown clients, explicit `format=json`).
	if s.isTOON(ctx, req) || s.isGCX(ctx, req) {
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

// resolveRepoFilter resolves the optional repo/project/ref params into
// a set of allowed repo prefixes, enforced against the session's
// workspace boundary.
//
// For a workspace-bound session (the daemon socket path) the boundary
// is mandatory and cannot be widened by args: a `workspace` arg may
// only name the session's own workspace, and `repo`/`project`/`ref`
// args are intersected with the workspace so they can only ever
// narrow. With no explicit narrowing the allow-set is every repo in
// the session's workspace — not "all repos".
//
// For an unbound session (embedded stdio / `gortex server
// --workspace` / legacy) it falls back to resolveRepoFilterArgs with
// the active-project default applied. A nil result there still means
// "no filter — all repos".
func (s *Server) resolveRepoFilter(ctx context.Context, req mcp.CallToolRequest) (map[string]bool, error) {
	repo := req.GetString("repo", "")
	project := req.GetString("project", "")
	ref := req.GetString("ref", "")
	workspaceArg := req.GetString("workspace", "")

	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		// Unbound — legacy behaviour, incl. the active-project default.
		return s.resolveRepoFilterArgs(repo, project, ref, true)
	}

	// A `workspace` arg may only name the session's own workspace. Any
	// other value is a cross-workspace escape attempt — reject it
	// outright rather than silently honouring the boundary and
	// returning a confusing empty result.
	if workspaceArg != "" && workspaceArg != sessWS {
		return nil, fmt.Errorf(
			"workspace %q is outside the active workspace %q; cross-workspace queries are not permitted",
			workspaceArg, sessWS)
	}

	wsRepos := map[string]bool{}
	if s.multiIndexer != nil {
		wsRepos = s.multiIndexer.ReposInWorkspace(sessWS)
	}

	// No explicit narrowing — the allow-set is the whole workspace.
	if repo == "" && project == "" && ref == "" {
		return wsRepos, nil
	}

	// Explicit narrowing: resolve the args, then intersect with the
	// workspace so a repo/project/ref arg can never escape it.
	narrowed, err := s.resolveRepoFilterArgs(repo, project, ref, false)
	if err != nil {
		return nil, err
	}
	if narrowed == nil {
		// Args resolved to "all" — clamp to the workspace.
		return wsRepos, nil
	}
	intersected := make(map[string]bool)
	for p := range narrowed {
		if wsRepos[p] {
			intersected[p] = true
		}
	}
	if len(intersected) == 0 {
		return nil, fmt.Errorf(
			"repo/project/ref filter resolves to nothing inside the active workspace %q; cross-workspace queries are not permitted",
			sessWS)
	}
	return intersected, nil
}

// resolveRepoFilterArgs folds explicit repo/project/ref args into a
// single allow-set of repo prefixes. A nil map means "no filter — all
// repos". When useActiveProjectDefault is true and no axis is given,
// the server's active project is applied as the default scope (the
// legacy single-tenant behaviour); workspace-bound callers pass false
// because their boundary is supplied separately.
//
// An explicit unknown project is a hard error (the caller asked for X
// by name, deserves to know X is wrong); a stale active-project
// default degrades to "no filter" with a warning log instead, so a
// single misconfigured config line does not break every MCP call.
func (s *Server) resolveRepoFilterArgs(repo, project, ref string, useActiveProjectDefault bool) (map[string]bool, error) {
	projectFromActive := false
	if useActiveProjectDefault && repo == "" && project == "" && ref == "" && s.activeProject != "" {
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
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor returned in `next_cursor` from a previous call. Pass it back to fetch the next page. Omit for the first page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail. Implies the caller will follow `next_cursor` to walk the rest. Default false (full result inline; transport spills to disk if oversized).")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed and `_truncated_by_budget` plus `_max_returned_<key>` / `_original_count_<key>` flags ride on the response. Omit for no cap.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each result (e.g. \"id,name,line\"). Drops the rest to save tokens.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
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
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleGetCluster,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_repo_outline",
			mcp.WithDescription("Narrative single-call overview of the indexed codebase: primary languages, top communities, load-bearing hotspots, most-imported files, and entry points. Use at session start (or when onboarding to an unfamiliar repo) instead of assembling this from graph_stats + analyze + manual inspection. Output stays under ~1k tokens."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetRepoOutline,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("graph_stats",
			mcp.WithDescription("Returns a compact summary of the indexed codebase: node/edge counts by kind and language. Call at session start to orient Claude in an unfamiliar repo."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
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
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
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
	offset := decodeCursor(req.GetString("cursor", ""))

	sess := s.sessionFor(ctx)
	sess.recordSearch(q)

	// Apply server-default scope merged with caller args. `workspace`
	// / `project` args win per-field; empty falls through to the
	// server's --workspace flag. SearchSymbolsScoped over-fetches and
	// post-filters, so ranking is preserved while results stay inside
	// the boundary. With pagination we over-fetch to (offset + limit
	// + 10) so the post-filter slack still leaves a full page.
	workspaceArg := req.GetString("workspace", "")
	projectArg := req.GetString("project", "")
	scopeWS, scopeProj := s.resolveQueryScope(ctx, workspaceArg, projectArg)
	scope := query.QueryOptions{WorkspaceID: scopeWS, ProjectID: scopeProj}
	nodes := s.engine.SearchSymbolsScoped(q, offset+limit+10, scope)

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
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

	// Rerank: fold locality + combo + frecency signals over the backend's
	// BM25 order. Locality ranks the session's home repo / project above
	// the rest of its workspace; combo + frecency are per-repo and
	// zero-valued until the agent has spent time in the codebase, so cold
	// queries return BM25 order with only the locality tier applied.
	rerankRepo, rerankProject := s.sessionLocality(ctx)
	nodes = applyRerankBoosts(nodes, s.combo, s.frecency, q, rerankRepo, rerankProject)

	// Remember the returned IDs for attribution on later consume calls.
	// Cap at top limit so unseen "overflow" results don't get credited.
	recordLastSearchFromNodes(sess, q, nodes, limit)

	total := len(nodes)
	// Slice the (offset, limit) window. nextCursor is empty when the
	// last row in `nodes` is included.
	end := offset + limit
	if end > total {
		end = total
	}
	if offset > total {
		offset = total
	}
	page := nodes[offset:end]
	nextCursor := ""
	if end < total {
		nextCursor = encodeCursor(end)
	}

	if isCompact(req) {
		return mcp.NewToolResultText(compactNodes(page)), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeSearchSymbols(page, total, len(page)))
	}

	if s.isTOON(ctx, req) {
		result := toonSearchResult{
			Results:   nodesToTOONRows(page),
			Total:     total,
			Truncated: end < total,
		}
		data, err := toon.Marshal(result)
		if err == nil {
			return mcp.NewToolResultText(string(data)), nil
		}
	}

	var results []map[string]any
	for _, n := range page {
		results = append(results, n.Brief())
	}
	resp := map[string]any{
		"results":   results,
		"total":     total,
		"truncated": end < total,
	}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	return s.respondJSONOrTOON(ctx, req, resp)
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
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
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
		return s.gcxResponseWithBudget(req)(encodeFileSummary(sg, etag))
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
	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
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
	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
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
	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
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
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
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
	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
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
	// Confine results to the session's workspace — these engine
	// methods don't take QueryOptions, so the boundary is enforced
	// here.
	nodes = s.scopedNodeSlice(ctx, nodes)

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
	// Confine results to the session's workspace — FindImplementations
	// doesn't take QueryOptions, so the boundary is enforced here.
	impls = s.scopedNodeSlice(ctx, impls)

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
	scopeWS, scopeProj := s.resolveQueryScope(ctx, workspaceArg, projectArg)
	sg := s.engine.FindUsagesScoped(id, query.QueryOptions{
		WorkspaceID:  scopeWS,
		ProjectID:    scopeProj,
		ExcludeTests: req.GetBool("exclude_tests", false),
	})

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	sg = filterSubGraph(sg, allowed)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeFindUsages(sg))
	}
	return s.returnSubGraph(ctx, req, sg)
}

func (s *Server) handleGetCluster(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
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
	return s.respondJSONOrTOON(ctx, req, s.buildGraphStatsPayload(ctx))
}

// buildGraphStatsPayload returns the same data the `graph_stats` tool
// emits. Shared with the `gortex://stats` resource so both surfaces
// stay byte-for-byte equal.
func (s *Server) buildGraphStatsPayload(ctx context.Context) map[string]any {
	stats := s.engine.Stats()
	result := map[string]any{
		"total_nodes": stats.TotalNodes,
		"total_edges": stats.TotalEdges,
		"by_kind":     stats.ByKind,
		"by_language": stats.ByLanguage,
	}

	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		result["per_repo"] = s.graph.RepoStats()
	}

	result["token_savings"] = s.tokenStatsFor(ctx).snapshot()

	if cs := s.cumulativeSavingsSnapshot(); cs != nil {
		result["cumulative_savings"] = cs
	}

	if s.semanticMgr != nil && s.semanticMgr.Enabled() {
		result["semantic"] = map[string]any{
			"enabled":   true,
			"providers": s.semanticMgr.Stats(),
		}
	}

	return result
}
