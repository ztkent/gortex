package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var (
	daemonDetach     bool
	daemonTail       int
	daemonEmbeddings bool
	// daemonEmbeddingsChanged records whether `--embeddings` was given
	// explicitly on `gortex daemon start`. buildDaemonState reads it
	// (the function has no *cobra.Command of its own) to decide whether
	// the flag overrides the `embedding:` config block. Set once in
	// runDaemonStart before buildDaemonState runs.
	daemonEmbeddingsChanged   bool
	daemonStatusWatch         bool
	daemonStatusInterval      time.Duration
	daemonHTTPAddr            string
	daemonHTTPAuthToken       string
	daemonBackend             string
	daemonBackendPath         string
	daemonBackendBufferPoolMB uint64
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the long-living Gortex daemon",
	Long: `The daemon holds the graph for all tracked repositories and serves every
MCP client (Claude Code, Cursor, Kiro, ...) plus the CLI from one shared
index.

If no daemon is running, ` + "`gortex mcp`" + ` still works standalone — the daemon
is additive, not required.`,
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	RunE:  runDaemonStart,
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon gracefully (waits for final snapshot)",
	RunE:  runDaemonStop,
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop and start the daemon (preserves tracked repos)",
	RunE:  runDaemonRestart,
}

var daemonReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Re-read config and pick up new or removed repos without restart",
	RunE:  runDaemonReload,
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon PID, uptime, tracked repos, memory, sessions",
	RunE:  runDaemonStatus,
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the daemon log file",
	RunE:  runDaemonLogs,
}

func init() {
	daemonStartCmd.Flags().BoolVar(&daemonDetach, "detach", false,
		"fork to background after starting (logs to the daemon log file — see `gortex daemon logs`)")
	daemonStartCmd.Flags().BoolVar(&daemonEmbeddings, "embeddings", false,
		"load a semantic embedding provider (opt-in — adds ~87 MB model download on first use and ~60 ms/symbol warmup)")
	daemonStartCmd.Flags().StringVar(&daemonHTTPAddr, "http-addr", "",
		"also expose the MCP 2026 Streamable HTTP transport on this TCP address (e.g. 127.0.0.1:7411); empty disables")
	daemonStartCmd.Flags().StringVar(&daemonHTTPAuthToken, "http-auth-token", "",
		"bearer token required on every Streamable HTTP request (default: read $GORTEX_DAEMON_HTTP_TOKEN; empty allows unauthenticated localhost binds)")
	daemonStartCmd.Flags().StringVar(&daemonBackend, "backend", "sqlite",
		"storage backend: sqlite (default — pure-Go embedded SQL, persists to --backend-path so warm restarts skip re-indexing) | memory (in-process, no persistence — fastest per-op but pays the full cold-warmup cost on every restart)")
	daemonStartCmd.Flags().StringVar(&daemonBackendPath, "backend-path", "",
		"directory where the on-disk backend persists its store. Required when --backend != memory. Default: ~/.gortex/store/<backend>.store")
	daemonStartCmd.Flags().Uint64Var(&daemonBackendBufferPoolMB, "backend-buffer-pool-mb", 0,
		"advisory page-cache cap (MiB) for on-disk backends. 0 reads $GORTEX_DAEMON_BUFFER_POOL_MB or lets the backend choose its own default; backends that manage their own cache (e.g. sqlite) ignore it")
	daemonLogsCmd.Flags().IntVarP(&daemonTail, "tail", "n", 50,
		"show only the last N log lines")
	daemonStatusCmd.Flags().BoolVarP(&daemonStatusWatch, "watch", "w", false,
		"continuously refresh the status until interrupted (alt-screen buffer)")
	daemonStatusCmd.Flags().DurationVar(&daemonStatusInterval, "interval", 2*time.Second,
		"refresh interval in --watch mode (clamped to >=200ms)")

	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonReloadCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonLogsCmd)
	rootCmd.AddCommand(daemonCmd)
}

