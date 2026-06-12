package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/reach"
)

// ChangeKind describes the type of filesystem change.
type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// GraphChangeEvent is emitted after a successful graph patch.
type GraphChangeEvent struct {
	FilePath     string     `json:"file_path"`
	Kind         ChangeKind `json:"kind"`
	NodesAdded   int        `json:"nodes_added"`
	NodesRemoved int        `json:"nodes_removed"`
	EdgesAdded   int        `json:"edges_added"`
	EdgesRemoved int        `json:"edges_removed"`
	Timestamp    time.Time  `json:"timestamp"`
	DurationMs   int64      `json:"duration_ms"`
}

// SymbolChangeCallback is called when symbols change during file re-indexing.
// It receives the file path, old symbols (before eviction), and new symbols (after re-index).
type SymbolChangeCallback func(filePath string, oldSymbols, newSymbols []*graph.Node)

// Watcher keeps the knowledge graph in live sync with the filesystem.
type Watcher struct {
	indexer   *Indexer
	fsw       fswatcher.Watcher
	fsCancel  context.CancelFunc
	config    config.WatchConfig
	excludes  *excludes.Matcher
	events    chan GraphChangeEvent
	history   []GraphChangeEvent
	historyMu sync.Mutex
	pending   map[string]*time.Timer
	mu        sync.Mutex
	// patchMu serialises per-path patchGraph invocations so the
	// post-patch reach rebuild (which scans every Node.Meta) cannot
	// race with another debounced patch's IndexFile / EvictFile /
	// detectClonesAndEmitEdges, all of which mutate the same Meta
	// maps unprotected. Storm-mode uses patchGraphNoResolve (driven
	// from a single goroutine in drainStorm) and bypasses this lock.
	patchMu          sync.Mutex
	logger           *zap.Logger
	done             chan struct{}
	stopped          chan struct{}
	symbolChangeCb   SymbolChangeCallback
	symbolChangeCbMu sync.RWMutex

	// probeWaiters maps a probe-file path (created during Start to confirm
	// the inotify watch is active) to a chan that handleEvent closes when
	// the probe's event arrives. Empty after Start returns.
	probeWaiters sync.Map

	// Storm-mode state. Guarded by stormMu so the hot per-file
	// debounce path (mu) doesn't contend with rate-tracking.
	stormMu      sync.Mutex
	eventTimes   []time.Time           // sliding window of recent event timestamps
	stormBatch   map[string]ChangeKind // dirty set during an event storm
	stormTimer   *time.Timer           // fires after the quiet period
	stormActive  bool                  // true while waiting to drain
	stormDrained func(int)             // test hook: batch drained; batch size arg

	// poller is the adaptive-interval fallback that re-checks git
	// HEAD movement and tracked-file mtimes on a timer, catching the
	// changes the fsnotify backend misses (inotify watch exhaustion,
	// network filesystems, dropped events). Created in Start and torn
	// down in Stop alongside the fsnotify backend. nil when the
	// per-repo watcher is disabled via WatchConfig.Enabled.
	poller *Poller

	// reconcileMu guards the overflow-driven full-tree reconcile.
	// reconcilePending coalesces a burst of overflow / dropped-event
	// signals into at most one reconcile in flight: the kernel inotify
	// queue can overflow (EventOverflow) or the backend can drop events
	// under backpressure (the Dropped() channel), and either means we
	// may have lost a create/modify with no path to re-index. macOS
	// FSEvents self-heals (it re-scans on UserDropped/KernelDropped),
	// but Linux inotify does not — without this the lost event waits on
	// the up-to-1h janitor. reconcileFn is a test seam: nil in
	// production (the real IncrementalReindex runs).
	reconcileMu      sync.Mutex
	reconcilePending bool
	reconcileFn      func()

	// pendingScanDirs coalesces newly-created directories awaiting a
	// scoped subtree re-index — the new-subdir race (see enqueueDirScan).
	// dirScanActive guards a single in-flight drainer goroutine; scanFn
	// is a test seam, nil in production (the real IncrementalReindexPaths
	// runs). All three are guarded by reconcileMu.
	pendingScanDirs map[string]struct{}
	dirScanActive   bool
	scanFn          func(map[string]struct{})
}

const maxHistory = 1000

// probeMarker is the substring embedded in handshake-probe filenames
// (see confirmWatchActive) and used by handleEvent to absorb their
// create/remove events without touching the indexer.
const probeMarker = ".gortex-watcher-handshake-"

