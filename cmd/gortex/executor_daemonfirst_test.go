package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// stubDaemonServer is a minimal in-process daemon that speaks just enough of
// the AF_UNIX wire protocol for resolveExecutor to treat it as a warm daemon:
// it ACKs the handshake, answers ControlStatus with a configurable tracked
// repo set, and echoes a tools/call result back over a ModeMCP channel.
//
// It deliberately implements the protocol by hand (rather than booting the
// real daemon.Server) so the test exercises resolveExecutor end-to-end
// against an isolated socket without indexing anything.
type stubDaemonServer struct {
	ln           net.Listener
	trackedRepos []string

	mu        sync.Mutex
	lastTool  string
	lastArgs  map[string]any
	mcpResult json.RawMessage // raw result payload echoed in the content block
	mcpError  *stubRPCError   // when set, the MCP reply is a JSON-RPC error
}

type stubRPCError struct {
	Code      int
	Message   string
	ErrorCode string // rides on error.data.error_code
}

// startStubDaemon binds a unix socket, points GORTEX_DAEMON_SOCKET at it,
// and serves until the test ends. The socket lives under a short temp dir
// (not t.TempDir()) because the AF_UNIX path limit (~104 bytes on macOS) is
// shorter than the deeply-nested per-subtest t.TempDir() paths.
func startStubDaemon(t *testing.T, trackedRepos []string) *stubDaemonServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "gxd")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Setenv("GORTEX_DAEMON_SOCKET", sock)

	s := &stubDaemonServer{
		ln:           ln,
		trackedRepos: trackedRepos,
		mcpResult:    json.RawMessage(`{"ok":true}`),
	}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *stubDaemonServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *stubDaemonServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)

	hsLine, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var hs daemon.Handshake
	if err := json.Unmarshal(hsLine, &hs); err != nil {
		return
	}
	if err := daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true, DaemonVersion: "stub"}); err != nil {
		return
	}

	switch hs.Mode {
	case daemon.ModeControl:
		s.serveControl(conn, reader)
	case daemon.ModeMCP:
		s.serveMCP(conn, reader)
	}
}

func (s *stubDaemonServer) serveControl(conn net.Conn, reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req daemon.ControlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		if req.Kind != daemon.ControlStatus {
			_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: false, ErrorCode: "unsupported"})
			continue
		}
		st := daemon.StatusResponse{Version: "stub", Ready: true}
		for _, p := range s.trackedRepos {
			st.TrackedRepos = append(st.TrackedRepos, daemon.TrackedRepoStatus{Path: p})
		}
		raw, _ := json.Marshal(st)
		_ = daemon.WriteJSONLine(conn, daemon.ControlResponse{OK: true, Result: raw})
	}
}

func (s *stubDaemonServer) serveMCP(conn net.Conn, reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return
		}
		s.mu.Lock()
		s.lastTool = frame.Params.Name
		s.lastArgs = frame.Params.Arguments
		mcpErr := s.mcpError
		result := s.mcpResult
		s.mu.Unlock()

		if mcpErr != nil {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(frame.ID),
				"error": map[string]any{
					"code":    mcpErr.Code,
					"message": mcpErr.Message,
					"data":    map[string]any{"error_code": mcpErr.ErrorCode},
				},
			}
			b, _ := json.Marshal(resp)
			_, _ = conn.Write(append(b, '\n'))
			continue
		}

		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(frame.ID),
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(result)}},
			},
		}
		b, _ := json.Marshal(resp)
		_, _ = conn.Write(append(b, '\n'))
	}
}

func (s *stubDaemonServer) seenTool() (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTool, s.lastArgs
}

