package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pelletier/go-toml/v2"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/codegen"
	"github.com/zzet/gortex/internal/codeowners"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/fixtures"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/licenses"
	"github.com/zzet/gortex/internal/modules"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/todos"
)

// IndexResult holds the outcome of an indexing operation.
type IndexResult struct {
	NodeCount  int          `json:"node_count"`
	EdgeCount  int          `json:"edge_count"`
	FileCount  int          `json:"file_count"`
	DurationMs int64        `json:"duration_ms"`
	Errors     []IndexError `json:"errors,omitempty"`
}

// IndexError records a per-file parsing failure.
type IndexError struct {
	FilePath string `json:"file_path"`
	Error    string `json:"error"`
}

// Indexer walks a repository and populates the graph.
type Indexer struct {
	graph       *graph.Graph
	registry    *parser.Registry
	resolver    *resolver.Resolver
	search      search.Backend
	config      config.IndexConfig
	excludes    *excludes.Matcher
	excludeOnce sync.Once
	rootPath    string
	logger      *zap.Logger

	// repoPrefix is set in multi-repo mode to prefix all file paths and node IDs.
	// When empty, the indexer operates in single-repo mode (backward compatible).
	repoPrefix string

	// workspaceID is the hard graph boundary slug for this repo.
	// Stamped onto every node emitted by this indexer via applyRepoPrefix
	// so query-time scoping doesn't have to look it up by repo prefix.
	// Defaults at the MultiIndexer layer to the per-repo `.gortex.yaml`
	// `workspace:` slug, falling back to repoPrefix when no slug is
	// declared (so legacy configs keep working).
	workspaceID string

	// projectID is the soft sub-boundary slug. Defaults to the repo
	// prefix in single-project repos. Monorepos resolve a per-file
	// projectID via the `projects[]` paths-glob mapping in
	// `.gortex.yaml`; until that lookup is wired in, every node from
	// this indexer carries the repo-default value.
	projectID string

	// contractRegistry holds detected API contracts (HTTP routes, gRPC, etc.).
	contractRegistry *contracts.Registry

	// trackedRepoModules maps repo names to Go module paths for cross-repo dependency detection.
	// Populated by MultiIndexer from go.mod files of tracked repos.
	trackedRepoModules map[string]string

	// embedder is the optional embedding provider for semantic search.
	embedder embedding.Provider

	// semanticMgr is the optional semantic enrichment manager.
	semanticMgr *semantic.Manager

	// Mtime tracking and parse error retention for index health diagnostics.
	parseErrors   []IndexError
	fileMtimes    map[string]int64
	lastIndexTime time.Time
	totalDetected int
	mtimeMu       sync.RWMutex

	// contractCache memoizes the contract-extractor output per file.
	// Keyed by graph file path (with repo prefix); value is the file's
	// disk mtime when last extracted plus the contracts that came out.
	// extractContracts replays cache hits to skip the read + 8-extractor
	// run for files that haven't changed since the last extraction —
	// the dominant cost on repos with tens of thousands of source files.
	contractCache   map[string]*contractCacheEntry
	contractCacheMu sync.RWMutex

	// upgradeOnce gates the BM25→Bleve auto-upgrade to exactly one
	// goroutine per indexer lifetime. Without this, every post-threshold
	// IndexCtx — which fires once per tracked repo during multi-repo
	// warmup — would spawn a fresh upgradeSearchToBleve goroutine.
	// Each rebuilds ~N-doc Bleve indexes (≈32 KiB/doc), so overlapping
	// upgrades peak memory far above steady-state and waste CPU on
	// rebuilds that the next Swap immediately discards. Also counts
	// scheduled upgrades so tests can observe gating decisions
	// without relying on log scrapes or timing.
	upgradeOnce      sync.Once
	upgradeSpawnedMu sync.Mutex
	upgradeSpawned   int

	// deferResolve, when set, makes IndexCtx skip the cross-cutting passes
	// (per-repo ResolveAll / InferImplements / semantic enrichment / contract
	// extraction + commit) so the multi-repo orchestrator can run them
	// serially after the parallel fan-out joins. Without this, two
	// goroutines indexing different repos into the shared graph race on
	// Edge.Meta during the resolver's mutation phase vs. the contract
	// pass's graph walk via AllEdges().
	deferResolve       bool
	pendingContractReg *contracts.Registry

	// codeownersOnce ensures the repo-level CODEOWNERS file is parsed
	// exactly once per indexer lifetime. The rule list is derived
	// from .github/CODEOWNERS / CODEOWNERS / docs/CODEOWNERS at
	// first use and applied per-file by applyCoverageDomains; an
	// absent file produces empty rules and a no-op pass.
	codeownersOnce  sync.Once
	codeownersRules []codeowners.Rule
}

// contractCacheEntry is a cached contract-extraction result for one file.
type contractCacheEntry struct {
	mtimeNano int64
	contracts []contracts.Contract
}

// New creates an Indexer.
func New(g *graph.Graph, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
	return &Indexer{
		graph:    g,
		registry: reg,
		resolver: resolver.New(g),
		// Wrap in Swappable so the auto-upgrade to Bleve at large
		// corpus sizes can happen in a background goroutine without
		// racing with concurrent searches. Subsequent reassignments to
		// idx.search (Hybrid wrap, etc.) should use swap helpers below.
		search:        search.NewSwappable(search.NewAuto()),
		config:        cfg,
		logger:        logger,
		fileMtimes:    make(map[string]int64),
		contractCache: make(map[string]*contractCacheEntry),
	}
}

// swappable returns the search backend cast to *search.Swappable. Panics
// if the invariant (idx.search is always a Swappable) is ever broken —
// that would be a programmer error in this file, not a runtime condition.
func (idx *Indexer) swappable() *search.Swappable {
	if sw, ok := idx.search.(*search.Swappable); ok {
		return sw
	}
	panic("indexer: search backend is not *search.Swappable — invariant violated")
}

// shouldIndexForSearch reports whether a node should be added to the
// text search index (BM25/Bleve). File and Import nodes are never
// searchable symbols. Beyond that, config.SkipSearch filters out
// (language, kind) pairs that would only add noise — JSON/YAML/TOML
// keys, CSS tokens, Terraform blocks, shell/build variables. All three
// text-index call sites (buildSearchIndex bulk loop, indexFile
// incremental add, upgradeSearchToBleve repopulate) must go through
// this predicate so they can't drift.
func (idx *Indexer) shouldIndexForSearch(n *graph.Node) bool {
	if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
		return false
	}
	if config.ShouldSkipSearch(idx.config.SkipSearch, n.Language, string(n.Kind)) {
		return false
	}
	return true
}

// upgradeSearchToBleve constructs a Bleve backend from the current graph
// and atomically swaps it in. Designed to run in a background goroutine
// triggered by IndexCtx after the initial index completes. Does nothing
// if Bleve construction fails (caller already hit AutoThreshold but the
// in-memory backend keeps serving correctly, just with worse memory
// characteristics).
func (idx *Indexer) upgradeSearchToBleve() {
	// Defensive early-return: if the active text backend is already
	// Bleve, there is nothing to upgrade. IndexCtx's sync.Once guard
	// prevents re-entry from the auto-upgrade path, but direct
	// callers (tests, manual invocation from tooling) could still
	// hit this function twice; a second run would pointlessly
	// rebuild a full Bleve index and Swap it over an identical one.
	inner := idx.swappable().Inner()
	if _, ok := inner.(*search.BleveBackend); ok {
		return
	}
	if hyb, ok := inner.(*search.HybridBackend); ok {
		if _, ok := hyb.TextBackend().(*search.BleveBackend); ok {
			return
		}
	}

	// Opt-in disk backend. Scorch stores the inverted index on disk
	// (~10-20× less heap than upsidedown+gtreap) at the cost of file
	// I/O during build. Users point GORTEX_BLEVE_DISK_DIR at a
	// writable path; we manage the file lifecycle inside it.
	diskDir := os.Getenv("GORTEX_BLEVE_DISK_DIR")

	var (
		blv *search.BleveBackend
		err error
	)
	if diskDir != "" {
		blv, err = search.NewBleveDisk(diskDir)
		if err != nil {
			idx.logger.Warn("search: bleve disk construction failed, falling back to in-memory",
				zap.String("dir", diskDir), zap.Error(err))
		}
	}
	if blv == nil {
		blv, err = search.NewBleve()
		if err != nil {
			idx.logger.Warn("search: bleve construction failed, staying on in-memory",
				zap.Error(err))
			return
		}
	}

	for _, n := range idx.graph.AllNodes() {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		blv.Add(n.ID, n.Name, n.FilePath, sig)
	}

	// Preserve the vector index if one is wired up. The previous inner
	// is normally a HybridBackend wrapping text + vector + embedder;
	// swapping in raw Bleve would let Swap's old.Close() run on the
	// old Hybrid, which closes only its text side (hybrid.Close) but
	// leaves the resulting inner — raw *BleveBackend — unwrapped, so
	// every downstream hybrid/semantic query silently degrades to
	// BM25 until the daemon restarts. Rewrap the fresh Bleve in a new
	// Hybrid carrying the old vector + embedder. The vector backend
	// itself is never closed by Hybrid.Close, so it stays alive even
	// after the old Hybrid is torn down by Swap.
	sw := idx.swappable()
	var replacement search.Backend = blv
	if oldHybrid, ok := sw.Inner().(*search.HybridBackend); ok {
		replacement = search.NewHybrid(blv, oldHybrid.VectorIndex(), oldHybrid.Embedder())
	}
	sw.Swap(replacement)

	mode := "memory"
	if blv.DiskPath() != "" {
		mode = "disk"
	}
	idx.logger.Info("search: upgraded to Bleve backend (background)",
		zap.Int("symbols", blv.Count()),
		zap.String("mode", mode),
		zap.String("disk_path", blv.DiskPath()))
}

// Graph returns the underlying graph.
func (idx *Indexer) Graph() *graph.Graph { return idx.graph }

// Search returns the search backend.
func (idx *Indexer) Search() search.Backend { return idx.search }

// ContractRegistry returns the contract registry populated during indexing.
func (idx *Indexer) ContractRegistry() *contracts.Registry { return idx.contractRegistry }

// SetContractRegistry installs reg as the indexer's contract registry.
// Used by the daemon warmup path to rehydrate the registry from a
// snapshot when IncrementalReindex skipped extraction (no stale files
// → extractContracts never ran → idx.contractRegistry stays nil after
// reconcile, which used to leave multi-repo `contracts` queries silently
// empty). Callers should only install when ContractRegistry() is nil;
// installing over a freshly-extracted registry would roll state backward.
func (idx *Indexer) SetContractRegistry(reg *contracts.Registry) {
	idx.contractRegistry = reg
}

// SetTrackedRepoModules sets the map of tracked repo names to Go module paths.
// This enables the GoModExtractor to detect cross-repo dependencies.
func (idx *Indexer) SetTrackedRepoModules(m map[string]string) { idx.trackedRepoModules = m }

// SetDeferResolve toggles whether IndexCtx defers the cross-cutting passes
// to a later RunDeferredPasses call. See the deferResolve field comment.
func (idx *Indexer) SetDeferResolve(v bool) { idx.deferResolve = v }