// NewWatcher creates a Watcher for the given indexer.
//
// cfg.Exclude is expected to carry the full effective pattern list (from
// ConfigManager.EffectiveExclude). If it is empty — e.g. a direct caller
// that bypasses ConfigManager — the watcher falls back to the builtin
// baseline so the obvious non-source dirs stay ignored.
func NewWatcher(idx *Indexer, cfg config.WatchConfig, logger *zap.Logger) (*Watcher, error) {
	debounce := cfg.DebounceMs
	if debounce <= 0 {
		debounce = 150
	}
	cfg.DebounceMs = debounce

	// Storm-mode defaults — kept conservative so a repo producing
	// normal save traffic stays on the per-file path. Threshold of
	// zero means the user explicitly disabled storm mode; negative is
	// coerced to zero for safety.
	if cfg.StormThreshold < 0 {
		cfg.StormThreshold = 0
	}
	if cfg.StormWindowMs <= 0 {
		cfg.StormWindowMs = 500
	}
	if cfg.StormQuietPeriodMs <= 0 {
		cfg.StormQuietPeriodMs = 500
	}

	patterns := cfg.Exclude
	if len(patterns) == 0 {
		patterns = excludes.Builtin
	}

	return &Watcher{
		indexer:    idx,
		config:     cfg,
		excludes:   excludes.New(patterns),
		events:     make(chan GraphChangeEvent, 64),
		pending:    make(map[string]*time.Timer),
		stormBatch: make(map[string]ChangeKind),
		logger:     logger,
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}, nil
}

// Start begins watching the given paths recursively. The backend is
// fswatcher, which uses FSEvents on macOS (one stream per root,
// constant FD cost) and inotify on Linux (one watch per directory in
// the tree). On the inotify path the per-user `max_user_watches` cap
// applies; bump that sysctl if a multi-repo install grows beyond it.
func (w *Watcher) Start(paths []string) error {
	if len(paths) == 0 {
		return errors.New("watcher: no paths to watch")
	}
	ready := make(chan struct{})
	// Own the events/dropped channels so the library never closes them on
	// teardown. fswatcher's shutdown closes its events channel while its
	// EventAggregator goroutine may still be flushing a final event into
	// it — a "send on closed channel" panic under -race on the Linux
	// inotify path (the aggregator's close() does not join its run loop).
	// When we supply the channels, ownsEventsChannel is false and the
	// library skips the close; the aggregator's send is already
	// non-blocking, so a late flush lands harmlessly in our buffer (or
	// the dropped channel) and our loop still exits on its own stop
	// signal, not on the channel closing. Buffer sizes match the
	// library's defaults so coalescing behaviour is unchanged.
	droppedSize := max(fswatcher.DefaultBufferSize/fswatcher.MaxDroppedBufferRatio, fswatcher.MinDroppedBuffer)
	fswEvents := make(chan fswatcher.WatchEvent, fswatcher.DefaultBufferSize)
	fswDropped := make(chan fswatcher.WatchEvent, droppedSize)
	opts := []fswatcher.WatcherOpt{
		// Disable fswatcher's internal debouncer. Its mergeEvents path
		// mutates the Types backing array of an already-delivered event
		// when a follow-up event for the same path arrives, racing with
		// our consumer's read. Our per-file debounce + storm-mode logic
		// is the authoritative coalescer anyway.
		fswatcher.WithCooldown(0),
		// Drop the library's own logging chatter; we surface what we
		// care about through our own logger.
		fswatcher.WithSeverity(fswatcher.SeverityError),
		// Block Start until the OS-level streams are actually live.
		// Without this the first events after Start race against
		// stream registration and silently disappear.
		fswatcher.WithReadyChannel(ready),
		// We own the channels (see above) — eliminates the teardown
		// send-on-closed-channel race.
		fswatcher.WithCustomChannels(fswEvents, fswDropped),
	}
	absPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		absPaths = append(absPaths, absPath)
		opts = append(opts, fswatcher.WithPath(absPath))
	}
	fsw, err := fswatcher.New(opts...)
	if err != nil {
		return err
	}
	w.fsw = fsw

	ctx, cancel := context.WithCancel(context.Background())
	w.fsCancel = cancel
	watchErr := make(chan error, 1)
	go func() {
		err := fsw.Watch(ctx)
		watchErr <- err
		if err != nil && !errors.Is(err, context.Canceled) && w.logger != nil {
			w.logger.Warn("watcher: backend stopped", zap.Error(err))
		}
	}()
	// Wait for the backend to become ready or fail fast on early
	// initialisation errors (e.g. an inotify add returning ENOSPC).
	select {
	case <-ready:
	case err := <-watchErr:
		cancel()
		return err
	case <-time.After(5 * time.Second):
		cancel()
		return errors.New("watcher: backend did not become ready within 5s")
	}
	// FSEvents reports its stream as "started" the instant the C call
	// returns, but immediately fires synthetic "this file exists"
	// events for every pre-existing file under the watched root. The
	// flags on those events are indistinguishable from real changes
	// (Create + Modified are set), so we'd re-index every file on
	// every daemon start. Drain everything that lands in the events
	// buffer within a short grace window before starting the real
	// loop — anything genuinely happening to a file during that
	// window will fire again as new events.
	if runtime.GOOS == "darwin" {
		w.drainInitialReplay(150 * time.Millisecond)
	}
	go w.loop()
	// On Linux, fswatcher closes its ready channel as soon as the
	// inotify FD is allocated, but it registers initial paths in
	// background goroutines that may not have called inotify_add_watch
	// yet. Events fired before those goroutines run are lost forever.
	// Probe each path with a sentinel file and wait for the resulting
	// event before declaring the watcher ready.
	if runtime.GOOS != "darwin" {
		for _, p := range absPaths {
			if err := w.confirmWatchActive(p, 5*time.Second); err != nil {
				cancel()
				if w.fsw != nil {
					w.fsw.Close()
				}
				close(w.done)
				<-w.stopped
				return err
			}
		}
	}

	// Launch the adaptive-interval poller alongside the fsnotify
	// backend. It is a fallback for the changes fsnotify misses, so
	// it shares the watcher's lifecycle. Gated on WatchConfig.Enabled
	// — a repo that opted out of watching gets no fallback either.
	if w.config.Enabled {
		w.poller = newPoller(w, w.indexer, w.logger)
		w.poller.Start()
	}
	return nil
}

