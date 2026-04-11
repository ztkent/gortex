package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

// ensureFresh checks if any of the given file paths are stale (modified on disk
// since last index) and re-indexes up to 5 of them when watch mode is not active.
// Returns the list of file paths that were refreshed.
func (s *Server) ensureFresh(filePaths []string) []string {
	// Skip when watcher is active — it handles updates.
	if s.watcher != nil {
		return nil
	}
	if s.indexer == nil {
		return nil
	}

	var refreshed []string
	limit := 5
	for _, fp := range filePaths {
		if len(refreshed) >= limit {
			break
		}
		if s.indexer.IsStale(fp) {
			absPath := fp
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, fp)
			}

			// In multi-repo mode, the file path may be prefixed with a repo name
			// (e.g. "ade/internal/..."). If the resolved path doesn't exist, try
			// resolving via the MultiIndexer which knows each repo's root.
			if _, statErr := os.Stat(absPath); statErr != nil && s.multiIndexer != nil {
				resolved := s.multiIndexer.ResolveFilePath(fp)
				if resolved != "" {
					absPath = resolved
				}
			}

			if err := s.indexer.IndexFile(absPath); err != nil {
				s.logger.Warn("auto re-index failed",
					zap.String("file", fp),
					zap.String("resolved", absPath),
					zap.Error(err))
				continue
			}
			refreshed = append(refreshed, fp)
		}
	}
	return refreshed
}

