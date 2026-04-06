package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
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
	serveIndex     string
	serveTransport string
	servePort      int
	serveWatch     bool
	serveDebounce  int
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
	serveCmd.Flags().IntVar(&serveDebounce, "debounce", 150, "debounce delay in ms")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer logger.Sync()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)

	// Index if path provided.
	if serveIndex != "" {
		fmt.Fprintf(os.Stderr, "[gortex] indexing %s...\n", serveIndex)
		result, err := idx.Index(serveIndex)
		if err != nil {
			return fmt.Errorf("indexing failed: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[gortex] indexed %d files (%d nodes, %d edges) in %dms\n",
			result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
	}

	// Start watcher if requested.
	var watcher *indexer.Watcher
	if serveWatch {
		wcfg := cfg.Watch
		wcfg.Enabled = true
		if serveDebounce > 0 {
			wcfg.DebounceMs = serveDebounce
		}

		watcher, err = indexer.NewWatcher(idx, wcfg, logger)
		if err != nil {
			return fmt.Errorf("watcher setup failed: %w", err)
		}

		watchPaths := wcfg.Paths
		if len(watchPaths) == 0 && serveIndex != "" {
			watchPaths = []string{serveIndex}
		}
		if len(watchPaths) == 0 {
			watchPaths = []string{"."}
		}

		if err := watcher.Start(watchPaths); err != nil {
			return fmt.Errorf("watcher start failed: %w", err)
		}
		defer watcher.Stop()

		fmt.Fprintf(os.Stderr, "[gortex] watch mode active\n")
	}

	// Create hub for fan-out of watcher events (also handles logging).
	var eventHub *hub.Hub
	if watcher != nil {
		eventHub = hub.New()
		go eventHub.Run(watcher.Events())
		defer eventHub.Stop()
	}

	// Create and start MCP server.
	eng := query.NewEngine(g)
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, watcher, logger)

	// Run initial analysis (community detection + process discovery).
	srv.RunAnalysis()
	fmt.Fprintf(os.Stderr, "[gortex] MCP server ready (transport: %s)\n", serveTransport)

	// Start web visualization server.
	webSrv := web.NewServer(g, eng, eventHub, logger)
	go func() {
		webAddr := fmt.Sprintf(":%d", servePort)
		fmt.Fprintf(os.Stderr, "[gortex] web UI at http://localhost:%d\n", servePort)
		if err := webSrv.Start(webAddr); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[gortex] web server error: %v\n", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		webSrv.Shutdown(ctx)
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeStdio()
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] received %s, shutting down\n", sig)
		return nil
	}
}
