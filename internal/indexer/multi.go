package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// RepoMetadata holds per-repo indexing state.
type RepoMetadata struct {
	RepoPrefix    string
	RootPath      string
	Identity      *RepoIdentity
	LastIndexTime time.Time
	FileCount     int
	NodeCount     int
	EdgeCount     int
	ParseErrors   []IndexError
	FileMtimes    map[string]int64
	// IsWorktree records whether RootPath was a linked git worktree
	// (as opposed to a main checkout) at the time the repo was
	// tracked. Captured here because once the worktree directory is
	// removed from disk it can no longer be classified — the janitor
	// needs this remembered flag to know a vanished root was a
	// worktree and may be garbage-collected.
	IsWorktree bool
}

// MultiIndexer orchestrates indexing across multiple repositories.
type MultiIndexer struct {
	graph     *graph.Graph
	registry  *parser.Registry
	search    search.Backend
	embedder  embedding.Provider
	repos     map[string]*RepoMetadata // repoPrefix → metadata
	indexers  map[string]*Indexer      // repoPrefix → per-repo indexer
	configMgr *config.ConfigManager
	logger    *zap.Logger
	mu        sync.RWMutex

	// deferGlobalPasses, when set, propagates SetDeferGlobalPasses(true)
	// to every per-repo Indexer constructed by this MultiIndexer. Batch
	// callers (warmup, ReconcileAll) flip it on around their loop and
	// invoke RunGlobalGraphPasses once at the end so the O(global) walks
	// (InferImplements / InferOverrides / markTestSymbolsAndEmitEdges)
	// don't run R times against an R-repo graph.
	deferGlobalPasses bool

	// deferResolve, when set, propagates SetDeferResolve(true) to every
	// per-repo Indexer constructed by this MultiIndexer. Used by the
	// parallel warmup path: per-repo ResolveAll / contract extract /
	// semantic enrich mutate the shared graph, so running them in
	// parallel across repos races. With this flag the parallel loop
	// just parses; RunDeferredPassesAll runs the per-repo passes
	// serially after the loop. Independent of deferGlobalPasses — that
	// flag covers a separate (cheaper) set of O(global) walks.
	deferResolve bool

	// resolverLSPHelper is the resolve-time LSP helper propagated
	// onto every per-repo Indexer and onto the global post-pass
	// resolver in RunDeferredPassesAll. nil means no LSP hot-path
	// (heuristic-only resolution, the pre-N5 behaviour). The
	// daemon installs the helper via SetResolverLSPHelper after
	// constructing the LSP router; a multi-repo composite helper
	// dispatches per-file by repo prefix.
	resolverLSPHelper resolver.LSPHelper

	// onRepoTracked, when non-nil, is invoked after a fresh
	// TrackRepoCtx call has resolved the repo's prefix and
	// absolute path but before indexing starts. The daemon uses
	// this hook to register a per-repo resolver-time LSP helper
	// against the LSPHelper registry so dynamically-tracked repos
	// participate in the N5 hot path without daemon restart.
	onRepoTracked func(prefix, absPath string)

	// skipVectorBuild, when set, propagates SetSkipVectorBuild(true) to
	// every per-repo Indexer this MultiIndexer constructs, so their
	// buildSearchIndex passes populate only the text index and never
	// embed. The daemon flips it on for the warmup loop when a snapshot
	// already carries the workspace vector index — re-embedding 30k+
	// symbols only to overwrite them with the cached index is the
	// dominant restart cost. After warmup it restores the cached index
	// once via ImportVectorIndex and clears the flag.
	skipVectorBuild bool

	// embedChunkOpts is the AST sub-chunking configuration propagated
	// to every per-repo Indexer this MultiIndexer constructs. The zero
	// value leaves the chunker on its built-in defaults.
	embedChunkOpts embedding.ChunkOptions

	// embedMaxSymbols overrides the vector-index size cap propagated to
	// every per-repo Indexer. Zero keeps the built-in default.
	embedMaxSymbols int
}

// SetEmbedder installs the embedding provider every per-repo indexer
// should use. Must be called before IndexAll / TrackRepo for vectors
// to land in the graph — without this the fresh Indexer created per
// repo has embedder=nil and buildSearchIndex skips the vector pass.
// Safe to call zero or one times; subsequent calls silently replace.
func (mi *MultiIndexer) SetEmbedder(e embedding.Provider) {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.embedder = e
}

// npmAliasResolver builds a resolver.NpmAliasResolver covering every
// tracked repo's on-disk root. Installed on the global post-pass
// resolver and the cross-repo resolver so a JS/TS import declared
// through an npm alias resolves to a locally-vendored real package
// anywhere in the workspace. Returns nil when no repo has a usable
// root — callers treat that as "no alias rewriting".
func (mi *MultiIndexer) npmAliasResolver() resolver.NpmAliasResolver {
	roots := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	idx := newNpmAliasIndex(roots)
	if idx == nil {
		return nil
	}
	return idx.Resolve
}

// workspaceMembershipResolver builds a resolver.WorkspaceMembership
// covering every tracked repo's on-disk root. Installed on the global
// post-pass resolver and the cross-repo resolver so a same-named import
// collision is broken in favour of the importer's own package-manager
// workspace member. Returns nil when no repo is a workspace root —
// callers treat that as "no workspace signal".
func (mi *MultiIndexer) workspaceMembershipResolver() resolver.WorkspaceMembership {
	roots := map[string]string{}
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots[prefix] = meta.RootPath
		}
	}
	idx := newWorkspaceMembershipIndex(roots)
	if idx == nil {
		return nil
	}
	return idx.Lookup
}

// newPerRepoIndexer constructs a per-repo Indexer with the standard
// MultiIndexer wiring (shared search backend, embedder if configured,
// deferred-global-passes flag propagated). Centralised so the flag
// plumbing stays in one place.
func (mi *MultiIndexer) newPerRepoIndexer(cfg config.IndexConfig) *Indexer {
	idx := New(mi.graph, mi.registry, cfg, mi.logger)
	idx.search = mi.search
	if mi.embedder != nil {
		idx.SetEmbedder(mi.embedder)
	}
	idx.SetDeferGlobalPasses(mi.deferGlobalPasses)
	idx.SetDeferResolve(mi.deferResolve)
	idx.SetSkipVectorBuild(mi.skipVectorBuild)
	idx.SetEmbeddingChunkOptions(mi.embedChunkOpts)
	idx.SetEmbeddingMaxSymbols(mi.embedMaxSymbols)
	if mi.resolverLSPHelper != nil {
		idx.SetResolverLSPHelper(mi.resolverLSPHelper)
	}
	return idx
}

// SetEmbeddingChunkOptions installs the AST sub-chunking configuration
// every per-repo Indexer this MultiIndexer constructs should use, and
// re-applies it to every per-repo Indexer already built. Call before
// IndexAll / TrackRepo so the warmup indexers pick it up.
func (mi *MultiIndexer) SetEmbeddingChunkOptions(opts embedding.ChunkOptions) {
	mi.mu.Lock()
	mi.embedChunkOpts = opts
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetEmbeddingChunkOptions(opts)
	}
}

// SetEmbeddingMaxSymbols installs the vector-index size cap every
// per-repo Indexer this MultiIndexer constructs should use, and
// re-applies it to every per-repo Indexer already built. Zero keeps
// the built-in default.
func (mi *MultiIndexer) SetEmbeddingMaxSymbols(n int) {
	mi.mu.Lock()
	mi.embedMaxSymbols = n
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetEmbeddingMaxSymbols(n)
	}
}

// SetSkipVectorBuild controls whether per-repo Indexers constructed
// from now on skip the embedding pass in buildSearchIndex (text index
// only). The daemon enables it for the warmup loop when a snapshot
// already carries the workspace vector index, then disables it and
// restores the cached index once warmup finishes. It also re-applies
// the flag to every per-repo Indexer already constructed so a flag
// flip mid-lifecycle takes effect everywhere.
func (mi *MultiIndexer) SetSkipVectorBuild(skip bool) {
	mi.mu.Lock()
	mi.skipVectorBuild = skip
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetSkipVectorBuild(skip)
	}
}

// SetResolverLSPHelper installs the resolve-time LSP helper used by
// every per-repo Indexer this MultiIndexer constructs from now on,
// and by the global post-pass resolver in RunDeferredPassesAll. Pass
// nil to detach. Safe to call zero or one times; subsequent calls
// silently replace and propagate to every existing per-repo indexer.
func (mi *MultiIndexer) SetResolverLSPHelper(h resolver.LSPHelper) {
	mi.mu.Lock()
	mi.resolverLSPHelper = h
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.Unlock()
	for _, idx := range live {
		idx.SetResolverLSPHelper(h)
	}
}