// TestResolveExecutor_DaemonFirst asserts resolveExecutor is daemon-first:
// when a warm daemon owns the repo it returns a daemonExecutor that relays
// the tool call over the daemon's MCP channel (the same warm graph the
// editor proxies hit). When no daemon is reachable, or the daemon does not
// track the repo, it returns ErrNoExecutor so the caller can fall back.
func TestResolveExecutor_DaemonFirst(t *testing.T) {
	repo := t.TempDir()

	t.Run("warm daemon owns repo -> daemonExecutor relays", func(t *testing.T) {
		stub := startStubDaemon(t, []string{repo})
		stub.mcpResult = json.RawMessage(`{"total":7}`)

		exec, err := resolveExecutor(repo)
		if err != nil {
			t.Fatalf("resolveExecutor on a warm daemon that owns the repo: %v", err)
		}
		defer func() { _ = exec.Close() }()

		if _, ok := exec.(*daemonExecutor); !ok {
			t.Fatalf("daemon-first path must return a *daemonExecutor, got %T", exec)
		}

		out, err := exec.CallTool(context.Background(), "graph_stats", map[string]any{"x": "y"})
		if err != nil {
			t.Fatalf("relay CallTool: %v", err)
		}
		var payload struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("relayed payload should be the tool json: %v (%s)", err, out)
		}
		if payload.Total != 7 {
			t.Fatalf("relayed payload total = %d, want 7", payload.Total)
		}
		// The tool name and the caller args really made it to the daemon.
		tool, args := stub.seenTool()
		if tool != "graph_stats" {
			t.Fatalf("daemon saw tool %q, want graph_stats", tool)
		}
		if args["x"] != "y" {
			t.Fatalf("caller args must be relayed, daemon saw %v", args)
		}
	})

	t.Run("no daemon reachable -> ErrNoExecutor", func(t *testing.T) {
		// Point the socket env at a path with no listener.
		t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "dead.sock"))
		_, err := resolveExecutor(repo)
		if !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("a dead socket must yield ErrNoExecutor, got %v", err)
		}
	})

	t.Run("daemon up but does not own repo -> ErrNoExecutor", func(t *testing.T) {
		// The daemon tracks some other tree, not our repo.
		startStubDaemon(t, []string{filepath.Join(t.TempDir(), "elsewhere")})
		_, err := resolveExecutor(repo)
		if !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("an untracked repo must yield ErrNoExecutor, got %v", err)
		}
	})
}

// TestDaemonExecutor_ErrDistinct asserts the daemonExecutor relay
// distinguishes the daemon's typed refusals (ErrRepoNotTracked) and the
// daemon-unavailable case (ErrNoExecutor) from a genuine tool error, which
// is surfaced verbatim. This is the distinction a CLI command relies on to
// decide whether to fall back or fail.
func TestDaemonExecutor_ErrDistinct(t *testing.T) {
	repo := t.TempDir()

	t.Run("repo_not_tracked maps to ErrRepoNotTracked", func(t *testing.T) {
		stub := startStubDaemon(t, []string{repo})
		stub.mcpError = &stubRPCError{Code: -32000, Message: "repository not tracked", ErrorCode: "repo_not_tracked"}

		exec, err := resolveExecutor(repo)
		if err != nil {
			t.Fatalf("resolveExecutor: %v", err)
		}
		defer func() { _ = exec.Close() }()

		_, callErr := exec.CallTool(context.Background(), "search_symbols", nil)
		if !errors.Is(callErr, ErrRepoNotTracked) {
			t.Fatalf("repo_not_tracked must map to ErrRepoNotTracked, got %v", callErr)
		}
		// It must NOT be confused with a generic tool error or no-executor.
		if errors.Is(callErr, ErrNoExecutor) {
			t.Fatalf("ErrRepoNotTracked must be distinct from ErrNoExecutor")
		}
	})

	t.Run("real tool error surfaces verbatim", func(t *testing.T) {
		stub := startStubDaemon(t, []string{repo})
		stub.mcpError = &stubRPCError{Code: -32602, Message: "bad symbol id"}

		exec, err := resolveExecutor(repo)
		if err != nil {
			t.Fatalf("resolveExecutor: %v", err)
		}
		defer func() { _ = exec.Close() }()

		_, callErr := exec.CallTool(context.Background(), "get_symbol", nil)
		if callErr == nil {
			t.Fatal("a real tool error must surface, got nil")
		}
		if errors.Is(callErr, ErrRepoNotTracked) || errors.Is(callErr, ErrNoExecutor) {
			t.Fatalf("a real tool error must not collapse to a sentinel, got %v", callErr)
		}
		if callErr.Error() != "bad symbol id" {
			t.Fatalf("real error must surface verbatim, got %q", callErr.Error())
		}
	})

	t.Run("ErrNoExecutor is its own sentinel", func(t *testing.T) {
		// No daemon at all -> the resolve step (not the call step) yields
		// ErrNoExecutor, which is a distinct sentinel from a tool error.
		t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
		_, err := resolveExecutor(repo)
		if !errors.Is(err, ErrNoExecutor) {
			t.Fatalf("want ErrNoExecutor, got %v", err)
		}
		if errors.Is(err, ErrRepoNotTracked) {
			t.Fatalf("ErrNoExecutor must be distinct from ErrRepoNotTracked")
		}
	})
}
