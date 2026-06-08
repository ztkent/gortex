package mcp

import (
	"context"
	"math"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func (s *Server) registerAnalysisTools() {
	s.addTool(
		mcp.NewTool("get_communities",
			mcp.WithDescription("Returns functional clusters discovered by community detection. Without id: list all communities with summaries. With id: full details of a specific community (members, files, cohesion)."),
			mcp.WithString("id", mcp.Description("Optional community ID (e.g. community-0). When set, returns full details of that community instead of the list.")),
		),
		s.handleGetCommunities,
	)

	s.addTool(
		mcp.NewTool("get_processes",
			mcp.WithDescription("Returns discovered execution flows — named chains of function calls starting from entry points. Without id: list all processes. With id: full step-by-step call chain for that process."),
			mcp.WithString("id", mcp.Description("Optional process ID (e.g. process-0). When set, returns the full step-by-step call chain for that process instead of the list.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetProcesses,
	)

	s.addTool(
		mcp.NewTool("detect_changes",
			mcp.WithDescription("Maps uncommitted git changes to symbols in the graph and runs blast radius analysis. The key pre-commit review tool."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
		),
		s.handleDetectChanges,
	)

	s.addTool(
		mcp.NewTool("suggest_queries",
			mcp.WithDescription("Cold-start helper: returns 5-10 starter exploration queries for an unfamiliar repository, derived from its entry points, load-bearing hubs, community bridges, and largest subsystems. Run at session start to orient before reaching for search_symbols / smart_context."),
			mcp.WithNumber("limit", mcp.Description("Max suggestions to return (default 8, capped at 20).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleSuggestQueries,
	)

	s.addTool(
		mcp.NewTool("search_text",
			mcp.WithDescription("Trigram-accelerated literal (or regexp) code search across the indexed repository — the alt grep backbone. Each hit carries the enclosing graph symbol (symbol_id / symbol_name) so you see which function or method a match landed in without a follow-up call. A trigram index narrows the candidate files, so a repo-wide search costs roughly the size of the matching files, not the whole tree. Use for literal-string / regexp lookups; use search_symbols for symbol-name / concept queries."),
			mcp.WithString("query", mcp.Description("Literal substring (case-sensitive) to search for — or a regular expression when regexp=true.")),
			mcp.WithBoolean("regexp", mcp.Description("Treat query as a regular expression instead of a literal substring. An invalid pattern is returned as a tool error. Default false.")),
			mcp.WithNumber("limit", mcp.Description("Max matching lines to return (default 100, capped at 1000).")),
			mcp.WithString("path", mcp.Description("Restrict matches to one or more sub-paths (comma-separated) -- a monorepo-service slice. Anchored, slash-segment-boundary prefixes relative to the repo root.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) -- when the scope carries sub-paths, they narrow the matches.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleSearchText,
	)
}

func (s *Server) handleGetCommunities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	comms := s.getCommunities()

	// If id is provided, return the single community in detail.
	if id := req.GetString("id", ""); id != "" {
		if comms == nil {
			return mcp.NewToolResultError("no communities detected yet"), nil
		}
		for _, c := range comms.Communities {
			if c.ID == id {
				return s.respondJSONOrTOON(ctx, req, c)
			}
		}
		return mcp.NewToolResultError("community not found: " + id), nil
	}

	// Otherwise return the list of summaries.
	if comms == nil || len(comms.Communities) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"communities": []any{},
			"message":     "no communities detected yet — run index_repository first",
		})
	}

	// List mode deliberately omits per-community `files` (can be hundreds
	// of paths each). Callers who want that drill into a specific
	// community via `id`; the detail response includes the full member
	// set. `file_count` preserves size signal without the string array.
	// `repo_prefix` is the majority repo of the community's members so
	// UIs can render a badge without paging through every member id.
	type summary struct {
		ID         string  `json:"id"`
		Label      string  `json:"label"`
		Size       int     `json:"size"`
		FileCount  int     `json:"file_count"`
		Cohesion   float64 `json:"cohesion"`
		RepoPrefix string  `json:"repo_prefix"`
		ParentID   string  `json:"parent_id,omitempty"`
	}
	var summaries []summary
	for _, c := range comms.Communities {
		summaries = append(summaries, summary{
			ID:         c.ID,
			Label:      c.Label,
			Size:       c.Size,
			FileCount:  len(c.Files),
			Cohesion:   c.Cohesion,
			RepoPrefix: majorityRepoPrefix(c.Members),
			ParentID:   c.ParentID,
		})
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"communities": summaries,
		"total":       len(summaries),
		"modularity":  comms.Modularity,
	})
}

func (s *Server) handleGetProcesses(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	procs := s.getProcesses()

	// If id is provided, return the single process in detail.
	if id := req.GetString("id", ""); id != "" {
		if procs == nil {
			return mcp.NewToolResultError("no processes discovered yet"), nil
		}
		for _, p := range procs.Processes {
			if p.ID == id {
				return s.respondJSONOrTOON(ctx, req, p)
			}
		}
		return mcp.NewToolResultError("process not found: " + id), nil
	}

	// Otherwise return the list of summaries.
	if procs == nil || len(procs.Processes) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"processes": []any{},
			"message":   "no processes discovered yet — run index_repository first",
		})
	}

	// `repo_prefixes` is the ordered set of distinct "owner/repo" prefixes
	// the flow's steps cross — the UI renders these as trail badges
	// without needing the full step id list.
	type summary struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		EntryPoint   string   `json:"entry_point"`
		StepCount    int      `json:"step_count"`
		FileCount    int      `json:"file_count"`
		Score        float64  `json:"score"`
		RepoPrefixes []string `json:"repo_prefixes"`
	}
	var summaries []summary
	for _, p := range procs.Processes {
		summaries = append(summaries, summary{
			ID:           p.ID,
			Name:         p.Name,
			EntryPoint:   p.EntryPoint,
			StepCount:    p.StepCount,
			FileCount:    len(p.Files),
			Score:        p.Score,
			RepoPrefixes: uniqueRepoPrefixesFromSteps(p.Steps),
		})
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"processes": summaries,
		"total":     len(summaries),
	})
}

// repoPrefixOf extracts the repo prefix from a node ID of the form
// "<repoPrefix>/<file-path>::<symbol>". The first `/` separates the
// repo name from the file path, and `::` separates the file from the
// symbol. IDs that don't contain `/` before the `::` (e.g.
// "unresolved::OSTRACE") have no repo prefix and return empty.
func repoPrefixOf(id string) string {
	pathPart := id
	if i := strings.Index(id, "::"); i >= 0 {
		pathPart = id[:i]
	}
	if j := strings.Index(pathPart, "/"); j >= 0 {
		return pathPart[:j]
	}
	return ""
}

// majorityRepoPrefix returns the most common repo prefix from a list of
// node IDs. Empty when no ID carries a prefix.
func majorityRepoPrefix(ids []string) string {
	counts := make(map[string]int, 4)
	for _, id := range ids {
		if p := repoPrefixOf(id); p != "" {
			counts[p]++
		}
	}
	best := ""
	bestN := 0
	for k, n := range counts {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	return best
}

// uniqueRepoPrefixesFromSteps returns the ordered set of distinct repo
// prefixes touched by a process flow, preserving DFS order so the UI
// renders "crosses" badges in call sequence rather than alphabetical.
func uniqueRepoPrefixesFromSteps(steps []analysis.Step) []string {
	seen := make(map[string]struct{}, 4)
	out := make([]string, 0, 4)
	for _, s := range steps {
		p := repoPrefixOf(s.ID)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func (s *Server) handleDetectChanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "unstaged")
	baseRef := req.GetString("base_ref", "main")

	// Determine repo root from the indexer's last indexed path
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
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"changed_symbols": []any{},
			"changed_files":   diff.ChangedFiles,
			"risk":            "NONE",
			"summary":         "no indexed symbols affected by current changes",
		})
	}

	// Run impact analysis on the changed symbols
	symbolIDs := make([]string, len(diff.ChangedSymbols))
	for i, cs := range diff.ChangedSymbols {
		symbolIDs[i] = cs.ID
	}

	impact := analysis.AnalyzeImpact(s.graph, symbolIDs, s.getCommunities(), s.getProcesses())

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"changed_symbols":      diff.ChangedSymbols,
		"changed_files":        diff.ChangedFiles,
		"risk":                 impact.Risk,
		"summary":              impact.Summary,
		"by_depth":             impact.ByDepth,
		"affected_processes":   impact.AffectedProcesses,
		"affected_communities": impact.AffectedCommunities,
		"test_files":           impact.TestFiles,
		"total_affected":       impact.TotalAffected,
	})
}