// RunDeferredPasses runs the cross-cutting passes that IndexCtx skipped in
// deferred mode: per-repo ResolveAll, InferImplements, semantic enrichment,
// and contract extraction + commit. Safe to call only after IndexCtx has
// populated the graph for this repo. Idempotent — second calls are a no-op
// because the pending registry is cleared at the end.
func (idx *Indexer) RunDeferredPasses(ctx context.Context) {
	if idx.pendingContractReg == nil {
		return
	}
	reporter := progress.FromContext(ctx)

	reporter.Report("resolving references", 0, 0)
	idx.resolver.ResolveAll()

	reporter.Report("inferring interfaces", 0, 0)
	idx.resolver.InferImplements()

	if idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders() {
		reporter.Report("semantic enrichment", 0, 0)
		roots := map[string]string{"default": idx.rootPath}
		results, err := idx.semanticMgr.EnrichAll(idx.graph, roots)
		if err != nil {
			idx.logger.Warn("semantic enrichment failed", zap.Error(err))
		} else if len(results) > 0 {
			for _, r := range results {
				idx.logger.Info("semantic enrichment result",
					zap.String("provider", r.Provider),
					zap.String("language", r.Language),
					zap.Int("confirmed", r.EdgesConfirmed),
					zap.Int("added", r.EdgesAdded),
					zap.Int("refuted", r.EdgesRefuted),
					zap.Float64("coverage", r.CoveragePercent),
				)
			}
		}
	}

	reporter.Report("extracting contracts", 0, 0)
	idx.extractGoModContracts(idx.pendingContractReg)
	idx.extractExternalModules()
	idx.extractDIContracts(idx.pendingContractReg)
	idx.commitContracts(idx.pendingContractReg)
	idx.pendingContractReg = nil

	// Test-edge pass: mark test functions and emit EdgeTests parallel
	// to EdgeCalls so get_test_targets / find_usages-with-exclude-tests
	// can answer in one hop.
	reporter.Report("test edge pass", 0, 0)
	marked, emitted := markTestSymbolsAndEmitEdges(idx.graph)
	if marked > 0 || emitted > 0 {
		idx.logger.Info("test edges emitted",
			zap.Int("test_symbols", marked),
			zap.Int("edges", emitted),
		)
	}
}

// RootPath returns the root path used for relative path computation.
func (idx *Indexer) RootPath() string { return idx.rootPath }

// SetRepoPrefix sets the repository prefix for multi-repo mode.
// When non-empty, all node IDs and file paths are prefixed with "<repoPrefix>/".
func (idx *Indexer) SetRepoPrefix(prefix string) { idx.repoPrefix = prefix }

// RepoPrefix returns the current repository prefix.
func (idx *Indexer) RepoPrefix() string { return idx.repoPrefix }

// SetWorkspaceID sets the workspace slug stamped onto nodes emitted
// by this indexer. Empty means "no workspace declared" — the
// applyRepoPrefix path will fall back to RepoPrefix so multi-repo
// configs without `.gortex.yaml::workspace:` keep working.
func (idx *Indexer) SetWorkspaceID(id string) { idx.workspaceID = id }

// WorkspaceID returns the workspace slug this indexer stamps on nodes.
func (idx *Indexer) WorkspaceID() string { return idx.workspaceID }

// SetProjectID sets the project slug stamped onto nodes emitted by
// this indexer. Single-project repos pass their repo name (the
// MultiIndexer default); monorepos compute a per-file slug from the
// `projects[]` mapping (follow-up work).
func (idx *Indexer) SetProjectID(id string) { idx.projectID = id }

// ProjectID returns the project slug this indexer stamps on nodes.
func (idx *Indexer) ProjectID() string { return idx.projectID }

// SetEmbedder sets the embedding provider for semantic search.
// When set, buildSearchIndex will create a HybridBackend with vector search.
func (idx *Indexer) SetEmbedder(p embedding.Provider) { idx.embedder = p }

// SetSemanticManager sets the semantic enrichment manager.
// When set, the indexer runs semantic enrichment after resolution.
func (idx *Indexer) SetSemanticManager(m *semantic.Manager) { idx.semanticMgr = m }

// SemanticManager returns the semantic enrichment manager.
func (idx *Indexer) SemanticManager() *semantic.Manager { return idx.semanticMgr }

// ExportVectorIndex returns the serialized vector index bytes, dims, and count.
// Returns nil, 0, 0 if no vector index is active.
func (idx *Indexer) ExportVectorIndex() ([]byte, int, int) {
	hybrid, ok := idx.swappable().Inner().(*search.HybridBackend)
	if !ok {
		return nil, 0, 0
	}
	vec := hybrid.VectorIndex()
	if vec == nil || vec.Count() == 0 {
		return nil, 0, 0
	}

	var buf bytes.Buffer
	if err := vec.Save(&buf); err != nil {
		idx.logger.Warn("failed to export vector index", zap.Error(err))
		return nil, 0, 0
	}
	return buf.Bytes(), vec.Dims(), vec.Count()
}

// ImportVectorIndex restores a vector index from serialized data and wraps
// the current text search backend into a HybridBackend.
func (idx *Indexer) ImportVectorIndex(data []byte, dims, count int) error {
	if len(data) == 0 || idx.embedder == nil {
		return nil
	}

	// Validate dimensions match the current embedder to avoid mismatches
	// when switching providers (e.g., GloVe 50d → ONNX 384d).
	embedderDims := idx.embedder.Dimensions()
	if embedderDims > 0 && embedderDims != dims {
		idx.logger.Info("vector index dims mismatch, will re-embed",
			zap.Int("cached_dims", dims), zap.Int("embedder_dims", embedderDims))
		return nil // skip import, buildSearchIndex will re-embed
	}

	vec := search.NewVector(dims)
	if err := vec.LoadFrom(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("import vector index: %w", err)
	}
	vec.SetCount(count)

	sw := idx.swappable()
	sw.Swap(search.NewHybrid(sw.Inner(), vec, idx.embedder))
	idx.logger.Info("restored vector index from cache",
		zap.Int("vectors", count), zap.Int("dims", dims))
	return nil
}

// prefixPath prepends the repoPrefix to a relative path when in multi-repo mode.
// Returns the path unchanged when repoPrefix is empty.
func (idx *Indexer) prefixPath(relPath string) string {
	if idx.repoPrefix == "" {
		return relPath
	}
	return idx.repoPrefix + "/" + relPath
}

// applyRepoPrefix transforms nodes and edges produced by an extractor to include
// the repo prefix in IDs and file paths. Sets Node.RepoPrefix on all nodes.
// This is a no-op when repoPrefix is empty (single-repo mode).
//
// Edge targets beginning with "unresolved::" are a sentinel meaning "the
// resolver will replace this with a real node ID after all files are
// indexed." Prefixing them turns "unresolved::fetchUsers" into
// "web/unresolved::fetchUsers", which the resolver's HasPrefix check on
// "unresolved::" misses — leaving every call edge permanently unresolved
// in multi-repo mode and breaking get_callers / get_call_chain across all
// languages. Skip prefixing on unresolved targets; the resolver will land
// the edge on a real ID that already carries its own correct prefix
// (possibly cross-repo, which the resolver marks explicitly).

// todoTags returns the configured TODO marker set or the default
// (TODO/FIXME/HACK/XXX/NOTE). Reads from the IndexConfig the indexer
// already holds — IsCoverageEnabled gating happens at the call site.
func (idx *Indexer) todoTags() []string {
	if tags := idx.config.Coverage.Todos.Tags; len(tags) > 0 {
		return tags
	}
	return []string{"TODO", "FIXME", "HACK", "XXX", "NOTE"}
}

// todoMaxText returns the configured cap on stored TODO text or the
// 200-char default.
func (idx *Indexer) todoMaxText() int {
	if n := idx.config.Coverage.Todos.MaxText; n > 0 {
		return n
	}
	return 200
}

// loadCodeownersRules lazily parses the repo's CODEOWNERS file. The
// sync.Once guarantees one parse per indexer; applyCoverageDomains
// is then a pure rule-match per file. Errors silently produce an
// empty rule set — the ownership domain is implicitly gated on
// file presence rather than failing extraction when the file is
// missing or malformed.
func (idx *Indexer) loadCodeownersRules() []codeowners.Rule {
	idx.codeownersOnce.Do(func() {
		rules, _, ok := codeowners.LoadFromRepo(idx.rootPath)
		if !ok {
			return
		}
		idx.codeownersRules = rules
	})
	return idx.codeownersRules
}

// applyCoverageDomains runs the per-file coverage extractors
// (todos, licenses, ownership) and applies the post-extraction
// strip pass for domains the language extractor always emits but
// the user has gated off (function_shape). Appended/stripped
// nodes/edges flow through the same applyRepoPrefix / graph.AddNode
// pipeline as the language extractor's output. Called from both
// the bulk index worker pool (IndexCtx) and the incremental
// indexFile path.
//
// relPath is the unprefixed file path; lang is the detected
// language; src is the file bytes.
func (idx *Indexer) applyCoverageDomains(relPath, lang string, src []byte, result *parser.ExtractionResult) {
	if idx.config.Coverage.IsEnabled("todos") {
		findings := todos.Scan(src, idx.todoTags(), idx.todoMaxText())
		todoNodes, todoEdges := todos.BuildGraphArtifacts(relPath, findings, lang)
		result.Nodes = append(result.Nodes, todoNodes...)
		result.Edges = append(result.Edges, todoEdges...)
	}
	if idx.config.Coverage.IsEnabled("licenses") {
		if spdx := licenses.Scan(src); spdx != "" {
			licNodes, licEdges := licenses.BuildGraphArtifacts(relPath, spdx, lang)
			result.Nodes = append(result.Nodes, licNodes...)
			result.Edges = append(result.Edges, licEdges...)
		}
	}
	if idx.config.Coverage.IsEnabled("ownership") {
		if rules := idx.loadCodeownersRules(); len(rules) > 0 {
			if owners := codeowners.MatchFile(relPath, rules); len(owners) > 0 {
				teamNodes, teamEdges := codeowners.BuildGraphArtifacts(relPath, owners, lang)
				result.Nodes = append(result.Nodes, teamNodes...)
				result.Edges = append(result.Edges, teamEdges...)
			}
		}
	}
	if idx.config.Coverage.IsEnabled("codegen") {
		if marker := codegen.Scan(src); marker.Generated {
			// Stamp the marker on the file node when the language
			// extractor produced one. Generated files without a
			// file-shaped result node still get the EdgeGeneratedBy
			// edge so downstream walks pick them up.
			for _, n := range result.Nodes {
				if n.Kind == graph.KindFile && n.FilePath == relPath {
					if n.Meta == nil {
						n.Meta = map[string]any{}
					}
					codegen.MarkFileNode(n.Meta, marker)
					break
				}
			}
			result.Edges = append(result.Edges, codegen.BuildGraphArtifacts(relPath, marker)...)
		}
	}
	if !idx.config.Coverage.IsEnabled("function_shape") {
		stripFunctionShape(result)
	}
	if !idx.config.Coverage.IsEnabled("type_shape") {
		stripTypeShape(result)
	}
	if !idx.config.Coverage.IsEnabled("constants") {
		revertConstantsToVariables(result)
	}
	if !idx.config.Coverage.IsEnabled("concurrency") {
		stripConcurrencyEdges(result)
	}
	if idx.config.Coverage.IsEnabled("fixtures") {
		applyFixtureClassification(relPath, lang, result)
	}
	if !idx.config.Coverage.IsEnabled("observability") {
		stripObservabilityArtifacts(result)
	}
	if !idx.config.Coverage.IsEnabled("flags") {
		stripFlagArtifacts(result)
	}
	if !idx.config.Coverage.IsEnabled("configs") {
		stripConfigArtifacts(result)
	}
}