// runDaemonStart starts the daemon in foreground (default) or detached
// (when --detach is passed). Detach does a self-exec: re-runs this binary
// with GORTEX_DAEMON_CHILD=1 set, which the inner exec picks up and runs
// the actual serve loop.
func runDaemonStart(cmd *cobra.Command, _ []string) error {
	if daemon.IsRunning() {
		return fmt.Errorf("daemon already running (socket: %s)", daemon.SocketPath())
	}
	// IsRunning only probes the socket. A daemon that is mid-shutdown — or
	// one whose socket wedged — still owns the PID file and, crucially, still
	// holds the store's on-disk lock. Starting over the top of it makes the
	// backend open fail with an opaque "failed to open database" lock
	// conflict, so refuse early with the PID and an actionable next step. The
	// detached child reaches here too, but it hasn't written its own PID file
	// yet (that happens in the serve loop), so this can't false-positive on
	// the daemon we're in the middle of starting.
	if pid, ok := daemon.RunningPID(); ok {
		return fmt.Errorf("daemon already running (pid %d) — stop it with `gortex daemon stop`, or use `gortex daemon restart`", pid)
	}
	if daemonDetach && os.Getenv("GORTEX_DAEMON_CHILD") != "1" {
		return spawnDetachedDaemon()
	}
	logger := newLogger()

	// Raise the per-process file-descriptor cap as early as possible.
	// fsnotify holds one FD per watched directory on Linux and one FD
	// per directory plus every file inside it on macOS, so a multi-repo
	// install easily blows past the inherited soft cap (256 on macOS,
	// 1024 on most Linuxes) and surfaces as "accept: too many open
	// files" once the daemon is hot.
	if fdl, err := daemon.RaiseFDLimit(); err != nil {
		logger.Warn("daemon: could not raise file-descriptor limit", zap.Error(err))
	} else {
		logger.Info("daemon: file-descriptor cap",
			zap.Uint64("soft", fdl.Soft), zap.Uint64("hard", fdl.Hard))
	}

	srv := daemon.New(daemon.SocketPath(), canonicalVersion(), logger)

	// Record whether `--embeddings` was set explicitly so
	// buildDaemonState can let it override the `embedding:` config
	// block. With `--detach` the re-exec'd child sees no flags, so an
	// explicit opt-in there must travel via GORTEX_EMBEDDINGS instead.
	daemonEmbeddingsChanged = cmd.Flags().Changed("embeddings")

	// Fast path: snapshot load + indexer + MCP server wiring. The
	// per-repo TrackRepoCtx loop and MultiWatcher init are deferred to
	// warmupDaemonState below so the socket opens immediately instead
	// of waiting 30–60s for contract re-extraction across every tracked
	// repo.
	state, err := buildDaemonState(logger)
	if err != nil {
		return fmt.Errorf("build daemon state: %w", err)
	}

	controller := &realController{
		graph:         state.graph,
		indexer:       state.indexer,
		multiIndexer:  state.multiIndexer,
		configManager: state.configManager,
		logger:        logger,
	}
	controller.onShutdown = func() error {
		// Stop watchers first so no late events race the snapshot
		// write — we want the snapshot to reflect a quiescent graph,
		// not one being mutated by an in-flight re-index.
		controller.mu.Lock()
		mw := controller.multiWatcher
		controller.mu.Unlock()
		if mw != nil {
			_ = mw.Stop()
		}
		if mg, ok := state.graph.(*graph.Graph); ok {
			// Memory backend — snapshot the full in-memory graph;
			// the next warmup replays nodes/edges from the gob+gzip
			// dump because there's no other persistence layer.
			saveSnapshot(mg, collectSnapshotRepos(state.multiIndexer), collectSnapshotContracts(state.multiIndexer), collectSnapshotVector(state.multiIndexer), version, logger)
		}
		// Persistent backends (sqlite) no longer write a metadata
		// snapshot: per-file mtimes live in the FileMtime sidecar
		// table, contract records ride on KindContract.Meta, and the
		// vector index is persisted by the backend itself. Warm
		// restart reads everything it needs from the on-disk store —
		// no gob+gzip round-trip required.
		// Run the shared stack's teardown chain — flushes the savings
		// store and closes the backend handle (checkpointing the sqlite
		// WAL) so the daemon shuts down cleanly.
		if state.shared != nil {
			_ = state.shared.Close()
		} else if state.mcpServer != nil {
			_ = state.mcpServer.FlushSavings()
		}
		return nil
	}
	srv.Controller = controller
	disp := newMCPDispatcher(state.mcpServer, state.multiIndexer, logger)
	// The local executor + the dispatcher's SetRouter are handed to the
	// controller so ControlProxy can build/publish/tear-down the router
	// live (gortex proxy on/off/add/remove) without a daemon restart.
	localExec := newLocalToolExecutor(state.mcpServer, logger)
	controller.localExecute = localExec
	controller.publishRouter = disp.SetRouter
	// Wire the multi-server router into the daemon dispatcher when
	// servers.toml exists. Local-only
	// daemons (no servers.toml) leave router=nil and dispatch flows
	// straight to the in-process MCP server unchanged.
	if scfg, scfgErr := daemon.LoadServersConfig(""); scfgErr == nil && scfg != nil && len(scfg.Server) > 0 {
		rosters := daemon.NewWorkspaceRosterCache(60 * time.Second)
		// Local identity is a reserved sentinel, never DefaultServer().Slug:
		// a remote marked default=true must still be proxied to, not
		// treated as the daemon's own graph.
		router := daemon.NewRouter(daemon.RouterConfig{
			Servers:      scfg,
			Rosters:      rosters,
			LocalSlug:    daemon.LocalServerSentinel,
			LocalExecute: localExec,
			Logger:       logger,
			Federation:   resolveFederationConfig(),
		})
		disp.SetRouter(router)
		controller.liveRouter = router
		logger.Info("daemon: multi-server router wired",
			zap.Int("servers", len(scfg.Server)), zap.String("local_slug", daemon.LocalServerSentinel))
	} else if scfgErr != nil {
		logger.Warn("daemon: servers.toml load error (running single-server)", zap.Error(scfgErr))
	}
	// Bridge the session proxy-toggle MCP tools to the daemon's
	// per-session overrides + the live router roster. The router
	// accessor is dynamic so a ControlProxy swap is reflected.
	if state.mcpServer != nil {
		state.mcpServer.SetRemoteOverrideSink(&sessionRemoteOverrideSink{
			sessions: srv.Sessions(),
			router:   disp.Router,
		})
	}
	srv.MCPDispatcher = disp

	// Optional MCP 2026 Streamable HTTP transport. Off by default
	// (--http-addr unset) so a fresh `gortex daemon start` keeps
	// the unix-socket-only behaviour every existing client already
	// expects. When set, the daemon mounts /mcp on the supplied
	// TCP address using the in-process streamable.Transport;
	// per-request session state is replayed out of an in-memory
	// store so multiple workers / reverse-proxies could later share
	// it. The auth token is mandatory for non-localhost binds —
	// exposing an unauthenticated MCP server on an external
	// interface is a footgun, not a feature.
	if daemonHTTPAddr != "" {
		token := daemonHTTPAuthToken
		if token == "" {
			token = os.Getenv("GORTEX_DAEMON_HTTP_TOKEN")
		}
		if !isLocalhostBind(daemonHTTPAddr) && token == "" {
			return fmt.Errorf("--http-addr %q is non-localhost; --http-auth-token (or $GORTEX_DAEMON_HTTP_TOKEN) is required", daemonHTTPAddr)
		}
		// Router was already wired into the dispatcher above; reuse
		// it here so the streamable transport sees the same proxy
		// fan-out for cross-workspace tool calls.
		var router *daemon.Router
		if r := disp.Router(); r != nil {
			router = r
		}
		srv.HTTPHandler = buildDaemonStreamableHandler(disp, srv.Sessions(), router, logger, token)
		srv.HTTPAddr = daemonHTTPAddr
		logger.Info("daemon: streamable HTTP transport configured",
			zap.String("addr", daemonHTTPAddr),
			zap.Bool("authenticated", token != ""))
	}

	// Opt-in pprof endpoint. No-op unless GORTEX_DAEMON_PPROF_ADDR is
	// set — keeps profiling off by default so the daemon doesn't hand
	// its heap to anything on localhost.
	startPProfIfEnabled(logger)

	// Wire the daemon-health snapshot fn into the MCP server's
	// healthBroadcaster. Captures the live controller, daemon
	// server (for session count), and multi indexer so every periodic
	// tick reflects current state. Stopped in the deferred shutdown
	// below so the ticker goroutine doesn't outlive the process.
	daemonStart := time.Now()
	if state.mcpServer != nil {
		srvCapture := srv
		stateCapture := state
		controllerCapture := controller
		state.mcpServer.AttachHealthSnapshot(func() map[string]any {
			out := map[string]any{
				"uptime_seconds": int64(time.Since(daemonStart).Seconds()),
				"ready":          controllerCapture.IsReady(),
				"warmup_seconds": int64(0),
			}
			if st, statusErr := controllerCapture.Status(context.Background()); statusErr == nil {
				out["warmup_seconds"] = st.WarmupSeconds
				out["tracked_repos"] = len(st.TrackedRepos)
				out["alloc_bytes"] = st.Runtime.Alloc
				out["sys_bytes"] = st.Runtime.Sys
				out["heap_inuse_bytes"] = st.Runtime.HeapInuse
				out["num_goroutine"] = st.Runtime.NumGoroutine
				out["num_gc"] = st.Runtime.NumGC
				if st.LSPRouter != nil {
					out["lsp_alive"] = len(st.LSPRouter.ActiveProviders)
					out["lsp_specs_registered"] = len(st.LSPRouter.EnabledSpecs)
				}
			}
			if sessions := srvCapture.Sessions(); sessions != nil {
				out["sessions"] = sessions.Count()
			}
			if stateCapture.graph != nil {
				out["graph_nodes"] = stateCapture.graph.NodeCount()
				out["graph_edges"] = stateCapture.graph.EdgeCount()
			}
			return out
		})
	}
	defer func() {
		if state.mcpServer != nil {
			state.mcpServer.StopHealthBroadcaster()
		}
	}()

	// Initial workspace_readiness phase — the snapshot has been
	// loaded but warmup hasn't started yet.
	publishReadinessPhase(state, "snapshot_loaded", false, map[string]any{
		"snapshot_repos": len(state.snapshotRepos),
	})

	// Periodic snapshots — 10 minute interval, gated on warmup-complete.
	// On a crash we lose at most one interval's worth of work, which is
	// acceptable given snapshot writes are atomic (tmp → rename) and can
	// never leave a truncated file on disk.
	//
	// Gating: a snapshot walks the whole graph (AllNodes + AllEdges) and
	// holds shard RLocks for the duration. While the daemon is still
	// warming up the parser worker pool is concurrently writing those
	// shards via AddBatch, so an unsynchronised snapshot tick steals
	// graph-lock budget from the work that needs to finish first and
	// pulls millions of node/edge pointers into a live allocation set
	// the GC then has to clean up. Skipping snapshots until ready cleared
	// a stall observed in profile #5 where saveSnapshotTo was the only
	// runnable goroutine on a daemon mid-warmup.
	// Periodic snapshots fire only for the memory backend — that's
	// the path that has no other persistence layer for the graph
	// itself. Persistent backends (sqlite) rely on the backend's own
	// durability (graph + FileMtimes + contracts + vectors all live
	// on disk) so the gob+gzip snapshot is dead weight in that mode.
	stopSnapshotter := func() {}
	if mg, ok := state.graph.(*graph.Graph); ok {
		stopSnapshotter = startPeriodicSnapshots(mg, state.multiIndexer, version, 10*time.Minute, controller.IsReady, logger)
	}
	defer stopSnapshotter()

	// Periodic savings flush — 5 minute interval. Bounds on-crash data
	// loss for the savings counters even when the call rate is too low
	// to trip the every-N-observations flush. No-op when persistence
	// isn't wired (e.g. cache dir unavailable).
	stopSavingsFlush := state.mcpServer.StartPeriodicSavingsFlush(5 * time.Minute)
	defer stopSavingsFlush()

	// Periodic reconciliation — the "janitor". Walks each tracked repo
	// and runs IncrementalReindex to evict files deleted offline and
	// re-index files whose mtime changed. Insurance against gaps in
	// fsnotify coverage (inotify watch limits, NFS mounts, kernel
	// event-queue overflow). Default interval 1 h; override via
	// GORTEX_RECONCILE_INTERVAL (a Go duration string, e.g. "15m").
	// Set to "0" to disable.
	stopJanitor := startReconcileJanitor(state.multiIndexer, reconcileInterval(), logger)
	defer stopJanitor()

	if err := srv.Listen(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"[gortex daemon] listening on %s (pid %d)\n",
		daemon.SocketPath(), os.Getpid())

	// Warmup runs in the background: re-index tracked repos, extract
	// contracts, attach file watchers. The daemon is already reachable
	// on the socket at this point, so clients can connect and start
	// issuing queries while this work continues. Queries against
	// not-yet-re-indexed repos still hit the snapshot data loaded in
	// buildDaemonState — they just won't reflect files that changed
	// since the snapshot was written until warmup gets to that repo.
	go func() {
		start := time.Now()
		logger.Info("daemon: warmup starting")
		mw := warmupDaemonState(state, logger)
		controller.AttachWatcher(mw)
		// Wire the daemon's MultiWatcher into the per-server history
		// surface so `get_recent_changes` and `get_symbol_history` see
		// real events under the daemon. Without this the tools always
		// reported "watch mode is not active" even though MultiWatcher
		// was actively re-indexing changed files.
		if state.mcpServer != nil && mw != nil {
			state.mcpServer.SetWatcher(mw)
		}
		// Community detection and process discovery only run when a
		// repo is tracked or indexed via MCP — a daemon coming up off
		// a snapshot never triggers them. Fire once here so
		// get_communities / get_processes / dashboards reflect the
		// loaded graph instead of returning "run index_repository
		// first" against a fully populated state.
		if state.mcpServer != nil {
			state.mcpServer.RunAnalysis()
			// Co-change pre-warm: fire the git-history mine in the
			// background so the first user-visible
			// find_co_changing_symbols / search-rerank call sees a
			// populated cache. On a persistent backend the mine is
			// dominated by the AllNodes + per-pair AddEdge disk-persist
			// step that mineCoChange already defers into its own
			// goroutine — but even the git log itself can take 10–30s
			// on a large history, and we want that off every request
			// path.
			state.mcpServer.PrewarmCoChange()
		}
		elapsed := time.Since(start)
		controller.MarkReady(elapsed)
		logger.Info("daemon: ready", zap.Duration("warmup", elapsed))
		publishReadinessPhase(state, "ready", true, map[string]any{
			"warmup_seconds": int64(elapsed.Seconds()),
			"warmup_ms":      elapsed.Milliseconds(),
		})
	}()

	return srv.Serve()
}