func (s *Server) registerEnhancementTools() {
	// verify_change
	s.mcpServer.AddTool(
		mcp.NewTool("verify_change",
			mcp.WithDescription("Given proposed signature changes, checks all callers and interface implementors for contract violations. Use before refactoring to catch breaking changes."),
			mcp.WithString("changes", mcp.Required(), mcp.Description("JSON array of {symbol_id, new_signature} objects")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-violation text output")),
		),
		s.handleVerifyChange,
	)

	// check_guards
	s.mcpServer.AddTool(
		mcp.NewTool("check_guards",
			mcp.WithDescription("Evaluates project-specific guard rules against a set of changed symbols. Reports co-change and boundary violations."),
			mcp.WithString("symbol_ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-rule text output")),
		),
		s.handleCheckGuards,
	)

	// prefetch_context
	s.mcpServer.AddTool(
		mcp.NewTool("prefetch_context",
			mcp.WithDescription("Predicts what context you will need next based on recent activity and a task description. Returns ranked symbols with relevance reasons."),
			mcp.WithString("task", mcp.Description("Natural language task description")),
			mcp.WithString("recent_symbols", mcp.Description("Comma-separated list of recently viewed symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for top 5 candidates")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
		),
		s.handlePrefetchContext,
	)

	// find_dead_code
	s.mcpServer.AddTool(
		mcp.NewTool("find_dead_code",
			mcp.WithDescription("Returns all symbols with zero incoming call or reference edges, excluding entry points, tests, and exported symbols. Variables are excluded by default because the graph lacks intra-function data-flow edges, making them false positives."),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
			mcp.WithBoolean("include_variables", mcp.Description("Include variable nodes (excluded by default — they are usually false positives since the graph does not track intra-function data flow)")),
		),
		s.handleFindDeadCode,
	)

	// find_hotspots
	s.mcpServer.AddTool(
		mcp.NewTool("find_hotspots",
			mcp.WithDescription("Returns symbols with disproportionately high complexity based on fan-in, fan-out, and community boundary crossings."),
			mcp.WithNumber("threshold", mcp.Description("Complexity score threshold (default: mean + 2σ)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
		),
		s.handleFindHotspots,
	)

	// scaffold
	s.mcpServer.AddTool(
		mcp.NewTool("scaffold",
			mcp.WithDescription("Generates code scaffolding from an existing symbol pattern, including registration wiring and test stubs."),
			mcp.WithString("example_id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
			mcp.WithString("new_name", mcp.Required(), mcp.Description("Name for the new symbol")),
			mcp.WithBoolean("dry_run", mcp.Description("Return scaffold without writing files (default: true)")),
			mcp.WithBoolean("compact", mcp.Description("Compact text output")),
		),
		s.handleScaffold,
	)

	// find_cycles
	s.mcpServer.AddTool(
		mcp.NewTool("find_cycles",
			mcp.WithDescription("Detects circular dependency chains in the import and call graphs. Classifies by severity: import-cycle, cross-community-cycle, call-cycle."),
			mcp.WithString("scope", mcp.Description("File path or package prefix to limit scope")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-cycle text output")),
		),
		s.handleFindCycles,
	)

	// would_create_cycle
	s.mcpServer.AddTool(
		mcp.NewTool("would_create_cycle",
			mcp.WithDescription("Checks whether adding a dependency edge between two symbols would create a new cycle."),
			mcp.WithString("from_id", mcp.Required(), mcp.Description("Source symbol ID")),
			mcp.WithString("to_id", mcp.Required(), mcp.Description("Target symbol ID")),
			mcp.WithBoolean("compact", mcp.Description("Compact text output")),
		),
		s.handleWouldCreateCycle,
	)

	// diff_context
	s.mcpServer.AddTool(
		mcp.NewTool("diff_context",
			mcp.WithDescription("Returns graph-enriched context for symbols affected by a git diff: source, callers, callees, community, processes, and per-file risk."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol condensed output")),
		),
		s.handleDiffContext,
	)

	// index_health
	s.mcpServer.AddTool(
		mcp.NewTool("index_health",
			mcp.WithDescription("Reports the health and completeness of the Gortex index: parse failures, stale files, language coverage, and health score."),
			mcp.WithBoolean("compact", mcp.Description("Single-line summary output")),
		),
		s.handleIndexHealth,
	)

	// get_symbol_history
	s.mcpServer.AddTool(
		mcp.NewTool("get_symbol_history",
			mcp.WithDescription("Returns symbols modified during the current session with modification counts. Flags churning symbols (modified 3+ times)."),
			mcp.WithString("symbol_id", mcp.Description("Specific symbol ID (omit for all)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
		),
		s.handleGetSymbolHistory,
	)

	// batch_edit
	s.mcpServer.AddTool(
		mcp.NewTool("batch_edit",
			mcp.WithDescription("Applies multiple symbol edits in dependency order. Re-indexes after each edit. Stops on failure and reports status."),
			mcp.WithString("edits", mcp.Required(), mcp.Description("JSON array of {symbol_id, old_source, new_source} objects")),
			mcp.WithBoolean("dry_run", mcp.Description("Return dependency-ordered plan without applying changes")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-edit summary")),
		),
		s.handleBatchEdit,
	)

	// get_contracts
	s.mcpServer.AddTool(
		mcp.NewTool("get_contracts",
			mcp.WithDescription("Lists detected API contracts (HTTP routes, gRPC services, GraphQL, topics, WebSocket, env vars, OpenAPI). Contracts are extracted during indexing."),
			mcp.WithString("repo", mcp.Description("Filter by repository prefix")),
			mcp.WithString("type", mcp.Description("Filter by contract type: http, grpc, graphql, topic, ws, env, openapi")),
			mcp.WithString("role", mcp.Description("Filter by role: provider or consumer")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-contract text output")),
		),
		s.handleGetContracts,
	)

	// check_contracts
	s.mcpServer.AddTool(
		mcp.NewTool("check_contracts",
			mcp.WithDescription("Detects contract mismatches: pairs providers with consumers by contract ID and reports orphan providers (no consumer) and orphan consumers (no provider)."),
			mcp.WithString("repo", mcp.Description("Filter by repository prefix before matching")),
			mcp.WithBoolean("compact", mcp.Description("Compact text summary")),
		),
		s.handleCheckContracts,
	)
}

// ---------------------------------------------------------------------------
// 10.2 handleVerifyChange
// ---------------------------------------------------------------------------

func (s *Server) handleVerifyChange(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	changesStr, err := req.RequireString("changes")
	if err != nil {
		return mcp.NewToolResultError("changes is required"), nil
	}

	var changes []analysis.SignatureChange
	if err := json.Unmarshal([]byte(changesStr), &changes); err != nil {
		return mcp.NewToolResultError("invalid changes JSON: " + err.Error()), nil
	}
	if len(changes) == 0 {
		return mcp.NewToolResultError("changes array is empty"), nil
	}

	result := analysis.VerifyChanges(s.graph, s.engine, changes)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range result.Violations {
			fmt.Fprintf(&b, "%s %s %s:%d %s\n", v.Kind, v.SymbolID, v.FilePath, v.Line, v.Description)
		}
		if result.Clean {
			fmt.Fprintf(&b, "clean: checked %d callers, %d implementors\n", result.CheckedCallers, result.CheckedImpls)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(result)
}

// ---------------------------------------------------------------------------
// 10.3 handleCheckGuards
// ---------------------------------------------------------------------------

func (s *Server) handleCheckGuards(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("symbol_ids")
	if err != nil {
		return mcp.NewToolResultError("symbol_ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	if len(s.guardRules) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"violations": []any{},
			"message":    "no guard rules configured",
		})
	}

	violations := analysis.EvaluateGuards(s.graph, s.guardRules, ids)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range violations {
			fmt.Fprintf(&b, "%s %s %s\n", v.Kind, v.RuleName, v.Description)
		}
		if len(violations) == 0 {
			b.WriteString("no guard rule violations\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"violations": violations,
		"total":      len(violations),
	})
}

// ---------------------------------------------------------------------------
// 10.4 handlePrefetchContext
// ---------------------------------------------------------------------------

// prefetchCandidate holds a scored symbol for prefetch ranking.
type prefetchCandidate struct {
	Node            *graph.Node `json:"-"`
	ID              string      `json:"id"`
	Kind            string      `json:"kind"`
	FilePath        string      `json:"file_path"`
	StartLine       int         `json:"start_line"`
	Reason          string      `json:"reason"`
	Confidence      float64     `json:"confidence"`
	SearchRelevance float64     `json:"-"`
	GraphProximity  float64     `json:"-"`
	CommunityBonus  float64     `json:"-"`
	Source          string      `json:"source,omitempty"`
}

func (s *Server) handlePrefetchContext(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := req.GetString("task", "")
	recentStr := req.GetString("recent_symbols", "")
	includeSource := false
	if v, ok := req.GetArguments()["include_source"].(bool); ok {
		includeSource = v
	}

	// Gather recent symbols from parameter or session state.
	var recentIDs []string
	if recentStr != "" {
		for _, id := range strings.Split(recentStr, ",") {
			recentIDs = append(recentIDs, strings.TrimSpace(id))
		}
	}
	if len(recentIDs) == 0 {
		s.session.mu.Lock()
		recentIDs = append(recentIDs, s.session.viewedSymbols...)
		s.session.mu.Unlock()
	}

	if task == "" && len(recentIDs) == 0 {
		return mcp.NewToolResultError("insufficient context for prefetch: provide a task description or recent_symbols"), nil
	}

	// Score map: symbolID → scores
	type scores struct {
		search    float64
		proximity float64
		community float64
		reason    string
		node      *graph.Node
	}
	scoreMap := make(map[string]*scores)

	getOrCreate := func(n *graph.Node) *scores {
		if sc, ok := scoreMap[n.ID]; ok {
			return sc
		}
		sc := &scores{node: n}
		scoreMap[n.ID] = sc
		return sc
	}

	// 1. BM25 search on task description (weight 0.4)
	if task != "" {
		searchResults := s.engine.SearchSymbols(task, 30)
		maxScore := 1.0
		for i, n := range searchResults {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Decay score by rank position
			relevance := 1.0 / float64(i+1)
			if relevance > maxScore {
				maxScore = relevance
			}
			sc.search = relevance
			if sc.reason == "" {
				sc.reason = "matches task keyword"
			}
		}
		// Normalize search scores
		if maxScore > 0 {
			for _, sc := range scoreMap {
				sc.search = sc.search / maxScore
			}
		}
	}

	// 2. Graph proximity from recent symbols (weight 0.4)
	communities := s.getCommunities()
	recentCommSet := make(map[string]bool)

	for _, rid := range recentIDs {
		if communities != nil {
			if cid, ok := communities.NodeToComm[rid]; ok {
				recentCommSet[cid] = true
			}
		}
		// Get neighbors at depth 1-2
		sg := s.engine.GetDependencies(rid, query.QueryOptions{Depth: 2, Limit: 30, Detail: "brief"})
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Closer = higher score
			proximity := 0.5 // depth 2
			// Check if depth 1
			for _, e := range sg.Edges {
				if (e.From == rid && e.To == n.ID) || (e.To == rid && e.From == n.ID) {
					proximity = 1.0
					break
				}
			}
			if proximity > sc.proximity {
				sc.proximity = proximity
				sc.reason = fmt.Sprintf("graph neighbor of %s", rid)
			}
		}
		// Also check dependents (callers)
		callers := s.engine.GetCallers(rid, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief"})
		for _, n := range callers.Nodes {
			if n.ID == rid || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			if 1.0 > sc.proximity {
				sc.proximity = 1.0
				sc.reason = fmt.Sprintf("caller of %s", rid)
			}
		}
	}

	// 3. Community bonus (weight 0.2)
	if communities != nil && len(recentCommSet) > 0 {
		for _, sc := range scoreMap {
			if cid, ok := communities.NodeToComm[sc.node.ID]; ok {
				if recentCommSet[cid] {
					sc.community = 1.0
					if sc.reason == "" {
						sc.reason = "same community as recent activity"
					}
				}
			}
		}
	}

	// Compute combined scores and build candidates
	var candidates []prefetchCandidate
	for id, sc := range scoreMap {
		// Exclude recently viewed symbols themselves
		isRecent := false
		for _, rid := range recentIDs {
			if id == rid {
				isRecent = true
				break
			}
		}
		if isRecent {
			continue
		}

		combined := 0.4*sc.search + 0.4*sc.proximity + 0.2*sc.community
		if combined <= 0 {
			continue
		}
		// Clamp confidence to [0, 1]
		confidence := math.Min(combined, 1.0)

		candidates = append(candidates, prefetchCandidate{
			Node:            sc.node,
			ID:              id,
			Kind:            string(sc.node.Kind),
			FilePath:        sc.node.FilePath,
			StartLine:       sc.node.StartLine,
			Reason:          sc.reason,
			Confidence:      math.Round(confidence*1000) / 1000,
			SearchRelevance: sc.search,
			GraphProximity:  sc.proximity,
			CommunityBonus:  sc.community,
		})
	}

	// Sort by confidence descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Confidence > candidates[j].Confidence
	})

	// Truncate to top 10
	totalCount := len(candidates)
	truncated := false
	if len(candidates) > 10 {
		candidates = candidates[:10]
		truncated = true
	}

	// Include source for top 5 if requested
	if includeSource && s.indexer != nil {
		for i := range candidates {
			if i >= 5 {
				break
			}
			n := candidates[i].Node
			if n.StartLine > 0 && n.EndLine > 0 {
				absPath := n.FilePath
				if root := s.indexer.RootPath(); root != "" {
					absPath = filepath.Join(root, n.FilePath)
				}
				if source, _, _, err := readLines(absPath, n.StartLine, n.EndLine, 0); err == nil {
					candidates[i].Source = source
				}
			}
		}
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range candidates {
			fmt.Fprintf(&b, "%s %s %s:%d %.3f %s\n", c.Kind, c.ID, c.FilePath, c.StartLine, c.Confidence, c.Reason)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"candidates": candidates,
		"total":      totalCount,
		"truncated":  truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.5 handleFindDeadCode and handleFindHotspots
// ---------------------------------------------------------------------------

func (s *Server) handleFindDeadCode(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	opts := analysis.FindDeadCodeOptions{}
	args := req.GetArguments()
	if v, ok := args["include_variables"].(bool); ok && v {
		opts.IncludeVariables = true
	}
	if v, ok := args["include_cgo_exports"].(bool); ok && v {
		opts.IncludeCgoExports = true
	}
	if v, ok := args["include_linkname_targets"].(bool); ok && v {
		opts.IncludeLinknameTargets = true
	}
	if v, ok := args["skip_cross_repo_nodes"].(bool); ok && v {
		opts.SkipCrossRepoNodes = true
	}

	entries := analysis.FindDeadCode(s.graph, s.getProcesses(), nil, opts)

	variablesNote := ""
	if !opts.IncludeVariables {
		variablesNote = "Variables excluded by default (graph lacks intra-function data flow). Pass include_variables=true to include them."
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d\n", e.Kind, e.ID, e.FilePath, e.Line)
		}
		if len(entries) == 0 {
			b.WriteString("no dead code found\n")
		}
		if variablesNote != "" {
			fmt.Fprintf(&b, "\nnote: %s\n", variablesNote)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	result := map[string]any{
		"dead_code": entries,
		"total":     len(entries),
	}
	if variablesNote != "" {
		result["note"] = variablesNote
	}
	return mcp.NewToolResultJSON(result)
}

func (s *Server) handleFindHotspots(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Check minimum graph size
	if s.graph.NodeCount() < 10 {
		return mcp.NewToolResultError("codebase too small for meaningful hotspot analysis (need at least 10 symbols)"), nil
	}

	threshold := 0.0
	if v, ok := req.GetArguments()["threshold"].(float64); ok {
		threshold = v
	}

	entries := analysis.FindHotspots(s.graph, s.getCommunities(), threshold)

	// Truncate to top 20
	totalCount := len(entries)
	truncated := false
	if len(entries) > 20 {
		entries = entries[:20]
		truncated = true
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d score=%.1f fan_in=%d fan_out=%d crossings=%d\n",
				e.Kind, e.ID, e.FilePath, e.Line, e.ComplexityScore, e.FanIn, e.FanOut, e.CommunityCrossings)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"hotspots":  entries,
		"total":     totalCount,
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.6 handleScaffold
// ---------------------------------------------------------------------------

func (s *Server) handleScaffold(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("example_id")
	if err != nil {
		return mcp.NewToolResultError("example_id is required"), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return mcp.NewToolResultError("new_name is required"), nil
	}

	// dry_run defaults to true (scaffold never writes by default)
	dryRun := true
	if v, ok := req.GetArguments()["dry_run"].(bool); ok {
		dryRun = v
	}

	result, err := analysis.GenerateScaffold(s.engine, s.indexer, exampleID, newName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := map[string]any{
		"edits":   result.Edits,
		"notes":   result.Notes,
		"dry_run": dryRun,
	}

	if !dryRun && s.indexer != nil {
		// Apply edits by writing files
		for _, edit := range result.Edits {
			absPath := edit.FilePath
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, edit.FilePath)
			}
			content, readErr := os.ReadFile(absPath)
			if readErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not read %s: %v", edit.FilePath, readErr)), nil
			}
			lines := strings.Split(string(content), "\n")
			insertIdx := edit.InsertionLine - 1
			if insertIdx < 0 {
				insertIdx = 0
			}
			if insertIdx > len(lines) {
				insertIdx = len(lines)
			}
			newLines := make([]string, 0, len(lines)+strings.Count(edit.Code, "\n")+2)
			newLines = append(newLines, lines[:insertIdx]...)
			newLines = append(newLines, "")
			newLines = append(newLines, edit.Code)
			newLines = append(newLines, lines[insertIdx:]...)
			if writeErr := os.WriteFile(absPath, []byte(strings.Join(newLines, "\n")), 0o644); writeErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not write %s: %v", edit.FilePath, writeErr)), nil
			}
		}
		resp["applied"] = true
	}

	return mcp.NewToolResultJSON(resp)
}

// ---------------------------------------------------------------------------
// 10.7 handleFindCycles and handleWouldCreateCycle
// ---------------------------------------------------------------------------

func (s *Server) handleFindCycles(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "")

	cycles := analysis.DetectCycles(s.graph, s.getCommunities(), scope)

	if len(cycles) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"cycles":  []any{},
			"message": "no dependency cycles detected",
		})
	}

	// Truncate to 20 highest-severity (already sorted by severity desc)
	totalCount := len(cycles)
	truncated := false
	if len(cycles) > 20 {
		cycles = cycles[:20]
		truncated = true
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range cycles {
			fmt.Fprintf(&b, "%s severity=%d %s\n", c.Kind, c.Severity, strings.Join(c.Path, " → "))
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"cycles":    cycles,
		"total":     totalCount,
		"truncated": truncated,
	})
}

func (s *Server) handleWouldCreateCycle(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fromID, err := req.RequireString("from_id")
	if err != nil {
		return mcp.NewToolResultError("from_id is required"), nil
	}
	toID, err := req.RequireString("to_id")
	if err != nil {
		return mcp.NewToolResultError("to_id is required"), nil
	}

	// Validate both symbols exist
	if s.graph.GetNode(fromID) == nil {
		return mcp.NewToolResultError("symbol not found: " + fromID), nil
	}
	if s.graph.GetNode(toID) == nil {
		return mcp.NewToolResultError("symbol not found: " + toID), nil
	}

	wouldCycle, path := analysis.WouldCreateCycle(s.graph, fromID, toID)

	if isCompact(req) {
		if wouldCycle {
			return mcp.NewToolResultText(fmt.Sprintf("would_cycle=true %s\n", strings.Join(path, " → "))), nil
		}
		return mcp.NewToolResultText("would_cycle=false\n"), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"would_cycle": wouldCycle,
		"path":        path,
	})
}

// ---------------------------------------------------------------------------
// 10.8 handleDiffContext
// ---------------------------------------------------------------------------

// diffFileGroup groups changed symbols by file with risk assessment.
type diffFileGroup struct {
	FilePath string           `json:"file_path"`
	Risk     string           `json:"risk"`
	Symbols  []diffSymbolInfo `json:"symbols"`
}

// diffSymbolInfo holds enriched context for a single changed symbol.
type diffSymbolInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	Signature string   `json:"signature,omitempty"`
	Source    string   `json:"source,omitempty"`
	Callers   []string `json:"callers,omitempty"`
	Callees   []string `json:"callees,omitempty"`
	Community string   `json:"community,omitempty"`
	Processes []string `json:"processes,omitempty"`
}

func (s *Server) handleDiffContext(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "unstaged")
	baseRef := req.GetString("base_ref", "main")

	repoRoot := "."
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			repoRoot = root
		}
	}

	diff, err := analysis.MapGitDiff(s.graph, repoRoot, scope, baseRef)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if len(diff.ChangedSymbols) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"files":   []any{},
			"message": "no changes detected",
		})
	}

	communities := s.getCommunities()
	processes := s.getProcesses()

	// Build enriched symbol info
	var allSymbols []diffSymbolInfo
	for _, cs := range diff.ChangedSymbols {
		node := s.graph.GetNode(cs.ID)
		if node == nil {
			continue
		}

		info := diffSymbolInfo{
			ID:        cs.ID,
			Name:      cs.Name,
			Kind:      cs.Kind,
			StartLine: cs.Line,
		}

		// Signature
		if sig, ok := node.Meta["signature"].(string); ok {
			info.Signature = sig
		}

		// Source
		if node.StartLine > 0 && node.EndLine > 0 && s.indexer != nil {
			absPath := node.FilePath
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, node.FilePath)
			}
			if source, _, _, readErr := readLines(absPath, node.StartLine, node.EndLine, 0); readErr == nil {
				info.Source = source
			}
		}

		// Callers (depth 1)
		callers := s.engine.GetCallers(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if cn.ID != cs.ID {
				info.Callers = append(info.Callers, cn.ID)
			}
		}

		// Callees (depth 1)
		callees := s.engine.GetCallChain(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callees.Nodes {
			if cn.ID != cs.ID {
				info.Callees = append(info.Callees, cn.ID)
			}
		}

		// Community
		if communities != nil {
			if cid, ok := communities.NodeToComm[cs.ID]; ok {
				info.Community = cid
			}
		}

		// Processes
		if processes != nil {
			info.Processes = processes.NodeToProcs[cs.ID]
		}

		allSymbols = append(allSymbols, info)
	}

	// Group by file
	fileMap := make(map[string][]diffSymbolInfo)
	for _, sym := range allSymbols {
		fp := ""
		if n := s.graph.GetNode(sym.ID); n != nil {
			fp = n.FilePath
		}
		if fp == "" {
			continue
		}
		fileMap[fp] = append(fileMap[fp], sym)
	}

	// Compute per-file risk
	var groups []diffFileGroup
	for fp, syms := range fileMap {
		// Compute risk based on blast radius of symbols in this file
		symbolIDs := make([]string, len(syms))
		for i, sym := range syms {
			symbolIDs[i] = sym.ID
		}
		impact := analysis.AnalyzeImpact(s.graph, symbolIDs, communities, processes)

		groups = append(groups, diffFileGroup{
			FilePath: fp,
			Risk:     string(impact.Risk),
			Symbols:  syms,
		})
	}

	// Sort by risk (CRITICAL > HIGH > MEDIUM > LOW)
	riskOrder := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
	sort.Slice(groups, func(i, j int) bool {
		ri := riskOrder[groups[i].Risk]
		rj := riskOrder[groups[j].Risk]
		if ri != rj {
			return ri < rj
		}
		return groups[i].FilePath < groups[j].FilePath
	})

	// Truncate to 50 symbols total
	totalSymbols := 0
	for _, g := range groups {
		totalSymbols += len(g.Symbols)
	}
	truncated := false
	if totalSymbols > 50 {
		truncated = true
		count := 0
		var truncGroups []diffFileGroup
		for _, g := range groups {
			if count >= 50 {
				break
			}
			remaining := 50 - count
			if len(g.Symbols) > remaining {
				g.Symbols = g.Symbols[:remaining]
			}
			truncGroups = append(truncGroups, g)
			count += len(g.Symbols)
		}
		groups = truncGroups
	}

	if isCompact(req) {
		var b strings.Builder
		for _, g := range groups {
			for _, sym := range g.Symbols {
				fmt.Fprintf(&b, "%s %s %s callers=%d callees=%d\n",
					sym.ID, sym.Kind, g.Risk, len(sym.Callers), len(sym.Callees))
			}
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total symbols)\n", totalSymbols)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"files":         groups,
		"total_symbols": totalSymbols,
		"total_files":   len(groups),
		"truncated":     truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.9 handleIndexHealth
// ---------------------------------------------------------------------------

func (s *Server) handleIndexHealth(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.indexer == nil {
		return mcp.NewToolResultError("no indexer available"), nil
	}

	totalDetected := s.indexer.TotalDetected()
	parseErrors := s.indexer.ParseErrors()

	// When totalDetected is 0 (e.g., graph restored from cache without a full re-index),
	// fall back to counting file nodes in the graph.
	if totalDetected == 0 {
		stats := s.graph.Stats()
		if fileCount, ok := stats.ByKind[string(graph.KindFile)]; ok {
			totalDetected = fileCount
		}
	}

	successfullyIndexed := totalDetected - len(parseErrors)
	if successfullyIndexed < 0 {
		successfullyIndexed = 0
	}

	// Compute health score
	var healthScore float64
	if totalDetected > 0 {
		healthScore = math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
	}

	// Find stale files
	var staleFiles []string
	mtimes := s.indexer.FileMtimes()
	for relPath := range mtimes {
		if s.indexer.IsStale(relPath) {
			staleFiles = append(staleFiles, relPath)
		}
	}

	// Language coverage from graph stats
	stats := s.graph.Stats()
	langCoverage := make(map[string]bool)
	for lang := range stats.ByLanguage {
		langCoverage[lang] = true
	}

	lastIndexTime := s.indexer.LastIndexTime()
	lastIndexStr := ""
	if !lastIndexTime.IsZero() {
		lastIndexStr = lastIndexTime.Format("2006-01-02T15:04:05Z07:00")
	}

	// Recommendation if health < 80%
	var recommendation string
	if healthScore < 80 {
		recommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."
	}

	if isCompact(req) {
		line := fmt.Sprintf("health=%.1f%% nodes=%d stale=%d failures=%d",
			healthScore, stats.TotalNodes, len(staleFiles), len(parseErrors))
		if recommendation != "" {
			line += " [needs re-index]"
		}
		return mcp.NewToolResultText(line + "\n"), nil
	}

	result := map[string]any{
		"health_score":         healthScore,
		"total_detected":       totalDetected,
		"successfully_indexed": successfullyIndexed,
		"language_coverage":    langCoverage,
		"last_index_time":      lastIndexStr,
		"node_count":           stats.TotalNodes,
		"edge_count":           stats.TotalEdges,
	}
	if len(parseErrors) > 0 {
		result["parse_failures"] = parseErrors
	}
	if len(staleFiles) > 0 {
		result["stale_files"] = staleFiles
	}
	if recommendation != "" {
		result["recommendation"] = recommendation
	}

	return mcp.NewToolResultJSON(result)
}

// ---------------------------------------------------------------------------
// 10.10 handleGetSymbolHistory
// ---------------------------------------------------------------------------

func (s *Server) handleGetSymbolHistory(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolID := req.GetString("symbol_id", "")

	if symbolID != "" {
		// Single symbol history
		mods := s.symHistory.Get(symbolID)
		if len(mods) == 0 {
			return mcp.NewToolResultJSON(map[string]any{
				"symbol_id":     symbolID,
				"modifications": []any{},
				"message":       "no modifications recorded for this symbol",
			})
		}

		churning := len(mods) >= 3

		if isCompact(req) {
			churnFlag := ""
			if churning {
				churnFlag = " [churning]"
			}
			return mcp.NewToolResultText(fmt.Sprintf("%s count=%d%s\n", symbolID, len(mods), churnFlag)), nil
		}

		return mcp.NewToolResultJSON(map[string]any{
			"symbol_id":     symbolID,
			"count":         len(mods),
			"modifications": mods,
			"churning":      churning,
		})
	}

	// All symbols, sorted by count descending
	all := s.symHistory.All()
	if len(all) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"symbols": []any{},
			"message": "no modifications recorded this session",
		})
	}

	type symbolEntry struct {
		SymbolID      string               `json:"symbol_id"`
		Count         int                  `json:"count"`
		Churning      bool                 `json:"churning"`
		Modifications []SymbolModification `json:"modifications"`
	}

	var entries []symbolEntry
	for id, mods := range all {
		entries = append(entries, symbolEntry{
			SymbolID:      id,
			Count:         len(mods),
			Churning:      len(mods) >= 3,
			Modifications: mods,
		})
	}

	// Sort by count descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Count > entries[j].Count
	})

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			churnFlag := ""
			if e.Churning {
				churnFlag = " [churning]"
			}
			fmt.Fprintf(&b, "%s count=%d%s\n", e.SymbolID, e.Count, churnFlag)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"symbols": entries,
		"total":   len(entries),
	})
}