// SetOnRepoTracked installs a callback fired once per TrackRepoCtx
// after the repo's prefix + absolute path have been resolved but
// before indexing starts. The daemon registers per-repo resolver-
// time LSP helpers from this hook so runtime-added repos
// participate in the N5 hot path. Pass nil to detach.
func (mi *MultiIndexer) SetOnRepoTracked(fn func(prefix, absPath string)) {
	mi.mu.Lock()
	mi.onRepoTracked = fn
	mi.mu.Unlock()
}

// BeginBatch enables deferred-global-passes mode for every per-repo
// Indexer that this MultiIndexer constructs after the call AND for
// every Indexer already in mi.indexers (so ReconcileAll's per-repo
// IncrementalReindex calls also skip the O(global) walks). Pair with
// EndBatch.
func (mi *MultiIndexer) BeginBatch() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.deferGlobalPasses = true
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(true)
	}
}

// BeginParallelBatch is BeginBatch plus parallel-safety: it also
// propagates SetDeferResolve(true) to every per-repo Indexer
// constructed during the batch. Use this when running the per-repo
// indexing loop across goroutines (warmup) — the parallel parsers
// must not race each other inside ResolveAll / contract extract /
// semantic enrich, which all mutate the shared graph. Pair with
// EndBatch; call RunDeferredPassesAll between the parallel parse and
// EndBatch to run the deferred per-repo passes serially.
func (mi *MultiIndexer) BeginParallelBatch() {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.deferGlobalPasses = true
	mi.deferResolve = true
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(true)
	}
}

// RunDeferredPassesAll runs RunDeferredPasses on every per-repo
// Indexer that carries a pending contract registry. Pairs with
// BeginParallelBatch: the parallel loop parses with deferResolve
// turned on; this serial loop drains the deferred per-repo passes
// (semantic enrich / contract extract+commit) without racing on the
// shared graph. The per-repo resolver pass is suppressed on every
// indexer because resolver.ResolveAll walks the entire shared graph
// — paying it R times is O(R · E). One global resolver.New(graph).
// ResolveAll at the end picks up every placeholder edge that the
// per-repo passes added.
func (mi *MultiIndexer) RunDeferredPassesAll(ctx context.Context) {
	mi.mu.RLock()
	indexers := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		indexers = append(indexers, idx)
	}
	mi.mu.RUnlock()
	for _, idx := range indexers {
		idx.SetSkipResolveInDeferred(true)
	}
	for _, idx := range indexers {
		idx.RunDeferredPasses(ctx)
	}
	for _, idx := range indexers {
		idx.SetSkipResolveInDeferred(false)
	}
	if mi.graph != nil {
		master := resolver.New(mi.graph)
		// Mirror the resolve-time LSP helper onto the master pass
		// too — RunDeferredPassesAll is where placeholder edges
		// added by deferred per-repo passes get resolved in batch,
		// and TS/JS-family edges should pick up LSP-precision
		// answers here just like the per-repo passes do.
		if mi.resolverLSPHelper != nil {
			master.SetLSPHelper(mi.resolverLSPHelper)
		}
		master.SetNpmAliasResolver(mi.npmAliasResolver())
		master.SetWorkspaceMembership(mi.workspaceMembershipResolver())
		master.ResolveAll()
	}
}

// EndBatch turns off deferred-global-passes mode and runs the graph-
// wide derivation passes (InferImplements, InferOverrides,
// markTestSymbolsAndEmitEdges) once against the shared graph. Restores
// the per-Indexer flag too so a subsequent one-off TrackRepoCtx call
// runs the passes inline as expected.
func (mi *MultiIndexer) EndBatch() {
	mi.mu.Lock()
	mi.deferGlobalPasses = false
	mi.deferResolve = false
	for _, idx := range mi.indexers {
		idx.SetDeferGlobalPasses(false)
	}
	mi.mu.Unlock()
	mi.RunGlobalGraphPasses()
}

// RunGlobalGraphPasses runs the graph-wide derivation passes once
// against the shared graph: InferImplements (structural interface
// satisfaction), InferOverrides (method-level overrides on
// extends/implements/composes parents), and markTestSymbolsAndEmitEdges
// (test→subject EdgeTests). Idempotent — graph.AddEdge dedupes by
// edgeKey and the resolver passes skip already-present parents.
func (mi *MultiIndexer) RunGlobalGraphPasses() {
	if mi.graph == nil {
		return
	}
	r := resolver.New(mi.graph)
	if added := r.InferImplements(); added > 0 {
		mi.logger.Info("inferred implements (global)", zap.Int("added", added))
	}
	if added := r.InferOverrides(); added > 0 {
		mi.logger.Info("inferred overrides (global)", zap.Int("added", added))
	}
	marked, emitted := markTestSymbolsAndEmitEdges(mi.graph)
	if marked > 0 || emitted > 0 {
		mi.logger.Info("test edges emitted (global)",
			zap.Int("test_symbols", marked),
			zap.Int("edges", emitted),
		)
	}
	if cs := detectClonesAndEmitEdges(mi.graph, mi.cloneThreshold()); cs.Items > 0 {
		mi.logger.Info("clone edges emitted (global)",
			zap.Int("items", cs.Items),
			zap.Int("clone_pairs", cs.Pairs),
			zap.Int("edges", cs.Edges),
			zap.Int("skipped_buckets", cs.SkippedBuckets),
			zap.Int("skipped_bucket_items", cs.SkippedBucketItems),
			zap.Int("diffused_pairs", cs.DiffusedPairs),
			zap.Int("diffused_edges", cs.DiffusedEdges),
		)
	}
	// gRPC stub-call resolution. After InferImplements (the
	// interface-satisfaction fallback signal) and before
	// DetectCrossRepoEdges so a cross-repo gRPC call gets its parallel
	// cross_repo_calls edge.
	if grpcResolved := resolver.ResolveGRPCStubCalls(mi.graph); grpcResolved > 0 {
		mi.logger.Info("gRPC stub calls resolved (global)",
			zap.Int("edges", grpcResolved),
		)
	}
	// Temporal stub-call resolution mirrors the gRPC staging — Java
	// interface→impl propagation needs EdgeImplements already
	// materialised; cross-repo workflow→activity dispatch then picks
	// up its parallel cross_repo_calls edge below.
	if temporalResolved := resolver.ResolveTemporalCalls(mi.graph); temporalResolved > 0 {
		mi.logger.Info("Temporal stub calls resolved (global)",
			zap.Int("edges", temporalResolved),
		)
	}
	// External-call placeholder synthesis (opt-in). Runs after the
	// stub passes so only genuinely un-indexed external targets are
	// left to materialise into call-chain terminals.
	if extCalls := resolver.SynthesizeExternalCalls(mi.graph, mi.externalCallSynthesisEnabled()); extCalls > 0 {
		mi.logger.Info("external-call placeholders synthesized (global)",
			zap.Int("edges", extCalls),
		)
	}
	// Cross-repo edge layer. Runs after InferImplements / InferOverrides
	// so the implements / extends edges they materialise across repo
	// boundaries pick up their parallel cross_repo_* edges.
	if crossRepoEdges := resolver.DetectCrossRepoEdges(mi.graph); crossRepoEdges > 0 {
		mi.logger.Info("cross-repo edges emitted (global)",
			zap.Int("edges", crossRepoEdges),
		)
	}
}

// cloneThreshold resolves the graph-wide Jaccard similarity cutoff for
// clone detection. Thresholds are configured per-repo but the LSH pass
// is graph-wide, so the strictest (highest) configured value across
// tracked repos wins — fewer false-positive EdgeSimilarTo edges. Zero
// (no repo set one) falls through to the clones package default.
func (mi *MultiIndexer) cloneThreshold() float64 {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	best := 0.0
	for _, idx := range mi.indexers {
		if t := idx.cloneThreshold(); t > best {
			best = t
		}
	}
	return best
}

// externalCallSynthesisEnabled resolves whether external-call placeholder
// synthesis should run over the shared graph. The pass is graph-wide, so
// it is enabled when any tracked repo opted in — a repo that wants the
// external hops in its call chains gets them even when a sibling repo
// left the option off.
func (mi *MultiIndexer) externalCallSynthesisEnabled() bool {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for _, idx := range mi.indexers {
		if idx.externalCallSynthesisEnabled() {
			return true
		}
	}
	return false
}

