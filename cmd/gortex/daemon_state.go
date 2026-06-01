package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/semantic/scip"
)

// daemonState is the bundle of long-lived objects the daemon owns. One
// instance per running daemon; every session the daemon accepts shares
// these pointers.
type daemonState struct {
	graph         graph.Store
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
	// snapshotPartial reports that the load shed stale records (dropped
	// nodes / dropped edges whose target vanished). When true, warmup
	// forces a full per-repo ResolveAll across every indexer instead of
	// the incremental "only files whose mtime changed" path. Without
	// this, edges that the loader dropped never come back — every
	// restart erodes the graph further until exported methods like
	// (*Node).Type show zero callers despite having dozens of real
	// callers in source. The IncrementalReindex path never re-resolves
	// unchanged files, so the lost edges are invisible to it.
	snapshotPartial bool
	// snapshotVector carries the workspace-global semantic-search
	// vector index restored from the snapshot. When its Index is
	// non-empty and an embedder is configured, warmupDaemonState
	// restores it after the per-repo re-index loop (which it runs with
	// vector building skipped) instead of re-embedding the whole graph.
	snapshotVector snapshotVector
	// MultiWatcher is built by warmupDaemonState (after tracked repos
	// have been re-indexed) and handed to realController via
	// AttachWatcher — it isn't held on daemonState because no caller
	// reads it from here.

	// resolverLSPRegistry composes per-repo ResolverHelpers consulted
	// by the cross-file resolver's hot path. Populated as repos
	// are tracked so each tsserver instance is scoped to its owning
	// workspace. nil when the resolve-time LSP path is disabled
	// (GORTEX_LSP_RESOLVER=0) or when semantic enrichment is off.
	resolverLSPRegistry *lsp.ResolverHelperRegistry

	// lspRouter is the daemon-shared LSP server pool. Held here so
	// the warmup loop can register per-repo helpers via
	// ResolverHelperRegistry without re-deriving the router from the
	// semantic manager.
	lspRouter *lsp.Router
}

// buildDaemonState constructs the full object graph the daemon needs:
// graph → indexer → multi-indexer → engine → MCP server, plus feedback
// and savings persistence. Mirrors the setup in runServe() but without
// stdio transport wiring — the daemon hands frames to MCPServer.HandleMessage
// via the mcpDispatcher rather than going through server.ServeStdio.
//
// Any previously-tracked repos (from ~/.config/gortex/config.yaml) are
// loaded on startup so the daemon restarts pick up where it left off.
// isFalsyEnv returns true when the env var is explicitly set to one
// of the "no" spellings: "0", "false", "no", "off", "n". An unset or
// empty env returns false (default-on semantics for opt-out flags).
func isFalsyEnv(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "0", "false", "no", "off", "n":
		return true
	default:
		return false
	}
}

// lspDisabledSet builds the set of LSP spec names that should NOT be
// auto-registered by Router.RegisterAvailable. Two inputs are merged:
//
//  1. Per-spec config overrides — any entry in `semantic.providers`
//     with `enabled: false` whose name matches a known LSP spec.
//     Already-disabled-by-config users keep their opt-out without
//     having to also set the env var.
//  2. The GORTEX_LSP_DISABLE env var — comma-separated spec names.
//     The literal value "all" or "*" disables auto-registration
//     entirely (the explicit-config loop above still runs).
//
// The special key "__all__" in the returned map signals
// "skip auto-register everywhere" and is checked separately by
// callers; per-spec keys carry the spec.Name.
func lspDisabledSet(providers []config.SemanticProviderConfig, envVar string) map[string]bool {
	out := map[string]bool{}
	for _, pc := range providers {
		if pc.Enabled {
			continue
		}
		// Only consider entries that resolve to a known LSP spec —
		// otherwise an `enabled: false` for a SCIP indexer or a
		// custom daemon would silently shadow an LSP of the same
		// name (rare in practice, but the registry-membership check
		// makes the intent explicit).
		if lsp.SpecByName(pc.Name) != nil {
			out[pc.Name] = true
		}
	}
	for _, raw := range strings.Split(envVar, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "all") || name == "*" {
			out["__all__"] = true
			continue
		}
		out[name] = true
	}
	return out
}