// ---------------------------------------------------------------------------
// 10.11 handleBatchEdit
// ---------------------------------------------------------------------------

// batchEditItem represents a single edit in a batch.
type batchEditItem struct {
	SymbolID  string `json:"symbol_id"`
	OldSource string `json:"old_source"`
	NewSource string `json:"new_source"`
}

// batchEditResult represents the outcome of a single edit in the batch.
type batchEditResult struct {
	SymbolID string `json:"symbol_id"`
	FilePath string `json:"file_path"`
	Status   string `json:"status"` // "applied", "failed", "skipped"
	Error    string `json:"error,omitempty"`
}

func (s *Server) handleBatchEdit(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	editsStr, err := req.RequireString("edits")
	if err != nil {
		return mcp.NewToolResultError("edits is required"), nil
	}

	var edits []batchEditItem
	if err := json.Unmarshal([]byte(editsStr), &edits); err != nil {
		return mcp.NewToolResultError("invalid edits JSON: " + err.Error()), nil
	}
	if len(edits) == 0 {
		return mcp.NewToolResultError("edits array is empty"), nil
	}

	dryRun := false
	if v, ok := req.GetArguments()["dry_run"].(bool); ok {
		dryRun = v
	}

	// Sort edits in dependency order using get_edit_plan logic:
	// Definitions first, then implementations, then callers.
	type editWithOrder struct {
		edit  batchEditItem
		order int
		file  string
	}

	var ordered []editWithOrder
	for _, edit := range edits {
		node := s.engine.GetSymbol(edit.SymbolID)
		order := 50 // default middle priority
		filePath := ""
		if node != nil {
			filePath = node.FilePath
			// Definitions/interfaces get lowest order (edited first)
			if node.Kind == graph.KindInterface || node.Kind == graph.KindType {
				order = 0
			} else if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
				// Check if this symbol is depended on by other edits
				for _, other := range edits {
					if other.SymbolID == edit.SymbolID {
						continue
					}
					// Check if other calls this symbol
					callers := s.engine.GetCallers(edit.SymbolID, query.QueryOptions{Depth: 1, Limit: 100, Detail: "brief"})
					for _, cn := range callers.Nodes {
						if cn.ID == other.SymbolID {
							order = 10 // this is a dependency — edit first
							break
						}
					}
				}
				if order == 50 {
					order = 20 // regular function
				}
			}
		}
		ordered = append(ordered, editWithOrder{edit: edit, order: order, file: filePath})
	}

	// Sort by order ascending (lowest = edit first)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].file < ordered[j].file
	})

	if dryRun {
		var plan []map[string]any
		for i, o := range ordered {
			entry := map[string]any{
				"order":     i + 1,
				"symbol_id": o.edit.SymbolID,
				"file_path": o.file,
				"status":    "planned",
			}
			plan = append(plan, entry)
		}

		if isCompact(req) {
			var b strings.Builder
			for _, p := range plan {
				fmt.Fprintf(&b, "%s %s planned\n", p["symbol_id"], p["file_path"])
			}
			return mcp.NewToolResultText(b.String()), nil
		}

		return mcp.NewToolResultJSON(map[string]any{
			"plan":    plan,
			"dry_run": true,
			"total":   len(plan),
		})
	}

	// Apply edits sequentially
	var results []batchEditResult
	failed := false

	for _, o := range ordered {
		if failed {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: o.file,
				Status:   "skipped",
			})
			continue
		}

		node := s.engine.GetSymbol(o.edit.SymbolID)
		if node == nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: o.file,
				Status:   "failed",
				Error:    "symbol not found: " + o.edit.SymbolID,
			})
			failed = true
			continue
		}

		if node.StartLine == 0 || node.EndLine == 0 {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "symbol has no line range",
			})
			failed = true
			continue
		}

		// Resolve file path
		absPath := node.FilePath
		if s.indexer != nil {
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, node.FilePath)
			}
		}

		// Read file
		content, readErr := os.ReadFile(absPath)
		if readErr != nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    fmt.Sprintf("could not read file: %v", readErr),
			})
			failed = true
			continue
		}

		fileStr := string(content)
		lines := strings.Split(fileStr, "\n")

		if node.StartLine > len(lines) || node.EndLine > len(lines) {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "symbol line range exceeds file length",
			})
			failed = true
			continue
		}

		symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")
		if !strings.Contains(symbolSource, o.edit.OldSource) {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "old_source not found within symbol",
			})
			failed = true
			continue
		}

		// Compute offset within file
		symbolStart := 0
		for i := 0; i < node.StartLine-1 && i < len(lines); i++ {
			symbolStart += len(lines[i]) + 1
		}
		symbolEnd := symbolStart + len(symbolSource)
		if symbolEnd > len(fileStr) {
			symbolEnd = len(fileStr)
		}

		offset := strings.Index(fileStr[symbolStart:symbolEnd], o.edit.OldSource)
		if offset < 0 {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    "old_source not found in symbol region",
			})
			failed = true
			continue
		}

		editStart := symbolStart + offset
		editEnd := editStart + len(o.edit.OldSource)
		newContent := fileStr[:editStart] + o.edit.NewSource + fileStr[editEnd:]

		if writeErr := os.WriteFile(absPath, []byte(newContent), 0o644); writeErr != nil {
			results = append(results, batchEditResult{
				SymbolID: o.edit.SymbolID,
				FilePath: node.FilePath,
				Status:   "failed",
				Error:    fmt.Sprintf("could not write file: %v", writeErr),
			})
			failed = true
			continue
		}

		// Re-index the file after edit
		if s.indexer != nil {
			_ = s.indexer.IndexFile(absPath)
		}

		results = append(results, batchEditResult{
			SymbolID: o.edit.SymbolID,
			FilePath: node.FilePath,
			Status:   "applied",
		})
	}

	if isCompact(req) {
		var b strings.Builder
		for _, r := range results {
			fmt.Fprintf(&b, "%s %s %s\n", r.SymbolID, r.FilePath, r.Status)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	// Count statuses
	applied, failedCount, skipped := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "applied":
			applied++
		case "failed":
			failedCount++
		case "skipped":
			skipped++
		}
	}

	return mcp.NewToolResultJSON(map[string]any{
		"results": results,
		"summary": map[string]int{
			"applied": applied,
			"failed":  failedCount,
			"skipped": skipped,
			"total":   len(results),
		},
	})
}