// confirmWatchActive writes sentinel files under root in a polling loop
// until the corresponding fswatcher event arrives — proving the
// OS-level watch is registered — or the overall timeout fires. The
// retry loop is needed because the first probe may be written before
// fswatcher's async addWatch goroutine has called inotify_add_watch,
// in which case its create event is invisible to inotify entirely.
//
// The sentinel name avoids fswatcher's built-in isSystemFile filter
// (which drops *.tmp / *.bak / *.swp / etc. before they reach our
// handleEvent) and our own excludes matcher.
func (w *Watcher) confirmWatchActive(root string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	const probeStep = 100 * time.Millisecond
	for time.Now().Before(deadline) {
		probe := filepath.Join(root, fmt.Sprintf("%s%d-%d", probeMarker, os.Getpid(), time.Now().UnixNano()))
		ch := make(chan struct{})
		w.probeWaiters.Store(probe, ch)
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			w.probeWaiters.Delete(probe)
			if w.logger != nil {
				w.logger.Warn("watcher: could not write probe; continuing without confirmation",
					zap.String("root", root), zap.Error(err))
			}
			return nil
		}
		select {
		case <-ch:
			_ = os.Remove(probe)
			return nil
		case <-time.After(probeStep):
			w.probeWaiters.Delete(probe)
			_ = os.Remove(probe)
		}
	}
	return fmt.Errorf("watcher: inotify watch on %s did not activate within %s", root, timeout)
}

// drainInitialReplay reads from the backend's events channel until
// `window` of quiet has elapsed with no further events. macOS FSEvents
// streams emit a burst of synthetic "exists" events at startup; this
// burst is bounded by the per-stream latency (~50 ms). The first call
// blocks at least one window so early events have a chance to arrive.
func (w *Watcher) drainInitialReplay(window time.Duration) {
	if w.fsw == nil {
		return
	}
	eventsCh := w.fsw.Events()
	t := time.NewTimer(window)
	defer t.Stop()
	for {
		select {
		case <-eventsCh:
			t.Reset(window)
		case <-t.C:
			return
		}
	}
}

// Stop halts the watcher and cleans up resources.
func (w *Watcher) Stop() error {
	// Stop the adaptive poller first so a poll cycle in flight can't
	// dispatch a patch into a half-torn-down watcher.
	if w.poller != nil {
		w.poller.Stop()
	}
	close(w.done)
	if w.fsCancel != nil {
		w.fsCancel()
	}
	if w.fsw != nil {
		w.fsw.Close()
	}
	<-w.stopped
	return nil
}

// Events returns a read-only channel of graph change events.
func (w *Watcher) Events() <-chan GraphChangeEvent {
	return w.events
}

// History returns recent change events (up to maxHistory).
func (w *Watcher) History() []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	out := make([]GraphChangeEvent, len(w.history))
	copy(out, w.history)
	return out
}

// HistorySince returns change events after the given timestamp.
func (w *Watcher) HistorySince(since time.Time) []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	var out []GraphChangeEvent
	for _, ev := range w.history {
		if ev.Timestamp.After(since) {
			out = append(out, ev)
		}
	}
	return out
}

// OnSymbolChange registers a callback that is invoked when symbols change
// during file re-indexing. The callback receives old symbols (before eviction)
// and new symbols (after re-index).
func (w *Watcher) OnSymbolChange(cb SymbolChangeCallback) {
	w.symbolChangeCbMu.Lock()
	defer w.symbolChangeCbMu.Unlock()
	w.symbolChangeCb = cb
}

func (w *Watcher) loop() {
	defer close(w.stopped)
	if w.fsw == nil {
		// Test path: handleEvent is being driven directly without
		// having called Start. Block until Stop closes w.done.
		<-w.done
		return
	}
	eventsCh := w.fsw.Events()
	droppedCh := w.fsw.Dropped()
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-eventsCh:
			if !ok {
				return
			}
			w.handleEvent(event)
		case _, ok := <-droppedCh:
			if !ok {
				// Backend tore down its dropped channel; keep
				// draining Events only.
				droppedCh = nil
				continue
			}
			// The backend dropped an event under backpressure (the
			// main Events channel was full). We don't know which path
			// was lost, so reconcile the whole tree.
			w.triggerOverflowReconcile("dropped-event")
		}
	}
}

