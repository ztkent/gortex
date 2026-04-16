package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search"
)

// realController is the production daemon.Controller implementation. It
// wraps the MultiIndexer and ConfigManager so track/untrack/reload/status
// operations go through the same code paths the current `gortex serve`
// command uses.
//
// Methods are serialized via a mutex — track/reload can race with status
// otherwise. The mutex is coarse; finer locking is a later optimization.
type realController struct {
	mu            sync.Mutex
	graph         *graph.Graph
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	multiWatcher  *indexer.MultiWatcher
	logger        *zap.Logger

	// onShutdown is invoked by the Shutdown method. Used by the daemon
	// main to flush savings, close the snapshot store, etc.
	onShutdown func() error

	// ready flips to true once warmup (per-repo re-index + watcher
	// startup) finishes. The socket accepts connections before this —
	// queries against not-yet-indexed repos return partial results
	// until ready is true. warmupSeconds records how long warmup took
	// so status can surface it.
	ready          atomic.Bool
	warmupSeconds  atomic.Int64
}

// Track indexes a new repository and persists it to the global config.
// Path is resolved to an absolute form before the MultiIndexer sees it.
func (c *realController) Track(ctx context.Context, p daemon.TrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}
	absPath, err := filepath.Abs(p.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	entry := config.RepoEntry{Path: absPath, Name: p.Name, Ref: p.Ref}
	result, err := c.multiIndexer.TrackRepoCtx(ctx, entry)
	if err != nil {
		return nil, err
	}
	if result == nil {
		// Already tracked — idempotent.
		return json.RawMessage(fmt.Sprintf(`{"status":"already_tracked","path":%q}`, absPath)), nil
	}

	// Project association from TrackParams.Project isn't wired yet — the
	// config package doesn't expose an AddRepoToProject helper. Callers
	// who need project scoping can edit ~/.config/gortex/config.yaml and
	// run `gortex daemon reload`; track from the daemon-v1 surface just
	// adds to the top-level repo list.

	// Attach a watcher to the newly-tracked repo so file edits in it
	// flow back into the graph live without a manual reload. Failures
	// here are logged but don't fail the track — an indexed-but-
	// unwatched repo is still queryable, just stale if edited.
	if c.multiWatcher != nil && c.configManager != nil {
		prefix := config.ResolvePrefix(entry)
		wcfg := c.configManager.GetRepoConfig(prefix).Watch
		if err := c.multiWatcher.AddRepo(prefix, wcfg); err != nil {
			c.logger.Warn("track: attach watcher failed",
				zap.String("prefix", prefix), zap.Error(err))
		}
	}

	// Persist the config change. TrackRepoCtx mutates the in-memory
	// GlobalConfig via AddRepo but does not flush to disk; without this
	// Save the new repo vanishes on daemon restart. Mirrors Untrack.
	if c.configManager != nil {
		if err := c.configManager.Global().Save(); err != nil {
			c.logger.Warn("track: save config failed", zap.Error(err))
		}
	}

	return json.Marshal(map[string]any{
		"status":     "tracked",
		"path":       absPath,
		"prefix":     config.ResolvePrefix(entry),
		"file_count": result.FileCount,
		"node_count": result.NodeCount,
		"edge_count": result.EdgeCount,
	})
}

// Untrack evicts a repo from the graph and drops it from config.
// PathOrPrefix accepts either an absolute path or a repo prefix.
func (c *realController) Untrack(_ context.Context, p daemon.UntrackParams) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.multiIndexer == nil {
		return nil, fmt.Errorf("multi-repo indexer not initialized")
	}

	prefix := p.PathOrPrefix
	// Resolve path → prefix if an absolute or relative path was given.
	if filepath.IsAbs(p.PathOrPrefix) {
		for pfx, meta := range c.multiIndexer.AllMetadata() {
			if meta.RootPath == p.PathOrPrefix {
				prefix = pfx
				break
			}
		}
	}

	// Detach the watcher before evicting from the graph — otherwise a
	// late fsnotify event could race the eviction and try to re-index
	// files whose nodes are already gone.
	if c.multiWatcher != nil {
		if err := c.multiWatcher.RemoveRepo(prefix); err != nil {
			c.logger.Debug("untrack: detach watcher",
				zap.String("prefix", prefix), zap.Error(err))
		}
	}

	nodesRemoved, edgesRemoved := c.multiIndexer.UntrackRepo(prefix)

	// Persist the config change.
	if c.configManager != nil {
		_ = c.configManager.Global().RemoveRepo(prefix)
		if err := c.configManager.Global().Save(); err != nil {
			c.logger.Warn("untrack: save config failed", zap.Error(err))
		}
	}

	return json.Marshal(map[string]any{
		"status":        "untracked",
		"prefix":        prefix,
		"nodes_removed": nodesRemoved,
		"edges_removed": edgesRemoved,
	})
}

// Reload re-reads the global config, indexes new repos that were added
// via direct config-file edits, and untracks any that were removed.
// Existing, unchanged tracked repos keep their current state.
func (c *realController) Reload(ctx context.Context) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.configManager == nil {
		return nil, fmt.Errorf("config manager not initialized")
	}
	if err := c.configManager.Reload(); err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}

	var added, removed int
	wantedPrefixes := make(map[string]bool)

	for _, entry := range c.configManager.Global().Repos {
		prefix := config.ResolvePrefix(entry)
		wantedPrefixes[prefix] = true
		if _, exists := c.multiIndexer.AllMetadata()[prefix]; exists {
			continue
		}
		if _, err := c.multiIndexer.TrackRepoCtx(ctx, entry); err != nil {
			c.logger.Warn("reload: track failed",
				zap.String("path", entry.Path), zap.Error(err))
			continue
		}
		added++
	}

	for prefix := range c.multiIndexer.AllMetadata() {
		if wantedPrefixes[prefix] {
			continue
		}
		c.multiIndexer.UntrackRepo(prefix)
		removed++
	}

	return json.Marshal(map[string]any{
		"added":   added,
		"removed": removed,
	})
}