// ---------------------------------------------------------------------------
// handleGetContracts
// ---------------------------------------------------------------------------

func (s *Server) handleGetContracts(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.contractRegistry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	repo := req.GetString("repo", "")
	contractType := req.GetString("type", "")
	role := req.GetString("role", "")

	var all []contracts.Contract
	if repo != "" {
		all = s.contractRegistry.ByRepo(repo)
	} else {
		all = s.contractRegistry.All()
	}

	// Apply filters.
	var filtered []contracts.Contract
	for _, c := range all {
		if contractType != "" && string(c.Type) != contractType {
			continue
		}
		if role != "" && string(c.Role) != role {
			continue
		}
		filtered = append(filtered, c)
	}

	if isCompact(req) {
		var b strings.Builder
		// Group by repo for readability in multi-repo mode.
		byRepo := make(map[string][]contracts.Contract)
		for _, c := range filtered {
			repo := c.RepoPrefix
			if repo == "" {
				repo = "(default)"
			}
			byRepo[repo] = append(byRepo[repo], c)
		}
		for repoName, items := range byRepo {
			if len(byRepo) > 1 {
				fmt.Fprintf(&b, "\n[%s] (%d contracts)\n", repoName, len(items))
			}
			for _, c := range items {
				fmt.Fprintf(&b, "%s %s %s %s:%d\n", c.Type, c.Role, c.ID, c.FilePath, c.Line)
			}
		}
		if len(filtered) == 0 {
			b.WriteString("no contracts found\n")
		}
		fmt.Fprintf(&b, "total: %d contracts\n", len(filtered))
		return mcp.NewToolResultText(b.String()), nil
	}

	// Group by repo, then by type for structured output.
	type repoGroup struct {
		Contracts map[string][]contracts.Contract `json:"contracts"`
		Total     int                             `json:"total"`
	}
	byRepo := make(map[string]*repoGroup)
	for _, c := range filtered {
		repo := c.RepoPrefix
		if repo == "" {
			repo = "(default)"
		}
		if byRepo[repo] == nil {
			byRepo[repo] = &repoGroup{Contracts: make(map[string][]contracts.Contract)}
		}
		byRepo[repo].Contracts[string(c.Type)] = append(byRepo[repo].Contracts[string(c.Type)], c)
		byRepo[repo].Total++
	}

	return mcp.NewToolResultJSON(map[string]any{
		"by_repo": byRepo,
		"total":   len(filtered),
	})
}