// startPeriodicSnapshots kicks off a goroutine that writes a snapshot on
// every tick. Returns a stop function the caller runs at shutdown. The
// final snapshot on shutdown is handled by onShutdown — this loop only
// covers the "crash resilience" case (interval loss vs full re-index).
// reconcileInterval returns the janitor tick interval, defaulting to 1
// hour. GORTEX_RECONCILE_INTERVAL overrides; "0" or "off" disables the
// janitor entirely (returns 0, which startReconcileJanitor treats as
// a no-op). Malformed values fall back to the default with a warning
// handled by the caller via the zero-return sentinel behaviour.
func reconcileInterval() time.Duration {
	raw := os.Getenv("GORTEX_RECONCILE_INTERVAL")
	if raw == "" {
		return time.Hour
	}
	if raw == "0" || raw == "off" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return time.Hour
	}
	return d
}

// startReconcileJanitor launches a background goroutine that, on every
// interval tick, garbage-collects the index of any linked git worktree
// whose root directory has vanished from disk and then calls
// MultiIndexer.ReconcileAll. interval=0 is a no-op; the returned stop
// function can be called unconditionally.
//
// The worktree GC runs *before* ReconcileAll on purpose: a removed
// worktree's root no longer exists, so ReconcileAll's IncrementalReindex
// would only error on the missing path without evicting anything.
// Pruning the vanished worktrees first keeps the reconcile sweep
// working on live repos and stops a deleted worktree's snapshot slot
// and graph nodes from leaking forever.
func startReconcileJanitor(mi *indexer.MultiIndexer, interval time.Duration, logger *zap.Logger) func() {
	if mi == nil || interval <= 0 {
		logger.Info("daemon: reconcile janitor disabled")
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		logger.Info("daemon: reconcile janitor running", zap.Duration("interval", interval))
		for {
			select {
			case <-t.C:
				if gced := mi.GCVanishedWorktrees(); len(gced) > 0 {
					logger.Info("janitor: pruned vanished worktrees",
						zap.Int("count", len(gced)))
				}
				mi.ReconcileAll()
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

func startPeriodicSnapshots(g *graph.Graph, mi *indexer.MultiIndexer, version string, interval time.Duration, isReady func() bool, logger *zap.Logger) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// Skip while the daemon is still warming up — the
				// graph walk inside saveSnapshot would fight the
				// parser worker pool for shard locks and pin a
				// transient allocation set the GC then has to drain.
				// The next tick after warmup completes catches up.
				if isReady != nil && !isReady() {
					logger.Debug("snapshot: skipped tick — daemon still warming up")
					continue
				}
				saveSnapshot(g, collectSnapshotRepos(mi), collectSnapshotContracts(mi), collectSnapshotVector(mi), version, logger)
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// spawnDetachedDaemon re-invokes the binary with GORTEX_DAEMON_CHILD=1
// set, the log redirected to the daemon log file, and the child
// parented to init. Parent exits as soon as the child has the socket up.
//
// On a TTY the parent shows a mesh-spinner banner and a styled "ready" card
// once the socket is live. On a non-TTY (CI scripts, automation) we keep the
// historical one-line "[gortex daemon] detached (pid X, log: Y)" message so
// existing parsers don't break.
func spawnDetachedDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := daemon.EnsureParentDir(daemon.LogFilePath()); err != nil {
		return err
	}
	logFile, err := os.OpenFile(daemon.LogFilePath(),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	child := exec.Command(exe, "daemon", "start")
	child.Env = append(os.Environ(), "GORTEX_DAEMON_CHILD=1")
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	// Detach the child from the parent's controlling terminal /
	// console so Ctrl-C on the parent doesn't kill the daemon.
	child.SysProcAttr = platform.DetachSysProcAttr()
	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("spawn daemon: %w", err)
	}
	// Don't block on the child — it's detached and inherits the log
	// file handle. Reap it in a background goroutine so a crash during
	// startup surfaces on `exited` instead of stalling the poll loop.
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	emitDaemonStartBanner(os.Stderr)
	sp := newDaemonSpawnSpinner(os.Stderr)
	if sp != nil {
		sp.Start("Waiting for daemon socket")
	}

	// Wait until the socket is live or a timeout hits, so we fail fast
	// if the child died on startup. The socket opens after buildDaemonState
	// decodes the snapshot; on a multi-hundred-MB snapshot that decode
	// can take 10–20 s, so 5 s used to time out a perfectly healthy
	// daemon mid-load. 60 s comfortably covers ~1 GiB snapshots while
	// still failing fast on a child that crashed outright (those die
	// in well under a second).
	start := time.Now()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.IsRunning() {
			elapsed := time.Since(start).Truncate(10 * time.Millisecond)
			if sp != nil {
				sp.Set("", fmt.Sprintf("socket up · %s", elapsed))
				sp.Done()
				emitDaemonStartSummary(os.Stderr, child.Process.Pid, elapsed)
			} else {
				fmt.Fprintf(os.Stderr, "[gortex daemon] detached (pid %d, log: %s)\n",
					child.Process.Pid, daemon.LogFilePath())
			}
			return nil
		}
		// Bail out early if the child has already exited — no point
		// waiting another 59 seconds for a corpse.
		select {
		case werr := <-exited:
			failMsg := fmt.Errorf("daemon exited during startup (%v); check %s",
				werr, daemon.LogFilePath())
			if sp != nil {
				sp.Fail(failMsg)
			}
			return failMsg
		default:
		}
		if sp != nil {
			sp.Set("", fmt.Sprintf("snapshot decoding · %s", time.Since(start).Truncate(100*time.Millisecond)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	timeoutErr := fmt.Errorf("daemon did not come up within 60s; check %s", daemon.LogFilePath())
	if sp != nil {
		sp.Fail(timeoutErr)
	}
	return timeoutErr
}

// newDaemonSpawnSpinner returns a spinner bound to w when it's a TTY (and the
// global --no-progress flag isn't set). Returns nil otherwise, so callers can
// branch on the spinner's presence to choose between the framed-card vs.
// one-line output paths.
func newDaemonSpawnSpinner(w io.Writer) *progress.Spinner {
	if noProgress || !progress.IsTTY(w) {
		return nil
	}
	return progress.NewSpinner(w)
}

// emitDaemonStartBanner prints the gortex mesh banner + subtitle for the
// detach flow. Only fires on a TTY — non-TTY callers stay quiet so script
// stderr remains parseable.
func emitDaemonStartBanner(w io.Writer) {
	if !progress.IsTTY(w) || noProgress || daemonRestartActive {
		return
	}
	banner := tui.Banner{
		Title:    "gortex daemon start",
		Subtitle: "Spawning daemon in the background.",
	}.Render()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, banner)
	_, _ = fmt.Fprintln(w)
}

// emitDaemonStartSummary prints the post-spawn card showing pid, socket, log
// path, elapsed time, and useful next-step hints. Only fires on a TTY.
func emitDaemonStartSummary(w io.Writer, pid int, elapsed time.Duration) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	stats := []string{
		progress.Stat(fmt.Sprintf("%d", pid), "pid", progress.StatGood),
		progress.Stat(elapsed.String(), "boot", progress.StatGood),
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("daemon ready"))
	_, _ = fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	_, _ = fmt.Fprintln(w, "     "+progress.Row("socket", daemon.SocketPath(), 8))
	_, _ = fmt.Fprintln(w, "     "+progress.Row("log", daemon.LogFilePath(), 8))
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "     "+progress.Heading("next"))
	_, _ = fmt.Fprintln(w, "     "+progress.Step(1, "track a repo:    gortex track <path>"))
	_, _ = fmt.Fprintln(w, "     "+progress.Step(2, "watch status:    gortex daemon status --watch"))
	_, _ = fmt.Fprintln(w, "     "+progress.Step(3, "shut down:       gortex daemon stop"))
	_, _ = fmt.Fprintln(w)
}

func runDaemonStop(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()
	if !daemon.IsRunning() {
		// The socket is gone, but a process may still be alive and holding
		// the store lock — a daemon mid-shutdown, or one whose socket wedged.
		// killByPID terminates it AND blocks until it has actually exited,
		// which is what `daemon restart` relies on to not race the lock.
		if _, ok := daemon.RunningPID(); ok {
			return killByPID()
		}
		emitDaemonStopAlreadyDown(w)
		return nil
	}

	// Capture uptime + socket *before* shutdown so we can show them in the
	// post-stop summary (the socket file vanishes on clean shutdown).
	socket := daemon.SocketPath()
	uptime := daemonUptimeBeforeStop()
	// Capture the PID too. ControlShutdown only *acks* — the daemon then
	// flushes and closes the store (releasing its on-disk lock) and exits
	// asynchronously (see server.go: the handler Shutdown()s ~100ms later in
	// a goroutine). We must block until that process is gone, or a following
	// `daemon start` races the still-held lock and dies with the opaque
	// "failed to open database with status 1".
	pid, havePID := daemon.RunningPID()

	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		// Daemon said it was alive but won't talk — probably a stale PID file
		// the daemon hasn't cleaned up. Fall back to killing by PID.
		return killByPID()
	}
	resp, err := c.Control(daemon.ControlShutdown, nil)
	_ = c.Close()
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("shutdown rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	if havePID {
		waitForDaemonExit(pid)
	}
	emitDaemonStopSummary(w, socket, uptime)
	return nil
}

// waitForDaemonExit blocks until the daemon process pid has exited — and thus
// released the store's on-disk lock — force-killing it if a graceful shutdown
// stalls. This is what makes `daemon stop` honest: when it returns, the store
// is free for the next process, which is the foundation `daemon restart`
// stands on. Polls cheaply; the common case (a clean flush) clears in well
// under a second.
func waitForDaemonExit(pid int) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !platform.ProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Graceful shutdown stalled (e.g. a wedged cgo call). Don't leave a
	// half-exited daemon clutching the lock — force it, then clean up the
	// socket/PID so the next start isn't tripped by stale files.
	fmt.Fprintln(os.Stderr, "[gortex daemon] graceful shutdown timed out — force-killing")
	_ = platform.KillProcess(pid)
	for i := 0; i < 60 && platform.ProcessAlive(pid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(daemon.PIDFilePath())
	_ = os.Remove(daemon.SocketPath())
}

// daemonUptimeBeforeStop best-effort-fetches the daemon's reported uptime via
// a Status control before shutdown so the summary card can show how long the
// process ran. Returns 0 on any error — we'd rather degrade the card than
// fail the stop.
func daemonUptimeBeforeStop() time.Duration {
	c, err := daemonControlClient()
	if err != nil {
		return 0
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlStatus, nil)
	if err != nil || !resp.OK {
		return 0
	}
	var st daemon.StatusResponse
	if jerr := json.Unmarshal(resp.Result, &st); jerr != nil {
		return 0
	}
	return time.Duration(st.UptimeSeconds) * time.Second
}

// emitDaemonStopAlreadyDown prints the "not running" message: a one-liner on
// non-TTY for script compat, a styled hint card on TTY.
func emitDaemonStopAlreadyDown(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		_, _ = fmt.Fprintln(w, "[gortex daemon] not running")
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "  "+progress.StyleHint.Render("◌  daemon was not running — nothing to stop"))
	_, _ = fmt.Fprintln(w, "     "+progress.Caption("start with `gortex daemon start --detach`"))
	_, _ = fmt.Fprintln(w)
}

// emitDaemonStopSummary prints the post-shutdown banner + result card mirroring
// daemon start's surface: uptime + socket path so the user can confirm the
// right daemon went down.
func emitDaemonStopSummary(w io.Writer, socket string, uptime time.Duration) {
	if !progress.IsTTY(w) || noProgress {
		_, _ = fmt.Fprintln(w, "[gortex daemon] stopped")
		return
	}
	if !daemonRestartActive {
		banner := tui.Banner{
			Title:    "gortex daemon stop",
			Subtitle: "Daemon shut down cleanly.",
		}.Render()
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, banner)
	}
	stats := []string{progress.Stat("clean shutdown", "", progress.StatGood)}
	if uptime > 0 {
		stats = append(stats, progress.Stat(uptime.Truncate(time.Second).String(), "uptime", progress.StatNeutral))
	}
	_, _ = fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("stopped"))
	_, _ = fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	if socket != "" {
		_, _ = fmt.Fprintln(w, "     "+progress.Row("socket", socket+" (removed)", 8))
	}
	_, _ = fmt.Fprintln(w)
}