// NewMultiIndexer creates a MultiIndexer.
func NewMultiIndexer(
	g *graph.Graph,
	reg *parser.Registry,
	s search.Backend,
	cm *config.ConfigManager,
	logger *zap.Logger,
) *MultiIndexer {
	return &MultiIndexer{
		graph:     g,
		registry:  reg,
		search:    s,
		repos:     make(map[string]*RepoMetadata),
		indexers:  make(map[string]*Indexer),
		configMgr: cm,
		logger:    logger,
	}
}

// IndexAll indexes all active repos concurrently. Each repo gets its own
// Indexer instance with repo-specific config. Returns per-repo IndexResults.
func (mi *MultiIndexer) IndexAll() (map[string]*IndexResult, error) {
	return mi.IndexScoped("", "")
}

// IndexScoped is IndexAll restricted to repos whose workspace and
// project slugs match. Empty filters disable that axis (so empty/empty
// is equivalent to IndexAll). Resolution honours the same precedence
// as resolveWorkspaceID/resolveProjectID — RepoEntry override →
// `.gortex.yaml::workspace` → repo prefix — so a `gortex server
// --workspace foo` invocation matches both repos that declare
// `workspace: foo` in their own `.gortex.yaml` and repos pinned to
// `foo` from the user's global config.
//
// Returns an error when the filters exclude every active repo, so a
// `--workspace typo` surfaces as a startup failure rather than a
// silently empty graph.
func (mi *MultiIndexer) IndexScoped(workspaceSlug, projectSlug string) (map[string]*IndexResult, error) {
	repos := mi.configMgr.ActiveRepos()
	if len(repos) == 0 {
		return nil, nil
	}
	if workspaceSlug != "" || projectSlug != "" {
		filtered := mi.filterReposByScope(repos, workspaceSlug, projectSlug)
		if len(filtered) == 0 {
			return nil, fmt.Errorf("scope filter matched zero of %d active repos (workspace=%q project=%q)", len(repos), workspaceSlug, projectSlug)
		}
		repos = filtered
	}

	// Single-repo mode: delegate without prefixing.
	if len(repos) == 1 {
		r, err := mi.indexSingleRepo(repos[0])
		if err == nil {
			mi.ReconcileContractEdges()
		}
		return r, err
	}

	r, err := mi.indexMultiRepo(repos)
	if err == nil {
		mi.ReconcileContractEdges()
	}
	return r, err
}

// filterReposByScope returns the subset of repos whose resolved
// workspace and project slugs match the supplied filters. Empty
// filters disable that axis. Loads each repo's `.gortex.yaml` first so
// resolution sees the workspace/project declared there — matching only
// against `RepoEntry.Workspace` would miss repos that declare their
// slug in their own config file (the typical case for first-party
// repos).
func (mi *MultiIndexer) filterReposByScope(repos []config.RepoEntry, workspaceSlug, projectSlug string) []config.RepoEntry {
	if workspaceSlug == "" && projectSlug == "" {
		return repos
	}
	out := make([]config.RepoEntry, 0, len(repos))
	for _, e := range repos {
		absPath, err := filepath.Abs(e.Path)
		if err != nil {
			continue
		}
		prefix := config.ResolvePrefix(e)
		mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
		cfg := mi.configMgr.GetRepoConfig(prefix)
		entryCopy := e
		if workspaceSlug != "" && resolveWorkspaceID(&entryCopy, cfg, prefix) != workspaceSlug {
			continue
		}
		if projectSlug != "" && resolveProjectID(&entryCopy, cfg, prefix) != projectSlug {
			continue
		}
		out = append(out, e)
	}
	return out
}

// indexSingleRepo indexes a single repo without prefixing for backward compatibility.
func (mi *MultiIndexer) indexSingleRepo(entry config.RepoEntry) (map[string]*IndexResult, error) {
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}

	prefix := config.ResolvePrefix(entry)
	mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
	cfg := mi.configMgr.GetRepoConfig(prefix)

	idx := mi.newPerRepoIndexer(cfg.Index)
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))
	// No repo prefix in single-repo mode.

	result, err := idx.Index(absPath)
	if err != nil {
		return nil, fmt.Errorf("indexing %s: %w", absPath, err)
	}

	identity, _ := DetectIdentity(absPath)

	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix:    prefix,
		RootPath:      absPath,
		Identity:      identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		IsWorktree:    ResolveWorktree(absPath).IsWorktree,
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	return map[string]*IndexResult{prefix: result}, nil
}

// readGoModModule reads the module path from a go.mod file.
func readGoModModule(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module"))
		}
	}
	return ""
}

// indexMultiRepo indexes multiple repos concurrently with repo prefixes.
func (mi *MultiIndexer) indexMultiRepo(repos []config.RepoEntry) (map[string]*IndexResult, error) {
	type repoResult struct {
		prefix string
		result *IndexResult
		idx    *Indexer
		meta   *RepoMetadata
		err    error
	}

	// Pre-scan: build tracked repo module map from go.mod files.
	// This enables cross-repo dependency detection via GoModExtractor.
	trackedModules := make(map[string]string)
	for _, entry := range repos {
		prefix := config.ResolvePrefix(entry)
		absPath, _ := filepath.Abs(entry.Path)
		if mod := readGoModModule(absPath); mod != "" {
			trackedModules[prefix] = mod
			mi.logger.Debug("tracked repo module", zap.String("repo", prefix), zap.String("module", mod))
		}
	}

	resultCh := make(chan repoResult, len(repos))
	var wg sync.WaitGroup

	for _, entry := range repos {
		wg.Add(1)
		go func(e config.RepoEntry) {
			defer wg.Done()

			prefix := config.ResolvePrefix(e)
			absPath, err := filepath.Abs(e.Path)
			if err != nil {
				resultCh <- repoResult{prefix: prefix, err: fmt.Errorf("resolving path %s: %w", e.Path, err)}
				return
			}

			mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
			cfg := mi.configMgr.GetRepoConfig(prefix)
			idx := mi.newPerRepoIndexer(cfg.Index)
			idx.SetRepoPrefix(prefix)
			entryCopy := e
			idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
			idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))
			idx.SetTrackedRepoModules(trackedModules)
			// Defer the per-repo cross-cutting passes (ResolveAll,
			// semantic enrich, contract extract+commit) so they don't
			// race against each other across goroutines on the shared
			// graph. They run serially below via RunDeferredPasses after
			// wg.Wait(). The graph-wide derivation passes run once after
			// the loop via mi.RunGlobalGraphPasses().
			idx.SetDeferResolve(true)

			result, err := idx.Index(absPath)
			if err != nil {
				resultCh <- repoResult{prefix: prefix, err: fmt.Errorf("indexing %s: %w", absPath, err)}
				return
			}

			identity, _ := DetectIdentity(absPath)

			meta := &RepoMetadata{
				RepoPrefix:    prefix,
				RootPath:      absPath,
				Identity:      identity,
				LastIndexTime: time.Now(),
				FileCount:     result.FileCount,
				NodeCount:     result.NodeCount,
				EdgeCount:     result.EdgeCount,
				ParseErrors:   result.Errors,
				FileMtimes:    idx.FileMtimes(),
				IsWorktree:    ResolveWorktree(absPath).IsWorktree,
			}

			resultCh <- repoResult{prefix: prefix, result: result, idx: idx, meta: meta}
		}(entry)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	results := make(map[string]*IndexResult)
	var indexErrors []string

	mi.mu.Lock()
	for rr := range resultCh {
		if rr.err != nil {
			mi.logger.Error("failed to index repo", zap.String("prefix", rr.prefix), zap.Error(rr.err))
			indexErrors = append(indexErrors, rr.err.Error())
			continue
		}
		mi.repos[rr.prefix] = rr.meta
		mi.indexers[rr.prefix] = rr.idx
		results[rr.prefix] = rr.result
	}
	mi.mu.Unlock()

	// Run the per-repo passes that the goroutines above deferred. Serial
	// across repos is the simple correctness fix: ResolveAll mutates
	// Edge.Meta on edges in the shared graph, and the contract pass walks
	// every edge — running them in parallel across repos races. Inside a
	// single repo's ResolveAll the resolver still uses its own worker
	// pool, and parsing (the dominant cost on a fresh index) already ran
	// in parallel above, so the wall-time hit is small.
	deferCtx := context.Background()
	for prefix := range results {
		if idx, ok := mi.indexers[prefix]; ok {
			idx.RunDeferredPasses(deferCtx)
		}
	}

	// Run a global cross-repo resolution pass once every repo is
	// indexed, with the cross-workspace boundary check wired in.
	// Without this, repos that import each other have unresolved
	// edges that only resolve when an editor touches a file (the
	// watcher path is the only other place this resolver runs). The
	// boundary lookup means cross-workspace candidates only resolve
	// when an explicit `cross_workspace_deps` declaration covers
	// them.
	cr := resolver.NewCrossRepo(mi.graph)
	cr.SetCrossWorkspaceDepLookup(mi.crossWorkspaceLookup())
	cr.SetNpmAliasResolver(mi.npmAliasResolver())
	cr.SetWorkspaceMembership(mi.workspaceMembershipResolver())
	cr.ResolveAll()
	mi.ReconcileContractEdges()

	// Graph-wide derivation passes run exactly once after every repo
	// has been parsed, every per-repo and cross-repo resolver has lifted
	// placeholder edges, and contract bridges are in place. RunDeferredPasses
	// intentionally skips these so we don't pay an O(global) walk per
	// repo (was the dominant cost at R≈100+).
	mi.RunGlobalGraphPasses()

	if len(indexErrors) > 0 && len(results) == 0 {
		return nil, fmt.Errorf("all repos failed to index: %s", strings.Join(indexErrors, "; "))
	}

	return results, nil
}

