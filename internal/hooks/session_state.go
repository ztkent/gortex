package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// sessionState is the small per-session record the PreToolUse hook
// persists across individual tool calls. Claude Code invokes the hook
// as a fresh process per tool call, so any cross-call signal (has the
// agent consulted the graph yet? how long is the non-symbolic streak?)
// has to round-trip through disk keyed by session_id.
//
// Every field must be safe to read as its zero value — a missing or
// corrupt state file degrades to "fresh session", never an error.
type sessionState struct {
	// GraphConsulted records that the agent has invoked at least one
	// Gortex MCP tool this session. ModeConsultUnlock keys the
	// deny→additionalContext downgrade on it.
	GraphConsulted bool `json:"graph_consulted,omitempty"`
}

// hookSessionDirEnvVar lets tests redirect the per-session state
// directory, parallel to GORTEX_HOOK_LOG for telemetry.
const hookSessionDirEnvVar = "GORTEX_HOOK_SESSION_DIR"

// sessionStateDir returns the directory holding per-session state
// files. Honors GORTEX_HOOK_SESSION_DIR so tests can point it at a
// t.TempDir(). Returns "" when no base directory can be resolved — all
// callers treat "" as "state disabled" and degrade gracefully.
func sessionStateDir() string {
	if p := strings.TrimSpace(os.Getenv(hookSessionDirEnvVar)); p != "" {
		return p
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "gortex", "sessions")
}

// sanitizeSessionID reduces an arbitrary session_id to a safe single
// path segment: only [A-Za-z0-9._-] survive, everything else becomes
// '_'. Guards against path traversal ("../") and separators in a
// session_id that originates from the untrusted hook payload.
func sanitizeSessionID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	// A name that sanitizes to all dots ("." / "..") is still unsafe
	// as a path segment — neutralize it.
	if out == "" || strings.Trim(out, ".") == "" {
		return ""
	}
	return out
}

// sessionStatePath returns the JSON file path for a session, or "" when
// the session ID is empty/unusable or no base directory is available.
func sessionStatePath(sessionID string) string {
	safe := sanitizeSessionID(sessionID)
	if safe == "" {
		return ""
	}
	dir := sessionStateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, safe+".json")
}

// loadSessionState reads the per-session record. Best-effort: an empty
// session ID, a missing file, or any read/decode error all yield a
// zero-value sessionState. The hook must never block on state I/O.
func loadSessionState(sessionID string) sessionState {
	path := sessionStatePath(sessionID)
	if path == "" {
		return sessionState{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionState{}
	}
	var st sessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return sessionState{}
	}
	return st
}

// saveSessionState writes the per-session record. Best-effort, mirroring
// telemetry.go: every error is swallowed so a read-only cache dir or a
// full disk can never stop a tool call from proceeding.
func saveSessionState(sessionID string, st sessionState) {
	path := sessionStatePath(sessionID)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
