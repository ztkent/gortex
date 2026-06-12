package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
	"go.uber.org/zap"
)

// Poller is a timer-driven fallback that complements the fsnotify
// per-file Watcher and the GitWatcher. fsnotify is not reliable
// everywhere: the Linux inotify backend silently stops delivering
// events once `max_user_watches` is exhausted on a huge tree, network
// filesystems (NFS, SMB, virtiofs) deliver events late or not at all,
// and any backend can drop events under load. The poller closes that
// gap by periodically re-checking two cheap signals — git HEAD
// movement and tracked-file mtimes — and dispatching the work the
// missed event would have triggered.
//
// The poll interval is adaptive: it scales with the size of the
// repository so a small project is checked often (changes land fast)
// while a large project is checked rarely (a sweep over thousands of
// tracked files is not free, and large repos should not be hammered).
// Project size is read from a real signal — the indexed node count of
// the repo's graph — and the resulting interval is clamped to a
// sensible [min, max] band.
type Poller struct {
	watcher  *Watcher
	indexer  *Indexer
	rootPath string
	logger   *zap.Logger

	interval time.Duration
	done     chan struct{}
	stopped  chan struct{}

	mu          sync.Mutex
	lastSHA     string
	loopStarted bool
	stopCalled  bool

	// swept is a test hook fired after every poll cycle with the
	// number of files the cycle re-dispatched. nil in production.
	swept func(int)
}

// Poll-interval bounds. A small repo polls near the floor; a large
// repo polls near the ceiling. The band is wide enough that the
// fallback never becomes a hot loop on a tiny repo, nor a no-op on a
// huge one.
const (
	pollIntervalMin = 15 * time.Second
	pollIntervalMax = 10 * time.Minute

	// pollNodesPerStep is the indexed-node count that maps to one
	// pollIntervalMin step of additional interval. A repo of ~2k
	// nodes adds one floor-width step, ~4k adds two, and so on,
	// until the interval saturates at pollIntervalMax. Chosen so a
	// typical small service (a few hundred nodes) sits at the floor
	// and a large monorepo (tens of thousands of nodes) sits at the
	// ceiling.
	pollNodesPerStep = 2000
)

// pollInterval derives an adaptive poll interval from a project-size
// signal — the indexed node count. The mapping is linear in the node
// count and clamped to [pollIntervalMin, pollIntervalMax]: an empty
// or tiny repo polls at the floor, and the interval grows by one
// floor-width step per pollNodesPerStep nodes until it saturates at
// the ceiling. A non-positive node count (a repo indexed to nothing,
// or a missing graph) falls back to the floor rather than to zero.
func pollInterval(nodeCount int) time.Duration {
	if nodeCount <= 0 {
		return pollIntervalMin
	}
	steps := nodeCount / pollNodesPerStep
	d := pollIntervalMin + time.Duration(steps)*pollIntervalMin
	if d < pollIntervalMin {
		return pollIntervalMin
	}
	if d > pollIntervalMax {
		return pollIntervalMax
	}
	return d
}

// newPoller builds an adaptive-interval poller for a Watcher. The
// interval is computed once at construction from the indexer's
// current graph size; it is stable for the poller's lifetime, which
// matches how the rest of the watcher subsystem treats per-repo
// tuning (set at start, not re-derived per tick).
func newPoller(w *Watcher, idx *Indexer, logger *zap.Logger) *Poller {
	root := ""
	if idx != nil {
		root = idx.rootPath
	}
	nodeCount := 0
	if idx != nil && idx.graph != nil {
		nodeCount = idx.graph.NodeCount()
	}
	// Capture the baseline HEAD SHA at construction so the very first
	// poll cycle can already detect a branch switch that happened
	// between newPoller and the first tick. A repo with no .git
	// directory yields an empty SHA and the HEAD check stays a no-op.
	lastSHA := ""
	if root != "" {
		lastSHA, _ = pollerHeadSHA(root)
	}
	return &Poller{
		watcher:  w,
		indexer:  idx,
		rootPath: root,
		logger:   logger,
		interval: pollInterval(nodeCount),
		lastSHA:  lastSHA,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start launches the polling goroutine. Safe to call once per Poller
// instance. A poller with no usable indexer or root path is inert —
// Start records the no-op and returns without launching the loop, and
// Stop stays safe to call.
func (p *Poller) Start() {
	if p.indexer == nil || p.rootPath == "" {
		return
	}
	p.mu.Lock()
	p.loopStarted = true
	p.mu.Unlock()
	go p.loop()
}

// Stop halts the poller. Idempotent — safe whether Start launched the
// loop or was a no-op. The wait on `stopped` only happens when the
// loop goroutine is actually running, so Stop never deadlocks on a
// channel nobody will close.
func (p *Poller) Stop() {
	p.mu.Lock()
	started := p.loopStarted
	already := p.stopCalled
	p.stopCalled = true
	p.mu.Unlock()
	if already {
		return
	}
	close(p.done)
	if started {
		<-p.stopped
	}
}

func (p *Poller) loop() {
	defer close(p.stopped)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	if p.logger != nil {
		p.logger.Debug("watcher: adaptive poller running",
			zap.String("root", p.rootPath),
			zap.Duration("interval", p.interval))
	}
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.poll()
		}
	}
}

