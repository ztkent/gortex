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
		// Enforce the session's runtime mode / workflow phase — a hard
		// gate even if the client never re-read tools/list.
		if blocked := s.checkToolGate(ctx, req.Params.Name); blocked != nil {
			return blocked, nil
		}
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
		// Warmup fast path: when the daemon is still warming up and
		// this is a graph-querying tool, the handler still runs (so
		// the caller gets a best-effort partial answer from the part
		// of the graph indexed so far) and the result is decorated
		// with a structured `warming` block — flag + real progress
		// percentage + phase + message. Graph-independent tools are
		// untouched; a ready daemon is a transparent pass-through.
		// See warmup_fastpath.go.
		env, warming := s.checkWarmupFastPath(req.Params.Name)
		res, hErr := h(ctx, req)
		if warming && hErr == nil {
			res = decorateResultWithWarming(res, env)
		}
		// Capture large successful responses into the session ring so
		// the post-filter tools can re-cut them without re-querying.
		if hErr == nil {
			s.captureResponse(ctx, req.Params.Name, res)
		}
		return res, hErr
	}
}

// errBaseSHADrift is the structured drift error returned by the
// disk-write edit tools (edit_file / edit_symbol / write_file) when
// the caller-supplied base_sha does not match the current on-disk
// blob SHA. The message mirrors daemon.ErrOverlayDrift so callers
// can pattern-match on a single substring across overlay-push and
// plain-write paths: "re-read and resubmit".
const errBaseSHADrift = "base_sha mismatch — re-read and resubmit"

// gitBlobSHA computes the git blob SHA-1 of the given content. The
// hash matches `git ls-files -s` / `git hash-object` output (i.e.
// sha1 of "blob <len>\0<content>"), so editors can pass the SHA they
// already have without any client-side reformatting. The returned
// string is lowercase hex. This is the canonical drift-anchor helper
// shared by overlay_push and the disk-write edit tools.
func gitBlobSHA(data []byte) string {
	h := sha1.New()
	// hash.Hash.Write never errors; fmt.Fprintf returns (n, err)
	// because it's the io.Writer interface, but the underlying
	// hash.Hash's Write contract forbids non-nil errors. Discard
	// both to keep the linter happy without inventing fake error
	// handling.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeExpectedSHA lowercases and trims a caller-supplied
// base_sha so comparisons are case- and whitespace-insensitive.
func normalizeExpectedSHA(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// overlaySHAMatches re-computes the git blob SHA of an on-disk file
// and compares it to the SHA the editor recorded at didOpen time.
// Returns false on any read error: the safer default is "drift" —
// the client re-reads and resubmits.
func overlaySHAMatches(absPath, expected string) bool {
	expected = normalizeExpectedSHA(expected)
	if expected == "" {
		return true
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	return gitBlobSHA(data) == expected
}

// _ keeps sync.Mutex referenced by the package even after future
// refactors strip a field — the import lints flagged a phantom
// dependency in the prior iteration; harmless guard.
var _ sync.Mutex