// handleEnhancedChangeImpact replaces the original explain_change_impact with risk tiering
// and cross-community warnings.
func (s *Server) handleEnhancedChangeImpact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	impact := analysis.AnalyzeImpact(s.graph, ids, s.getCommunities(), s.getProcesses())

	result := map[string]any{
		"risk":                 impact.Risk,
		"summary":              impact.Summary,
		"by_depth":             impact.ByDepth,
		"affected_processes":   impact.AffectedProcesses,
		"affected_communities": impact.AffectedCommunities,
		"test_files":           impact.TestFiles,
		"total_affected":       impact.TotalAffected,
		"cross_repo_impact":    impact.CrossRepoImpact,
	}

	// Include per-repo grouping when cross-repo impact is detected.
	if impact.CrossRepoImpact {
		result["by_repo"] = impact.ByRepo
	}

	// Epistemic lower bound: the affected count is a floor when the blast
	// radius crosses a dynamic-dispatch / interface site the resolver could
	// not bind. Surface the flag + the boundary list so an agent knows
	// ">=N, could be more" and can act on each named site.
	if impact.LowerBound {
		result["lower_bound"] = true
	}
	if len(impact.Boundaries) > 0 {
		result["boundaries"] = impact.Boundaries
	}

	// When the blast radius is empty, an agent cannot tell genuinely
	// safe-to-change symbols apart from symbols the extractor never
	// wired up. Classify each input so a safety gate is not disarmed
	// by a false "0 affected".
	if impact.TotalAffected == 0 {
		var caveats []graph.ZeroImpactCaveat
		for _, id := range ids {
			if id == "" {
				continue
			}
			if c := graph.CaveatForZeroEdge(s.graph, id); c != nil {
				caveats = append(caveats, graph.ZeroImpactCaveat{
					ID:      id,
					Class:   c.Class,
					Message: c.Message,
				})
			}
		}
		if len(caveats) > 0 {
			result["zero_impact_caveat"] = caveats
		}
	}

	// Cross-community warning
	if len(impact.AffectedCommunities) >= 2 {
		communities := s.getCommunities()
		warning := s.computeCrossCommunityWarning(impact.AffectedCommunities, communities)
		result["cross_community_warning"] = warning
	} else {
		result["cross_community_warning"] = nil
		if len(impact.AffectedCommunities) == 1 {
			result["community_note"] = "change is community-local"
		}
	}

	// Contract impact — if any of the changed symbols is referenced
	// as a request/response body by a declared contract, surface the
	// full list so the reviewer sees "this struct backs N routes"
	// before the edit lands. Live validate pass runs on the affected
	// contracts so existing breaking drift is reported alongside the
	// pending-change blast radius.
	if ci := s.computeContractImpact(ids); ci != nil {
		result["contract_impact"] = ci
		if impact.Risk == analysis.RiskLow && ci.Breaking > 0 {
			result["risk"] = analysis.RiskHigh
			result["contract_risk_upgrade"] = "risk raised to HIGH — type is a contract boundary with breaking drift"
		}
	}

	if s.isGCX(ctx, req) {
		// encodeChangeImpact reads the same map shape we'd return as
		// JSON; routing through it keeps a single source of truth for
		// field names and avoids divergence on the next analyzer
		// addition.
		return s.gcxResponseWithBudget(req)(encodeChangeImpact(result))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// -----------------------------------------------------------------------------
// Contract impact helper
// -----------------------------------------------------------------------------

// contractImpact enumerates the contracts that reference one of the
// input type IDs as a request or response body, and rolls up the
// current validation issues for that subset so change-review sees
// breaking drift in the same payload as community / risk info.
type contractImpact struct {
	Affected     []contractImpactEntry     `json:"affected"`
	Breaking     int                       `json:"breaking"`
	Warning      int                       `json:"warning"`
	Info         int                       `json:"info"`
	SampleIssues []contracts.ContractIssue `json:"sample_issues,omitempty"`
}

type contractImpactEntry struct {
	ContractID string `json:"contract_id"`
	Position   string `json:"position"` // request | response
	Role       string `json:"role"`     // provider | consumer
	Repo       string `json:"repo"`
	TypeID     string `json:"type_id"`
}

// computeContractImpact walks every contract in the effective
// registry and returns the ones whose request_type or response_type
// matches any of the changed symbol IDs. Returns nil when nothing
// matches so the JSON payload stays compact.
func (s *Server) computeContractImpact(changedIDs []string) *contractImpact {
	reg := s.effectiveContractRegistry()
	if reg == nil {
		return nil
	}
	changed := make(map[string]struct{}, len(changedIDs))
	for _, id := range changedIDs {
		changed[id] = struct{}{}
	}

	var entries []contractImpactEntry
	affectedIDs := make(map[string]struct{})
	for _, c := range reg.All() {
		reqType := impactMetaString(c.Meta, "request_type")
		respType := impactMetaString(c.Meta, "response_type")
		if _, hit := changed[reqType]; hit && reqType != "" {
			entries = append(entries, contractImpactEntry{
				ContractID: c.ID, Position: "request",
				Role: string(c.Role), Repo: c.RepoPrefix, TypeID: reqType,
			})
			affectedIDs[c.ID] = struct{}{}
		}
		if _, hit := changed[respType]; hit && respType != "" {
			entries = append(entries, contractImpactEntry{
				ContractID: c.ID, Position: "response",
				Role: string(c.Role), Repo: c.RepoPrefix, TypeID: respType,
			})
			affectedIDs[c.ID] = struct{}{}
		}
	}
	if len(entries) == 0 {
		return nil
	}

	// Validate the affected subset only — Validate on the full
	// registry would drown the payload in unrelated drift.
	sub := contracts.NewRegistry()
	for _, c := range reg.All() {
		if _, ok := affectedIDs[c.ID]; ok {
			sub.Add(c)
		}
	}
	lookup := contracts.ShapeLookup(func(id string) *contracts.Shape {
		n := s.graph.GetNode(id)
		if n == nil || n.Meta == nil {
			return nil
		}
		switch v := n.Meta["shape"].(type) {
		case *contracts.Shape:
			return v
		case contracts.Shape:
			return &v
		}
		return nil
	})
	issues := contracts.Validate(sub, lookup)

	out := &contractImpact{Affected: entries}
	for _, is := range issues {
		switch is.Severity {
		case contracts.SeverityBreaking:
			out.Breaking++
		case contracts.SeverityWarning:
			out.Warning++
		case contracts.SeverityInfo:
			out.Info++
		}
	}
	// Keep the first 10 issues inline; full list is always one
	// `contracts validate` call away.
	if len(issues) > 10 {
		out.SampleIssues = issues[:10]
	} else {
		out.SampleIssues = issues
	}
	return out
}

func impactMetaString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// CommunityCoupling describes the coupling between two communities.
type CommunityCoupling struct {
	CommunityA     string  `json:"community_a"`
	CommunityB     string  `json:"community_b"`
	LabelA         string  `json:"label_a"`
	LabelB         string  `json:"label_b"`
	CouplingScore  float64 `json:"coupling_score"`
	TightlyCoupled bool    `json:"tightly_coupled"`
}

// CrossCommunityWarning describes cross-community impact.
type CrossCommunityWarning struct {
	AffectedCommunities []string            `json:"affected_communities"`
	Couplings           []CommunityCoupling `json:"couplings,omitempty"`
}

func (s *Server) computeCrossCommunityWarning(affectedCommunities []string, communities *analysis.CommunityResult) *CrossCommunityWarning {
	warning := &CrossCommunityWarning{
		AffectedCommunities: affectedCommunities,
	}

	if communities == nil {
		return warning
	}

	// Build community label lookup
	commLabels := make(map[string]string)
	commMembers := make(map[string]map[string]bool)
	for _, c := range communities.Communities {
		commLabels[c.ID] = c.Label
		memberSet := make(map[string]bool, len(c.Members))
		for _, m := range c.Members {
			memberSet[m] = true
		}
		commMembers[c.ID] = memberSet
	}

	// For each pair of affected communities, compute coupling score
	for i := 0; i < len(affectedCommunities); i++ {
		for j := i + 1; j < len(affectedCommunities); j++ {
			cA := affectedCommunities[i]
			cB := affectedCommunities[j]

			membersA := commMembers[cA]
			membersB := commMembers[cB]

			if len(membersA) == 0 || len(membersB) == 0 {
				continue
			}

			// Count edges crossing the boundary and total edges in both communities
			crossBoundary := 0
			totalEdges := 0

			edges := s.graph.AllEdges()
			for _, e := range edges {
				inA := membersA[e.From] || membersA[e.To]
				inB := membersB[e.From] || membersB[e.To]

				if inA || inB {
					totalEdges++
				}
				// Cross-boundary: one end in A, other in B
				if (membersA[e.From] && membersB[e.To]) || (membersB[e.From] && membersA[e.To]) {
					crossBoundary++
				}
			}

			var couplingScore float64
			if totalEdges > 0 {
				couplingScore = math.Round(float64(crossBoundary)/float64(totalEdges)*10000) / 100
			}

			warning.Couplings = append(warning.Couplings, CommunityCoupling{
				CommunityA:     cA,
				CommunityB:     cB,
				LabelA:         commLabels[cA],
				LabelB:         commLabels[cB],
				CouplingScore:  couplingScore,
				TightlyCoupled: couplingScore > 15,
			})
		}
	}

	return warning
}
