package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
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
// Subscription is opt-in per session via `subscribe_diagnostics`. Each
// subscriber carries a small filter set (minimum severity, path
// prefix) so different sessions can subscribe to disjoint slices of
// the workspace's diagnostic stream. Notifications are delivered
// per-session via `SendNotificationToSpecificClient` — sessions that
// did not subscribe never see any payload.
//
// Delta-only: each (file, content-hash) pair fires the broadcast at
// most once. Repeated identical publishes (which some servers emit on
// every save even when nothing changed) are suppressed at the
// broadcaster.
//
// Initial-state replay: a fresh subscribe call walks the snapshot of
// every alive provider and sends the current diagnostic state for
// every file matching the subscriber's filter, tagged
// `initial_replay: true`. After the replay the subscriber receives
// only deltas.
type diagnosticsBroadcaster struct {
	server specificNotificationSender
	logger *zap.Logger

	// snapFn returns the current diagnostic state across every alive
	// provider. Called at subscribe time for initial-state replay.
	// Nil disables replay (used by tests that don't wire the router).
	snapFn func() []lsp.DiagnosticsEntry

	mu          sync.RWMutex
	subscribers map[string]*diagnosticsSubscriber // session ID → filter
	lastHash    map[string]string                 // absPath → sha256(diagnostics) for delta filter
}

// diagnosticsSubscriber records a session's filter knobs.
type diagnosticsSubscriber struct {
	sessionID   string
	minSeverity int    // LSP severities: 1=error, 2=warning, 3=info, 4=hint. 0 = no filter.
	pathPrefix  string // absolute-path prefix; empty = all files.
}

// subscribeOptions are the per-subscribe filter inputs surfaced by
// `subscribe_diagnostics`.
type subscribeOptions struct {
	MinSeverity int
	PathPrefix  string
}

// specificNotificationSender is the slice of *server.MCPServer the
// broadcaster needs to deliver per-session push notifications.
// Defined here so tests can inject a recorder without spinning up a
// full MCP server. Distinct from progress.go's `notificationSender`
// (which routes SendNotificationToClient via context-extracted
// session) — diagnostics need to address arbitrary subscribed
// sessions rather than the in-flight tool-request session.
type specificNotificationSender interface {
	SendNotificationToSpecificClient(sessionID string, method string, params map[string]any) error
}

func newDiagnosticsBroadcaster(srv specificNotificationSender, snapFn func() []lsp.DiagnosticsEntry, logger *zap.Logger) *diagnosticsBroadcaster {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &diagnosticsBroadcaster{
		server:      srv,
		logger:      logger,
		snapFn:      snapFn,
		subscribers: make(map[string]*diagnosticsSubscriber),
		lastHash:    make(map[string]string),
	}
}

