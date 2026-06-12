package forge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v88/github"
	"golang.org/x/sync/errgroup"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/indexer"
)

// ghClient wraps a *github.Client with the resolved owner/repo slug for a
// single repository. It is constructed by newGHClient and used by the
// free functions; callers never hold it directly.
type ghClient struct {
	gh      *github.Client
	owner   string
	repo    string
	timeout time.Duration
}

// makeGitHubClient builds the *github.Client. It is a package var so a
// test can swap it for a client pointed at an httptest server. tok is the
// resolved auth token and base, when non-empty, is the enterprise base
// URL (also used as the upload URL).
var makeGitHubClient = func(tok, base string) (*github.Client, error) {
	opts := []github.ClientOptionsFunc{github.WithAuthToken(tok)}
	if base != "" {
		opts = append(opts, github.WithEnterpriseURLs(base, base))
	}
	return github.NewClient(opts...)
}

// newGHClient resolves the token and owner/repo slug for repoDir and
// builds a ghClient. A missing token surfaces ErrNotAuthenticated; an
// unresolvable slug surfaces a wrapped error. When GITHUB_API_URL / GH_HOST
// names a GitHub Enterprise host the client is switched to that host's
// base URL.
func newGHClient(ctx context.Context, repoDir string) (*ghClient, error) {
	tok := resolveToken()
	if tok == "" {
		return nil, ErrNotAuthenticated
	}
	owner, repo, err := resolveSlug(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	gh, err := makeGitHubClient(tok, enterpriseBase())
	if err != nil {
		return nil, fmt.Errorf("forge: building github client: %w", err)
	}
	return &ghClient{gh: gh, owner: owner, repo: repo, timeout: callTimeout}, nil
}

// resolveToken returns the GitHub token from GH_TOKEN, falling back to
// GITHUB_TOKEN. It returns "" when neither is set.
func resolveToken() string {
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

// enterpriseBase returns the GitHub Enterprise API base URL when
// GITHUB_API_URL or GH_HOST names a non-github.com host, else "". The
// returned URL carries a trailing slash, as go-github's base-URL contract
// requires.
func enterpriseBase() string {
	if v := strings.TrimSpace(os.Getenv("GITHUB_API_URL")); v != "" {
		if h := hostOf(v); h != "" && !isPublicGitHub(h) {
			return ensureTrailingSlash(v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("GH_HOST")); v != "" {
		h := hostOf(v)
		if h == "" {
			h = v
		}
		if h != "" && !isPublicGitHub(h) {
			return "https://" + h + "/api/v3/"
		}
	}
	return ""
}

func hostOf(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func isPublicGitHub(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "api.github.com" || host == "www.github.com"
}

func ensureTrailingSlash(s string) string {
	if strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
}

// resolveSlug derives owner/name for repoDir. It first asks the indexed
// repo identity (indexer.DetectIdentity → NormalizeRemoteURL canonical
// form), then falls back to `git remote get-url origin` through gitcmd.
func resolveSlug(ctx context.Context, repoDir string) (owner, repo string, err error) {
	if id, idErr := indexer.DetectIdentity(repoDir); idErr == nil && id != nil {
		if o, r, ok := ownerRepoFrom(id.CanonicalID); ok {
			return o, r, nil
		}
		if o, r, ok := ownerRepoFrom(id.RemoteURL); ok {
			return o, r, nil
		}
	}
	// Fallback: read the remote directly through the git chokepoint.
	raw, rErr := gitcmd.Output(ctx, repoDir, "remote", "get-url", "origin")
	if rErr != nil {
		return "", "", fmt.Errorf("forge: resolving owner/repo for %s: %w", repoDir, rErr)
	}
	if o, r, ok := ownerRepoFrom(indexer.NormalizeRemoteURL(raw)); ok {
		return o, r, nil
	}
	return "", "", fmt.Errorf("forge: could not derive owner/repo from remote %q", raw)
}

// ownerRepoFrom extracts (owner, repo) from a normalized remote of the
// form "host/owner/repo" (or "owner/repo"). The trailing two
// slash-separated components are the owner and repo.
func ownerRepoFrom(canonical string) (owner, repo string, ok bool) {
	canonical = strings.TrimSpace(canonical)
	canonical = strings.TrimSuffix(canonical, ".git")
	canonical = strings.Trim(canonical, "/")
	if canonical == "" {
		return "", "", false
	}
	parts := strings.Split(canonical, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	repo = parts[len(parts)-1]
	owner = parts[len(parts)-2]
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

// mapErr maps go-github's typed rate-limit errors onto ErrRateLimited
// (preserving a Retry-After hint) and leaves every other error untouched.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var rl *github.RateLimitError
	if errors.As(err, &rl) {
		if !rl.Rate.Reset.IsZero() {
			d := time.Until(rl.Rate.Reset.Time)
			if d < 0 {
				d = 0
			}
			return fmt.Errorf("%w (retry after %s)", ErrRateLimited, d.Round(time.Second))
		}
		return fmt.Errorf("%w: %s", ErrRateLimited, rl.Message)
	}
	var ab *github.AbuseRateLimitError
	if errors.As(err, &ab) {
		if ab.RetryAfter != nil {
			return fmt.Errorf("%w (retry after %s)", ErrRateLimited, ab.RetryAfter.Round(time.Second))
		}
		return fmt.Errorf("%w: %s", ErrRateLimited, ab.Message)
	}
	return err
}

// callCtx derives a per-call context bounded by the client's timeout.
func (c *ghClient) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	t := c.timeout
	if t <= 0 {
		t = callTimeout
	}
	return context.WithTimeout(ctx, t)
}

// ListPRs lists pull requests. PR.Files is left empty; decision/CI
// aggregates are filled only when opts requests them.
func (c *ghClient) ListPRs(ctx context.Context, opts ListOpts) ([]PR, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	state := opts.State
	if state == "" {
		state = "open"
	}
	perPage := opts.Limit
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	ghOpts := &github.PullRequestListOptions{
		State:       state,
		ListOptions: github.ListOptions{PerPage: perPage},
	}
	ghPRs, _, err := c.gh.PullRequests.List(cctx, c.owner, c.repo, ghOpts)
	if err != nil {
		return nil, mapErr(err)
	}

	out := make([]PR, 0, len(ghPRs))
	heads := make([]string, 0, len(ghPRs))
	for _, p := range ghPRs {
		if p == nil {
			continue
		}
		if opts.Author != "" && p.GetUser().GetLogin() != opts.Author {
			continue
		}
		out = append(out, prFromGH(p))
		heads = append(heads, p.GetHead().GetSHA())
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}

	// Reconstruct opt-in aggregates concurrently across the result set.
	if opts.WithDecision || opts.WithCI {
		if err := c.fillAggregates(cctx, out, heads, opts.WithDecision, opts.WithCI); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ViewPR fetches a single pull request, hydrates Files, and always fills
// the reconstructed ReviewDecision / CIRollup aggregates.
func (c *ghClient) ViewPR(ctx context.Context, num int) (*PR, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	p, _, err := c.gh.PullRequests.Get(cctx, c.owner, c.repo, num)
	if err != nil {
		return nil, mapErr(err)
	}
	pr := prFromGH(p)

	files, err := c.listFiles(cctx, num)
	if err != nil {
		return nil, err
	}
	pr.Files = make([]string, 0, len(files))
	for _, f := range files {
		pr.Files = append(pr.Files, f.GetFilename())
	}

	if dec, err := c.reviewDecision(cctx, num); err != nil {
		return nil, err
	} else {
		pr.ReviewDecision = dec
	}
	if ci, err := c.ciRollup(cctx, p.GetHead().GetSHA()); err != nil {
		return nil, err
	} else {
		pr.CIRollup = ci
	}
	return &pr, nil
}

// PRFiles returns the changed file paths of a pull request.
func (c *ghClient) PRFiles(ctx context.Context, num int) ([]string, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	files, err := c.listFiles(cctx, num)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.GetFilename())
	}
	return out, nil
}

// DiffPR returns the per-file diff, each file's patch run through
// analysis.ParseDiffHunks.
func (c *ghClient) DiffPR(ctx context.Context, num int) (*PRDiff, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	p, _, err := c.gh.PullRequests.Get(cctx, c.owner, c.repo, num)
	if err != nil {
		return nil, mapErr(err)
	}
	files, err := c.listFiles(cctx, num)
	if err != nil {
		return nil, err
	}

	diff := &PRDiff{
		Number:  p.GetNumber(),
		BaseRef: p.GetBase().GetRef(),
		HeadRef: p.GetHead().GetRef(),
	}
	var raw strings.Builder
	for _, f := range files {
		patch := f.GetPatch()
		path := f.GetFilename()
		// GitHub's per-file .patch carries only the hunk body — no
		// `+++ b/<file>` header — so synthesize one so ParseDiffHunks
		// scopes the hunks to this file (and so Raw is a valid diff).
		var withHeader string
		if patch != "" {
			withHeader = "--- a/" + path + "\n+++ b/" + path + "\n" + patch
			if !strings.HasSuffix(withHeader, "\n") {
				withHeader += "\n"
			}
		}
		pf := PRFile{
			Path:    path,
			OldPath: f.GetPreviousFilename(),
			Status:  f.GetStatus(),
			Hunks:   analysis.ParseDiffHunks(withHeader),
		}
		diff.Files = append(diff.Files, pf)
		raw.WriteString(withHeader)
	}
	diff.Raw = raw.String()
	return diff, nil
}

// PostReviewComments posts a batch of inline review comments as a single
// COMMENT review on the pull request.
func (c *ghClient) PostReviewComments(ctx context.Context, num int, comments []ReviewComment) error {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	drafts := make([]*github.DraftReviewComment, 0, len(comments))
	for _, rc := range comments {
		side := rc.Side
		if side == "" {
			side = "RIGHT"
		}
		d := &github.DraftReviewComment{
			Path: github.Ptr(rc.Path),
			Body: github.Ptr(rc.Body),
			Side: github.Ptr(side),
			Line: github.Ptr(rc.Line),
		}
		// A multi-line range carries a start line strictly before Line.
		if rc.StartLine > 0 && rc.StartLine < rc.Line {
			d.StartLine = github.Ptr(rc.StartLine)
			d.StartSide = github.Ptr(side)
		}
		drafts = append(drafts, d)
	}
	req := &github.PullRequestReviewRequest{
		Event:    github.Ptr("COMMENT"),
		Comments: drafts,
	}
	_, _, err := c.gh.PullRequests.CreateReview(cctx, c.owner, c.repo, num, req)
	return mapErr(err)
}

// DefaultBranch returns the repository's default branch via the GitHub
// API, falling back to the local probe in churn.DefaultBranch when the
// API call fails or returns empty.
func (c *ghClient) DefaultBranch(ctx context.Context) string {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	if repo, _, err := c.gh.Repositories.Get(cctx, c.owner, c.repo); err == nil {
		if b := repo.GetDefaultBranch(); b != "" {
			return b
		}
	}
	return churn.DefaultBranch("")
}

// listFiles fetches the changed-file records of a pull request.
func (c *ghClient) listFiles(ctx context.Context, num int) ([]*github.CommitFile, error) {
	files, _, err := c.gh.PullRequests.ListFiles(ctx, c.owner, c.repo, num, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, mapErr(err)
	}
	return files, nil
}

// fillAggregates reconstructs the requested ReviewDecision / CIRollup
// fields for every PR in prs, fetching concurrently under an errgroup with
// a bounded worker count.
func (c *ghClient) fillAggregates(ctx context.Context, prs []PR, heads []string, withDecision, withCI bool) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := range prs {
		i := i
		head := ""
		if i < len(heads) {
			head = heads[i]
		}
		g.Go(func() error {
			if withDecision {
				dec, err := c.reviewDecision(gctx, prs[i].Number)
				if err != nil {
					return err
				}
				prs[i].ReviewDecision = dec
			}
			if withCI {
				ci, err := c.ciRollup(gctx, head)
				if err != nil {
					return err
				}
				prs[i].CIRollup = ci
			}
			return nil
		})
	}
	return g.Wait()
}

// reviewDecision reconstructs a PR's review decision from its reviews.
// The latest non-COMMENTED state per reviewer is taken; the result is
// CHANGES_REQUESTED if any reviewer's latest state requests changes,
// APPROVED if at least one approves and none requests changes, else
// REVIEW_REQUIRED.
func (c *ghClient) reviewDecision(ctx context.Context, num int) (string, error) {
	reviews, _, err := c.gh.PullRequests.ListReviews(ctx, c.owner, c.repo, num, &github.ListOptions{PerPage: 100})
	if err != nil {
		return "", mapErr(err)
	}
	// Latest meaningful state per reviewer, in API order (chronological).
	latest := map[string]string{}
	for _, r := range reviews {
		if r == nil {
			continue
		}
		state := strings.ToUpper(r.GetState())
		if state == "COMMENTED" || state == "DISMISSED" || state == "PENDING" {
			continue
		}
		login := r.GetUser().GetLogin()
		if login == "" {
			continue
		}
		latest[login] = state
	}
	approved := false
	for _, state := range latest {
		switch state {
		case "CHANGES_REQUESTED":
			return "CHANGES_REQUESTED", nil
		case "APPROVED":
			approved = true
		}
	}
	if approved {
		return "APPROVED", nil
	}
	return "REVIEW_REQUIRED", nil
}

// ciRollup reconstructs a PR's CI rollup from check-runs and the legacy
// combined commit status for the head ref, collapsed by RollupCI to one
// of NONE / FAILURE / PENDING / SUCCESS.
func (c *ghClient) ciRollup(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "NONE", nil
	}
	var states []string

	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, ref, &github.ListCheckRunsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return "", mapErr(err)
	}
	if runs != nil {
		for _, run := range runs.CheckRuns {
			if run == nil {
				continue
			}
			states = append(states, checkRunState(run))
		}
	}

	combined, _, err := c.gh.Repositories.GetCombinedStatus(ctx, c.owner, c.repo, ref, &github.ListOptions{PerPage: 100})
	if err != nil {
		return "", mapErr(err)
	}
	if combined != nil {
		for _, st := range combined.Statuses {
			if st == nil {
				continue
			}
			states = append(states, strings.ToLower(st.GetState()))
		}
	}

	return collapseStates(states), nil
}

// checkRunState normalizes a check-run's status+conclusion to one of
// failure / pending / success.
func checkRunState(run *github.CheckRun) string {
	if strings.ToLower(run.GetStatus()) != "completed" {
		return "pending"
	}
	switch strings.ToLower(run.GetConclusion()) {
	case "success", "neutral", "skipped":
		return "success"
	case "failure", "timed_out", "action_required", "cancelled", "stale", "startup_failure":
		return "failure"
	default:
		return "pending"
	}
}

// collapseStates folds a set of per-check states (failure/pending/success
// or the combined-status vocabulary error/failure/pending/success) into a
// single rollup: any failure → FAILURE, else any pending → PENDING, else
// SUCCESS, with an empty set → NONE.
func collapseStates(states []string) string {
	if len(states) == 0 {
		return "NONE"
	}
	pending := false
	for _, s := range states {
		switch strings.ToLower(s) {
		case "failure", "error":
			return "FAILURE"
		case "pending", "in_progress", "queued", "":
			pending = true
		}
	}
	if pending {
		return "PENDING"
	}
	return "SUCCESS"
}

// prFromGH projects a go-github *github.PullRequest onto a forge.PR. It
// does NOT hydrate Files or the reconstructed aggregates.
func prFromGH(p *github.PullRequest) PR {
	pr := PR{
		Number:    p.GetNumber(),
		Title:     p.GetTitle(),
		Author:    p.GetUser().GetLogin(),
		BaseRef:   p.GetBase().GetRef(),
		HeadRef:   p.GetHead().GetRef(),
		IsDraft:   p.GetDraft(),
		UpdatedAt: p.GetUpdatedAt().Time,
		Mergeable: p.GetMergeableState(),
		URL:       p.GetHTMLURL(),
		State:     p.GetState(),
	}
	return pr
}
