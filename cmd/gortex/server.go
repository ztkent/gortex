package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/mcp/streamable"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/semantic/scip"
	"github.com/zzet/gortex/internal/server"
	"github.com/zzet/gortex/internal/server/hub"
)

var (
	serverPort       int
	serverBind       string
	serverAuthToken  string
	serverIndex      string
	serverCORSOrigin string
	serverWatch      bool
	serverTrack      []string
	serverProject    string
	// serverWorkspace is the workspace slug filter. When set, the MCP
	// layer scopes every query to this workspace by default —
	// equivalent to passing `workspace: <slug>` on each call. Empty
	// means "no scope" (legacy multi-workspace view).
	serverWorkspace string
	// serverScopeProject is the project slug filter, intended to
	// further narrow inside `--workspace`. Named --scope-project to
	// avoid colliding with the existing --project flag (which selects
	// the GlobalConfig active-project — a named group of repos, a
	// distinct concept).
	serverScopeProject    string
	serverCacheDir        string
	serverNoCache         bool
	serverEmbeddings      bool
	serverEmbeddingsURL   string
	serverEmbeddingsModel string
	serverSemantic        bool
	serverNoSemantic      bool
	serverSemanticMode    string
	// serverSnapshot is a path to a gob+gzip snapshot file (the
	// format `gortex index --snapshot <path>` writes). Loaded into
	// the in-memory graph before the HTTP listener accepts traffic.
	// Used by gortex-cloud's per-workspace supervisor to boot a
	// hosted gortex server from R2/Hetzner-OS-cached state.
	serverSnapshot string
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the HTTP server API for external integrations",
	Long:  "Exposes Gortex MCP tools as an HTTP/JSON API under /v1/*: /v1/health, /v1/tools, /v1/tools/{name}, /v1/stats, /v1/graph, /v1/events. The UI is a separate Next.js frontend (github.com/gortexhq/web) that talks to this server over HTTP.",
	RunE:  runServer,
}

func init() {
	serverCmd.Flags().IntVar(&serverPort, "port", 4747, "HTTP port to listen on")
	serverCmd.Flags().StringVar(&serverBind, "bind", "127.0.0.1", "bind address. Accepts: '127.0.0.1' / '0.0.0.0' (TCP, --auth-token required when not localhost) or 'unix:///path/to.sock' for a Unix-domain socket")
	serverCmd.Flags().StringVar(&serverAuthToken, "auth-token", "", "bearer token required on every /v1/* request (fallback: $GORTEX_SERVER_TOKEN)")
	serverCmd.Flags().StringVar(&serverIndex, "index", "", "repository path to index on startup")
	serverCmd.Flags().StringVar(&serverCORSOrigin, "cors-origin", "*", "allowed CORS origin (use '*' for any)")
	serverCmd.Flags().BoolVar(&serverWatch, "watch", false, "keep graph in sync with filesystem changes")
	serverCmd.Flags().StringSliceVar(&serverTrack, "track", nil, "additional repository paths to track")
	serverCmd.Flags().StringVar(&serverProject, "project", "", "active project name (GlobalConfig group of repos)")
	serverCmd.Flags().StringVar(&serverWorkspace, "workspace", "", "workspace slug — restricts BOTH indexing and queries to repos whose resolved workspace matches (RepoEntry override → .gortex.yaml::workspace → repo prefix). Empty means all workspaces.")
	serverCmd.Flags().StringVar(&serverScopeProject, "scope-project", "", "project slug — narrows further inside --workspace (also gates indexing). No effect without --workspace.")
	serverCmd.Flags().StringVar(&serverCacheDir, "cache-dir", "", "graph cache directory (default ~/.cache/gortex/)")
	serverCmd.Flags().BoolVar(&serverNoCache, "no-cache", false, "disable graph caching")
	serverCmd.Flags().BoolVar(&serverEmbeddings, "embeddings", false, "enable semantic search")
	serverCmd.Flags().StringVar(&serverEmbeddingsURL, "embeddings-url", "", "embedding API URL (e.g. http://localhost:11434 for Ollama)")
	serverCmd.Flags().StringVar(&serverEmbeddingsModel, "embeddings-model", "", "embedding model name")
	serverCmd.Flags().BoolVar(&serverSemantic, "semantic", false, "enable semantic enrichment (SCIP, go/types, LSP)")
	serverCmd.Flags().BoolVar(&serverNoSemantic, "no-semantic", false, "disable semantic enrichment")
	serverCmd.Flags().StringVar(&serverSemanticMode, "semantic-mode", "typecheck", "Go analysis mode: typecheck or callgraph")
	serverCmd.Flags().StringVar(&serverSnapshot, "snapshot", "", "load a snapshot file at startup (gob+gzip; the format `gortex index --snapshot` writes). Used by gortex-cloud's per-workspace supervisor to boot from a precomputed snapshot.")
	rootCmd.AddCommand(serverCmd)
}