// publish is the router-level hook target. Builds the MCP notification
// payload, applies the delta filter, and delivers per-subscriber via
// `SendNotificationToSpecificClient`. Non-blocking: each delivery
// returns immediately, the SDK drops to its error hook when a
// session's notification channel is full.
func (b *diagnosticsBroadcaster) publish(specName, absPath string, diags []lsp.Diagnostic) {
	if b == nil || b.server == nil {
		return
	}

	hash := hashDiagnostics(diags)

	b.mu.Lock()
	if b.lastHash[absPath] == hash {
		b.mu.Unlock()
		return
	}
	b.lastHash[absPath] = hash
	subs := make([]*diagnosticsSubscriber, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	if len(subs) == 0 {
		// Hash already updated above — late subscribers won't see a
		// stale replay of this payload during their initial snapshot.
		return
	}

	for _, sub := range subs {
		if !pathMatchesScope(absPath, sub) {
			continue
		}
		filtered := filterDiagnosticsBySeverity(diags, sub.minSeverity)
		params := map[string]any{
			"uri":         pathToFileURI(absPath),
			"path":        absPath,
			"server":      specName,
			"diagnostics": diagsToWire(filtered),
		}
		if err := b.server.SendNotificationToSpecificClient(sub.sessionID, "notifications/diagnostics", params); err != nil {
			// Common cases: session disconnected mid-flight, or its
			// notification channel is full. Both are recoverable —
			// the MCP SDK has its own error-hook for the latter.
			b.logger.Debug("send diagnostics notification failed",
				zap.String("session", sub.sessionID),
				zap.String("path", absPath),
				zap.Error(err),
			)
		}
	}
}

// subscribe records sessionID with the given filter knobs and
// immediately replays the current diagnostic state matching the
// subscriber's scope. Returns the number of replayed file payloads.
//
// Idempotent in the sense that resubscribing with new options
// overwrites the previous filter; the replay re-fires either way so
// the new filter produces a fresh view of the current state.
func (b *diagnosticsBroadcaster) subscribe(sessionID string, opts subscribeOptions) int {
	if sessionID == "" {
		return 0
	}
	sub := &diagnosticsSubscriber{
		sessionID:   sessionID,
		minSeverity: opts.MinSeverity,
		pathPrefix:  opts.PathPrefix,
	}
	b.mu.Lock()
	b.subscribers[sessionID] = sub
	snapFn := b.snapFn
	b.mu.Unlock()

	if snapFn == nil {
		return 0
	}
	snap := snapFn()
	sent := 0
	for _, e := range snap {
		if !pathMatchesScope(e.AbsPath, sub) {
			continue
		}
		filtered := filterDiagnosticsBySeverity(e.Diagnostics, sub.minSeverity)
		params := map[string]any{
			"uri":            pathToFileURI(e.AbsPath),
			"path":           e.AbsPath,
			"server":         e.SpecName,
			"diagnostics":    diagsToWire(filtered),
			"initial_replay": true,
		}
		if err := b.server.SendNotificationToSpecificClient(sub.sessionID, "notifications/diagnostics", params); err != nil {
			b.logger.Debug("send initial-replay notification failed",
				zap.String("session", sub.sessionID),
				zap.String("path", e.AbsPath),
				zap.Error(err),
			)
			continue
		}
		sent++
	}
	return sent
}

// unsubscribe removes sessionID from the opt-in set. Idempotent. Safe
// to call from session-lifecycle hooks even when the session never
// subscribed.
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

// pathMatchesScope reports whether absPath falls inside the
// subscriber's configured path prefix. Empty prefix matches everything.
func pathMatchesScope(absPath string, sub *diagnosticsSubscriber) bool {
	if sub == nil || sub.pathPrefix == "" {
		return true
	}
	return strings.HasPrefix(absPath, sub.pathPrefix)
}

// filterDiagnosticsBySeverity drops every entry with severity numerically
// greater than minSeverity (LSP convention: lower number = higher
// severity, 1=error, 4=hint). minSeverity ≤ 0 disables the filter.
//
// Returns a fresh slice — never aliases the input.
func filterDiagnosticsBySeverity(diags []lsp.Diagnostic, minSeverity int) []lsp.Diagnostic {
	if minSeverity <= 0 {
		out := make([]lsp.Diagnostic, len(diags))
		copy(out, diags)
		return out
	}
	out := make([]lsp.Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Severity == 0 || d.Severity <= minSeverity {
			out = append(out, d)
		}
	}
	return out
}

// hashDiagnostics derives a stable fingerprint for a diagnostic list
// so the delta filter can suppress identical re-publishes.
func hashDiagnostics(diags []lsp.Diagnostic) string {
	if len(diags) == 0 {
		return "empty"
	}
	b, err := json.Marshal(diags)
	if err != nil {
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
	if absPath[0] == '/' {
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
			mcp.WithDescription("Opt the current MCP session into `notifications/diagnostics` push events. Once subscribed, every LSP `textDocument/publishDiagnostics` matching your filter is forwarded to your session as an MCP notification with `{uri, path, server, diagnostics}` payload. The current diagnostic state of every matching file is replayed immediately as `initial_replay: true` so you don't have to wait for the next edit. Optional `min_severity` (1=error, 2=warning, 3=info, 4=hint; default 0=all) and `path_prefix` (absolute path prefix; default empty=all files) restrict which payloads reach this session. Resubscribing overwrites the previous filter and re-replays the current state. Pair with `unsubscribe_diagnostics` to opt back out."),
			mcp.WithNumber("min_severity", mcp.Description("Drop diagnostics whose LSP severity number exceeds this value. 1=error, 2=warning, 3=info, 4=hint. 0 (default) keeps everything.")),
			mcp.WithString("path_prefix", mcp.Description("Absolute-path prefix; only diagnostics for files under this prefix are forwarded. Empty (default) keeps everything.")),
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

func (s *Server) handleSubscribeDiagnostics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.diagBroadcaster == nil {
		return mcp.NewToolResultError("diagnostics broadcaster is not configured (no LSP router)"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		// Embedded mode — single client. Use a sentinel so the
		// broadcaster has at least one subscriber.
		id = "embedded"
	}
	opts := subscribeOptions{
		MinSeverity: int(req.GetFloat("min_severity", 0)),
		PathPrefix:  req.GetString("path_prefix", ""),
	}
	replayed := s.diagBroadcaster.subscribe(id, opts)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":   true,
		"session_id":   id,
		"subscribers":  s.diagBroadcaster.subscriberCount(),
		"min_severity": opts.MinSeverity,
		"path_prefix":  opts.PathPrefix,
		"replayed":     replayed,
	})
}

func (s *Server) handleUnsubscribeDiagnostics(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.diagBroadcaster == nil {
		return mcp.NewToolResultError("diagnostics broadcaster is not configured"), nil
	}
	id := SessionIDFromContext(ctx)
	if id == "" {
		id = "embedded"
	}
	s.diagBroadcaster.unsubscribe(id)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"subscribed":  false,
		"session_id":  id,
		"subscribers": s.diagBroadcaster.subscriberCount(),
	})
}

// SetLSPDiagnosticsBroadcasting attaches a forwarding hook to the
// configured LSP router so every router-managed publishDiagnostics
// turns into a `notifications/diagnostics` delivery (subject to the
// per-session opt-in set + filters). Safe to call before or after
// tools are registered.
//
// No-ops cleanly when no LSP router is wired.
func (s *Server) SetLSPDiagnosticsBroadcasting() {
	r := s.lspRouter()
	if r == nil {
		return
	}
	if s.diagBroadcaster == nil {
		s.diagBroadcaster = newDiagnosticsBroadcaster(s.mcpServer, r.DiagnosticsSnapshot, s.logger)
	}
	r.SetDiagnosticsHook(func(specName, absPath string, diags []lsp.Diagnostic) {
		s.diagBroadcaster.publish(specName, absPath, diags)
	})
}