// poll runs one fallback cycle: detect git HEAD movement, then scan
// tracked-file mtimes for changes the fsnotify backend may have
// missed, dispatching each through the same per-file patch path the
// live watcher uses. Best-effort throughout — a failed git call or an
// unreadable file is logged and skipped, never fatal.
func (p *Poller) poll() {
	swept := 0
	if p.pollGitHead() {
		swept++
	}
	swept += p.pollFilesystem()
	if p.swept != nil {
		p.swept(swept)
	}
}

// pollGitHead checks whether HEAD has moved since the last cycle. A
// moved HEAD is the branch-switch / commit signal the GitWatcher's
// fsnotify watch normally catches; the poller is the backstop for the
// case where that watch missed the ref-file event. It dispatches the
// reconcile through the indexer's existing per-file batch path by
// re-indexing every changed path, mirroring GitWatcher.reconcile.
// Returns true when a move was observed and reconciled.
func (p *Poller) pollGitHead() bool {
	newSHA, err := pollerHeadSHA(p.rootPath)
	if err != nil || newSHA == "" {
		return false
	}
	p.mu.Lock()
	oldSHA := p.lastSHA
	p.mu.Unlock()
	if oldSHA == "" {
		// First observation: seed lastSHA and don't diff against a
		// phantom range. There is no prior commit to reconcile from.
		p.mu.Lock()
		p.lastSHA = newSHA
		p.mu.Unlock()
		return false
	}
	if oldSHA == newSHA {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	changes, err := pollerDiffNameStatus(ctx, p.rootPath, oldSHA, newSHA)
	if err != nil {
		// Leave lastSHA at oldSHA so the next cycle retries this exact
		// range. Advancing it here would permanently skip the
		// un-reconciled oldSHA..newSHA span on a transient diff failure.
		if p.logger != nil {
			p.logger.Debug("watcher: poller git diff failed",
				zap.String("from", oldSHA), zap.String("to", newSHA),
				zap.Error(err))
		}
		return false
	}

	// Diff succeeded — the range is now safe to mark reconciled. Advance
	// lastSHA before dispatching so a concurrent poll doesn't re-diff the
	// same span; dispatch failures of individual files are best-effort
	// and don't warrant re-running the whole diff.
	p.mu.Lock()
	p.lastSHA = newSHA
	p.mu.Unlock()

	n := 0
	for _, c := range changes {
		switch c.Status {
		case 'A', 'M', 'T', 'R', 'C':
			abs := filepath.Join(p.rootPath, c.Path)
			if _, statErr := os.Stat(abs); statErr != nil {
				continue
			}
			p.watcher.patchGraph(abs, ChangeModified)
			n++
		case 'D':
			abs := filepath.Join(p.rootPath, c.Path)
			if _, statErr := os.Stat(abs); statErr != nil {
				p.watcher.patchGraph(abs, ChangeDeleted)
				n++
			}
		}
	}
	if p.logger != nil {
		p.logger.Info("watcher: poller reconciled missed ref change",
			zap.String("from", oldSHA[:min(len(oldSHA), 12)]),
			zap.String("to", newSHA[:min(len(newSHA), 12)]),
			zap.Int("paths", n))
	}
	return n > 0
}

// pollFilesystem walks the indexer's per-file mtime map and re-indexes
// any tracked file whose on-disk mtime advanced past the recorded
// value — the modification the fsnotify backend should have reported.
// It also evicts files that have vanished from disk. The mtime map is
// the indexer's own bookkeeping (it stamps every file it indexes), so
// this reuses an existing source of truth instead of re-walking the
// tree. Returns the number of files re-dispatched.
func (p *Poller) pollFilesystem() int {
	snapshot := p.indexer.FileMtimes()
	if len(snapshot) == 0 {
		return 0
	}
	n := 0
	for relPath, recorded := range snapshot {
		abs := filepath.Join(p.rootPath, filepath.FromSlash(relPath))
		info, err := os.Stat(abs)
		if err != nil {
			// The file is gone — the delete event was missed.
			p.watcher.patchGraph(abs, ChangeDeleted)
			n++
			continue
		}
		if info.IsDir() {
			continue
		}
		if info.ModTime().UnixNano() > recorded {
			// The file changed on disk after we last indexed it —
			// the modify event was missed.
			p.watcher.patchGraph(abs, ChangeModified)
			n++
		}
	}
	if n > 0 && p.logger != nil {
		p.logger.Info("watcher: poller re-indexed files missed by fsnotify",
			zap.Int("paths", n))
	}
	return n
}

// pollerHeadSHA resolves the current HEAD commit SHA of a worktree.
// Shells out to git so symbolic refs, packed-refs, and worktree
// indirection resolve without re-implementing git ref logic. A repo
// with no .git directory yields an empty string and no error from the
// caller's perspective — the poller simply skips the HEAD check.
func pollerHeadSHA(repoPath string) (string, error) {
	return gitcmd.Output(context.Background(), repoPath, "rev-parse", "HEAD")
}

// pollerDiffNameStatus runs `git diff --name-status -M -C -z` between
// two commits and decodes the result, reusing the GitWatcher's
// NUL-delimited parser so rename / copy detection behaves identically
// on the fallback path.
func pollerDiffNameStatus(ctx context.Context, repoPath, oldSHA, newSHA string) ([]gitChange, error) {
	out, err := gitcmd.Run(ctx, repoPath,
		"diff", "--name-status", "-M", "-C", "-z", oldSHA, newSHA)
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatus(out), nil
}