// IndexRepo re-indexes a single repo by prefix. Evicts existing data first.
func (mi *MultiIndexer) IndexRepo(repoPrefix string) (*IndexResult, error) {
	mi.mu.RLock()
	meta, ok := mi.repos[repoPrefix]
	mi.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("repository not found: %s", repoPrefix)
	}

	// Evict existing data for this repo.
	if mi.IsMultiRepo() {
		mi.graph.EvictRepo(repoPrefix)
	}

	mi.configMgr.LoadWorkspaceConfig(repoPrefix, meta.RootPath)
	cfg := mi.configMgr.GetRepoConfig(repoPrefix)
	idx := mi.newPerRepoIndexer(cfg.Index)
	if mi.IsMultiRepo() {
		idx.SetRepoPrefix(repoPrefix)
	}
	entry := mi.configMgr.Global().FindRepoByPrefix(repoPrefix)
	idx.SetWorkspaceID(resolveWorkspaceID(entry, cfg, repoPrefix))
	idx.SetProjectID(resolveProjectID(entry, cfg, repoPrefix))

	result, err := idx.Index(meta.RootPath)
	if err != nil {
		return nil, fmt.Errorf("indexing %s: %w", meta.RootPath, err)
	}

	mi.mu.Lock()
	mi.repos[repoPrefix] = &RepoMetadata{
		RepoPrefix:    repoPrefix,
		RootPath:      meta.RootPath,
		Identity:      meta.Identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
	}
	mi.indexers[repoPrefix] = idx
	mi.mu.Unlock()

	// TODO: After re-indexing, run CrossRepoResolver.ResolveForRepo(repoPrefix)
	// to update cross-repo edges. This will be implemented in Task 7.1.

	mi.ReconcileContractEdges()

	return result, nil
}

// IncrementalReindexRepo incrementally re-indexes a single tracked repo
// by prefix: only the files that changed since the last pass are
// re-parsed and deleted files are evicted, against the repo's existing
// per-repo Indexer (so its mtime snapshot is preserved). Unlike
// IndexRepo it does NOT evict the whole repo first.
//
// When paths is non-empty the pass is scoped to those files /
// directories; otherwise the whole repo root is scanned. Returns an
// error when the prefix is not a tracked repo.
func (mi *MultiIndexer) IncrementalReindexRepo(repoPrefix string, paths []string) (*IndexResult, error) {
	mi.mu.RLock()
	meta, ok := mi.repos[repoPrefix]
	idx := mi.indexers[repoPrefix]
	mi.mu.RUnlock()
	if !ok || meta == nil {
		return nil, fmt.Errorf("repository not found: %s", repoPrefix)
	}
	if idx == nil {
		// Tracked but no live indexer (e.g. restored from snapshot
		// without one) — fall back to a full re-index, which rebuilds
		// the per-repo indexer from scratch.
		return mi.IndexRepo(repoPrefix)
	}

	result, err := idx.IncrementalReindexPaths(meta.RootPath, paths)
	if err != nil {
		return nil, fmt.Errorf("reindexing %s: %w", meta.RootPath, err)
	}

	mi.mu.Lock()
	mi.repos[repoPrefix] = &RepoMetadata{
		RepoPrefix:    repoPrefix,
		RootPath:      meta.RootPath,
		Identity:      meta.Identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
	}
	mi.mu.Unlock()

	mi.ReconcileContractEdges()

	return result, nil
}

// TrackRepo validates the path, detects identity, indexes, and adds to config.
func (mi *MultiIndexer) TrackRepo(entry config.RepoEntry) (*IndexResult, error) {
	return mi.TrackRepoCtx(context.Background(), entry)
}

// TrackRepoCtx is TrackRepo with a context, allowing callers to pipe progress
// reporters (via progress.WithReporter) through to the underlying Index call.
func (mi *MultiIndexer) TrackRepoCtx(ctx context.Context, entry config.RepoEntry) (*IndexResult, error) {
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}

	// Validate path exists and is a directory.
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}

	identity, err := DetectIdentity(absPath)
	if err != nil {
		return nil, fmt.Errorf("detecting identity for %s: %w", absPath, err)
	}

	prefix := config.ResolvePrefix(entry)
	if prefix == "" || prefix == "." {
		prefix = identity.RepoPrefix
	}

	// Check if already tracked.
	mi.mu.RLock()
	if _, exists := mi.repos[prefix]; exists {
		mi.mu.RUnlock()
		return nil, nil // already tracked
	}
	hook := mi.onRepoTracked
	mi.mu.RUnlock()

	if hook != nil {
		hook(prefix, absPath)
	}

	// Determine if we need to prefix. We must consider both repos already
	// indexed in mi.repos AND the total repos configured — at daemon warmup
	// TrackRepoCtx is called in a loop over all configured repos, so at
	// iteration 0 mi.repos is empty while the config already has N entries.
	// Counting only mi.repos used to leave the first-indexed repo without a
	// prefix while every later repo got one, producing two ID schemes for
	// the same graph and halving cross-file edge density.
	totalConfigured := 1 // ourselves
	if mi.configMgr != nil {
		totalConfigured = len(mi.configMgr.Global().Repos)
	}
	willBeMultiRepo := len(mi.repos)+1 >= 2 || totalConfigured >= 2

	// Lazy-load the per-repo `.gortex.yaml` so GetRepoConfig sees the
	// workspace / project slugs declared inside the repo. Without
	// this the production code path never reads the file and every
	// repo silently falls back to `workspace = repoPrefix`, making
	// shared-workspace cross-repo matching impossible to express. Idempotent
	// — repeated calls just re-cache the parse.
	if mi.configMgr != nil {
		mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
	}

	cfg := mi.configMgr.GetRepoConfig(prefix)
	idx := mi.newPerRepoIndexer(cfg.Index)
	if willBeMultiRepo {
		idx.SetRepoPrefix(prefix)
	}
	// Workspace / project slugs stamped on every node. Resolution
	// order (highest priority first): RepoEntry.Workspace from the
	// global config (lets users pin OSS repos without committing a
	// `.gortex.yaml`) → `.gortex.yaml::workspace` → repoPrefix
	// (default). resolveWorkspaceID encodes the precedence; the
	// WorkspaceID-keyed contract registry and the boundary-enforced
	// matcher both consume the result.
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))

	result, err := idx.IndexCtx(ctx, absPath)
	if err != nil {
		return nil, fmt.Errorf("indexing %s: %w", absPath, err)
	}

	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix:    prefix,
		RootPath:      absPath,
		Identity:      identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		IsWorktree:    ResolveWorktree(absPath).IsWorktree,
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	// Add to global config.
	entry.Path = absPath
	if err := mi.configMgr.Global().AddRepo(entry); err != nil {
		mi.logger.Warn("failed to add repo to config", zap.Error(err))
	}

	// Skip the per-repo contract reconcile when batching: it walks every
	// edge in the shared graph to evict stale EdgeMatches and rebuilds
	// the matcher across every indexer, so paying it once per repo on a
	// warmup over 100+ repos is O(R · E). The batch caller runs it once
	// after the loop (RunGlobalResolve does, and the janitor's ReconcileAll
	// fires it post-loop too).
	if !mi.deferGlobalPasses {
		mi.ReconcileContractEdges()
	}

	return result, nil
}