// Status gathers per-repo stats and basic process metrics. Daemon-level
// fields (PID, uptime, socket, session count) are filled in by the
// daemon itself before the response goes out.
func (c *realController) Status(_ context.Context) (daemon.StatusResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var tracked []daemon.TrackedRepoStatus
	if c.multiIndexer != nil {
		// meta.NodeCount / meta.EdgeCount were frozen at TrackRepo time
		// from graph.NodeCount() — which is the *whole* multi-repo graph,
		// not this repo's slice. Recompute from graph.RepoStats() at
		// status time so the numbers actually reflect this repo's
		// contribution. Falls back to the stored counts when the graph
		// has no entry for the prefix (shouldn't happen in practice,
		// but keeps the output complete rather than zeroed).
		var repoStats map[string]graph.GraphStats
		if c.graph != nil {
			repoStats = c.graph.RepoStats()
		}

		// Search and vector backends are process-wide (one shared index
		// across all repos), so we compute the global size once and
		// split it proportionally to each repo's node share. Not exact,
		// but it's the best attribution we can make without indexing
		// per-repo which would double storage for the sake of a status
		// breakdown.
		var (
			globalSearchBytes, globalVectorBytes uint64
			totalNodes                           int
		)
		if backend := c.multiIndexer.Search(); backend != nil {
			// If the backend is Hybrid, split text vs vector; otherwise
			// treat the whole thing as "search."
			if hyb, ok := backend.(*search.HybridBackend); ok {
				globalSearchBytes = hyb.TextSizeBytes()
				globalVectorBytes = hyb.VectorSizeBytes()
			} else {
				globalSearchBytes = search.BackendSize(backend)
			}
		}
		for _, s := range repoStats {
			totalNodes += s.TotalNodes
		}

		for prefix, meta := range c.multiIndexer.AllMetadata() {
			nodes := meta.NodeCount
			edges := meta.EdgeCount
			if s, ok := repoStats[prefix]; ok {
				nodes = s.TotalNodes
				edges = s.TotalEdges
			}

			var mem daemon.MemoryBreakdown
			if c.graph != nil {
				est := c.graph.RepoMemoryEstimate(prefix)
				mem.NodesBytes = est.NodeBytes
				mem.EdgesBytes = est.EdgeBytes
			}
			if totalNodes > 0 && nodes > 0 {
				share := float64(nodes) / float64(totalNodes)
				mem.SearchBytes = uint64(float64(globalSearchBytes) * share)
				mem.VectorsBytes = uint64(float64(globalVectorBytes) * share)
			}
			mem.TotalBytes = mem.NodesBytes + mem.EdgesBytes + mem.SearchBytes + mem.VectorsBytes

			tracked = append(tracked, daemon.TrackedRepoStatus{
				Prefix:    prefix,
				Path:      meta.RootPath,
				Files:     meta.FileCount,
				Nodes:     nodes,
				Edges:     edges,
				LastIndex: meta.LastIndexTime.Unix(),
				Memory:    mem,
			})
		}
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return daemon.StatusResponse{
		TrackedRepos:  tracked,
		MemoryBytes:   mem.Alloc,
		Ready:         c.ready.Load(),
		WarmupSeconds: c.warmupSeconds.Load(),
	}, nil
}

// SearchSymbols runs a substring match over node names and returns the
// matching symbols. It's the cheap probe path for clients (notably the
// Grep-redirect hook) that need a fast yes/no without setting up a full
// MCP session. File and Import nodes are excluded — the hook only cares
// about real symbol matches.
func (c *realController) SearchSymbols(_ context.Context, p daemon.SearchSymbolsParams) (daemon.SearchSymbolsResult, error) {
	c.mu.Lock()
	g := c.graph
	c.mu.Unlock()

	if g == nil || p.Query == "" {
		return daemon.SearchSymbolsResult{}, nil
	}

	limit := p.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	needle := strings.ToLower(p.Query)
	hits := make([]daemon.SymbolHit, 0, limit)
	for _, n := range g.AllNodes() {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if p.Repo != "" && n.RepoPrefix != p.Repo {
			continue
		}
		if !strings.Contains(strings.ToLower(n.Name), needle) {
			continue
		}
		hits = append(hits, daemon.SymbolHit{
			Name:     n.Name,
			Kind:     string(n.Kind),
			FilePath: n.FilePath,
			Line:     n.StartLine,
		})
		if len(hits) >= limit {
			break
		}
	}
	return daemon.SearchSymbolsResult{Hits: hits}, nil
}

// AttachWatcher is called by warmup to hand over the MultiWatcher once
// it has been initialized. Until this is called, realController.Track
// skips the per-repo watcher attach — a newly-tracked repo gets its
// watcher when the warmup-constructed MultiWatcher iterates
// mi.AllMetadata() at startup.
func (c *realController) AttachWatcher(mw *indexer.MultiWatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.multiWatcher = mw
}

// MarkReady flips the ready flag and records how long warmup took.
// Safe to call concurrently with Status (atomic loads on the read side).
func (c *realController) MarkReady(d time.Duration) {
	c.warmupSeconds.Store(int64(d.Seconds()))
	c.ready.Store(true)
}

// Shutdown gives the caller (the daemon main) a chance to flush any
// per-instance stores. The actual socket teardown is the Server's job.
func (c *realController) Shutdown(_ context.Context) error {
	c.mu.Lock()
	hook := c.onShutdown
	c.mu.Unlock()
	if hook != nil {
		return hook()
	}
	return nil
}

