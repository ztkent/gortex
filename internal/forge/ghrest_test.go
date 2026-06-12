package forge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Recorded GitHub REST responses (trimmed to the fields forge reads).

const prListJSON = `[
  {
    "number": 7,
    "title": "Add forge",
    "state": "open",
    "draft": false,
    "html_url": "https://github.com/octo/gortex/pull/7",
    "mergeable_state": "clean",
    "updated_at": "2026-06-01T10:00:00Z",
    "user": {"login": "alice"},
    "base": {"ref": "main"},
    "head": {"ref": "feature", "sha": "headsha"}
  }
]`

const prGetJSON = `{
  "number": 7,
  "title": "Add forge",
  "state": "open",
  "draft": false,
  "html_url": "https://github.com/octo/gortex/pull/7",
  "mergeable_state": "clean",
  "updated_at": "2026-06-01T10:00:00Z",
  "user": {"login": "alice"},
  "base": {"ref": "main"},
  "head": {"ref": "feature", "sha": "headsha"}
}`

const prFilesJSON = `[
  {
    "filename": "internal/forge/forge.go",
    "status": "added",
    "patch": "@@ -0,0 +1,3 @@\n+package forge\n+\n+// new\n"
  },
  {
    "filename": "internal/forge/ghrest.go",
    "previous_filename": "internal/forge/old.go",
    "status": "renamed",
    "patch": "@@ -1,2 +1,2 @@\n-old\n+new\n line2\n"
  }
]`

const prReviewsJSON = `[
  {"state": "APPROVED", "user": {"login": "bob"}, "submitted_at": "2026-06-01T09:00:00Z"},
  {"state": "COMMENTED", "user": {"login": "carol"}, "submitted_at": "2026-06-01T09:30:00Z"},
  {"state": "CHANGES_REQUESTED", "user": {"login": "carol"}, "submitted_at": "2026-06-01T09:40:00Z"}
]`

const checkRunsJSON = `{
  "total_count": 2,
  "check_runs": [
    {"name": "build", "status": "completed", "conclusion": "success"},
    {"name": "test", "status": "completed", "conclusion": "failure"}
  ]
}`

const combinedStatusJSON = `{
  "state": "failure",
  "sha": "headsha",
  "total_count": 1,
  "statuses": [
    {"state": "success", "context": "lint"}
  ]
}`

func TestReviewDecision_LatestPerReviewer(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	got, err := c.reviewDecision(context.Background(), 7)
	if err != nil {
		t.Fatalf("reviewDecision: %v", err)
	}
	// carol's latest non-comment state is CHANGES_REQUESTED → wins.
	if got != "CHANGES_REQUESTED" {
		t.Errorf("reviewDecision = %q, want CHANGES_REQUESTED", got)
	}
}

func TestCIRollup(t *testing.T) {
	srv := fixtureServer(t)
	c := newTestClient(t, srv.URL)
	got, err := c.ciRollup(context.Background(), "headsha")
	if err != nil {
		t.Fatalf("ciRollup: %v", err)
	}
	if got != "FAILURE" {
		t.Errorf("ciRollup = %q, want FAILURE", got)
	}
}

func TestCIRollup_NoRef(t *testing.T) {
	c := &ghClient{owner: "octo", repo: "gortex"}
	got, err := c.ciRollup(context.Background(), "")
	if err != nil {
		t.Fatalf("ciRollup: %v", err)
	}
	if got != "NONE" {
		t.Errorf("ciRollup(empty ref) = %q, want NONE", got)
	}
}

func TestParsePorcelainWorktrees(t *testing.T) {
	out := "worktree /repo/main\nHEAD abcd1234\nbranch refs/heads/main\n\n" +
		"worktree /repo/wt-feature\nHEAD ef567890\nbranch refs/heads/feature\n\n" +
		"worktree /repo/detached\nHEAD 99999999\ndetached\n"
	got := parsePorcelainWorktrees(out)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Path != "/repo/main" || got[0].Branch != "main" || got[0].Head != "abcd1234" {
		t.Errorf("entry[0] = %+v", got[0])
	}
	if got[1].Branch != "feature" {
		t.Errorf("entry[1].Branch = %q, want feature", got[1].Branch)
	}
	if got[2].Branch != "" {
		t.Errorf("detached entry must have empty Branch, got %q", got[2].Branch)
	}
}

func TestLocalWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit(main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(main, "add", ".")
	runGit(main, "commit", "-q", "-m", "init")
	wt := filepath.Join(dir, "wt")
	runGit(main, "worktree", "add", "-q", "-b", "feature", wt)

	entries, err := LocalWorktrees(context.Background(), main)
	if err != nil {
		t.Fatalf("LocalWorktrees: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d worktrees, want 2: %+v", len(entries), entries)
	}
	branches := map[string]bool{}
	for _, e := range entries {
		branches[e.Branch] = true
		if e.Path == "" || e.Head == "" {
			t.Errorf("entry missing path/head: %+v", e)
		}
	}
	if !branches["main"] || !branches["feature"] {
		t.Errorf("branches = %v, want main+feature", branches)
	}
}