// ReconcileRepoCtx registers a repo that already has nodes in the graph
// (typically restored from a daemon snapshot) and brings it back into
// agreement with the filesystem without a full re-index. priorMtimes
// carries the mtimes recorded at the time the snapshot was taken;
// IncrementalReindex uses them to detect files that changed offline
// (re-indexes) and files that were deleted offline (evicts).
//
// Falls back to TrackRepoCtx when the repo is not yet tracked AND no
// prior mtimes are available — in that case there's nothing to
// reconcile against and a full index is the correct path.
func (mi *MultiIndexer) ReconcileRepoCtx(ctx context.Context, entry config.RepoEntry, priorMtimes map[string]int64) (*IndexResult, error) {
	start := time.Now()

	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", absPath)
	}
	identity, err := DetectIdentity(absPath)
	if err != nil {
		return nil, fmt.Errorf("detecting identity for %s: %w", absPath, err)
	}

	prefix := config.ResolvePrefix(entry)
	if prefix == "" || prefix == "." {
		prefix = identity.RepoPrefix
	}

	// Already tracked — nothing to do.
	mi.mu.RLock()
	_, exists := mi.repos[prefix]
	mi.mu.RUnlock()
	if exists {
		return nil, nil
	}

	// Fall back to full TrackRepoCtx when we have no prior mtimes:
	// there's nothing meaningful to reconcile against, and
	// IncrementalReindex would treat every file as stale, producing
	// the same duplicate-writes problem we're fixing.
	if len(priorMtimes) == 0 {
		return mi.TrackRepoCtx(ctx, entry)
	}

	totalConfigured := 1
	if mi.configMgr != nil {
		totalConfigured = len(mi.configMgr.Global().Repos)
	}
	willBeMultiRepo := len(mi.repos)+1 >= 2 || totalConfigured >= 2

	// Pick up `.gortex.yaml` workspace/project declarations on
	// reconcile too — config can change between sessions, and warmup-
	// time reconcile is the path that runs after a daemon restart.
	if mi.configMgr != nil {
		mi.configMgr.LoadWorkspaceConfig(prefix, absPath)
	}

	cfg := mi.configMgr.GetRepoConfig(prefix)
	idx := mi.newPerRepoIndexer(cfg.Index)
	if willBeMultiRepo {
		idx.SetRepoPrefix(prefix)
	}
	entryCopy := entry
	idx.SetWorkspaceID(resolveWorkspaceID(&entryCopy, cfg, prefix))
	idx.SetProjectID(resolveProjectID(&entryCopy, cfg, prefix))
	idx.SetRootPath(absPath)
	idx.SetFileMtimes(priorMtimes)

	result, err := idx.IncrementalReindex(absPath)
	if err != nil {
		return nil, fmt.Errorf("reconciling %s: %w", absPath, err)
	}

	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix:    prefix,
		RootPath:      absPath,
		Identity:      identity,
		LastIndexTime: time.Now(),
		FileCount:     result.FileCount,
		NodeCount:     result.NodeCount,
		EdgeCount:     result.EdgeCount,
		ParseErrors:   result.Errors,
		FileMtimes:    idx.FileMtimes(),
		IsWorktree:    ResolveWorktree(absPath).IsWorktree,
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	entry.Path = absPath
	if err := mi.configMgr.Global().AddRepo(entry); err != nil {
		mi.logger.Warn("failed to add repo to config", zap.Error(err))
	}

	// See TrackRepoCtx for why this is skipped under deferGlobalPasses.
	if !mi.deferGlobalPasses {
		mi.ReconcileContractEdges()
	}

	mi.logger.Info("daemon: reconciled repo from snapshot",
		zap.String("prefix", prefix),
		zap.Int("stale_files_reindexed", result.StaleFileCount),
		zap.Duration("elapsed", time.Since(start)))

	return result, nil
}

// ReconcileAll runs IncrementalReindex on every tracked repo. Used by
// the daemon's periodic janitor to catch files whose mutations slipped
// past fsnotify — inotify watch limits, NFS / SMB mounts, kernel event
// queue overflow, or daemon downtime where edits happened while nobody
// was listening. Cheap on steady-state repos (one filepath.WalkDir +
// per-file os.Stat per repo), and correctness is self-healing: whatever
// was missed gets picked up on the next tick.
//
// Returns a map of prefix → IndexResult for logging / metrics. Errors
// per repo are logged and do not abort the rest of the sweep — a broken
// permission bit on one repo should not starve reconciliation on the
// others.
func (mi *MultiIndexer) ReconcileAll() map[string]*IndexResult {
	mi.mu.RLock()
	prefixes := make([]string, 0, len(mi.indexers))
	for p := range mi.indexers {
		prefixes = append(prefixes, p)
	}
	mi.mu.RUnlock()

	// Same batch trick as warmup: each per-repo IncrementalReindex
	// triggers an O(global) InferImplements/InferOverrides walk if we
	// don't suppress it. With ~100 repos that's ~100× the work for the
	// hourly janitor.
	mi.BeginBatch()
	defer mi.EndBatch()

	results := make(map[string]*IndexResult, len(prefixes))
	reindexed := 0
	for _, prefix := range prefixes {
		mi.mu.RLock()
		idx, ok := mi.indexers[prefix]
		meta, metaOK := mi.repos[prefix]
		mi.mu.RUnlock()
		if !ok || !metaOK || meta == nil || meta.RootPath == "" {
			continue
		}
		result, err := idx.IncrementalReindex(meta.RootPath)
		if err != nil {
			mi.logger.Warn("janitor: reconcile failed",
				zap.String("prefix", prefix), zap.Error(err))
			continue
		}
		if result != nil && result.StaleFileCount > 0 {
			mi.logger.Info("janitor: reconciled repo",
				zap.String("prefix", prefix),
				zap.Int("stale_files_reindexed", result.StaleFileCount))
			reindexed++
		}
		results[prefix] = result

		// Keep RepoMetadata.FileMtimes in sync so the next snapshot
		// picks up the reconciled mtimes.
		mi.mu.Lock()
		if m, ok := mi.repos[prefix]; ok && m != nil {
			m.FileMtimes = idx.FileMtimes()
			m.LastIndexTime = time.Now()
		}
		mi.mu.Unlock()
	}

	if reindexed > 0 {
		mi.ReconcileContractEdges()
	}
	return results
}

// UntrackRepo evicts a repo from the graph and removes it from config.
func (mi *MultiIndexer) UntrackRepo(repoPrefix string) (int, int) {
	mi.mu.Lock()
	meta, ok := mi.repos[repoPrefix]
	if !ok {
		mi.mu.Unlock()
		return 0, 0
	}
	delete(mi.repos, repoPrefix)
	delete(mi.indexers, repoPrefix)
	mi.mu.Unlock()

	nodesRemoved, edgesRemoved := mi.graph.EvictRepo(repoPrefix)

	// Remove from global config.
	if meta.RootPath != "" {
		if err := mi.configMgr.Global().RemoveRepo(meta.RootPath); err != nil {
			mi.logger.Warn("failed to remove repo from config",
				zap.String("prefix", repoPrefix), zap.Error(err))
		}
	}

	mi.ReconcileContractEdges()

	return nodesRemoved, edgesRemoved
}

// WorktreeGC is the per-repo outcome of GCVanishedWorktrees.
type WorktreeGC struct {
	RepoPrefix   string
	RootPath     string
	NodesRemoved int
	EdgesRemoved int
}

// GCVanishedWorktrees garbage-collects the index of any tracked linked
// git worktree whose root directory has disappeared from disk — the
// `git worktree remove` (or manual deletion) case. Each vanished
// worktree's branch-keyed snapshot slot and graph nodes would otherwise
// leak forever: a removed worktree never fires a per-file fsnotify
// delete for its whole tree, and the janitor's IncrementalReindex just
// errors out on the missing root without evicting anything.
//
// Only repos recorded as worktrees (RepoMetadata.IsWorktree) are
// eligible — a vanished *main* checkout is left alone, since that is
// far more likely a transient mount problem than an intentional
// removal, and untracking it would also orphan every linked worktree
// that shares its .git. The directory-existence test uses the same
// not-exist-only rule as the per-file deletion detector, so a flaky
// filesystem cannot trigger a destructive eviction.
//
// Returns one WorktreeGC record per repo evicted; an empty slice when
// every tracked worktree is still present.
func (mi *MultiIndexer) GCVanishedWorktrees() []WorktreeGC {
	// Snapshot the candidate set under the read lock, then evict
	// outside it — UntrackRepo takes the write lock itself.
	type candidate struct {
		prefix string
		root   string
	}
	var candidates []candidate
	mi.mu.RLock()
	for prefix, meta := range mi.repos {
		if meta == nil || !meta.IsWorktree || meta.RootPath == "" {
			continue
		}
		if WorktreeRootGone(meta.RootPath) {
			candidates = append(candidates, candidate{prefix: prefix, root: meta.RootPath})
		}
	}
	mi.mu.RUnlock()

	if len(candidates) == 0 {
		return nil
	}

	out := make([]WorktreeGC, 0, len(candidates))
	for _, c := range candidates {
		nodes, edges := mi.UntrackRepo(c.prefix)
		mi.logger.Info("janitor: garbage-collected vanished worktree",
			zap.String("prefix", c.prefix),
			zap.String("root", c.root),
			zap.Int("nodes_removed", nodes),
			zap.Int("edges_removed", edges))
		out = append(out, WorktreeGC{
			RepoPrefix:   c.prefix,
			RootPath:     c.root,
			NodesRemoved: nodes,
			EdgesRemoved: edges,
		})
	}
	return out
}

