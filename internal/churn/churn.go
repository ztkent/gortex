// Package churn computes per-symbol and per-file commit density from
// the git log of a chosen branch (typically the default branch) and
// persists the result on graph nodes. Once enriched, the MCP tool
// get_churn_rate is a pure graph scan — no `git` subprocess at read
// time. The graph store is the source of truth; the disk-backed
// LadyBug backend keeps the data across daemon restarts, while
// in-memory backends recompute on demand.
//
// Design notes:
//
//   - We blame at an explicit rev (the default branch) rather than
//     HEAD. Feature-branch work-in-progress doesn't pollute the
//     persisted churn signal — the data answers "what's churning on
//     main" regardless of where the agent is checked out.
//
//   - Per-file blame is invoked once and projected onto every symbol
//     in the file. The repo walk inside `git blame` dominates the
//     cost; per-symbol invocations would multiply it by the symbol
//     count.
//
//   - After mutating n.Meta we re-call g.AddNode(n). The in-memory
//     store treats this as a no-op (the pointer is already in the
//     graph); the LadyBug backend treats it as an UPSERT that
//     re-serialises Meta to its on-disk row. This is the only path
//     that persists Meta mutations into LadyBug — without it the
//     enrichment would be invisible on the next daemon restart.
package churn

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/graph"
)

// Options controls how the enricher resolves and persists churn data.
type Options struct {
	// Branch is the rev to blame and log. Required — call site is
	// expected to resolve the repo's default branch (origin/main,
	// origin/master, …) and pass it in. We do not default to HEAD
	// because the whole point of pre-computation is to pin the
	// signal to a stable branch.
	Branch string
	// Now lets tests fix the clock for deterministic age_days. When
	// zero, time.Now() is used.
	Now time.Time
}

// Result summarises an enrichment pass.
type Result struct {
	Files   int    // file nodes stamped with a churn summary
	Symbols int    // function/method nodes stamped with per-symbol churn
	Branch  string // the rev used (echoed back for the CLI)
	HeadSHA string // the resolved SHA at enrich time (stored on each file)
}

