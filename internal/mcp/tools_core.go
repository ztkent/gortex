package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	toon "github.com/toon-format/toon-go"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
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
	ID          string `toon:"id"`
	Kind        string `toon:"kind"`
	Name        string `toon:"name"`
	FilePath    string `toon:"file_path"`
	StartLine   int    `toon:"start_line"`
	Enclosing   string `toon:"enclosing,omitempty"`
	EnclosingID string `toon:"enclosing_id,omitempty"`
	IsTest      bool   `toon:"is_test"`
	TestRole    string `toon:"test_role"`
	TestRunner  string `toon:"test_runner,omitempty"`
}

// toonEdgeRow is a TOON-optimized flat representation of a graph edge.
type toonEdgeRow struct {
	From       string  `toon:"from"`
	To         string  `toon:"to"`
	Kind       string  `toon:"kind"`
	Origin     string  `toon:"origin,omitempty"`
	Tier       string  `toon:"tier,omitempty"`
	Confidence float64 `toon:"confidence"`
	Label      string  `toon:"label"`
}

// toonCallerNoteRow is a TOON-optimized flat representation of one
// caller's concurrency annotation (get_callers only).
type toonCallerNoteRow struct {
	ID                 string `toon:"id"`
	SyncGuarded        bool   `toon:"sync_guarded"`
	SyncGuardedWhy     string `toon:"sync_guarded_why,omitempty"`
	CrossConcurrent    bool   `toon:"cross_concurrent"`
	CrossConcurrentWhy string `toon:"cross_concurrent_why,omitempty"`
}

// toonSubGraphResult wraps nodes and edges for TOON tabular output.
type toonSubGraphResult struct {
	Nodes       []toonNodeRow         `toon:"nodes"`
	Edges       []toonEdgeRow         `toon:"edges"`
	Total       int                   `toon:"total"`
	Truncated   bool                  `toon:"truncated"`
	Caveat      *graph.ZeroEdgeCaveat `toon:"caveat,omitempty"`
	CallerNotes []toonCallerNoteRow   `toon:"caller_notes,omitempty"`
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
		testRunner, _ := n.Meta["test_runner"].(string)
		encID, encName := graph.EnclosingFromID(n.ID, n.Kind)
		rows = append(rows, toonNodeRow{
			ID:          n.ID,
			Kind:        string(n.Kind),
			Name:        n.Name,
			FilePath:    n.FilePath,
			StartLine:   n.StartLine,
			Enclosing:   encName,
			EnclosingID: encID,
			IsTest:      isTest,
			TestRole:    testRole,
			TestRunner:  testRunner,
		})
	}
	return rows
}

// callerNotesToTOONRows flattens the get_callers concurrency-annotation
// map into TOON rows, sorted by node ID for deterministic output.
// Returns nil for an empty map so the `caller_notes` field is omitted.
func callerNotesToTOONRows(notes map[string]*graph.ConcurrencyAnnotation) []toonCallerNoteRow {
	if len(notes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(notes))
	for id := range notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]toonCallerNoteRow, 0, len(ids))
	for _, id := range ids {
		a := notes[id]
		rows = append(rows, toonCallerNoteRow{
			ID:                 id,
			SyncGuarded:        a.SyncGuarded,
			SyncGuardedWhy:     a.SyncGuardedWhy,
			CrossConcurrent:    a.CrossConcurrent,
			CrossConcurrentWhy: a.CrossConcurrentWhy,
		})
	}
	return rows
}