// GetMetadata returns the metadata for a specific repo, or nil if not found.
func (mi *MultiIndexer) GetMetadata(repoPrefix string) *RepoMetadata {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return mi.repos[repoPrefix]
}

// AllMetadata returns a copy of all repo metadata.
func (mi *MultiIndexer) AllMetadata() map[string]*RepoMetadata {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	out := make(map[string]*RepoMetadata, len(mi.repos))
	for k, v := range mi.repos {
		out[k] = v
	}
	return out
}

// IsMultiRepo returns true when more than one repo is tracked.
func (mi *MultiIndexer) IsMultiRepo() bool {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return len(mi.repos) > 1
}

// RepoForFile returns the repo prefix for a given file path by checking
// which repo root contains it. Returns empty string if no match.
func (mi *MultiIndexer) RepoForFile(filePath string) string {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return ""
	}

	mi.mu.RLock()
	defer mi.mu.RUnlock()

	var bestPrefix string
	var bestLen int

	for prefix, meta := range mi.repos {
		if strings.HasPrefix(absPath, meta.RootPath+string(filepath.Separator)) || absPath == meta.RootPath {
			if len(meta.RootPath) > bestLen {
				bestLen = len(meta.RootPath)
				bestPrefix = prefix
			}
		}
	}

	return bestPrefix
}

// GetIndexer returns the Indexer for a specific repo prefix, or nil.
func (mi *MultiIndexer) GetIndexer(repoPrefix string) *Indexer {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return mi.indexers[repoPrefix]
}

// IndexerForFile routes an absolute path to the per-repo Indexer that
// owns it. Returns (nil, "") when no tracked repo contains the path.
// Used by the MCP overlay middleware to find the right Indexer for a
// pushed file when constructing the per-request overlay layer.
func (mi *MultiIndexer) IndexerForFile(absPath string) (*Indexer, string) {
	prefix := mi.RepoForFile(absPath)
	if prefix == "" {
		return nil, ""
	}
	return mi.GetIndexer(prefix), prefix
}

// ResolveFilePath takes a repo-prefixed relative path (e.g. "ade/internal/foo.go")
// and returns the absolute filesystem path by looking up the repo's root directory.
// Returns empty string if the repo prefix is not found.
func (mi *MultiIndexer) ResolveFilePath(prefixedPath string) string {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	for prefix, meta := range mi.repos {
		if strings.HasPrefix(prefixedPath, prefix+"/") {
			relPath := strings.TrimPrefix(prefixedPath, prefix+"/")
			return filepath.Join(meta.RootPath, relPath)
		}
	}
	return ""
}