// stripConfigArtifacts drops KindConfigKey nodes plus
// EdgeReadsConfig / EdgeWritesConfig edges when the configs
// coverage domain is gated off. Endpoint-aware so any leftover
// edges to stripped key nodes are pruned.
func stripConfigArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if n.Kind == graph.KindConfigKey {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeReadsConfig || e.Kind == graph.EdgeWritesConfig {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripFlagArtifacts drops KindFlag nodes and EdgeTogglesFlag
// edges when the flags coverage domain is gated off. Mirrors the
// observability strip — endpoint-aware so any leftover edges that
// pointed to a removed flag node are also dropped.
func stripFlagArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFlag {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeTogglesFlag {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripObservabilityArtifacts drops KindEvent nodes and EdgeEmits
// edges when the observability coverage domain is gated off. Used
// for the same reason as the function-shape and type-shape strips:
// the language extractor always emits, and the indexer prunes
// per-file before applyRepoPrefix so the gate stays a pure-config
// dial without parser plumbing.
func stripObservabilityArtifacts(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if n.Kind == graph.KindEvent {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeEmits {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// applyFixtureClassification reclassifies the language extractor's
// emitted file node from KindFile to KindFixture when the file
// lives under a testdata/ directory. When the language extractor
// produced no file node (file types without a registered
// extractor), a standalone KindFixture node is emitted instead.
//
// Reference edges from test functions to fixtures are out of scope
// for v1 — agents can already filter by kind to enumerate fixtures.
func applyFixtureClassification(relPath, lang string, result *parser.ExtractionResult) {
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile && n.FilePath == relPath {
			if fixtures.ReclassifyFileToFixture(n) {
				return
			}
			break
		}
	}
	result.Nodes = append(result.Nodes, fixtures.BuildGraphArtifacts(relPath, lang)...)
}

// stripConcurrencyEdges removes the EdgeSpawns / EdgeSends /
// EdgeRecvs edges introduced by the concurrency coverage domain.
// EdgeCalls is left in place — spawns are emitted in addition to
// the corresponding call edge, so dropping just the spawn edge
// reverts to the pre-coverage call graph without losing reachability.
func stripConcurrencyEdges(result *parser.ExtractionResult) {
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		switch e.Kind {
		case graph.EdgeSpawns, graph.EdgeSends, graph.EdgeRecvs:
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// revertConstantsToVariables re-classifies KindConstant /
// KindEnumMember nodes back to KindVariable when the constants
// coverage domain is gated off. Unlike stripFunctionShape /
// stripTypeShape this is a re-classification, not a removal —
// users who disable the domain still want their `const` and `iota`
// declarations in the graph, just under the original kind that
// pre-coverage code expected.
func revertConstantsToVariables(result *parser.ExtractionResult) {
	for _, n := range result.Nodes {
		switch n.Kind {
		case graph.KindConstant, graph.KindEnumMember:
			n.Kind = graph.KindVariable
		}
	}
}

// stripTypeShape removes the alias / composition edges introduced
// by the type_shape coverage domain (EdgeAliases, EdgeComposes).
// EdgeExtends is left in place — it's an existing edge kind whose
// newtype-derived emissions fall under the spec's "EdgeExtends
// continues to mean newtype / inheritance / interface extension"
// guarantee, not a new domain signal.
func stripTypeShape(result *parser.ExtractionResult) {
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		switch e.Kind {
		case graph.EdgeAliases, graph.EdgeComposes:
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

// stripFunctionShape removes the param/closure/generic_param nodes
// and their associated edges from a per-file extraction result.
// Used when the function_shape coverage domain is gated off — the
// language extractor always emits these for resolution-internal
// reasons, and we drop them after the extractor returns rather than
// wire a config dependency through the parser layer.
func stripFunctionShape(result *parser.ExtractionResult) {
	stripped := make(map[string]struct{})
	keptNodes := result.Nodes[:0]
	for _, n := range result.Nodes {
		if isFunctionShapeNode(n.Kind) {
			stripped[n.ID] = struct{}{}
			continue
		}
		keptNodes = append(keptNodes, n)
	}
	result.Nodes = keptNodes
	keptEdges := result.Edges[:0]
	for _, e := range result.Edges {
		if isFunctionShapeEdge(e.Kind) {
			continue
		}
		if _, ok := stripped[e.From]; ok {
			continue
		}
		if _, ok := stripped[e.To]; ok {
			continue
		}
		keptEdges = append(keptEdges, e)
	}
	result.Edges = keptEdges
}

func isFunctionShapeNode(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindParam, graph.KindClosure, graph.KindGenericParam:
		return true
	}
	return false
}

func isFunctionShapeEdge(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeParamOf, graph.EdgeReturns, graph.EdgeTypedAs, graph.EdgeCaptures:
		return true
	}
	return false
}

func (idx *Indexer) applyRepoPrefix(nodes []*graph.Node, edges []*graph.Edge) {
	// Stamp WorkspaceID / ProjectID on every node emitted by this
	// indexer regardless of mode — single-repo and multi-repo both
	// need the boundary slugs for query scoping and contract
	// matching. Single-repo callers can leave them empty; the
	// MultiIndexer path always sets them via SetWorkspaceID /
	// SetProjectID before calling Index.
	if idx.workspaceID != "" || idx.projectID != "" {
		for _, n := range nodes {
			if idx.workspaceID != "" && n.WorkspaceID == "" {
				n.WorkspaceID = idx.workspaceID
			}
			if idx.projectID != "" && n.ProjectID == "" {
				n.ProjectID = idx.projectID
			}
		}
	}
	if idx.repoPrefix == "" {
		return
	}
	prefix := idx.repoPrefix + "/"
	const unresolvedMarker = "unresolved::"
	for _, n := range nodes {
		n.ID = prefix + n.ID
		n.FilePath = prefix + n.FilePath
		n.RepoPrefix = idx.repoPrefix
	}
	for _, e := range edges {
		e.From = prefix + e.From
		if !strings.HasPrefix(e.To, unresolvedMarker) {
			e.To = prefix + e.To
		}
		e.FilePath = prefix + e.FilePath
	}
}

// Index walks root and populates the graph using a concurrent worker pool.
//
// This is the backwards-compatible entry point; it delegates to IndexCtx with
// a background context. Callers wanting progress notifications or cancellation
// should use IndexCtx directly.
func (idx *Indexer) Index(root string) (*IndexResult, error) {
	return idx.IndexCtx(context.Background(), root)
}

// IndexCtx is Index with a context, enabling progress reporting. The reporter
// is pulled from ctx via progress.FromContext — attach one with
// progress.WithReporter to receive stage updates. If no reporter is attached,
// stage calls are silently dropped.
func (idx *Indexer) IndexCtx(ctx context.Context, root string) (*IndexResult, error) {
	start := time.Now()
	reporter := progress.FromContext(ctx)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.rootPath = absRoot

	reporter.Report("walking files", 0, 0)

	// Collect files. Files over IndexConfig.MaxFileSize are skipped
	// during the walk — they're nearly always generated/minified code
	// that dominates parse time without contributing useful signal.
	// A single summary warning reports how many were skipped so the
	// user knows when the cap is biting.
	maxSize := idx.config.MaxFileSize
	var files []string
	var skippedLarge int
	var skippedBytes int64
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldExclude(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.registry.DetectLanguage(path); !ok {
			return nil
		}
		if idx.shouldExclude(path, absRoot) {
			return nil
		}
		if maxSize > 0 {
			if info, statErr := d.Info(); statErr == nil && info.Size() > maxSize {
				skippedLarge++
				skippedBytes += info.Size()
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if skippedLarge > 0 {
		idx.logger.Info("indexer: skipped large files over MaxFileSize",
			zap.Int("count", skippedLarge),
			zap.Int64("total_bytes", skippedBytes),
			zap.Int64("limit_bytes", maxSize))
	}
	reporter.Report("walking files", len(files), len(files))

	// Worker pool.
	workers := idx.config.Workers
	if workers <= 0 {
		workers = 1
	}

	// Workers parse files, write the resulting nodes/edges to the
	// sharded graph, and run per-file contract extractors on the same
	// src bytes — all in one pass. Reusing src avoids the 10k+ disk
	// re-reads the old "parse then extractContracts" flow did; running
	// the contract extractors per-worker parallelises what used to be
	// a serial post-pass; language-filtered dispatch skips extractors
	// that can't match (HTTP on .css, OpenAPI on .ts, etc.).
	const parseReportEvery = 50
	totalFiles := len(files)

	_, contractExtractorsByLang := idx.buildPerFileContractExtractors()
	contractReg := contracts.NewRegistry()
	var contractMu sync.Mutex

	fileCh := make(chan string, workers*4)
	var errMu sync.Mutex
	var errors []IndexError
	var processed int64
	var fileCount int64

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var localContracts []contracts.Contract
			for path := range fileCh {
				p := atomic.AddInt64(&processed, 1)
				if p == 1 || p%parseReportEvery == 0 {
					reporter.Report("parsing", int(p), totalFiles)
				}

				src, err := os.ReadFile(path)
				if err != nil {
					errMu.Lock()
					errors = append(errors, IndexError{FilePath: path, Error: err.Error()})
					errMu.Unlock()
					continue
				}

				relPath, _ := filepath.Rel(absRoot, path)
				lang, _ := idx.registry.DetectLanguage(path)
				ext, _ := idx.registry.GetByLanguage(lang)
				if ext == nil {
					continue
				}

				result, err := ext.Extract(relPath, src)
				if err != nil {
					errMu.Lock()
					errors = append(errors, IndexError{FilePath: path, Error: err.Error()})
					errMu.Unlock()
					continue
				}

				// Append coverage artifacts (todos / licenses /
				// ownership) before applyRepoPrefix so they get the
				// same multi-repo namespacing treatment as
				// language-extractor output.
				idx.applyCoverageDomains(relPath, lang, src, result)

				idx.applyRepoPrefix(result.Nodes, result.Edges)

				// Find the file node (if the extractor produced one)
				// and collect its outgoing edges — contract extractors
				// take the file-scope edge set (imports, etc.), not
				// every intra-file edge.
				var fileNodeID, fileGraphPath string
				for _, n := range result.Nodes {
					if n.Kind == graph.KindFile {
						fileNodeID = n.ID
						fileGraphPath = n.FilePath
						break
					}
				}
				var fileScopeEdges []*graph.Edge
				if fileNodeID != "" {
					for _, e := range result.Edges {
						if e.From == fileNodeID {
							fileScopeEdges = append(fileScopeEdges, e)
						}
					}
				}

				for _, n := range result.Nodes {
					idx.graph.AddNode(n)
				}
				for _, e := range result.Edges {
					idx.graph.AddEdge(e)
				}

				if fileGraphPath != "" {
					exts := contractExtractorsByLang[lang]
					if len(exts) > 0 {
						c := idx.runContractExtractorsForFile(
							fileGraphPath, src, result.Nodes, fileScopeEdges, exts)
						localContracts = append(localContracts, c...)

						// Populate the per-file contract cache so a
						// later IncrementalReindex can skip this file
						// on a cache hit.
						if info, statErr := os.Stat(path); statErr == nil {
							idx.contractCacheMu.Lock()
							idx.contractCache[fileGraphPath] = &contractCacheEntry{
								mtimeNano: info.ModTime().UnixNano(),
								contracts: c,
							}
							idx.contractCacheMu.Unlock()
						}
					}
				}
				atomic.AddInt64(&fileCount, 1)
			}
			if len(localContracts) > 0 {
				contractMu.Lock()
				for _, c := range localContracts {
					contractReg.Add(c)
				}
				contractMu.Unlock()
			}
		}()
	}

	for _, f := range files {
		fileCh <- f
	}
	close(fileCh)
	wg.Wait()

	if processed > 0 {
		reporter.Report("parsing", int(processed), totalFiles)
	}

	// Populate fileMtimes for all detected files.
	idx.mtimeMu.Lock()
	idx.fileMtimes = make(map[string]int64, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			relPath, _ := filepath.Rel(absRoot, f)
			idx.fileMtimes[filepath.ToSlash(relPath)] = info.ModTime().UnixNano()
		}
	}
	idx.mtimeMu.Unlock()

	// Retain parse errors and record index metadata.
	idx.parseErrors = errors
	idx.totalDetected = len(files)
	idx.lastIndexTime = time.Now()

	if idx.deferResolve {
		// Multi-repo orchestrator runs these serially after wg.Wait()
		// to avoid races on the shared graph between this goroutine's
		// ResolveAll mutation phase and a sibling goroutine's contract
		// pass walking AllEdges. See SetDeferResolve.
		idx.pendingContractReg = contractReg
	} else {
		reporter.Report("resolving references", 0, 0)
		// Resolve cross-file references.
		idx.resolver.ResolveAll()

		reporter.Report("inferring interfaces", 0, 0)
		// Infer structural interface satisfaction.
		idx.resolver.InferImplements()

		// Semantic enrichment (SCIP, go/types, LSP).
		if idx.semanticMgr != nil && idx.semanticMgr.Enabled() && idx.semanticMgr.HasProviders() {
			reporter.Report("semantic enrichment", 0, 0)
			roots := map[string]string{"default": absRoot}
			results, err := idx.semanticMgr.EnrichAll(idx.graph, roots)
			if err != nil {
				idx.logger.Warn("semantic enrichment failed", zap.Error(err))
			} else if len(results) > 0 {
				for _, r := range results {
					idx.logger.Info("semantic enrichment result",
						zap.String("provider", r.Provider),
						zap.String("language", r.Language),
						zap.Int("confirmed", r.EdgesConfirmed),
						zap.Int("added", r.EdgesAdded),
						zap.Int("refuted", r.EdgesRefuted),
						zap.Float64("coverage", r.CoveragePercent),
					)
				}
			}
		}
	}

	reporter.Report("building search index", 0, 0)
	// Build search index.
	idx.buildSearchIndex()

	if !idx.deferResolve {
		// Contracts were already extracted inline during parse (per file,
		// per worker). Here we just finish up: run the go.mod extractor
		// (not associated with any file node) and commit contract nodes /
		// provides/consumes edges from the merged registry.
		reporter.Report("extracting contracts", 0, 0)
		idx.extractGoModContracts(contractReg)
		idx.extractExternalModules()
		idx.extractDIContracts(contractReg)
		idx.commitContracts(contractReg)

		// Test-edge pass mirrors the deferred-mode hook in
		// RunDeferredPasses — runs once the call graph is final.
		reporter.Report("test edge pass", 0, 0)
		marked, emitted := markTestSymbolsAndEmitEdges(idx.graph)
		if marked > 0 || emitted > 0 {
			idx.logger.Info("test edges emitted",
				zap.Int("test_symbols", marked),
				zap.Int("edges", emitted),
			)
		}
	}

	// Auto-upgrade to Bleve if above threshold. Run in the background
	// so the foreground IndexCtx returns immediately — populating
	// Bleve with 50k+ symbols takes 30-60s and adding that to the
	// initial-index latency was the dominant tail. Searches against
	// idx.search keep hitting the in-memory backend until the swap
	// completes; nothing observes a half-built Bleve.
	//
	// upgradeOnce gates the spawn so multi-repo warmup, which calls
	// IndexCtx once per tracked repo, doesn't launch one upgrade
	// goroutine per post-threshold repo. One per indexer lifetime.
	if idx.search.Count() >= search.AutoThreshold {
		idx.upgradeOnce.Do(func() {
			reporter.Report("scheduling search backend upgrade", 0, 0)
			idx.upgradeSpawnedMu.Lock()
			idx.upgradeSpawned++
			idx.upgradeSpawnedMu.Unlock()
			go idx.upgradeSearchToBleve()
		})
	}

	reporter.Report("indexing complete", int(fileCount), len(files))

	nodes, edges := idx.repoNodeEdgeCount()
	return &IndexResult{
		NodeCount:  nodes,
		EdgeCount:  edges,
		FileCount:  int(fileCount),
		DurationMs: time.Since(start).Milliseconds(),
		Errors:     errors,
	}, nil
}

// repoNodeEdgeCount returns this indexer's contribution to the graph,
// scoped to its repoPrefix in multi-repo mode. In single-repo mode
// (empty prefix) every node carries an empty RepoPrefix anyway, so the
// graph totals equal the repo's contribution and we use the cheap
// global accessors. The multi-repo path uses RepoMemoryEstimate which
// walks only this repo's byRepo bucket — O(repo size), not O(graph) —
// so callers that stamp RepoMetadata.NodeCount/EdgeCount no longer
// freeze the workspace-wide total at TrackRepo time.
func (idx *Indexer) repoNodeEdgeCount() (int, int) {
	if idx.repoPrefix == "" {
		return idx.graph.NodeCount(), idx.graph.EdgeCount()
	}
	est := idx.graph.RepoMemoryEstimate(idx.repoPrefix)
	return est.NodeCount, est.EdgeCount
}

// IndexFile parses a single file and patches the graph (evict then
// add), including per-file resolver work for cross-file references.
// Use in the single-event fsnotify path where each edit is isolated.
func (idx *Indexer) IndexFile(filePath string) error {
	return idx.indexFile(filePath, true)
}

// IndexFileNoResolve is IndexFile minus the per-file resolver call.
// Callers in batch paths (storm mode, branch-switch reconcile, git
// diff dispatch) use this when they will run resolver.ResolveAll()
// once at the end of the batch; otherwise a 500-file checkout pays
// the per-file resolver cost 500 times instead of once.
func (idx *Indexer) IndexFileNoResolve(filePath string) error {
	return idx.indexFile(filePath, false)
}

func (idx *Indexer) indexFile(filePath string, resolve bool) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	relPath, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		relPath = filePath
	}

	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)

	// Evict existing data for this file (graph + search).
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			idx.search.Remove(n.ID)
		}
	}
	idx.graph.EvictFile(graphPath)

	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	lang, ok := idx.registry.DetectLanguage(absPath)
	if !ok {
		return nil
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return nil
	}

	result, err := ext.Extract(relPath, src)
	if err != nil {
		return err
	}

	// Coverage extractors (todos, licenses, ownership). Same call
	// site exists in the bulk IndexCtx worker pool — see
	// applyCoverageDomains.
	idx.applyCoverageDomains(relPath, lang, src, result)

	idx.applyRepoPrefix(result.Nodes, result.Edges)

	for _, n := range result.Nodes {
		idx.graph.AddNode(n)
	}
	for _, e := range result.Edges {
		idx.graph.AddEdge(e)
	}

	// Add new symbols to search index. shouldIndexForSearch enforces
	// the same SkipSearch filter used by the bulk and upgrade paths.
	for _, n := range result.Nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		idx.search.Add(n.ID, n.Name, n.FilePath, sig)
	}

	if resolve {
		idx.resolver.ResolveFile(graphPath)
	}

	// Update mtime for this file (uses raw relPath for disk-based tracking).
	if info, err := os.Stat(absPath); err == nil {
		idx.mtimeMu.Lock()
		idx.fileMtimes[filepath.ToSlash(relPath)] = info.ModTime().UnixNano()
		idx.mtimeMu.Unlock()
	}

	return nil
}

// ResolveAll re-runs the global cross-file reference resolver and
// interface-implementation inference. Exposed for batch paths that
// defer per-file resolver work until the end of a batch.
func (idx *Indexer) ResolveAll() {
	idx.resolver.ResolveAll()
	idx.resolver.InferImplements()
}

// EvictFile removes all nodes and edges belonging to filePath.
func (idx *Indexer) EvictFile(filePath string) (int, int) {
	relPath, err := filepath.Rel(idx.rootPath, filePath)
	if err != nil {
		relPath = filePath
	}
	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)
	// Remove from search index.
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			idx.search.Remove(n.ID)
		}
	}
	return idx.graph.EvictFile(graphPath)
}

