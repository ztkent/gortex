package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm/conversationlog"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/server"
	"github.com/zzet/gortex/internal/server/hub"
	"github.com/zzet/gortex/internal/serverstack"
)

var (
	mcpIndex           string
	mcpTransport       string
	mcpPort            int
	mcpBind            string
	mcpAuthToken       string
	mcpWatch           bool
	mcpServerAPI       bool
	mcpCORSOrigin      string
	mcpDebounce        int
	mcpTrack           []string
	mcpProject         string
	mcpCacheDir        string
	mcpNoCache         bool
	mcpEmbeddings      bool
	mcpEmbeddingsURL   string
	mcpEmbeddingsModel string
	mcpSemantic        bool
	mcpNoSemantic      bool
	mcpSemanticMode    string
	mcpNoDaemon        bool
	mcpForceProxy      bool
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP server (stdio transport)",
	RunE:  runMCP,
}

func init() {
	mcpCmd.Flags().StringVar(&mcpIndex, "index", "", "repository path to index on startup")
	mcpCmd.Flags().StringVar(&mcpTransport, "transport", "stdio", "transport: stdio")
	mcpCmd.Flags().IntVar(&mcpPort, "port", 8765, "port for HTTP server API when --server is set")
	mcpCmd.Flags().BoolVar(&mcpWatch, "watch", false, "keep graph in sync with filesystem changes")
	mcpCmd.Flags().IntVar(&mcpDebounce, "debounce", 150, "debounce delay in ms")
	mcpCmd.Flags().BoolVar(&mcpServerAPI, "server", false, "start HTTP server API alongside MCP stdio")
	mcpCmd.Flags().StringVar(&mcpBind, "bind", "127.0.0.1", "bind address for --server; requires --auth-token when not localhost")
	mcpCmd.Flags().StringVar(&mcpAuthToken, "auth-token", "", "bearer token for --server's /v1/* requests (fallback: $GORTEX_SERVER_TOKEN)")
	mcpCmd.Flags().StringVar(&mcpCORSOrigin, "cors-origin", "*", "allowed CORS origin for server API")
	mcpCmd.Flags().StringSliceVar(&mcpTrack, "track", nil, "additional repository paths to track")
	mcpCmd.Flags().StringVar(&mcpProject, "project", "", "active project name")
	mcpCmd.Flags().StringVar(&mcpCacheDir, "cache-dir", "", "graph cache directory (default ~/.gortex/cache/)")
	mcpCmd.Flags().BoolVar(&mcpNoCache, "no-cache", false, "disable graph caching")
	mcpCmd.Flags().BoolVar(&mcpEmbeddings, "embeddings", false, "enable semantic search (built-in word vectors or transformer if compiled in)")
	mcpCmd.Flags().StringVar(&mcpEmbeddingsURL, "embeddings-url", "", "embedding API URL (e.g. http://localhost:11434 for Ollama)")
	mcpCmd.Flags().StringVar(&mcpEmbeddingsModel, "embeddings-model", "", "embedding model name (default: auto-detect)")
	mcpCmd.Flags().BoolVar(&mcpSemantic, "semantic", false, "enable semantic enrichment (SCIP, go/types, LSP)")
	mcpCmd.Flags().BoolVar(&mcpNoSemantic, "no-semantic", false, "disable semantic enrichment")
	mcpCmd.Flags().StringVar(&mcpSemanticMode, "semantic-mode", "typecheck", "Go analysis mode: typecheck or callgraph")
	mcpCmd.Flags().BoolVar(&mcpNoDaemon, "no-daemon", false, "deprecated no-op (warns when set); the embedded server is used automatically when no daemon is available")
	mcpCmd.Flags().BoolVar(&mcpForceProxy, "proxy", false, "require a running daemon and proxy through it (error if unavailable)")
	rootCmd.AddCommand(mcpCmd)
}

var legacyMCPFlagsWarned bool

// warnLegacyMCPFlags emits one stderr line per explicitly-set legacy flag
// (--index/--watch/--proxy/--no-daemon). These are permanent no-op compat
// shims for un-migrated on-disk editor configs. stderr only — stdout is
// the MCP JSON-RPC stream and a stray byte corrupts the protocol.
func warnLegacyMCPFlags(cmd *cobra.Command) {
	if legacyMCPFlagsWarned {
		return
	}
	legacyMCPFlagsWarned = true
	for _, name := range []string{"index", "watch", "proxy", "no-daemon"} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			fmt.Fprintf(os.Stderr, "[gortex] note: --%s is deprecated and ignored on the proxy path; "+
				"`gortex mcp` proxies to the daemon (auto-starting it) and falls back to an embedded server.\n", name)
		}
	}
}

