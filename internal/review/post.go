package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/forge"
)

// ErrPublicPostBlocked is returned when PostFindings is asked to post to a
// public or fork pull request without the AllowPublic opt-in. Posting to a
// public / fork PR makes the comments world-readable, so it requires an explicit
// confirmation (config review.post.allow_public or the --confirm-public flag).
var ErrPublicPostBlocked = errors.New("posting to a public/fork PR requires --confirm-public")

// PostTarget identifies the pull request a review is posted to. Public carries
// the public / fork signal: when true the comments are world-readable, so the
// AllowPublic opt-in is required. The caller (CLI / MCP) determines Public from
// the forge / repo identity; PostFindings only consumes it.
type PostTarget struct {
	Provider  string // "github" (default) — the forge backend
	Owner     string
	Repo      string
	PRNumber  int
	CommitSHA string // optional HEAD sha the review is anchored to
	Public    bool   // the target PR is on a public or fork repo (world-readable)
}

// PostOptions tunes how findings are posted.
type PostOptions struct {
	// DryRun builds and returns the would-post payloads without any network
	// call. The dry-run payloads are redacted identically to the live path, so a
	// planted secret never appears even in a dry run.
	DryRun bool
	// Summary is the top-level review summary body posted alongside the
	// inline comments.
	Summary string
	// AsSingleReview batches every inline comment into one forge review
	// (GitHub createReview) rather than per-comment discussions. Always true for
	// the GitHub backend.
	AsSingleReview bool
	// AllowPublic permits posting to a public / fork PR. Off by default so a
	// misconfigured token never leaks comments to a world-readable thread.
	AllowPublic bool
	// RefuseOnSecret, when true (the default via NewPostOptions), skips any
	// finding whose body still carried a secret rather than posting it with the
	// secret redacted. When false the finding is posted with the secret replaced
	// by the placeholder.
	RefuseOnSecret bool
}

// NewPostOptions returns PostOptions with RefuseOnSecret defaulted on — the
// safe default is to drop a finding that quoted a secret rather than post a
// redacted version of it.
func NewPostOptions() PostOptions {
	return PostOptions{RefuseOnSecret: true}
}

// PostResult reports the outcome of a post. Payloads is populated only on a dry
// run (the would-post per-comment payloads, already redacted).
type PostResult struct {
	// Posted is the number of inline comments actually posted (or that would be
	// posted on a dry run).
	Posted int `json:"posted"`
	// Skipped is the number of findings dropped because their body still carried
	// a secret and RefuseOnSecret was set.
	Skipped int `json:"skipped"`
	// Redacted is the number of findings whose body had a secret redacted out
	// before it was built into a payload.
	Redacted int `json:"redacted"`
	// ReviewURL is the posted review's URL (live path only).
	ReviewURL string `json:"review_url,omitempty"`
	// Payloads carries the per-comment would-post payloads on a dry run.
	Payloads []map[string]any `json:"payloads,omitempty"`
}

// postReviewComments is the forge-poster seam. It is a package var so a test can
// stub the post without a network call (and so a future forge backend can be
// swapped in). It maps onto the L0 forge free function by exact name.
var postReviewComments = forge.PostReviewComments

