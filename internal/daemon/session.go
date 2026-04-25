package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync"
	"time"
)

// Session represents one proxy or CLI connection to the daemon. Per-session
// state (recent activity, symbol history, token stats for this client)
// lives here; shared state (the graph, feedback store, cumulative savings)
// lives on the Server.
//
// A Session is created on a successful handshake and destroyed when its
// socket connection closes. The daemon routes every inbound frame to its
// session by looking up the net.Conn in the session registry.
type Session struct {
	ID            string
	Mode          ConnectionMode
	CWD           string
	ClientName    string
	// ClientVersion is the version reported by the MCP client in its
	// `initialize` request (`params.clientInfo.version`). Empty until
	// the daemon dispatcher sees that frame; the env-var sniff in
	// `cmd/gortex/proxy.go::detectClientName` only fills ClientName.
	ClientVersion string
	// ClientNameSource records where ClientName came from so the
	// MCP-frame snooper can decide whether to overwrite it. "handshake"
	// is the env-var fallback the proxy posts at connect time;
	// "initialize" is the authoritative MCP-protocol value. Anything
	// from "initialize" wins over any "handshake" — including the
	// "unknown" string the proxy uses when env-var detection fails.
	ClientNameSource string
	ClientPID     int
	DefaultRepo   string
	ActiveProject string
	StartedAt     time.Time

	// Conn is the underlying socket. Kept for close-on-shutdown and
	// logging; handlers should not read from or write to it directly —
	// framing is the transport's job.
	Conn net.Conn

	// Per-session mutable state that will move over from internal/mcp's
	// Server during the session-isolation refactor. Left as interface{}
	// for now so the types can evolve without churning this file every
	// iteration — the refactor will replace this with concrete pointers.
	SessionState any
	SymHistory   any
	TokenStats   any

	// mu protects ClientName / ClientVersion / ClientNameSource which
	// can be updated by the dispatcher mid-session when the MCP
	// initialize frame arrives.
	mu sync.RWMutex
}

// SetClientInfo updates the session's client metadata from the MCP
// `initialize` frame. Called by the daemon dispatcher when it sees
// the first `initialize` request on this session. Idempotent — a
// second call (e.g. on protocol re-init) just overwrites.
func (s *Session) SetClientInfo(name, version string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if name != "" {
		s.ClientName = name
		s.ClientNameSource = "initialize"
	}
	if version != "" {
		s.ClientVersion = version
	}
	s.mu.Unlock()
}

// SnapshotClientInfo returns the current client name/version pair
// safely under the session lock. Used by the status path which reads
// while the dispatcher may be writing.
func (s *Session) SnapshotClientInfo() (name, version string) {
	if s == nil {
		return "", ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ClientName, s.ClientVersion
}

// SessionRegistry tracks active sessions. Safe for concurrent access from
// the accept goroutine and the control-surface handlers.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session // session_id → Session
	byConn   map[net.Conn]*Session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*Session),
		byConn:   make(map[net.Conn]*Session),
	}
}

// Register creates and stores a new session for the given connection.
// Called after a successful handshake. Generates the session ID.
func (r *SessionRegistry) Register(conn net.Conn, h Handshake) *Session {
	s := &Session{
		ID:               newSessionID(),
		Mode:             h.Mode,
		CWD:              h.CWD,
		ClientName:       h.ClientName,
		ClientNameSource: "handshake",
		ClientPID:        h.PID,
		StartedAt:        time.Now(),
		Conn:             conn,
	}
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.byConn[conn] = s
	r.mu.Unlock()
	return s
}

// Remove deletes the session for a connection. Idempotent — safe to call
// from both the accept-loop's defer and the shutdown path.
func (r *SessionRegistry) Remove(conn net.Conn) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byConn[conn]
	if s == nil {
		return nil
	}
	delete(r.byConn, conn)
	delete(r.sessions, s.ID)
	return s
}

// Get returns the session for a connection, or nil if the connection hasn't
// completed its handshake yet (or was already removed).
func (r *SessionRegistry) Get(conn net.Conn) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byConn[conn]
}

// Count returns the number of live sessions — used by the status command
// and for metrics.
func (r *SessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// All returns a snapshot of every live session. The caller must not
// mutate the returned Session objects; they're shared with the registry.
func (r *SessionRegistry) All() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// newSessionID generates a short URL-safe identifier. 8 bytes of entropy
// gives us 16 hex chars — collision-resistant enough for a per-user
// single-process registry without bloating log lines.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}
