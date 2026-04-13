package main

import (
	"context"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
)

// daemonState is the bundle of long-lived objects the daemon owns. One
// instance per running daemon; every session the daemon accepts shares
// these pointers.
type daemonState struct {
	graph         *graph.Graph
	indexer       *indexer.Indexer
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	mcpServer     *gortexmcp.Server
	// MultiWatcher is built by warmupDaemonState (after tracked repos
	// have been re-indexed) and handed to realController via
	// AttachWatcher — it isn't held on daemonState because no caller
	// reads it from here.
}

// buildDaemonState constructs the full object graph the daemon needs:
// graph → indexer → multi-indexer → engine → MCP server, plus feedback
// and savings persistence. Mirrors the setup in runServe() but without
// stdio transport wiring — the daemon hands frames to MCPServer.HandleMessage
// via the mcpDispatcher rather than going through server.ServeStdio.
//
// Any previously-tracked repos (from ~/.config/gortex/config.yaml) are
// loaded on startup so the daemon restarts pick up where it left off.
func buildDaemonState(logger *zap.Logger) (*daemonState, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// Warm-start from snapshot when one exists. Subsequent TrackRepo
	// calls re-index only the files that changed since the snapshot was
	// written, so restart cost is near-zero on steady-state repos.
	if _, err := loadSnapshot(g, logger); err != nil {
		logger.Warn("daemon: snapshot load failed", zap.Error(err))
	}

	idx := indexer.New(g, reg, cfg.Index, logger)

	// Embeddings: default to the bundled Hugot provider (pure-Go ONNX).
	// API-first if GORTEX_EMBEDDINGS_URL is set — lets the daemon share an
	// embedding server with bridge mode when both are in use.
	if url := os.Getenv("GORTEX_EMBEDDINGS_URL"); url != "" {
		idx.SetEmbedder(embedding.NewAPIProvider(url, os.Getenv("GORTEX_EMBEDDINGS_MODEL")))
	} else if embedder, embErr := embedding.NewLocalProvider(); embErr == nil {
		idx.SetEmbedder(embedder)
	} else {
		logger.Warn("daemon: embedding provider unavailable", zap.Error(embErr))
	}

	cm, err := config.NewConfigManager("")
	if err != nil {
		logger.Warn("daemon: could not load global config", zap.Error(err))
	}

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
	}

	// MCP server wiring. Multi-repo options are passed only when a
	// ConfigManager is available — otherwise the server runs in
	// single-repo mode and multi-repo tools return errors.
	var multiOpts []gortexmcp.MultiRepoOptions
	if mi != nil || cm != nil {
		multiOpts = append(multiOpts, gortexmcp.MultiRepoOptions{
			MultiIndexer:  mi,
			ConfigManager: cm,
			ActiveProject: "",
		})
	}

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules, multiOpts...)

	// Semantic manager, feedback, savings — same wiring as runServe.
	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
	}
	srv.InitFeedback("", "")

	if savingsStore, err := savings.Open(savings.DefaultPath()); err == nil {
		srv.InitSavings(savingsStore, "")
	} else {
		logger.Warn("daemon: savings persistence disabled", zap.Error(err))
	}

	// MultiWatcher is created in warmupDaemonState after tracked repos
	// have been re-indexed — NewMultiWatcher needs mi.AllMetadata() to be
	// populated to attach per-repo watchers. Until then, multiWatcher is
	// nil; queries still work, but file edits don't flow into the graph
	// for the few seconds warmup takes.

	return &daemonState{
		graph:         g,
		indexer:       idx,
		multiIndexer:  mi,
		configManager: cm,
		mcpServer:     srv,
	}, nil
}

// warmupDaemonState performs the per-repo TrackRepoCtx loop and brings
// up the MultiWatcher. Split out from buildDaemonState so the daemon can
// open its socket and accept connections before this work finishes —
// re-extracting contracts across many repos can take tens of seconds
// and there's no reason to make clients wait for it.
func warmupDaemonState(state *daemonState, logger *zap.Logger) *indexer.MultiWatcher {
	if state.multiIndexer == nil || state.configManager == nil {
		return nil
	}

	ctx := progress.WithReporter(context.Background(), progress.Nop{})
	for _, entry := range state.configManager.Global().Repos {
		if _, err := state.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
			logger.Warn("daemon: startup track failed",
				zap.String("path", entry.Path), zap.Error(err))
		}
	}

	watchCfgs := make(map[string]config.WatchConfig)
	for prefix := range state.multiIndexer.AllMetadata() {
		watchCfgs[prefix] = state.configManager.GetRepoConfig(prefix).Watch
	}
	mw, err := indexer.NewMultiWatcher(state.multiIndexer, watchCfgs, logger)
	if err != nil {
		logger.Warn("daemon: multi-watcher init failed", zap.Error(err))
		return nil
	}
	if err := mw.Start(); err != nil {
		logger.Warn("daemon: multi-watcher start failed", zap.Error(err))
		return nil
	}
	logger.Info("daemon: watching", zap.Int("repos", len(watchCfgs)))
	return mw
}
