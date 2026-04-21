package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
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
	// snapshotRepos carries per-repo FileMtimes restored from a daemon
	// snapshot. Populated by buildDaemonState; consumed by
	// warmupDaemonState to route each configured repo through
	// ReconcileRepoCtx (incremental) instead of TrackRepoCtx (full
	// index). nil or missing entries → fall back to full index.
	snapshotRepos map[string]*snapshotRepo
	// snapshotContracts carries the per-repo contract entries restored
	// from the snapshot. Warmup injects these into each indexer after
	// ReconcileRepoCtx when IncrementalReindex skipped re-extraction (no
	// stale files). Without this the per-repo contracts.Registry stays
	// nil for every quiescent repo, so `contracts` / `contracts check`
	// return empty results even though the graph holds the nodes.
	snapshotContracts map[string][]contracts.Contract
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
// isTruthyEnv returns true for the usual "yes" env-var spellings,
// case-insensitively: "1", "true", "yes", "on", "y". Anything else —
// including empty, "0", "false", "no", "off" — returns false.
func isTruthyEnv(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

func buildDaemonState(logger *zap.Logger) (*daemonState, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// Warm-start from snapshot when one exists. Subsequent
	// ReconcileRepoCtx calls re-index only the files that changed since
	// the snapshot was written, so restart cost is near-zero on
	// steady-state repos. The returned per-repo FileMtimes are what
	// make that incremental path viable — without them, warmup would
	// have no signal to distinguish "indexed and unchanged" from "new
	// on disk", treat everything as stale, and produce duplicate
	// nodes/edges on every restart (bug B1).
	loadResult, err := loadSnapshot(g, logger)
	if err != nil {
		logger.Warn("daemon: snapshot load failed", zap.Error(err))
	}

	idx := indexer.New(g, reg, cfg.Index, logger)

	// Embeddings are OPT-IN on the daemon. Enabled by any of:
	//   --embeddings (CLI flag on `gortex daemon start`)
	//   GORTEX_EMBEDDINGS=1 / true / yes / on (env var)
	//   GORTEX_EMBEDDINGS_URL=<url> (routes to an OpenAI-compat API)
	//
	// Why opt-in: the bundled Hugot MiniLM-L6-v2 model adds an ~87 MB
	// silent download on first use and blocks warmup for ~60 ms per
	// indexed symbol (≈8 min per 8k-symbol repo, hours on multi-repo
	// setups). On our own retrieval fixture, BM25 beats static-GloVe
	// semantic by 4×+ on every tier — most users never need semantic
	// at all, and those who do can opt in explicitly.
	var embedder embedding.Provider
	apiURL := os.Getenv("GORTEX_EMBEDDINGS_URL")
	switch {
	case apiURL != "":
		embedder = embedding.NewAPIProvider(apiURL, os.Getenv("GORTEX_EMBEDDINGS_MODEL"))
		logger.Info("daemon: embeddings enabled (api)", zap.String("url", apiURL))
	case daemonEmbeddings || isTruthyEnv("GORTEX_EMBEDDINGS"):
		if e, embErr := embedding.NewLocalProvider(); embErr == nil {
			embedder = e
			logger.Info("daemon: embeddings enabled (local)",
				zap.String("type", fmt.Sprintf("%T", e)),
				zap.Int("dim", e.Dimensions()))
		} else {
			logger.Warn("daemon: embeddings requested but unavailable", zap.Error(embErr))
		}
	default:
		logger.Info("daemon: embeddings disabled (default) — set --embeddings or GORTEX_EMBEDDINGS=1 to enable")
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
	}

	cm, err := config.NewConfigManager("")
	if err != nil {
		logger.Warn("daemon: could not load global config", zap.Error(err))
	}

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
		// Without this, every per-repo Indexer created inside TrackRepoCtx
		// has embedder=nil and buildSearchIndex skips the vector pass —
		// daemon-tracked repos end up with text-only search.
		if embedder != nil {
			mi.SetEmbedder(embedder)
		}
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
		snapshotRepos:     loadResult.Repos,
		snapshotContracts: loadResult.Contracts,
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
		// Route repos whose nodes came from the snapshot through
		// ReconcileRepoCtx — it calls IncrementalReindex, which
		// evicts files deleted while the daemon was down and
		// re-indexes only files whose mtime changed. Repos not in
		// the snapshot (newly tracked, or first startup after a
		// schema bump) fall back to TrackRepoCtx, which does a full
		// walk. Both paths end with the repo registered on the
		// MultiIndexer and its contract edges reconciled.
		priorMtimes := priorMtimesForEntry(state.snapshotRepos, entry)
		if priorMtimes != nil {
			if _, err := state.multiIndexer.ReconcileRepoCtx(ctx, entry, priorMtimes); err != nil {
				logger.Warn("daemon: startup reconcile failed",
					zap.String("path", entry.Path), zap.Error(err))
			}
			continue
		}
		if _, err := state.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
			logger.Warn("daemon: startup track failed",
				zap.String("path", entry.Path), zap.Error(err))
		}
	}

	// Rehydrate per-repo contract registries from the snapshot. Only
	// target indexers whose registry is still nil — a non-nil registry
	// means IncrementalReindex (or a fresh TrackRepoCtx) re-extracted
	// contracts from source, and that result is authoritative. Without
	// this, every steady-state repo's ContractRegistry stays nil and
	// MergedContractRegistry skips them, so `contracts` returns only
	// the contracts of repos whose files happened to change since the
	// last shutdown.
	if len(state.snapshotContracts) > 0 {
		injectedRepos, injectedCount := 0, 0
		for prefix := range state.multiIndexer.AllMetadata() {
			idx := state.multiIndexer.GetIndexer(prefix)
			if idx == nil || idx.ContractRegistry() != nil {
				continue
			}
			cs, ok := state.snapshotContracts[prefix]
			if !ok || len(cs) == 0 {
				continue
			}
			reg := contracts.NewRegistry()
			for _, c := range cs {
				reg.Add(c)
			}
			idx.SetContractRegistry(reg)
			injectedRepos++
			injectedCount += len(cs)
		}
		if injectedRepos > 0 {
			logger.Info("daemon: rehydrated contract registries from snapshot",
				zap.Int("repos", injectedRepos),
				zap.Int("contracts", injectedCount))
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

// priorMtimesForEntry finds the snapshotted FileMtimes map for a
// configured repo entry, matching on absolute RootPath. Falls back to
// prefix-based lookup when no path match is found — useful if the
// user's config moved but the prefix is stable. Returns nil when no
// match exists (first startup, schema bump, or newly-added repo).
func priorMtimesForEntry(repos map[string]*snapshotRepo, entry config.RepoEntry) map[string]int64 {
	if len(repos) == 0 {
		return nil
	}
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		absPath = entry.Path
	}
	for _, r := range repos {
		if r == nil {
			continue
		}
		if r.RootPath == absPath {
			return r.FileMtimes
		}
	}
	if prefix := config.ResolvePrefix(entry); prefix != "" && prefix != "." {
		if r := repos[prefix]; r != nil {
			return r.FileMtimes
		}
	}
	return nil
}

// collectSnapshotRepos snapshots the per-repo metadata needed to
// reconcile the next startup: RepoPrefix, RootPath, and FileMtimes.
// Called from the shutdown and periodic-snapshot paths so restart
// warmups can run IncrementalReindex instead of a full walk.
func collectSnapshotRepos(mi *indexer.MultiIndexer) []snapshotRepo {
	if mi == nil {
		return nil
	}
	meta := mi.AllMetadata()
	if len(meta) == 0 {
		return nil
	}
	out := make([]snapshotRepo, 0, len(meta))
	for prefix, m := range meta {
		if m == nil {
			continue
		}
		// Copy the mtimes map — saveSnapshot encodes asynchronously
		// on shutdown and we don't want a late watcher event mutating
		// the live map mid-encode.
		mtimes := make(map[string]int64, len(m.FileMtimes))
		for k, v := range m.FileMtimes {
			mtimes[k] = v
		}
		out = append(out, snapshotRepo{
			RepoPrefix: prefix,
			RootPath:   m.RootPath,
			FileMtimes: mtimes,
		})
	}
	return out
}

// collectSnapshotContracts flattens every per-repo contract registry
// into a single wire-form slice ordered by repo prefix. The warmup path
// will redistribute by RepoPrefix when loading, so cross-repo ordering
// is irrelevant here; the stable per-prefix grouping just keeps logs
// and diffs readable. Called at the same points as collectSnapshotRepos
// so the header counts and the repo/contract records agree.
func collectSnapshotContracts(mi *indexer.MultiIndexer) []snapshotContract {
	if mi == nil {
		return nil
	}
	prefixes := make([]string, 0)
	for prefix := range mi.AllMetadata() {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	var out []snapshotContract
	for _, prefix := range prefixes {
		idx := mi.GetIndexer(prefix)
		if idx == nil {
			continue
		}
		reg := idx.ContractRegistry()
		if reg == nil {
			continue
		}
		for _, c := range reg.All() {
			out = append(out, toSnapshotContract(c))
		}
	}
	return out
}
