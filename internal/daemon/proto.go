// Package daemon implements the long-living Gortex daemon plus the tiny
// stdio proxy that relays MCP traffic to it.
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
	// MemoryBytes is runtime.MemStats.Alloc — live allocated heap.
	// Retained for backwards compatibility with older clients; new
	// clients should read from Runtime.
	MemoryBytes   uint64             `json:"memory_bytes"`
	Runtime       RuntimeStats       `json:"runtime"`
	SearchBackend SearchBackendStats `json:"search_backend"`
	// PProfAddr is set when the daemon has opened an HTTP pprof
	// listener (via the GORTEX_DAEMON_PPROF_ADDR env var). Empty
	// string means pprof is not enabled on this daemon.
	PProfAddr string `json:"pprof_addr,omitempty"`
	// Ready is false while the daemon is still loading the snapshot and
	// re-indexing tracked repos in the background. The socket is reachable
	// even when Ready=false; queries against not-yet-indexed repos may
	// return partial results until warmup completes.
	Ready         bool  `json:"ready"`
	WarmupSeconds int64 `json:"warmup_seconds"`

	// Workspaces aggregates TrackedRepos by workspace slug. Empty
	// when no repo declares one (every repo defaults to its own
	// workspace; the table form is more compact in that case).
	Workspaces []WorkspaceSummary `json:"workspaces,omitempty"`

	// MCPSessions lists every connected proxy client (Claude Code,
	// Cursor, Codex, etc.). Empty when the daemon hasn't yet been
	// asked to track sessions.
	MCPSessions []MCPSessionStatus `json:"mcp_sessions,omitempty"`

	// ConfiguredServers reflects `~/.gortex/servers.toml` when
	// present. Empty when running single-server (the file is missing
	// or empty); in that case the daemon serves locally and the
	// multi-server router is disabled.
	ConfiguredServers []ConfiguredServerStatus `json:"configured_servers,omitempty"`
	// LocalServerSlug is the slug this daemon recognises as itself
	// when matching against ConfiguredServers — set from servers.toml
	// `default` or the first entry. Empty when no servers.toml.
	LocalServerSlug string `json:"local_server_slug,omitempty"`
}

// RuntimeStats captures Go runtime.MemStats fields users care about
// when diagnosing daemon memory: live vs reserve, GC pressure, and
// goroutine count. All byte fields are raw (not human-formatted).
type RuntimeStats struct {
	Alloc        uint64 `json:"alloc"`         // live heap allocations
	Sys          uint64 `json:"sys"`           // total bytes from OS
	HeapInuse    uint64 `json:"heap_inuse"`    // bytes in in-use spans
	HeapIdle     uint64 `json:"heap_idle"`     // bytes in idle spans (released or reserve)
	HeapReleased uint64 `json:"heap_released"` // bytes returned to OS
	StackInuse   uint64 `json:"stack_inuse"`   // goroutine stacks
	NumGC        uint32 `json:"num_gc"`        // completed GC cycles
	NumGoroutine int    `json:"num_goroutine"` // live goroutines
}

// SearchBackendStats identifies which search backend is currently
// serving queries, so users can read the `search_b` column in the
// repo breakdown with the right mental model. Bleve with the default
// gtreap KV store costs ~32 KiB per document; BM25 costs ~2 KiB.
type SearchBackendStats struct {
	Name      string `json:"name"`                 // "bm25" | "bleve-memory" | "bleve-disk"
	DocCount  int    `json:"doc_count"`            // indexed documents across all repos
	Bytes     uint64 `json:"bytes"`                // approximate heap footprint
	DiskPath  string `json:"disk_path,omitempty"`  // set only when Name == "bleve-disk"
	DiskBytes uint64 `json:"disk_bytes,omitempty"` // current on-disk size for "bleve-disk"
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
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	// Project is the GlobalConfig active-project slug — a named
	// grouping of repos in `~/.config/gortex/config.yaml::projects`.
	// Distinct from `WorkspaceProject` below, which is the project
	// slug from `.gortex.yaml::project`. Kept here for backwards
	// compatibility with older daemon clients that read the field.
	Project string `json:"project,omitempty"`
	// Workspace is the workspace slug stamped onto every node emitted
	// from this repo. Falls back to Prefix when no
	// `.gortex.yaml::workspace` is declared. Two repos that share a
	// Workspace pair their contracts as one logical service.
	Workspace string `json:"workspace,omitempty"`
	// WorkspaceProject is the project slug — the soft sub-boundary
	// inside Workspace. Falls back to Prefix when no
	// `.gortex.yaml::project` is declared.
	WorkspaceProject string          `json:"workspace_project,omitempty"`
	Ref              string          `json:"ref,omitempty"`
	Files            int             `json:"files"`
	Nodes            int             `json:"nodes"`
	Edges            int             `json:"edges"`
	LastIndex        int64           `json:"last_index_unix"`
	Memory           MemoryBreakdown `json:"memory"`
}

// WorkspaceSummary aggregates per-workspace stats so `gortex daemon
// status` can render a "workspaces" block above the per-repo table.
// Reflects the workspace hard boundary: counts roll up across every
// repo declaring the same workspace slug.
type WorkspaceSummary struct {
	Slug     string   `json:"slug"`
	Repos    []string `json:"repos"`
	Projects []string `json:"projects"`
	Files    int      `json:"files"`
	Nodes    int      `json:"nodes"`
	Edges    int      `json:"edges"`
}

// MCPSessionStatus is one row in the sessions list. Reports the
// per-client state the daemon tracks: connection time, current cwd
// (used by the router for cwd-based workspace resolution), and the
// remote agent identifier when the client supplied one in MCP's
// `clientInfo`.
type MCPSessionStatus struct {
	ID            string `json:"id"`
	Cwd           string `json:"cwd,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	ConnectedSecs int64  `json:"connected_secs"`
}

// ConfiguredServerStatus is one row in the servers list, mirroring a
// `~/.gortex/servers.toml::[[server]]` entry plus a "this is us"
// flag set when Slug equals the daemon's local slug.
type ConfiguredServerStatus struct {
	Slug       string   `json:"slug"`
	URL        string   `json:"url"`
	Default    bool     `json:"default,omitempty"`
	Local      bool     `json:"local,omitempty"`
	Workspaces []string `json:"workspaces,omitempty"`
	HasAuth    bool     `json:"has_auth,omitempty"`
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
	// DiskBytes is populated only when the Bleve backend is running in
	// disk mode (GORTEX_BLEVE_DISK_DIR set). Each repo gets a
	// node-proportional share of the on-disk index size. Zero in
	// memory-only mode.
	DiskBytes  uint64 `json:"disk_bytes,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
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
