package indexer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// GitWatcher observes `.git/HEAD` (and the target ref) for a single
// tracked repository. When the resolved commit moves, it computes the
// diff between the old and new commits and dispatches EvictFile /
// IndexFileNoResolve per changed path, running the resolver once at
// the end. This is the branch-switch / rebase / reset path: the
// regular fsnotify per-file watcher sees 500 Remove+Create events for
// a checkout and walks each through per-file resolve + search, which
// is measurably slow and incorrect for renames (git sees them as
// atomic; fsnotify sees unrelated remove+create pairs).
//
// Gitwatcher complements the file watcher — it does not replace it.
// File edits outside git operations (save buffer, `git stash`,
// `git reset --mixed` leaving the working tree dirty) still fire
// normal fsnotify events and flow through the per-file path.
type GitWatcher struct {
	repoPath    string
	indexer     *Indexer
	logger      *zap.Logger
	fsw         *fsnotify.Watcher
	debounce    time.Duration
	done        chan struct{}
	stopped     chan struct{}
	mu          sync.Mutex
	lastSHA     string
	fireTimer   *time.Timer
	loopStarted bool
	stopCalled  bool
	// drained is a test hook that fires after a reconcile completes
	// with the number of files patched. nil in production.
	drained func(int)
}

// NewGitWatcher creates a watcher for repoPath/.git/HEAD. repoPath is
// the absolute path to the worktree root; the .git dir is discovered
// by looking at the HEAD file (handles worktrees, submodules via the
// `gitdir:` indirection).
func NewGitWatcher(repoPath string, idx *Indexer, logger *zap.Logger) (*GitWatcher, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &GitWatcher{
		repoPath: absRepo,
		indexer:  idx,
		logger:   logger,
		fsw:      fsw,
		debounce: 300 * time.Millisecond,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}, nil
}

// Start sets up fsnotify watches on the repo's git control files and
// launches the event-processing goroutine. Safe to call once per
// GitWatcher instance. Returns an error (and does not launch the loop)
// when the repo has no .git directory — Stop remains safe to call.
func (gw *GitWatcher) Start() error {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return fmt.Errorf("resolve .git dir for %s: %w", gw.repoPath, err)
	}
	// HEAD + refs/heads/ cover branch switches and same-branch
	// commits; packed-refs covers the gc case where loose refs get
	// packed and moved out of refs/heads. Missing files are not fatal
	// — a fresh repo may not have packed-refs yet.
	for _, rel := range []string{"HEAD", "packed-refs", "refs/heads"} {
		path := filepath.Join(gitDir, rel)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := gw.fsw.Add(path); err != nil {
			gw.logger.Warn("git-watcher: failed to watch",
				zap.String("path", path), zap.Error(err))
		}
	}

	gw.lastSHA, _ = gw.currentSHA(context.Background())
	gw.mu.Lock()
	gw.loopStarted = true
	gw.mu.Unlock()
	go gw.loop()
	return nil
}

// Stop halts the watcher. Idempotent — safe whether Start succeeded,
// failed, or was never called. We only block on `stopped` when the
// loop goroutine is actually running; otherwise Stop would deadlock
// on a channel nobody's going to close.
func (gw *GitWatcher) Stop() error {
	gw.mu.Lock()
	started := gw.loopStarted
	already := gw.stopCalled
	gw.stopCalled = true
	gw.mu.Unlock()
	if already {
		return nil
	}
	close(gw.done)
	_ = gw.fsw.Close()
	if started {
		<-gw.stopped
	}
	return nil
}

func (gw *GitWatcher) loop() {
	defer close(gw.stopped)
	for {
		select {
		case <-gw.done:
			return
		case event, ok := <-gw.fsw.Events:
			if !ok {
				return
			}
			gw.scheduleReconcile(event.Name)
		case err, ok := <-gw.fsw.Errors:
			if !ok {
				return
			}
			gw.logger.Warn("git-watcher: fsnotify error", zap.Error(err))
		}
	}
}

// scheduleReconcile coalesces bursts of ref-file events (a branch
// switch touches HEAD, refs/heads/<new>, and often packed-refs in
// rapid succession) into a single reconcile. Resets the debounce
// timer on every event and lets the last one win.
func (gw *GitWatcher) scheduleReconcile(trigger string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.fireTimer != nil {
		gw.fireTimer.Stop()
	}
	gw.fireTimer = time.AfterFunc(gw.debounce, func() {
		gw.reconcile(trigger)
	})
}