// daemonRestartActive flips on for the duration of runDaemonRestart so the
// inner stop / start emit functions skip their own banners — restart shows the
// logo once at the top and then lists the stop + start cards underneath.
var daemonRestartActive bool

func runDaemonRestart(cmd *cobra.Command, args []string) error {
	daemonRestartActive = true
	defer func() { daemonRestartActive = false }()

	emitDaemonRestartBanner(cmd.ErrOrStderr())

	// Stop is idempotent when not running and now blocks until the old
	// process has fully exited — releasing the store's on-disk lock — before
	// returning. That's what lets the start below reuse the store without
	// racing the lock. The old code polled `daemon.IsRunning()` here, which
	// watched the wrong resource: the socket is torn down ~100ms after the
	// shutdown ack, long before the process exits and the lock clears, so the
	// poll fell through early and the restart died on "failed to open
	// database with status 1".
	if err := runDaemonStop(cmd, args); err != nil {
		return err
	}
	daemonDetach = true
	return runDaemonStart(cmd, args)
}

// emitDaemonRestartBanner prints the unified header for `gortex daemon
// restart` so the user sees the mesh logo once instead of twice.
func emitDaemonRestartBanner(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	banner := tui.Banner{
		Title:    "gortex daemon restart",
		Subtitle: "Cycling daemon: stop then start.",
	}.Render()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, banner)
	_, _ = fmt.Fprintln(w)
}

