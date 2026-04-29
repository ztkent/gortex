package indexer

import (
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/resolver"
)

// MultiWatcher manages file watchers across multiple repositories.
type MultiWatcher struct {
	watchers    map[string]*Watcher    // repoPrefix → file watcher
	gitWatchers map[string]*GitWatcher // repoPrefix → .git ref watcher
	started     map[string]bool        // tracks which watchers have been started
	multi       *MultiIndexer
	resolver    *resolver.CrossRepoResolver
	logger      *zap.Logger
	events      chan GraphChangeEvent
	done        chan struct{}
	mu          sync.Mutex
}

// NewMultiWatcher creates a MultiWatcher that watches all configured repos.
// Each repo gets its own Watcher with repo-specific exclude patterns.
func NewMultiWatcher(
	mi *MultiIndexer,
	configs map[string]config.WatchConfig,
	logger *zap.Logger,
) (*MultiWatcher, error) {
	mw := &MultiWatcher{
		watchers:    make(map[string]*Watcher),
		gitWatchers: make(map[string]*GitWatcher),
		started:     make(map[string]bool),
		multi:       mi,
		resolver:    resolver.NewCrossRepo(mi.Graph()),
		logger:      logger,
		events:      make(chan GraphChangeEvent, 128),
		done:        make(chan struct{}),
	}
	// Wire the cross-workspace boundary check into the resolver so
	// cross-repo edges are only resolved when the source workspace
	// declared the target via `cross_workspace_deps`.
	mw.resolver.SetCrossWorkspaceDepLookup(mi.crossWorkspaceLookup())

	for prefix, cfg := range configs {
		if err := mw.createWatcher(prefix, cfg); err != nil {
			// Log warning and continue if a repo root is inaccessible.
			logger.Warn("failed to create watcher for repo",
				zap.String("prefix", prefix),
				zap.Error(err),
			)
			continue
		}
	}

	return mw, nil
}

// createWatcher creates a per-repo Watcher for the given prefix.
func (mw *MultiWatcher) createWatcher(prefix string, cfg config.WatchConfig) error {
	meta := mw.multi.GetMetadata(prefix)
	if meta == nil {
		return fmt.Errorf("repository not found: %s", prefix)
	}

	// Verify the repo root is accessible.
	if _, err := os.Stat(meta.RootPath); err != nil {
		return fmt.Errorf("repo root inaccessible: %s: %w", meta.RootPath, err)
	}

	idx := mw.multi.GetIndexer(prefix)
	if idx == nil {
		return fmt.Errorf("no indexer for repo: %s", prefix)
	}

	w, err := NewWatcher(idx, cfg, mw.logger.With(zap.String("repo", prefix)))
	if err != nil {
		return fmt.Errorf("creating watcher for %s: %w", prefix, err)
	}

	mw.watchers[prefix] = w
	return nil
}

// Start begins watching all configured repos. Events from per-repo watchers
// are merged into the single Events() channel.
func (mw *MultiWatcher) Start() error {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	for prefix, w := range mw.watchers {
		meta := mw.multi.GetMetadata(prefix)
		if meta == nil {
			mw.logger.Warn("skipping watcher start: repo metadata not found",
				zap.String("prefix", prefix))
			continue
		}

		// Verify root is still accessible before starting.
		if _, err := os.Stat(meta.RootPath); err != nil {
			mw.logger.Warn("repo root inaccessible, skipping watcher",
				zap.String("prefix", prefix),
				zap.String("root", meta.RootPath),
				zap.Error(err),
			)
			continue
		}

		if err := w.Start([]string{meta.RootPath}); err != nil {
			mw.logger.Warn("failed to start watcher for repo",
				zap.String("prefix", prefix),
				zap.Error(err),
			)
			continue
		}

		mw.started[prefix] = true

		// Start the .git/HEAD watcher alongside the file watcher.
		// It's best-effort — repos without a .git dir (uninitialised
		// worktrees, tarball checkouts) simply skip it.
		if idx := mw.multi.GetIndexer(prefix); idx != nil {
			gw, err := NewGitWatcher(meta.RootPath, idx, mw.logger.With(zap.String("repo", prefix)))
			if err != nil {
				mw.logger.Debug("git-watcher: init failed",
					zap.String("prefix", prefix), zap.Error(err))
			} else if err := gw.Start(); err != nil {
				mw.logger.Debug("git-watcher: start failed",
					zap.String("prefix", prefix), zap.Error(err))
				_ = gw.Stop()
			} else {
				mw.gitWatchers[prefix] = gw
			}
		}

		// Forward events from this watcher and trigger cross-repo resolution.
		go mw.forwardEvents(prefix, w)
	}

	return nil
}