// ---------------------------------------------------------------------------
// handleCheckContracts
// ---------------------------------------------------------------------------

func (s *Server) handleCheckContracts(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.contractRegistry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	repo := req.GetString("repo", "")

	// If repo is specified, build a filtered registry for matching.
	reg := s.contractRegistry
	if repo != "" {
		reg = contracts.NewRegistry()
		for _, c := range s.contractRegistry.ByRepo(repo) {
			reg.Add(c)
		}
	}

	result := contracts.Match(reg)

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "matched: %d pairs\n", len(result.Matched))
		for _, m := range result.Matched {
			cross := ""
			if m.CrossRepo {
				cross = " [cross-repo]"
			}
			provRepo := m.Provider.RepoPrefix
			consRepo := m.Consumer.RepoPrefix
			if provRepo == "" {
				provRepo = "(default)"
			}
			if consRepo == "" {
				consRepo = "(default)"
			}
			fmt.Fprintf(&b, "  %s: [%s] %s:%d -> [%s] %s:%d%s\n",
				m.ContractID,
				provRepo, m.Provider.FilePath, m.Provider.Line,
				consRepo, m.Consumer.FilePath, m.Consumer.Line,
				cross)
		}
		fmt.Fprintf(&b, "orphan providers: %d\n", len(result.OrphanProviders))
		for _, o := range result.OrphanProviders {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		fmt.Fprintf(&b, "orphan consumers: %d\n", len(result.OrphanConsumers))
		for _, o := range result.OrphanConsumers {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return mcp.NewToolResultJSON(map[string]any{
		"matched":          result.Matched,
		"orphan_providers": result.OrphanProviders,
		"orphan_consumers": result.OrphanConsumers,
		"summary": map[string]int{
			"matched_pairs":    len(result.Matched),
			"orphan_providers": len(result.OrphanProviders),
			"orphan_consumers": len(result.OrphanConsumers),
		},
	})
}