// buildSearchIndex populates the search backend from the current graph.
// When an embedder is set, also builds a vector index and wraps both
// in a HybridBackend with RRF fusion.
//
// In multi-repo mode the search backend is shared across every repo
// (Indexer.search is wired to MultiIndexer.search at construction).
// Walking g.AllNodes() and re-Add()ing every node would mean each
// freshly-tracked repo pays an O(workspace) re-index pass over all
// previously-tracked repos' nodes — quadratic in repo count and the
// dominant cost of warming up a 260-repo workspace. So when this
// indexer carries a non-empty repoPrefix we walk only that repo's
// byRepo bucket; the other repos' entries are already in the shared
// backend from when they were tracked. Single-repo mode keeps the
// AllNodes() path because nodes there carry an empty RepoPrefix and
// GetRepoNodes("") would miss them.
func (idx *Indexer) buildSearchIndex() {
	var nodes []*graph.Node
	if idx.repoPrefix != "" {
		nodes = idx.graph.GetRepoNodes(idx.repoPrefix)
	} else {
		nodes = idx.graph.AllNodes()
	}

	// Build text index. The SkipSearch filter (wired through
	// idx.shouldIndexForSearch) drops config-key-style variable nodes
	// that would only pad the index — see docs on IndexConfig.SkipSearch.
	for _, n := range nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		idx.search.Add(n.ID, n.Name, n.FilePath, sig)
	}

	// Build vector index if embedder is available.
	if idx.embedder == nil {
		return
	}

	dims := idx.embedder.Dimensions()
	if dims == 0 {
		dims = 300 // default for static provider
	}

	// Collect texts and IDs for batch embedding. Nodes matching
	// Semantic.SkipEmbed (e.g. CSS custom properties, terraform blocks,
	// YAML/TOML/shell config vars) are kept in the text index but
	// excluded from the vector index — embedding them is pure cost
	// with no semantic payoff and on big monorepos dominates RAM.
	var texts []string
	var ids []string
	var skipped int
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if config.ShouldSkipEmbed(idx.config.SkipEmbed, n.Language, string(n.Kind)) {
			skipped++
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		text := fmt.Sprintf("%s %s %s %s", n.Kind, n.Name, sig, n.FilePath)
		texts = append(texts, text)
		ids = append(ids, n.ID)
	}
	if skipped > 0 {
		idx.logger.Info("skipped embedding for low-value nodes",
			zap.Int("count", skipped),
			zap.Int("embedded", len(texts)))
	}

	if len(texts) == 0 {
		return
	}

	// Embedding scaling guards. Hard-cap the vector index for repos
	// big enough that the cost no longer pays off — BM25 alone is a
	// fine fallback and an OOM during initial index is much worse than
	// missing the semantic boost. Chunk the EmbedBatch calls so any
	// single API request stays small (matters for hosted embedders
	// with per-request token limits).
	const (
		embedMaxSymbols   = 100_000
		embedChunkSize    = 500
		embedChunkTimeout = 60 * time.Second
	)

	if len(texts) > embedMaxSymbols {
		idx.logger.Warn("vector index disabled — symbol count exceeds threshold",
			zap.Int("symbols", len(texts)),
			zap.Int("threshold", embedMaxSymbols),
			zap.String("hint", "BM25 text search remains active; raise threshold via Indexer config if you have memory headroom"))
		return
	}

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedChunkSize {
		end := start + embedChunkSize
		if end > len(texts) {
			end = len(texts)
		}
		chunkCtx, cancel := context.WithTimeout(context.Background(), embedChunkTimeout)
		chunkVecs, err := idx.embedder.EmbedBatch(chunkCtx, texts[start:end])
		cancel()
		if err != nil {
			// A partial vector index would mis-score later queries
			// (some symbols semantically findable, others not) — bail
			// to text-only search rather than ship an inconsistent
			// hybrid backend.
			idx.logger.Warn("vector index aborted on chunk failure",
				zap.Int("offset", start),
				zap.Int("chunk_size", end-start),
				zap.Error(err))
			return
		}
		vectors = append(vectors, chunkVecs...)
	}

	// Detect actual dimensions from first vector.
	if len(vectors) > 0 && len(vectors[0]) > 0 {
		dims = len(vectors[0])
	}

	vecBackend := search.NewVector(dims)
	for i, vec := range vectors {
		if vec != nil {
			vecBackend.Add(ids[i], vec)
		}
	}

	// Wrap text + vector into hybrid backend, swapping it in atomically
	// so any concurrent searches keep seeing a coherent backend.
	//
	// Unwrap any existing HybridBackend to its text side before
	// re-wrapping. Without this, buildSearchIndex called again (e.g.
	// once per tracked repo during daemon warmup) would stack a fresh
	// Hybrid on top of the previous one — nested Hybrids retain all
	// their stale vector indexes, ballooning live memory by an order
	// of magnitude. The text backend (BM25 or Bleve) has already been
	// updated with every node via idx.search.Add above; a single
	// Hybrid wrapping it + the latest vecBackend is all we need.
	sw := idx.swappable()
	inner := sw.Inner()
	if hyb, ok := inner.(*search.HybridBackend); ok {
		inner = hyb.TextBackend()
	}
	sw.Swap(search.NewHybrid(inner, vecBackend, idx.embedder))
	idx.logger.Info("vector index built",
		zap.Int("symbols", vecBackend.Count()),
		zap.Int("dimensions", dims))
}

