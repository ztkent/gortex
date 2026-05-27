package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/platform"
)

// Server is the long-living Gortex daemon. It owns the Unix socket
// listener, the session registry, and the control-surface dispatcher.
// MCP traffic is plumbed through a ToolDispatcher that's injected at
// construction time — the daemon package deliberately doesn't depend
// on internal/mcp to keep the direction of imports clean.
type Server struct {
	SocketPath string
	Version    string
	Logger     *zap.Logger

	// Dispatcher handles MCP mode traffic (JSON-RPC 2.0) after handshake.
	// A nil Dispatcher means this daemon is control-only — useful for
	// tests and for early integration before the MCP passthrough lands.
	MCPDispatcher MCPDispatcher

	// Controller handles control-mode RPCs (track/untrack/reload/status/shutdown).
	Controller Controller

	// HTTPHandler, when non-nil, is mounted on a TCP listener at
	// HTTPAddr alongside the unix-socket dispatcher. This is how the
	// MCP 2026 Streamable HTTP transport reaches the daemon —
	// internal/mcp/streamable.Transport plugs in here. Nil disables
	// the HTTP face entirely; the unix-socket transport keeps
	// working unchanged. HTTPAddr accepts standard net.Listen
	// addresses; "127.0.0.1:7411" is the recommended default for a
	// single-user dev box.
	HTTPHandler http.Handler
	HTTPAddr    string

	sessions     *SessionRegistry
	listener     net.Listener
	httpListener net.Listener
	httpServer   *http.Server
	started      time.Time

	shutdown chan struct{}
	doneOnce sync.Once
	conns    map[net.Conn]struct{}
	connsMu  sync.Mutex
}

// MCPDispatcher is implemented by whichever layer runs the MCP tool
// handlers. The daemon hands off one JSON-RPC frame at a time (raw bytes,
// newline-delimited) and the dispatcher returns the response bytes to
// write back. Session gives the dispatcher the per-client context it
// needs (scope, session-level state). Return an empty slice to suppress
// the response (notifications with no reply).
type MCPDispatcher interface {
	Dispatch(ctx context.Context, sess *Session, frame []byte) ([]byte, error)
}

// SessionEndedHook is an optional extension that MCPDispatcher
// implementations can satisfy to get a disconnect callback. The daemon
// invokes it in the per-connection goroutine's defer, giving
// implementations a chance to release per-session state (e.g., the
// `*mcp.Server.sessions` map entry) so idle memory doesn't grow with
// total session-count-ever.
//
// Implementations must be fast and non-blocking — this fires during
// connection teardown.
type SessionEndedHook interface {
	SessionEnded(sess *Session)
}

// Controller implements the daemon's control surface. Separated from
// MCPDispatcher so the two can evolve independently and so control-only
// tests don't need a full MCP stack.
type Controller interface {
	Track(ctx context.Context, params TrackParams) (json.RawMessage, error)
	Untrack(ctx context.Context, params UntrackParams) (json.RawMessage, error)
	Reload(ctx context.Context) (json.RawMessage, error)
	Status(ctx context.Context) (StatusResponse, error)
	// SearchSymbols is the cheap probe path used by external clients
	// (Claude Code's Grep-redirect hook) that need a single short answer
	// without setting up a full MCP session.
	SearchSymbols(ctx context.Context, params SearchSymbolsParams) (SearchSymbolsResult, error)
	// EnrichChurn runs the per-symbol / per-file churn enricher against
	// the daemon's in-process graph. Exposed over the control surface so
	// CLI invocations (and the post-commit / post-merge git hook) can
	// trigger it without taking the LadyBug write lock the daemon owns.
	EnrichChurn(ctx context.Context, params EnrichChurnParams) (EnrichChurnResult, error)
	// Shutdown is invoked via the control surface and should return
	// quickly; the daemon's actual shutdown work happens after the
	// response is written.
	Shutdown(ctx context.Context) error
}

