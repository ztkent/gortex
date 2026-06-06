package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/daemon"
)

// Workflow enforcement modes.
const (
	workflowModeBlock = "block" // out-of-phase calls are refused
	workflowModeWarn  = "warn"  // out-of-phase edits auto-advance the phase
)

// workflowPhase is one ordered stage of a workflow.
type workflowPhase struct {
	name       string
	allowsEdit bool
}

// workflowState is the per-session phase-enforcement state machine. It is
// nil on sessionState until a workflow is started; guarded by sessionState.mu.
type workflowState struct {
	phases  []workflowPhase
	current int
	mode    string
}

// defaultWorkflowPhases is the canonical explore → implement → verify
// ordering: editing tools are gated until the implement phase.
func defaultWorkflowPhases() []workflowPhase {
	return []workflowPhase{
		{name: "explore", allowsEdit: false},
		{name: "implement", allowsEdit: true},
		{name: "verify", allowsEdit: true},
	}
}

func (w *workflowState) phase() workflowPhase {
	if w.current < 0 || w.current >= len(w.phases) {
		return workflowPhase{name: "unknown"}
	}
	return w.phases[w.current]
}

func (w *workflowState) phaseNames() []string {
	out := make([]string, len(w.phases))
	for i, p := range w.phases {
		out[i] = p.name
	}
	return out
}

// advance moves to the next phase, returning false when already at the
// final phase.
func (w *workflowState) advance() bool {
	if w.current >= len(w.phases)-1 {
		return false
	}
	w.current++
	return true
}

// firstEditPhase returns the index of the earliest edit-capable phase, or
// the current index when no phase permits edits.
func (w *workflowState) firstEditPhase() int {
	for i, p := range w.phases {
		if p.allowsEdit {
			return i
		}
	}
	return w.current
}

// workflowHidesEdits reports whether a block-mode workflow currently in a
// non-editing phase should hide editing tools from this session.
func (s *Server) workflowHidesEdits(ctx context.Context) bool {
	sess := s.sessionFor(ctx)
	if sess == nil {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	wf := sess.workflow
	return wf != nil && wf.mode == workflowModeBlock && !wf.phase().allowsEdit
}

// checkWorkflowGate enforces phase ordering for editing tools. In block
// mode an out-of-phase edit is refused with a structured error; in warn
// mode the workflow auto-advances to the first editing phase and the call
// proceeds. Non-editing tools are never phase-gated.
func (s *Server) checkWorkflowGate(ctx context.Context, toolName string) *mcp.CallToolResult {
	if !daemon.IsMutating(toolName) {
		return nil
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	wf := sess.workflow
	if wf == nil || wf.phase().allowsEdit {
		return nil
	}
	if wf.mode == workflowModeWarn {
		wf.current = wf.firstEditPhase() // auto-advance
		return nil
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolOutOfPhase,
		Message: fmt.Sprintf("%q is an editing tool but the active workflow is in the %q phase, which "+
			"does not permit edits. Advance the workflow (workflow action=\"advance\") until it reaches "+
			"an editing phase.", toolName, wf.phase().name),
		Retriable: true,
		Data: map[string]any{
			"tool":          toolName,
			"current_phase": wf.phase().name,
			"phases":        wf.phaseNames(),
			"recovery":      "call the workflow tool with action=advance",
		},
	})
}

// registerWorkflowTool registers the workflow phase-enforcement tool.
func (s *Server) registerWorkflowTool() {
	s.addTool(mcp.NewTool("workflow",
		mcp.WithDescription("Drive a phase-enforcement workflow for this session. Phases run "+
			"explore → implement → verify, and editing tools are gated until the implement phase. "+
			"In \"block\" mode an out-of-phase edit is refused with a structured error and the phase "+
			"must be advanced explicitly; in \"warn\" mode an out-of-phase edit auto-advances the "+
			"phase. Actions: start, advance, status, stop."),
		mcp.WithString("action", mcp.Required(),
			mcp.Description("start | advance | status | stop")),
		mcp.WithString("mode",
			mcp.Description("for action=start: \"block\" (default) or \"warn\"")),
	), s.handleWorkflow)
}

func (s *Server) handleWorkflow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError("action is required (start | advance | status | stop)"), nil
	}
	act := strings.ToLower(strings.TrimSpace(action))
	sess := s.sessionFor(ctx)

	var (
		invalidAction bool
		noWorkflow    bool
	)
	sess.mu.Lock()
	switch act {
	case "start":
		mode := workflowModeBlock
		if strings.ToLower(strings.TrimSpace(req.GetString("mode", ""))) == workflowModeWarn {
			mode = workflowModeWarn
		}
		sess.workflow = &workflowState{phases: defaultWorkflowPhases(), mode: mode}
	case "advance":
		if sess.workflow == nil {
			noWorkflow = true
		} else {
			sess.workflow.advance()
		}
	case "stop":
		sess.workflow = nil
	case "status":
		// reported below
	default:
		invalidAction = true
	}
	// Snapshot under the lock; build the response after releasing it so
	// respondJSONOrTOON's session lookups never re-enter sess.mu.
	payload := map[string]any{"active": false}
	if wf := sess.workflow; wf != nil && !invalidAction {
		payload = map[string]any{
			"active":        true,
			"mode":          wf.mode,
			"current_phase": wf.phase().name,
			"phases":        wf.phaseNames(),
			"edits_allowed": wf.phase().allowsEdit,
		}
	}
	sess.mu.Unlock()

	if invalidAction {
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   fmt.Sprintf("unknown action %q — want start, advance, status, or stop", action),
		}), nil
	}
	if noWorkflow {
		return mcp.NewToolResultError("no workflow is active — call workflow with action=start first"), nil
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}
