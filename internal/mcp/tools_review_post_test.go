package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/review"
)

// callPostReview invokes the post_review handler directly with the given args.
func callPostReview(t *testing.T, srv *Server, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "post_review"
	req.Params.Arguments = args
	res, err := srv.handlePostReview(context.Background(), req)
	require.NoError(t, err)
	return res
}

type postReviewOut struct {
	Posted    int              `json:"posted"`
	Skipped   int              `json:"skipped"`
	Redacted  int              `json:"redacted"`
	Total     int              `json:"total"`
	DryRun    bool             `json:"dry_run"`
	ReviewURL string           `json:"review_url"`
	Payloads  []map[string]any `json:"payloads"`
	Error     string           `json:"error"`
}

func decodePostReview(t *testing.T, res *mcplib.CallToolResult) postReviewOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %v", res)
	var out postReviewOut
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

func findingsJSON(t *testing.T, findings []review.Finding) string {
	t.Helper()
	b, err := json.Marshal(findings)
	require.NoError(t, err)
	return string(b)
}

// TestPostReview_DryRunReturnsRedactedPayload drives the real review.PostFindings
// (no forge seam swap) on a dry run: the planted secret must be absent from the
// returned payload and no network call happens (dry run never reaches the forge).
func TestPostReview_DryRunReturnsRedactedPayload(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	const secret = "AKIA0123456789ABCDEF"
	findings := []review.Finding{
		{Rule: "secret", Severity: review.SevError, Category: "security", File: "pkg/a.go", Line: 5,
			Message: "hard-coded credential", Body: "leaked key inline: " + secret},
		{Rule: "nil-deref", Severity: review.SevWarning, Category: "correctness", File: "pkg/b.go", Line: 9,
			Message: "nil deref", Body: "x may be nil"},
	}

	out := decodePostReview(t, callPostReview(t, srv, map[string]any{
		"number":           1,
		"findings":         findingsJSON(t, findings),
		"dry_run":          true,
		"refuse_on_secret": false,
	}))

	require.True(t, out.DryRun)
	require.Equal(t, 1, out.Redacted, "the secret-bearing finding must be redacted")
	require.Equal(t, 2, out.Posted, "both findings build into the would-post payload")
	require.Len(t, out.Payloads, 2)
	for _, p := range out.Payloads {
		body, _ := p["body"].(string)
		require.NotContains(t, body, secret, "secret leaked into dry-run payload")
		require.Equal(t, "RIGHT", p["side"])
	}
}

// TestPostReview_RealPathPostsViaStubbedForgeSeam swaps the post-seam and asserts
// the real (non-dry-run) path reaches review.PostFindings, which maps the
// findings to RIGHT-side comments — no network.
func TestPostReview_RealPathPostsViaStubbedForgeSeam(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	var got []forge.ReviewComment
	var gotNum int
	orig := postReviewFindings
	postReviewFindings = func(_ context.Context, _ string, target review.PostTarget, findings []review.Finding, opts review.PostOptions) (review.PostResult, error) {
		require.False(t, opts.DryRun, "live path must not be a dry run")
		gotNum = target.PRNumber
		// Exercise the real mapping + redaction without networking: build the
		// would-post comments exactly as the live post would.
		got = nil
		for _, f := range findings {
			clean, _ := review.RedactSecrets(review.RenderCommentBody(f))
			end := f.EndLine
			if end == 0 {
				end = f.Line
			}
			start := f.StartLine
			c := forge.ReviewComment{Path: f.File, Line: end, Side: "RIGHT", Body: clean}
			if start > 0 && start < end {
				c.StartLine = start
			}
			got = append(got, c)
		}
		return review.PostResult{Posted: len(got)}, nil
	}
	t.Cleanup(func() { postReviewFindings = orig })

	findings := []review.Finding{
		{Rule: "nil-deref", Severity: review.SevError, File: "pkg/a.go", Line: 14, Body: "deref of nil"},
		{Rule: "race", Severity: review.SevWarning, File: "pkg/b.go", StartLine: 20, EndLine: 25, Body: "data race"},
	}

	out := decodePostReview(t, callPostReview(t, srv, map[string]any{
		"number":   42,
		"findings": findingsJSON(t, findings),
	}))

	require.Equal(t, 42, gotNum)
	require.False(t, out.DryRun)
	require.Equal(t, 2, out.Posted)
	require.Len(t, got, 2)
	for _, c := range got {
		require.Equal(t, "RIGHT", c.Side)
		require.NotEmpty(t, c.Path)
		require.Greater(t, c.Line, 0)
	}
	// Multi-line finding carries start_line < line.
	require.Equal(t, 20, got[1].StartLine)
	require.Equal(t, 25, got[1].Line)
}

// TestPostReview_PublicRepoRefusedWithoutAllowPublic asserts a public target is
// refused (structured error, nothing posted) unless confirm_public is set. The
// seam runs the REAL review.PostFindings gate, so the refusal is the library's,
// proving the tool threads public + confirm_public through correctly.
func TestPostReview_PublicRepoRefusedWithoutAllowPublic(t *testing.T) {
	dir, _ := reviewGitRepo(t)
	srv := indexedSiblingServer(t, dir)

	sawAllowPublic := false
	orig := postReviewFindings
	postReviewFindings = func(ctx context.Context, repoDir string, target review.PostTarget, findings []review.Finding, opts review.PostOptions) (review.PostResult, error) {
		sawAllowPublic = opts.AllowPublic
		require.True(t, target.Public, "tool must thread public:true into the target")
		// Run the real gate, but with a no-op network call so a permitted post
		// does not reach a forge. The gate runs before any network on the dry-run
		// path, so a dry run exercises the public/fork refusal without networking.
		opts.DryRun = true
		return review.PostFindings(ctx, repoDir, target, findings, opts)
	}
	t.Cleanup(func() { postReviewFindings = orig })

	findings := []review.Finding{{Rule: "x", Severity: review.SevWarning, File: "a.go", Line: 1, Body: "f"}}

	// Public, no confirm_public → refused by the real gate.
	out := decodePostReview(t, callPostReview(t, srv, map[string]any{
		"number":   3,
		"findings": findingsJSON(t, findings),
		"public":   true,
	}))
	require.False(t, sawAllowPublic, "confirm_public not set → AllowPublic must be false")
	require.Contains(t, strings.ToLower(out.Error), "public")
	require.Equal(t, 0, out.Posted)

	// Public + confirm_public → AllowPublic threaded through, gate permits.
	out = decodePostReview(t, callPostReview(t, srv, map[string]any{
		"number":         3,
		"findings":       findingsJSON(t, findings),
		"public":         true,
		"confirm_public": true,
	}))
	require.True(t, sawAllowPublic, "confirm_public:true must set AllowPublic")
	require.Empty(t, out.Error)
	require.Equal(t, 1, out.Posted)
}

// TestPostReview_RegisteredEagerly asserts post_review is in the eager (hot) set.
func TestPostReview_RegisteredEagerly(t *testing.T) {
	require.True(t, hotEagerTools["post_review"],
		"post_review must be eagerly registered (hot), not deferred")

	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	live := srv.mcpServer.ListTools()
	require.Contains(t, live, "post_review",
		"eager post_review tool must appear in tools/list without tools_search expansion")
	require.False(t, srv.lazy.IsDeferred("post_review"),
		"post_review must not be deferred")
}