func runDaemonReload(_ *cobra.Command, _ []string) error {
	c, err := daemonControlClient()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlReload, nil)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("reload rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	_, _ = fmt.Fprintln(os.Stderr, "[gortex daemon] reloaded")
	return nil
}

func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	if daemonStatusWatch {
		return runDaemonStatusWatch(cmd)
	}
	st, err := fetchDaemonStatusForCLI()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	renderDaemonHeader(w, st)
	renderDaemonWorkspaces(w, st)
	renderDaemonRepos(w, st)
	renderDaemonSessions(w, st)
	renderDaemonServers(w, st)
	return nil
}

// fetchDaemonStatusForCLI dials the control socket once and returns a parsed
// StatusResponse. Shared by the one-shot and watch paths.
func fetchDaemonStatusForCLI() (daemon.StatusResponse, error) {
	var st daemon.StatusResponse
	c, err := daemonControlClient()
	if err != nil {
		return st, err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Control(daemon.ControlStatus, nil)
	if err != nil {
		return st, err
	}
	if !resp.OK {
		return st, fmt.Errorf("status rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return st, fmt.Errorf("parse status: %w", err)
	}
	return st, nil
}

// renderDaemonHeader writes the fixed-schema key/value facts about the
// daemon process as a borderless two-column table.
func renderDaemonHeader(w io.Writer, st daemon.StatusResponse) {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false
	t.Style().Options.SeparateHeader = false
	t.Style().Options.SeparateRows = false
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft, WidthMax: 12},
		{Number: 2, Align: text.AlignLeft},
	})
	t.AppendRow(table.Row{"daemon", st.Version})
	t.AppendRow(table.Row{"pid", st.PID})
	t.AppendRow(table.Row{"socket", st.SocketPath})
	t.AppendRow(table.Row{"uptime", formatDuration(time.Duration(st.UptimeSeconds) * time.Second)})
	if st.Ready {
		t.AppendRow(table.Row{
			"state",
			fmt.Sprintf("ready (warmup %s)", formatDuration(time.Duration(st.WarmupSeconds)*time.Second)),
		})
	} else {
		t.AppendRow(table.Row{"state", "warming up (socket reachable, background re-index in progress)"})
	}
	t.AppendRow(table.Row{"sessions", st.Sessions})
	if st.MemoryBytes > 0 {
		t.AppendRow(table.Row{"memory", formatBytes(st.MemoryBytes)})
	}
	if sb := st.SearchBackend; sb.Name != "" {
		switch {
		case sb.DiskPath != "":
			t.AppendRow(table.Row{"search", fmt.Sprintf(
				"%s  docs=%d  heap=%s  disk=%s  path=%s",
				sb.Name, sb.DocCount, formatBytes(sb.Bytes),
				formatBytes(sb.DiskBytes), sb.DiskPath)})
		default:
			t.AppendRow(table.Row{"search", fmt.Sprintf(
				"%s  docs=%d  heap=%s", sb.Name, sb.DocCount, formatBytes(sb.Bytes))})
		}
	}
	rt := st.Runtime
	if rt.Sys > 0 {
		t.AppendRow(table.Row{"runtime", fmt.Sprintf(
			"alloc=%s  sys=%s  heap_inuse=%s  heap_idle=%s  heap_released=%s  stacks=%s  gc=%d  goroutines=%d",
			formatBytes(rt.Alloc),
			formatBytes(rt.Sys),
			formatBytes(rt.HeapInuse),
			formatBytes(rt.HeapIdle),
			formatBytes(rt.HeapReleased),
			formatBytes(rt.StackInuse),
			rt.NumGC,
			rt.NumGoroutine,
		)})
	}
	if st.PProfAddr != "" {
		t.AppendRow(table.Row{"pprof", fmt.Sprintf(
			"http://%s/debug/pprof/  (example: go tool pprof -http=: http://%s/debug/pprof/heap)",
			st.PProfAddr, st.PProfAddr)})
	}
	t.Render()
}