// New builds a Server but does not start listening.
func New(socketPath, version string, logger *zap.Logger) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{
		SocketPath: socketPath,
		Version:    version,
		Logger:     logger,
		sessions:   NewSessionRegistry(),
		shutdown:   make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

// Listen creates the socket, writes the PID file, and installs the
// shutdown-signal handlers for graceful shutdown. The socket permissions
// are 0o600 on Unix — the daemon is user-local and nothing else on the
// machine should reach it; on Windows, %LocalAppData% ACLs scope it to
// the user instead.
func (s *Server) Listen() error {
	if err := EnsureParentDir(s.SocketPath); err != nil {
		return fmt.Errorf("ensure socket dir: %w", err)
	}
	// Remove stale socket file from a crashed previous run. If the daemon
	// is actually running, the PID check below will catch it and abort.
	_ = os.Remove(s.SocketPath)

	if err := s.writePIDFile(); err != nil {
		return fmt.Errorf("pid file: %w", err)
	}

	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "unix", s.SocketPath)
	if err != nil {
		_ = os.Remove(PIDFilePath())
		return fmt.Errorf("listen: %w", err)
	}
	// chmod the socket to user-only on Unix. Windows has no POSIX mode
	// bits — the socket inherits the ACLs of %LocalAppData%, which is
	// already user-scoped — so skip it there.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.SocketPath, 0o600); err != nil {
			_ = l.Close()
			return fmt.Errorf("chmod socket: %w", err)
		}
	}
	s.listener = l
	s.started = time.Now()

	// Optional HTTP listener for the MCP 2026 Streamable transport.
	// We bring it up alongside the unix-socket listener so both
	// transports share the same shutdown / lifecycle plumbing. A
	// listen failure here is fatal — running the unix-socket
	// transport silently while HTTP is down would mask the operator
	// misconfiguration that pointed clients at a port that never
	// answered.
	if s.HTTPHandler != nil && s.HTTPAddr != "" {
		httpLn, herr := net.Listen("tcp", s.HTTPAddr)
		if herr != nil {
			_ = l.Close()
			_ = os.Remove(PIDFilePath())
			return fmt.Errorf("listen http: %w", herr)
		}
		s.httpListener = httpLn
		s.httpServer = &http.Server{
			Handler:           s.HTTPHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	// Install signal handlers once the listener is live.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)
	go func() {
		<-sigCh
		s.Logger.Info("daemon: received signal, shutting down")
		_ = s.Shutdown()
	}()
	return nil
}

// Serve runs the accept loop. Blocks until Shutdown is called or the
// listener returns an unrecoverable error. When an HTTP listener was
// brought up by Listen it runs concurrently in its own goroutine; an
// HTTP-side failure pushes onto the same shutdown channel so the
// unix-socket loop tears down too.
func (s *Server) Serve() error {
	if s.listener == nil {
		return errors.New("daemon: Listen must be called before Serve")
	}
	if s.httpListener != nil && s.httpServer != nil {
		go func() {
			if err := s.httpServer.Serve(s.httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.Logger.Warn("daemon: http serve exited", zap.Error(err))
			}
		}()
		s.Logger.Info("daemon: http listener active",
			zap.String("addr", s.httpListener.Addr().String()))
	}
	s.Logger.Info("daemon: serving", zap.String("socket", s.SocketPath))
	var emfileBackoff time.Duration
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// listener closed during Shutdown — normal exit.
			select {
			case <-s.shutdown:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// EMFILE means the process is out of file descriptors.
			// Without backoff the loop spins, pinning a CPU and making
			// the FD pressure even worse. The exponential ramp gives
			// in-flight handlers time to release descriptors.
			if isEMFILE(err) {
				if emfileBackoff == 0 {
					emfileBackoff = 5 * time.Millisecond
				} else if emfileBackoff < time.Second {
					emfileBackoff *= 2
				}
				s.Logger.Warn("daemon: accept failed, FD-starved — backing off",
					zap.Error(err), zap.Duration("sleep", emfileBackoff))
				select {
				case <-time.After(emfileBackoff):
				case <-s.shutdown:
					return nil
				}
				continue
			}
			emfileBackoff = 0
			s.Logger.Warn("daemon: accept failed", zap.Error(err))
			continue
		}
		emfileBackoff = 0
		s.trackConn(conn)
		go s.handle(conn)
	}
}

// handle runs the per-connection lifecycle: handshake → dispatch loop →
// cleanup. Every exit path must remove the session and close the conn.
func (s *Server) handle(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.untrackConn(conn)
		if sess := s.sessions.Remove(conn); sess != nil {
			// Fire the optional disconnect hook so implementations can
			// release per-session resources keyed by this ID.
			if hook, ok := s.MCPDispatcher.(SessionEndedHook); ok && hook != nil {
				hook.SessionEnded(sess)
			}
			s.Logger.Debug("daemon: session closed",
				zap.String("session_id", sess.ID),
				zap.String("client", sess.ClientName))
		}
	}()

	reader := bufio.NewReader(conn)
	sess, err := s.handshake(conn, reader)
	if err != nil {
		s.Logger.Warn("daemon: handshake failed", zap.Error(err))
		return
	}

	switch sess.Mode {
	case ModeMCP:
		s.serveMCP(conn, reader, sess)
	case ModeControl:
		s.serveControl(conn, reader, sess)
	default:
		s.Logger.Warn("daemon: unknown mode after handshake",
			zap.String("mode", string(sess.Mode)))
	}
}

