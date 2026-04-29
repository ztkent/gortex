package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// mcpDispatcher routes MCP JSON-RPC frames from daemon sessions to the
// shared *gortexmcp.Server. Every frame returns through
// MCPServer.HandleMessage, which is the public entry point the
// mark3labs/mcp-go library exposes for non-stdio embeddings.
//
// Session isolation is handled by threading the daemon-assigned session
// ID into ctx via gortexmcp.WithSessionID before HandleMessage runs.
// Tool handlers resolve per-client state through Server.sessionFor(ctx).
type mcpDispatcher struct {
	srv          *gortexmcp.Server
	multiIndexer *indexer.MultiIndexer
	logger       *zap.Logger
	router       *daemon.Router
}

func newMCPDispatcher(srv *gortexmcp.Server, mi *indexer.MultiIndexer, logger *zap.Logger) *mcpDispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &mcpDispatcher{srv: srv, multiIndexer: mi, logger: logger}
}

// SetRouter wires the hybrid-read router into the daemon's MCP
// dispatch. With a router set, tools/call frames carrying
// a `workspace` arg or a session cwd that resolves to a non-local
// server are proxied; all other frames flow through the local
// MCPServer.HandleMessage path unchanged.
func (d *mcpDispatcher) SetRouter(r *daemon.Router) { d.router = r }

// Dispatch implements daemon.MCPDispatcher. It hands the raw JSON-RPC
// frame to MCPServer.HandleMessage and returns the response bytes.
// Empty return value means the client sent a notification (no response).
//
// The session ID from the daemon connection is attached to ctx via
// gortexmcp.WithSessionID so tool handlers reach per-session state
// through sessionFor(ctx) rather than the shared default. This is what
// keeps client A's recent-activity separate from client B's.
func (d *mcpDispatcher) Dispatch(ctx context.Context, sess *daemon.Session, frame []byte) ([]byte, error) {
	if d.srv == nil || d.srv.MCPServer() == nil {
		return nil, fmt.Errorf("mcp dispatcher: no server attached")
	}

	// Fast-path reject untracked cwds. Returns a structured JSON-RPC
	// error the agent can surface in chat ("run `gortex track .`")
	// rather than a silent wrong-result. Skipped when the session has
	// no cwd (the CLI and test harnesses don't set one), so control-
	// flow paths keep working unchanged. With a multi-server router
	// wired, a cwd that resolves to a remote workspace via the roster
	// also counts as reachable — otherwise the cwd-walk priority
	// chain in RouteForCwd would be dead code from the dispatcher's
	// perspective.
	if sess.CWD != "" && !d.cwdReachable(sess.CWD) {
		return d.notTrackedError(sess, frame), nil
	}

	ctx = gortexmcp.WithSessionID(ctx, sess.ID)

	// Identify the MCP client. The handshake's ClientName is the
	// proxy's env-var-based guess (often "unknown" when no known env
	// var matched). The MCP `initialize` request carries the
	// authoritative `clientInfo.name` and `clientInfo.version`, so we
	// snoop it here and overwrite the session metadata. Subsequent
	// status calls then show "claude-code 1.0.42" instead of
	// "unknown".
	d.maybeSnoopInitialize(sess, frame)

	// For tools/call frames carrying a workspace scope or a cwd that
	// routes elsewhere, the daemon
	// proxies to the right server instead of running locally. Other
	// frames (initialize, tools/list, notifications) flow through
	// the local MCPServer below; routing them across a federation
	// would change semantics that are intentionally machine-local.
	if d.router != nil {
		if proxied, ok := d.tryProxyToolCall(ctx, sess, frame); ok {
			return proxied, nil
		}
	}

	// HandleMessage returns either a JSONRPCResponse, a JSONRPCError, or
	// nil (the message was a notification). It never panics on malformed
	// JSON — it returns a JSON-RPC parse-error frame instead.
	reply := d.srv.MCPServer().HandleMessage(ctx, json.RawMessage(frame))
	if reply == nil {
		return nil, nil
	}

	out, err := json.Marshal(reply)
	if err != nil {
		d.logger.Warn("dispatch: marshal reply failed",
			zap.String("session_id", sess.ID), zap.Error(err))
		return nil, fmt.Errorf("marshal reply: %w", err)
	}
	return out, nil
}

// SessionEnded implements daemon.SessionEndedHook. When a proxy
// disconnects, drop its entry from the MCP server's session map so idle
// per-session state doesn't accumulate for the daemon's lifetime.
func (d *mcpDispatcher) SessionEnded(sess *daemon.Session) {
	if d.srv != nil && sess != nil {
		d.srv.ReleaseSession(sess.ID)
	}
}

// cwdReachable reports whether a session cwd has any chance of
// being served by this daemon — locally or remotely. The local arm
// is isCWDTracked. The remote arm consults the router so a cwd
// whose .gortex.yaml::workspace is hosted by a server in
// servers.toml is treated as reachable; the call will be proxied by
// tryProxyToolCall later in Dispatch. Without this, the cwd-walk
// priority chain in RouteForCwd would never trigger from the MCP
// path because the cwd-tracked guard rejects first.
//
// Reachable when:
//   - cwd is empty (no opinion — control-style sessions),
//   - cwd is inside a locally tracked repo,
//   - or the router resolves cwd to a known workspace via an
//     explicit signal: scope-override, .gortex.yaml::workspace, or a
//     server's declared workspaces / cached roster.
//
// The "default-server fall-through" case is intentionally NOT
// treated as reachable: a cwd that nobody claims would otherwise
// silently route to whatever server happens to be marked default
// in servers.toml, which is the same wrong-result class
// repo_not_tracked is meant to prevent.
func (d *mcpDispatcher) cwdReachable(cwd string) bool {
	if cwd == "" {
		return true
	}
	if d.isCWDTracked(cwd) {
		return true
	}
	if d.router == nil {
		return false
	}
	lookup := d.router.LookupForCwd(cwd, "")
	switch lookup.Source {
	case "scope-override", "config-yaml", "roster":
		return true
	}
	return false
}

