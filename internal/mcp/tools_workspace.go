package mcp

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/workspace"
)

// These are unconditionally registered (single-project mode degrades
// to a one-member view) so an agent's first call into the server can
// discover what `repo` values are legal before issuing any
// scope: repo or scope: fan-out call.
func (s *Server) registerWorkspaceTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("list_repos",
			mcp.WithDescription(
				"Lists every project in the active workspace. Workspace-scope tool: do not pass `repo`. "+
					"In workspace mode returns the auto-discovered, non-excluded children. "+
					"In single-project mode returns the one bound project as a degenerate one-member workspace."),
		),
		s.handleListRepos,
	)

	s.mcpServer.AddTool(
		mcp.NewTool("workspace_info",
			mcp.WithDescription(
				"Returns workspace identity: bind mode, root directory, marker contents, the auto-discovered member set, and any unknown marker keys. "+
					"Workspace-scope tool: do not pass `repo`."),
		),
		s.handleWorkspaceInfo,
	)
}

// handleListRepos implements scope: workspace's `list_repos`. Returns
// the auto-discovered, non-excluded member set. Single-project mode
// degrades to the one-member [bound project] list.
//
// Pre-handshake (Bind() == nil): returns an empty list rather than
// erroring. The MCP server may be running in legacy single-repo mode
// where no two-entry-point handshake has happened — in that case the
// concept of a workspace doesn't apply and an empty list is the
// honest answer.
func (s *Server) handleListRepos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Enforce scope: workspace's "no `repo`" rule explicitly so a
	// caller passing `repo` gets a clear protocol error instead of
	// silent acceptance.
	if _, errResult := s.ResolveToolScope("list_repos", req.GetArguments()["repo"]); errResult != nil {
		return errResult, nil
	}

	bind := s.bind
	out := map[string]any{}
	if bind == nil {
		out["mode"] = "unbound"
		out["repos"] = []string{}
		return s.respondJSONOrTOON(ctx, req, out)
	}

	out["mode"] = bind.Mode.String()
	out["root"] = bind.Root
	out["repos"] = bind.MemberNames()
	return s.respondJSONOrTOON(ctx, req, out)
}

// handleWorkspaceInfo implements `workspace_info`. Returns enough
// detail for an agent to reason about the bind: mode, root, marker
// excludes, marker unknown keys, and the resolved member set with
// per-member paths.
func (s *Server) handleWorkspaceInfo(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if _, errResult := s.ResolveToolScope("workspace_info", req.GetArguments()["repo"]); errResult != nil {
		return errResult, nil
	}

	bind := s.bind
	if bind == nil {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"mode":  "unbound",
			"repos": []map[string]string{},
		})
	}

	members := make([]map[string]string, 0, len(bind.Members))
	for _, m := range bind.Members {
		members = append(members, map[string]string{
			"name": m.Name,
			"path": m.Path,
		})
	}

	excludes := append([]string(nil), bind.Marker.Exclude...)
	sort.Strings(excludes)

	unknownKeys := make([]string, 0, len(bind.Marker.Unknown))
	for k := range bind.Marker.Unknown {
		unknownKeys = append(unknownKeys, k)
	}
	sort.Strings(unknownKeys)

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"mode":             bind.Mode.String(),
		"root":             bind.Root,
		"marker":           workspace.MarkerFile,
		"excludes":         excludes,
		"unknown_keys":     unknownKeys,
		"members":          members,
		"isolation_bounds": bind.Root,
	})
}