func runServer(cmd *cobra.Command, _ []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// Resolve auth token: flag wins, env var fallback.
	authToken := serverAuthToken
	if authToken == "" {
		authToken = os.Getenv("GORTEX_SERVER_TOKEN")
	}

	// Bind/auth policy. Without a token we force the listener onto
	// localhost; binding to any external interface without auth is a
	// foot-gun (anyone on the network could invoke arbitrary MCP
	// tools), so reject that combination up front. Unix-domain
	// sockets are inherently localhost-bounded by filesystem
	// permissions, so no token is required for them.
	usingUnixSocket := strings.HasPrefix(serverBind, "unix://")
	if authToken == "" && !usingUnixSocket {
		if !isLocalhostBind(serverBind) {
			return fmt.Errorf("--bind %q requires --auth-token (or $GORTEX_SERVER_TOKEN); refusing to expose unauthenticated server on external interface", serverBind)
		}
		fmt.Fprintln(os.Stderr, "[gortex] server: unauthenticated mode; localhost only")
	}

	// Resolve a stable server id. We only fail when a token path is
	// set but not writable — without one, a future daemon client just
	// can't detect reconnects, which is non-fatal.
	serverID, err := resolveServerID(serverCacheDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex] server: id persistence disabled: %v\n", err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build graph/parser/indexer/query/MCP stack.
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	languages.RegisterCustomGrammars(reg, cfg.Index.Grammars, logger)
	idx := indexer.New(g, reg, cfg.Index, logger)

	// --snapshot pre-loads the in-memory graph from a gob+gzip file
	// before the HTTP listener accepts traffic. Used by gortex-cloud's
	// supervisor (it downloads the snapshot from object storage to
	// local disk, then launches `gortex server --snapshot <path>`).
	// Failures here are logged but non-fatal — the server proceeds
	// with an empty graph and `--index` / multi-repo paths populate
	// it.
	if serverSnapshot != "" {
		if res, err := loadSnapshotFrom(g, serverSnapshot, logger); err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] server: snapshot load failed: %v\n", err)
		} else if res.Loaded {
			fmt.Fprintf(os.Stderr, "[gortex] server: snapshot loaded (path=%s)\n", serverSnapshot)
		}
	}

	// Set up embedding provider for semantic search. Kept local so it
	// can be handed off to MultiIndexer below; otherwise per-repo
	// indexers built inside TrackRepoCtx have embedder=nil.
	// resolveEmbedder applies explicit flag/env > `embedding:` config >
	// default-on static, so a stock server gets semantic search.
	embedder, embDesc, embErr := resolveEmbedder(embedderRequest{
		flagChanged: cmd.Flags().Changed("embeddings"),
		flagEnabled: serverEmbeddings,
		flagURL:     serverEmbeddingsURL,
		flagModel:   serverEmbeddingsModel,
	}, cfg)
	if embErr != nil {
		fmt.Fprintf(os.Stderr, "[gortex] server: embeddings disabled: %v\n", embErr)
	} else if embedder != nil {
		fmt.Fprintf(os.Stderr, "[gortex] server: semantic search enabled (%s)\n", embDesc)
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
		idx.SetEmbeddingChunkOptions(embeddingChunkOptions(cfg))
		idx.SetEmbeddingMaxSymbols(cfg.Embedding.MaxSymbols)
		idx.SetEmbeddingAPIConcurrency(cfg.Embedding.APIConcurrency)
	}

	// N5 hot-path locals — see daemon_state.go / mcp.go for context.
	var (
		serverResolverLSPRegistry *lsp.ResolverHelperRegistry
		serverResolverLSPRouter   *lsp.Router
	)

	// Set up semantic enrichment.
	if !serverNoSemantic && (serverSemantic || cfg.Semantic.Enabled) {
		semCfg := cfg.Semantic
		semCfg.Enabled = true

		semInternalCfg := semantic.Config{
			Enabled:           semCfg.Enabled,
			TimeoutSeconds:    semCfg.TimeoutSeconds,
			EnrichOnWatch:     semCfg.EnrichOnWatch,
			WatchDebounceMs:   semCfg.WatchDebounceMs,
			RefuteUnconfirmed: semCfg.RefuteUnconfirmed,
		}
		for _, pc := range semCfg.Providers {
			semInternalCfg.Providers = append(semInternalCfg.Providers, semantic.ProviderConfig{
				Name:        pc.Name,
				Command:     pc.Command,
				Args:        pc.Args,
				Languages:   pc.Languages,
				Priority:    pc.Priority,
				Enabled:     pc.Enabled,
				Mode:        pc.Mode,
				Daemon:      pc.Daemon,
				MaxParallel: pc.MaxParallel,
			})
		}

		semMgr := semantic.NewManager(semInternalCfg, logger)

		mode := goanalysis.ModeTypeCheck
		if serverSemanticMode == "callgraph" {
			mode = goanalysis.ModeCallGraph
		}
		goProvider := goanalysis.NewProvider(mode, false, logger)
		semMgr.RegisterProvider(goProvider)
		// Wire the goanalysis provider as the contract pipeline's
		// BindingResolver — once Enrich runs, contract enrichment can
		// upgrade Origin from ast_inferred to lsp_resolved using
		// compiler-grade type info. See spec-contract-extraction.md §4.5.
		contracts.SetBindingResolver(goProvider)

		// Daemon-managed LSP router. Same shape as `gortex mcp` —
		// owns subprocess lifecycle for every spec the user opted
		// into, lazy-spawns on first request, idle reaper + LRU
		// eviction. Manager borrows providers via the LSPRouter
		// interface for batch enrichment; the MCP server reads it
		// back through SemanticManager().LSPRouter() for on-demand
		// requests.
		lspWorkspace := serverIndex
		if lspWorkspace == "" {
			lspWorkspace, _ = os.Getwd()
		}
		lspRouter := lsp.NewRouter(lspWorkspace, logger).
			WithIdleTimeout(10 * time.Minute).
			WithReaperInterval(time.Minute).
			WithMaxAlive(6)
		semMgr.SetLSPRouter(lspRouter)

		for _, pc := range semCfg.Providers {
			if !pc.Enabled {
				continue
			}
			switch {
			case strings.HasPrefix(pc.Name, "scip-") && pc.Command != "":
				semMgr.RegisterProvider(scip.NewProvider(pc.Command, pc.Args, pc.Languages, semCfg.TimeoutSeconds, logger))
			case lsp.SpecByName(pc.Name) != nil:
				// Known registry spec (gopls, tsserver, pyright,
				// rust-analyzer, clangd, jdtls, …). Router owns
				// lifecycle — register the spec so it shows up in
				// EnabledSpecNames; first ForSpec call triggers the
				// lazy spawn.
				lspRouter.RegisterSpec(lsp.SpecByName(pc.Name))
			case pc.Daemon:
				// Custom user-defined daemon (no registry spec) —
				// keep the legacy eager-construction path so
				// out-of-registry LSP servers still work.
				semMgr.RegisterProvider(lsp.NewProvider(pc.Command, pc.Args, pc.Languages, pc.Daemon, pc.MaxParallel, logger))
			}
		}

		// Auto-register every known LSP spec whose binary resolves on
		// PATH — same default-on shape as the daemon path, so
		// `gortex server` and `gortex daemon start` agree on which
		// LSPs are available without forcing users to learn a YAML
		// knob. Per-spec opt-out via `semantic.providers: [{ name,
		// enabled: false }]` or GORTEX_LSP_DISABLE=name1,name2.
		// GORTEX_LSP_DISABLE=all skips the auto-register entirely.
		disabled := lspDisabledSet(cfg.Semantic.Providers, os.Getenv("GORTEX_LSP_DISABLE"))
		var autoRegistered []string
		if !disabled["__all__"] {
			autoRegistered = lspRouter.RegisterAvailable(disabled)
		}

		idx.SetSemanticManager(semMgr)

		// Resolve-time LSP hot path. Same wiring as daemon/mcp.
		if !isFalsyEnv("GORTEX_LSP_RESOLVER") {
			serverResolverLSPRegistry = lsp.NewResolverHelperRegistry()
			serverResolverLSPRouter = lspRouter
			idx.SetResolverLSPHelper(serverResolverLSPRegistry)

			if abs, err := filepath.Abs(lspWorkspace); err == nil && lspWorkspace != "" {
				tsSpec := lsp.SpecByName("typescript-language-server")
				if tsSpec != nil && lspRouter.Available(tsSpec) && repoLikelyHasTypeScriptIntent(abs) {
					absRootCapture := abs
					poolSize := lsp.ResolverPoolSizeFromEnv(1)
					helper := buildResolverLSPHelper(lspRouter, tsSpec, absRootCapture, poolSize, logger)
					serverResolverLSPRegistry.Register("", helper)
				}
			}
		}

		fmt.Fprintf(os.Stderr, "[gortex] server: semantic enrichment enabled (mode: %s, lsp_auto_registered: %v)\n", serverSemanticMode, autoRegistered)
	}

	// Multi-repo support.
	cm, err := config.NewConfigManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex] warning: could not load global config: %v\n", err)
	}

	if cm != nil && len(serverTrack) > 0 {
		for _, trackPath := range serverTrack {
			absPath, err := filepath.Abs(trackPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not resolve --track path %s: %v\n", trackPath, err)
				continue
			}
			if err := cm.Global().AddRepo(config.RepoEntry{Path: absPath}); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not add --track repo %s: %v\n", absPath, err)
			}
		}
	}

	activeProject := serverProject
	if activeProject == "" && cm != nil {
		activeProject = cm.Global().ActiveProject
	}
	if cm != nil {
		cm.Global().ActiveProject = activeProject
	}

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
		if embedder != nil {
			mi.SetEmbedder(embedder)
			mi.SetEmbeddingChunkOptions(embeddingChunkOptions(cfg))
			mi.SetEmbeddingMaxSymbols(cfg.Embedding.MaxSymbols)
			mi.SetEmbeddingAPIConcurrency(cfg.Embedding.APIConcurrency)
		}
		if serverResolverLSPRegistry != nil {
			mi.SetResolverLSPHelper(serverResolverLSPRegistry)
			if serverResolverLSPRouter != nil {
				routerRef := serverResolverLSPRouter
				registryRef := serverResolverLSPRegistry
				mi.SetOnRepoTracked(func(prefix, absPath string) {
					tsSpec := lsp.SpecByName("typescript-language-server")
					if tsSpec == nil || !routerRef.Available(tsSpec) {
						return
					}
					absRootCapture := absPath
					helper := lsp.NewLazyResolverHelper(
						func() (*lsp.Provider, error) {
							return routerRef.ForSpecWorkspace(tsSpec, absRootCapture)
						},
						absRootCapture,
						tsSpec.Extensions,
						0,
						logger,
					)
					registryRef.Register(prefix, helper)
				})
			}
		}
	}

	var multiOpts []gortexmcp.MultiRepoOptions
	if mi != nil || cm != nil {
		multiOpts = append(multiOpts, gortexmcp.MultiRepoOptions{
			MultiIndexer:   mi,
			ConfigManager:  cm,
			ActiveProject:  activeProject,
			ScopeWorkspace: serverWorkspace,
			ScopeProject:   serverScopeProject,
		})
	}
	if serverScopeProject != "" && serverWorkspace == "" {
		fmt.Fprintln(os.Stderr, "[gortex] server: --scope-project without --workspace will match repos across every workspace whose project slug equals this value")
	}

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	eng.ApplyRerankWeights(cfg.Search.Weights)
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules, multiOpts...)
	srv.SetArchitecture(cfg.Architecture)
	srv.SetArtifacts(cfg.Artifacts)
	srv.SetNamedQueries(cfg.Queries)

	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
		// Hook the LSP router (if any) into the MCP
		// `notifications/diagnostics` broadcaster so subscribed
		// clients receive publishDiagnostics in real time. No-op
		// when no router is wired.
		srv.SetLSPDiagnosticsBroadcasting()
	}

	// Create persistence store.
	var store persistence.Store
	if serverNoCache {
		store = persistence.NopStore{}
	} else {
		var err error
		store, err = persistence.NewFileStore(serverCacheDir, version)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] server: cache disabled: %v\n", err)
			store = persistence.NopStore{}
		}
	}

	// Build the HTTP handler — start serving immediately, index in background.
	serverHandler := server.NewHandler(srv.MCPServer(), g, version, logger)
	if cm != nil {
		serverHandler.SetConfigManager(cm)
	}
	if serverID != "" {
		serverHandler.SetServerID(serverID)
	}
	// Editor overlay sessions. 5-minute idle TTL is long enough for
	// an MCP client to push, query, and tear down between turns;
	// short enough that a crashed client doesn't leak buffers
	// indefinitely.
	// Editor-overlay manager. Idle TTL resolution:
	//   GORTEX_OVERLAY_IDLE_TTL env var > daemon.DefaultOverlayIdleTTL.
	// Tests can disable expiry by setting GORTEX_OVERLAY_IDLE_TTL=0.
	// Most relevant security guarantee: when the MCP session ends,
	// Server.ReleaseSession drops the overlay immediately, so the
	// TTL is a fail-safe for missed disconnects rather than the
	// primary cleanup path.
	overlays := daemon.NewOverlayManager(daemon.OverlayIdleTTLFromEnv(0))
	serverHandler.SetOverlayManager(overlays)
	srv.SetOverlayManager(overlays)

	// Wire the multi-server router. When `~/.gortex/servers.toml` is
	// present, every
	// /v1/tools/<name> call flows through the router which decides
	// local-fast-path vs proxy. Missing or empty servers.toml leaves
	// the router nil and the handler keeps its single-server
	// behaviour. The local executor re-enters handleToolCall via
	// the in-process MCP tool dispatch — encoded by passing the
	// body straight back to the local mcp server through a
	// small shim that bypasses the router (preventing infinite
	// recursion).
	if scfg, scfgErr := daemon.LoadServersConfig(""); scfgErr == nil && scfg != nil && len(scfg.Server) > 0 {
		rosters := daemon.NewWorkspaceRosterCache(60 * time.Second)
		localExec := newLocalToolExecutor(srv, logger)
		// localSlug is taken from the first server marked default,
		// then falls back to the first entry. This is the slug the
		// router treats as "us" — proxy targets that match it run
		// locally instead of dialing back into ourselves.
		var localSlug string
		if def := scfg.DefaultServer(); def != nil {
			localSlug = def.Slug
		}
		router := daemon.NewRouter(daemon.RouterConfig{
			Servers:      scfg,
			Rosters:      rosters,
			LocalSlug:    localSlug,
			LocalExecute: localExec,
			Logger:       logger,
		})
		serverHandler.SetRouter(router)
		fmt.Fprintf(os.Stderr, "[gortex] server: multi-server router wired (%d servers, local=%q)\n",
			len(scfg.Server), localSlug)
	} else if scfgErr != nil {
		fmt.Fprintf(os.Stderr, "[gortex] server: servers.toml load error (running single-server): %v\n", scfgErr)
	}

	// MCP 2026 Streamable HTTP transport on /mcp (POST/GET/DELETE).
	// Stateless from the network's perspective: every request
	// carries `Mcp-Session-Id`; the per-request worker replays
	// state out of an in-memory store. Behind a load balancer
	// the store moves to a shared backend (Redis, …) — the
	// streamable.SessionStore interface keeps the transport
	// itself decoupled from that choice. Reuses the multi-server
	// router we just wired so tools/call frames still proxy
	// across the federation when their workspace lives elsewhere.
	streamableTransport := streamable.New(streamable.Config{
		Dispatcher: streamable.MCPServerDispatcher{Server: srv.MCPServer()},
		Logger:     logger,
		Router:     serverHandler.Router(),
	})
	serverHandler.SetStreamableTransport(streamableTransport)
	fmt.Fprintf(os.Stderr, "[gortex] server: streamable HTTP transport active on /mcp\n")

	// Watch mode: set up the event hub so /v1/events has a source.
	if serverWatch {
		wcfg := cfg.Watch
		wcfg.Enabled = true
		watcher, err := indexer.NewWatcher(idx, wcfg, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] server: watcher setup failed: %v\n", err)
		} else {
			watchPaths := wcfg.Paths
			if len(watchPaths) == 0 && serverIndex != "" {
				watchPaths = []string{serverIndex}
			}
			if len(watchPaths) == 0 {
				watchPaths = []string{"."}
			}
			if err := watcher.Start(watchPaths); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] server: watcher start failed: %v\n", err)
			} else {
				srv.SetWatcher(watcher)
				eventHub := hub.New()
				go eventHub.Run(watcher.Events())
				srv.WatchForReanalysis(eventHub, 500)
				serverHandler.SetEventHub(eventHub)
				fmt.Fprintf(os.Stderr, "[gortex] server: watch mode active\n")
			}
		}
	}

	// Wrap with auth (no-op when authToken is empty), then CORS.
	handler := server.WithAuth(serverHandler, authToken)
	corsOpts := server.CORSOptions{AllowOrigins: []string{serverCORSOrigin}}
	handler = server.WithCORS(handler, corsOpts)

	httpServer := &http.Server{
		Handler: handler,
	}

	errCh := make(chan error, 1)
	if usingUnixSocket {
		// Unix-domain socket transport. Path follows "unix://" —
		// `unix:///var/run/gortex/<slug>.sock` is the
		// recommended per-slug shape. Cleaning up a stale socket file
		// from a previous (crashed) run lets restart-on-failure
		// supervisors not need a wrapping rm.
		socketPath := strings.TrimPrefix(serverBind, "unix://")
		if socketPath == "" {
			return fmt.Errorf("--bind unix:// requires a path (e.g. unix:///var/run/gortex/main.sock)")
		}
		// Clean up a stale socket from a crashed previous run. We
		// only remove if it really is a socket — a regular file
		// at the path is left alone so we can't accidentally clobber
		// user data.
		if fi, err := os.Stat(socketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(socketPath)
		}
		// Best-effort dir creation; the listener will surface a clear
		// error if the dir is missing and uncreatable.
		_ = os.MkdirAll(filepath.Dir(socketPath), 0o755)
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			return fmt.Errorf("listen on unix socket %s: %w", socketPath, err)
		}
		// Restrict to the running user — agent processes that talk
		// to this socket already share a uid with the daemon. Other
		// users on the host shouldn't be able to invoke MCP tools.
		_ = os.Chmod(socketPath, 0o600)
		fmt.Fprintf(os.Stderr, "[gortex] server listening on unix://%s\n", socketPath)
		go func() {
			if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	} else {
		httpServer.Addr = fmt.Sprintf("%s:%d", serverBind, serverPort)
		fmt.Fprintf(os.Stderr, "[gortex] server listening on http://%s\n", httpServer.Addr)
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	// Background: index, multi-repo, analyze — graph populates while HTTP is live.
	go func() {
		// When MultiIndexer is available (global config has repos), use it exclusively.
		// Single --index flag is only used when no multi-repo config exists.
		if mi != nil {
			if serverWorkspace != "" || serverScopeProject != "" {
				fmt.Fprintf(os.Stderr, "[gortex] server: multi-repo indexing (scope: workspace=%q project=%q)...\n", serverWorkspace, serverScopeProject)
			} else {
				fmt.Fprintf(os.Stderr, "[gortex] server: multi-repo indexing...\n")
			}
			if _, err := mi.IndexScoped(serverWorkspace, serverScopeProject); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] server: multi-repo indexing error: %v\n", err)
			}
		} else if serverIndex != "" {
			commitHash := gitCommitHash(serverIndex)
			branch := gitBranch(serverIndex)
			repoKey := canonicalRepo(serverIndex)
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
					idx.SetRootPath(serverIndex)

					if len(snap.VectorIndex) > 0 && snap.VectorDims > 0 {
						if err := idx.ImportVectorIndex(snap.VectorIndex, snap.VectorDims, snap.VectorCount); err != nil {
							fmt.Fprintf(os.Stderr, "[gortex] server: vector index restore failed: %v\n", err)
						}
					}

					result, err := idx.IncrementalReindex(serverIndex)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[gortex] server: incremental reindex failed: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "[gortex] server: restored graph (%d nodes, %d edges), re-indexed %d stale files in %dms\n",
							result.NodeCount, result.EdgeCount, result.FileCount, result.DurationMs)
					}
					cached = true
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] server: cache load failed, will re-index: %v\n", err)
				}
			}

			if !cached {
				fmt.Fprintf(os.Stderr, "[gortex] server: indexing %s...\n", serverIndex)
				result, err := idx.Index(serverIndex)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] server: indexing failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "[gortex] server: indexed %d files (%d nodes, %d edges) in %dms\n",
					result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
			}
		}

		// Search backend is auto-updated via SearchProvider (idx.Search)

		// Set contract registry: in multi-repo mode, merge all per-repo registries.
		if mi != nil {
			srv.SetContractRegistry(mi.MergedContractRegistry())
		} else if cr := idx.ContractRegistry(); cr != nil {
			srv.SetContractRegistry(cr)
		}

		srv.RunAnalysis()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)

	select {
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] server: received %s, shutting down\n", sig)

		if serverIndex != "" {
			commitHash := gitCommitHash(serverIndex)
			if commitHash != "" {
				snap := &persistence.Snapshot{
					Version:    version,
					RepoPath:   canonicalRepo(serverIndex),
					CommitHash: commitHash,
					Branch:     gitBranch(serverIndex),
					IndexedAt:  time.Now(),
					Nodes:      g.AllNodes(),
					Edges:      g.AllEdges(),
					FileMtimes: idx.FileMtimes(),
				}
				snap.VectorIndex, snap.VectorDims, snap.VectorCount = idx.ExportVectorIndex()
				if err := store.Save(snap); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] server: cache save failed: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] server: saved graph snapshot (%d nodes, %d edges)\n",
						len(snap.Nodes), len(snap.Edges))
				}
			}
		}

		return httpServer.Close()
	}
}

// isLocalhostBind reports whether bind resolves to the loopback
// interface. An empty string means "all interfaces" and is not safe.
func isLocalhostBind(bind string) bool {
	switch bind {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// resolveServerID loads or creates the per-machine server id. When
// cacheDir is empty the id lives alongside other gortex cache files
// (~/.cache/gortex/server.id); otherwise cacheDir/server.id.
func resolveServerID(cacheDir string) (string, error) {
	path := filepath.Join(cacheDir, "server.id")
	if cacheDir == "" {
		def, err := server.DefaultServerIDPath()
		if err != nil {
			return "", err
		}
		path = def
	}
	return server.LoadOrCreateServerID(path)
}