// RepoPrefixes returns the set of registered repo prefixes. The returned
// slice is a snapshot — safe to retain and iterate concurrently with
// other multi-indexer operations. Order is unspecified; callers that
// need stability should sort.
func (mi *MultiIndexer) RepoPrefixes() []string {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	prefixes := make([]string, 0, len(mi.repos))
	for prefix := range mi.repos {
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

// RepoRoot returns the local filesystem root for the given repo prefix.
// ok is true only when the prefix is registered AND meta.RootPath is non-empty.
// Caller is responsible for joining repo-relative file paths against the root.
func (mi *MultiIndexer) RepoRoot(repoPrefix string) (string, bool) {
	if repoPrefix == "" {
		return "", false
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	meta, ok := mi.repos[repoPrefix]
	if !ok || meta == nil || meta.RootPath == "" {
		return "", false
	}
	return meta.RootPath, true
}

// LinkedWorktreeRoots returns the on-disk roots of every tracked linked
// git worktree that shares its .git common directory with the checkout
// at mainRepoPath — i.e. the worktree siblings of that main repo. The
// query is keyed on the resolved MainRepoPath so it matches whether the
// caller passes a main checkout or one of its worktrees.
//
// Used by the edit-tool file resolver: because all worktrees of one
// repo reuse a single index identity, a repo-relative path can resolve
// against a sibling checkout. When the resolved file belongs to a
// linked worktree, the resolver re-roots the write there so an edit
// lands in the worktree's copy rather than the main checkout's.
func (mi *MultiIndexer) LinkedWorktreeRoots(mainRepoPath string) []string {
	if mainRepoPath == "" {
		return nil
	}
	wantMain := resolvedMainRepo(mainRepoPath)

	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var out []string
	for _, meta := range mi.repos {
		if meta == nil || meta.RootPath == "" || !meta.IsWorktree {
			continue
		}
		if resolvedMainRepo(meta.RootPath) == wantMain {
			out = append(out, meta.RootPath)
		}
	}
	return out
}

// resolvedMainRepo resolves a checkout path to its repo's main worktree
// root with symlinks evaluated. ResolveWorktree derives the main path
// two different ways — filepath.Abs for a main checkout vs git's
// canonicalized `commondir` for a linked worktree — and on platforms
// where the temp / home tree is a symlink (macOS /var -> /private/var)
// those two forms differ for the same repo. Evaluating symlinks on the
// result gives one stable identity that both inputs agree on.
func resolvedMainRepo(path string) string {
	main := ResolveWorktree(path).MainRepoPath
	if resolved, err := filepath.EvalSymlinks(main); err == nil {
		return resolved
	}
	return main
}

// MergedContractRegistry combines contract registries from all per-repo
// indexers into a single registry. In multi-repo mode each repo's indexer
// runs extractContracts independently; this merges the results.
func (mi *MultiIndexer) MergedContractRegistry() *contracts.Registry {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	merged := contracts.NewRegistry()
	for repoPrefix, idx := range mi.indexers {
		cr := idx.ContractRegistry()
		if cr == nil {
			continue
		}
		// Re-stamp the workspace/project slugs from the indexer
		// alongside the repo prefix on merge. The contracts already
		// carry these slugs from
		// their source registry, but AddAllScoped is idempotent (skips
		// non-empty existing values) so this stays correct even if a
		// future code path forgets the stamp on first insert.
		merged.AddAllScoped(cr.All(), repoPrefix, idx.WorkspaceID(), idx.ProjectID())
	}
	return merged
}

// attachInlinedShapes folds the field shape of each contract's
// response_type / request_type into the contract's Meta so the
// dashboard can render the expanded field list. Targets contracts
// where the type has been resolved to a graph node ID (contains
// "::") AND the type node has a shape stored in its Meta.
//
// Type-shape extraction normally runs in commitContracts via
// snapshotContractShapes + inlineEnvelopeShapes — but those passes
// run during the initial extract and miss contracts added later by
// InlineWrappers. This is the post-inline equivalent: it doesn't
// re-extract shapes (the type nodes already have them from
// snapshotContractShapes if they were referenced anywhere), it just
// attaches them to the new contract entries.
func (mi *MultiIndexer) attachInlinedShapes(cr *contracts.Registry, g *graph.Graph) {
	idsToTouch := map[string]bool{}
	for _, c := range cr.All() {
		if c.Meta == nil {
			continue
		}
		for _, key := range []string{"response_type", "request_type"} {
			if v, _ := c.Meta[key].(string); v != "" && strings.Contains(v, "::") {
				idsToTouch[c.ID] = true
				break
			}
		}
		if env, ok := c.Meta["response_envelope"].([]map[string]any); ok && len(env) > 0 {
			// Touch any contract that has an envelope, even when
			// the rows still carry bare type names — the loop below
			// upgrades them. Otherwise we skip them and lose the
			// shape attachment for sibling-file types.
			idsToTouch[c.ID] = true
			_ = env
		}
	}
	srcCache := map[string][]byte{}
	resolveShape := func(typeID string) any {
		if typeID == "" || !strings.Contains(typeID, "::") {
			return nil
		}
		node := g.GetNode(typeID)
		if node == nil {
			return nil
		}
		if node.Kind != graph.KindType && node.Kind != graph.KindInterface {
			return nil
		}
		if node.Meta == nil {
			node.Meta = map[string]any{}
		}
		if shape, ok := node.Meta["shape"]; ok && shape != nil {
			return shape
		}
		// Lazy-extract: snapshotContractShapes only walks types
		// referenced by the initial bulk extract. Types referenced
		// ONLY by wrapper-inlined contracts need this fallback or
		// their fields stay unread.
		src := srcCache[node.FilePath]
		if src == nil {
			data, ok := mi.readNodeSource(node)
			if !ok {
				srcCache[node.FilePath] = []byte{}
				return nil
			}
			src = data
			srcCache[node.FilePath] = src
		}
		if len(src) == 0 {
			return nil
		}
		extracted := contracts.ExtractShape(node.FilePath, src, node.StartLine, node.EndLine)
		if extracted == nil {
			return nil
		}
		node.Meta["shape"] = extracted
		return extracted
	}
	for id := range idsToTouch {
		items := cr.ByID(id)
		changed := false
		for i := range items {
			if items[i].Meta == nil {
				continue
			}
			// Top-level request/response type shapes.
			for _, pair := range []struct{ typeKey, shapeKey string }{
				{"response_type", "response_shape"},
				{"request_type", "request_shape"},
			} {
				if _, has := items[i].Meta[pair.shapeKey]; has {
					continue
				}
				typeID, _ := items[i].Meta[pair.typeKey].(string)
				if shape := resolveShape(typeID); shape != nil {
					items[i].Meta[pair.shapeKey] = shape
					changed = true
				}
			}
			// Envelope rows — upgrade bare type names to graph IDs
			// (so the shape lookup hits) and attach shapes.
			if env, ok := items[i].Meta["response_envelope"].([]map[string]any); ok && len(env) > 0 {
				envChanged := false
				for ri, row := range env {
					typeID, _ := row["type"].(string)
					// Upgrade bare type name → graph ID when the
					// in-file resolveTypeInFile pass left it bare
					// (the type lives in a sibling file).
					if typeID != "" && !strings.Contains(typeID, "::") {
						matches := g.FindNodesByName(typeID)
						var resolved string
						for _, n := range matches {
							if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
								continue
							}
							resolved = n.ID
							if items[i].RepoPrefix != "" && strings.HasPrefix(n.ID, items[i].RepoPrefix+"/") {
								break // prefer same-repo
							}
						}
						if resolved != "" {
							env[ri]["type"] = resolved
							typeID = resolved
							envChanged = true
						}
					}
					if _, has := row["shape"]; has {
						continue
					}
					if shape := resolveShape(typeID); shape != nil {
						env[ri]["shape"] = shape
						envChanged = true
					}
				}
				if envChanged {
					items[i].Meta["response_envelope"] = env
					changed = true
				}
			}
		}
		if changed {
			cr.ReplaceByID(id, items)
		}
	}
}

// readNodeSource returns the source bytes of the file the node lives
// in, resolving the repo prefix to a real disk path via tracked-repo
// metadata. Mirrors wrapperSourceReader's path-resolution dance.
func (mi *MultiIndexer) readNodeSource(node *graph.Node) ([]byte, bool) {
	if node == nil || node.FilePath == "" {
		return nil, false
	}
	rel := node.FilePath
	if node.RepoPrefix != "" {
		meta := mi.GetMetadata(node.RepoPrefix)
		if meta == nil || meta.RootPath == "" {
			return nil, false
		}
		rel = strings.TrimPrefix(rel, node.RepoPrefix+"/")
		data, err := os.ReadFile(filepath.Join(meta.RootPath, rel))
		if err != nil {
			return nil, false
		}
		return data, true
	}
	for _, m := range mi.AllMetadata() {
		data, err := os.ReadFile(filepath.Join(m.RootPath, rel))
		if err == nil {
			return data, true
		}
	}
	return nil, false
}

// wrapperSourceReader returns a SourceReader closure that maps a graph
// node back to its on-disk bytes by joining the node's repo-relative
// FilePath with the repo's RootPath from MultiIndexer metadata. In
// single-repo mode (no RepoPrefix on nodes) the indexer's sole root is
// used. Read results are memoized inside the closure so multi-caller
// wrappers don't trigger N disk reads per file.
func (mi *MultiIndexer) wrapperSourceReader() contracts.SourceReader {
	cache := make(map[string][]byte)
	miss := make(map[string]struct{})

	readFile := func(absPath string) ([]byte, bool) {
		if b, ok := cache[absPath]; ok {
			return b, true
		}
		if _, skipped := miss[absPath]; skipped {
			return nil, false
		}
		b, err := os.ReadFile(absPath)
		if err != nil {
			miss[absPath] = struct{}{}
			return nil, false
		}
		cache[absPath] = b
		return b, true
	}

	return func(n *graph.Node) ([]byte, bool) {
		if n == nil || n.FilePath == "" {
			return nil, false
		}
		// Multi-repo case: strip "<repo-prefix>/" from the FilePath and
		// join with the repo's recorded root.
		if n.RepoPrefix != "" {
			meta := mi.GetMetadata(n.RepoPrefix)
			if meta == nil || meta.RootPath == "" {
				return nil, false
			}
			rel := strings.TrimPrefix(n.FilePath, n.RepoPrefix+"/")
			return readFile(filepath.Join(meta.RootPath, rel))
		}
		// Single-repo fallback: a node without a RepoPrefix carries its
		// path relative to the sole indexer root. Try each known repo
		// root since the single-repo path wraps through indexSingleRepo.
		for _, meta := range mi.AllMetadata() {
			if b, ok := readFile(filepath.Join(meta.RootPath, n.FilePath)); ok {
				return b, true
			}
		}
		// Last resort: treat FilePath as already absolute (tests).
		return readFile(n.FilePath)
	}
}

// ReconcileContractEdges walks the merged contract registry, runs the
// consumer↔provider matcher, and writes the results into the graph as
// EdgeMatches edges pointing from consumer-contract nodes to their matched
// provider-contract nodes. Every call evicts the prior set of EdgeMatches
// first so the edges stay in sync with the current contract view; that's
// correct after track / untrack / re-index / watcher re-scan. Returns the
// number of match edges added so callers can log or test the effect.
//
// This is what makes get_call_chain traverse service boundaries: without a
// persisted contract→contract edge, the matcher's result is only visible
// via the `contracts check` tool and traversals stop at each service's
// boundary.
func (mi *MultiIndexer) ReconcileContractEdges() int {
	g := mi.Graph()
	if g == nil {
		return 0
	}

	// Evict any prior EdgeMatches so the graph reflects only the current
	// registry. Collect first, remove second — we're mutating the same
	// out-edge lists we'd be iterating otherwise.
	type edgeKey struct{ from, to string }
	var stale []edgeKey
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeMatches {
			stale = append(stale, edgeKey{e.From, e.To})
		}
	}
	for _, k := range stale {
		g.RemoveEdge(k.from, k.to, graph.EdgeMatches)
	}

	merged := mi.MergedContractRegistry()
	if merged == nil {
		return 0
	}

	// Inline HTTP wrapper callers (T2.4). Codebases that route every
	// endpoint through a helper like `request(path, ...)` produce one
	// parametric consumer contract per wrapper at extraction time —
	// useless for matching. InlineWrappers walks incoming call edges
	// of each wrapper, re-reads the caller's source, and emits a
	// specific consumer contract per literal path. The returned
	// contracts are also persisted into their owning repo's per-repo
	// registry so subsequent `contracts list`/`check` calls see them
	// (MergedContractRegistry rebuilds fresh each call and would
	// otherwise lose them).
	inlined := contracts.InlineWrappers(merged, g, mi.wrapperSourceReader())
	if len(inlined) > 0 {
		mi.mu.RLock()
		for _, c := range inlined {
			if c.RepoPrefix == "" {
				continue
			}
			idx, ok := mi.indexers[c.RepoPrefix]
			if !ok {
				continue
			}
			cr := idx.ContractRegistry()
			if cr == nil {
				continue
			}
			// Skip if the same contract is already persisted —
			// ReconcileContractEdges runs on every repo change, and
			// appending the same inlined contract on every pass would
			// blow up the registry with duplicates. Compare on the
			// Registry.All() dedupe key.
			alreadyPersisted := false
			for _, existing := range cr.ByID(c.ID) {
				if existing.SymbolID == c.SymbolID &&
					existing.FilePath == c.FilePath &&
					existing.Role == c.Role {
					alreadyPersisted = true
					break
				}
			}
			if !alreadyPersisted {
				cr.Add(c)
			}
		}
		mi.mu.RUnlock()

		// Wrapper-inlined contracts arrive AFTER commitContracts ran
		// its UpgradeBareTypeRefs pass, so their response_type /
		// request_type still carries bare names like "ToolInfo".
		// Re-run the upgrade against the merged graph so downstream
		// snapshotContractShapes finds the type node and the
		// dashboard sees fields instead of a string.
		mi.mu.RLock()
		lookup := func(name, repoHint string) []string {
			matches := mi.graph.FindNodesByName(name)
			if len(matches) == 0 {
				return nil
			}
			ids := make([]string, 0, len(matches))
			for _, n := range matches {
				if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
					continue
				}
				ids = append(ids, n.ID)
			}
			// Prefer same-repo when multiple match.
			if len(ids) > 1 && repoHint != "" {
				var sameRepo []string
				for _, id := range ids {
					if strings.HasPrefix(id, repoHint+"/") {
						sameRepo = append(sameRepo, id)
					}
				}
				if len(sameRepo) > 0 {
					return sameRepo
				}
			}
			return ids
		}
		for _, idx := range mi.indexers {
			cr := idx.ContractRegistry()
			if cr != nil {
				cr.UpgradeBareTypeRefs(lookup)
			}
		}
		// UpgradeBareTypeRefs leaves names with ≥2 candidates alone
		// (e.g. a TS app declaring `DashboardSnapshot` in both
		// `lib/schema.ts` and `lib/types.ts`). disambiguateBareTypesViaImports
		// re-reads the consumer's source, parses its `import` lines,
		// and picks the candidate whose graph FilePath matches an
		// imported module. Runs before attachInlinedShapes so the
		// shape attachment sees fully-qualified IDs.
		for _, idx := range mi.indexers {
			cr := idx.ContractRegistry()
			if cr != nil {
				mi.disambiguateBareTypesViaImports(cr, mi.graph)
			}
		}
		// Now that response_type / request_type point at real graph
		// nodes, fold each referenced type's shape (struct fields)
		// into the contract's Meta so the dashboard renders the
		// expanded field list instead of just the type name. Mirrors
		// what snapshotContractShapes + inlineEnvelopeShapes do for
		// initially-extracted contracts.
		for _, idx := range mi.indexers {
			cr := idx.ContractRegistry()
			if cr == nil {
				continue
			}
			mi.attachInlinedShapes(cr, mi.graph)
		}
		mi.mu.RUnlock()
	}

	// Bind provider-contract SymbolIDs that came from spec files
	// (.proto for gRPC, OpenAPI YAML/JSON for HTTP). Without this
	// step the matcher finds pairs but the bridge-emission check
	// below skips them because provider SymbolID is empty. Must run
	// before Match so Match sees the updated records.
	contracts.BindProviderSymbols(merged, g)

	result := contracts.Match(merged)
	added := 0
	for _, m := range result.Matched {
		// Connect the consumer's enclosing symbol directly to the
		// provider's enclosing symbol. Contract nodes in the graph are
		// deduped by Contract.ID, so a provider and a consumer that share
		// the same ID collapse into one node — a contract→contract edge
		// would be a self-loop. Symbol→symbol bypasses that and gives
		// get_call_chain the traversal it needs: saveTuck (extension) →
		// Handler.CreateTuck (core-api).
		if m.Provider.SymbolID == "" || m.Consumer.SymbolID == "" {
			continue
		}
		if m.Provider.SymbolID == m.Consumer.SymbolID {
			continue
		}
		g.AddEdge(&graph.Edge{
			From:            m.Consumer.SymbolID,
			To:              m.Provider.SymbolID,
			Kind:            graph.EdgeMatches,
			FilePath:        m.Consumer.FilePath,
			Line:            m.Consumer.Line,
			Confidence:      1.0,
			ConfidenceLabel: "EXTRACTED",
			Origin:          graph.OriginASTResolved,
			CrossRepo:       m.CrossRepo,
		})
		added++
	}
	return added
}

