// Package daemon implements the long-living Gortex daemon plus the tiny
// stdio proxy that relays MCP traffic to it. See spec-daemon.md at the repo
// root for the end-to-end architecture.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
)

// ProtocolVersion is the daemon wire-protocol version. Both ends of a
// connection must agree; the handshake exchange rejects mismatches so an
// older proxy talking to a newer daemon (or vice versa) fails loudly
// instead of corrupting state.
const ProtocolVersion = 1

// ConnectionMode identifies the traffic shape the client wants after the
// handshake completes.
//
// ModeMCP — MCP JSON-RPC 2.0 pass-through (newline-delimited). The proxy
// dumps whatever its MCP client sent on stdin straight to the socket.
// ModeControl — Gortex control RPC (track, untrack, status, reload,
// shutdown). Used by the CLI and other daemon subcommands.
type ConnectionMode string

const (
	ModeMCP     ConnectionMode = "mcp"
	ModeControl ConnectionMode = "control"
)

// Handshake is the first message every client sends after dialing the
// socket. Everything flowing through after the ACK is mode-dependent.
//
// CWD lets the daemon derive a default repo scope when the client is an
// MCP proxy. Empty is acceptable for control clients.
type Handshake struct {
	Version    int            `json:"version"`
	Mode       ConnectionMode `json:"mode"`
	CWD        string         `json:"cwd,omitempty"`
	ClientName string         `json:"client,omitempty"` // e.g. "claude-code", "kiro", "cli"
	PID        int            `json:"pid,omitempty"`
}

// HandshakeAck is the daemon's reply to a handshake. On failure ErrorCode
// is non-empty and the connection is closed after the ack is written.
type HandshakeAck struct {
	// Protocol / status.
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrorMsg  string `json:"error_msg,omitempty"`

	// Populated on success.
	SessionID     string `json:"session_id,omitempty"`
	DefaultRepo   string `json:"default_repo,omitempty"`
	ActiveProject string `json:"active_project,omitempty"`

	// For clients that want to compare before trusting the connection.
	DaemonVersion string `json:"daemon_version,omitempty"`
}

// Error codes reported in HandshakeAck.ErrorCode. Kept small and stable so
// client code can branch on them without parsing prose.
const (
	ErrProtocolMismatch = "protocol_mismatch"
	ErrUnsupportedMode  = "unsupported_mode"
	ErrRepoNotTracked   = "repo_not_tracked"
	ErrInternal         = "internal"
)

// ControlRequest is the envelope for every message sent in ModeControl.
// Kind identifies which operation; Params is operation-specific JSON.
type ControlRequest struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ControlResponse is the paired envelope from the daemon. One response per
// request, in order. Non-empty ErrorCode means the operation failed.
type ControlResponse struct {
	OK        bool            `json:"ok"`
	ErrorCode string          `json:"error_code,omitempty"`
	ErrorMsg  string          `json:"error_msg,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// Control operation kinds. One constant per kind so callers can't typo.
const (
	ControlTrack         = "track"
	ControlUntrack       = "untrack"
	ControlReload        = "reload"
	ControlStatus        = "status"
	ControlShutdown      = "shutdown"
	ControlSearchSymbols = "search_symbols"
)

// TrackParams is the payload for ControlTrack.
type TrackParams struct {
	Path    string `json:"path"`
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	Ref     string `json:"ref,omitempty"`
}

// UntrackParams is the payload for ControlUntrack.
type UntrackParams struct {
	PathOrPrefix string `json:"path_or_prefix"`
}

// StatusResponse is the payload returned under Result on a successful
// ControlStatus call.
type StatusResponse struct {
	Version       string              `json:"version"`
	PID           int                 `json:"pid"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	SocketPath    string              `json:"socket_path"`
	TrackedRepos  []TrackedRepoStatus `json:"tracked_repos"`
	Sessions      int                 `json:"sessions"`
	MemoryBytes   uint64              `json:"memory_bytes"`
	// Ready is false while the daemon is still loading the snapshot and
	// re-indexing tracked repos in the background. The socket is reachable
	// even when Ready=false; queries against not-yet-indexed repos may
	// return partial results until warmup completes.
	Ready          bool  `json:"ready"`
	WarmupSeconds  int64 `json:"warmup_seconds"`
}

// SearchSymbolsParams is the payload for ControlSearchSymbols.
// Repo (optional) limits results to one repo prefix; Limit caps the result
// count (zero defaults to a small built-in cap so unbounded calls can't
// stall the hook).
type SearchSymbolsParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

// SymbolHit is one entry in SearchSymbolsResult.Hits.
type SymbolHit struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line,omitempty"`
}

// SearchSymbolsResult is the payload returned under Result for a
// successful ControlSearchSymbols call.
type SearchSymbolsResult struct {
	Hits []SymbolHit `json:"hits"`
}

// TrackedRepoStatus is one row in StatusResponse.TrackedRepos.
type TrackedRepoStatus struct {
	Prefix    string          `json:"prefix"`
	Path      string          `json:"path"`
	Name      string          `json:"name,omitempty"`
	Project   string          `json:"project,omitempty"`
	Ref       string          `json:"ref,omitempty"`
	Files     int             `json:"files"`
	Nodes     int             `json:"nodes"`
	Edges     int             `json:"edges"`
	LastIndex int64           `json:"last_index_unix"`
	Memory    MemoryBreakdown `json:"memory"`
}

// MemoryBreakdown is a per-repo memory estimate split across the
// data structures that dominate the daemon's footprint. All values
// are approximate — exact accounting would require walking Go's
// heap, which is too expensive for a status call. See the individual
// estimators (graph.RepoMemoryEstimate, search.BleveBackend.SizeBytes,
// search.VectorBackend.SizeBytes) for methodology.
type MemoryBreakdown struct {
	NodesBytes   uint64 `json:"nodes_bytes"`
	EdgesBytes   uint64 `json:"edges_bytes"`
	SearchBytes  uint64 `json:"search_bytes"`
	VectorsBytes uint64 `json:"vectors_bytes"`
	TotalBytes   uint64 `json:"total_bytes"`
}

// WriteJSONLine writes v as one JSON object followed by a newline. The
// daemon protocol is newline-delimited JSON (NDJSON) so we can scan it
// cheaply with bufio.Scanner.
func WriteJSONLine(w io.Writer, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	buf = append(buf, '\n')
	_, err = w.Write(buf)
	return err
}
