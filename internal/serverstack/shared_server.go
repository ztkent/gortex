package serverstack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
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
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/semantic/scip"
)

// Lifecycle selects the backend default, whether warm-restart/snapshot
// machinery the entry point wires is appropriate, and the store-lock
// posture.
type Lifecycle int

const (
	// LifecycleDaemon is the durable, long-lived daemon: sqlite default,
	// cross-process store lock.
	LifecycleDaemon Lifecycle = iota
	// LifecycleHTTP is the daemon's HTTP surface (gortex daemon --http):
	// durable, sqlite default, store lock.
	LifecycleHTTP
	// LifecycleOneshot is the ephemeral embedded server: memory-only,
	// FileStore snapshot, no store lock.
	LifecycleOneshot
)

// Writable reports whether the lifecycle owns a durable on-disk store
// (and therefore takes the cross-process store lock).
func (l Lifecycle) Writable() bool { return l == LifecycleDaemon || l == LifecycleHTTP }

// defaultBackend resolves the backend name for an empty cfg.Backend.
func (l Lifecycle) defaultBackend() string {
	if l == LifecycleOneshot {
		return "memory"
	}
	return "sqlite"
}

// SharedServerConfig carries the knobs that vary between the daemon, the
// HTTP surface, and the one-shot embedded path. The first block is the
// authoritative public surface consumers cite; the rest are
// entry-point-resolved options threaded through with the already-loaded
// config rather than re-derived here.
type SharedServerConfig struct {
	Lifecycle    Lifecycle // backend default + store-lock posture
	Index        string    // workspace root the indexer/LSP/bind anchor at
	Backend      string    // "" resolves via Lifecycle; else memory|sqlite
	BackendPath  string    // "" => ~/.gortex/store/store.sqlite
	SnapshotPath string    // gob+gzip pre-load path; entry point applies it
	HTTPAddr     string    // opts the lifecycle into the /mcp HTTP surface
	Watch        bool      // filesystem watcher / incremental reindex

	// Entry-point-resolved options (not part of the authoritative surface).
	Config         *config.Config       // loaded .gortex.yaml (required)
	Global         *config.GlobalConfig // loaded ~/.gortex/config.yaml
	Logger         *zap.Logger
	Version        string
	Embedder       EmbedderRequest
	BufferPoolMB   uint64
	SideStores     SideStores
	ScopeWorkspace string
	ScopeProject   string
	// ActiveProject names the project the MCP server should start scoped
	// to (multi-repo mode hint). Empty leaves it unset.
	ActiveProject string
	// SemanticMode selects the goanalysis provider mode: "callgraph"
	// builds the call graph; anything else (default) type-checks.
	SemanticMode string
	// SavingsPath overrides the token-savings ledger database; empty
	// defaults to the machine-global sidecar under the data dir
	// (savings.DefaultDBPath() — the same database the `gortex savings`
	// CLI reads), independent of SideStores: deriving it from a per-mode
	// side-store dir would split the ledger between writer and reader.
	// Tests MUST set it (with SavingsLegacyJSON) to temp paths or the
	// constructor mutates the developer's real ledger. SavingsRepo
	// scopes the accumulated totals (empty = workspace-global).
	// SavingsLegacyJSON names the flat-file era's cumulative
	// savings.json to import once — its sibling .jsonl event log rides
	// along; empty uses the historical default location under the
	// cache dir.
	SavingsPath       string
	SavingsLegacyJSON string
	SavingsRepo       string
}

// SideStores configures where the agent-authored knowledge stores
// persist. Each (dir, repo) pair selects the sidecar DB file (dir, which
// opens <dir>/sidecar.sqlite) and the partition key (repo, hashed via
// persistence.RepoCacheKey); an empty dir OR repo yields an in-memory
// store that never flushes. The zero value therefore makes every store
// ephemeral — entry points opt into persistence explicitly, encoding
// whether the server owns one repo (per-repo keying) or many (a
// workspace-global key).
type SideStores struct {
	// NotesDir / NotesRepo back the durable, repo-partitioned notes and
	// development-memory stores.
	NotesDir  string
	NotesRepo string
	// FeedbackDir / FeedbackRepo back feedback, the combo tracker, and the
	// frecency tracker (the implicit-signal stores).
	FeedbackDir  string
	FeedbackRepo string
	// NotebookPath is the repository-local notebook root (committed to
	// git so it travels with the repo); empty leaves the notebook
	// uninitialised.
	NotebookPath string
}

