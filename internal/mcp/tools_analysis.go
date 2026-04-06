package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
)

func (s *Server) registerAnalysisTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("get_communities",
			mcp.WithDescription("Returns functional clusters discovered by community detection. Each cluster groups symbols that work together. Use to understand the architecture at a module level without reading files."),
		),
		s.handleGetCommunities,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_community",
			mcp.WithDescription("Returns details of a specific community: all member symbols, files, and cohesion score."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Community ID (e.g. community-0)")),
		),
		s.handleGetCommunity,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_processes",
			mcp.WithDescription("Returns discovered execution flows — named chains of function calls starting from entry points (main, handlers, controllers). Use to understand what the code does, not just what calls what."),
		),
		s.handleGetProcesses,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_process",
			mcp.WithDescription("Returns the full step-by-step call chain for a specific execution flow."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Process ID (e.g. process-0)")),
		),
		s.handleGetProcess,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("detect_changes",
			mcp.WithDescription("Maps uncommitted git changes to symbols in the graph and runs blast radius analysis. The key pre-commit review tool."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
		),
		s.handleDetectChanges,
	)
}

func (s *Server) handleGetCommunities(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	comms := s.getCommunities()
	if comms == nil || len(comms.Communities) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"communities": []any{},
			"message":     "no communities detected yet — run index_repository first",
		})
	}

	// Return summaries (not full member lists)
	type summary struct {
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		Size     int      `json:"size"`
		Files    []string `json:"files"`
		Cohesion float64  `json:"cohesion"`
	}
	var summaries []summary
	for _, c := range comms.Communities {
		summaries = append(summaries, summary{
			ID:       c.ID,
			Label:    c.Label,
			Size:     c.Size,
			Files:    c.Files,
			Cohesion: c.Cohesion,
		})
	}
	return mcp.NewToolResultJSON(map[string]any{
		"communities": summaries,
		"total":       len(summaries),
		"modularity":  comms.Modularity,
	})
}

func (s *Server) handleGetCommunity(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	comms := s.getCommunities()
	if comms == nil {
		return mcp.NewToolResultError("no communities detected yet"), nil
	}

	for _, c := range comms.Communities {
		if c.ID == id {
			return mcp.NewToolResultJSON(c)
		}
	}
	return mcp.NewToolResultError("community not found: " + id), nil
}

func (s *Server) handleGetProcesses(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	procs := s.getProcesses()
	if procs == nil || len(procs.Processes) == 0 {
		return mcp.NewToolResultJSON(map[string]any{
			"processes": []any{},
			"message":   "no processes discovered yet — run index_repository first",
		})
	}

	// Return summaries
	type summary struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		EntryPoint string  `json:"entry_point"`
		StepCount  int     `json:"step_count"`
		FileCount  int     `json:"file_count"`
		Score      float64 `json:"score"`
	}
	var summaries []summary
	for _, p := range procs.Processes {
		summaries = append(summaries, summary{
			ID:         p.ID,
			Name:       p.Name,
			EntryPoint: p.EntryPoint,
			StepCount:  p.StepCount,
			FileCount:  len(p.Files),
			Score:      p.Score,
		})
	}
	return mcp.NewToolResultJSON(map[string]any{
		"processes": summaries,
		"total":     len(summaries),
	})
}

func (s *Server) handleGetProcess(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	procs := s.getProcesses()
	if procs == nil {
		return mcp.NewToolResultError("no processes discovered yet"), nil
	}

	for _, p := range procs.Processes {
		if p.ID == id {
			return mcp.NewToolResultJSON(p)
		}
	}
	return mcp.NewToolResultError("process not found: " + id), nil
}

func (s *Server) handleDetectChanges(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		return mcp.NewToolResultJSON(map[string]any{
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

	return mcp.NewToolResultJSON(map[string]any{
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

// handleEnhancedChangeImpact replaces the original explain_change_impact with risk tiering.
func (s *Server) handleEnhancedChangeImpact(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("symbol_ids")
	if err != nil {
		return mcp.NewToolResultError("symbol_ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	impact := analysis.AnalyzeImpact(s.graph, ids, s.getCommunities(), s.getProcesses())
	return mcp.NewToolResultJSON(impact)
}