// handshake reads one handshake frame, validates it, and replies with an
// ack. A rejected handshake writes an error ack then closes the connection.
func (s *Server) handshake(conn net.Conn, reader *bufio.Reader) (*Session, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read handshake: %w", err)
	}

	var h Handshake
	if err := json.Unmarshal(line, &h); err != nil {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrInternal,
			ErrorMsg:  "invalid handshake json: " + err.Error(),
		})
		return nil, fmt.Errorf("parse handshake: %w", err)
	}
	if h.Version != ProtocolVersion {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrProtocolMismatch,
			ErrorMsg: fmt.Sprintf("daemon expects protocol %d, client sent %d",
				ProtocolVersion, h.Version),
		})
		return nil, fmt.Errorf("protocol mismatch: %d vs %d", ProtocolVersion, h.Version)
	}
	if h.Mode != ModeMCP && h.Mode != ModeControl {
		_ = WriteJSONLine(conn, HandshakeAck{
			ErrorCode: ErrUnsupportedMode,
			ErrorMsg:  "mode must be 'mcp' or 'control'",
		})
		return nil, fmt.Errorf("unsupported mode: %q", h.Mode)
	}

	sess := s.sessions.Register(conn, h)

	ack := HandshakeAck{
		OK:            true,
		SessionID:     sess.ID,
		DaemonVersion: s.Version,
	}
	if err := WriteJSONLine(conn, ack); err != nil {
		_ = s.sessions.Remove(conn)
		return nil, fmt.Errorf("write ack: %w", err)
	}
	s.Logger.Debug("daemon: session established",
		zap.String("session_id", sess.ID),
		zap.String("mode", string(sess.Mode)),
		zap.String("cwd", sess.CWD),
		zap.String("client", sess.ClientName))
	return sess, nil
}

// serveMCP pumps MCP JSON-RPC frames. Each line on the wire is a single
// message. The Dispatcher gets the raw frame + session context and
// returns the raw reply to write back. Nil reply = no response (the
// client sent a notification).
func (s *Server) serveMCP(conn net.Conn, reader *bufio.Reader, sess *Session) {
	if s.MCPDispatcher == nil {
		_ = WriteJSONLine(conn, map[string]any{
			"jsonrpc": "2.0",
			"error": map[string]any{
				"code":    -32000,
				"message": "daemon started without MCP dispatcher; control-only mode",
			},
			"id": nil,
		})
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.Logger.Debug("daemon: mcp read closed",
					zap.String("session_id", sess.ID), zap.Error(err))
			}
			return
		}
		// Scanner-style: trim trailing newline but keep the payload as-is
		// so the dispatcher sees valid JSON.
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		if len(line) == 0 {
			continue
		}

		ctx := context.Background()
		reply, err := s.MCPDispatcher.Dispatch(ctx, sess, line)
		if err != nil {
			s.Logger.Warn("daemon: dispatch error",
				zap.String("session_id", sess.ID), zap.Error(err))
			continue
		}
		if len(reply) == 0 {
			continue
		}
		// The dispatcher returns a full JSON-RPC frame; re-append newline.
		if _, werr := conn.Write(append(reply, '\n')); werr != nil {
			s.Logger.Debug("daemon: mcp write failed",
				zap.String("session_id", sess.ID), zap.Error(werr))
			return
		}
	}
}

// serveControl drains ControlRequest messages, invokes the Controller,
// and writes paired ControlResponse messages.
func (s *Server) serveControl(conn net.Conn, reader *bufio.Reader, sess *Session) {
	if s.Controller == nil {
		_ = WriteJSONLine(conn, ControlResponse{
			ErrorCode: ErrInternal,
			ErrorMsg:  "daemon started without controller",
		})
		return
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req ControlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = WriteJSONLine(conn, ControlResponse{
				ErrorCode: ErrInternal,
				ErrorMsg:  "malformed request: " + err.Error(),
			})
			continue
		}
		resp := s.handleControl(sess, req)
		if err := WriteJSONLine(conn, resp); err != nil {
			return
		}
		if req.Kind == ControlShutdown && resp.OK {
			// Give the client one more moment to flush the ack before the
			// listener goes away, then stop.
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = s.Shutdown()
			}()
			return
		}
	}
}