// shouldExclude reports whether a path is excluded by the effective
// ignore list. The matcher is built lazily from idx.config.Exclude,
// which is populated by ConfigManager.GetRepoConfig with the full
// layered list (builtin + global + RepoEntry + workspace).
func (idx *Indexer) shouldExclude(path, root string) bool {
	m := idx.excludeMatcher()
	if m == nil {
		return false
	}
	return m.MatchAbs(path, root)
}

func (idx *Indexer) excludeMatcher() *excludes.Matcher {
	idx.excludeOnce.Do(func() {
		patterns := idx.config.Exclude
		// A nil/empty list from upstream means "no layering was applied"
		// (e.g. a direct caller of indexer.New without ConfigManager).
		// Fall back to the builtin baseline so the walk still skips the
		// obvious non-source dirs.
		if len(patterns) == 0 {
			patterns = excludes.Builtin
		}
		idx.excludes = excludes.New(patterns)
	})
	return idx.excludes
}

// ParseErrors returns the parse errors from the last full index.
func (idx *Indexer) ParseErrors() []IndexError {
	return idx.parseErrors
}

// FileMtimes returns a copy of the file modification time map.
func (idx *Indexer) FileMtimes() map[string]int64 {
	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	out := make(map[string]int64, len(idx.fileMtimes))
	for k, v := range idx.fileMtimes {
		out[k] = v
	}
	return out
}

// SetFileMtimes restores the file modification time map from a persisted snapshot.
func (idx *Indexer) SetFileMtimes(mtimes map[string]int64) {
	idx.mtimeMu.Lock()
	defer idx.mtimeMu.Unlock()
	idx.fileMtimes = make(map[string]int64, len(mtimes))
	for k, v := range mtimes {
		idx.fileMtimes[k] = v
	}
}

// SetRootPath sets the root path for relative path computation.
func (idx *Indexer) SetRootPath(root string) {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	idx.rootPath = abs
}

