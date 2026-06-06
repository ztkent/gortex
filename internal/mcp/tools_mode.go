package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// Planning mode and the federation write-gate share one canonical
// write-tool set — daemon.MutatingTools — so a tool is hidden/blocked
// in a read-only session by exactly the same list that refuses to route
// it to a remote. See internal/daemon/mutating.go.

// sessionPlanningMode reports whether the request's session is in
// planning mode (no writes permitted).
func (s *Server) sessionPlanningMode(ctx context.Context) bool {
	sess := s.sessionFor(ctx)
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.planningMode
}

// toolSurfaceFilter is the per-session tools/list filter wired into the
// MCP server. In planning mode it drops every editing tool so the agent
// never sees a tool it is not allowed to call.
// editingToolsHidden reports whether editing tools must be removed from
// this session's tool surface — either planning mode or a block-mode
// workflow currently in a non-editing phase.
func (s *Server) editingToolsHidden(ctx context.Context) bool {
	return s.sessionPlanningMode(ctx) || s.workflowHidesEdits(ctx)
}

func (s *Server) toolSurfaceFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	if s.editingToolsHidden(ctx) {
		kept := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			if daemon.MutatingTools[t.Name] {
				continue
			}
			kept = append(kept, t)
		}
		tools = kept
	}
	// Per-host adaptation: drop tools the host duplicates and apply any
	// host-specific description overrides (see host_context.go).
	return s.sessionHostContext(ctx).apply(tools)
}

// checkToolGate returns a structured error result when toolName must not
// run given the session's runtime mode, or nil when the call may
// proceed. This is the hard guarantee behind planning mode: even a
// client that never re-read tools/list cannot slip an edit through.
func (s *Server) checkToolGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	if blocked := s.checkPlanningModeGate(ctx, toolName); blocked != nil {
		return blocked
	}
	if blocked := s.checkWorkflowGate(ctx, toolName); blocked != nil {
		return blocked
	}
	return nil
}

// checkPlanningModeGate blocks editing tools while the session is in
// planning mode.
func (s *Server) checkPlanningModeGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	if !daemon.IsMutating(toolName) {
		return nil
	}
	if !s.sessionPlanningMode(ctx) {
		return nil
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolBlockedByMode,
		Message: fmt.Sprintf("%q is an editing tool and this session is in planning mode — no writes are "+
			"permitted. Call set_planning_mode with mode \"editing\" to enable edits.", toolName),
		Retriable: true,
		Data:      map[string]any{"tool": toolName, "mode": "planning"},
	})
}

// registerPlanningModeTool registers set_planning_mode — the runtime
// switch between a guaranteed no-writes planning phase and normal editing.
func (s *Server) registerPlanningModeTool() {
	s.addTool(mcp.NewTool("set_planning_mode",
		mcp.WithDescription("Switch this session between \"planning\" mode — every editing tool "+
			"(edit_file, edit_symbol, write_file, rename_symbol) is removed from the tool surface and "+
			"hard-blocked, a guaranteed no-writes phase — and \"editing\" mode, where edits are enabled. "+
			"Use planning mode while exploring or drafting a change so no accidental writes happen."),
		mcp.WithString("mode", mcp.Required(),
			mcp.Description("\"planning\" (editing tools removed and blocked) or \"editing\" (edits enabled)")),
	), s.handleSetPlanningMode)
}

func (s *Server) handleSetPlanningMode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	raw, err := req.RequireString("mode")
	if err != nil {
		return mcp.NewToolResultError("mode is required (\"planning\" or \"editing\")"), nil
	}
	var planning bool
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "planning", "plan", "read-only", "readonly":
		planning = true
	case "editing", "edit", "write":
		planning = false
	default:
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   fmt.Sprintf("unknown mode %q — want \"planning\" or \"editing\"", raw),
		}), nil
	}

	sess := s.sessionFor(ctx)
	sess.mu.Lock()
	sess.planningMode = planning
	sess.mu.Unlock()

	mode := "editing"
	note := "Editing tools are enabled."
	if planning {
		mode = "planning"
		note = "Editing tools are removed from the tool surface and hard-blocked. " +
			"Re-read tools/list to refresh the surface."
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"mode":            mode,
		"editing_enabled": !planning,
		"editing_tools":   daemon.SortedMutatingTools(),
		"note":            note,
	})
}
