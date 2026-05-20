package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

var (
	evalPort     int
	evalIndex    string
	evalCacheDir string
)

var evalServerCmd = &cobra.Command{
	Use:   "eval-server",
	Short: "Start the eval HTTP server for benchmarking",
	Long:  "Starts an HTTP daemon wrapping Gortex MCP tools for evaluation. Exposes /health, /tool/{name}, /augment, and /stats endpoints.",
	RunE:  runEvalServer,
}

func init() {
	evalServerCmd.Flags().IntVar(&evalPort, "port", 4747, "HTTP port to listen on")
	evalServerCmd.Flags().StringVar(&evalIndex, "index", "", "repository path to index on startup")
	evalServerCmd.Flags().StringVar(&evalCacheDir, "cache-dir", "", "index cache directory (default ~/.gortex-eval-cache)")
	rootCmd.AddCommand(evalServerCmd)
}

func runEvalServer(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build the same graph/parser/indexer/query/MCP stack as serve.go.
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules)
	srv.SetArchitecture(cfg.Architecture)

	// Index the repository if --index is provided, with cache support.
	if evalIndex != "" {
		cache, err := eval.NewCache(evalCacheDir, version)
		if err != nil {
			return fmt.Errorf("creating index cache: %w", err)
		}

		// Derive repo name from directory name and get commit hash.
		repoName := filepath.Base(evalIndex)
		commitHash := gitCommitHash(evalIndex)

		cached := false
		if commitHash != "" {
			if cache.Check(repoName, commitHash) && cache.Validate(repoName, commitHash) {
				cachePath, err := cache.Load(repoName, commitHash)
				if err == nil {
					fmt.Fprintf(os.Stderr, "[gortex] eval-server: loaded cached index from %s\n", cachePath)
					cached = true
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] eval-server: cache load failed, will re-index: %v\n", err)
				}
			}
		}

		if !cached {
			fmt.Fprintf(os.Stderr, "[gortex] eval-server: indexing %s...\n", evalIndex)
			result, err := idx.Index(evalIndex)
			if err != nil {
				return fmt.Errorf("indexing %s: %w", evalIndex, err)
			}
			fmt.Fprintf(os.Stderr, "[gortex] eval-server: indexed %d files (%d nodes, %d edges) in %dms\n",
				result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)

			// Store to cache for future runs.
			if commitHash != "" {
				if err := cache.Store(repoName, commitHash, evalIndex); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] eval-server: cache store warning: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] eval-server: cached index for %s@%s\n", repoName, commitHash[:8])
				}
			}
		}
	}

	// Run analysis (communities, processes) after indexing.
	srv.RunAnalysis()

	// Wire the MCP server's tool dispatch into an HTTP handler.
	handler := eval.NewHandler(srv.MCPServer(), g, version, logger)

	addr := fmt.Sprintf(":%d", evalPort)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	fmt.Fprintf(os.Stderr, "[gortex] eval-server listening on http://localhost:%d\n", evalPort)

	// Start HTTP server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("eval-server: %w", err)
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] eval-server: received %s, shutting down\n", sig)
		return httpServer.Close()
	}
}

// gitCommitHash is defined in git.go