// PostFindings maps gated findings onto forge inline review comments and posts
// them, after a mandatory secret-redaction and public-repo gate.
//
// The pipeline, in order:
//
//  1. Public / fork gate: when target.Public is true and opts.AllowPublic is
//     false, return ErrPublicPostBlocked without building or sending anything.
//  2. Per finding: render its comment body, run RedactSecrets over it FIRST.
//     When a secret was found and RefuseOnSecret is set, skip the finding
//     (counted in Skipped); otherwise keep it with the secret replaced by the
//     placeholder (counted in Redacted).
//  3. Build a forge.ReviewComment per surviving finding (Side="RIGHT",
//     StartLine carrying the multi-line range start).
//  4. On a dry run, return the would-post payloads (already redacted) without a
//     network call. Otherwise post the batch via the forge poster.
//
// repoDir is the working tree the forge layer resolves the owner/name slug and
// token from; target.PRNumber selects the PR.
func PostFindings(ctx context.Context, repoDir string, target PostTarget, findings []Finding, opts PostOptions) (PostResult, error) {
	var res PostResult

	// Public / fork gate — refuse before any payload is built or sent.
	if target.Public && !opts.AllowPublic {
		return res, ErrPublicPostBlocked
	}

	comments := make([]forge.ReviewComment, 0, len(findings))
	for _, f := range findings {
		clean, hits := RedactSecrets(RenderCommentBody(f))
		if hits > 0 {
			if opts.RefuseOnSecret {
				// Drop the finding entirely rather than post a redacted body.
				res.Skipped++
				continue
			}
			res.Redacted++
		}
		c := findingToReviewComment(f)
		c.Body = clean
		comments = append(comments, c)
		res.Posted++
	}

	if opts.DryRun {
		res.Payloads = make([]map[string]any, 0, len(comments))
		for _, c := range comments {
			res.Payloads = append(res.Payloads, reviewCommentPayload(c))
		}
		return res, nil
	}

	if len(comments) == 0 {
		// Nothing survived redaction — there is nothing to post, and the forge
		// layer would reject an empty review.
		return res, nil
	}

	if err := postReviewComments(ctx, repoDir, target.PRNumber, comments); err != nil {
		// Clean degradation: report nothing as posted, surface the forge error.
		res.Posted = 0
		return res, err
	}

	if target.Owner != "" && target.Repo != "" && target.PRNumber > 0 {
		res.ReviewURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", target.Owner, target.Repo, target.PRNumber)
	}
	return res, nil
}

// findingToReviewComment adapts a review.Finding onto the L0 forge.ReviewComment
// by exact name. The anchor is the new side (Side="RIGHT"); Line is the comment
// line (the finding's end line for a multi-line range) and StartLine is set only
// when the range spans more than one line, clamped so start_line < line.
func findingToReviewComment(f Finding) forge.ReviewComment {
	end := f.EndLine
	if end == 0 {
		end = f.Line
	}
	if end == 0 {
		end = f.StartLine
	}
	start := f.StartLine
	if start == 0 {
		start = f.Line
	}
	// Clamp / swap so start_line <= line.
	if start > end {
		start, end = end, start
	}
	c := forge.ReviewComment{
		Path: f.File,
		Line: end,
		Side: "RIGHT",
		Body: f.Body,
	}
	// StartLine is meaningful only for a true multi-line range.
	if start > 0 && start < end {
		c.StartLine = start
	}
	return c
}

// reviewCommentPayload projects a forge.ReviewComment onto the dry-run payload
// map — the GitHub createReview comment shape ({path, side, line, start_line?,
// body}). start_line is omitted for a single-line comment.
func reviewCommentPayload(c forge.ReviewComment) map[string]any {
	m := map[string]any{
		"path": c.Path,
		"side": c.Side,
		"line": c.Line,
		"body": c.Body,
	}
	if c.StartLine > 0 && c.StartLine < c.Line {
		m["start_line"] = c.StartLine
	}
	return m
}

// RenderCommentBody renders a finding into the markdown body of an inline review
// comment: a severity badge + category + confidence header, the finding's body
// (or message when no body was generated), an optional suggestion, and a
// machine-readable footer carrying the finding's identity key so an already-
// posted finding can be deduplicated on a later run.
func RenderCommentBody(f Finding) string {
	var b strings.Builder

	badge := severityBadge(f.Severity)
	header := badge
	if f.Category != "" {
		header += " · " + f.Category
	}
	if f.Confidence > 0 {
		header += fmt.Sprintf(" · confidence %.0f%%", f.Confidence*100)
	}
	b.WriteString("**")
	b.WriteString(header)
	b.WriteString("**\n\n")

	body := strings.TrimSpace(f.Body)
	if body == "" {
		body = strings.TrimSpace(f.Message)
	}
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}

	if s := strings.TrimSpace(f.Suggestion); s != "" {
		b.WriteString("\n**Suggestion:** ")
		b.WriteString(s)
		b.WriteString("\n")
	}

	key := f.IdentityKey
	if key == "" {
		key = IdentityKey(f)
	}
	b.WriteString("\n<!-- gortex-finding: ")
	b.WriteString(key)
	b.WriteString(" -->")

	return b.String()
}

// severityBadge renders a finding's severity as a short markdown badge.
func severityBadge(s Severity) string {
	switch normalizeSeverity(string(s)) {
	case SevCritical:
		return "[CRITICAL]"
	case SevError:
		return "[ERROR]"
	case SevWarning:
		return "[WARNING]"
	default:
		return "[INFO]"
	}
}