// renderDaemonRepos writes the per-repo breakdown as a single table.
// Rows sort by attributed memory descending so the largest consumers
// appear first. An "other" row at the bottom covers the delta between
// process-total memory and the sum of attributed per-repo memory —
// embedder model weights, runtime heap headroom, semantic caches, etc.
func renderDaemonRepos(w io.Writer, st daemon.StatusResponse) {
	if len(st.TrackedRepos) == 0 {
		_, _ = fmt.Fprintln(w, "\ntracked repos: (none)")
		return
	}

	rows := make([]daemon.TrackedRepoStatus, len(st.TrackedRepos))
	copy(rows, st.TrackedRepos)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Memory.TotalBytes > rows[j].Memory.TotalBytes
	})

	// The disk_b column only appears when any repo actually has disk
	// usage — i.e. Bleve is running in disk mode. Keeping it
	// conditional stops the default in-memory output from carrying a
	// dead column users would (rightly) ask about.
	showDisk := false
	for _, r := range rows {
		if r.Memory.DiskBytes > 0 {
			showDisk = true
			break
		}
	}

	_, _ = fmt.Fprintln(w, "\ntracked repos:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.Style().Format.Footer = text.FormatDefault

	// The workspace column only adds signal when at least one repo
	// declares an explicit workspace (i.e. one that doesn't equal the
	// repo prefix). Pure-default setups already see the prefix in
	// column 1; printing the same string twice is just noise.
	showWS := false
	for _, r := range rows {
		if r.Workspace != "" && r.Workspace != r.Prefix {
			showWS = true
			break
		}
	}

	header := table.Row{"repo"}
	colConfigs := []table.ColumnConfig{{Number: 1, Align: text.AlignLeft}}
	if showWS {
		header = append(header, "workspace")
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignLeft})
	}
	header = append(header, "total", "files", "nodes", "edges",
		"nodes_b", "edges_b", "search_b", "vectors_b")
	for i := 0; i < 8; i++ {
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignRight})
	}
	if showDisk {
		header = append(header, "disk_b")
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignRight})
	}
	header = append(header, "path")
	colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignLeft})
	t.AppendHeader(header)
	t.SetColumnConfigs(colConfigs)

	var attributed uint64
	for _, r := range rows {
		attributed += r.Memory.TotalBytes
		row := table.Row{r.Prefix}
		if showWS {
			ws := r.Workspace
			if r.WorkspaceProject != "" && r.WorkspaceProject != ws {
				ws = ws + "/" + r.WorkspaceProject
			}
			row = append(row, ws)
		}
		row = append(row,
			formatBytes(r.Memory.TotalBytes),
			r.Files,
			r.Nodes,
			r.Edges,
			formatBytes(r.Memory.NodesBytes),
			formatBytes(r.Memory.EdgesBytes),
			formatBytes(r.Memory.SearchBytes),
			formatBytes(r.Memory.VectorsBytes),
		)
		if showDisk {
			row = append(row, formatBytes(r.Memory.DiskBytes))
		}
		row = append(row, r.Path)
		t.AppendRow(row)
	}

	if st.MemoryBytes > attributed {
		other := st.MemoryBytes - attributed
		footer := table.Row{"other"}
		if showWS {
			footer = append(footer, "")
		}
		footer = append(footer, formatBytes(other), "", "", "", "", "", "", "")
		if showDisk {
			footer = append(footer, "")
		}
		footer = append(footer, "embedder + runtime + caches (not attributed)")
		t.AppendFooter(footer)
	}

	t.Render()
}

