package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic/lsp"
)

// diagnosticsBroadcaster forwards `textDocument/publishDiagnostics`
// payloads from every router-managed LSP provider to MCP clients as
// `notifications/diagnostics`. Subscribed clients receive push events
// instead of polling `get_diagnostics`.
//
// Subscription model: opt-in per session via `subscribe_diagnostics`.
// Sessions that never subscribe receive nothing — most agents only
// want diagnostics for files they're actively editing, so silent
// broadcast would be noise. A subscribed session receives every
// publishDiagnostics across the workspace; per-file scoping can come
// later as a follow-on.
//
// Delta-only: each (uri, content-hash) pair is broadcast at most once.
// Repeated identical publishes (which some servers emit on every save)
// are suppressed at the broadcaster level so MCP clients don't see
// redundant traffic.
type diagnosticsBroadcaster struct {
	server notificationBroadcaster
	logger *zap.Logger

	mu          sync.RWMutex
	subscribers map[string]struct{} // session ID set
	lastHash    map[string]string   // absPath → sha256(diagnostics) for delta filter
}

// notificationBroadcaster is the slice of *server.MCPServer the
// broadcaster needs. Defined here so tests can inject a recorder
// without spinning up a full MCP server.
type notificationBroadcaster interface {
	SendNotificationToAllClients(method string, params map[string]any)
}

func newDiagnosticsBroadcaster(srv notificationBroadcaster, logger *zap.Logger) *diagnosticsBroadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &diagnosticsBroadcaster{
		server:      srv,
		logger:      logger,
		subscribers: make(map[string]struct{}),
		lastHash:    make(map[string]string),
	}
}

// publish is the router-level hook target. Builds the MCP notification
// payload, applies the delta filter, and broadcasts to every connected
// client that's opted in. Non-blocking — `SendNotificationToAllClients`
// drops to an error hook when a session's notification channel is full.
func (b *diagnosticsBroadcaster) publish(specName, absPath string, diags []lsp.Diagnostic) {
	if b == nil || b.server == nil {
		return
	}

	hash := hashDiagnostics(diags)

	b.mu.Lock()
	prev := b.lastHash[absPath]
	if prev == hash {
		b.mu.Unlock()
		return // delta filter — identical payload, skip broadcast.
	}
	b.lastHash[absPath] = hash
	hasSubscribers := len(b.subscribers) > 0
	b.mu.Unlock()

	if !hasSubscribers {
		// Track the hash anyway so a late-subscribing session
		// doesn't get an immediate replay of the last burst.
		return
	}

	params := map[string]any{
		"uri":         pathToFileURI(absPath),
		"path":        absPath,
		"server":      specName,
		"diagnostics": diagsToWire(diags),
	}
	b.server.SendNotificationToAllClients("notifications/diagnostics", params)
}

// subscribe records sessionID as opted-in to diagnostics broadcasts.
// Idempotent.
func (b *diagnosticsBroadcaster) subscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	b.subscribers[sessionID] = struct{}{}
	b.mu.Unlock()
}

// unsubscribe removes sessionID from the opt-in set. Idempotent.
func (b *diagnosticsBroadcaster) unsubscribe(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.subscribers, sessionID)
	b.mu.Unlock()
}

// subscriberCount returns the number of opted-in sessions. Used by
// status / debug surfaces.
func (b *diagnosticsBroadcaster) subscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// hashDiagnostics derives a stable fingerprint for a diagnostic list
// so the delta filter can suppress identical re-publishes.
func hashDiagnostics(diags []lsp.Diagnostic) string {
	if len(diags) == 0 {
		return "empty"
	}
	b, err := json.Marshal(diags)
	if err != nil {
		// json.Marshal of a struct slice should not fail in practice
		// — fall back to a per-call random-ish fingerprint to force a
		// re-broadcast rather than silently masking.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pathToFileURI converts an absolute filesystem path to a file:// URI.
// Centralised so push payloads match the URI shape MCP clients expect.
func pathToFileURI(absPath string) string {
	if absPath == "" {
		return ""
	}
	if len(absPath) > 0 && absPath[0] == '/' {
		return "file://" + absPath
	}
	return "file:///" + absPath
}

// registerDiagnosticsTools wires the subscribe/unsubscribe MCP tools.
// `subscribe_diagnostics` opts the calling session into push
// notifications; `unsubscribe_diagnostics` opts it back out.
func (s *Server) registerDiagnosticsTools() {
	s.mcpServer.AddTool(
		mcp.NewTool("subscribe_diagnostics",
			mcp.WithDescription("Opt the current MCP session into `notifications/diagnostics` push events. Once subscribed, every LSP `textDocument/publishDiagnostics` for any router-managed server is forwarded to your session as an MCP notification with `{uri, path, server, diagnostics}` payload. Idempotent — calling twice is safe. Use this instead of polling `get_diagnostics` when you need real-time edit-time feedback. Pair with `unsubscribe_diagnostics` to opt back out."),
		),
		s.handleSubscribeDiagnostics,
	)
	s.mcpServer.AddTool(
		mcp.NewTool("unsubscribe_diagnostics",
			mcp.WithDescription("Opt the current MCP session out of `notifications/diagnostics` push events. Idempotent."),
		),
		s.handleUnsubscribeDiagnostics,
	)
}

func (s *Server) handleSubscribeDiagnostics(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.diagBroadcaster == nil {
		return mcp.NewToolResultError("diagnostics broadcaster is not configured (no LSP router)"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		// Embedded mode — single client. Use a sentinel so the
		// broadcaster has at least one subscriber.
		id = "embedded"
	}
	s.diagBroadcaster.subscribe(id)
	return mcp.NewToolResultJSON(map[string]any{
		"subscribed":  true,
		"session_id":  id,
		"subscribers": s.diagBroadcaster.subscriberCount(),
	})
}

func (s *Server) handleUnsubscribeDiagnostics(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.diagBroadcaster == nil {
		return mcp.NewToolResultError("diagnostics broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	s.diagBroadcaster.unsubscribe(id)
	return mcp.NewToolResultJSON(map[string]any{
		"subscribed":  false,
		"session_id":  id,
		"subscribers": s.diagBroadcaster.subscriberCount(),
	})
}

// SetLSPDiagnosticsBroadcasting attaches a forwarding hook to the
// configured LSP router so every router-managed publishDiagnostics
// turns into a `notifications/diagnostics` broadcast (subject to the
// per-session opt-in set). Safe to call before or after tools are
// registered.
//
// No-ops cleanly when no LSP router is wired.
func (s *Server) SetLSPDiagnosticsBroadcasting() {
	r := s.lspRouter()
	if r == nil {
		return
	}
	if s.diagBroadcaster == nil {
		s.diagBroadcaster = newDiagnosticsBroadcaster(s.mcpServer, s.logger)
	}
	r.SetDiagnosticsHook(func(specName, absPath string, diags []lsp.Diagnostic) {
		s.diagBroadcaster.publish(specName, absPath, diags)
	})
}