// forwardEvents reads events from a per-repo watcher and forwards them
// to the merged events channel. After each event, it triggers cross-repo
// resolution for the owning repo.
func (mw *MultiWatcher) forwardEvents(prefix string, w *Watcher) {
	for {
		select {
		case <-mw.done:
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}

			// After re-indexing, trigger cross-repo resolution.
			if mw.multi.IsMultiRepo() {
				stats := mw.resolver.ResolveForRepo(prefix)
				if stats.CrossRepoEdges > 0 {
					mw.logger.Debug("cross-repo edges updated after file change",
						zap.String("repo", prefix),
						zap.String("file", ev.FilePath),
						zap.Int("cross_repo_edges", stats.CrossRepoEdges),
					)
				}
			}

			// Non-blocking send to merged channel.
			select {
			case mw.events <- ev:
			default:
			}
		}
	}
}

// Stop halts all per-repo watchers and cleans up resources.
func (mw *MultiWatcher) Stop() error {
	close(mw.done)

	mw.mu.Lock()
	defer mw.mu.Unlock()

	var firstErr error
	for prefix, w := range mw.watchers {
		// Only stop watchers that were actually started.
		if !mw.started[prefix] {
			continue
		}
		if err := w.Stop(); err != nil && firstErr == nil {
			firstErr = err
			mw.logger.Warn("error stopping watcher",
				zap.String("prefix", prefix),
				zap.Error(err),
			)
		}
		if gw, ok := mw.gitWatchers[prefix]; ok {
			_ = gw.Stop()
		}
	}

	return firstErr
}

// Events returns a read-only channel of merged graph change events from all repos.
func (mw *MultiWatcher) Events() <-chan GraphChangeEvent {
	return mw.events
}

// AddRepo creates and starts a watcher for a newly tracked repo.
func (mw *MultiWatcher) AddRepo(repoPrefix string, cfg config.WatchConfig) error {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	if _, exists := mw.watchers[repoPrefix]; exists {
		return fmt.Errorf("watcher already exists for repo: %s", repoPrefix)
	}

	if err := mw.createWatcher(repoPrefix, cfg); err != nil {
		mw.logger.Warn("failed to add watcher for repo",
			zap.String("prefix", repoPrefix),
			zap.Error(err),
		)
		return err
	}

	w := mw.watchers[repoPrefix]
	meta := mw.multi.GetMetadata(repoPrefix)
	if meta == nil {
		return fmt.Errorf("repository metadata not found: %s", repoPrefix)
	}

	if err := w.Start([]string{meta.RootPath}); err != nil {
		delete(mw.watchers, repoPrefix)
		return fmt.Errorf("starting watcher for %s: %w", repoPrefix, err)
	}

	mw.started[repoPrefix] = true
	if idx := mw.multi.GetIndexer(repoPrefix); idx != nil {
		if gw, err := NewGitWatcher(meta.RootPath, idx, mw.logger.With(zap.String("repo", repoPrefix))); err == nil {
			if err := gw.Start(); err == nil {
				mw.gitWatchers[repoPrefix] = gw
			} else {
				_ = gw.Stop()
			}
		}
	}
	go mw.forwardEvents(repoPrefix, w)
	return nil
}

// RemoveRepo stops and removes the watcher for a repo.
func (mw *MultiWatcher) RemoveRepo(repoPrefix string) error {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	w, exists := mw.watchers[repoPrefix]
	if !exists {
		return fmt.Errorf("no watcher for repo: %s", repoPrefix)
	}

	var err error
	if mw.started[repoPrefix] {
		err = w.Stop()
	}
	if gw, ok := mw.gitWatchers[repoPrefix]; ok {
		_ = gw.Stop()
		delete(mw.gitWatchers, repoPrefix)
	}
	delete(mw.watchers, repoPrefix)
	delete(mw.started, repoPrefix)
	return err
}