// renderDaemonWorkspaces prints the per-workspace rollup above the
// repos table. When every workspace is a default singleton (each
// repo in its own auto-named workspace), it emits a one-line hint
// pointing at `gortex workspace set` instead of a wall-of-text
// table that just duplicates the per-repo view.
func renderDaemonWorkspaces(w io.Writer, st daemon.StatusResponse) {
	if len(st.Workspaces) == 0 {
		return
	}
	multiRepo := false
	for _, ws := range st.Workspaces {
		if len(ws.Repos) > 1 {
			multiRepo = true
			break
		}
	}

	if !multiRepo {
		// Compact form: tell the user the workspace boundary is in
		// default mode and how to opt repos into a shared workspace.
		// Avoids
		// printing a 33-row table where every row says "1 repo".
		_, _ = fmt.Fprintf(w,
			"\nworkspaces: %d (one per repo, default — every repo is its own workspace)\n",
			len(st.Workspaces))
		_, _ = fmt.Fprintln(w,
			"  Group repos into a shared workspace with `gortex workspace set <repo> <slug> --global`")
		_, _ = fmt.Fprintln(w,
			"  or `gortex workspace set-all <slug> --root <path> --global`. See `gortex workspace --help`.")
		return
	}

	_, _ = fmt.Fprintln(w, "\nworkspaces:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"workspace", "repos", "projects", "files", "nodes", "edges"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignRight},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignRight},
		{Number: 5, Align: text.AlignRight},
		{Number: 6, Align: text.AlignRight},
	})
	for _, ws := range st.Workspaces {
		projects := strings.Join(ws.Projects, ", ")
		if len(projects) > 50 {
			projects = projects[:47] + "..."
		}
		t.AppendRow(table.Row{ws.Slug, len(ws.Repos), projects, ws.Files, ws.Nodes, ws.Edges})
	}
	t.Render()
}