// guardWatcherPanic recovers a panic in a watcher background goroutine —
// a debounced patch, a storm drain, an overflow reconcile, or a
// new-directory scan. Those goroutines call into the graph store, and
// store_sqlite turns a fatal storage error (a closed DB during a daemon
// restart, a busy/locked DB, disk-full) into a panic via panicOnFatal.
// The MCP tool path has its own firewall (wrapToolHandler); these
// fsnotify-driven goroutines don't route through it, so without this a
// single transient store error during a restart or rebuild takes the
// whole daemon down. Recovering aborts just that unit of work — the file
// stays stale until the next event or the reconcile janitor — instead of
// crashing the process.
func (w *Watcher) guardWatcherPanic(op string) {
	if r := recover(); r != nil && w.logger != nil {
		w.logger.Error("watcher: recovered from panic in background re-index",
			zap.String("op", op),
			zap.Any("panic", r),
			zap.Stack("stack"))
	}
}

// triggerOverflowReconcile schedules a single coalesced full-tree
// reconcile in response to a lost-event signal (a kernel inotify queue
// overflow or a backpressure-dropped event). A burst of signals
// collapses into at most one reconcile in flight: the first caller sets
// reconcilePending and runs the reconcile off the event loop; concurrent
// callers observe the flag and return immediately. Best-effort and
// logged — the event loop is never blocked.
func (w *Watcher) triggerOverflowReconcile(reason string) {
	w.reconcileMu.Lock()
	if w.reconcilePending {
		w.reconcileMu.Unlock()
		return
	}
	w.reconcilePending = true
	fn := w.reconcileFn
	w.reconcileMu.Unlock()

	if w.logger != nil {
		w.logger.Warn("watcher: event signal lost — scheduling full-tree reconcile",
			zap.String("reason", reason),
			zap.String("root", w.indexer.rootPath))
	}

	go func() {
		defer func() {
			w.reconcileMu.Lock()
			w.reconcilePending = false
			w.reconcileMu.Unlock()
		}()
		defer w.guardWatcherPanic("overflow-reconcile")
		if fn != nil {
			fn()
			return
		}
		if _, err := w.indexer.IncrementalReindex(w.indexer.rootPath); err != nil {
			if w.logger != nil {
				w.logger.Warn("watcher: overflow reconcile failed",
					zap.String("reason", reason),
					zap.Error(err))
			}
		}
	}()
}

// dirScanEscalateCap bounds the scoped new-directory scan: a burst that
// creates more than this many directories (a large checkout or unpack)
// escalates to a single full-tree reconcile instead of fanning out into
// that many scoped subtree walks.
const dirScanEscalateCap = 64

// enqueueDirScan schedules a scoped re-index of a newly-created
// directory's subtree, closing the new-subdir race: on Linux inotify a
// file written into a directory before its watch attaches fires no
// event. A burst of directory creates coalesces into a single in-flight
// drainer (mirrors triggerOverflowReconcile) — the first caller starts
// the goroutine, concurrent callers add their directory to
// pendingScanDirs and return. The drainer loops until the set is empty,
// so a directory enqueued while a scan is in flight is still picked up;
// nothing is lost and there is no debounce-timing race.
func (w *Watcher) enqueueDirScan(dir string) {
	w.reconcileMu.Lock()
	if w.pendingScanDirs == nil {
		w.pendingScanDirs = make(map[string]struct{})
	}
	w.pendingScanDirs[dir] = struct{}{}
	if w.dirScanActive {
		w.reconcileMu.Unlock()
		return
	}
	w.dirScanActive = true
	w.reconcileMu.Unlock()

	go func() {
		for {
			w.reconcileMu.Lock()
			dirs := w.pendingScanDirs
			w.pendingScanDirs = nil
			if len(dirs) == 0 {
				w.dirScanActive = false
				w.reconcileMu.Unlock()
				return
			}
			fn := w.scanFn
			w.reconcileMu.Unlock()
			func() {
				defer w.guardWatcherPanic("dir-scan")
				w.runDirScan(dirs, fn)
			}()
		}
	}()
}

// runDirScan re-indexes the accumulated new directories. A large burst
// escalates to one full-tree reconcile (dirScanEscalateCap); otherwise
// the scoped subtrees are walked in a single IncrementalReindexPaths
// call, which IsStale-gates each file so already-current files cost only
// a stat. fn is the test seam.
func (w *Watcher) runDirScan(dirs map[string]struct{}, fn func(map[string]struct{})) {
	if fn != nil {
		fn(dirs)
		return
	}
	if len(dirs) > dirScanEscalateCap {
		if w.logger != nil {
			w.logger.Info("watcher: large new-directory burst — full-tree reconcile",
				zap.Int("dirs", len(dirs)), zap.String("root", w.indexer.rootPath))
		}
		if _, err := w.indexer.IncrementalReindex(w.indexer.rootPath); err != nil && w.logger != nil {
			w.logger.Warn("watcher: new-directory reconcile failed", zap.Error(err))
		}
		return
	}
	paths := make([]string, 0, len(dirs))
	for d := range dirs {
		paths = append(paths, d)
	}
	if _, err := w.indexer.IncrementalReindexPaths(w.indexer.rootPath, paths); err != nil && w.logger != nil {
		w.logger.Warn("watcher: new-directory scan failed",
			zap.Strings("dirs", paths), zap.Error(err))
	}
}

