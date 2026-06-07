package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	cwd, wdErr := resolveLaunchCWD()
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

	// Orphan watchdog: if our parent (the MCP client) dies, stdin EOF is the
	// normal shutdown signal — but a client that is SIGKILLed, or whose stdin
	// pipe is inherited and held open elsewhere, can leave this proxy wedged
	// forever, pinning a daemon session. Poll the parent PID and unblock the
	// select when we get reparented (to init or a subreaper).
	orphanCh := make(chan struct{}, 1)
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	go orphanWatch(watchCtx, orphanPollInterval, os.Getppid, func() {
		fmt.Fprintln(os.Stderr, "[gortex mcp] parent process exited; closing proxy")
		select {
		case orphanCh <- struct{}{}:
		default:
		}
	})

	// Wait for first completion; exit on context cancellation or orphaning too.
	select {
	case pumpErr := <-errCh:
		if pumpErr != nil && !errors.Is(pumpErr, io.EOF) {
			return true, fmt.Errorf("proxy pump: %w", pumpErr)
		}
	case <-orphanCh:
	case <-ctx.Done():
	}
	cancelWatch()
	_ = client.Close()
	// Bound the drain: a pump blocked reading a never-closing stdin (the exact
	// orphan case) must not pin shutdown — the process is exiting regardless.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(proxyDrainTimeout):
	}
	return true, nil
}

// orphanPollInterval is how often the proxy checks whether its parent
// process is still alive; proxyDrainTimeout bounds the post-close drain.
// Both are vars so tests can shorten them.
var (
	orphanPollInterval = 5 * time.Second
	proxyDrainTimeout  = 2 * time.Second
)

// orphanWatch polls getppid every interval and invokes onOrphan exactly
// once when the proxy's parent process has gone away — detected as a change
// of parent PID (reparented to init=1 on classic Unix, or to the nearest
// subreaper). Watching for a *change* is strictly more robust than testing
// for PID 1 alone, which misses subreaper reparenting (containers, systemd
// user sessions, a wrapping CLI that calls prctl(PR_SET_CHILD_SUBREAPER)).
// It self-disarms when there is no meaningful parent to watch (orig <= 1),
// and is an inert no-op on platforms that never reparent — there the parent
// PID stays equal to orig for the whole process lifetime.
func orphanWatch(ctx context.Context, interval time.Duration, getppid func() int, onOrphan func()) {
	orig := getppid()
	if orig <= 1 || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if getppid() != orig {
				onOrphan()
				return
			}
		}
	}
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
	case os.Getenv("ANTIGRAVITY_AGENT") != "":
		return "antigravity"
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

// resolveLaunchCWD picks the most plausible project cwd for an MCP
// launch, defending against editors that spawn the MCP server with
// cwd unset or set to a non-project directory:
//
//   - Antigravity sometimes spawns with cwd=`/`.
//   - Cursor launches user-level `~/.cursor/mcp.json` entries with
//     cwd=$HOME (see gortexhq/gortex#19).
//
// Resolution order:
//  1. os.Getwd() when it looks like a project root (not `/` or $HOME).
//  2. $PWD when it differs and isn't `/` or $HOME.
//  3. The first non-empty editor workspace env var (CURSOR_WORKSPACE,
//     CLAUDE_CODE_WORKSPACE, WINDSURF_WORKSPACE, KIRO_WORKSPACE,
//     CODEX_WORKSPACE, ANTIGRAVITY_WORKSPACE, VSCODE_WORKSPACE).
//  4. Fall through to whatever Getwd() returned — the daemon (or the
//     embedded handshake) will surface a clear entry-point error.
func resolveLaunchCWD() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if !isAmbiguousLaunchCWD(cwd) {
		return cwd, nil
	}
	if pwd := os.Getenv("PWD"); pwd != cwd && !isAmbiguousLaunchCWD(pwd) {
		return pwd, nil
	}
	for _, key := range []string{
		"CURSOR_WORKSPACE",
		"CLAUDE_CODE_WORKSPACE",
		"WINDSURF_WORKSPACE",
		"KIRO_WORKSPACE",
		"CODEX_WORKSPACE",
		"ANTIGRAVITY_WORKSPACE",
		"VSCODE_WORKSPACE",
	} {
		if v := os.Getenv(key); !isAmbiguousLaunchCWD(v) {
			return v, nil
		}
	}
	return cwd, nil
}

// isAmbiguousLaunchCWD returns true when `p` is an editor-launch cwd
// we can't trust to point at the active project — empty, `/`, or the
// user's home directory.
//
// The home comparison goes through filepath.EvalSymlinks so the
// macOS `/var → /private/var` redirect (and similar symlinks) don't
// cause a false negative when an editor sets cwd via os.Chdir and
// then Getwd reports the resolved form.
func isAmbiguousLaunchCWD(p string) bool {
	if p == "" || p == "/" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if p == home {
		return true
	}
	resP, errP := filepath.EvalSymlinks(p)
	resH, errH := filepath.EvalSymlinks(home)
	return errP == nil && errH == nil && resP == resH
}

// The former shouldTryProxy stdin-TTY heuristic was removed: `gortex mcp`
// is now daemon-first via resolveDaemonDecision (ensure-daemon → relay,
// with an embedded fallback) regardless of whether stdin is a terminal
// or a pipe.