// IncrementalReindex walks the file tree and re-indexes only files that changed
// since the last snapshot. It also evicts nodes for deleted files.
func (idx *Indexer) IncrementalReindex(root string) (*IndexResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.rootPath = absRoot

	// Collect files currently on disk.
	diskFiles := make(map[string]bool)
	var staleFiles []string

	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldExclude(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.registry.DetectLanguage(path); !ok {
			return nil
		}
		if idx.shouldExclude(path, absRoot) {
			return nil
		}

		relPath, _ := filepath.Rel(absRoot, path)
		relPath = filepath.ToSlash(relPath)
		diskFiles[relPath] = true

		if idx.IsStale(relPath) {
			staleFiles = append(staleFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Detect deleted files: in fileMtimes but not on disk.
	idx.mtimeMu.RLock()
	var deletedFiles []string
	for relPath := range idx.fileMtimes {
		if !diskFiles[relPath] {
			deletedFiles = append(deletedFiles, relPath)
		}
	}
	idx.mtimeMu.RUnlock()

	// Evict deleted files.
	for _, relPath := range deletedFiles {
		graphPath := idx.prefixPath(relPath)
		idx.graph.EvictFile(graphPath)
		idx.mtimeMu.Lock()
		delete(idx.fileMtimes, relPath)
		idx.mtimeMu.Unlock()
	}

	// Re-index stale files.
	for _, f := range staleFiles {
		if err := idx.IndexFile(f); err != nil {
			idx.logger.Debug("incremental reindex: failed to index file",
				zap.String("file", f), zap.Error(err))
		}
	}

	// Re-infer interface implementations (edges may have been lost during eviction).
	idx.resolver.InferImplements()

	// Rebuild search index to ensure consistency.
	idx.buildSearchIndex()

	// Update totalDetected so index_health reports correctly after cache restore.
	if idx.totalDetected == 0 {
		idx.totalDetected = len(diskFiles)
	}

	// Re-extract contracts only if stale files were re-indexed.
	if len(staleFiles) > 0 || len(deletedFiles) > 0 {
		idx.extractContracts()
	}

	nodes, edges := idx.repoNodeEdgeCount()
	return &IndexResult{
		NodeCount:  nodes,
		EdgeCount:  edges,
		FileCount:  len(staleFiles),
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// LastIndexTime returns the timestamp of the last full index.
func (idx *Indexer) LastIndexTime() time.Time {
	return idx.lastIndexTime
}

// TotalDetected returns the total number of files detected during the last full index.
func (idx *Indexer) TotalDetected() int {
	return idx.totalDetected
}

// buildPerFileContractExtractors returns the set of extractors that
// operate on a single source file (everything except GoModExtractor,
// which runs once against go.mod at the repo root) plus a language →
// [extractors] map so callers can skip extractors whose
// SupportedLanguages() doesn't include a given file's language.
// Building the language map once avoids doing the string-membership
// check per file.
func (idx *Indexer) buildPerFileContractExtractors() ([]contracts.Extractor, map[string][]contracts.Extractor) {
	extractors := []contracts.Extractor{
		&contracts.HTTPExtractor{},
		&contracts.GRPCExtractor{},
		&contracts.GraphQLExtractor{},
		&contracts.OpenAPIExtractor{},
		&contracts.TopicExtractor{},
		&contracts.WebSocketExtractor{},
		&contracts.EnvVarExtractor{},
	}
	byLang := make(map[string][]contracts.Extractor)
	for _, ex := range extractors {
		for _, lang := range ex.SupportedLanguages() {
			byLang[lang] = append(byLang[lang], ex)
		}
	}
	return extractors, byLang
}

// runContractExtractorsForFile applies the given extractors to a single
// file and returns the raw contracts (with RepoPrefix already set).
// Called both inline from parse workers and from the full-walk
// extractContracts path — they share the same per-file work.
func (idx *Indexer) runContractExtractorsForFile(
	graphPath string,
	src []byte,
	fileNodes []*graph.Node,
	fileEdges []*graph.Edge,
	exts []contracts.Extractor,
) []contracts.Contract {
	if len(exts) == 0 {
		return nil
	}
	var out []contracts.Contract
	for _, ex := range exts {
		found := ex.Extract(graphPath, src, fileNodes, fileEdges)
		for i := range found {
			found[i].RepoPrefix = idx.repoPrefix
			// Stamp the workspace / project slugs alongside the repo
			// prefix so the matcher's boundary check has the data it
			// needs without a second registry walk. Empty slugs
			// default to RepoPrefix at Match time via
			// Contract.EffectiveWorkspace / EffectiveProject.
			if idx.workspaceID != "" {
				found[i].WorkspaceID = idx.workspaceID
			}
			if idx.projectID != "" {
				found[i].ProjectID = idx.projectID
			}
		}
		out = append(out, found...)
	}
	return out
}

// commitContracts writes contract nodes + provides/consumes edges for
// every contract in reg, and sets idx.contractRegistry to reg. Called
// once per index pass after all per-file contracts have been collected
// (inline from parse workers) plus go.mod has been processed.
func (idx *Indexer) commitContracts(reg *contracts.Registry) {
	// Upgrade bare type names in contract Meta (e.g. "UserResp") to
	// full symbol IDs (e.g. "pkg/resp.go::UserResp") now that the
	// graph is complete. During extraction the enricher only saw
	// the handler's file-scoped node list, so types declared in a
	// sibling file stayed as bare names.
	reg.UpgradeBareTypeRefs(func(name, repoHint string) []string {
		matches := idx.graph.FindNodesByName(name)
		var same, others []string
		for _, n := range matches {
			if n.Kind != graph.KindType {
				continue
			}
			if repoHint != "" && strings.HasPrefix(n.ID, repoHint+"/") {
				same = append(same, n.ID)
				continue
			}
			others = append(others, n.ID)
		}
		if len(same) > 0 {
			return same
		}
		return others
	})

	// Cross-file handler resolution. When a route is registered with
	// a handler identifier that the file-scoped extractor couldn't
	// resolve (`h.ServeArchive` in router.go wiring a method defined
	// in archive_handler.go), the contract's SymbolID fell back to
	// the enclosing router function and schema extraction ran
	// against the router's body — which has every route's bindings
	// piled on top of each other. Re-run enrichment with the
	// correct per-handler scope now that the graph is complete.
	idx.resolveProviderHandlers(reg)

	// Trace response variables back to their call-site return types.
	// Handles `source, err := h.svc.Get(...)` → response_type is
	// whatever `h.svc.Get` returns. The enricher can't do this
	// without graph access; this pass reads each method's signature
	// directly off the graph node, parses the first non-error
	// return type, and resolves it to a symbol ID.
	idx.resolveCallReturnTypes(reg)

	// Snapshot field-level shapes for every type that's referenced as
	// a contract's request / response body. This is Stage 2 — without
	// per-field data Stage 3 (validation, breaking-change detection)
	// has nothing to diff. We de-duplicate by symbol ID so heavy
	// fan-in types (a User DTO used by 40 routes) only get parsed
	// once per index pass.
	idx.snapshotContractShapes(reg)

	for _, c := range reg.All() {
		contractNode := &graph.Node{
			ID:       c.ID,
			Kind:     graph.KindContract,
			Name:     c.ID,
			FilePath: c.FilePath,
			Language: "contract",
			Meta:     map[string]any{"type": string(c.Type), "role": string(c.Role)},
		}
		idx.graph.AddNode(contractNode)

		edgeKind := graph.EdgeProvides
		if c.Role == contracts.RoleConsumer {
			edgeKind = graph.EdgeConsumes
		}
		if c.SymbolID != "" {
			idx.graph.AddEdge(&graph.Edge{
				From:     c.SymbolID,
				To:       c.ID,
				Kind:     edgeKind,
				FilePath: c.FilePath,
				Line:     c.Line,
			})
		}
	}

	idx.contractRegistry = reg
	repo := idx.rootPath
	if idx.repoPrefix != "" {
		repo = idx.repoPrefix
	}
	idx.logger.Info("contracts extracted",
		zap.String("repo", repo),
		zap.Int("count", len(reg.All())))
}

// resolveProviderHandlers finds the actual handler for every HTTP
// provider contract whose per-file extraction couldn't resolve the
// handler identifier (typically routers in one file wiring handlers
// defined in sibling files). For each such contract:
//
//   - Take Meta["handler_trail"] — the full expression between the
//     HandleFunc parens, which carries every handler candidate
//     (wrappers + inner handler). Fall back to "handler_ident"
//     when no trail was captured (older contracts, simple consumer
//     patterns).
//   - Enumerate candidates in source order and look each up in the
//     graph; take the innermost (last) one that resolves. That
//     picks h.ServeArchive out of WithAuth(h.ServeArchive) instead
//     of the WithAuth wrapper.
//   - Re-run EnrichHTTPContract against the handler's file with the
//     handler's line range so the enricher sees its actual body
//     instead of the router's.
//   - Drop `handler_ident` / `handler_trail` from meta afterwards —
//     they were internal resolution hints.
func (idx *Indexer) resolveProviderHandlers(reg *contracts.Registry) {
	type pending struct {
		contractID string
		trail      string
		fallback   string
		repoHint   string
	}
	var todo []pending
	for _, c := range reg.All() {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}
		trail, _ := c.Meta["handler_trail"].(string)
		fallback, _ := c.Meta["handler_ident"].(string)
		if trail == "" && fallback == "" {
			continue
		}
		// Skip contracts where schema is already populated — the
		// initial file-scoped pass worked.
		if src, _ := c.Meta["schema_source"].(string); src == "extracted" || src == "partial" {
			continue
		}
		todo = append(todo, pending{contractID: c.ID, trail: trail, fallback: fallback, repoHint: c.RepoPrefix})
	}
	if len(todo) == 0 {
		return
	}

	// Cache file source + node list per file path — a single router
	// often refers to dozens of handlers in the same sibling file.
	fileSrc := make(map[string][]byte)
	fileNodes := make(map[string][]*graph.Node)

	resolved := 0
	for _, p := range todo {
		handlerNode := idx.resolveInnermostHandler(p.trail, p.fallback, p.repoHint)
		if handlerNode == nil {
			continue
		}
		src, ok := fileSrc[handlerNode.FilePath]
		if !ok {
			diskPath := handlerNode.FilePath
			if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
				diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
			}
			diskPath = filepath.Join(idx.rootPath, diskPath)
			data, err := os.ReadFile(diskPath)
			if err != nil {
				fileSrc[handlerNode.FilePath] = nil
				continue
			}
			fileSrc[handlerNode.FilePath] = data
			src = data
		}
		if src == nil {
			continue
		}
		nodes, ok := fileNodes[handlerNode.FilePath]
		if !ok {
			nodes = idx.graph.GetFileNodes(handlerNode.FilePath)
			fileNodes[handlerNode.FilePath] = nodes
		}

		// Re-run enrichment. EnrichHTTPContract reads the contract's
		// SymbolID to locate the handler body range — swap it in
		// temporarily to the resolved handler so the lookup works.
		matches := reg.ByID(p.contractID)
		if len(matches) == 0 {
			continue
		}
		for i, c := range matches {
			if c.Role != contracts.RoleProvider {
				continue
			}
			// Operate on a copy; Registry entries are values.
			patched := c
			patched.SymbolID = handlerNode.ID
			patched.FilePath = handlerNode.FilePath
			if patched.Meta == nil {
				patched.Meta = map[string]any{}
			}
			// Drop prior path_params so the enricher's fresh pass
			// repopulates consistently (path hasn't changed, but we
			// want the call-path to be identical to Stage 1).
			lines := splitLines(src)
			contracts.EnrichHTTPContract(&patched, lines, nodes, detectLangFromPath(handlerNode.FilePath))
			delete(patched.Meta, "handler_ident")
			delete(patched.Meta, "handler_trail")
			matches[i] = patched
			resolved++
		}
		// Write back the mutated set. The registry doesn't have an
		// "update" API; we use AddAll semantics via Set-like
		// operations. Simpler: clear then re-add all roles to this ID.
		reg.ReplaceByID(p.contractID, matches)
	}
	if resolved > 0 {
		idx.logger.Info("resolved cross-file provider handlers",
			zap.Int("count", resolved),
			zap.Int("considered", len(todo)))
	}
}

// resolveInnermostHandler picks the innermost handler candidate from
// the call trail that resolves to a real function or method in the
// graph. Walks candidates in source order and keeps the LAST
// successful lookup — for `WithAuth(h.ServeArchive)` that's
// `h.ServeArchive`, not the `WithAuth` wrapper. Falls back to the
// single identifier when no trail is available (e.g. simple bare
// `r.GET("/x", listUsers)` patterns).
func (idx *Indexer) resolveInnermostHandler(trail, fallback, repoHint string) *graph.Node {
	candidates := contracts.HandlerCandidatesInTrail(trail)
	if len(candidates) == 0 && fallback != "" {
		candidates = []string{fallback}
	}
	var best *graph.Node
	for _, c := range candidates {
		if n := idx.lookupHandler(c, repoHint); n != nil {
			best = n
		}
	}
	return best
}

// lookupHandler maps a raw identifier from a route pattern to the
// graph node for the handler function / method.
//
//   - "h.ServeArchive" → method named "ServeArchive", prefer same repo.
//   - "ServeArchive"   → function or method of that name.
//   - "pkg.Foo"        → same as first form, package-qualified call.
//
// Returns nil when no candidate resolves unambiguously.
func (idx *Indexer) lookupHandler(ident, repoHint string) *graph.Node {
	// Strip a leading receiver / package qualifier — "h.ServeArchive"
	// → "ServeArchive".
	name := ident
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return nil
	}
	candidates := idx.graph.FindNodesByName(name)
	if len(candidates) == 0 {
		return nil
	}
	var sameRepo, other []*graph.Node
	for _, n := range candidates {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if repoHint != "" && strings.HasPrefix(n.ID, repoHint+"/") {
			sameRepo = append(sameRepo, n)
			continue
		}
		other = append(other, n)
	}
	if len(sameRepo) == 1 {
		return sameRepo[0]
	}
	if len(sameRepo) == 0 && len(other) == 1 {
		return other[0]
	}
	return nil // ambiguous
}

func splitLines(src []byte) []string {
	return strings.Split(string(src), "\n")
}

// detectLangFromPath mirrors internal/contracts.detectLanguage so the
// enricher's language-gate fires correctly for the handler's own file.
func detectLangFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return "go"
	case strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".tsx"):
		return "typescript"
	case strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".jsx"):
		return "javascript"
	case strings.HasSuffix(path, ".py"):
		return "python"
	case strings.HasSuffix(path, ".java"):
		return "java"
	case strings.HasSuffix(path, ".kt"), strings.HasSuffix(path, ".kts"):
		return "kotlin"
	case strings.HasSuffix(path, ".dart"):
		return "dart"
	}
	return ""
}

// responseHelperCallRe pulls the third argument out of a JSON-response
// helper call, e.g. `respondJSON(w, http.StatusOK, source)` → "source",
// `WriteJSON(w, 200, &result)` → "result". Matches every helper name
// the Go enricher knows about so the two pipes stay in sync.
var responseHelperCallRe = regexp.MustCompile(
	`(?:[A-Za-z_]\w*\.)?(?:[Rr]espond|[Ww]rite|[Ss]end|[Rr]ender)(?:JSON|Json)\(\s*\w+\s*,\s*[^,]+?\s*,\s*&?([A-Za-z_]\w*)\s*\)`,
)

// goCallBindRe matches a Go variable declaration whose right-hand
// side is a function / method call. Capture 1 is the variable name,
// capture 2 is the call expression (receiver + method or function
// name, without the argument list):
//
//	source, err := h.emailSources.Get(ctx, id)  → ("source", "h.emailSources.Get")
//	data       := buildThing()                   → ("data",   "buildThing")
//	resp       := client.List(ctx)               → ("resp",   "client.List")
var goCallBindRe = regexp.MustCompile(
	`(?m)^\s*(\w+)(?:\s*,\s*\w+)?\s*:?=\s*([A-Za-z_][\w.]*)\(`,
)

// receiverMatchesHint decides whether a method node could plausibly
// be the target of a call whose receiver chain includes `hint` as
// its penultimate segment. For `h.tucks.Update`, the hint is
// "tucks" and we accept receivers whose name (stripped of pointer
// marker) contains "tucks" case-insensitively:
//
//	*TucksStore.Update       ✓  (receiver "TucksStore" contains "tucks")
//	*PostgresTuckStore.Update ✓  (contains "tuck")
//	*EmailSources.Update     ✗  (no "tucks")
//
// The hint may itself be the receiver variable (`h` in `h.Update(...)`)
// when the call has only two segments; in that case any same-repo
// method named `Update` passes — but the upstream `len(matches) != 1`
// check still demands uniqueness, which is the real guard.
func receiverMatchesHint(n *graph.Node, hint string) bool {
	if hint == "" {
		return true
	}
	// Method ID looks like "<repo>/<file>::Receiver.Method". Extract
	// "Receiver" by splitting once on `::` then taking the type part
	// before the last `.`.
	idParts := strings.Split(n.ID, "::")
	if len(idParts) < 2 {
		return true // conservative: no receiver info available → don't filter out
	}
	last := idParts[len(idParts)-1]
	dot := strings.LastIndex(last, ".")
	if dot < 0 {
		return true // plain function, no receiver
	}
	recv := strings.TrimPrefix(last[:dot], "*")
	// Handle both singular and plural forms: "tucks" in hint matches
	// "TuckStore" by containing "tuck", and "tuck" hint matches
	// "Tucks" too. Strip a trailing `s` from the longer side to let
	// singular/plural pairs match.
	return strings.Contains(strings.ToLower(recv), strings.ToLower(hint)) ||
		strings.Contains(strings.ToLower(recv), strings.ToLower(strings.TrimSuffix(hint, "s"))) ||
		strings.Contains(strings.ToLower(recv), strings.ToLower(hint+"s"))
}