// renderDaemonSessions lists every connected MCP client. Skipped
// when no sessions are registered — single-process stdio embeds
// don't go through the daemon socket so they never show up here.
func renderDaemonSessions(w io.Writer, st daemon.StatusResponse) {
	if len(st.MCPSessions) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nMCP sessions:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"id", "client", "version", "connected", "cwd"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignRight},
		{Number: 5, Align: text.AlignLeft},
	})
	for _, s := range st.MCPSessions {
		client := s.ClientName
		if client == "" {
			client = "unknown"
		}
		t.AppendRow(table.Row{
			s.ID,
			client,
			s.ClientVersion,
			formatDuration(time.Duration(s.ConnectedSecs) * time.Second),
			s.Cwd,
		})
	}
	t.Render()
}

// renderDaemonServers shows the `~/.gortex/servers.toml` roster.
// Skipped when no file is present — the daemon is in single-server
// mode and there's nothing to list. The "local" column flags the
// entry the multi-server router treats as this daemon itself; auth
// is reported as "yes/no" (token values stay private to the
// daemon).
func renderDaemonServers(w io.Writer, st daemon.StatusResponse) {
	if len(st.ConfiguredServers) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nconfigured servers (~/.gortex/servers.toml):")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"slug", "url", "local", "default", "auth", "workspaces"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignCenter},
		{Number: 4, Align: text.AlignCenter},
		{Number: 5, Align: text.AlignCenter},
		{Number: 6, Align: text.AlignLeft},
	})
	yesno := func(b bool) string {
		if b {
			return "yes"
		}
		return ""
	}
	for _, s := range st.ConfiguredServers {
		t.AppendRow(table.Row{
			s.Slug,
			s.URL,
			yesno(s.Local),
			yesno(s.Default),
			yesno(s.HasAuth),
			strings.Join(s.Workspaces, ", "),
		})
	}
	t.Render()
}

func runDaemonLogs(cmd *cobra.Command, _ []string) error {
	path := daemon.LogFilePath()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	lines, err := tailLines(f, daemonTail)
	if err != nil {
		return err
	}
	for _, l := range lines {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), l)
	}
	return nil
}

// daemonControlClient is the shared "dial + expect running" helper for
// the read-only control subcommands. Returns a clear error instead of
// a misleading ErrDaemonUnavailable.
func daemonControlClient() (*daemon.Client, error) {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable (%v) — is it running? Try `gortex daemon start`", err)
	}
	return c, nil
}

// resolveDaemonBufferPoolMB returns the effective buffer-pool cap.
// Precedence: --backend-buffer-pool-mb flag > GORTEX_DAEMON_BUFFER_POOL_MB env > 0
// (which Open then maps to DefaultBufferPoolMB inside the store).
func resolveDaemonBufferPoolMB() uint64 {
	if daemonBackendBufferPoolMB != 0 {
		return daemonBackendBufferPoolMB
	}
	if env := strings.TrimSpace(os.Getenv("GORTEX_DAEMON_BUFFER_POOL_MB")); env != "" {
		if v, err := strconv.ParseUint(env, 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// killByPID is the fallback stop path for stale daemons that have a PID
// file but don't respond on the socket. Asks the process to terminate,
// waits, then force-kills. Silently returns nil if the PID no longer
// exists.
func killByPID() error {
	pidBytes, err := os.ReadFile(daemon.PIDFilePath())
	if err != nil {
		return nil
	}
	pid, _ := strconv.Atoi(string(pidBytes))
	if pid <= 0 {
		return nil
	}
	_ = platform.TerminateProcess(pid)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !platform.ProcessAlive(pid) {
			// Process gone.
			_ = os.Remove(daemon.PIDFilePath())
			_ = os.Remove(daemon.SocketPath())
			fmt.Fprintln(os.Stderr, "[gortex daemon] stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Last resort.
	_ = platform.KillProcess(pid)
	_ = os.Remove(daemon.PIDFilePath())
	_ = os.Remove(daemon.SocketPath())
	fmt.Fprintln(os.Stderr, "[gortex daemon] stopped (force-killed)")
	return nil
}

// tailLines returns the last n lines of f. Used by `daemon logs`. Small
// implementation — log files are capped at a few MB so we can afford a
// full read and slice rather than seeking from the end.
func tailLines(f io.Reader, n int) ([]string, error) {
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	// Split on newline without pulling in bufio.Scanner buffer-size gotchas.
	var out []string
	start := 0
	for i, b := range buf {
		if b == '\n' {
			out = append(out, string(buf[start:i]))
			start = i + 1
		}
	}
	if start < len(buf) {
		out = append(out, string(buf[start:]))
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// stubController is a placeholder Controller so `gortex daemon start`
// works end-to-end before the real MultiIndexer integration lands. It