// Graph returns the underlying shared graph.
func (mi *MultiIndexer) Graph() *graph.Graph {
	return mi.graph
}

// Search returns the shared search backend.
func (mi *MultiIndexer) Search() search.Backend {
	return mi.search
}

// ExportVectorIndex serializes the workspace-global semantic-search
// vector index — there is one shared HNSW index across every tracked
// repo, not one per repo. Returns nil, 0, 0 when no vector index is
// active (embeddings disabled, or the backend is still text-only).
// Used by the daemon snapshot path so a default-on daemon does not
// re-embed the whole graph on every restart.
func (mi *MultiIndexer) ExportVectorIndex() ([]byte, int, int) {
	sw, ok := mi.search.(*search.Swappable)
	if !ok {
		return nil, 0, 0
	}
	hybrid, ok := sw.Inner().(*search.HybridBackend)
	if !ok {
		return nil, 0, 0
	}
	vec := hybrid.VectorIndex()
	if vec == nil || vec.Count() == 0 {
		return nil, 0, 0
	}
	var buf bytes.Buffer
	if err := vec.Save(&buf); err != nil {
		mi.logger.Warn("failed to export vector index", zap.Error(err))
		return nil, 0, 0
	}
	return buf.Bytes(), vec.Dims(), vec.Count()
}

// ImportVectorIndex restores a previously-exported vector index into
// the shared search backend, wrapping the current text backend in a
// HybridBackend. It is a no-op when embeddings are disabled (no
// configured embedder) or when the cached index's dimensionality does
// not match the active embedder — a provider switch (GloVe 50d → ONNX
// 384d) makes the cached vectors meaningless, so the indexer re-embeds
// instead. Returns an error only on a structurally corrupt index blob.
func (mi *MultiIndexer) ImportVectorIndex(data []byte, dims, count int) error {
	if len(data) == 0 || mi.embedder == nil {
		return nil
	}
	if embedderDims := mi.embedder.Dimensions(); embedderDims > 0 && embedderDims != dims {
		mi.logger.Info("vector index dims mismatch, will re-embed",
			zap.Int("cached_dims", dims), zap.Int("embedder_dims", embedderDims))
		return nil
	}
	sw, ok := mi.search.(*search.Swappable)
	if !ok {
		return nil
	}
	vec := search.NewVector(dims)
	if err := vec.LoadFrom(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("import vector index: %w", err)
	}
	vec.SetCount(count)

	// Unwrap an existing HybridBackend to its text side before
	// re-wrapping so we never nest Hybrids (each retains a stale
	// vector index — see buildSearchIndex for the memory rationale).
	inner := sw.Inner()
	if hyb, ok := inner.(*search.HybridBackend); ok {
		inner = hyb.TextBackend()
	}
	sw.Swap(search.NewHybrid(inner, vec, mi.embedder))
	mi.logger.Info("restored vector index from snapshot",
		zap.Int("vectors", count), zap.Int("dims", dims))
	return nil
}

// AutoDetectRepos walks immediate subdirectories of parentPath looking for
// .git directories. If parentPath itself is a Git repo, it returns a single
// entry (the caller should index it as single-repo). If zero Git repos are
// found, it returns nil so the caller can fall back to single-repo mode.
// This is gated by the workspace.auto_detect config flag.
func (mi *MultiIndexer) AutoDetectRepos(parentPath string) []config.RepoEntry {
	absPath, err := filepath.Abs(parentPath)
	if err != nil {
		mi.logger.Warn("auto-detect: failed to resolve path", zap.String("path", parentPath), zap.Error(err))
		return nil
	}

	// If the path itself is a Git repo, return it as a single repo.
	if isGitRepo(absPath) {
		return []config.RepoEntry{{
			Path: absPath,
			Name: filepath.Base(absPath),
		}}
	}

	// Walk immediate subdirectories (not recursive) for .git dirs.
	entries, err := os.ReadDir(absPath)
	if err != nil {
		mi.logger.Warn("auto-detect: failed to read directory", zap.String("path", absPath), zap.Error(err))
		return nil
	}

	var repos []config.RepoEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subDir := filepath.Join(absPath, entry.Name())
		if isGitRepo(subDir) {
			repos = append(repos, config.RepoEntry{
				Path: subDir,
				Name: entry.Name(), // Derive RepoPrefix from subdirectory name.
			})
		}
	}

	// If zero Git repos found, return nil — caller falls back to single-repo.
	if len(repos) == 0 {
		return nil
	}

	return repos
}

// isGitRepo checks whether the given directory contains a .git subdirectory.
func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}
