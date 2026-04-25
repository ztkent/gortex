package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
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
// operations go through the same code paths the current `gortex mcp`
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

// searchBackendInfo bundles the daemon.SearchBackendStats payload with
// the separate text/vector byte counts we need to split per-repo.
type searchBackendInfo struct {
	daemon.SearchBackendStats
	vectorBytes uint64
}

// resolveSearchBackend inspects the live search backend and produces
// the stats needed by status rendering: which backend is active, total
// document count, its heap footprint, and (for disk-backed Bleve) the
// on-disk size.
//
// Real-world unwrap order: Swappable → HybridBackend → (text, vector).
// The text side is itself a concrete BM25/Bleve. Both layers have to
// be peeled; if we stop early we fall into the default branch and the
// status reports "unknown" — which was the bug users saw.
func resolveSearchBackend(b search.Backend) searchBackendInfo {
	out := searchBackendInfo{}
	if b == nil {
		return out
	}

	// 1) Unwrap Swappable so we see the currently-active inner.
	inner := b
	if sw, ok := inner.(*search.Swappable); ok {
		inner = sw.Inner()
	}
	// 2) If Hybrid is in play, split its text/vector sizes and keep
	//    drilling into the text side for name/doc-count identification.
	if hyb, ok := inner.(*search.HybridBackend); ok {
		out.vectorBytes = hyb.VectorSizeBytes()
		inner = hyb.TextBackend()
		// TextBackend() itself could be a Swappable in some setups —
		// unlikely today but cheap to guard.
		if sw, ok := inner.(*search.Swappable); ok {
			inner = sw.Inner()
		}
	}

	switch back := inner.(type) {
	case *search.BleveBackend:
		if path := back.DiskPath(); path != "" {
			out.Name = "bleve-disk"
			out.DiskPath = path
			out.DiskBytes = back.DiskBytes()
		} else {
			out.Name = "bleve-memory"
		}
		out.DocCount = back.Count()
		out.Bytes = back.SizeBytes()
	case *search.BM25Backend:
		out.Name = "bm25"
		out.DocCount = back.Count()
		out.Bytes = back.SizeBytes()
	default:
		out.Name = "unknown"
		out.DocCount = b.Count()
		out.Bytes = search.BackendSize(b)
	}
	return out
}