func buildDaemonState(logger *zap.Logger) (*daemonState, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Load user-defined domain-extractor rules — TOML files of
	// tree-sitter patterns surfaced through `analyze kind=domain`.
	for _, pattern := range cfg.RuleFiles {
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, rp := range matches {
			if n, lerr := astquery.LoadUserRulesFile(rp); lerr != nil {
				logger.Warn("daemon: failed to load domain rule file",
					zap.String("file", rp), zap.Error(lerr))
			} else if n > 0 {
				logger.Info("daemon: loaded domain-extractor rules",
					zap.String("file", rp), zap.Int("rules", n))
			}
		}
	}

	g, backendCleanup, err := openBackend(daemonBackend, daemonBackendPath, resolveDaemonBufferPoolMB(), logger)
	if err != nil {
		return nil, fmt.Errorf("opening backend %q: %w", daemonBackend, err)
	}
	// Cleanup runs at daemon shutdown via the returned state's
	// teardown chain (see DaemonState.Close); store it on the
	// state so deferred close fires after every other shutdown
	// step (snapshot save, etc.).
	defer func() {
		if err != nil {
			backendCleanup()
		}
	}()

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	languages.RegisterCustomGrammars(reg, cfg.Index.Grammars, logger)

	// Warm-start from snapshot when one exists. Subsequent
	// ReconcileRepoCtx calls re-index only the files that changed since
	// the snapshot was written, so restart cost is near-zero on
	// steady-state repos. The returned per-repo FileMtimes are what
	// make that incremental path viable — without them, warmup would
	// have no signal to distinguish "indexed and unchanged" from "new
	// on disk", treat everything as stale, and produce duplicate
	// nodes/edges on every restart (bug B1).
	//
	// Two snapshot shapes:
	//
	//   - Memory backend: full graph replay (loadSnapshot). The
	//     gob+gzip dump IS the persistence layer; nodes + edges are
	//     replayed into the empty *graph.Graph.
	//
	//   - Persistent backend (sqlite): metadata-only load
	//     (loadSnapshotMetadata). The graph already lives in the
	//     backend's own on-disk store, so the snapshot only needs to
	//     carry the data the backend doesn't track — per-repo
	//     FileMtimes, contract registries, vector index. Skipping the
	//     load entirely (the previous behaviour) left priorMtimes
	//     empty and routed every warm restart through a full
	//     TrackRepoCtx → BulkUpsertSymbolFTS reindex path.
	var loadResult snapshotLoadResult
	if mg, ok := g.(*graph.Graph); ok {
		loadResult, err = loadSnapshot(mg, logger)
		if err != nil {
			logger.Warn("daemon: snapshot load failed", zap.Error(err))
		}
	}
	// Disk-backed daemons don't read a metadata snapshot: per-
	// repo FileMtimes live in the FileMtime sidecar table (loaded
	// per-repo by priorMtimesFromStore in the parallel_parse loop
	// below), KindContract nodes carry the rich contract record on
	// Node.Meta (rehydrated via contracts.LoadRegistryFromGraph),
	// and vector queries route to the backend's native vector index.
	// The legacy gob round-trip is now memory-backend-only.

	idx := indexer.New(g, reg, cfg.Index, logger)

	// Locals carrying resolve-time LSP wiring out of the optional
	// semantic-enrichment block so the daemonState constructor
	// below sees them whether or not the block ran.
	var (
		resolverLSPRegistry *lsp.ResolverHelperRegistry
		lspRouterOut        *lsp.Router
	)

	// Semantic enrichment (opt-in via .gortex.yaml `semantic.enabled:
	// true`). Mirrors the wiring in `gortex mcp` / `gortex server`: a
	// daemon-managed LSP router owns subprocess lifecycle, SCIP and
	// goanalysis providers register eagerly, and Manager.SetLSPRouter
	// installs the bridge so EnrichAll can lazy-spawn LSPs on demand.
	if cfg.Semantic.Enabled {
		semCfg := semantic.Config{
			Enabled:           cfg.Semantic.Enabled,
			TimeoutSeconds:    cfg.Semantic.TimeoutSeconds,
			EnrichOnWatch:     cfg.Semantic.EnrichOnWatch,
			RefuteUnconfirmed: cfg.Semantic.RefuteUnconfirmed,
		}
		for _, pc := range cfg.Semantic.Providers {
			out := semantic.ProviderConfig{
				Name:      pc.Name,
				Languages: pc.Languages,
				Command:   pc.Command,
				Args:      pc.Args,
				Env:       pc.Env,
				Mode:      pc.Mode,
				Daemon:    pc.Daemon,
				Priority:  pc.Priority,
				Enabled:   pc.Enabled,
			}
			if pc.Connect != nil {
				out.Connect = &semantic.ConnectConfig{
					Network:       pc.Connect.Network,
					Address:       pc.Connect.Address,
					FallbackSpawn: pc.Connect.FallbackSpawn,
				}
			}
			semCfg.Providers = append(semCfg.Providers, out)
		}
		semMgr := semantic.NewManager(semCfg, logger)

		goProvider := goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, logger)
		semMgr.RegisterProvider(goProvider)
		contracts.SetBindingResolver(goProvider)

		lspWorkspace, _ := os.Getwd()
		lspRouter := lsp.NewRouter(lspWorkspace, logger).
			WithIdleTimeout(10 * time.Minute).
			WithReaperInterval(time.Minute).
			WithMaxAlive(6).
			WithAdditionalWorkspaceFolders(cfg.Semantic.AdditionalWorkspaceFolders)
		semMgr.SetLSPRouter(lspRouter)

		for _, pc := range semCfg.Providers {
			if !pc.Enabled {
				continue
			}
			switch {
			case strings.HasPrefix(pc.Name, "scip-") && pc.Command != "":
				scipProv := scip.NewProvider(pc.Command, pc.Args, pc.Languages, semCfg.TimeoutSeconds, logger)
				// `mode: definitions` selects the definitions-only fast
				// path — the C# / .NET coverage-helper use case.
				if pc.Mode == "definitions" {
					scipProv = scipProv.WithDefinitionsOnly()
				}
				semMgr.RegisterProvider(scipProv)
			case lsp.SpecByName(pc.Name) != nil:
				// Apply any command / args / env / connect overrides
				// from .gortex.yaml — this is how a user pins a JRE
				// for jdtls or wires the IDE-coexistence path that
				// dials the editor's already-running LSP instead of
				// spawning a duplicate subprocess.
				var connect *lsp.ConnectSpec
				if pc.Connect != nil {
					connect = &lsp.ConnectSpec{
						Network:       pc.Connect.Network,
						Address:       pc.Connect.Address,
						FallbackSpawn: pc.Connect.FallbackSpawn,
					}
				}
				lspRouter.RegisterSpec(lsp.SpecWithOverridesConnect(
					lsp.SpecByName(pc.Name), pc.Command, pc.Args, pc.Env, connect))
			case pc.Daemon:
				semMgr.RegisterProvider(lsp.NewProvider(pc.Command, pc.Args, pc.Languages, pc.Daemon, pc.MaxParallel, logger))
			}
		}

		// Auto-register every known LSP spec whose binary resolves on
		// PATH. Compiler-grade providers (gopls, tsserver, pyright,
		// rust-analyzer, clangd, jdtls, …) should be on by default —
		// users get diagnostics / code actions / find_implementations
		// without learning the YAML knob, and the lazy-spawn router
		// keeps cost at "one cached PATH lookup" until a tool actually
		// calls into a spec. Per-spec opt-out works two ways:
		//   - .gortex.yaml: `semantic.providers: [{ name: gopls,
		//     enabled: false }]` — config-side disable.
		//   - GORTEX_LSP_DISABLE=gopls,tsserver — env-var quick kill.
		// GORTEX_LSP_DISABLE=all (or =*) disables auto-register
		// entirely while still honoring the explicit-config loop above.
		disabled := lspDisabledSet(cfg.Semantic.Providers, os.Getenv("GORTEX_LSP_DISABLE"))
		var autoRegistered []string
		if !disabled["__all__"] {
			autoRegistered = lspRouter.RegisterAvailable(disabled)
		}

		idx.SetSemanticManager(semMgr)

		// Resolve-time LSP hot path. The resolver consults this
		// registry for TS/JS/JSX/TSX edges before falling back to AST
		// heuristics. The registry holds per-repo ResolverHelpers
		// (built lazily from the same router that backs the enricher
		// path), so the daemon honours the same disable knobs and
		// PATH-availability semantics as the enricher. Disabled when
		// GORTEX_LSP_RESOLVER=0/false/no (env-var quick kill) or when
		// every TS-family spec is explicitly opted out via
		// GORTEX_LSP_DISABLE.
		if !isFalsyEnv("GORTEX_LSP_RESOLVER") {
			resolverLSPRegistry = lsp.NewResolverHelperRegistry()
			lspRouterOut = lspRouter
			idx.SetResolverLSPHelper(resolverLSPRegistry)
			logger.Info("daemon: resolve-time LSP hot path enabled")
		} else {
			logger.Info("daemon: resolve-time LSP hot path disabled (GORTEX_LSP_RESOLVER=0)")
		}

		logger.Info("daemon: semantic enrichment enabled",
			zap.Int("providers", len(semCfg.Providers)),
			zap.Strings("lsp_auto_registered", autoRegistered))
	}

	// Embeddings on the daemon follow the precedence explicit flag/env
	// > `embedding:` config > default. The default is semantic search
	// ON with the static GloVe provider — it needs zero download and
	// is CPU-only, so the zero-config promise still holds. An explicit
	// opt-in to the transformer backend (`provider: local`, or
	// GORTEX_EMBEDDINGS with no config) still pays the ~87 MB MiniLM
	// download on first use and the per-symbol warmup cost; static is
	// the default precisely to avoid that.
	embedder, embDesc, embErr := resolveEmbedder(embedderRequest{
		flagChanged: daemonEmbeddingsChanged,
		flagEnabled: daemonEmbeddings,
	}, cfg)
	switch {
	case embErr != nil:
		logger.Warn("daemon: embeddings requested but unavailable", zap.Error(embErr))
	case embedder != nil:
		logger.Info("daemon: embeddings enabled",
			zap.String("provider", embDesc),
			zap.Int("dim", embedder.Dimensions()))
	default:
		logger.Info("daemon: embeddings disabled — set embedding.enabled: false in config, or pass --embeddings / GORTEX_EMBEDDINGS to override")
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
		idx.SetEmbeddingChunkOptions(embeddingChunkOptions(cfg))
		idx.SetEmbeddingMaxSymbols(cfg.Embedding.MaxSymbols)
		idx.SetEmbeddingAPIConcurrency(cfg.Embedding.APIConcurrency)
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
			mi.SetEmbeddingChunkOptions(embeddingChunkOptions(cfg))
			mi.SetEmbeddingMaxSymbols(cfg.Embedding.MaxSymbols)
			mi.SetEmbeddingAPIConcurrency(cfg.Embedding.APIConcurrency)
			// When the snapshot already carries the workspace vector
			// index and its dimensionality matches the active embedder,
			// run the warmup re-index with vector building skipped —
			// warmupDaemonState restores the cached index in one shot
			// afterwards. Re-embedding the whole graph only to overwrite
			// it with the cache is the dominant default-on restart cost.
			vec := loadResult.Vector
			if len(vec.Index) > 0 && vec.Dims == embedder.Dimensions() {
				mi.SetSkipVectorBuild(true)
				logger.Info("daemon: snapshot carries vector index — warmup will restore it instead of re-embedding",
					zap.Int("vectors", vec.Count),
					zap.Int("dims", vec.Dims))
			}
		}
		// Propagate the resolve-time LSP helper so every per-repo
		// Indexer constructed by the MultiIndexer's warmup /
		// TrackRepoCtx / ReconcileRepoCtx paths participates in the
		// resolve-time LSP path (and so the global post-pass resolver
		// in RunDeferredPassesAll picks up LSP precision too).
		if resolverLSPRegistry != nil {
			mi.SetResolverLSPHelper(resolverLSPRegistry)

			// Install a per-track hook so runtime-tracked repos
			// (via the track_repository MCP tool) also register
			// a per-repo helper without requiring a daemon
			// restart. The hook is a no-op when no tsserver is
			// on PATH.
			if lspRouterOut != nil {
				routerRef := lspRouterOut
				registryRef := resolverLSPRegistry
				poolSize := lsp.ResolverPoolSizeFromEnv(1)
				mi.SetOnRepoTracked(func(prefix, absPath string) {
					tsSpec := lsp.SpecByName("typescript-language-server")
					if tsSpec == nil || !routerRef.Available(tsSpec) {
						return
					}
					if !repoLikelyHasTypeScriptIntent(absPath) {
						return
					}
					absRootCapture := absPath
					helper := buildResolverLSPHelper(routerRef, tsSpec, absRootCapture, poolSize, logger)
					registryRef.Register(prefix, helper)
				})
			}
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
	eng.ApplyRerankWeights(cfg.Search.Weights)
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules, multiOpts...)
	srv.SetArchitecture(cfg.Architecture)
	srv.SetArtifacts(cfg.Artifacts)
	srv.SetNamedQueries(cfg.Queries)
	srv.SetSearchConfig(cfg.Search)

	// Editor-overlay manager. Idle TTL resolved via
	// GORTEX_OVERLAY_IDLE_TTL > daemon.DefaultOverlayIdleTTL (30m).
	// Server.ReleaseSession (called on MCP-client disconnect) drops
	// the overlay synchronously, so the TTL is only the fallback
	// path for missed disconnects.
	overlays := daemon.NewOverlayManager(daemon.OverlayIdleTTLFromEnv(0))
	srv.SetOverlayManager(overlays)

	// Semantic manager, feedback, savings — same wiring as runServe.
	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
		srv.SetLSPDiagnosticsBroadcasting()
	}
	srv.InitFeedback("", "")
	srv.InitNotes("", "")
	srv.InitMemories("", "")
	// Daemon mode has no single repo to anchor a per-repo notebook
	// against, but the agent still wants persistence across daemon
	// restarts and shared visibility across sessions. Fall back to a
	// global notebook under the unified data dir; CLI mode keeps the
	// per-repo .gortex/notebook/ path wired in cmd/gortex/mcp.go.
	srv.InitNotebook(filepath.Join(platform.DataDir(), "notebook-cache"))
	srv.InitCombo("", "", gortexmcp.ModeAI)
	srv.InitFrecency("", "", gortexmcp.ModeAI)

	if savingsStore, err := savings.Open(savings.DefaultPath()); err == nil {
		srv.InitSavings(savingsStore, "")
	} else {
		logger.Warn("daemon: savings persistence disabled", zap.Error(err))
	}

	// LLM service (opt-in via the `.gortex.yaml` `llm:` block,
	// `~/.config/gortex/config.yaml::llm:`, or GORTEX_LLM_* env vars).
	// Repo-local config wins per non-zero field; the global config
	// fills the rest; env overrides land last inside SetupLLM via
	// MergeEnv. The active provider is chosen by `llm.provider`
	// (local / anthropic / openai / ollama / claudecli / gemini /
	// bedrock / deepseek). No-op when the active provider has no model
	// configured; a provider that fails to construct (e.g. "local"
	// without `-tags llama`, a missing API key, "claudecli" without
	// the `claude` binary on PATH, "bedrock" without AWS creds) is logged and
	// the service stays disabled.
	gc, _ := config.LoadGlobal()
	srv.SetupLLM(gc.MergeLLMInto(cfg.LLM))

	// MultiWatcher is created in warmupDaemonState after tracked repos
	// have been re-indexed — NewMultiWatcher needs mi.AllMetadata() to be
	// populated to attach per-repo watchers. Until then, multiWatcher is
	// nil; queries still work, but file edits don't flow into the graph
	// for the few seconds warmup takes.

	return &daemonState{
		graph:               g,
		indexer:             idx,
		multiIndexer:        mi,
		configManager:       cm,
		mcpServer:           srv,
		snapshotRepos:       loadResult.Repos,
		snapshotContracts:   loadResult.Contracts,
		snapshotPartial:     loadResult.Partial,
		snapshotVector:      loadResult.Vector,
		resolverLSPRegistry: resolverLSPRegistry,
		lspRouter:           lspRouterOut,
	}, nil
}

