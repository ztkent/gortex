package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/zzet/gortex/internal/daemon"
)

// runProxy relays MCP JSON-RPC traffic between stdio (the MCP client) and
// the daemon's Unix socket. Exactly what `gortex mcp` does when it
// detects a running daemon and isn't forced to embedded mode.
//
// Returns (true, nil) when the proxy ran and finished cleanly. Returns
// (false, nil) when the daemon isn't reachable — the caller should fall
// back to embedded mode. Any other error is a real problem.
func runProxy(ctx context.Context) (ran bool, err error) {
	cwd, wdErr := os.Getwd()
	if wdErr != nil {
		return false, fmt.Errorf("cwd: %w", wdErr)
	}
	h := daemon.Handshake{
		Mode:       daemon.ModeMCP,
		CWD:        cwd,
		ClientName: detectClientName(),
	}
	client, err := daemon.Dial(h)
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			return false, nil
		}
		return false, fmt.Errorf("dial daemon: %w", err)
	}
	defer func() { _ = client.Close() }()

	fmt.Fprintf(os.Stderr,
		"[gortex mcp] proxying to daemon (session %s, default_repo=%q)\n",
		client.Ack.SessionID, client.Ack.DefaultRepo)

	// Bidirectional pump:
	//   stdin → socket (MCP requests from the client)
	//   socket → stdout (MCP responses + notifications)
	//
	// We run both on goroutines and exit when either side hits EOF.
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		err := pumpLines(os.Stdin, client.Conn)
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		err := pumpLines(client.Conn, os.Stdout)
		errCh <- err
	}()

	// Wait for first completion; exit on context cancellation too.
	select {
	case pumpErr := <-errCh:
		if pumpErr != nil && !errors.Is(pumpErr, io.EOF) {
			return true, fmt.Errorf("proxy pump: %w", pumpErr)
		}
	case <-ctx.Done():
	}
	_ = client.Close()
	wg.Wait()
	return true, nil
}

// pumpLines copies newline-delimited frames from src to dst. Uses a
// line-aware scanner so partial reads don't split a single MCP message
// between two writes (which would confuse the peer's parser).
func pumpLines(src io.Reader, dst io.Writer) error {
	r := bufio.NewReaderSize(src, 1<<20) // 1 MB — some MCP replies are chunky
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// detectClientName makes a best-effort guess at which MCP client spawned
// us. Purely for the initial handshake telemetry — the authoritative
// answer comes from the MCP `initialize` request's clientInfo, which
// the daemon dispatcher (cmd/gortex/daemon_mcp.go::maybeSnoopInitialize)
// applies once the first frame arrives. The handshake-time guess
// only matters for the few hundred milliseconds before initialize
// reaches us.
//
// Env-var sniffing here favours the actual variables current MCP
// hosts set. Claude Code: CLAUDECODE=1 (current builds set this) plus
// CLAUDE_CODE_ENTRYPOINT=cli|sdk|... Other hosts kept best-effort.
func detectClientName() string {
	switch {
	case os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" || os.Getenv("CLAUDE_CODE_WORKSPACE") != "":
		return "claude-code"
	case os.Getenv("CURSOR_TRACE_ID") != "" || os.Getenv("CURSOR_WORKSPACE") != "":
		return "cursor"
	case os.Getenv("KIRO_WORKSPACE") != "":
		return "kiro"
	case os.Getenv("WINDSURF_WORKSPACE") != "":
		return "windsurf"
	case os.Getenv("CODEX_WORKSPACE") != "":
		return "codex"
	case os.Getenv("VSCODE_PID") != "" || os.Getenv("VSCODE_IPC_HOOK") != "":
		// VS Code with the MCP extension. Coarse — Continue / Cline
		// embedders run inside VS Code too, so this is just a hint
		// until the MCP initialize frame lands and overrides it.
		return "vscode"
	case os.Getenv("ZED_TERM") != "" || os.Getenv("ZED_TERMINAL") != "":
		return "zed"
	}
	return "unknown"
}

// shouldTryProxy returns true when `gortex mcp` should attempt to
// proxy through the daemon before falling back to embedded mode.
//
// The rule: proxy when stdin is a pipe (we were spawned by an MCP
// client) and the user hasn't passed --no-daemon. Users running
// `gortex mcp` in a terminal expect the embedded behavior they've
// always had.
func shouldTryProxy(forceNoDaemon, forceProxy bool) bool {
	if forceNoDaemon {
		return false
	}
	if forceProxy {
		return true
	}
	// Stdin is a character device when it's a terminal. A pipe or socket
	// means we're being fed bytes by a parent process — almost always an
	// MCP client.
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}
