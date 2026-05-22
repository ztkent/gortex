package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zzet/gortex/internal/daemon"
)

// SetOverlayManager wires the editor-overlay manager into the MCP
// server. After this call:
//
//   - Every `tools/call` whose session has overlay buffers attached is
//     wrapped with a per-request middleware that constructs a shadow-
//     graph view (`*graph.OverlaidView`) layering the parsed overlay
//     on top of the immutable base graph. The view is attached to the
//     request context; tool handlers read it via `s.readerFor(ctx)`
//     instead of touching `s.graph` directly. The base graph is never
//     mutated, so concurrent sessions — overlay-active or not — see
//     their own consistent view and the file watcher never races on
//     overlay state.
//
//   - The overlay management MCP tools (`overlay_register`,
//     `overlay_push`, `overlay_list`, `overlay_delete`, `overlay_drop`)
//     become live so MCP-native editor extensions can manage overlays
//     without reaching for the parallel `/v1/overlay/*` HTTP surface.
//
// Passing nil leaves the server in pre-overlay behaviour (reads always
// come from the base graph; overlay tools are not registered). Calling
// twice re-registers the overlay tools idempotently.
func (s *Server) SetOverlayManager(mgr *daemon.OverlayManager) {
	s.overlays = mgr
	if mgr == nil {
		return
	}
	s.registerOverlayToolsOnce.Do(func() {
		s.registerOverlayTools()
	})
}

// OverlayManager returns the wired editor-overlay manager, or nil
// when overlay support is disabled for this server instance.
func (s *Server) OverlayManager() *daemon.OverlayManager { return s.overlays }

// wrapToolHandler returns a tool handler decorated with the
// overlay-view middleware. Tool registration helpers (`s.addTool`)
// route every handler through this so the daemon-dispatched path
// (HandleMessage) and the HTTP `CallToolStrict` path get identical
// shadow-graph semantics — the latter bypasses mcp-go's hook surface,
// so handler-level wrapping is the only place that covers both
// transports.
//
// The middleware is non-mutating: it parses the calling session's
// overlay buffers once per request (cached by (sessID, contentHash) in
// s.overlayLayerCache) and attaches the resulting view to ctx via
// WithOverlayView. Tool handlers obtain the active reader via
// s.readerFor(ctx), which returns the view when present and the base
// graph otherwise. Concurrent sessions are isolated by construction
// because no shared state is touched.
//
// When the calling session has no overlay or no overlay manager is
// wired, this is a transparent pass-through (one map lookup, zero
// parsing) — non-overlay traffic pays no cost.
func (s *Server) wrapToolHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	// Prompt-injection screening sits closest to the handler so it
	// sees the real arguments and the real result (see sanitize.go).
	h = s.sanitizeToolHandler(h)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Tolerate hallucinated / mistyped parameter names before the
		// handler reads arguments (e.g. "symbol" accepted as "id").
		s.reconcileToolParams(&req)
		view, err := s.buildOverlayViewForCtx(ctx)
		if err != nil {
			// Drift surfaces as a structured tool error result so the
			// client knows to re-read and resubmit. Return (result,
			// nil) so the JSON-RPC framing carries the message rather
			// than a transport error.
			return mcp.NewToolResultError(err.Error()), nil
		}
		if view != nil {
			ctx = WithOverlayView(ctx, view)
		}
		return h(ctx, req)
	}
}

// overlaySHAMatches re-computes the git blob SHA of an on-disk file
// and compares it to the SHA the editor recorded at didOpen time.
// Matches `git ls-files -s` / `git hash-object` output (i.e. blob
// header `blob <len>\0<content>` then sha1), so editors can pass the
// SHA they already have without any client-side reformatting.
// Returns false on any read error: the safer default is "drift" —
// the client re-reads and resubmits.
func overlaySHAMatches(absPath, expected string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return true
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	h := sha1.New()
	// hash.Hash.Write never errors; fmt.Fprintf returns (n, err)
	// because it's the io.Writer interface, but the underlying
	// hash.Hash's Write contract forbids non-nil errors. Discard
	// both to keep the linter happy without inventing fake error
	// handling.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)) == expected
}

// _ keeps sync.Mutex referenced by the package even after future
// refactors strip a field — the import lints flagged a phantom
// dependency in the prior iteration; harmless guard.
var _ sync.Mutex