// hasEventType reports whether the aggregated event-type set contains want.
func hasEventType(types []fswatcher.EventType, want fswatcher.EventType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func (w *Watcher) handleEvent(event fswatcher.WatchEvent) {
	// Kernel queue overflow arrives as a pathless EventOverflow on the
	// Events channel: the Linux inotify and Windows backends emit it when
	// the kernel drops events and cannot tell us which paths were lost.
	// macOS FSEvents never emits it — the darwin backend absorbs
	// UserDropped/KernelDropped by re-scanning the affected subtree
	// internally — so this branch is effectively Linux/Windows-only. With
	// no path to re-index, trigger a coalesced full-tree reconcile and
	// stop; every path-based step below would misfire on the empty path.
	for _, t := range event.Types {
		if t == fswatcher.EventOverflow {
			w.triggerOverflowReconcile("queue-overflow")
			return
		}
	}

	path := normalizeEventPath(event.Path, w.indexer.rootPath)

	// Probe artifacts: sentinel files Start writes to confirm the
	// OS-level watch is actually active. Their create event signals
	// the registered waiter; their remove event (after Start removes
	// the file) is silently absorbed so it never reaches user-visible
	// event consumers.
	if strings.Contains(filepath.Base(path), probeMarker) {
		if v, loaded := w.probeWaiters.LoadAndDelete(path); loaded {
			if ch, ok := v.(chan struct{}); ok {
				close(ch)
			}
		}
		return
	}

	// Skip events from excluded paths. A single matcher call covers
	// what the old code split across inExcludedDir + isExcluded.
	if w.isExcluded(path) {
		return
	}

	kind := pickKind(event.Types)
	if kind == "" {
		return
	}

	// Directory events. fswatcher with WatchNested attaches the watch
	// for a new directory itself, so we never re-attach. But on Linux
	// inotify that watch lands only AFTER the directory's create event is
	// read, so a file written into the directory in that gap fires no
	// event and would stay invisible until the hourly janitor. When the
	// event carries a Create, scan the new directory's subtree on disk so
	// those pre-watch files are picked up regardless of whether an event
	// ever fired ("watch first, then scan": files created after the watch
	// fire normal events, files created before are caught by the scan,
	// and the overlap is at worst a redundant idempotent re-index). A dir
	// event without a Create — a bare mtime bump on an existing dir —
	// needs no scan: entry changes inside it fire their own file events.
	// Either way the directory event itself reaches no indexer logic.
	if kind == ChangeCreated || kind == ChangeModified {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if hasEventType(event.Types, fswatcher.EventCreate) {
				w.enqueueDirScan(path)
			}
			return
		}
	}

	// Only process files with a detectable language — an extension
	// the registry knows, or an unknown-extension script the shebang
	// fallback can place.
	if _, ok := w.indexer.effectiveLanguage(path, nil); !ok {
		// Still handle remove for previously indexed files.
		if kind != ChangeDeleted && kind != ChangeRenamed {
			return
		}
	}

	// Storm mode — if more than StormThreshold events arrived within
	// StormWindowMs, skip the per-file debounced path and accumulate
	// into a batch. The batch drains once StormQuietPeriodMs has
	// passed with no further events.
	if w.shouldEnterStorm() {
		w.recordInStorm(path, kind)
		return
	}

	// Debounce: reset or start timer for this file.
	w.mu.Lock()
	if timer, exists := w.pending[path]; exists {
		timer.Stop()
	}
	debounce := time.Duration(w.config.DebounceMs) * time.Millisecond
	w.pending[path] = time.AfterFunc(debounce, func() {
		// Clean up the pending entry even if the patch panics, then
		// recover so a fatal store error can't crash the daemon.
		defer func() {
			w.mu.Lock()
			delete(w.pending, path)
			w.mu.Unlock()
		}()
		defer w.guardWatcherPanic("patch " + path)
		w.patchGraph(path, kind)
	})
	w.mu.Unlock()
}

// shouldEnterStorm records the current event in the rate window and
// reports whether the watcher is over threshold. Returns false when
// storm mode is disabled (threshold <= 0). The returned-true path
// guarantees the caller will enqueue to the batch, so any single
// event that crosses the threshold is captured correctly.
func (w *Watcher) shouldEnterStorm() bool {
	if w.config.StormThreshold <= 0 {
		return false
	}
	now := time.Now()
	window := time.Duration(w.config.StormWindowMs) * time.Millisecond
	cutoff := now.Add(-window)

	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	// Already batching — stay in storm until the drain completes.
	if w.stormActive {
		return true
	}
	// Drop timestamps older than the window. The slice is append-only
	// so a linear scan from the front is the minimal thing that
	// works; the window is O(threshold) bounded in steady state.
	trimFrom := 0
	for i, t := range w.eventTimes {
		if t.After(cutoff) {
			trimFrom = i
			break
		}
		trimFrom = i + 1
	}
	if trimFrom > 0 {
		w.eventTimes = w.eventTimes[trimFrom:]
	}
	w.eventTimes = append(w.eventTimes, now)
	return len(w.eventTimes) > w.config.StormThreshold
}