// returnSubGraph returns a SubGraph in the requested format (JSON, compact, GCX, or TOON).
// Method on Server so the format negotiation can consult per-session
// client identity (claude-code → gcx, etc.).
func (s *Server) returnSubGraph(ctx context.Context, req mcp.CallToolRequest, sg *query.SubGraph) (*mcp.CallToolResult, error) {
	// Decorate nodes with absolute paths once, up front, so every output
	// format below surfaces an openable path. The canonical graph nodes
	// are copied, never mutated.
	sg.Nodes = s.withAbsPaths(sg.Nodes)
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
		var trimmed bool
		if shape, ok := degradeShapes[req.Params.Name]; ok {
			payload, trimmed = applyDegradation(payload, shape, budget)
		} else {
			payload, trimmed = applyBudget(payload, budget)
		}
		if trimmed {
			payload = decorateTokenBudgetJSON(payload, req)
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
		tier := e.Tier
		if tier == "" {
			tier = graph.ResolvedBy(e.Origin)
		}
		edgeRows = append(edgeRows, toonEdgeRow{
			From:       e.From,
			To:         e.To,
			Kind:       string(e.Kind),
			Origin:     e.Origin,
			Tier:       tier,
			Confidence: e.Confidence,
			Label:      label,
		})
	}
	result := toonSubGraphResult{
		Nodes:       nodesToTOONRows(sg.Nodes),
		Edges:       edgeRows,
		Total:       sg.TotalNodes,
		Truncated:   sg.Truncated,
		Caveat:      sg.Caveat,
		CallerNotes: callerNotesToTOONRows(sg.CallerNotes),
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

	// A named saved-scope supplies the repo allow-set when no explicit
	// repo/project/ref narrows the call (see scopes.go).
	scopeArg := req.GetString("scope", "")
	var scopeRepos map[string]bool
	if scopeArg != "" && repo == "" && project == "" && ref == "" {
		sc, ok := s.lookupScope(scopeArg)
		if !ok {
			return nil, fmt.Errorf("unknown scope %q — run list_scopes to see saved scopes, or create one with save_scope", scopeArg)
		}
		scopeRepos = s.scopeRepoSet(sc)
		if len(scopeRepos) == 0 {
			return nil, fmt.Errorf("saved scope %q names no repositories", scopeArg)
		}
	}

	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		// Unbound — legacy behaviour, incl. the active-project default.
		if scopeRepos != nil {
			return scopeRepos, nil
		}
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

	// A named scope, intersected with the workspace so it can only ever
	// narrow — a scope is a convenience, never a clamp escape.
	if scopeRepos != nil {
		intersected := make(map[string]bool)
		for p := range scopeRepos {
			if wsRepos[p] {
				intersected[p] = true
			}
		}
		if len(intersected) == 0 {
			return nil, fmt.Errorf(
				"saved scope %q resolves to nothing inside the active workspace %q",
				scopeArg, sessWS)
		}
		return intersected, nil
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
		fmt.Fprintf(&b, "%s %s %s:%d", n.Kind, qualifiedName(n), n.FilePath, n.StartLine)
		// Append the enclosing owner when the node is declared inside
		// one -- a method on a type, a field of a struct, a closure.
		if _, ename := graph.EnclosingFromID(n.ID, n.Kind); ename != "" {
			fmt.Fprintf(&b, " (in %s)", ename)
		}
		b.WriteByte('\n')
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

// enrichSubGraphEdges populates ConfidenceLabel, Origin, and Tier on every
// edge in a SubGraph. Origin is backfilled from kind + confidence +
// semantic_source meta when unset; Tier is the coarse (ast / lsp /
// heuristic) provenance label derived from Origin so clients can filter
// or group without recomputing the mapping.
func enrichSubGraphEdges(sg *query.SubGraph) {
	for _, e := range sg.Edges {
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		if e.Origin == "" {
			src, _ := e.Meta["semantic_source"].(string)
			e.Origin = graph.DefaultOriginFor(e.Kind, e.Confidence, src)
		}
		e.Tier = graph.ResolvedBy(e.Origin)
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
	// Append concurrency annotations for callers that carry one. Sorted
	// by node ID so the compact output is deterministic.
	if len(sg.CallerNotes) > 0 {
		ids := make([]string, 0, len(sg.CallerNotes))
		for id := range sg.CallerNotes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			a := sg.CallerNotes[id]
			fmt.Fprintf(&b, "concurrency %s:", id)
			if a.SyncGuarded {
				b.WriteString(" sync_guarded")
			}
			if a.CrossConcurrent {
				b.WriteString(" cross_concurrent")
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *Server) registerCoreTools() {
	s.addTool(
		mcp.NewTool("index_repository",
			mcp.WithDescription("Index or re-index a local repository path into Gortex. Call once at session start if not already running with --watch."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
		),
		s.handleIndexRepository,
	)

	s.addTool(
		mcp.NewTool("reindex_repository",
			mcp.WithDescription("Incrementally re-index a repository: re-parses only the files that changed since the last index pass (mtime, or content under Merkle mode) and evicts nodes for deleted files. Much cheaper than index_repository, which rebuilds the whole graph. Pass `paths` to scope the pass to specific files or directories; omit it to scan the whole repository. Returns what was reindexed plus node/edge counts, the stale-file count, and timing."),
			mcp.WithString("path", mcp.Description("Absolute path to the repository, or a tracked repo prefix. Defaults to the active repository.")),
			mcp.WithArray("paths",
				mcp.Description("Optional list of file or directory paths to scope the reindex to — absolute, or relative to the repository root. Directories are walked recursively. When omitted, the whole repository root is scanned."),
				mcp.WithStringItems(),
			),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleReindexRepository,
	)

	s.addTool(
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

	s.addTool(
		mcp.NewTool("search_symbols",
			mcp.WithDescription("Use instead of Grep to find symbols across the whole codebase. Supports natural language queries with camelCase-aware tokenization and BM25 ranking — 'validate token auth' finds validateToken, AuthMiddleware, parseJWT."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query — symbol name, concept, or keywords. Also accepts inline field-qualified clauses: `kind:function lang:go path:internal/ repo:gortex project:web validateToken` — recognised fields are kind, lang (aliases ts/js/py/rs/…), path, repo, project; everything else is free text. A field-qualified query that matches nothing retries on the free text alone (response carries `filters_relaxed: true`).")),
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
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — restricts results to that scope's repositories. Ignored when an explicit repo / project / ref is also given.")),
			mcp.WithString("path", mcp.Description("Restrict results to one or more sub-paths (comma-separated) -- the monorepo-service slice (e.g. \"services/billing,libs/auth\"). Anchored, slash-segment-boundary prefixes relative to the repo root: \"services/billing\" matches services/billing/x.go, not other/services/billingX. Unions with any inline path: clause and a scope's saved paths.")),
			mcp.WithString("kind", mcp.Description("Filter to one or more node kinds (comma-separated). Standard kinds: function, method, type, interface, variable, constant, field, file, package, import, contract. Coverage kinds: param, closure, enum_member, generic_param, module, table, column, config_key, flag, event, migration, fixture, todo, team, license, release, doc (Markdown prose section).")),
			mcp.WithString("assist", mcp.Description("LLM assist mode: \"auto\" (default — engages on natural-language queries, skips identifier lookups), \"on\" (force engage), \"off\" (bypass), \"deep\" (on + a body-grounded verification pass that reads candidate code and HONESTLY drops irrelevant matches — slower, may return empty results when nothing genuinely matches). Requires an LLM provider configured via `llm.provider` (local / anthropic / openai / ollama / claudecli / gemini / bedrock / deepseek); behaves as \"off\" when none is available.")),
			mcp.WithBoolean("debug", mcp.Description("When true, attach a `rerank` block to the response carrying per-candidate scores and per-signal contributions from the 11-signal rerank pipeline (bm25, semantic, fan_in, hits, fan_out, churn, community, minhash, api_signature, type_signature, recency, feedback) plus the active per-signal weight map. Off by default; enable to inspect ranking decisions or tune `.gortex.yaml::search::weights`.")),
			mcp.WithString("query_class", mcp.Description("Advisory hint that tunes the bm25-vs-semantic balance of the rerank: \"auto\" (default — detect from query shape), \"symbol\" (identifier / API lookup — BM25-heavy), \"concept\" (natural-language description — balanced), \"path\" (file-path query — most BM25-heavy), \"signature\" (type/function-signature fragment — BM25-leaning), \"keyword_soup\" (a degenerate boolean OR-list \u2014 suppresses LLM expansion and splits the soup into per-disjunct BM25 fetches; a `query_advice` nudge rides on the response). The class actually used is echoed back as `query_class` in the response.")),
			mcp.WithString("expand", mcp.Description("Query-expansion channels: \"both\" (default \u2014 LLM expansion when the assist gate engages, plus the deterministic equivalence-class table), \"equivalence\" (only the LLM-free curated synonym table + per-repo auto-mined concepts), \"llm\" (only LLM expansion), \"off\" (pure BM25, no expansion). Equivalence expansion bridges query vocabulary to the words a symbol uses (auth->login, delete->remove) and runs even with no LLM provider configured.")),
			mcp.WithString("corpus", mcp.Description("Which corpus to search: \"code\" (default \u2014 code symbols only), \"docs\" (only Markdown prose-section nodes \u2014 the heading-delimited documentation sections), \"all\" (both). With docs/all a prose query matches the right README / guide section by its body text.")),
			mcp.WithNumber("max_per_file", mcp.Description("Cap how many results a single source file may contribute to the diverse head of the result set (default 3). Hits beyond the cap are demoted below not-yet-capped results — never dropped — so the top of the list spans more files. Set 0 to disable diversification.")),
		),
		s.handleSearchSymbols,
	)

	s.addTool(
		mcp.NewTool("get_file_summary",
			mcp.WithDescription("Use instead of Read to understand a file's role: returns all its symbols and imports without reading source lines."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Relative file path")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("if_none_match", mcp.Description("ETag from a previous response — returns not_modified if content unchanged")),
		),
		s.handleGetFileSummary,
	)

	s.addTool(
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

	s.addTool(
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

	s.addTool(
		mcp.NewTool("get_call_chain",
			mcp.WithDescription("Traces the call graph forward from a function without reading source. Use to understand what a function ultimately triggers."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function node ID")),
			mcp.WithNumber("depth", mcp.Description("Traversal depth (default: 4)")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleGetCallChain,
	)

	s.addTool(
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

	s.addTool(
		mcp.NewTool("find_implementations",
			mcp.WithDescription("Finds all concrete types that implement an interface. Use before changing an interface to identify all types that will be affected."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Interface node ID")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleFindImplementations,
	)

	s.addTool(
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

	s.addTool(
		mcp.NewTool("get_class_hierarchy",
			mcp.WithDescription("Returns the inheritance subgraph around a type, interface, or method. Walks EdgeExtends + EdgeImplements + EdgeComposes for type nodes and EdgeOverrides for method nodes — the same graph data find_implementations and find_overrides expose, but as a multi-hop tree so an agent gets the whole chain (parents → root, children → leaves) in one call. Use before refactoring an OO hierarchy or to answer 'what does this class inherit from / who subclasses it'."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Seed node ID — a type, interface, or method")),
			mcp.WithString("direction", mcp.Description("'up' (parents/interfaces this extends or implements; methods this overrides), 'down' (subtypes / implementers / overriders), or 'both' (default)")),
			mcp.WithNumber("depth", mcp.Description("Max hops to walk in each direction (default: 5, hard cap: 64)")),
			mcp.WithBoolean("include_methods", mcp.Description("When true and the seed/visited node is a type or interface, also include its methods (via EdgeMemberOf) and walk the EdgeOverrides chain rooted at each method.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
		),
		s.handleGetClassHierarchy,
	)

	s.addTool(
		mcp.NewTool("find_usages",
			mcp.WithDescription("Use instead of Grep to find every reference to a symbol across the codebase. Returns precise locations with zero false positives."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Node ID")),
			mcp.WithNumber("limit", mcp.Description("Max nodes (default: 50)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output (saves 50-70% tokens)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("min_tier", mcp.Description(minTierParamDescription)),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop references originating in test functions (set true to see only production usages)")),
			mcp.WithString("group_by", mcp.Description("Set to \"file\" to bucket the usages by the file each reference originates in -- each group carries the per-file use count and the enclosing symbol of every reference. Omit for the default flat result.")),
		),
		s.handleFindUsages,
	)

	s.addTool(
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

	s.addTool(
		mcp.NewTool("get_repo_outline",
			mcp.WithDescription("Narrative single-call overview of the indexed codebase: primary languages, top communities, load-bearing hotspots, most-imported files, and entry points. Use at session start (or when onboarding to an unfamiliar repo) instead of assembling this from graph_stats + analyze + manual inspection. Output stays under ~1k tokens."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetRepoOutline,
	)

	s.addTool(
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

// handleReindexRepository implements the reindex_repository tool: an
// incremental re-index that re-parses only changed files (and evicts
// deleted ones) instead of rebuilding the whole graph. The optional
// `paths` argument scopes the pass to specific files / directories.
func (s *Server) handleReindexRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	paths := req.GetStringSlice("paths", nil)
	// Drop blank entries so a caller passing [""] doesn't accidentally
	// degrade a scoped request into a whole-repo scan.
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			cleaned = append(cleaned, p)
		}
	}
	paths = cleaned

	pathArg := req.GetString("path", "")

	var (
		result *indexer.IndexResult
		err    error
	)

	// Multi-repo mode: route through the per-repo indexer so nodes keep
	// their RepoPrefix and per-repo stats stay consistent.
	if s.multiIndexer != nil {
		prefix := ""
		if pathArg != "" {
			prefix = s.resolveRepoPrefixOrReconcile(ctx, pathArg)
			if prefix == "" {
				return mcp.NewToolResultError(fmt.Sprintf(
					"path %q is not a tracked repository; use track_repository to add it", pathArg)), nil
			}
		} else {
			// No path given — fall back to the session's bound repo,
			// then to the sole tracked repo when there is exactly one.
			prefix, _ = s.sessionLocality(ctx)
			if prefix == "" {
				if tracked := s.multiIndexer.RepoPrefixes(); len(tracked) == 1 {
					prefix = tracked[0]
				}
			}
			if prefix == "" {
				return mcp.NewToolResultError(
					"reindex_repository: no repository specified and the active repository is ambiguous; pass `path`"), nil
			}
		}
		result, err = s.multiIndexer.IncrementalReindexRepo(prefix, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else {
		if s.indexer == nil {
			return mcp.NewToolResultError("reindex_repository: no indexer available"), nil
		}
		root := pathArg
		if root == "" {
			root = s.indexer.RootPath()
		}
		if root == "" {
			return mcp.NewToolResultError(
				"reindex_repository: no repository root known; pass `path`"), nil
		}
		root, absErr := filepath.Abs(root)
		if absErr != nil {
			return mcp.NewToolResultError(absErr.Error()), nil
		}
		result, err = s.indexer.IncrementalReindexPaths(root, paths)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	s.RunAnalysis()

	scope := "repository"
	if len(paths) > 0 {
		scope = "paths"
	}
	payload := map[string]any{
		"scope":            scope,
		"result":           result,
		"node_count":       result.NodeCount,
		"edge_count":       result.EdgeCount,
		"file_count":       result.FileCount,
		"stale_file_count": result.StaleFileCount,
		"duration_ms":      result.DurationMs,
	}
	if len(paths) > 0 {
		payload["reindexed_paths"] = paths
	}
	if len(result.FailedFiles) > 0 {
		payload["failed_files"] = result.FailedFiles
	}
	return s.respondJSONOrTOON(ctx, req, payload)
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

	node := s.engineFor(ctx).GetSymbol(id)
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
		return s.respondJSONOrTOON(ctx, req, s.withAbsPath(node).Brief())
	}

	// Full: include node + direct edges.
	out := s.engineFor(ctx).GetOutEdges(node.ID)
	in := s.engineFor(ctx).GetInEdges(node.ID)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"node":      s.withAbsPath(node),
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

	// Field-qualified query syntax: lift `kind:` / `lang:` / `path:` /
	// `repo:` / `project:` clauses out of the query string. The
	// residual free text drives search, rerank, and classification;
	// the clauses become post-filters and scope, merged with the
	// explicit kind / repo / project arguments.
	fq := parseFieldQuery(q)
	q = fq.Text

	// Apply server-default scope merged with caller args. `workspace`
	// / `project` args win per-field; empty falls through to the
	// server's --workspace flag. SearchSymbolsScoped over-fetches and
	// post-filters, so ranking is preserved while results stay inside
	// the boundary. With pagination we over-fetch to (offset + limit
	// + 10) so the post-filter slack still leaves a full page.
	workspaceArg := req.GetString("workspace", "")
	projectArg := req.GetString("project", "")
	if projectArg == "" {
		projectArg = fq.Project
	}
	scopeWS, scopeProj := s.resolveQueryScope(ctx, workspaceArg, projectArg)
	scope := query.QueryOptions{WorkspaceID: scopeWS, ProjectID: scopeProj}

	// Keyword-soup defense: a degenerate boolean / OR-list query
	// ("A OR B OR 'no access'") defeats ordinary retrieval. Detect it
	// up front -- explicit query_class:"keyword_soup" pins it, else the
	// structural detector decides. On a soup query the LLM expansion +
	// rerank passes are suppressed (wasted on garbage) and the soup is
	// split into per-disjunct BM25 fetches; a `query_advice` nudge is
	// attached so the agent learns to send a cleaner query.
	soupMode := s.searchConfig().EffectiveKeywordSoupRewrite()
	soupQueryClass := strings.EqualFold(strings.TrimSpace(req.GetString("query_class", "")), "keyword_soup")
	soupDetected, soupReason := rerank.LooksLikeKeywordSoup(q)
	isSoup := soupMode != config.KeywordSoupOff && (soupDetected || soupQueryClass)
	if isSoup && soupReason == "" {
		soupReason = "query reads as a boolean OR-list; search ranks best on a single concept or symbol name -- run one query per disjunct, or describe the intent in plain words"
	}

	// LLM assist gate: decides whether the expansion + rerank passes
	// run for this query. The service-enabled check is layered inside
	// the helpers so a stub build is a clean bypass. A soup query
	// forces assist off -- neither expansion nor rerank earns its cost
	// on a disjunct list.
	assist := parseAssistMode(req)
	// expand mode picks which query-expansion channels run -- LLM,
	// the deterministic equivalence table, both (default), or off.
	expand := parseExpandMode(req)
	engage := shouldEngageAssist(assist, q) && s.llmService != nil && s.llmService.Enabled()
	if isSoup || !expand.allowsLLMExpansion() {
		engage = false
	}

	fetchLimit := offset + limit + 10
	if engage {
		// Slightly widen the BM25 over-fetch when we're going to
		// rerank: more head candidates means a more useful reorder.
		fetchLimit = offset + limit + rerankCap
	}

	// Expansion terms feeding the BM25 OR-merge: LLM-derived synonyms
	// when assist engaged, or the soup's split disjuncts when this is
	// a soup query handled in "split" mode. The two are mutually
	// exclusive -- soup forces assist off above.
	// LLM-derived synonyms (when assist engaged), the soup's split
	// disjuncts (soup query in split mode), and the deterministic
	// equivalence-class siblings all compose into one expansion-term
	// list. fetchAndMergeBM25 dedups the merged result by node ID, so
	// overlapping channels never double-count a candidate.
	var llmTerms, soupFragments, equivTerms []string
	if engage {
		llmTerms = expandSearchTerms(ctx, s, q)
	}
	if isSoup && soupMode == config.KeywordSoupSplit {
		soupFragments = rerank.SplitSoupFragments(q)
	}
	if expand.allowsEquivalenceExpansion() {
		equivTerms = s.expandEquivalenceClasses(q)
	}
	expandedTerms := mergeExpansionTerms(soupFragments, llmTerms, equivTerms)

	var nodes []*graph.Node
	var primaryCount int
	if len(expandedTerms) > 0 {
		nodes, primaryCount = fetchAndMergeBM25(s.engineFor(ctx), q, expandedTerms, fetchLimit, scope)
	} else {
		nodes = s.engineFor(ctx).SearchSymbolsScoped(q, fetchLimit, scope)
		primaryCount = len(nodes)
	}
	mergedCount := len(nodes) // pre-filter; comparable to primaryCount

	// Apply repo/project/ref filter.
	allowed, filterErr := s.resolveRepoFilter(ctx, req)
	if filterErr != nil {
		return mcp.NewToolResultError(filterErr.Error()), nil
	}
	nodes = filterNodes(nodes, allowed)

	// kind filter so callers can scope to a single new node kind
	// (todo, license, team, module, …). Comma-separated list —
	// case-insensitive — applied post-search so BM25 ranking is
	// preserved within the kept set. The explicit `kind` argument
	// wins; an in-query `kind:` clause fills in when it is absent.
	kindArg := strings.TrimSpace(req.GetString("kind", ""))
	if kindArg == "" {
		kindArg = fq.Kind
	}
	if kindArg != "" {
		nodes = filterNodesByKind(nodes, kindArg)
	}
	// lang: / path: / repo: clauses from the field-qualified syntax.
	nodes = applyFieldFilters(nodes, fq)

	// Sub-path scoping: anchored, slash-segment prefix narrowing
	// below the repository grain. The filter set unions the `path`
	// argument, the inline `path:` clause, and any `scope:`-named
	// saved scope's Paths. Distinct from the loose `path:` substring
	// match in applyFieldFilters -- this is a directory-boundary
	// anchored prefix test.
	if pathFilter := s.resolvePathFilter(req, fq); len(pathFilter) > 0 {
		nodes = applyPathFilter(nodes, pathFilter)
	}

	// Corpus selection: `code` (default) keeps only code symbols,
	// `docs` keeps only prose-section (KindDoc) nodes, `all` keeps
	// both. Runs after the path filter so a scoped docs query stays
	// inside its sub-path.
	corpus, corpusErr := parseCorpus(req)
	if corpusErr != nil {
		return mcp.NewToolResultError(corpusErr.Error()), nil
	}
	nodes = filterNodesByCorpus(nodes, corpus)

	// Fuzzy fallback: a field-qualified query that filtered down to
	// nothing retries on the free text alone (still inside the
	// caller's repo / project scope), so an over-narrow or typo'd
	// clause degrades to a useful result set instead of an empty one.
	filtersRelaxed := false
	if len(nodes) == 0 && q != "" && (kindArg != "" || fq.hasFieldFilters()) {
		relaxed := filterNodes(s.engineFor(ctx).SearchSymbolsScoped(q, fetchLimit, scope), allowed)
		if len(relaxed) > 0 {
			nodes = relaxed
			filtersRelaxed = true
		}
	}

	// LLM rerank runs AFTER kind/repo filters so the model only sees
	// the candidate pool the caller will actually receive, and BEFORE
	// the combo/frecency boost so per-session signals can still
	// override a stale rerank.
	var verifyDbg verifyDebug
	var verifyRan bool
	if engage {
		nodes = rerankWithLLM(ctx, s, q, nodes)
		// `deep` mode adds a body-grounded verification pass that
		// reads candidate code and HONESTLY drops the ones whose
		// body isn't actually about the query. An empty kept set is
		// preserved — it's the load-bearing "nothing genuinely matches"
		// signal that distinguishes deep mode from plain rerank.
		if assist == assistDeep {
			nodes, verifyDbg, verifyRan = verifyWithLLM(ctx, s, q, nodes)
		}
	}

	// Rerank: run the I13 11-signal pipeline over the candidate set
	// with the session-aware Context wired in. Structural signals
	// (BM25 rank, fan-in / fan-out, MinHash similarity, signature
	// match, recency, community) discriminate within the backend's
	// BM25 order; session signals (locality, combo, frecency,
	// feedback, churn) layer on top once the agent has spent time
	// in the codebase. Cold queries with no session data fall back
	// to a structural-only pass.
	rctx := s.buildRerankContext(ctx, q)
	// Per-class rerank weighting: detect the query class (or honour an
	// explicit query_class hint) and pin it on the rerank Context so
	// the pipeline scales the bm25 / semantic blend accordingly.
	queryClass := rerank.ClassifyQuery(q)
	if qcArg := strings.TrimSpace(req.GetString("query_class", "")); qcArg != "" {
		parsed, ok := rerank.ParseQueryClass(qcArg)
		if !ok {
			return mcp.NewToolResultError("invalid query_class: " + qcArg + " (want auto, symbol, concept, path, signature, or keyword_soup)"), nil
		}
		if parsed != rerank.QueryClassUnknown {
			queryClass = parsed
		}
	}
	// A detected soup query reports the keyword_soup class even when
	// the caller did not pin it, so the response surfaces the class
	// the handler actually treated the query as.
	if isSoup {
		queryClass = rerank.QueryClassKeywordSoup
	}
	rctx.QueryClass = queryClass
	var rerankBreakdown []*rerank.Candidate
	nodes = applyRerankBoosts(s, nodes, q, rctx, &rerankBreakdown)

	// Per-file diversification: keep one file's many symbols from
	// monopolising the head of the result set. Runs after the rerank
	// so demotion acts on final scores; nothing is dropped.
	nodes, rerankBreakdown = diversifyByFile(nodes, rerankBreakdown, req.GetInt("max_per_file", defaultMaxPerFile))

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
	// Decorate the page with absolute file paths so every output format
	// below surfaces an openable path alongside the repo-relative one.
	page = s.withAbsPaths(page)
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
		"results":     results,
		"total":       total,
		"truncated":   end < total,
		"query_class": queryClass.String(),
	}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	if filtersRelaxed {
		resp["filters_relaxed"] = true
	}
	// query_advice nudges the agent toward a cleaner query when the
	// input was detected as keyword-soup. In "split" mode the search
	// still ran (over the split disjuncts); the advice is purely
	// instructional and rides alongside the results.
	if isSoup && soupReason != "" {
		advice := map[string]any{"reason": soupReason}
		if len(soupFragments) > 0 {
			advice["split_into"] = soupFragments
		}
		resp["query_advice"] = advice
	}
	// When LLM assist engaged, expose a small debug surface so callers
	// (and the agent itself) can see what the model contributed.
	// Suppressed when engage was false to keep the common-path response
	// shape unchanged.
	if engage {
		assistDebug := map[string]any{
			"engaged":       true,
			"primary_count": primaryCount, // BM25 hits on original query alone, pre-filter
			"merged_count":  mergedCount,  // BM25 hits after merging expansion terms, pre-filter
			"final_count":   total,        // post-filter, post-rerank — matches the top-level total
		}
		if len(llmTerms) > 0 {
			assistDebug["terms"] = llmTerms
		}
		if len(equivTerms) > 0 {
			assistDebug["equivalence_terms"] = equivTerms
		}
		if verifyRan {
			assistDebug["verify_considered"] = verifyDbg.Considered
			assistDebug["verify_kept_ids"] = verifyDbg.Kept
			assistDebug["verify_kept"] = len(verifyDbg.Kept)
			assistDebug["verify_dropped"] = len(verifyDbg.Considered) - len(verifyDbg.Kept)
		}
		resp["assist"] = assistDebug
	}
	// Surface the 11-signal rerank breakdown when the caller asked
	// for it via debug=true. Page-sliced to match the rows in
	// `results`. Keeps the common-path response shape unchanged.
	if req.GetBool("debug", false) && len(rerankBreakdown) > 0 {
		pageBreakdown := rerankBreakdown
		if offset < len(pageBreakdown) {
			pageBreakdown = pageBreakdown[offset:]
		} else {
			pageBreakdown = nil
		}
		if len(pageBreakdown) > limit {
			pageBreakdown = pageBreakdown[:limit]
		}
		resp["rerank"] = encodeRerankBreakdown(pageBreakdown, s.engineFor(ctx).Rerank())
	}
	return s.respondJSONOrTOON(ctx, req, resp)
}

// encodeRerankBreakdown converts a candidate slice into a JSON-ready
// breakdown — one row per candidate carrying its final score and the
// per-signal contributions in the canonical order.
func encodeRerankBreakdown(cands []*rerank.Candidate, pipeline *rerank.Pipeline) []map[string]any {
	if len(cands) == 0 {
		return nil
	}
	weights := map[string]float64{}
	if pipeline != nil {
		weights = pipeline.Weights()
	}
	out := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		row := map[string]any{
			"id":    c.Node.ID,
			"score": roundTo(c.Score, 4),
		}
		if c.TextRank >= 0 {
			row["text_rank"] = c.TextRank
		}
		if c.VectorRank >= 0 {
			row["vector_rank"] = c.VectorRank
		}
		if len(c.Signals) > 0 {
			sigs := make(map[string]float64, len(c.Signals))
			for _, name := range rerank.AllSignalNames() {
				if v, ok := c.Signals[name]; ok {
					sigs[name] = roundTo(v, 4)
				}
			}
			row["signals"] = sigs
		}
		if len(weights) > 0 {
			row["weights"] = weights
		}
		out = append(out, row)
	}
	return out
}

func roundTo(v float64, places int) float64 {
	if v == 0 {
		return 0
	}
	pow := 1.0
	for range places {
		pow *= 10
	}
	return float64(int64(v*pow+0.5)) / pow
}

func (s *Server) handleGetFileSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fp, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Auto re-index stale file before querying.
	s.ensureFresh([]string{fp})

	sg := s.engineFor(ctx).GetFileSymbols(fp)
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
	sg := s.engineFor(ctx).GetDependencies(id, opts)
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
	sg := s.engineFor(ctx).GetDependents(id, opts)
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
	sg := s.engineFor(ctx).GetCallChain(id, opts)

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
	sg := s.engineFor(ctx).GetCallers(id, opts)
	sg.FilterByMinTier(minTier)
	enrichSubGraphEdges(sg)
	annotateCallerConcurrency(s.engineFor(ctx).Reader(), sg, id)
	if len(sg.Edges) == 0 {
		sg.Caveat = graph.CaveatForZeroEdge(s.graph, id)
	}
	return s.returnSubGraph(ctx, req, sg)
}

// annotateCallerConcurrency attaches a ConcurrencyAnnotation to every
// caller node in a get_callers result that carries at least one
// concurrency flag. The seed node itself is skipped — it is the
// queried symbol, not one of its callers. File / import nodes are
// skipped because no concurrency fact applies to them. A node with no
// flag set is left out of the map, so an absent entry reads as
// "neither sync_guarded nor cross_concurrent".
func annotateCallerConcurrency(r graph.Reader, sg *query.SubGraph, seedID string) {
	if r == nil || sg == nil {
		return
	}
	for _, n := range sg.Nodes {
		if n == nil || n.ID == seedID {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		ann := graph.ClassifyConcurrency(r, n.ID)
		if !ann.Any() {
			continue
		}
		if sg.CallerNotes == nil {
			sg.CallerNotes = make(map[string]*graph.ConcurrencyAnnotation)
		}
		annCopy := ann
		sg.CallerNotes[n.ID] = &annCopy
	}
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
		nodes = s.engineFor(ctx).FindOverridden(id)
	default:
		nodes = s.engineFor(ctx).FindOverridesMinTier(id, minTier)
	}
	// Confine results to the session's workspace — these engine
	// methods don't take QueryOptions, so the boundary is enforced
	// here.
	nodes = s.scopedNodeSlice(ctx, nodes)
	nodes = s.withAbsPaths(nodes)

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
	impls := s.engineFor(ctx).FindImplementationsMinTier(id, minTier)
	// Confine results to the session's workspace — FindImplementations
	// doesn't take QueryOptions, so the boundary is enforced here.
	impls = s.scopedNodeSlice(ctx, impls)
	impls = s.withAbsPaths(impls)

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

func (s *Server) handleGetClassHierarchy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	direction := query.HierarchyDirection(req.GetString("direction", string(query.HierarchyBoth)))
	switch direction {
	case query.HierarchyUp, query.HierarchyDown, query.HierarchyBoth:
		// ok
	default:
		return mcp.NewToolResultError("direction must be one of: up, down, both"), nil
	}
	depth := req.GetInt("depth", 5)
	includeMethods := req.GetBool("include_methods", false)
	minTier := req.GetString("min_tier", "")

	scopeWS, scopeProj := s.scopeFromRequest(ctx, &req)
	opts := query.QueryOptions{
		WorkspaceID: scopeWS,
		ProjectID:   scopeProj,
		MinTier:     minTier,
	}
	sg := s.engineFor(ctx).ClassHierarchy(id, direction, depth, includeMethods, opts)
	enrichSubGraphEdges(sg)
	return s.returnSubGraph(ctx, req, sg)
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
	sg := s.engineFor(ctx).FindUsagesScoped(id, query.QueryOptions{
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
	if len(sg.Edges) == 0 {
		sg.Caveat = graph.CaveatForZeroEdge(s.graph, id)
	}
	// group_by:"file" buckets the usages by the file each reference
	// originates in -- an opt-in shape for callers that want a
	// per-file rollup. The flat SubGraph stays the default so
	// existing consumers are unaffected.
	if gb := strings.ToLower(strings.TrimSpace(req.GetString("group_by", ""))); gb == "file" {
		return s.respondJSONOrTOON(ctx, req, groupUsagesByFile(sg))
	}
	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeFindUsages(sg))
	}
	return s.returnSubGraph(ctx, req, sg)
}

// usageFileGroup is one file's worth of references from a
// group_by:"file" find_usages response.
type usageFileGroup struct {
	File  string           `json:"file"`
	Count int              `json:"count"`
	Uses  []usageGroupItem `json:"uses"`
}

// usageGroupItem is one reference inside a usageFileGroup -- the
// line it sits on plus the enclosing symbol.
type usageGroupItem struct {
	Line       int    `json:"line"`
	EdgeKind   string `json:"edge_kind"`
	SymbolID   string `json:"symbol_id,omitempty"`
	SymbolName string `json:"symbol_name,omitempty"`
}

// groupUsagesByFile buckets a find_usages SubGraph by the file each
// reference originates in. The `from` endpoint of every edge is the
// usage site; its file path is the bucket key and the from-node's
// name/ID is the enclosing symbol. Files are sorted by descending
// use count, then by path for a stable order.
func groupUsagesByFile(sg *query.SubGraph) map[string]any {
	nodeByID := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		nodeByID[n.ID] = n
	}
	groups := map[string]*usageFileGroup{}
	for _, e := range sg.Edges {
		from := nodeByID[e.From]
		file := e.FilePath
		if file == "" && from != nil {
			file = from.FilePath
		}
		if file == "" {
			file = "(unknown)"
		}
		g := groups[file]
		if g == nil {
			g = &usageFileGroup{File: file}
			groups[file] = g
		}
		item := usageGroupItem{Line: e.Line, EdgeKind: string(e.Kind)}
		if from != nil {
			item.SymbolID = from.ID
			item.SymbolName = from.Name
		}
		g.Uses = append(g.Uses, item)
		g.Count++
	}
	out := make([]*usageFileGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].File < out[j].File
	})
	return map[string]any{
		"grouped_by": "file",
		"file_count": len(out),
		"total_uses": len(sg.Edges),
		"groups":     out,
		"truncated":  sg.Truncated,
	}
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
	sg := s.engineFor(ctx).GetCluster(id, opts)
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
	stats := s.engineFor(ctx).Stats()
	result := map[string]any{
		"total_nodes": stats.TotalNodes,
		"total_edges": stats.TotalEdges,
		"by_kind":     stats.ByKind,
		"by_language": stats.ByLanguage,
	}

	// Provenance churn: how many times an in-graph edge's identity
	// changed because its Origin was upgraded or reverted. Surfaced
	// as the tamper-evidence signal — a non-zero value means edge
	// provenance moved since the graph was built.
	result["edge_identity_revisions"] = s.readerFor(ctx).EdgeIdentityRevisions()

	if s.multiIndexer != nil && s.multiIndexer.IsMultiRepo() {
		result["per_repo"] = s.readerFor(ctx).RepoStats()
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

	if ns := s.notificationsStatus(); ns != nil {
		result["notifications"] = ns
	}

	return result
}

// notificationsStatus reports each push-notification channel's live
// subscriber count and last-published payload. nil when no broadcaster
// is wired (single-shot CLI modes). Consumed by graph_stats /
// gortex://stats so a debugging client can see who is subscribed and
// what the broadcasters last sent without standing up its own
// subscriber.
func (s *Server) notificationsStatus() map[string]any {
	out := map[string]any{}
	if s.diagBroadcaster != nil {
		out["diagnostics"] = map[string]any{
			"subscribers": s.diagBroadcaster.subscriberCount(),
		}
	}
	if s.readinessBroadcaster != nil {
		row := map[string]any{
			"subscribers": s.readinessBroadcaster.subscriberCount(),
		}
		if snap := s.readinessBroadcaster.snapshot(); snap != nil {
			row["last_state"] = snap
		}
		out["workspace_readiness"] = row
	}
	if s.healthBroadcaster != nil {
		row := map[string]any{
			"subscribers": s.healthBroadcaster.subscriberCount(),
		}
		if snap := s.healthBroadcaster.snapshot(); snap != nil {
			row["last_snapshot"] = snap
		}
		out["daemon_health"] = row
	}
	if s.staleRefsBroadcaster != nil {
		out["stale_refs"] = map[string]any{
			"subscribers": s.staleRefsBroadcaster.subscriberCount(),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