// repoLikelyHasTypeScriptIntent returns true when the repo root
// carries one of the canonical TS / JS project markers: tsconfig.json,
// jsconfig.json, or package.json. Used as a registration-time gate
// so a Go-only repo with a stray .ts file (a Playwright config, a
// vendored test fixture) doesn't trigger a full tsserver spawn the
// first time the resolver hits that file. Repos that fail the check
// fall through to AST-only resolution for TS/JS edges — degraded
// gracefully rather than booting a per-workspace tsserver process
// that scans the whole tree.
//
// Cheap: three stat calls per repo at warmup time. Misses are biased
// safe — a workspace with TS code but none of these markers is rare,
// and even then the cost is "no LSP precision" rather than incorrect
// behaviour.
func repoLikelyHasTypeScriptIntent(absRoot string) bool {
	for _, marker := range []string{"tsconfig.json", "jsconfig.json", "package.json"} {
		if _, err := os.Stat(filepath.Join(absRoot, marker)); err == nil {
			return true
		}
	}
	return false
}

// buildResolverLSPHelper constructs the resolve-time LSP helper for a
// workspace, choosing between the router-cached lazy path (poolSize
// ≤ 1) and the fresh-spawn pool path (poolSize > 1).
//
// Why the branch matters: the router-cached path reuses Router's
// existing idle reaper — workspaces that go quiet release their
// tsserver. A naive multi-provider pool keeps every spawn alive for
// the process lifetime; multiplied across 400+ tracked workspaces
// that's hundreds of GB of resident tsserver state. Until we have
// reaping in the pool itself, multi-provider mode is opt-in
// (GORTEX_LSP_POOL_SIZE > 1) and the recommendation is to use it only
// when the tracked-workspace count is small.
func buildResolverLSPHelper(router *lsp.Router, spec *lsp.ServerSpec, absRoot string, poolSize int, logger *zap.Logger) *lsp.ResolverHelper {
	if poolSize <= 1 {
		return lsp.NewLazyResolverHelper(
			func() (*lsp.Provider, error) {
				return router.ForSpecWorkspace(spec, absRoot)
			},
			absRoot,
			spec.Extensions,
			0,
			logger,
		)
	}
	return lsp.NewPooledResolverHelper(
		func() (*lsp.Provider, error) {
			return lsp.SpawnProviderForResolver(spec, absRoot, logger)
		},
		absRoot,
		spec.Extensions,
		0,
		poolSize,
		logger,
	)
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
	// BeginParallelBatch / EndBatch tells every per-repo Indexer
	// constructed inside the loop to skip both the graph-wide
	// derivation passes (InferImplements / InferOverrides /
	// markTestSymbolsAndEmitEdges) AND the per-repo cross-cutting
	// passes (ResolveAll / semantic enrich / contract extract+commit).
	// The latter mutate the shared graph in ways that race when
	// goroutines run them concurrently across repos, so the parallel
	// loop below just parses; RunDeferredPassesAll drains the deferred
	// per-repo passes serially before the global resolve. Without this
	// batch wrapper, a 100+ repo warmup is O(R · global_size).
	state.multiIndexer.BeginParallelBatch()

	repos := state.configManager.Global().Repos

	// Register a per-repo resolver-time LSP helper for every
	// tracked repo BEFORE the parallel warmup loop fires. The
	// helpers are lazy: tsserver isn't spawned until the resolver
	// asks for a TS/JS edge resolution, so there's no startup cost
	// for repos with no TS code.
	if state.resolverLSPRegistry != nil && state.lspRouter != nil {
		tsSpec := lsp.SpecByName("typescript-language-server")
		if tsSpec != nil && state.lspRouter.Available(tsSpec) {
			poolSize := lsp.ResolverPoolSizeFromEnv(1)
			registered, skipped := 0, 0
			for _, entry := range repos {
				absRoot, err := filepath.Abs(entry.Path)
				if err != nil {
					continue
				}
				if !repoLikelyHasTypeScriptIntent(absRoot) {
					skipped++
					continue
				}
				prefix := strings.TrimPrefix(config.ResolvePrefix(entry), "/")
				absRootCapture := absRoot
				helper := buildResolverLSPHelper(state.lspRouter, tsSpec, absRootCapture, poolSize, logger)
				state.resolverLSPRegistry.Register(prefix, helper)
				registered++
			}
			logger.Info("daemon: resolve-time LSP helpers registered",
				zap.Int("ts_repos", registered),
				zap.Int("non_ts_repos_skipped", skipped),
				zap.Int("pool_size", poolSize))
		}
	}
	// Bounded worker pool — disk I/O dominates parsing for most repos,
	// but a few CPU-heavy ones overlap with disk waits on others. NumCPU
	// gives good throughput on local SSDs without thrashing slow
	// external mounts (which dominate at this scale). Capped so a 32-core
	// box doesn't over-subscribe a single spinning drive.
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 12 {
		workers = 12
	}
	if workers > len(repos) {
		workers = len(repos)
	}
	logger.Info("daemon: warmup phase start",
		zap.String("phase", "parallel_parse"),
		zap.Int("repos", len(repos)),
		zap.Int("workers", workers),
		zap.Bool("snapshot_partial_forces_full_walk", state.snapshotPartial))
	publishReadinessPhase(state, "parallel_parse", false, map[string]any{
		"tracked_repos": len(repos),
		"workers":       workers,
	})
	phaseStart := time.Now()

	jobs := make(chan config.RepoEntry, len(repos))
	var wg sync.WaitGroup
	// changedRepos counts repos that actually did indexing work this
	// warmup: a cold full-track, or a reconcile that re-indexed / evicted
	// at least one file. When it stays zero, NOTHING on disk changed
	// since the last shutdown, so the persisted graph already holds every
	// resolved and derived edge — the global resolution passes below
	// (RunDeferredPassesAll / RunGlobalResolve / RunGlobalGraphPasses) are
	// pure recomputation and get skipped, which is what makes a true warm
	// restart near-instant instead of replaying the full cold-warmup cost.
	var changedRepos atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				// Per-entry panic guard so one repo's crash during
				// reindex doesn't kill the worker — the bad repo logs
				// and skips, the worker proceeds to the next job, and
				// warmup completes.
				func(entry config.RepoEntry) {
					defer func() {
						if r := recover(); r != nil {
							logger.Error("daemon: warmup repo panic recovered",
								zap.String("path", entry.Path),
								zap.Any("panic", r))
						}
					}()
					// Route repos whose nodes came from the snapshot through
					// ReconcileRepoCtx — it calls IncrementalReindex, which
					// evicts files deleted while the daemon was down and
					// re-indexes only files whose mtime changed. Repos not in
					// the snapshot (newly tracked, or first startup after a
					// schema bump) fall back to TrackRepoCtx, which does a
					// full walk. Both paths end with the repo registered on
					// the MultiIndexer; contract reconciliation is deferred
					// to the single RunGlobalResolve call below.
					//
					// snapshotPartial == true forces the full-walk path even
					// when prior mtimes exist: the partial-load signal means
					// the persisted resolution state is no longer trustworthy
					// (stale edges were dropped because their targets vanished),
					// and the incremental path only re-resolves files whose
					// mtime changed — so the dropped edges would never come
					// back. Without this override every restart progressively
					// erodes the graph until exported methods show zero
					// callers despite having dozens of real call sites.
					repoStart := time.Now()
					// Prefer mtimes stored in the backend's FileMtime
					// sidecar table — that lifts the persistence off the
					// gob snapshot for disk-backed backends, which is the
					// path that actually rebuilds across restarts. Falls
					// back to the snapshot's per-repo FileMtimes when the
					// backend doesn't implement the reader (memory) or
					// hasn't seen this repo yet.
					priorMtimes := priorMtimesFromStore(state.graph, entry, logger)
					if len(priorMtimes) == 0 {
						priorMtimes = priorMtimesForEntry(state.snapshotRepos, entry)
					}
					if state.snapshotPartial {
						priorMtimes = nil
					}
					// A backend that crossed a schema-rebuild migration rung
					// (NeedsRebuild) has on-disk rows in the old shape that an
					// incremental reconcile cannot fix. Drop prior mtimes so every
					// file re-indexes into the new schema (the nil branch below
					// runs a full TrackRepoCtx and marks the repo changed, so the
					// global resolve/derivation passes re-run too). No-op for
					// backends without the capability and whenever no rebuild rung
					// was crossed — the common case.
					if storeNeedsRebuild(state.graph) {
						if len(priorMtimes) > 0 {
							logger.Info("daemon: backend signalled schema rebuild; forcing full re-index",
								zap.String("path", entry.Path))
						}
						priorMtimes = nil
					}
					pathFn := "track"
					if priorMtimes != nil {
						pathFn = "reconcile"
						res, err := state.multiIndexer.ReconcileRepoCtx(ctx, entry, priorMtimes)
						switch {
						case err != nil:
							logger.Warn("daemon: startup reconcile failed",
								zap.String("path", entry.Path), zap.Error(err))
							// Treat a failed reconcile as "changed" so the global
							// passes still run — degrade toward correctness, not
							// toward the fast path, when we can't trust the delta.
							changedRepos.Add(1)
						case res != nil && (res.StaleFileCount > 0 || res.DeletedFileCount > 0 || len(res.FailedFiles) > 0):
							changedRepos.Add(1)
						}
					} else {
						// No prior mtimes → full cold (re)index of this repo,
						// which is "changed" by definition.
						changedRepos.Add(1)
						if _, err := state.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
							logger.Warn("daemon: startup track failed",
								zap.String("path", entry.Path), zap.Error(err))
						}
					}
					elapsed := time.Since(repoStart)
					if elapsed > 2*time.Second {
						logger.Info("daemon: warmup repo elapsed",
							zap.String("path", entry.Path),
							zap.String("path_fn", pathFn),
							zap.Duration("elapsed", elapsed))
					}
				}(entry)
			}
		}()
	}
	for _, entry := range repos {
		jobs <- entry
	}
	close(jobs)
	wg.Wait()
	logger.Info("daemon: warmup phase done",
		zap.String("phase", "parallel_parse"),
		zap.Duration("elapsed", time.Since(phaseStart)))
	publishReadinessPhase(state, "parallel_parse_done", false, map[string]any{
		"tracked_repos": len(repos),
		"elapsed_ms":    time.Since(phaseStart).Milliseconds(),
	})

	// Warm-restart fast path. When the reconcile loop above re-indexed
	// nothing, the persistent backend already carries every resolved and
	// derived edge from the prior run; the deferred per-repo passes, the
	// cross-repo resolve, and the graph-wide derivation passes would all
	// just recompute what's on disk. Skipping them is what turns a warm
	// restart from a multi-minute replay of the cold-warmup cost into a
	// near-instant "open store, reconcile zero files, start watching".
	// The in-memory backend reaches here too, but its snapshot replay
	// already restored the derived edges, so the skip is equally safe.
	anyChanged := changedRepos.Load() > 0
	logger.Info("daemon: warmup change detection",
		zap.Int64("changed_repos", changedRepos.Load()),
		zap.Int("tracked_repos", len(repos)),
		zap.Bool("global_passes", anyChanged))

	// Drain deferred per-repo passes (ResolveAll / semantic enrich /
	// contract extract+commit) serially across the indexers the parallel
	// loop populated. Must run before RunGlobalResolve so cross-repo
	// resolution sees fully-lifted per-repo placeholder edges.
	if anyChanged {
		phaseStart = time.Now()
		publishReadinessPhase(state, "deferred_passes_all", false, nil)
		state.multiIndexer.RunDeferredPassesAll(ctx)
		logger.Info("daemon: warmup phase done",
			zap.String("phase", "deferred_passes_all"),
			zap.Duration("elapsed", time.Since(phaseStart)))
		publishReadinessPhase(state, "deferred_passes_all_done", false, map[string]any{
			"elapsed_ms": time.Since(phaseStart).Milliseconds(),
		})
	}

	// Rehydrate per-repo contract registries from the snapshot. Only
	// target indexers whose registry is still nil — a non-nil registry
	// means IncrementalReindex (or a fresh TrackRepoCtx) re-extracted
	// contracts from source, and that result is authoritative. Without
	// this, every steady-state repo's ContractRegistry stays nil and
	// MergedContractRegistry skips them, so `contracts` returns only
	// the contracts of repos whose files happened to change since the
	// last shutdown.
	{
		phaseStart = time.Now()
		injectedRepos, injectedCount := 0, 0
		for prefix := range state.multiIndexer.AllMetadata() {
			idx := state.multiIndexer.GetIndexer(prefix)
			if idx == nil || idx.ContractRegistry() != nil {
				continue
			}
			// Primary path: rebuild the per-repo registry from
			// KindContract nodes already in the backend's graph.
			// The indexer stamps every contract record onto
			// Node.Meta at commit time, so the graph is the
			// authoritative source — no gob round-trip needed.
			reg := contracts.LoadRegistryFromGraph(state.graph, prefix)
			if reg == nil {
				// Fallback to the legacy gob-snapshot path for
				// daemons upgrading across this change. The
				// snapshot copy is read-only by this point so the
				// two sources can't drift mid-flight.
				cs, ok := state.snapshotContracts[prefix]
				if !ok || len(cs) == 0 {
					continue
				}
				reg = contracts.NewRegistry()
				for _, c := range cs {
					reg.Add(c)
				}
			}
			idx.SetContractRegistry(reg)
			injectedRepos++
			injectedCount += len(reg.All())
		}
		if injectedRepos > 0 {
			logger.Info("daemon: rehydrated contract registries from graph/snapshot",
				zap.Int("repos", injectedRepos),
				zap.Int("contracts", injectedCount),
				zap.Duration("elapsed", time.Since(phaseStart)))
		}
	}

	// Backfill `WorkspaceID` / `ProjectID` onto nodes and contracts
	// loaded from a legacy snapshot. Old snapshots have these fields
	// as zero (gob decodes unknown fields silently); without this
	// stamp the matcher's EffectiveWorkspace falls back to RepoPrefix
	// and explicit shared-workspace declarations stop working until
	// every file is touched. Idempotent — re-running on a stamped
	// graph is a no-op.
	phaseStart = time.Now()
	if nodes, conts := state.multiIndexer.BackfillWorkspaceSlugs(); nodes+conts > 0 {
		logger.Info("daemon: backfilled workspace/project slugs from .gortex.yaml",
			zap.Int("nodes", nodes),
			zap.Int("contracts", conts),
			zap.Duration("elapsed", time.Since(phaseStart)))
	}

	// Run a cross-repo resolution pass once warmup has stamped the
	// workspace slugs. Files touched by IncrementalReindex already
	// re-resolve via the per-repo Resolver; this catches cross-repo
	// edges in unchanged files plus stamps cross_workspace_deps
	// eligibility on stubs. Mirrors what MultiIndexer.IndexAll does
	// for a fresh-start daemon (where there's no snapshot to reconcile
	// against). After resolution, contract bridge edges may have
	// changed too, so ReconcileContractEdges runs again.
	if anyChanged {
		phaseStart = time.Now()
		publishReadinessPhase(state, "global_resolve", false, nil)
		state.multiIndexer.RunGlobalResolve()
		logger.Info("daemon: warmup phase done",
			zap.String("phase", "global_resolve"),
			zap.Duration("elapsed", time.Since(phaseStart)))
		publishReadinessPhase(state, "global_resolve_done", false, map[string]any{
			"elapsed_ms": time.Since(phaseStart).Milliseconds(),
		})
	}

	// Finish the batch: turn off the per-repo skip flag and run the
	// graph-wide derivation passes once. RunGlobalResolve above just
	// lifted the last cross-repo placeholder EdgeCalls, so EdgeTests
	// derivation here picks up cross-repo test→subject pairs that
	// were unresolved during the per-repo loop. On the warm-restart fast
	// path (nothing changed) ResetBatch clears the deferred-batch flags
	// without re-running those passes — the persisted graph already has
	// the derived edges.
	phaseStart = time.Now()
	publishReadinessPhase(state, "end_batch", false, nil)
	if anyChanged {
		state.multiIndexer.EndBatch()
	} else {
		state.multiIndexer.ResetBatch()
	}
	logger.Info("daemon: warmup phase done",
		zap.String("phase", "end_batch"),
		zap.Duration("elapsed", time.Since(phaseStart)))
	publishReadinessPhase(state, "end_batch_done", false, map[string]any{
		"elapsed_ms": time.Since(phaseStart).Milliseconds(),
	})

	// Restore the workspace vector index from the snapshot. The warmup
	// loop above ran with vector building skipped (SetSkipVectorBuild),
	// so the search backend is text-only at this point; ImportVectorIndex
	// wraps it into a HybridBackend with the cached vectors. This is the
	// step that lets a default-on daemon avoid re-embedding the whole
	// graph on every restart. SetSkipVectorBuild(false) afterwards means
	// any later file-change re-index rebuilds vectors normally.
	if vec := state.snapshotVector; len(vec.Index) > 0 {
		phaseStart = time.Now()
		if err := state.multiIndexer.ImportVectorIndex(vec.Index, vec.Dims, vec.Count); err != nil {
			logger.Warn("daemon: vector index restore failed — semantic search will rebuild on next index",
				zap.Error(err))
		} else {
			logger.Info("daemon: restored vector index from snapshot",
				zap.Int("vectors", vec.Count),
				zap.Int("dims", vec.Dims),
				zap.Duration("elapsed", time.Since(phaseStart)))
		}
		state.multiIndexer.SetSkipVectorBuild(false)
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
	publishReadinessPhase(state, "watcher_started", false, map[string]any{
		"watched_repos": len(watchCfgs),
	})
	return mw
}

