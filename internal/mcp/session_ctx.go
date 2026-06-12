package mcp

import (
	"context"
	"sync"

	"github.com/zzet/gortex/internal/savings"
)

// sessionCtxKey is the private context key under which a caller
// (typically the daemon's MCP dispatcher) stashes the session ID for
// the current request. The value is read by `Server.sessionFor` so
// tool handlers resolve to the correct per-client state.
//
// Unexported so external packages can't inject one accidentally — use
// WithSessionID / SessionIDFromContext.
type sessionCtxKey struct{}

// WithSessionID returns a context carrying id. The daemon's MCP
// dispatcher wraps each inbound frame's context with this before
// calling MCPServer.HandleMessage, giving every tool handler access
// to the per-session state without touching the handler signature.
//
// An empty id is treated as "no session" and returns ctx unchanged —
// that's the path the embedded stdio server takes, where there's only
// one implicit session.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, id)
}

// SessionIDFromContext returns the session ID attached via
// WithSessionID, or "" when none is present. Callers treat "" as
// "default shared session" — the same state the embedded server uses.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(sessionCtxKey{}).(string); ok {
		return id
	}
	return ""
}

// sessionCWDCtxKey carries the session's working directory. The
// daemon's MCP dispatcher stashes it alongside the session ID so tool
// handlers can resolve — and enforce — the workspace boundary for the
// session (Server.sessionScope). Unexported: external packages must
// use WithSessionCWD / SessionCWDFromContext.
type sessionCWDCtxKey struct{}

// WithSessionCWD returns a context carrying the session's working
// directory. The daemon dispatcher wraps each inbound frame with this
// before calling MCPServer.HandleMessage, giving every tool handler
// the cwd needed to resolve the session's workspace scope.
//
// An empty cwd returns ctx unchanged — that's the embedded stdio path
// (one implicit session, no cwd) and control clients; both fall back
// to the server-default scope.
func WithSessionCWD(ctx context.Context, cwd string) context.Context {
	if cwd == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionCWDCtxKey{}, cwd)
}

// SessionCWDFromContext returns the session cwd attached via
// WithSessionCWD, or "" when none is present.
func SessionCWDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if cwd, ok := ctx.Value(sessionCWDCtxKey{}).(string); ok {
		return cwd
	}
	return ""
}

// sessionLocal bundles the per-client state that should not aggregate
// across sessions: recent agent activity (viewed/modified files and
// symbols), and session-scoped token-savings counters. Shared pieces —
// the graph, feedback store, the cumulative savings store on disk —
// stay on *Server directly or are referenced via pointers that all
// sessions share.
type sessionLocal struct {
	session    *sessionState
	tokenStats *tokenStats
}

// newSessionLocal constructs a fresh per-session state container. The
// persistent savings store pointer is threaded in so per-session
// record() calls still contribute to cumulative totals on disk — each
// session's in-memory counters are isolated but the file they flush to
// is shared. parent, when non-nil, is the process-wide tokenStats
// aggregate; every per-session record() call also bumps it so the
// shared default reflects daemon-wide live activity.
func newSessionLocal(id string, persistent *savings.Store, repoPath string, parent *tokenStats) *sessionLocal {
	return &sessionLocal{
		session: newSessionState(),
		tokenStats: &tokenStats{
			persistent: persistent,
			repoPath:   repoPath,
			parent:     parent,
			sessionID:  id,
		},
	}
}

// sessionMap is a thread-safe string→*sessionLocal registry. Used by
// *Server to multiplex session-scoped state when running inside the
// daemon. The embedded / stdio server path doesn't consult this map;
// it reads *Server.session directly.
//
// The map also holds a pointer to the shared persistent savings store,
// so per-session tokenStats created by lazy get() calls inherit it
// automatically. Updating it via setPersistent propagates to every
// existing entry as well.
type sessionMap struct {
	mu         sync.Mutex
	sessions   map[string]*sessionLocal
	persistent *savings.Store
	repoPath   string
	// parent is the process-wide tokenStats aggregate. Each per-session
	// counter created by get() inherits it as its parent so record()
	// calls fan out to the daemon-wide totals.
	parent *tokenStats
}

func newSessionMap() *sessionMap {
	return &sessionMap{sessions: make(map[string]*sessionLocal)}
}

// setParentTokenStats installs the process-wide tokenStats so every
// session created here aggregates into it. Called once at server
// construction (Server.attachSessionMap) before any client connects.
func (m *sessionMap) setParentTokenStats(parent *tokenStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parent = parent
	for _, sl := range m.sessions {
		if sl.tokenStats == nil {
			continue
		}
		sl.tokenStats.mu.Lock()
		sl.tokenStats.parent = parent
		sl.tokenStats.mu.Unlock()
	}
}

// get returns the session state for id, creating it if absent. Never
// returns nil — a missing entry is created lazily. Thread-safe.
func (m *sessionMap) get(id string) *sessionLocal {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl, ok := m.sessions[id]
	if !ok {
		sl = newSessionLocal(id, m.persistent, m.repoPath, m.parent)
		m.sessions[id] = sl
	}
	return sl
}

// release drops the session entry for id. Called when the daemon's
// accept loop sees a proxy disconnect.
func (m *sessionMap) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
}

// setPersistent updates the shared savings store pointer and
// propagates it into every live session so no existing client flushes
// savings to a stale (or nil) store.
func (m *sessionMap) setPersistent(store *savings.Store, repoPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persistent = store
	m.repoPath = repoPath
	for _, sl := range m.sessions {
		if sl.tokenStats == nil {
			continue
		}
		sl.tokenStats.mu.Lock()
		sl.tokenStats.persistent = store
		sl.tokenStats.repoPath = repoPath
		sl.tokenStats.mu.Unlock()
	}
}