// SharedServer bundles the constructed stack plus the teardown chain.
type SharedServer struct {
	Graph        graph.Store
	Indexer      *indexer.Indexer
	MultiIndexer *indexer.MultiIndexer // nil in single-repo standalone
	Engine       *query.Engine
	MCP          *gortexmcp.Server
	ConfigMgr    *config.ConfigManager
	Overlays     *daemon.OverlayManager
	// EmbedderDims is the active embedder's vector dimensionality, or 0
	// when embeddings are off. The entry point's snapshot warm-start
	// compares it to a snapshot's vector dims before skipping a re-embed.
	EmbedderDims int

	// ResolverLSPRegistry / LSPRouter are the resolve-time LSP wiring the
	// entry point's warmup hooks reference; nil when semantic enrichment
	// is off.
	ResolverLSPRegistry *lsp.ResolverHelperRegistry
	LSPRouter           *lsp.Router

	cleanup []func() // run LIFO by Close (backend close, savings flush).
}

// Close runs the teardown chain LIFO.
func (s *SharedServer) Close() error {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
	return nil
}

// NewSharedServer builds the whole server stack — graph.Store ->
// parser.Registry -> indexer -> query.Engine -> mcp.Server plus every
// side-effect init — through one wiring shared by the daemon, the HTTP
// surface, and the one-shot embedded path. Snapshot warm-start and the
// per-repo warmup loop stay with the entry point (they orchestrate around
// the returned graph/indexer); this constructor is the dedup spine for
// the stack wiring itself.
func NewSharedServer(cfg SharedServerConfig) (*SharedServer, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	conf := cfg.Config
	if conf == nil {
		conf = config.Default()
	}
	backendName := cfg.Backend
	if backendName == "" {
		backendName = cfg.Lifecycle.defaultBackend()
	}

	// Load user-defined domain-extractor rules (TOML tree-sitter patterns).
	for _, pattern := range conf.RuleFiles {
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, rp := range matches {
			if n, lerr := astquery.LoadUserRulesFile(rp); lerr != nil {
				logger.Warn("serverstack: failed to load domain rule file",
					zap.String("file", rp), zap.Error(lerr))
			} else if n > 0 {
				logger.Info("serverstack: loaded domain-extractor rules",
					zap.String("file", rp), zap.Int("rules", n))
			}
		}
	}

	s := &SharedServer{}

	// Cross-process store lock: a writable, on-disk lifecycle
	// acquires an advisory flock on store.sqlite.lock and fails fast if
	// another process owns the store. SQLite's in-process writeMu +
	// busy_timeout serialise in-process writers only; nothing else stops
	// a second OS process opening the same file and corrupting it.
	if cfg.Lifecycle.Writable() && isSqliteBackend(backendName) {
		storePath, perr := resolveBackendPath(cfg.BackendPath, "store.sqlite")
		if perr != nil {
			return nil, perr
		}
		lock := flock.New(storePath + ".lock")
		locked, lerr := lock.TryLock()
		if lerr != nil {
			return nil, fmt.Errorf("acquire store lock %q: %w", storePath+".lock", lerr)
		}
		if !locked {
			hint := "the store is already owned by another gortex process"
			if pid, ok := daemon.RunningPID(); ok {
				hint = fmt.Sprintf("store already owned by daemon pid %d", pid)
			}
			return nil, fmt.Errorf("%s — stop it first (`gortex daemon stop`)", hint)
		}
		s.cleanup = append(s.cleanup, func() { _ = lock.Unlock() })
	}

	g, backendCleanup, err := OpenBackend(backendName, cfg.BackendPath, cfg.BufferPoolMB, logger)
	if err != nil {
		return nil, err
	}
	s.cleanup = append(s.cleanup, backendCleanup)
	s.Graph = g

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	languages.RegisterCustomGrammars(reg, conf.Index.Grammars, logger)
	languages.RegisterExtractorPlugins(reg, conf.Index.ExtractorPlugins, logger)
	languages.RegisterFallbackChunkers(reg, conf.Index.FallbackChunkers, logger)

	idx := indexer.New(g, reg, conf.Index, logger)
	s.Indexer = idx

	// Semantic enrichment (opt-in). A daemon-managed LSP router owns
	// subprocess lifecycle; SCIP + goanalysis providers register eagerly;
	// Manager.SetLSPRouter installs the bridge so EnrichAll can lazy-spawn
	// LSPs on demand. RegisterAvailable auto-registers every known LSP
	// spec whose binary resolves on PATH (per-spec / GORTEX_LSP_DISABLE
	// opt-out). All lifecycles get this (reconciles the prior mcp-only
	// drift where the embedded path skipped auto-registration).
	if conf.Semantic.Enabled {
		semCfg := semantic.Config{
			Enabled:           conf.Semantic.Enabled,
			TimeoutSeconds:    conf.Semantic.TimeoutSeconds,
			EnrichOnWatch:     conf.Semantic.EnrichOnWatch,
			WatchDebounceMs:   conf.Semantic.WatchDebounceMs,
			RefuteUnconfirmed: conf.Semantic.RefuteUnconfirmed,
		}
		for _, pc := range conf.Semantic.Providers {
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

		goMode := goanalysis.ModeTypeCheck
		if cfg.SemanticMode == "callgraph" {
			goMode = goanalysis.ModeCallGraph
		}
		goProvider := goanalysis.NewProvider(goMode, false, logger)
		semMgr.RegisterProvider(goProvider)
		contracts.SetBindingResolver(goProvider)

		lspWorkspace := cfg.Index
		if lspWorkspace == "" {
			lspWorkspace, _ = os.Getwd()
		}
		lspRouter := lsp.NewRouter(lspWorkspace, logger).
			WithIdleTimeout(10 * time.Minute).
			WithReaperInterval(time.Minute).
			WithMaxAlive(6).
			WithAdditionalWorkspaceFolders(conf.Semantic.AdditionalWorkspaceFolders)
		semMgr.SetLSPRouter(lspRouter)

		for _, pc := range semCfg.Providers {
			if !pc.Enabled {
				continue
			}
			switch {
			case strings.HasPrefix(pc.Name, "scip-") && pc.Command != "":
				scipProv := scip.NewProvider(pc.Command, pc.Args, pc.Languages, semCfg.TimeoutSeconds, logger)
				if pc.Mode == "definitions" {
					scipProv = scipProv.WithDefinitionsOnly()
				}
				semMgr.RegisterProvider(scipProv)
			case lsp.SpecByName(pc.Name) != nil:
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

		disabled := LspDisabledSet(conf.Semantic.Providers, os.Getenv("GORTEX_LSP_DISABLE"))
		var autoRegistered []string
		if !disabled["__all__"] {
			autoRegistered = lspRouter.RegisterAvailable(disabled)
		}

		idx.SetSemanticManager(semMgr)

		if !IsFalsyEnv("GORTEX_LSP_RESOLVER") {
			s.ResolverLSPRegistry = lsp.NewResolverHelperRegistry()
			s.LSPRouter = lspRouter
			idx.SetResolverLSPHelper(s.ResolverLSPRegistry)
			// Single-repo standalone mode: anchor a "" prefix helper at
			// the workspace the server points at. Only fires when Index is
			// set (the embedded path); the daemon leaves Index empty and
			// registers per-repo helpers via the OnRepoTracked hook.
			if cfg.Index != "" {
				if abs, aerr := filepath.Abs(cfg.Index); aerr == nil {
					tsSpec := lsp.SpecByName("typescript-language-server")
					if tsSpec != nil && lspRouter.Available(tsSpec) && RepoLikelyHasTypeScriptIntent(abs) {
						helper := BuildResolverLSPHelper(lspRouter, tsSpec, abs, lsp.ResolverPoolSizeFromEnv(1), logger)
						s.ResolverLSPRegistry.Register("", helper)
					}
				}
			}
			logger.Info("serverstack: resolve-time LSP hot path enabled")
		} else {
			logger.Info("serverstack: resolve-time LSP hot path disabled (GORTEX_LSP_RESOLVER=0)")
		}

		logger.Info("serverstack: semantic enrichment enabled",
			zap.Int("providers", len(semCfg.Providers)),
			zap.Strings("lsp_auto_registered", autoRegistered))
	}

	// Embeddings: explicit flag/env > `embedding:` config > default (on,
	// static GloVe).
	embedder, embDesc, embErr := ResolveEmbedder(cfg.Embedder, conf)
	switch {
	case embErr != nil:
		logger.Warn("serverstack: embeddings requested but unavailable", zap.Error(embErr))
	case embedder != nil:
		logger.Info("serverstack: embeddings enabled",
			zap.String("provider", embDesc),
			zap.Int("dim", embedder.Dimensions()))
	default:
		logger.Info("serverstack: embeddings disabled")
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
		idx.SetEmbeddingChunkOptions(EmbeddingChunkOptions(conf))
		idx.SetEmbeddingMaxSymbols(conf.Embedding.MaxSymbols)
		idx.SetEmbeddingAPIConcurrency(conf.Embedding.APIConcurrency)
		s.EmbedderDims = embedder.Dimensions()
	}

	cm, err := config.NewConfigManager("")
	if err != nil {
		logger.Warn("serverstack: could not load global config", zap.Error(err))
	}
	s.ConfigMgr = cm

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
		if embedder != nil {
			mi.SetEmbedder(embedder)
			mi.SetEmbeddingChunkOptions(EmbeddingChunkOptions(conf))
			mi.SetEmbeddingMaxSymbols(conf.Embedding.MaxSymbols)
			mi.SetEmbeddingAPIConcurrency(conf.Embedding.APIConcurrency)
		}
		if s.ResolverLSPRegistry != nil {
			mi.SetResolverLSPHelper(s.ResolverLSPRegistry)
			if s.LSPRouter != nil {
				routerRef := s.LSPRouter
				registryRef := s.ResolverLSPRegistry
				poolSize := lsp.ResolverPoolSizeFromEnv(1)
				mi.SetOnRepoTracked(func(prefix, absPath string) {
					tsSpec := lsp.SpecByName("typescript-language-server")
					if tsSpec == nil || !routerRef.Available(tsSpec) {
						return
					}
					if !RepoLikelyHasTypeScriptIntent(absPath) {
						return
					}
					helper := BuildResolverLSPHelper(routerRef, tsSpec, absPath, poolSize, logger)
					registryRef.Register(prefix, helper)
				})
			}
		}
	}
	s.MultiIndexer = mi

	var multiOpts []gortexmcp.MultiRepoOptions
	if mi != nil || cm != nil {
		multiOpts = append(multiOpts, gortexmcp.MultiRepoOptions{
			MultiIndexer:   mi,
			ConfigManager:  cm,
			ActiveProject:  cfg.ActiveProject,
			ScopeWorkspace: cfg.ScopeWorkspace,
			ScopeProject:   cfg.ScopeProject,
		})
	}

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	eng.ApplyRerankWeights(conf.Search.Weights)
	s.Engine = eng

	gortexmcp.Version = cfg.Version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, conf.Guards.Rules, multiOpts...)
	srv.SetArchitecture(conf.Architecture)
	srv.SetArtifacts(conf.Artifacts)
	srv.SetNamedQueries(conf.Queries)
	srv.SetSearchConfig(conf.Search)
	s.MCP = srv

	overlays := daemon.NewOverlayManager(daemon.OverlayIdleTTLFromEnv(0))
	srv.SetOverlayManager(overlays)
	s.Overlays = overlays
	stopOverlayJanitor := overlays.StartJanitor(0, func(dropped int) {
		logger.Info("overlay janitor: swept idle sessions", zap.Int("dropped", dropped))
	})
	s.cleanup = append(s.cleanup, stopOverlayJanitor)

	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
		srv.SetLSPDiagnosticsBroadcasting()
	}

	// Side-stores. The HTTP path previously wired NONE of these; routing it
	// through the shared constructor adds notes/memories/savings to it.
	sideCfg := cfg.SideStores
	srv.InitFeedback(sideCfg.FeedbackDir, sideCfg.FeedbackRepo)
	srv.InitNotes(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitMemories(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitSuppressions(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitNotebook(sideCfg.NotebookPath)
	srv.InitCombo(sideCfg.FeedbackDir, sideCfg.FeedbackRepo, gortexmcp.ModeAI)
	srv.InitFrecency(sideCfg.FeedbackDir, sideCfg.FeedbackRepo, gortexmcp.ModeAI)

	// The savings ledger is machine-global: every entry point defaults to
	// the same sidecar database the `gortex savings` CLI reads. Deriving
	// it from a per-mode side-store dir would split the ledger between
	// writer and reader — the failure mode the flat files had.
	savingsPath := cfg.SavingsPath
	if savingsPath == "" {
		savingsPath = savings.DefaultDBPath()
	}
	if savingsStore, err := savings.Open(savingsPath); err == nil {
		legacyJSON := cfg.SavingsLegacyJSON
		if legacyJSON == "" {
			legacyJSON = savings.DefaultPath()
		}
		if ierr := savingsStore.ImportLegacy(legacyJSON); ierr != nil {
			logger.Warn("serverstack: legacy savings import failed", zap.Error(ierr))
		}
		srv.InitSavings(savingsStore, cfg.SavingsRepo)
		s.cleanup = append(s.cleanup, func() { _ = srv.FlushSavings() })
	} else {
		logger.Warn("serverstack: savings persistence disabled", zap.Error(err))
	}

	global := cfg.Global
	if global == nil {
		global, _ = config.LoadGlobal()
	}
	if global != nil {
		srv.SetupLLM(global.MergeLLMInto(conf.LLM))
	}

	return s, nil
}
