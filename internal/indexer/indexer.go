package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
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

// RootPath returns the root path used for relative path computation.
func (idx *Indexer) RootPath() string { return idx.rootPath }

// SetRepoPrefix sets the repository prefix for multi-repo mode.
// When non-empty, all node IDs and file paths are prefixed with "<repoPrefix>/".
func (idx *Indexer) SetRepoPrefix(prefix string) { idx.repoPrefix = prefix }

// RepoPrefix returns the current repository prefix.
func (idx *Indexer) RepoPrefix() string { return idx.repoPrefix }

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
func (idx *Indexer) applyRepoPrefix(nodes []*graph.Node, edges []*graph.Edge) {
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

	reporter.Report("building search index", 0, 0)
	// Build search index.
	idx.buildSearchIndex()

	// Contracts were already extracted inline during parse (per file,
	// per worker). Here we just finish up: run the go.mod extractor
	// (not associated with any file node) and commit contract nodes /
	// provides/consumes edges from the merged registry.
	reporter.Report("extracting contracts", 0, 0)
	idx.extractGoModContracts(contractReg)
	idx.commitContracts(contractReg)

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

	return &IndexResult{
		NodeCount:  idx.graph.NodeCount(),
		EdgeCount:  idx.graph.EdgeCount(),
		FileCount:  int(fileCount),
		DurationMs: time.Since(start).Milliseconds(),
		Errors:     errors,
	}, nil
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
func (idx *Indexer) buildSearchIndex() {
	nodes := idx.graph.AllNodes()

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

	return &IndexResult{
		NodeCount:  idx.graph.Stats().TotalNodes,
		EdgeCount:  idx.graph.Stats().TotalEdges,
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
	reg.AddAll(found, idx.repoPrefix)
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