// EnrichGraph computes per-symbol and per-file churn and stamps the
// data on graph nodes. Returns counts plus the resolved SHA. Errors
// only when the repo can't be opened or the branch can't be resolved
// at all; per-file failures are best-effort and skip that file.
//
// Persistence: every mutated node is re-upserted via g.AddNode(n).
// On LadyBug-backed stores this round-trips through the Cypher MERGE
// path; on the in-memory store the pointer was already mutated in
// place, but the redundant AddNode call keeps the semantics uniform
// between backends and lets the enricher run against either.
func EnrichGraph(ctx context.Context, g graph.Store, repoRoot string, opts Options) (Result, error) {
	if g == nil || repoRoot == "" {
		return Result{}, fmt.Errorf("churn: graph and repoRoot are required")
	}
	if strings.TrimSpace(opts.Branch) == "" {
		return Result{}, fmt.Errorf("churn: Options.Branch is required (default-branch resolution belongs to the caller)")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	headSHA := runGit(repoRoot, "rev-parse", "--verify", "--quiet", opts.Branch)
	if headSHA == "" {
		return Result{}, fmt.Errorf("churn: branch %q does not resolve in %s", opts.Branch, repoRoot)
	}

	// Group symbols by file path. We deliberately keep file nodes in
	// a separate map so we can stamp their summary even when no
	// function/method is in scope (some files contain only types or
	// constants).
	type bucket struct {
		file    *graph.Node // optional — may be nil
		symbols []*graph.Node
	}
	byPath := map[string]*bucket{}
	for _, n := range g.AllNodes() {
		if n.FilePath == "" {
			continue
		}
		switch n.Kind {
		case graph.KindFile:
			b := byPath[n.FilePath]
			if b == nil {
				b = &bucket{}
				byPath[n.FilePath] = b
			}
			b.file = n
		case graph.KindFunction, graph.KindMethod:
			if n.StartLine == 0 {
				continue
			}
			b := byPath[n.FilePath]
			if b == nil {
				b = &bucket{}
				byPath[n.FilePath] = b
			}
			b.symbols = append(b.symbols, n)
		}
	}

	res := Result{Branch: opts.Branch, HeadSHA: headSHA}
	for filePath, b := range byPath {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if len(b.symbols) == 0 && b.file == nil {
			continue
		}
		rel := stripRepoPrefix(filePath, repoRoot)
		commits, err := fileCommits(repoRoot, opts.Branch, rel)
		if err != nil || len(commits) == 0 {
			continue
		}
		var blameLines map[int]blame.Author
		if len(b.symbols) > 0 {
			blameLines, _ = blame.RunAt(repoRoot, opts.Branch, rel)
		}

		// File summary: aggregate across all commits.
		if b.file != nil {
			stampFileChurn(b.file, commits, headSHA, opts.Branch, now)
			g.AddNode(b.file)
			res.Files++
		}

		if len(blameLines) == 0 {
			continue
		}
		// Per-symbol: project blame line range, then look up each
		// commit's timestamp/author in the commits map. Falls back
		// to blame timestamps when the commit isn't in the log
		// (shallow clones, signed-off cherry-picks).
		for _, s := range b.symbols {
			if stampSymbolChurn(s, blameLines, commits, now) {
				g.AddNode(s)
				res.Symbols++
			}
		}
	}
	return res, nil
}

// commitRecord is one row of `git log --format=%H|%ct|%ae`.
type commitRecord struct {
	SHA   string
	When  time.Time
	Email string
}

// fileCommits returns the commit history for relPath on branch.
// Ordered newest → oldest. Empty slice when the file has no history
// on that branch (untracked, or the rev predates the file).
func fileCommits(repoRoot, branch, relPath string) ([]commitRecord, error) {
	cmd := exec.Command("git", "-C", repoRoot, "log", branch,
		"--no-merges", "--follow", "--format=%H|%ct|%ae", "--", relPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var records []commitRecord
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		records = append(records, commitRecord{
			SHA:   parts[0],
			When:  time.Unix(ts, 0),
			Email: parts[2],
		})
	}
	return records, scanner.Err()
}

// stampFileChurn writes the file-level summary onto n.Meta["churn"]
// and pins enrichment provenance under n.Meta["churn_meta"].
func stampFileChurn(n *graph.Node, commits []commitRecord, headSHA, branch string, now time.Time) {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	commitCount := len(commits)
	first := commits[len(commits)-1].When
	last := commits[0].When
	ageDays := int(now.Sub(first).Hours() / 24)
	activeDays := ageDays
	if activeDays < 1 {
		activeDays = 1
	}
	n.Meta["churn"] = map[string]any{
		"commit_count":   commitCount,
		"age_days":       ageDays,
		"churn_rate":     roundTwo(float64(commitCount) / float64(activeDays)),
		"last_author":    commits[0].Email,
		"last_commit_at": last.UTC().Format(time.RFC3339),
	}
	n.Meta["churn_meta"] = map[string]any{
		"head_sha":    headSHA,
		"branch":      branch,
		"computed_at": now.UTC().Format(time.RFC3339),
	}
}

// stampSymbolChurn projects the file's blame onto the symbol's line
// range and stamps n.Meta["churn"]. Returns true when the symbol's
// range had at least one blamed line — false when blame produced no
// coverage (uncommitted lines or the file is untracked at the rev).
func stampSymbolChurn(n *graph.Node, blameLines map[int]blame.Author, commits []commitRecord, now time.Time) bool {
	endLine := n.EndLine
	if endLine == 0 {
		endLine = n.StartLine
	}
	commitsSeen := map[string]struct{}{}
	var oldest, newest time.Time
	latestEmail := ""
	for line := n.StartLine; line <= endLine; line++ {
		a, ok := blameLines[line]
		if !ok {
			continue
		}
		commitsSeen[a.Commit] = struct{}{}
		if oldest.IsZero() || a.Timestamp.Before(oldest) {
			oldest = a.Timestamp
		}
		if newest.IsZero() || a.Timestamp.After(newest) {
			newest = a.Timestamp
			latestEmail = a.Email
		}
	}
	if len(commitsSeen) == 0 {
		return false
	}
	// Prefer the canonical author email from the log over the blame
	// author email when both exist — `git log` carries the merged-in
	// author identity, while blame may show the original
	// pre-rebase author.
	if email := latestAuthorFromCommits(commitsSeen, commits); email != "" {
		latestEmail = email
	}
	ageDays := 0
	if !oldest.IsZero() {
		ageDays = int(now.Sub(oldest).Hours() / 24)
	}
	activeDays := ageDays
	if activeDays < 1 {
		activeDays = 1
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta["churn"] = map[string]any{
		"commit_count":   len(commitsSeen),
		"age_days":       ageDays,
		"churn_rate":     roundTwo(float64(len(commitsSeen)) / float64(activeDays)),
		"last_author":    latestEmail,
		"last_commit_at": newest.UTC().Format(time.RFC3339),
	}
	return true
}

// latestAuthorFromCommits picks the email of the most-recent commit
// that touches the symbol's range, using the per-file log as the
// authority for author identity (blame can lag a rebase / cherry-pick).
func latestAuthorFromCommits(commitsSeen map[string]struct{}, commits []commitRecord) string {
	for _, c := range commits {
		if _, ok := commitsSeen[c.SHA]; ok {
			return c.Email
		}
	}
	return ""
}

// roundTwo rounds to two decimals so the JSON output stays compact
// — single-digit precision swallows the difference between 0.03 and
// 0.04 churn-per-day, which matters for ranking.
func roundTwo(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// stripRepoPrefix removes a leading repo segment from multi-repo
// indexer paths so the path we hand to git is repo-relative. Mirrors
// the helper in internal/blame; duplicated rather than exported
// because the blame copy is unexported by design.
func stripRepoPrefix(filePath, repoRoot string) string {
	if !strings.Contains(filePath, "/") {
		return filePath
	}
	if _, err := exec.LookPath("git"); err != nil {
		return filePath
	}
	abs := filepath.Join(repoRoot, filePath)
	if fileExists(abs) {
		return filePath
	}
	if idx := strings.Index(filePath, "/"); idx >= 0 {
		trimmed := filePath[idx+1:]
		if fileExists(filepath.Join(repoRoot, trimmed)) {
			return trimmed
		}
	}
	return filePath
}

var fileExists = func(path string) bool {
	cmd := exec.Command("test", "-f", path)
	return cmd.Run() == nil
}

// runGit shells out and returns trimmed stdout, or "" on error. Used
// only for the one-shot rev-parse; full enrichment calls go through
// fileCommits / blame.RunAt directly.
func runGit(repoRoot string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DefaultBranch returns the repository's default branch as a
// rev-parseable reference (preferring "origin/<name>" when an upstream
// is configured, falling back to a local branch when not). Returns ""
// when none of the candidates resolve — the caller is then expected
// to surface a clear error rather than silently picking the current
// branch (feature branches must not pollute the persisted data).
//
// Exposed so MCP-side enrich handlers can resolve the same branch
// the CLI does without duplicating the probe order across packages.
func DefaultBranch(repoRoot string) string {
	probe := func(args ...string) (string, bool) {
		cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	if ref, ok := probe("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ok && ref != "" {
		return ref
	}
	for _, candidate := range []string{"origin/main", "origin/master", "origin/trunk"} {
		if _, ok := probe("rev-parse", "--verify", "--quiet", candidate); ok {
			return candidate
		}
	}
	for _, candidate := range []string{"main", "master", "trunk"} {
		if _, ok := probe("rev-parse", "--verify", "--quiet", candidate); ok {
			return candidate
		}
	}
	return ""
}
