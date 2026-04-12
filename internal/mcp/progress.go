package mcp

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/progress"
)

// notificationSender is the small slice of *server.MCPServer that
// mcpProgressReporter depends on. Defined here so tests can inject a fake
// without constructing a full MCP server.
type notificationSender interface {
	SendNotificationToClient(ctx context.Context, method string, params map[string]any) error
}

// mcpProgressReporter translates progress.Reporter.Report calls into MCP
// `notifications/progress` messages sent back to the originating client.
//
// The MCP spec requires the `progress` field to increase monotonically, so we
// ignore the per-stage `current` counter for that purpose and instead emit a
// strictly-increasing tick. The human-readable stage + counters go into
// `message`; `total` is omitted because the total work unit count changes
// across stages (files, symbols, batches) and reporting a moving total is more
// confusing than helpful.
type mcpProgressReporter struct {
	ctx    context.Context
	sender notificationSender
	token  mcp.ProgressToken
	tick   atomic.Int64
}

// newProgressReporter builds a reporter when the client supplied a
// progressToken. Returns nil (caller substitutes progress.Nop) when token is
// nil or the sender is nil.
func newProgressReporter(ctx context.Context, sender notificationSender, token mcp.ProgressToken) progress.Reporter {
	if sender == nil || token == nil {
		return nil
	}
	return &mcpProgressReporter{ctx: ctx, sender: sender, token: token}
}

func (r *mcpProgressReporter) Report(stage string, current, total int) {
	n := r.tick.Add(1)

	var msg string
	switch {
	case total > 0:
		msg = fmt.Sprintf("%s (%d/%d)", stage, current, total)
	case current > 0:
		msg = fmt.Sprintf("%s (%d)", stage, current)
	default:
		msg = stage
	}

	params := map[string]any{
		"progressToken": r.token,
		"progress":      float64(n),
		"message":       msg,
	}
	// Errors sending notifications are non-fatal — the operation must continue.
	_ = r.sender.SendNotificationToClient(r.ctx, "notifications/progress", params)
}

// progressCtx returns a context with a reporter attached when the request
// carries a _meta.progressToken. When no token is present or the server has
// no MCP backend, the returned context is ctx unchanged (FromContext will
// yield progress.Nop, so callers need no conditional logic).
func (s *Server) progressCtx(ctx context.Context, req mcp.CallToolRequest) context.Context {
	if req.Params.Meta == nil || req.Params.Meta.ProgressToken == nil {
		return ctx
	}
	r := newProgressReporter(ctx, s.mcpServer, req.Params.Meta.ProgressToken)
	if r == nil {
		return ctx
	}
	return progress.WithReporter(ctx, r)
}