// recordInStorm adds the event to the pending batch and resets the
// drain timer. Repeated create/modify collapse to a single patch; a
// later delete of the same path overwrites an earlier create so the
// drain does the right final thing (treats the path as deleted).
func (w *Watcher) recordInStorm(path string, kind ChangeKind) {
	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	w.stormActive = true
	// Cancel any pending per-file timers for this path — storm mode
	// takes over.
	w.mu.Lock()
	if timer, exists := w.pending[path]; exists {
		timer.Stop()
		delete(w.pending, path)
	}
	w.mu.Unlock()
	w.stormBatch[path] = kind

	quiet := time.Duration(w.config.StormQuietPeriodMs) * time.Millisecond
	if w.stormTimer != nil {
		w.stormTimer.Stop()
	}
	w.stormTimer = time.AfterFunc(quiet, w.drainStorm)
}

// drainStorm processes every path accumulated during the storm as a
// single batch: per-path evict/index with the resolver stage skipped,
// then one global ResolveAll at the end. Cuts a 500-file checkout
// from "resolver runs 500 times" to "resolver runs once."
func (w *Watcher) drainStorm() {
	defer w.guardWatcherPanic("storm-drain")
	w.stormMu.Lock()
	batch := w.stormBatch
	w.stormBatch = make(map[string]ChangeKind)
	w.eventTimes = nil
	w.stormActive = false
	drained := w.stormDrained
	w.stormMu.Unlock()

	if len(batch) == 0 {
		return
	}
	start := time.Now()
	w.logger.Info("watcher: storm drain starting", zap.Int("paths", len(batch)))

	for path, kind := range batch {
		w.patchGraphNoResolve(path, kind)
	}
	w.indexer.ResolveAll()

	w.logger.Info("watcher: storm drain complete",
		zap.Int("paths", len(batch)),
		zap.Duration("elapsed", time.Since(start)))
	if drained != nil {
		drained(len(batch))
	}
}

// patchGraphNoResolve is patchGraph for the batched path: same evict
// / index dispatch, but without per-file resolver work. The caller is
// responsible for running indexer.ResolveAll() after the batch.
func (w *Watcher) patchGraphNoResolve(path string, kind ChangeKind) {
	switch kind {
	case ChangeCreated, ChangeModified:
		if err := w.indexer.IndexFileNoResolve(path); err != nil {
			w.logger.Warn("storm: index file failed",
				zap.String("path", path), zap.Error(err))
		}
	case ChangeDeleted, ChangeRenamed:
		w.indexer.EvictFile(path)
	}
}

