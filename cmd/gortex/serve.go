package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/bridge"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/web"
	"github.com/zzet/gortex/internal/web/hub"
)

var (
	serveIndex      string
	serveTransport  string
	servePort       int
	serveWatch      bool
	serveWeb        bool
	serveBridge     bool
	serveCORSOrigin string
	serveDebounce   int
	serveTrack      []string
	serveProject    string
	serveCacheDir      string
	serveNoCache       bool
	serveEmbeddings    bool
	serveEmbeddingsURL string
	serveEmbeddingsModel string
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the MCP server",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&serveIndex, "index", "", "repository path to index on startup")
	serveCmd.Flags().StringVar(&serveTransport, "transport", "stdio", "transport: stdio")
	serveCmd.Flags().IntVar(&servePort, "port", 8765, "port for HTTP transport")
	serveCmd.Flags().BoolVar(&serveWatch, "watch", false, "keep graph in sync with filesystem changes")
	serveCmd.Flags().BoolVar(&serveWeb, "web", false, "start web visualization UI")
	serveCmd.Flags().IntVar(&serveDebounce, "debounce", 150, "debounce delay in ms")
	serveCmd.Flags().BoolVar(&serveBridge, "bridge", false, "start HTTP bridge API alongside MCP stdio")
	serveCmd.Flags().StringVar(&serveCORSOrigin, "cors-origin", "*", "allowed CORS origin for bridge API")
	serveCmd.Flags().StringSliceVar(&serveTrack, "track", nil, "additional repository paths to track")
	serveCmd.Flags().StringVar(&serveProject, "project", "", "active project name")
	serveCmd.Flags().StringVar(&serveCacheDir, "cache-dir", "", "graph cache directory (default ~/.cache/gortex/)")
	serveCmd.Flags().BoolVar(&serveNoCache, "no-cache", false, "disable graph caching")
	serveCmd.Flags().BoolVar(&serveEmbeddings, "embeddings", false, "enable semantic search (built-in word vectors or transformer if compiled in)")
	serveCmd.Flags().StringVar(&serveEmbeddingsURL, "embeddings-url", "", "embedding API URL (e.g. http://localhost:11434 for Ollama)")
	serveCmd.Flags().StringVar(&serveEmbeddingsModel, "embeddings-model", "", "embedding model name (default: auto-detect)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)

	// Set up embedding provider for semantic search.
	if serveEmbeddingsURL != "" {
		embedder := embedding.NewAPIProvider(serveEmbeddingsURL, serveEmbeddingsModel)
		idx.SetEmbedder(embedder)
		fmt.Fprintf(os.Stderr, "[gortex] semantic search enabled (API: %s)\n", serveEmbeddingsURL)
	} else if serveEmbeddings {
		embedder, err := embedding.NewLocalProvider()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] warning: embeddings disabled: %v\n", err)
		} else {
			idx.SetEmbedder(embedder)
			fmt.Fprintf(os.Stderr, "[gortex] semantic search enabled (local)\n")
		}
	}

	// Initialize ConfigManager for multi-repo support.
	cm, err := config.NewConfigManager("")
	if err != nil {
		// Non-fatal: fall back to single-repo mode.
		fmt.Fprintf(os.Stderr, "[gortex] warning: could not load global config: %v\n", err)
	}

	// Add --track repos to GlobalConfig.
	if cm != nil && len(serveTrack) > 0 {
		for _, trackPath := range serveTrack {
			absPath, err := filepath.Abs(trackPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not resolve --track path %s: %v\n", trackPath, err)
				continue
			}
			// Skip duplicates.
			if err := cm.Global().AddRepo(config.RepoEntry{Path: absPath}); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not add --track repo %s: %v\n", absPath, err)
			}
		}
	}

	// Determine active project.
	activeProject := serveProject
	if activeProject == "" && cm != nil {
		activeProject = cm.Global().ActiveProject
	}
	if cm != nil {
		cm.Global().ActiveProject = activeProject
	}

	// Initialize MultiIndexer when we have a ConfigManager.
	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
	}

	// Build multi-repo options for the MCP server.
	var multiOpts []gortexmcp.MultiRepoOptions
	if mi != nil || cm != nil {
		multiOpts = append(multiOpts, gortexmcp.MultiRepoOptions{
			MultiIndexer:  mi,
			ConfigManager: cm,
			ActiveProject: activeProject,
		})
	}

	// Create MCP server immediately so the stdio handshake can complete
	// before indexing (which may take time on large repos).
	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules, multiOpts...)

	fmt.Fprintf(os.Stderr, "[gortex] MCP server ready (transport: %s)\n", serveTransport)

	// Start bridge HTTP API if requested.
	if serveBridge {
		bridgeHandler := bridge.NewHandler(srv.MCPServer(), g, version, logger)
		corsOpts := bridge.CORSOptions{AllowOrigins: []string{serveCORSOrigin}}
		handler := bridge.WithCORS(bridgeHandler, corsOpts)
		go func() {
			bridgeAddr := fmt.Sprintf(":%d", servePort)
			fmt.Fprintf(os.Stderr, "[gortex] bridge API at http://localhost:%d\n", servePort)
			if err := http.ListenAndServe(bridgeAddr, handler); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "[gortex] bridge server error: %v\n", err)
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
	if serveNoCache {
		store = persistence.NopStore{}
	} else {
		var err error
		store, err = persistence.NewFileStore(serveCacheDir, version)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] warning: cache disabled: %v\n", err)
			store = persistence.NopStore{}
		}
	}

	// Background: index, watch, analyze — graph populates while MCP is live.
	go func() {
		if serveIndex != "" {
			commitHash := gitCommitHash(serveIndex)
			cached := false

			if commitHash != "" && store.Check(serveIndex, commitHash) && store.Validate(serveIndex, commitHash) {
				snap, err := store.Load(serveIndex, commitHash)
				if err == nil {
					for _, n := range snap.Nodes {
						g.AddNode(n)
					}
					for _, e := range snap.Edges {
						g.AddEdge(e)
					}
					idx.SetFileMtimes(snap.FileMtimes)
					idx.SetRootPath(serveIndex)

					// Restore vector index if available.
					if len(snap.VectorIndex) > 0 && snap.VectorDims > 0 {
						if err := idx.ImportVectorIndex(snap.VectorIndex, snap.VectorDims, snap.VectorCount); err != nil {
							fmt.Fprintf(os.Stderr, "[gortex] vector index restore failed: %v\n", err)
						}
					}

					result, err := idx.IncrementalReindex(serveIndex)
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
				fmt.Fprintf(os.Stderr, "[gortex] indexing %s...\n", serveIndex)
				result, err := idx.Index(serveIndex)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] indexing failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "[gortex] indexed %d files (%d nodes, %d edges) in %dms\n",
					result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
			}
		}

		// Start watcher if requested.
		if serveWatch {
			wcfg := cfg.Watch
			wcfg.Enabled = true
			if serveDebounce > 0 {
				wcfg.DebounceMs = serveDebounce
			}

			watcher, err := indexer.NewWatcher(idx, wcfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] watcher setup failed: %v\n", err)
				return
			}

			watchPaths := wcfg.Paths
			if len(watchPaths) == 0 && serveIndex != "" {
				watchPaths = []string{serveIndex}
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

			// Start web visualization server (only if --web flag is set).
			if serveWeb {
				webSrv := web.NewServer(g, eng, eventHub, logger)
				go func() {
					webAddr := fmt.Sprintf(":%d", servePort)
					fmt.Fprintf(os.Stderr, "[gortex] web UI at http://localhost:%d\n", servePort)
					if err := webSrv.Start(webAddr); err != nil && err != http.ErrServerClosed {
						fmt.Fprintf(os.Stderr, "[gortex] web server error: %v\n", err)
					}
				}()
			}
		} else if serveWeb {
			// Web without watch — no event hub needed.
			webSrv := web.NewServer(g, eng, nil, logger)
			go func() {
				webAddr := fmt.Sprintf(":%d", servePort)
				fmt.Fprintf(os.Stderr, "[gortex] web UI at http://localhost:%d\n", servePort)
				if err := webSrv.Start(webAddr); err != nil && err != http.ErrServerClosed {
					fmt.Fprintf(os.Stderr, "[gortex] web server error: %v\n", err)
				}
			}()
		}

		// Run initial analysis.
		srv.RunAnalysis()
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] received %s, shutting down\n", sig)

		// Persist graph snapshot on shutdown.
		if serveIndex != "" {
			commitHash := gitCommitHash(serveIndex)
			if commitHash != "" {
				snap := &persistence.Snapshot{
					Version:    version,
					RepoPath:   serveIndex,
					CommitHash: commitHash,
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
