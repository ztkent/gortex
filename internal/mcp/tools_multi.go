package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// registerMultiRepoTools registers MCP tools for multi-repo management:
// track_repository, untrack_repository, set_active_project, get_active_project.
func (s *Server) registerMultiRepoTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("track_repository",
			mcp.WithDescription("Add a repository to the tracked workspace at runtime. Indexes immediately and persists to config."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
			mcp.WithString("name", mcp.Description("Optional repo prefix override")),
		),
		s.handleTrackRepository,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("untrack_repository",
			mcp.WithDescription("Remove a repository from the tracked workspace at runtime. Evicts nodes/edges and persists to config."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path or repo prefix to remove")),
		),
		s.handleUntrackRepository,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("set_active_project",
			mcp.WithDescription("Switch the active project scope. Persists to config and re-scopes all subsequent queries."),
			mcp.WithString("project", mcp.Required(), mcp.Description("Project name to activate")),
		),
		s.handleSetActiveProject,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("get_active_project",
			mcp.WithDescription("Return the current active project name and its list of member repositories."),
		),
		s.handleGetActiveProject,
	)
}

// handleTrackRepository validates the path, indexes the repo, and persists to GlobalConfig.
func (s *Server) handleTrackRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	// Validate path exists and is a directory.
	info, statErr := os.Stat(path)
	if statErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid path: %s", path)), nil
	}
	if !info.IsDir() {
		return mcp.NewToolResultError(fmt.Sprintf("invalid path: %s (not a directory)", path)), nil
	}

	if s.multiIndexer == nil {
		return mcp.NewToolResultError("multi-repo indexing is not enabled"), nil
	}

	entry := config.RepoEntry{Path: path}
	if name, ok := req.GetArguments()["name"].(string); ok && name != "" {
		entry.Name = name
	}

	result, trackErr := s.multiIndexer.TrackRepoCtx(s.progressCtx(ctx, req), entry)
	if trackErr != nil {
		return mcp.NewToolResultError(trackErr.Error()), nil
	}

	// Already tracked — TrackRepo returns nil result when repo exists.
	if result == nil {
		return mcp.NewToolResultText("repository already tracked"), nil
	}

	// Persist updated config.
	if s.configManager != nil {
		if saveErr := s.configManager.Global().Save(); saveErr != nil {
			s.logger.Warn("failed to persist config after tracking repo",
				zap.String("path", path), zap.Error(saveErr))
		}
	}

	// Re-run analysis after adding a new repo.
	s.RunAnalysis()

	return mcp.NewToolResultJSON(map[string]any{
		"status":     "tracked",
		"path":       path,
		"prefix":     config.ResolvePrefix(entry),
		"file_count": result.FileCount,
		"node_count": result.NodeCount,
		"edge_count": result.EdgeCount,
	})
}

// handleUntrackRepository removes a repo from the workspace and persists to GlobalConfig.
func (s *Server) handleUntrackRepository(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError("path is required"), nil
	}

	if s.multiIndexer == nil {
		return mcp.NewToolResultError("multi-repo indexing is not enabled"), nil
	}

	// Try to find the repo by prefix or by path.
	prefix := s.resolveRepoPrefix(path)
	if prefix == "" {
		return mcp.NewToolResultError(fmt.Sprintf("repository not tracked: %s", path)), nil
	}

	nodesRemoved, edgesRemoved := s.multiIndexer.UntrackRepo(prefix)

	// Persist updated config.
	if s.configManager != nil {
		if saveErr := s.configManager.Global().Save(); saveErr != nil {
			s.logger.Warn("failed to persist config after untracking repo",
				zap.String("path", path), zap.Error(saveErr))
		}
	}

	// Re-run analysis after removing a repo.
	s.RunAnalysis()

	return mcp.NewToolResultJSON(map[string]any{
		"status":        "untracked",
		"prefix":        prefix,
		"nodes_removed": nodesRemoved,
		"edges_removed": edgesRemoved,
	})
}

// handleSetActiveProject validates the project name, updates the active project,
// persists to GlobalConfig, and re-scopes queries.
func (s *Server) handleSetActiveProject(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, err := req.RequireString("project")
	if err != nil {
		return mcp.NewToolResultError("project is required"), nil
	}

	if s.configManager == nil {
		return mcp.NewToolResultError("configuration manager is not available"), nil
	}

	gc := s.configManager.Global()

	// Validate project exists.
	repos, resolveErr := gc.ResolveRepos(project)
	if resolveErr != nil {
		// Build list of available projects for the error message.
		available := make([]string, 0, len(gc.Projects))
		for name := range gc.Projects {
			available = append(available, name)
		}
		return mcp.NewToolResultError(fmt.Sprintf(
			"project not found: %s (available: %s)", project, strings.Join(available, ", "),
		)), nil
	}

	// Update active project in config and on server.
	gc.ActiveProject = project
	s.activeProject = project

	// Persist to disk.
	if saveErr := gc.Save(); saveErr != nil {
		s.logger.Warn("failed to persist active project change",
			zap.String("project", project), zap.Error(saveErr))
	}

	return mcp.NewToolResultJSON(map[string]any{
		"status":  "active",
		"project": project,
		"repos":   buildRepoList(repos),
	})
}

// handleGetActiveProject returns the current active project name and its repo list.
func (s *Server) handleGetActiveProject(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.configManager == nil {
		return mcp.NewToolResultJSON(map[string]any{
			"project": "",
			"repos":   []any{},
		})
	}

	gc := s.configManager.Global()
	project := s.activeProject
	if project == "" {
		project = gc.ActiveProject
	}

	result := map[string]any{
		"project": project,
	}

	if project == "" {
		// No active project — return top-level repos.
		result["repos"] = buildRepoList(gc.Repos)
	} else {
		repos, resolveErr := gc.ResolveRepos(project)
		if resolveErr != nil {
			result["repos"] = []any{}
			result["error"] = resolveErr.Error()
		} else {
			result["repos"] = buildRepoList(repos)
		}
	}

	return mcp.NewToolResultJSON(result)
}

// resolveRepoPrefix resolves a path-or-prefix string to a repo prefix.
// It first checks if the input matches a known repo prefix directly,
// then tries to match it as a file path against tracked repo root paths.
func (s *Server) resolveRepoPrefix(pathOrPrefix string) string {
	if s.multiIndexer == nil {
		return ""
	}

	// Check if it's a known prefix directly.
	if meta := s.multiIndexer.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix
	}

	// Try to match as a path — check all tracked repos.
	for prefix, meta := range s.multiIndexer.AllMetadata() {
		if meta.RootPath == pathOrPrefix {
			return prefix
		}
	}

	return ""
}

// buildRepoList converts a slice of RepoEntry to a JSON-friendly list.
func buildRepoList(repos []config.RepoEntry) []map[string]string {
	list := make([]map[string]string, 0, len(repos))
	for _, r := range repos {
		entry := map[string]string{
			"path":   r.Path,
			"prefix": config.ResolvePrefix(r),
		}
		if r.Ref != "" {
			entry["ref"] = r.Ref
		}
		list = append(list, entry)
	}
	return list
}
