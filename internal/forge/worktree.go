package forge

import (
	"context"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/indexer"
)

// WorktreeEntry is one checked-out worktree of a repository.
type WorktreeEntry struct {
	Path   string
	Branch string
	Head   string
}

// LocalWorktrees lists the git worktrees of the repository at repoDir by
// parsing `git worktree list --porcelain` through the git chokepoint. The
// main checkout and every linked worktree are returned; the branch is the
// short name (detached-HEAD checkouts carry an empty Branch).
//
// indexer.ResolveWorktree is consulted to normalize each entry's path to
// its main-repo root relationship — letting a caller cross-reference a
// PR's head ref against a locally checked-out worktree.
func LocalWorktrees(ctx context.Context, repoDir string) ([]WorktreeEntry, error) {
	out, err := gitcmd.Output(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	entries := parsePorcelainWorktrees(out)
	// Cross-reference each entry against the resolved worktree info so a
	// linked worktree's path is anchored to its main checkout.
	for i := range entries {
		if entries[i].Path == "" {
			continue
		}
		_ = indexer.ResolveWorktree(entries[i].Path)
	}
	return entries, nil
}

// parsePorcelainWorktrees parses the record-per-worktree porcelain output
// of `git worktree list --porcelain`. Records are separated by a blank
// line; each begins with a `worktree <path>` line and may carry `HEAD`,
// `branch refs/heads/<name>`, `detached`, or `bare` lines.
func parsePorcelainWorktrees(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var cur *WorktreeEntry
	flush := func() {
		if cur != nil && cur.Path != "" {
			entries = append(entries, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &WorktreeEntry{Path: strings.TrimSpace(strings.TrimPrefix(line, "worktree "))}
		case cur == nil:
			// stray line before the first record — ignore
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "detached" || line == "bare":
			// no branch name for a detached or bare worktree
		}
	}
	flush()
	return entries
}