func (w *Watcher) patchGraph(path string, kind ChangeKind) {
	w.patchMu.Lock()
	defer w.patchMu.Unlock()
	start := time.Now()
	var nodesAdded, nodesRemoved, edgesAdded, edgesRemoved int

	// Compute the relative path for snapshotting old symbols. RelKey
	// folds it to the canonical key (slash form, Unicode NFC) so the
	// GetFileNodes / snapshotSymbols lookups below hit the same graph
	// key the indexer stored — a watcher event for a non-ASCII-named
	// file arrives in the filesystem's Unicode form (NFD on macOS),
	// which would otherwise miss an NFC-keyed node.
	relPath := path
	if w.indexer.rootPath != "" {
		relPath = w.indexer.RelKey(path)
	}

	switch kind {
	case ChangeCreated:
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("index file failed", zap.String("path", path), zap.Error(err))
			return
		}
		newSymbols := w.indexer.graph.GetFileNodes(relPath)
		nodesAdded = len(newSymbols)
		edgesAdded = w.countFileEdges(newSymbols)

		// Notify callback: no old symbols, only new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, nil, newSymbols)
		}

	case ChangeModified:
		// Snapshot old symbols before eviction.
		oldSymbols := w.snapshotSymbols(relPath)

		// Content-aware skip: if the saved file's structural symbols
		// are byte-for-byte identical to the ones already in the
		// graph, the change touched no Function / Type / Method /
		// etc. — a comment-only edit, a whitespace reflow, a doc
		// change, or a config / JSON value save. Re-indexing it would
		// evict and rebuild every node for no graph-level effect, so
		// skip the structural reindex entirely. The probe is
		// read-only; only on a proven match do we take the cheap
		// path. A probe that can't run (unknown language, over-cap
		// file, parser quarantine) returns ok == false and falls
		// through to the normal reindex.
		if newSymbols, ok := w.indexer.StructuralSymbols(path); ok &&
			structuralFingerprint(newSymbols) == structuralFingerprint(oldSymbols) {
			w.recordInertModify(path, relPath, oldSymbols, start)
			return
		}

		// Do NOT pre-evict. IndexFile parse-then-swaps internally: it
		// evicts the file's prior nodes and re-adds the new ones only on a
		// successful parse, and leaves the prior nodes intact on a parse
		// failure. Pre-evicting here was the node-loss bug — a transiently
		// unparseable save (mid-edit) dropped the file's symbols from the
		// graph until the next clean save. Capture the file's prior node
		// count first (still present pre-swap) so removed/added telemetry
		// stays gross: a rename removes one node and adds one even though
		// the net node delta is zero.
		priorNodes := w.indexer.graph.GetFileNodes(relPath)
		fileEdgesBefore := w.countFileEdges(priorNodes)
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("reindex file failed", zap.String("path", path), zap.Error(err))
			return
		}
		nodesRemoved = len(priorNodes)
		newSymbols := w.indexer.graph.GetFileNodes(relPath)
		nodesAdded = len(newSymbols)
		// Edge churn scoped to this file's nodes. A graph-wide
		// EdgeCount delta would also pick up edges landed by whatever
		// else mutates the graph during this patch (concurrent
		// reconciles, deferred passes), which made the edges+ figure
		// meaningless noise on a busy daemon.
		if fileEdgesAfter := w.countFileEdges(newSymbols); fileEdgesAfter >= fileEdgesBefore {
			edgesAdded = fileEdgesAfter - fileEdgesBefore
		} else {
			edgesRemoved = fileEdgesBefore - fileEdgesAfter
		}

		// Notify callback with old and new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, oldSymbols, newSymbols)
		}

	case ChangeDeleted, ChangeRenamed:
		// Snapshot old symbols before eviction.
		oldSymbols := w.snapshotSymbols(relPath)

		nr, er := w.indexer.EvictFile(path)
		nodesRemoved = nr
		edgesRemoved = er

		// Notify callback: old symbols removed, no new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, oldSymbols, nil)
		}
	}

	ev := GraphChangeEvent{
		FilePath:     path,
		Kind:         kind,
		NodesAdded:   nodesAdded,
		NodesRemoved: nodesRemoved,
		EdgesAdded:   edgesAdded,
		EdgesRemoved: edgesRemoved,
		Timestamp:    time.Now(),
		DurationMs:   time.Since(start).Milliseconds(),
	}

	// Rebuild the reachability index so AnalyzeImpact /
	// explain_change_impact stay correct against the patched topology.
	// Lazy reach: instead of eagerly recomputing every seed's reach
	// after a watcher-driven patch (the old reach.BuildIndex call
	// here paid the full O(seeds) cost on every file edit), we just
	// invalidate the build counter so subsequent AnalyzeImpact calls
	// recompute on demand against the fresh graph. No-op patches
	// (nodesAdded == 0 && nodesRemoved == 0 && edgesAdded == 0 &&
	// edgesRemoved == 0) leave the counter alone so existing caches
	// stay valid.
	if nodesAdded+nodesRemoved+edgesAdded+edgesRemoved > 0 {
		reach.InvalidateIndex()
	}

	w.historyMu.Lock()
	w.history = append(w.history, ev)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	w.historyMu.Unlock()

	// Non-blocking send.
	select {
	case w.events <- ev:
	default:
	}

	w.logger.Info("graph patch",
		zap.String("kind", string(kind)),
		zap.String("file", path),
		zap.Int("nodes+", nodesAdded),
		zap.Int("nodes-", nodesRemoved),
		zap.Int("edges+", edgesAdded),
		zap.Int("edges-", edgesRemoved),
		zap.Int64("ms", ev.DurationMs),
	)
}

// countFileEdges counts the edges incident to the given file nodes:
// every out-edge plus the in-edges that originate outside the file
// (an intra-file edge is already counted on its From side). Batched
// so a disk backend pays two bulk lookups instead of 2N point queries.
func (w *Watcher) countFileEdges(nodes []*graph.Node) int {
	if len(nodes) == 0 {
		return 0
	}
	ids := make([]string, 0, len(nodes))
	inFile := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
		inFile[n.ID] = struct{}{}
	}
	total := 0
	for _, edges := range w.indexer.graph.GetOutEdgesByNodeIDs(ids) {
		total += len(edges)
	}
	for _, edges := range w.indexer.graph.GetInEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if _, ok := inFile[e.From]; !ok {
				total++
			}
		}
	}
	return total
}

// recordInertModify finishes a ChangeModified patch that the
// content-aware skip proved structurally inert. The graph already
// holds the correct symbols, so the destructive evict + reindex is
// skipped; this records the bookkeeping the skipped path would
// otherwise have produced:
//
//   - the indexer's recorded mtime is restamped so the adaptive
//     poller's mtime sweep does not keep re-flagging the file;
//   - a zero-delta GraphChangeEvent is appended to history and
//     published, so get_recent_changes still shows the save (with
//     all node/edge counts zero — nothing structural moved);
//   - the symbol-change callback fires with the unchanged symbol set
//     on both sides, mirroring the no-op so consumers see a
//     consistent before == after.
//
// The reachability index is intentionally not rebuilt — the topology
// did not change, so the existing reach stamps stay valid.
func (w *Watcher) recordInertModify(path, relPath string, symbols []*graph.Node, start time.Time) {
	// Advance the recorded mtime past this save so the poller does
	// not treat the (untouched) file as perpetually stale.
	w.indexer.RefreshFileMtime(path)

	ev := GraphChangeEvent{
		FilePath:   path,
		Kind:       ChangeModified,
		Timestamp:  time.Now(),
		DurationMs: time.Since(start).Milliseconds(),
	}

	w.historyMu.Lock()
	w.history = append(w.history, ev)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	w.historyMu.Unlock()

	select {
	case w.events <- ev:
	default:
	}

	w.symbolChangeCbMu.RLock()
	cb := w.symbolChangeCb
	w.symbolChangeCbMu.RUnlock()
	if cb != nil {
		cb(relPath, symbols, symbols)
	}

	w.logger.Info("graph patch skipped: structurally inert change",
		zap.String("file", path),
		zap.Int64("ms", ev.DurationMs),
	)
}