// parseFirstNonErrorReturnType walks a Go function signature and
// returns the first return type that isn't `error`. Signatures as
// stored in the graph have the form:
//
//	func ((s *Store)) Get(args) (*EmailSource, error)
//	func list() []*User
//	func save(x Foo) error
//
// Regex-based extraction struggles with the receiver's `((...))`
// nesting and with multi-paren return groups — we parse with an
// explicit bracket-depth counter so every shape above is handled
// the same way.

// resolveCallReturnTypes is the graph-aware companion to the
// regex-based schema enricher. For every HTTP provider contract whose
// response couldn't be pinned syntactically (response_expr is set,
// response_type is empty), we:
//
//   - Pull the bound variable's name out of the helper-call expression.
//   - Read the handler's body from disk.
//   - Find the variable's declaration line and parse the RHS call.
//   - Look up the called method by name in the graph (preferring the
//     same file / same repo).
//   - Parse the method's signature meta for the first non-error
//     return type, strip `*` / `[]`, resolve to a type node's ID.
//   - Patch the contract's meta in place.
//
// This is the proper tracing the name-based heuristic only
// approximated — it follows the variable to its definition instead
// of guessing from its name.
func (idx *Indexer) resolveCallReturnTypes(reg *contracts.Registry) {
	resolved := 0
	handlerBodies := make(map[string]string)

	for _, c := range reg.All() {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}
		if rt, _ := c.Meta["response_type"].(string); rt != "" {
			continue
		}
		respExpr, _ := c.Meta["response_expr"].(string)
		if respExpr == "" {
			continue
		}
		// Pull the bound variable name out of the helper call.
		m := responseHelperCallRe.FindStringSubmatch(respExpr)
		if len(m) < 2 {
			continue
		}
		varName := m[1]

		handler := idx.graph.GetNode(c.SymbolID)
		if handler == nil {
			continue
		}
		body, ok := handlerBodies[c.SymbolID]
		if !ok {
			body = idx.readHandlerSource(handler)
			handlerBodies[c.SymbolID] = body
		}
		if body == "" {
			continue
		}

		typeID := idx.traceVarTypeFromBody(body, varName, c.RepoPrefix)
		if typeID == "" {
			continue
		}
		// Patch in place.
		items := reg.ByID(c.ID)
		changed := false
		for i := range items {
			if items[i].Role != contracts.RoleProvider || items[i].SymbolID != c.SymbolID {
				continue
			}
			if items[i].Meta == nil {
				items[i].Meta = map[string]any{}
			}
			items[i].Meta["response_type"] = typeID
			items[i].Meta["schema_source"] = "extracted"
			delete(items[i].Meta, "response_expr")
			changed = true
		}
		if changed {
			reg.ReplaceByID(c.ID, items)
			resolved++
		}
	}
	if resolved > 0 {
		idx.logger.Info("resolved response types from call signatures",
			zap.Int("count", resolved))
	}
}

// readHandlerSource returns the handler function's source lines,
// trimmed to its start_line..end_line range. Empty string when the
// file can't be read or the node's span is missing.
func (idx *Indexer) readHandlerSource(handler *graph.Node) string {
	if handler.StartLine <= 0 {
		return ""
	}
	diskPath := handler.FilePath
	if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
		diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
	}
	diskPath = filepath.Join(idx.rootPath, diskPath)
	data, err := os.ReadFile(diskPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	end := handler.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	start := handler.StartLine
	if start > len(lines) {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}

// traceVarTypeFromBody walks the handler body for `varName`'s
// declaration, extracts the RHS call, looks up the called method in
// the graph, and returns the method's first non-error return type as
// a symbol ID. Empty string when any step fails.
//
// Ambiguity is treated as failure. Common method names like `Update`,
// `Get`, `List` exist on many stores in a real codebase — picking the
// first same-repo match would silently attribute the wrong type
// (e.g. `h.tucks.Update(...)` resolving to `EmailSources.Update`
// because that entry sorts first). The call's receiver chain gives us
// a disambiguation hint (`tucks` in `h.tucks.Update`) which we use to
// filter by receiver type name. When no single candidate survives,
// we return "" so the UI honestly shows that the type wasn't resolved
// rather than showing a wrong one.
func (idx *Indexer) traceVarTypeFromBody(body, varName, repoHint string) string {
	bindings := goCallBindRe.FindAllStringSubmatch(body, -1)
	var callExpr string
	for _, b := range bindings {
		if b[1] == varName {
			callExpr = b[2]
			break
		}
	}
	if callExpr == "" {
		return ""
	}
	// Split the call path. `h.tucks.Update` → ["h", "tucks", "Update"].
	// The last segment is the method name; the penultimate is the
	// receiver field / package, which we use to disambiguate when
	// multiple methods share the name.
	parts := strings.Split(callExpr, ".")
	methodName := parts[len(parts)-1]
	if methodName == "" {
		return ""
	}
	var receiverHint string
	if len(parts) >= 2 {
		receiverHint = parts[len(parts)-2]
	}

	candidates := idx.graph.FindNodesByName(methodName)
	var matches []*graph.Node
	for _, n := range candidates {
		if n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
			continue
		}
		if repoHint != "" && !strings.HasPrefix(n.ID, repoHint+"/") {
			continue
		}
		if receiverHint != "" && !receiverMatchesHint(n, receiverHint) {
			continue
		}
		matches = append(matches, n)
	}
	if len(matches) == 0 {
		return ""
	}

	// Interface + implementation stacks often produce multiple
	// receivers that share the same method signature — a production
	// postgres store and a mock test store both implement
	// `emailSources.Update(...) (*EmailSource, error)`. Parse every
	// candidate's signature; if they all agree on the first
	// non-error return type, use that. Otherwise bail so we don't
	// attribute a wrong type silently.
	var retType string
	for _, m := range matches {
		if m.Meta == nil {
			continue
		}
		sig, _ := m.Meta["signature"].(string)
		t := parseFirstNonErrorReturnType(sig)
		if t == "" {
			continue
		}
		// De-prioritise mock receivers: if a non-mock candidate
		// later disagrees we want that to be the authoritative one.
		if retType == "" {
			retType = t
			continue
		}
		if t != retType {
			// Candidates disagree — can't tell which wins. The
			// caller sees the raw expression and can drill in.
			return ""
		}
	}
	if retType == "" {
		return ""
	}
	// Strip `*` / `[]` / package qualifier so resolveTypeByName can
	// match the plain type-node name.
	retType = strings.TrimLeft(retType, "*[]")
	if dot := strings.LastIndex(retType, "."); dot >= 0 {
		retType = retType[dot+1:]
	}
	// Look up the type node, preferring same-repo matches.
	typeCandidates := idx.graph.FindNodesByName(retType)
	var bestType *graph.Node
	for _, n := range typeCandidates {
		if n.Kind != graph.KindType {
			continue
		}
		if repoHint != "" && strings.HasPrefix(n.ID, repoHint+"/") {
			bestType = n
			break
		}
		if bestType == nil {
			bestType = n
		}
	}
	if bestType != nil {
		return bestType.ID
	}
	// Bare name — downstream UpgradeBareTypeRefs can still upgrade
	// it later, but we return it as-is so the consumer sees something
	// real.
	return retType
}

func parseFirstNonErrorReturnType(sig string) string {
	sig = strings.TrimSpace(sig)
	if !strings.HasPrefix(sig, "func") {
		return ""
	}
	sig = strings.TrimSpace(strings.TrimPrefix(sig, "func"))

	// Optional receiver. Two forms to recognise:
	//   `((*Recv)) Name(params)`  — gortex's stored double-paren form
	//   `(r *Recv) Name(params)`  — standard Go source form
	// In both cases a function name follows the receiver parens.
	// Anonymous function types (`func(a, b) (c, d)`) have no
	// receiver — the first `(` opens the parameter list and is
	// followed by another `(` for the return group or end-of-string.
	// Disambiguate by peeking past the first balanced `(...)` group
	// for an identifier letter.
	if strings.HasPrefix(sig, "(") {
		end := findBalancedParenEnd(sig)
		if end < 0 {
			return ""
		}
		afterFirstGroup := strings.TrimSpace(sig[end+1:])
		if len(afterFirstGroup) > 0 {
			r := afterFirstGroup[0]
			isIdent := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_'
			if isIdent {
				sig = afterFirstGroup
			}
		}
	}

	// Skip the function name — everything up to the parameter list's
	// opening `(`.
	if i := strings.Index(sig, "("); i >= 0 {
		sig = sig[i:]
	} else {
		return ""
	}

	// Skip parameter list.
	end := findBalancedParenEnd(sig)
	if end < 0 {
		return ""
	}
	sig = strings.TrimSpace(sig[end+1:])
	if sig == "" {
		return ""
	}

	// Return clause — either `(T1, T2, ...)` or a bare single type.
	var inner string
	if strings.HasPrefix(sig, "(") {
		end := findBalancedParenEnd(sig)
		if end < 0 {
			return ""
		}
		inner = sig[1:end]
	} else {
		inner = sig
	}
	return firstNonErrorReturnField(inner)
}

