package indexer

import (
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
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
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
}

// MultiIndexer orchestrates indexing across multiple repositories.
type MultiIndexer struct {
	graph     *graph.Graph
	registry  *parser.Registry
	search    search.Backend
	repos     map[string]*RepoMetadata // repoPrefix → metadata
	indexers  map[string]*Indexer       // repoPrefix → per-repo indexer
	configMgr *config.ConfigManager
	logger    *zap.Logger
	mu        sync.RWMutex
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
	repos := mi.configMgr.ActiveRepos()
	if len(repos) == 0 {
		return nil, nil
	}

	// Single-repo mode: delegate without prefixing.
	if len(repos) == 1 {
		return mi.indexSingleRepo(repos[0])
	}

	return mi.indexMultiRepo(repos)
}

// indexSingleRepo indexes a single repo without prefixing for backward compatibility.
func (mi *MultiIndexer) indexSingleRepo(entry config.RepoEntry) (map[string]*IndexResult, error) {
	absPath, err := filepath.Abs(entry.Path)
	if err != nil {
		return nil, fmt.Errorf("resolving path %s: %w", entry.Path, err)
	}

	prefix := config.ResolvePrefix(entry)
	cfg := mi.configMgr.GetRepoConfig(prefix)

	idx := New(mi.graph, mi.registry, cfg.Index, mi.logger)
	idx.search = mi.search
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

			cfg := mi.configMgr.GetRepoConfig(prefix)
			idx := New(mi.graph, mi.registry, cfg.Index, mi.logger)
			idx.search = mi.search
			idx.SetRepoPrefix(prefix)
			idx.SetTrackedRepoModules(trackedModules)

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

	// TODO: After all repos are indexed, run CrossRepoResolver.ResolveAll()
	// to create cross-repo edges. This will be implemented in Task 7.1.

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

	cfg := mi.configMgr.GetRepoConfig(repoPrefix)
	idx := New(mi.graph, mi.registry, cfg.Index, mi.logger)
	idx.search = mi.search
	if mi.IsMultiRepo() {
		idx.SetRepoPrefix(repoPrefix)
	}

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
	mi.mu.RUnlock()

	// Determine if we need to prefix (will be multi-repo after adding).
	willBeMultiRepo := len(mi.repos) >= 1

	cfg := mi.configMgr.GetRepoConfig(prefix)
	idx := New(mi.graph, mi.registry, cfg.Index, mi.logger)
	idx.search = mi.search
	if willBeMultiRepo {
		idx.SetRepoPrefix(prefix)
	}

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
	}
	mi.indexers[prefix] = idx
	mi.mu.Unlock()

	// Add to global config.
	entry.Path = absPath
	if err := mi.configMgr.Global().AddRepo(entry); err != nil {
		mi.logger.Warn("failed to add repo to config", zap.Error(err))
	}

	return result, nil
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

	return nodesRemoved, edgesRemoved
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
		merged.AddAll(cr.All(), repoPrefix)
	}
	return merged
}

// Graph returns the underlying shared graph.
func (mi *MultiIndexer) Graph() *graph.Graph {
	return mi.graph
}

// Search returns the shared search backend.
func (mi *MultiIndexer) Search() search.Backend {
	return mi.search
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