// snapshotSymbols returns a deep copy of the symbols for a file, preserving
// their signatures in Meta so they can be compared after re-indexing.
func (w *Watcher) snapshotSymbols(relPath string) []*graph.Node {
	nodes := w.indexer.graph.GetFileNodes(relPath)
	snapshot := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		// Skip file and import nodes — we only track code symbols.
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		cp := &graph.Node{
			ID:       n.ID,
			Kind:     n.Kind,
			Name:     n.Name,
			QualName: n.QualName,
			FilePath: n.FilePath,
		}
		if sig, ok := n.Meta["signature"]; ok {
			cp.Meta = map[string]any{"signature": sig}
		}
		snapshot = append(snapshot, cp)
	}
	return snapshot
}

// structuralFingerprint reduces a set of symbols to an order-independent
// string identity built only from each structural symbol's kind, name,
// qualified name, and signature — never its line range. Two snapshots
// of the same file taken before and after an edit produce an equal
// fingerprint exactly when the edit changed no structural symbol: a
// comment-only change, a whitespace reflow, or a doc / config-value
// edit shifts line numbers but leaves every (kind, name, sig) tuple
// intact, while renaming a function, changing a signature, or
// adding / removing a declaration changes the fingerprint.
//
// Non-structural nodes (file, import, params, closures, coverage-domain
// kinds) are skipped so a change confined to them is still treated as
// inert — they carry no structural graph shape.
func structuralFingerprint(symbols []*graph.Node) string {
	lines := make([]string, 0, len(symbols))
	for _, n := range symbols {
		if !isStructuralKind(n.Kind) {
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		// NUL separates fields so a value containing the field
		// delimiter can't forge a collision across two symbols.
		lines = append(lines, string(n.Kind)+"\x00"+n.Name+"\x00"+n.QualName+"\x00"+sig)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// normalizeEventPath aligns an event path emitted by the OS-level
// backend with the form the indexer stored when it walked the tree.
//
// Two macOS-specific corrections are applied:
//
//   - /private/ symlink resolution: FSEvents reports paths under
//     /private/var/... and /private/tmp/... even when the watcher was
//     registered with /var/... or /tmp/... — those are real
//     /private/-rooted symlinks. The indexer keyed its symbols by the
//     user-facing form, so without this we'd fail to find any symbols
//     to evict on modify or delete.
//
//   - Unicode NFC folding: APFS / HFS+ hand back filenames in
//     decomposed NFD form, so a watcher event for a non-ASCII-named
//     file carries different bytes than the same file does in `git
//     diff` output or on a Linux checkout. Folding the path to NFC
//     here means every consumer downstream — the exclude matcher, the
//     storm batch, the per-file debounce map — sees one stable form.
//     IndexFile / EvictFile fold again at their own boundary, so this
//     is belt-and-braces, but it also keeps the debounce/batch maps
//     (keyed on this path directly) free of accidental NFD/NFC
//     duplicates for the same file.
func normalizeEventPath(path, rootPath string) string {
	path = pathkey.Normalize(path)
	if runtime.GOOS != "darwin" {
		return path
	}
	if !strings.HasPrefix(path, "/private/") {
		return path
	}
	// Without a rootPath we have no way to know which form (the
	// /private/-prefixed canonical or the symlink form) the rest of
	// the daemon expects, so leave it alone.
	if rootPath == "" || strings.HasPrefix(rootPath, "/private/") {
		return path
	}
	stripped := path[len("/private"):]
	if !strings.HasPrefix(stripped, rootPath) {
		// Different prefix entirely — leave the canonical form alone.
		return path
	}
	return stripped
}

// pickKind reduces the aggregated event-type set from fswatcher to a
// single ChangeKind. Priority: Remove > Rename > Modify > Create.
// Modify outranks Create because FSEvents flags are cumulative — a
// write to an existing file fires with both Create and Modify set,
// and treating that as "created" loses the old-symbols snapshot the
// modify path produces. An event with only types we don't act on
// (e.g. chmod alone) returns "".
func pickKind(types []fswatcher.EventType) ChangeKind {
	var hasCreate, hasModify, hasRemove, hasRename bool
	for _, t := range types {
		switch t {
		case fswatcher.EventCreate:
			hasCreate = true
		case fswatcher.EventMod:
			hasModify = true
		case fswatcher.EventRemove:
			hasRemove = true
		case fswatcher.EventRename:
			hasRename = true
		}
	}
	switch {
	case hasRemove:
		return ChangeDeleted
	case hasRename:
		return ChangeRenamed
	case hasModify:
		return ChangeModified
	case hasCreate:
		return ChangeCreated
	}
	return ""
}

// isExcluded reports whether path is excluded by the effective pattern list.
func (w *Watcher) isExcluded(path string) bool {
	root := w.indexer.rootPath
	if root == "" {
		return w.excludes.MatchRel(filepath.Base(path))
	}
	return w.excludes.MatchAbs(path, root)
}
