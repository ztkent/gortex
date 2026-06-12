// Package forge is the single network surface for pull-request I/O. It
// wraps the official go-github SDK behind a small set of free functions
// — ListPRs, ViewPR, PRFiles, DiffPR, PostReviewComments, Available —
// every one taking (ctx, repoDir, …). Each constructs an internal
// ghClient that resolves the GitHub token (GH_TOKEN → GITHUB_TOKEN) and
// the owner/name slug from the indexed repo identity (falling back to
// `git remote get-url origin` through gitcmd).
//
// The consumed surface is the free functions, not a method set: callers
// never hold a client. The Client interface exists only as a test seam
// (a func-var indirection injects canned data) and as the future-backend
// seam — a deferred gh-CLI or GitLab backend slots in behind it without
// touching any caller.
//
// GitHub REST exposes no single reviewDecision or statusCheckRollup
// field; those are GraphQL aggregates. ReviewDecision and CIRollup are
// reconstructed here from ListReviews and check-runs/combined-status,
// opt-in per call (ListOpts.WithDecision / WithCI; ViewPR always fills
// them) so a cheap ListPRs skips the extra round-trips.
package forge

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/analysis"
)

// callTimeout bounds every network round-trip a forge call makes.
const callTimeout = 30 * time.Second

// ErrNotAuthenticated is returned when no GitHub token is resolvable
// from the environment.
var ErrNotAuthenticated = errors.New("no GitHub token: set GH_TOKEN (or GITHUB_TOKEN)")

// ErrRateLimited is returned when GitHub answers with a rate-limit /
// abuse-rate-limit error. The underlying *github.RateLimitError /
// *github.AbuseRateLimitError is mapped onto this sentinel (with a
// Retry-After hint preserved in the wrapped error's message) so callers
// can test errors.Is(err, ErrRateLimited).
var ErrRateLimited = errors.New("github rate limited")

// PR is the canonical pull-request value. It is built from a go-github
// *github.PullRequest; ReviewDecision and CIRollup are reconstructed (the
// REST API has no such aggregate fields). Files is EMPTY after ListPRs —
// only ViewPR / PRFiles hydrate it.
type PR struct {
	Number         int
	Title          string
	Author         string
	BaseRef        string // PullRequest.Base.Ref
	HeadRef        string // PullRequest.Head.Ref
	IsDraft        bool
	ReviewDecision string // reconstructed from ListReviews (REST has no reviewDecision field)
	CIRollup       string // reconstructed from Checks + GetCombinedStatus; collapsed by RollupCI
	UpdatedAt      time.Time
	Mergeable      string
	URL            string
	State          string
	Files          []string // EMPTY after ListPRs; only ViewPR / PRFiles hydrate it
}

// PRDiff is the per-file diff of a pull request, each file's patch parsed
// into hunks via analysis.ParseDiffHunks.
type PRDiff struct {
	Number  int
	BaseRef string
	HeadRef string
	Files   []PRFile
	Raw     string
}

// PRFile is one changed file in a PR diff.
type PRFile struct {
	Path    string
	OldPath string
	Hunks   []analysis.DiffHunk // analysis.ParseDiffHunks on the file's .GetPatch()
	Status  string
}

// ReviewComment is the one inline-comment type. Posting maps a finding
// to a ReviewComment; StartLine carries the multi-line range start and
// Side defaults to "RIGHT" (the new side).
type ReviewComment struct {
	Path      string
	Line      int
	StartLine int
	Side      string
	Body      string
}

// ListOpts tunes ListPRs. Decision/CI reconstruction is opt-in per call
// so a cheap list skips the extra round-trips.
type ListOpts struct {
	State        string
	Limit        int
	Author       string
	WithDecision bool
	WithCI       bool
}

// Client is the test seam / future-backend seam. The free functions are
// the consumed surface; this interface lets a test inject canned data and
// lets a deferred gh-CLI or GitLab backend slot in without touching any
// caller.
type Client interface {
	ListPRs(ctx context.Context, opts ListOpts) ([]PR, error)
	ViewPR(ctx context.Context, num int) (*PR, error)
	PRFiles(ctx context.Context, num int) ([]string, error)
	DiffPR(ctx context.Context, num int) (*PRDiff, error)
	PostReviewComments(ctx context.Context, num int, comments []ReviewComment) error
}

// newClient resolves a *ghClient for repoDir. It is a package var so a
// test can swap it for one backed by an httptest server with a fixed
// owner/repo, bypassing token + git-remote resolution.
var newClient = newGHClient

// ListPRs lists pull requests for the repo at repoDir. PR.Files is EMPTY
// on every returned PR — triage and conflict detection must call
// PRFiles(num) explicitly per PR. Decision/CI aggregates are filled only
// when opts requests them.
func ListPRs(ctx context.Context, repoDir string, opts ListOpts) ([]PR, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.ListPRs(ctx, opts)
}

// ViewPR fetches a single pull request and hydrates its Files plus the
// reconstructed ReviewDecision / CIRollup aggregates.
func ViewPR(ctx context.Context, repoDir string, num int) (*PR, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.ViewPR(ctx, num)
}

// PRFiles returns the paths of files changed in a pull request.
func PRFiles(ctx context.Context, repoDir string, num int) ([]string, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.PRFiles(ctx, num)
}

// DiffPR returns the per-file diff of a pull request, each file's patch
// parsed into hunks.
func DiffPR(ctx context.Context, repoDir string, num int) (*PRDiff, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.DiffPR(ctx, num)
}

// PostReviewComments posts a batch of inline review comments on a pull
// request as a single review.
func PostReviewComments(ctx context.Context, repoDir string, num int, comments []ReviewComment) error {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return err
	}
	return c.PostReviewComments(ctx, num, comments)
}

// Available reports whether a GitHub token (GH_TOKEN or GITHUB_TOKEN) is
// resolvable from the environment.
func Available(ctx context.Context) bool {
	return resolveToken() != ""
}