// reconcile reads the current HEAD SHA, diffs against the previously
// seen SHA via `git diff --name-status`, and dispatches
// EvictFile / IndexFileNoResolve per path. Runs ResolveAll once at
// the end. Silently no-ops when HEAD hasn't moved — branches can
// touch packed-refs without the resolved commit actually changing.
func (gw *GitWatcher) reconcile(trigger string) {
	if gw.rebaseInProgress() {
		// Defer until the rebase lands. The final rebase state
		// either updates HEAD (triggering another fsnotify fire) or
		// leaves the branch where it was — in both cases we'll pick
		// up the right end state on the next event.
		gw.logger.Debug("git-watcher: rebase in progress, deferring",
			zap.String("trigger", trigger))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	newSHA, err := gw.currentSHA(ctx)
	if err != nil || newSHA == "" {
		return
	}

	gw.mu.Lock()
	oldSHA := gw.lastSHA
	gw.mu.Unlock()
	if oldSHA == newSHA {
		return
	}

	// First observation (no prior SHA) — just record it without
	// diffing. The caller warmed up with a full index already.
	if oldSHA == "" {
		gw.mu.Lock()
		gw.lastSHA = newSHA
		gw.mu.Unlock()
		return
	}

	start := time.Now()
	changes, err := gw.diffNameStatus(ctx, oldSHA, newSHA)
	if err != nil {
		gw.logger.Warn("git-watcher: diff failed",
			zap.String("from", oldSHA), zap.String("to", newSHA), zap.Error(err))
		return
	}

	patched := gw.applyChanges(changes)

	// Re-run the global resolver once after the batch. Skipping the
	// per-file resolver during applyChanges is what makes this fast
	// on 500-file branch switches.
	gw.indexer.ResolveAll()

	gw.mu.Lock()
	gw.lastSHA = newSHA
	drained := gw.drained
	gw.mu.Unlock()

	gw.logger.Info("git-watcher: reconciled ref change",
		zap.String("from", oldSHA[:min(len(oldSHA), 12)]),
		zap.String("to", newSHA[:min(len(newSHA), 12)]),
		zap.Int("paths", patched),
		zap.Duration("elapsed", time.Since(start)))

	if drained != nil {
		drained(patched)
	}
}

// gitChange describes one entry in `git diff --name-status` output.
// Status is a single char (A/M/D/T) or R/C with a similarity score.
type gitChange struct {
	Status  byte
	Path    string
	OldPath string // only populated for R/C
}

// applyChanges dispatches the decoded diff to the indexer. Returns
// the count of files touched (index + evict).
func (gw *GitWatcher) applyChanges(changes []gitChange) int {
	n := 0
	for _, c := range changes {
		switch c.Status {
		case 'A', 'M', 'T':
			abs := filepath.Join(gw.repoPath, c.Path)
			if _, err := os.Stat(abs); err != nil {
				// Git says it's here, working tree disagrees —
				// partial / sparse checkout. Skip without error.
				continue
			}
			if err := gw.indexer.IndexFileNoResolve(abs); err != nil {
				gw.logger.Warn("git-watcher: index failed",
					zap.String("path", c.Path), zap.Error(err))
				continue
			}
			n++
		case 'D':
			// A 'D' in the diff means the file left git tracking, not
			// necessarily the disk. A file un-tracked (git rm --cached)
			// or absent from the new branch but kept on disk is
			// "untracked but visible" and must stay indexed. Only a
			// file genuinely gone from disk is evicted; one still on
			// disk is re-indexed unless it is now excluded.
			abs := filepath.Join(gw.repoPath, c.Path)
			_, statErr := os.Stat(abs)
			switch {
			case statErr != nil:
				gw.indexer.EvictFile(abs)
			case gw.indexer.shouldExclude(abs, gw.repoPath, false):
				gw.indexer.EvictFile(abs)
			default:
				if err := gw.indexer.IndexFileNoResolve(abs); err != nil {
					gw.logger.Warn("git-watcher: re-index of now-untracked file failed",
						zap.String("path", c.Path), zap.Error(err))
				}
			}
			n++
		case 'R':
			// Rename: evict the old path, index the new one. Git
			// gives us the pair atomically — fsnotify would have
			// surfaced this as a remove + create with no linkage.
			if c.OldPath != "" {
				gw.indexer.EvictFile(filepath.Join(gw.repoPath, c.OldPath))
			}
			abs := filepath.Join(gw.repoPath, c.Path)
			if _, err := os.Stat(abs); err == nil {
				if err := gw.indexer.IndexFileNoResolve(abs); err != nil {
					gw.logger.Warn("git-watcher: index failed (rename)",
						zap.String("path", c.Path), zap.Error(err))
					continue
				}
			}
			n++
		case 'C':
			// Copy: the source is unchanged; we only need to index
			// the new file.
			abs := filepath.Join(gw.repoPath, c.Path)
			if _, err := os.Stat(abs); err == nil {
				if err := gw.indexer.IndexFileNoResolve(abs); err != nil {
					gw.logger.Warn("git-watcher: index failed (copy)",
						zap.String("path", c.Path), zap.Error(err))
					continue
				}
			}
			n++
		}
	}
	return n
}

// currentSHA returns the resolved commit SHA of HEAD. Shells out to
// git rather than parsing .git/HEAD directly so symbolic refs,
// packed-refs, and worktree indirection all work without us
// reimplementing git's ref resolution.
func (gw *GitWatcher) currentSHA(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gw.repoPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// diffNameStatus shells out to `git diff --name-status -M -C oldSHA..newSHA`
// and decodes the output into gitChange records. -M enables rename
// detection, -C enables copy detection.
func (gw *GitWatcher) diffNameStatus(ctx context.Context, oldSHA, newSHA string) ([]gitChange, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gw.repoPath,
		"diff", "--name-status", "-M", "-C", "-z", oldSHA, newSHA)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatus(out), nil
}

// parseDiffNameStatus decodes the `-z` NUL-delimited output of
// `git diff --name-status`. Each entry is: STATUS\0path[\0newpath].
// Rename (R) and copy (C) statuses carry a similarity score appended
// to the letter (e.g., "R100") and come with two paths separated by
// a NUL. Everything else is a single path.
func parseDiffNameStatus(out []byte) []gitChange {
	var changes []gitChange
	// bufio.Scanner with a NUL split function gives us one token per
	// field; we consume them in pairs (or triples for R/C).
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanner.Split(scanNul)

	for scanner.Scan() {
		status := scanner.Text()
		if status == "" {
			continue
		}
		letter := status[0]
		if !scanner.Scan() {
			break
		}
		path := scanner.Text()
		c := gitChange{Status: letter, Path: path}
		if letter == 'R' || letter == 'C' {
			if !scanner.Scan() {
				break
			}
			c.OldPath = path
			c.Path = scanner.Text()
		}
		changes = append(changes, c)
	}
	return changes
}

// scanNul is a bufio.SplitFunc that tokenises on NUL bytes. Used for
// `git diff -z` output where paths may contain whitespace.
func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// rebaseInProgress reports whether a rebase, merge, or cherry-pick is
// currently in flight — any of which touch HEAD/refs rapidly and
// produce intermediate states we don't want to reconcile against.
// Detection is by sentinel file presence in .git (the canonical way
// other git tooling does it).
func (gw *GitWatcher) rebaseInProgress() bool {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return false
	}
	for _, sentinel := range []string{"rebase-merge", "rebase-apply",
		"MERGE_HEAD", "CHERRY_PICK_HEAD", "BISECT_LOG"} {
		if _, err := os.Stat(filepath.Join(gitDir, sentinel)); err == nil {
			return true
		}
	}
	return false
}

// resolveGitDir returns the absolute path to the .git directory for a
// worktree. Handles the worktree / submodule case where .git is a
// file containing `gitdir: <path>` instead of a directory.
func resolveGitDir(repoPath string) (string, error) {
	candidate := filepath.Join(repoPath, ".git")
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return candidate, nil
	}
	// .git is a file pointing at the real gitdir — common for
	// worktrees (under modules/<name>) and submodules.
	content, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(content))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("malformed .git file: %s", candidate)
	}
	dir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir, nil
}