func (s *Server) handleControl(_ *Session, req ControlRequest) ControlResponse {
	ctx := context.Background()
	switch req.Kind {
	case ControlTrack:
		var p TrackParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.Track(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlUntrack:
		var p UntrackParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.Untrack(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlReload:
		result, err := s.Controller.Reload(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true, Result: result}

	case ControlStatus:
		st, err := s.Controller.Status(ctx)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		// Daemon-level fields the controller doesn't know about.
		st.Version = s.Version
		st.PID = os.Getpid()
		st.UptimeSeconds = int64(time.Since(s.started).Seconds())
		st.SocketPath = s.SocketPath
		st.Sessions = s.sessions.Count()
		// Per-session detail (cwd, client name, connect time) for the
		// status command's "sessions" block. The controller can't see
		// these — sessions live on the daemon server, not the
		// MultiIndexer — so we attach them here. Sorted newest-first
		// so the list reads as "what's connected right now".
		if all := s.sessions.All(); len(all) > 0 {
			now := time.Now()
			rows := make([]MCPSessionStatus, 0, len(all))
			for _, sess := range all {
				if sess == nil {
					continue
				}
				name, version := sess.SnapshotClientInfo()
				row := MCPSessionStatus{
					ID:            sess.ID,
					Cwd:           sess.CWD,
					ClientName:    name,
					ClientVersion: version,
				}
				if !sess.StartedAt.IsZero() {
					row.ConnectedSecs = int64(now.Sub(sess.StartedAt).Seconds())
				}
				rows = append(rows, row)
			}
			st.MCPSessions = rows
		}
		buf, _ := json.Marshal(st)
		return ControlResponse{OK: true, Result: buf}

	case ControlSearchSymbols:
		var p SearchSymbolsParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.SearchSymbols(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal search result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}

	case ControlShutdown:
		if err := s.Controller.Shutdown(ctx); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		return ControlResponse{OK: true}

	case ControlEnrichChurn:
		var p EnrichChurnParams
		if err := unmarshalParams(req.Params, &p); err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		result, err := s.Controller.EnrichChurn(ctx, p)
		if err != nil {
			return controlErr(ErrInternal, err.Error())
		}
		buf, err := json.Marshal(result)
		if err != nil {
			return controlErr(ErrInternal, "marshal enrich_churn result: "+err.Error())
		}
		return ControlResponse{OK: true, Result: buf}
	}
	return controlErr(ErrInternal, "unknown control kind: "+req.Kind)
}

// Shutdown stops the accept loop, closes outstanding connections, and
// removes the socket and PID files. Safe to call multiple times.
func (s *Server) Shutdown() error {
	var first error
	s.doneOnce.Do(func() {
		close(s.shutdown)
		if s.listener != nil {
			first = s.listener.Close()
		}
		// Tear down the HTTP listener with a short grace window so
		// in-flight Streamable responses can finish flushing. We
		// don't propagate the http error unless the unix-socket
		// listener succeeded — the operator already sees a
		// unix-socket close error in the same path.
		if s.httpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if herr := s.httpServer.Shutdown(ctx); herr != nil && first == nil {
				first = herr
			}
			cancel()
		}
		// Close all live conns so per-conn goroutines exit their read loops.
		s.connsMu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.connsMu.Unlock()
		_ = os.Remove(s.SocketPath)
		_ = os.Remove(PIDFilePath())
	})
	return first
}

// writePIDFile fails if a live daemon is already running, so starting
// twice is a loud "already running" error rather than a silent overwrite.
func (s *Server) writePIDFile() error {
	path := PIDFilePath()
	if err := EnsureParentDir(path); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if pid, _ := strconv.Atoi(string(existing)); pid > 0 {
			if platform.ProcessAlive(pid) {
				return fmt.Errorf("daemon already running (pid %d)", pid)
			}
			// Stale pid file — old daemon crashed without cleanup.
			_ = os.Remove(path)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func (s *Server) trackConn(c net.Conn) {
	s.connsMu.Lock()
	s.conns[c] = struct{}{}
	s.connsMu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.connsMu.Lock()
	delete(s.conns, c)
	s.connsMu.Unlock()
}

// Sessions exposes the registry for inspection (status command, tests).
func (s *Server) Sessions() *SessionRegistry { return s.sessions }

// StartedAt returns the time Listen() completed — used for uptime math.
func (s *Server) StartedAt() time.Time { return s.started }

// unmarshalParams decodes RawMessage into a typed struct, treating empty
// or null params as an empty struct (zero value) so callers don't need
// to special-case missing params.
func unmarshalParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

func controlErr(code, msg string) ControlResponse {
	return ControlResponse{ErrorCode: code, ErrorMsg: msg}
}