func runMCP(cmd *cobra.Command, args []string) error {
	warnLegacyMCPFlags(cmd)

	// Daemon-first: ensure a daemon is up (auto-starting it under a
	// single-flight lock when GORTEX_AUTOSTART allows), then relay stdio
	// over its socket. The old stdin-TTY heuristic is gone — behavior is
	// identical from a terminal or a pipe given the same daemon state. The
	// legacy --no-daemon flag is an inert no-op (warned above): whether we
	// proxy or fall back to the embedded server is decided purely by daemon
	// presence + GORTEX_AUTOSTART, never by the flag.
	switch resolveDaemonDecision() {
	case daemonReady, daemonAutostarted:
		ran, proxyErr := runProxy(cmd.Context())
		if proxyErr != nil {
			return proxyErr
		}
		if ran {
			return nil
		}
		// Lost the daemon between ensure and dial (rare) — fall
		// through to the embedded server.
	}

	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// The embedded server runs in single-repo mode over --index: it
	// indexes that tree and serves the whole graph (no marker handshake,
	// no .gortex/workspace.toml). Multi-repo scoping is the daemon's job.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	// Resolve the embedded semantic decision: --no-semantic forces off,
	// --semantic forces on, otherwise semantic.enabled in config decides.
	cfg.Semantic.Enabled = !mcpNoSemantic && (mcpSemantic || cfg.Semantic.Enabled)

	// Side-store layout for the embedded path: every store partitions
	// per-repo under the indexed repo path, and the notebook is repo-local
	// (committed to git). When --cache-dir is unset, notes / memories still
	// persist via the shared cache dir's sidecar.
	sideStoreCacheDir := mcpCacheDir
	if sideStoreCacheDir == "" {
		sideStoreCacheDir = platform.CacheDir()
	}
	// The savings ledger is machine-global — the same sidecar database
	// every entry point writes and the `gortex savings` CLI reads.
	// --cache-dir deliberately does NOT relocate it: users set that flag
	// to move the graph cache, and quietly splitting the ledger away
	// from the dashboard's default read path recreates the
	// empty-dashboard failure mode. Isolation (tests, sandboxes) comes
	// from XDG_DATA_HOME / XDG_CACHE_HOME, which both ledger paths
	// honour.

	ss, err := serverstack.NewSharedServer(serverstack.SharedServerConfig{
		Lifecycle: serverstack.LifecycleOneshot,
		Index:     mcpIndex,
		Config:    cfg,
		Logger:    logger,
		Version:   version,
		Embedder: serverstack.EmbedderRequest{
			FlagChanged: cmd.Flags().Changed("embeddings"),
			FlagEnabled: mcpEmbeddings,
			FlagURL:     mcpEmbeddingsURL,
			FlagModel:   mcpEmbeddingsModel,
		},
		ActiveProject: mcpProject,
		SemanticMode:  mcpSemanticMode,
		SideStores: serverstack.SideStores{
			NotesDir:     sideStoreCacheDir,
			NotesRepo:    mcpIndex,
			FeedbackDir:  mcpCacheDir,
			FeedbackRepo: mcpIndex,
			NotebookPath: mcpIndex,
		},
		SavingsRepo: mcpIndex,
	})
	if err != nil {
		return fmt.Errorf("build server stack: %w", err)
	}
	defer func() { _ = ss.Close() }()

	g := ss.Graph
	idx := ss.Indexer
	cm := ss.ConfigMgr
	mi := ss.MultiIndexer
	srv := ss.MCP

	// Persist the resolved active project so the MCP server and
	// set_active_project agree on the default scope.
	activeProject := mcpProject
	if activeProject == "" && cm != nil {
		activeProject = cm.Global().ActiveProject
	}
	if cm != nil {
		cm.Global().ActiveProject = activeProject
	}

	// Register --track repos for the background watch / track loop.
	if cm != nil {
		for _, trackPath := range mcpTrack {
			absPath, aerr := filepath.Abs(trackPath)
			if aerr != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not resolve --track path %s: %v\n", trackPath, aerr)
				continue
			}
			if aerr := cm.Global().AddRepo(config.RepoEntry{Path: absPath}); aerr != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not add --track repo %s: %v\n", absPath, aerr)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[gortex] MCP server ready (transport: %s)\n", mcpTransport)

	// Start server HTTP API if requested.
	if mcpServerAPI {
		authToken := mcpAuthToken
		if authToken == "" {
			authToken = os.Getenv("GORTEX_SERVER_TOKEN")
		}
		if authToken == "" {
			if !isLocalhostBind(mcpBind) {
				return fmt.Errorf("--bind %q requires --auth-token (or $GORTEX_SERVER_TOKEN); refusing to expose unauthenticated server on external interface", mcpBind)
			}
			fmt.Fprintln(os.Stderr, "[gortex] server: unauthenticated mode; localhost only")
		}

		serverHandler := server.NewHandler(srv.MCPServer(), g, version, logger)
		if cm != nil {
			serverHandler.SetConfigManager(cm)
		}
		if id, err := resolveServerID(mcpCacheDir); err == nil {
			serverHandler.SetServerID(id)
		} else {
			fmt.Fprintf(os.Stderr, "[gortex] server: id persistence disabled: %v\n", err)
		}
		// Conversation-log inspector: enable the /v1/conversations* routes
		// (opt-in sink) with the route-scoped DNS-rebind guard cooperating
		// with this server's auth token.
		serverHandler.SetConversationDir(conversationlog.DirFromEnv())
		authTokenForGuard := authToken
		serverHandler.SetConversationGuard(nil, func() string { return authTokenForGuard })
		handler := server.WithAuth(serverHandler, authToken)
		corsOpts := server.CORSOptions{AllowOrigins: []string{mcpCORSOrigin}}
		handler = server.WithCORS(handler, corsOpts)
		go func() {
			serverAddr := fmt.Sprintf("%s:%d", mcpBind, mcpPort)
			fmt.Fprintf(os.Stderr, "[gortex] server API at http://%s\n", serverAddr)
			if err := http.ListenAndServe(serverAddr, handler); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "[gortex] server API error: %v\n", err)
			}
		}()
	}

	// Start MCP stdio in a goroutine so we can do background init.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeStdio()
	}()

	// Create persistence store.
	var store persistence.Store
	if mcpNoCache {
		store = persistence.NopStore{}
	} else {
		var err error
		store, err = persistence.NewFileStore(mcpCacheDir, version)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] warning: cache disabled: %v\n", err)
			store = persistence.NopStore{}
		}
	}

	// Background: index, watch, analyze — graph populates while MCP is live.
	go func() {
		if mcpIndex != "" {
			commitHash := gitCommitHash(mcpIndex)
			branch := gitBranch(mcpIndex)
			repoKey := canonicalRepo(mcpIndex)
			cached := false

			if commitHash != "" && store.Check(repoKey, branch, commitHash) && store.Validate(repoKey, branch, commitHash) {
				snap, err := store.Load(repoKey, branch, commitHash)
				if err == nil {
					for _, n := range snap.Nodes {
						g.AddNode(n)
					}
					for _, e := range snap.Edges {
						g.AddEdge(e)
					}
					idx.SetFileMtimes(snap.FileMtimes)
					idx.SetRootPath(mcpIndex)

					// Restore vector index if available.
					if len(snap.VectorIndex) > 0 && snap.VectorDims > 0 {
						if err := idx.ImportVectorIndex(snap.VectorIndex, snap.VectorDims, snap.VectorCount); err != nil {
							fmt.Fprintf(os.Stderr, "[gortex] vector index restore failed: %v\n", err)
						}
					}

					result, err := idx.IncrementalReindex(mcpIndex)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[gortex] incremental reindex failed: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "[gortex] restored graph (%d nodes, %d edges), re-indexed %d stale files in %dms\n",
							result.NodeCount, result.EdgeCount, result.FileCount, result.DurationMs)
					}
					cached = true
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] cache load failed, will re-index: %v\n", err)
				}
			}

			if !cached {
				fmt.Fprintf(os.Stderr, "[gortex] indexing %s...\n", mcpIndex)
				result, err := idx.Index(mcpIndex)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] indexing failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "[gortex] indexed %d files (%d nodes, %d edges) in %dms\n",
					result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
			}
		}

		// Search backend is auto-updated via SearchProvider (idx.Search)

		// Pass contract registry to MCP server.
		// In multi-repo mode, merge all per-repo registries.
		if mi != nil {
			srv.SetContractRegistry(mi.MergedContractRegistry())
		} else if cr := idx.ContractRegistry(); cr != nil {
			srv.SetContractRegistry(cr)
		}

		// Start watcher if requested.
		if mcpWatch {
			wcfg := cfg.Watch
			wcfg.Enabled = true
			if mcpDebounce > 0 {
				wcfg.DebounceMs = mcpDebounce
			}

			watcher, err := indexer.NewWatcher(idx, wcfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] watcher setup failed: %v\n", err)
				return
			}

			watchPaths := wcfg.Paths
			if len(watchPaths) == 0 && mcpIndex != "" {
				watchPaths = []string{mcpIndex}
			}
			if len(watchPaths) == 0 {
				watchPaths = []string{"."}
			}

			if err := watcher.Start(watchPaths); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] watcher start failed: %v\n", err)
				return
			}
			srv.SetWatcher(watcher)

			// Create hub for fan-out of watcher events.
			eventHub := hub.New()
			go eventHub.Run(watcher.Events())

			srv.WatchForReanalysis(eventHub, 500)
			fmt.Fprintf(os.Stderr, "[gortex] watch mode active\n")
		}

		// Run initial analysis.
		srv.RunAnalysis()
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] received %s, shutting down\n", sig)

		// Persist graph snapshot on shutdown.
		if mcpIndex != "" {
			commitHash := gitCommitHash(mcpIndex)
			if commitHash != "" {
				snap := &persistence.Snapshot{
					Version:    version,
					RepoPath:   canonicalRepo(mcpIndex),
					CommitHash: commitHash,
					Branch:     gitBranch(mcpIndex),
					IndexedAt:  time.Now(),
					Nodes:      g.AllNodes(),
					Edges:      g.AllEdges(),
					FileMtimes: idx.FileMtimes(),
				}
				// Include vector index if available.
				snap.VectorIndex, snap.VectorDims, snap.VectorCount = idx.ExportVectorIndex()
				if err := store.Save(snap); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] cache save failed: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] saved graph snapshot (%d nodes, %d edges)\n",
						len(snap.Nodes), len(snap.Edges))
				}
			}
		}

		return nil
	}
}
