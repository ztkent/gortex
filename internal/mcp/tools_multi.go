package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// registerMultiRepoTools registers MCP tools for multi-repo management:
// track_repository, untrack_repository, set_active_project, get_active_project.
func (s *Server) registerMultiRepoTools() {
	s.addTool(
		mcp.NewTool("track_repository",
			mcp.WithDescription("Add a repository to the tracked workspace at runtime. Indexes immediately and persists to config."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path to repository")),
			mcp.WithString("name", mcp.Description("Optional repo prefix override")),
			mcp.WithBoolean("as_worktree", mcp.Description("Track a linked git worktree as an independent instance (derived `<base>@<workspace>` prefix) even when its repo is already tracked elsewhere. Auto-detected when the worktree's .gortex.yaml declares a different workspace; set this to force it.")),
		),
		s.handleTrackRepository,
	)

	s.addTool(
		mcp.NewTool("untrack_repository",
			mcp.WithDescription("Remove a repository from the tracked workspace at runtime. Evicts nodes/edges and persists to config."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Path or repo prefix to remove")),
		),
		s.handleUntrackRepository,
	)

	s.addTool(
		mcp.NewTool("set_active_project",
			mcp.WithDescription("Switch the active project scope. Persists to config and re-scopes all subsequent queries."),
			mcp.WithString("project", mcp.Required(), mcp.Description("Project name to activate")),
		),
		s.handleSetActiveProject,
	)

	s.addTool(
		mcp.NewTool("get_active_project",
			mcp.WithDescription("Return the current active project name and its list of member repositories."),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleGetActiveProject,
	)

	s.addTool(
		mcp.NewTool("query_project",
			mcp.WithDescription("Search symbols in another project or repository without switching the "+
				"active project. A read-only, one-shot cross-project lookup: it resolves the named "+
				"project (or a bare tracked-repo prefix), searches it, and returns — the active project "+
				"and the session scope are left unchanged. Use this instead of set_active_project for a "+
				"quick look into another project."),
			mcp.WithString("project", mcp.Required(),
				mcp.Description("Project name, per-repo project tag, or tracked-repo prefix to search")),
			mcp.WithString("query", mcp.Required(), mcp.Description("Symbol search query")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
		),
		s.handleQueryProject,
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
	if asWT, ok := req.GetArguments()["as_worktree"].(bool); ok {
		entry.AsWorktree = asWT
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

	prefix := result.RepoPrefix
	if prefix == "" {
		prefix = config.ResolvePrefix(entry)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":     "tracked",
		"path":       path,
		"prefix":     prefix,
		"file_count": result.FileCount,
		"node_count": result.NodeCount,
		"edge_count": result.EdgeCount,
	})
}

// handleUntrackRepository removes a repo from the workspace and persists to GlobalConfig.
func (s *Server) handleUntrackRepository(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":        "untracked",
		"prefix":        prefix,
		"nodes_removed": nodesRemoved,
		"edges_removed": edgesRemoved,
	})
}

// handleSetActiveProject validates the project name, updates the active project,
// persists to GlobalConfig, and re-scopes queries.
func (s *Server) handleSetActiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":  "active",
		"project": project,
		"repos":   buildRepoList(repos),
	})
}

// handleGetActiveProject returns the current active project name and its repo list.
func (s *Server) handleGetActiveProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.respondJSONOrTOON(ctx, req, s.buildActiveProjectPayload(ctx))
}

// buildActiveProjectPayload returns the same data the `get_active_project`
// tool emits. Shared with the `gortex://active-project` resource.
//
// For a workspace-bound session it reports the session's own resolved
// scope — the boundary the query tools actually enforce — rather than
// the process-global config default, which would mask whether scoping
// is in effect.
func (s *Server) buildActiveProjectPayload(ctx context.Context) map[string]any {
	if sessWS, sessProj, bound := s.sessionScope(ctx); bound {
		return map[string]any{
			"workspace": sessWS,
			"project":   sessProj,
			"bound":     true,
			"repos":     s.sessionWorkspaceRepos(ctx),
		}
	}

	if s.configManager == nil {
		return map[string]any{
			"project": "",
			"repos":   []any{},
		}
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
		result["repos"] = buildRepoList(gc.Repos)
		return result
	}

	repos, resolveErr := gc.ResolveRepos(project)
	if resolveErr != nil {
		// Common after the workspace bind drops to "unbound"
		// while a stale active_project still points at a project
		// the workspace no longer discovers. Fall back to the
		// workspace-level repo list and record the drift in `note`.
		result["project"] = ""
		result["repos"] = buildRepoList(gc.Repos)
		result["note"] = fmt.Sprintf("active_project %q not found in current workspace; returning top-level repos", project)
		return result
	}

	result["repos"] = buildRepoList(repos)
	return result
}

