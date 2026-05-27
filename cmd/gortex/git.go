package main

import (
	"bytes"
	"os/exec"
	"strings"

	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/indexer"
)

// gitCommitHash returns the HEAD commit hash for the repository at dir,
// or an empty string if git is unavailable or the directory is not a repo.
func gitCommitHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// gitBranch returns the current branch name for the repository at dir.
// It returns an empty string when git is unavailable, the directory is
// not a repo, or HEAD is detached — callers then key snapshots by
// commit hash instead of branch.
func gitBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	branch := strings.TrimSpace(out.String())
	if branch == "HEAD" {
		return "" // detached HEAD — no branch to key on
	}
	return branch
}

// canonicalRepo resolves a git worktree to the main repository it
// shares a .git directory with, so every worktree of one repo keys its
// index cache under a shared base — the per-branch snapshot slot then
// gives each worktree its own entry. A non-worktree path is returned
// unchanged.
func canonicalRepo(dir string) string {
	return indexer.ResolveWorktree(dir).MainRepoPath
}

// gitDefaultBranch returns the repository's default branch as a
// rev-parseable reference. Thin wrapper over churn.DefaultBranch so
// the CLI, daemon controller, and MCP tool resolve the same branch
// the same way.
func gitDefaultBranch(dir string) string {
	return churn.DefaultBranch(dir)
}