// publishReadinessPhase forwards a workspace_readiness phase
// transition to the MCP server's readiness broadcaster. Safe to
// call when the server isn't wired (single-process modes that
// bypass the daemon).
func publishReadinessPhase(state *daemonState, phase string, ready bool, extra map[string]any) {
	if state == nil || state.mcpServer == nil {
		return
	}
	state.mcpServer.PublishReadiness(phase, ready, extra)
}

// priorMtimesFromStore asks the backend for its persisted FileMtime
// rows for the repo described by entry. Returns nil when the backend
// doesn't implement the reader (in-memory backend) or has no recorded
// mtimes for the repo (fresh cold start). When non-nil it short-
// circuits the gob-snapshot lookup so the warm path is driven by
// data the backend persisted itself.
func priorMtimesFromStore(g graph.Store, entry config.RepoEntry, logger *zap.Logger) map[string]int64 {
	reader, ok := g.(graph.FileMtimeReader)
	if !ok {
		if logger != nil {
			logger.Info("daemon: priorMtimesFromStore: store does not implement FileMtimeReader")
		}
		return nil
	}
	prefix := strings.TrimPrefix(config.ResolvePrefix(entry), "/")
	if prefix == "" {
		if logger != nil {
			logger.Info("daemon: priorMtimesFromStore: empty prefix",
				zap.String("entry_path", entry.Path),
				zap.String("entry_name", entry.Name))
		}
		return nil
	}
	mtimes := reader.LoadFileMtimes(prefix)
	if logger != nil {
		logger.Info("daemon: priorMtimesFromStore loaded",
			zap.String("prefix", prefix),
			zap.Int("count", len(mtimes)))
	}
	return mtimes
}

// storeNeedsRebuild reports whether the backend signalled, via the optional
// NeedsRebuild capability, that a schema migration crossed a rung an ALTER
// could not satisfy — so its persisted rows are in an old shape and the
// warm/incremental reconcile must be bypassed for a full re-index. This is a
// generic, opt-in capability probe: a backend implements NeedsRebuild() bool
// to participate. No backend currently does, so this always reports false;
// it stays as a hook for any future on-disk store that needs schema-version
// gating on warm restart.
func storeNeedsRebuild(g any) bool {
	rb, ok := g.(interface{ NeedsRebuild() bool })
	return ok && rb.NeedsRebuild()
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

// collectSnapshotVector serializes the workspace-global semantic-search
// vector index for the snapshot. The daemon's search backend is shared
// across every tracked repo, so there is exactly one vector index;
// MultiIndexer.ExportVectorIndex returns an empty blob when embeddings
// are disabled or no vectors were built, in which case the snapshot
// simply carries no vector data and the next warmup re-embeds.
func collectSnapshotVector(mi *indexer.MultiIndexer) snapshotVector {
	if mi == nil {
		return snapshotVector{}
	}
	data, dims, count := mi.ExportVectorIndex()
	return snapshotVector{Index: data, Dims: dims, Count: count}
}
