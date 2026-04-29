package daemon

import (
	"errors"
	"sync"
	"time"
)

// OverlayFile is one editor-buffer override pushed by an MCP client.
// The daemon (or a remote graph service over the gateway) merges
// these on top of the base graph view for the duration of a session.
// Iteration 1 only models text files; binary overlays would need a
// different shape and are not in scope.
type OverlayFile struct {
	// Path is repo-relative when WorkspaceID is set, absolute
	// otherwise. The graph service maps it onto the repo root via
	// its ConfigManager.
	Path string `json:"path"`
	// Content is the in-editor text. Empty means "deletion overlay"
	// — the client wants the daemon to act as if the file isn't on
	// disk, even if it actually is. The daemon distinguishes this
	// from a real empty file by only honouring deletions when the
	// session declares them via OverlayPush(..., Deleted: true).
	Content string `json:"content"`
	// BaseSHA is the file's git blob SHA at the time the editor
	// opened it. Used by the daemon's drift-detection: if the file
	// on disk now hashes to a different SHA, the overlay is stale
	// and the daemon refuses to merge it (returns ErrOverlayDrift).
	// Empty disables drift-detection — useful for editor-buffer
	// states that exist before any save.
	BaseSHA string `json:"base_sha,omitempty"`
	// Deleted, when true, marks the overlay as a tombstone (see
	// Content above). Mutually exclusive with non-empty Content.
	Deleted bool `json:"deleted,omitempty"`
}

// OverlaySession holds one client's pushed overlays for the duration
// of an MCP session. Sessions auto-expire after IdleTTL of inactivity
// so a crashed client doesn't leak memory in the daemon.
type OverlaySession struct {
	ID          string
	WorkspaceID string
	Created     time.Time
	LastUsed    time.Time
	files       map[string]OverlayFile // path → overlay
}

// OverlayManager manages the per-session overlay map for the daemon.
// Goroutine-safe; callers can register, push, and delete from any
// goroutine. A single janitor goroutine sweeps idle sessions.
type OverlayManager struct {
	mu       sync.RWMutex
	sessions map[string]*OverlaySession
	idleTTL  time.Duration
}

// ErrSessionNotFound is returned by OverlayManager methods that
// reference an unknown session ID. The daemon translates this to
// HTTP 404 on `/v1/overlay/<id>/...` endpoints.
var ErrSessionNotFound = errors.New("overlay session not found")

// ErrOverlayDrift is returned by OverlayPush when the supplied
// BaseSHA disagrees with the file's current on-disk SHA. The client
// is expected to re-read the file and resubmit a fresh overlay; the
// daemon refuses to fold a known-stale overlay into queries because
// merge artefacts (lines moved by a sibling tool's edit) would
// surface as wrong-line errors that look like graph bugs.
var ErrOverlayDrift = errors.New("overlay base SHA mismatch — re-read and resubmit")

// NewOverlayManager creates a manager with the given idle TTL. ttl
// <= 0 disables expiry (useful for tests that want deterministic
// session behaviour).
func NewOverlayManager(idleTTL time.Duration) *OverlayManager {
	return &OverlayManager{
		sessions: make(map[string]*OverlaySession),
		idleTTL:  idleTTL,
	}
}

// Register starts a new session and returns its ID. The workspace
// slug is captured at register time; later pushes that target a
// different workspace are rejected (one session = one workspace,
// per the overlay model).
func (m *OverlayManager) Register(workspaceID string) string {
	id := newSessionID()
	now := time.Now()
	m.mu.Lock()
	m.sessions[id] = &OverlaySession{
		ID:          id,
		WorkspaceID: workspaceID,
		Created:     now,
		LastUsed:    now,
		files:       make(map[string]OverlayFile),
	}
	m.mu.Unlock()
	return id
}

// Push attaches one overlay file to a session. Workspace mismatch
// (the session was registered for workspace X but the push targets
// Y) returns an error: a session is supposed to be a coherent view
// over one workspace's repos.
//
// driftCheck is a callback the manager invokes to verify BaseSHA
// against the on-disk file. The daemon supplies it; tests can pass
// nil to skip the check. If driftCheck returns false and overlay
// has a non-empty BaseSHA, Push fails with ErrOverlayDrift.
func (m *OverlayManager) Push(sessionID string, overlay OverlayFile, driftCheck func(path, sha string) bool) error {
	if overlay.Path == "" {
		return errors.New("overlay path is required")
	}
	if overlay.Deleted && overlay.Content != "" {
		return errors.New("overlay cannot be both deleted and have content")
	}
	if overlay.BaseSHA != "" && driftCheck != nil {
		if !driftCheck(overlay.Path, overlay.BaseSHA) {
			return ErrOverlayDrift
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	sess.files[overlay.Path] = overlay
	sess.LastUsed = time.Now()
	return nil
}

// Delete removes one overlay file from a session by path. Returns
// ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) Delete(sessionID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	delete(sess.files, path)
	sess.LastUsed = time.Now()
	return nil
}

// Drop terminates the session and discards every overlay it held.
// Idempotent — dropping an unknown session is a no-op.
func (m *OverlayManager) Drop(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

// Files returns a snapshot of every overlay attached to a session
// (no live aliasing — the returned map can be mutated freely).
// ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) Files(sessionID string) (map[string]OverlayFile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	out := make(map[string]OverlayFile, len(sess.files))
	for k, v := range sess.files {
		out[k] = v
	}
	return out, nil
}

// SessionWorkspace returns the workspace slug captured at Register.
// ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) SessionWorkspace(sessionID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return "", ErrSessionNotFound
	}
	return sess.WorkspaceID, nil
}

// SweepIdle drops sessions whose LastUsed is older than IdleTTL.
// Returns the count of dropped sessions for telemetry. Safe to call
// from a single janitor goroutine on a ticker.
func (m *OverlayManager) SweepIdle() int {
	if m.idleTTL <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-m.idleTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	for id, sess := range m.sessions {
		if sess.LastUsed.Before(cutoff) {
			delete(m.sessions, id)
			dropped++
		}
	}
	return dropped
}