// Status gathers per-repo stats and basic process metrics. Daemon-level
// fields (PID, uptime, socket, session count) are filled in by the
// daemon itself before the response goes out.
func (c *realController) Status(_ context.Context) (daemon.StatusResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		tracked                  []daemon.TrackedRepoStatus
		searchBackendForResponse daemon.SearchBackendStats
		totalNodes               int
	)
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
		backendStats := resolveSearchBackend(c.multiIndexer.Search())
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
				mem.SearchBytes = uint64(float64(backendStats.Bytes) * share)
				mem.VectorsBytes = uint64(float64(backendStats.vectorBytes) * share)
				mem.DiskBytes = uint64(float64(backendStats.DiskBytes) * share)
			}
			mem.TotalBytes = mem.NodesBytes + mem.EdgesBytes + mem.SearchBytes + mem.VectorsBytes

			// Pull the §4.2 workspace/project slugs straight off the
			// per-repo Indexer — that's the source of truth that
			// stamps every node emitted by this repo. Falls back to
			// the prefix on legacy setups where no .gortex.yaml
			// declares them (the resolveWorkspaceID default).
			var ws, wsProj string
			if idx := c.multiIndexer.GetIndexer(prefix); idx != nil {
				ws = idx.WorkspaceID()
				wsProj = idx.ProjectID()
			}
			if ws == "" {
				ws = prefix
			}
			if wsProj == "" {
				wsProj = prefix
			}

			tracked = append(tracked, daemon.TrackedRepoStatus{
				Prefix:           prefix,
				Path:             meta.RootPath,
				Workspace:        ws,
				WorkspaceProject: wsProj,
				Files:            meta.FileCount,
				Nodes:            nodes,
				Edges:            edges,
				LastIndex:        meta.LastIndexTime.Unix(),
				Memory:           mem,
			})
		}
		searchBackendForResponse = backendStats.SearchBackendStats
	}

	// Aggregate per-workspace stats so the renderer can emit a
	// "workspaces" block. Hidden when every repo defaults to its own
	// slug (the legacy single-workspace-per-repo case where the
	// summary just duplicates the table).
	wsAgg := make(map[string]*daemon.WorkspaceSummary)
	wsKeys := make([]string, 0)
	for _, r := range tracked {
		s, ok := wsAgg[r.Workspace]
		if !ok {
			s = &daemon.WorkspaceSummary{Slug: r.Workspace}
			wsAgg[r.Workspace] = s
			wsKeys = append(wsKeys, r.Workspace)
		}
		s.Repos = append(s.Repos, r.Prefix)
		seenProj := false
		for _, p := range s.Projects {
			if p == r.WorkspaceProject {
				seenProj = true
				break
			}
		}
		if !seenProj {
			s.Projects = append(s.Projects, r.WorkspaceProject)
		}
		s.Files += r.Files
		s.Nodes += r.Nodes
		s.Edges += r.Edges
	}
	// Always populate the per-workspace rollup — even when every
	// workspace is a default singleton. Hiding it on legacy setups
	// makes the §4 boundary feature invisible, which is the opposite
	// of what users want when they're trying to migrate. Renderer-
	// side compaction (single-line hint vs full table) keeps the
	// output tidy when there's nothing meaningful to summarise.
	sort.Strings(wsKeys)
	workspaces := make([]daemon.WorkspaceSummary, 0, len(wsKeys))
	for _, k := range wsKeys {
		workspaces = append(workspaces, *wsAgg[k])
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return daemon.StatusResponse{
		TrackedRepos:  tracked,
		MemoryBytes:   mem.Alloc,
		SearchBackend: searchBackendForResponse,
		Runtime: daemon.RuntimeStats{
			Alloc:        mem.Alloc,
			Sys:          mem.Sys,
			HeapInuse:    mem.HeapInuse,
			HeapIdle:     mem.HeapIdle,
			HeapReleased: mem.HeapReleased,
			StackInuse:   mem.StackInuse,
			NumGC:        mem.NumGC,
			NumGoroutine: runtime.NumGoroutine(),
		},
		PProfAddr:         daemonPProfAddr(),
		Ready:             c.ready.Load(),
		WarmupSeconds:     c.warmupSeconds.Load(),
		Workspaces:        workspaces,
		ConfiguredServers: c.collectConfiguredServers(),
		LocalServerSlug:   c.localServerSlug(),
	}, nil
	// MCPSessions is populated by the daemon Server (it owns the
	// SessionRegistry — the controller doesn't have a back-pointer).
	// See internal/daemon/server.go around the ControlStatus handler.
}

// collectConfiguredServers reads `~/.gortex/servers.toml` (best
// effort — a missing or malformed file just returns nil) and
// projects it onto the status response. Auth tokens are NOT
// included; the HasAuth flag is enough for the human-facing
// "yes/no" decision.
func (c *realController) collectConfiguredServers() []daemon.ConfiguredServerStatus {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil || cfg == nil || len(cfg.Server) == 0 {
		return nil
	}
	local := c.localServerSlug()
	out := make([]daemon.ConfiguredServerStatus, 0, len(cfg.Server))
	for _, s := range cfg.Server {
		out = append(out, daemon.ConfiguredServerStatus{
			Slug:       s.Slug,
			URL:        s.URL,
			Default:    s.Default,
			Local:      s.Slug == local,
			Workspaces: s.Workspaces,
			HasAuth:    s.AuthToken != "" || s.AuthTokenEnv != "",
		})
	}
	return out
}

// localServerSlug returns the slug servers.toml's `default = true`
// entry uses, falling back to the first entry when none is marked.
// Empty when no servers.toml.
func (c *realController) localServerSlug() string {
	cfg, err := daemon.LoadServersConfig("")
	if err != nil || cfg == nil || len(cfg.Server) == 0 {
		return ""
	}
	if def := cfg.DefaultServer(); def != nil {
		return def.Slug
	}
	return ""
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