// resolveRepoPrefix resolves a path-or-prefix string to a repo prefix by
// consulting only the in-memory MultiIndexer state. Use
// resolveRepoPrefixOrReconcile when drift between persisted config and
// in-memory state could produce a false miss.
func (s *Server) resolveRepoPrefix(pathOrPrefix string) string {
	if s.multiIndexer == nil || pathOrPrefix == "" {
		return ""
	}

	// Check if it's a known prefix directly.
	if meta := s.multiIndexer.GetMetadata(pathOrPrefix); meta != nil {
		return pathOrPrefix
	}

	// Try to match as a path — check all tracked repos. Also try the
	// absolute form since users may pass either.
	absInput, _ := filepath.Abs(pathOrPrefix)
	for prefix, meta := range s.multiIndexer.AllMetadata() {
		if meta.RootPath == pathOrPrefix || (absInput != "" && meta.RootPath == absInput) {
			return prefix
		}
	}

	return ""
}

// diffJoinPrefix resolves the graph repo prefix used to join repo-relative
// diff / forge file paths to indexed nodes: multi-repo daemons key file
// paths as "<prefix>/<rel>" while git and forge APIs emit repo-relative
// paths. repoRoot is the already-resolved working-tree root. Returns "" in
// single-repo / unprefixed mode, where the raw lookup already matches.
func (s *Server) diffJoinPrefix(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	if p := s.resolveRepoPrefix(repoRoot); p != "" {
		return p
	}
	if s.indexer != nil && s.indexer.RootPath() == repoRoot {
		return s.indexer.RepoPrefix()
	}
	return ""
}

// diffRepoScope resolves the working-tree root and graph repo prefix a
// diff-driven handler operates on. An explicit selector (a repo prefix or a
// filesystem path — the CLI defaults to the caller's working directory) is
// normalized and honoured exclusively: when it names nothing tracked the
// result is empty so the caller errors instead of silently diffing another
// repo. With no selector the lone tracked repo wins, then the session's
// cwd-bound repo (clients dial the daemon with their working directory).
// Both empty means no resolvable working tree.
func (s *Server) diffRepoScope(ctx context.Context, repo string) (repoRoot, repoPrefix string) {
	if repo != "" {
		if p := s.resolveRepoPrefix(repo); p != "" {
			repo = p
		}
		root := pickRepoRoot(s.collectRepoRoots(repo), repo)
		if root == "" {
			return "", ""
		}
		return root, s.diffJoinPrefix(root)
	}
	if root := pickRepoRoot(s.collectRepoRoots(""), ""); root != "" {
		return root, s.diffJoinPrefix(root)
	}
	if cwd := SessionCWDFromContext(ctx); cwd != "" && s.multiIndexer != nil {
		if _, _, prefix, ok := s.multiIndexer.ScopeForCWD(cwd); ok && prefix != "" {
			if root, ok := s.multiIndexer.RepoRoot(prefix); ok && root != "" {
				return root, prefix
			}
		}
	}
	return "", ""
}

// resolveRepoPrefixOrReconcile resolves a path-or-prefix to a repo prefix
// and reconciles persisted-config state into the in-memory MultiIndexer on
// miss. Warmup can silently drop a repo (transient index failure, daemon
// restart with a stale snapshot, crash mid-warmup) and leave it listed
// under get_active_project but absent from mi.repos; the user's next
// operation then errors with "not a tracked repository" for something
// they can plainly see in the project list. Here, if the input matches a
// persisted config entry, we auto-track it before returning the prefix.
func (s *Server) resolveRepoPrefixOrReconcile(ctx context.Context, pathOrPrefix string) string {
	if prefix := s.resolveRepoPrefix(pathOrPrefix); prefix != "" {
		return prefix
	}
	if s.multiIndexer == nil || s.configManager == nil {
		return ""
	}

	absInput, _ := filepath.Abs(pathOrPrefix)
	for _, entry := range s.configManager.Global().Repos {
		entryAbs, _ := filepath.Abs(entry.Path)
		if entry.Path != pathOrPrefix && entryAbs != absInput &&
			config.ResolvePrefix(entry) != pathOrPrefix {
			continue
		}
		if _, err := s.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
			s.logger.Warn("auto-track from config failed",
				zap.String("path", entry.Path), zap.Error(err))
			return ""
		}
		return s.resolveRepoPrefix(pathOrPrefix)
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