// isCWDTracked reports whether the proxy's cwd lies inside any tracked
// repo. Equal paths or any subdirectory of a tracked root qualify —
// e.g. a proxy in ~/projects/myapp/internal counts as tracked when
// ~/projects/myapp is in the tracked set.
//
// Returns true when the daemon has no multi-indexer (single-repo mode,
// anything-goes) so we don't accidentally reject valid embedded-style
// sessions during the rollout.
func (d *mcpDispatcher) isCWDTracked(cwd string) bool {
	if d.multiIndexer == nil {
		return true
	}
	cwd = filepath.Clean(cwd)
	for _, meta := range d.multiIndexer.AllMetadata() {
		root := filepath.Clean(meta.RootPath)
		if cwd == root {
			return true
		}
		if strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// tryProxyToolCall inspects a JSON-RPC frame and, if it's a
// tools/call that the router resolves to a remote server, proxies it
// and returns the wrapped JSON-RPC response. Returns ok=false when
// the frame is not a tools/call, the router returns ErrRouteUnresolved
// (local-fast path), or the proxy itself errors (we let the local
// path handle it as a fallback so transient network blips don't
// break the user's session).
func (d *mcpDispatcher) tryProxyToolCall(ctx context.Context, sess *daemon.Session, frame []byte) ([]byte, bool) {
	var peek struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &peek); err != nil {
		return nil, false
	}
	if peek.Method != "tools/call" || peek.Params.Name == "" {
		return nil, false
	}
	scope, _ := peek.Params.Arguments["workspace"].(string)
	body, err := json.Marshal(map[string]any{"arguments": peek.Params.Arguments})
	if err != nil {
		return nil, false
	}
	out, status, rerr := d.router.RouteToolCall(ctx, peek.Params.Name, body, daemon.RouteContext{
		Cwd:           sess.CWD,
		ScopeOverride: scope,
	})
	if rerr != nil || status == 0 {
		// ErrRouteUnresolved or some other failure — let the local
		// HandleMessage path take over (the same body works there).
		return nil, false
	}
	if status >= 400 {
		// Surface the upstream error as a JSON-RPC error so the
		// client sees a structured failure instead of a 4xx that
		// gets swallowed.
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      peek.ID,
			"error": map[string]any{
				"code":    -32000,
				"message": fmt.Sprintf("proxy %s/%s: status %d", "remote", peek.Params.Name, status),
				"data": map[string]any{
					"upstream_status": status,
					"upstream_body":   string(out),
				},
			},
		}
		buf, _ := json.Marshal(resp)
		return buf, true
	}
	// Success — wrap the proxied bytes as a JSON-RPC result.
	var result any
	if err := json.Unmarshal(out, &result); err != nil {
		// Non-JSON upstream — surface as text content for visibility.
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(out)}},
		}
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      peek.ID,
		"result":  result,
	}
	buf, _ := json.Marshal(resp)
	return buf, true
}

// maybeSnoopInitialize parses one inbound JSON-RPC frame and, if
// it's an MCP `initialize` request carrying `params.clientInfo`,
// updates the session's ClientName/ClientVersion. Non-initialize
// frames are ignored. Errors swallowed — this is best-effort
// metadata enrichment, not a correctness path.
func (d *mcpDispatcher) maybeSnoopInitialize(sess *daemon.Session, frame []byte) {
	if sess == nil || len(frame) == 0 {
		return
	}
	var peek struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &peek); err != nil {
		return
	}
	if peek.Method != "initialize" {
		return
	}
	if peek.Params.ClientInfo.Name == "" && peek.Params.ClientInfo.Version == "" {
		return
	}
	sess.SetClientInfo(peek.Params.ClientInfo.Name, peek.Params.ClientInfo.Version)
	d.logger.Info("daemon: identified MCP client",
		zap.String("session_id", sess.ID),
		zap.String("client", peek.Params.ClientInfo.Name),
		zap.String("version", peek.Params.ClientInfo.Version))
}

// notTrackedError builds a JSON-RPC error frame the agent surfaces to
// the user. The id is echoed from the inbound frame when it's a request;
// a zero id for notifications is fine (clients ignore responses to
// notifications anyway).
//
// Kept structured — error.code uses the MCP-convention -32000 range for
// server-defined errors; error.data carries machine-readable fields
// (error_code, path, suggestion) so a tool UI can offer a one-click
// "track this repo" button without regex-parsing the message string.
func (d *mcpDispatcher) notTrackedError(sess *daemon.Session, inbound []byte) []byte {
	// Pull the request id out of the inbound frame so the response
	// pairs correctly. If parsing fails (malformed frame), send a
	// null id — JSON-RPC clients treat that as "error with no
	// matching request" which is still more informative than
	// silence.
	var peek struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(inbound, &peek)

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      peek.ID,
		"error": map[string]any{
			"code":    -32000,
			"message": fmt.Sprintf("repository not tracked: %s", sess.CWD),
			"data": map[string]any{
				"error_code": "repo_not_tracked",
				"path":       sess.CWD,
				"suggestion": fmt.Sprintf("Run `gortex track %s` to include this repo in the shared graph.", sess.CWD),
			},
		},
	}
	out, _ := json.Marshal(resp)
	return out
}