// findBalancedParenEnd returns the index of the `)` that closes the
// `(` at s[0]. Returns -1 when the parens don't balance.
func findBalancedParenEnd(s string) int {
	if len(s) == 0 || s[0] != '(' {
		return -1
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// firstNonErrorReturnField splits a return-clause body by top-level
// commas and returns the first type expression that isn't `error`.
// Named return parameters (`result *User`) are handled by taking the
// last whitespace-separated token as the type.
func firstNonErrorReturnField(inner string) string {
	var fields []string
	depth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				fields = append(fields, strings.TrimSpace(inner[start:i]))
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(inner[start:]); last != "" {
		fields = append(fields, last)
	}
	for _, f := range fields {
		t := f
		// Named return: `ctx context.Context`, `result *User`. Grab
		// the last whitespace-separated token — that's the type.
		if parts := strings.Fields(f); len(parts) > 1 {
			t = parts[len(parts)-1]
		}
		if t == "error" || strings.HasSuffix(t, ".error") {
			continue
		}
		return t
	}
	return ""
}

// snapshotContractShapes walks every request_type / response_type
// reference in the registry, loads each referenced type node's source,
// and attaches the extracted Shape to the node's Meta["shape"].
//
// We:
//   - Collect the unique set of symbol IDs — a popular DTO might be a
//     request/response on dozens of routes and we want to parse its
//     source once.
//   - Read each file once (cached in the source map).
//   - Skip nodes whose ID doesn't look like a symbol (bare names that
//     couldn't be upgraded) — those have nothing to dereference.
//   - Skip type nodes that already have a shape attached from a prior
//     pass on the same session (ETag-style short-circuit).
func (idx *Indexer) snapshotContractShapes(reg *contracts.Registry) {
	symbols := make(map[string]struct{})
	for _, c := range reg.All() {
		for _, key := range []string{"request_type", "response_type"} {
			v, _ := c.Meta[key].(string)
			if v == "" || !strings.Contains(v, "::") {
				continue
			}
			symbols[v] = struct{}{}
		}
	}
	if len(symbols) == 0 {
		return
	}
	srcCache := make(map[string][]byte)
	attached := 0
	for id := range symbols {
		node := idx.graph.GetNode(id)
		if node == nil || node.Kind != graph.KindType {
			continue
		}
		if _, done := node.Meta["shape"]; done {
			continue
		}
		src, ok := srcCache[node.FilePath]
		if !ok {
			// File paths in the graph are repo-prefixed; trim the
			// prefix for disk I/O.
			diskPath := node.FilePath
			if idx.repoPrefix != "" && strings.HasPrefix(diskPath, idx.repoPrefix+"/") {
				diskPath = strings.TrimPrefix(diskPath, idx.repoPrefix+"/")
			}
			diskPath = filepath.Join(idx.rootPath, diskPath)
			data, err := os.ReadFile(diskPath)
			if err != nil {
				srcCache[node.FilePath] = nil
				continue
			}
			srcCache[node.FilePath] = data
			src = data
		}
		if src == nil {
			continue
		}
		shape := contracts.ExtractShape(node.FilePath, src, node.StartLine, node.EndLine)
		if shape == nil {
			continue
		}
		if node.Meta == nil {
			node.Meta = map[string]any{}
		}
		node.Meta["shape"] = shape
		attached++
	}
	if attached > 0 {
		idx.logger.Info("contract shapes snapshotted",
			zap.Int("types", attached),
			zap.Int("examined", len(symbols)))
	}
}

// extractExternalModules parses the repo's go.mod once and writes
// KindModule nodes plus EdgeDependsOnModule edges into the graph.
// A synthetic KindFile node is emitted for go.mod itself so the
// edges have a real source endpoint that survives applyRepoPrefix.
// Safe to call when no go.mod exists. Other manifest formats
// (package.json, pnpm-lock, requirements.txt, Cargo.toml, …) are
// future additions — each lands as another switch case here.
//
// Import-node → module-node edges (per the broader coverage spec)
// are deferred to v2; the v1 file-level edge is already enough for
// agents asking "what does this repo depend on".
func (idx *Indexer) extractExternalModules() {
	if !idx.config.Coverage.IsEnabled("modules") {
		return
	}
	// Walk known manifest formats at the repo root. Each manifest
	// produces an independent Spec list and gets its own synthetic
	// file node — the file→module edge stays scoped to the
	// originating manifest so cross-ecosystem queries (e.g. "what
	// does package.json declare") don't bleed into go.mod's
	// answer.
	manifests := []struct {
		path           string
		parse          func([]byte) []modules.Spec
		ownPathFromSrc func([]byte) string
	}{
		{
			path:           "go.mod",
			parse:          modules.ParseGoMod,
			ownPathFromSrc: readGoModModulePath,
		},
		{
			path:           "package.json",
			parse:          modules.ParsePackageJSON,
			ownPathFromSrc: readPackageJSONOwnName,
		},
		{
			path:           "pyproject.toml",
			parse:          modules.ParsePyProject,
			ownPathFromSrc: readPyProjectOwnName,
		},
		{
			path:           "requirements.txt",
			parse:          modules.ParseRequirementsTxt,
			ownPathFromSrc: nil, // requirements.txt has no own-name notion
		},
	}

	for _, m := range manifests {
		idx.extractOneModuleManifest(m.path, m.parse, m.ownPathFromSrc)
	}
}

// extractOneModuleManifest reads a single manifest file from the
// repo root, parses it via the supplied parser, and writes the
// resulting nodes/edges + import-link edges into the graph. Used
// from extractExternalModules's per-manifest dispatch.
func (idx *Indexer) extractOneModuleManifest(relPath string, parse func([]byte) []modules.Spec, ownPathFromSrc func([]byte) string) {
	manifestAbs := filepath.Join(idx.rootPath, relPath)
	src, err := os.ReadFile(manifestAbs)
	if err != nil {
		return
	}
	specs := parse(src)
	nodes, edges := modules.BuildGraphArtifacts(relPath, specs)
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	// Synthetic file node for the manifest — it isn't represented
	// through the language-extractor pipeline (no extractor is
	// registered for the .mod extension; package.json may have one
	// but the JSON walker doesn't emit a synthetic file node we
	// can reuse), so the EdgeDependsOnModule edges would otherwise
	// dangle from a missing source endpoint after applyRepoPrefix
	// runs in multi-repo mode.
	manifestNode := &graph.Node{
		ID:       relPath,
		Kind:     graph.KindFile,
		Name:     filepath.Base(relPath),
		FilePath: relPath,
		Language: manifestLanguage(relPath),
	}
	allNodes := append([]*graph.Node{manifestNode}, nodes...)
	idx.applyRepoPrefix(allNodes, edges)
	for _, n := range allNodes {
		idx.graph.AddNode(n)
	}
	for _, e := range edges {
		idx.graph.AddEdge(e)
	}

	// Connect each KindImport node to its matching module via
	// longest-prefix path resolution. Repo-internal imports (the
	// own-module path) are filtered inside LinkImports — when the
	// manifest doesn't expose an own-name, the filter is a no-op
	// which is safe (no own-module imports to match against).
	var ownModulePath string
	if ownPathFromSrc != nil {
		ownModulePath = ownPathFromSrc(src)
	}
	modules.LinkImports(idx.graph, specs, ownModulePath)
}

// manifestLanguage returns the language tag stamped on a manifest's
// synthetic file node. Used purely for Brief listings — the kind
// field carries the structural type.
func manifestLanguage(relPath string) string {
	switch filepath.Base(relPath) {
	case "go.mod":
		return "go"
	case "package.json":
		return "json"
	case "pyproject.toml":
		return "toml"
	case "requirements.txt":
		return "text"
	}
	return ""
}

// readPyProjectOwnName returns the package's own name from the
// pyproject.toml `[project] name` field. Used by LinkImports's
// own-module filter so a workspace package's internal imports
// don't accidentally collide with external pypi names. Returns ""
// on parse error or when the field is absent.
func readPyProjectOwnName(src []byte) string {
	var manifest struct {
		Project struct {
			Name string `toml:"name"`
		} `toml:"project"`
	}
	if err := toml.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	return manifest.Project.Name
}

// readPackageJSONOwnName extracts the manifest's `name` field — the
// npm equivalent of go.mod's `module` directive. Returns "" on
// missing or malformed JSON; LinkImports treats "" as "no own-
// module filter", which is safe because internal-package matches
// (e.g. workspaces) won't accidentally collide with external deps
// at the longest-prefix scan.
func readPackageJSONOwnName(src []byte) string {
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(src, &manifest); err != nil {
		return ""
	}
	return manifest.Name
}

// readGoModModulePath extracts the `module ` directive value from
// go.mod source. Mirrors the inline parse in coverage.ReadModulePath
// — we keep both copies tiny rather than introducing a one-import
// shared helper that would force a layering compromise (coverage
// shouldn't depend on indexer; indexer shouldn't depend on coverage).
func readGoModModulePath(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// extractGoModContracts runs the go.mod-specific extractor once against
// the repo root (go.mod isn't represented as a file node in the graph).
// Results are added to reg. Safe to call when no go.mod exists.
func (idx *Indexer) extractGoModContracts(reg *contracts.Registry) {
	goModPath := filepath.Join(idx.rootPath, "go.mod")
	goModSrc, err := os.ReadFile(goModPath)
	if err != nil {
		return
	}
	goModExtractor := &contracts.GoModExtractor{TrackedRepos: idx.trackedRepoModules}
	goModFilePath := "go.mod"
	if idx.repoPrefix != "" {
		goModFilePath = idx.repoPrefix + "/go.mod"
	}
	found := goModExtractor.Extract(goModFilePath, goModSrc, nil, nil)
	reg.AddAllScoped(found, idx.repoPrefix, idx.workspaceID, idx.projectID)
}

// extractContracts scans all file nodes in the graph and runs contract
// extractors to detect API contracts (HTTP routes, gRPC services,
// GraphQL, topics, etc.). Detected contracts are added as graph nodes
// with provides/consumes edges.
//
// This full-walk path is used by IncrementalReindex (where many files
// are already cached). IndexCtx instead runs the per-file work inline
// with parsing — see the worker loop — and skips this function.
func (idx *Indexer) extractContracts() {
	reg := contracts.NewRegistry()
	_, byLang := idx.buildPerFileContractExtractors()

	// Track which file nodes we saw this pass so we can prune stale
	// cache entries afterwards (files that left the graph).
	seenFiles := make(map[string]struct{})

	for _, fileNode := range idx.graph.AllNodes() {
		if fileNode.Kind != graph.KindFile {
			continue
		}

		// In multi-repo mode, only process files belonging to this repo.
		// File paths are prefixed with repo name (e.g. "labrador/api/...").
		relPath := fileNode.FilePath
		if idx.repoPrefix != "" {
			if !strings.HasPrefix(relPath, idx.repoPrefix+"/") {
				continue // skip files from other repos
			}
			// Strip repo prefix to get the actual file path relative to rootPath
			relPath = strings.TrimPrefix(relPath, idx.repoPrefix+"/")
		}

		absPath := filepath.Join(idx.rootPath, relPath)
		info, statErr := os.Stat(absPath)
		if statErr != nil {
			continue
		}
		seenFiles[fileNode.FilePath] = struct{}{}
		currentMtime := info.ModTime().UnixNano()

		// Cache hit: replay the previously-extracted contracts without
		// re-reading the file or re-running the 8 extractors. This is
		// the dominant savings path on repos with many files where most
		// haven't changed since the last extraction (e.g. live re-index
		// after a single-file edit).
		idx.contractCacheMu.RLock()
		cached, ok := idx.contractCache[fileNode.FilePath]
		idx.contractCacheMu.RUnlock()
		if ok && cached.mtimeNano == currentMtime {
			for _, c := range cached.contracts {
				reg.Add(c)
			}
			continue
		}

		src, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}

		fileNodes := idx.graph.GetFileNodes(fileNode.FilePath)
		fileEdges := idx.graph.GetOutEdges(fileNode.ID)

		// Language-filtered dispatch: skip extractors that don't list
		// this file's language in SupportedLanguages(). On big repos
		// with lots of .css / .svg / .json etc. this cuts a lot of
		// no-op extractor calls.
		exts := byLang[fileNode.Language]
		fileContracts := idx.runContractExtractorsForFile(
			fileNode.FilePath, src, fileNodes, fileEdges, exts)
		for _, c := range fileContracts {
			reg.Add(c)
		}

		idx.contractCacheMu.Lock()
		idx.contractCache[fileNode.FilePath] = &contractCacheEntry{
			mtimeNano: currentMtime,
			contracts: fileContracts,
		}
		idx.contractCacheMu.Unlock()
	}

	// Prune cache entries for files that are no longer in the graph
	// (deleted, or repo untracked). Otherwise the cache grows unboundedly
	// across the lifetime of the daemon.
	idx.contractCacheMu.Lock()
	for path := range idx.contractCache {
		if _, ok := seenFiles[path]; !ok {
			delete(idx.contractCache, path)
		}
	}
	idx.contractCacheMu.Unlock()

	idx.extractGoModContracts(reg)
	idx.extractDIContracts(reg)
	idx.commitContracts(reg)
}

// IsStale returns true if the file at relPath has been modified on disk since
// it was last indexed, based on comparing stored mtime against current disk mtime.
func (idx *Indexer) IsStale(relPath string) bool {
	relPath = filepath.ToSlash(relPath)

	idx.mtimeMu.RLock()
	storedMtime, ok := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	if !ok {
		// Unknown file — treat as stale.
		return true
	}

	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	info, err := os.Stat(absPath)
	if err != nil {
		// Can't stat — treat as stale.
		return true
	}

	return info.ModTime().UnixNano() != storedMtime
}
