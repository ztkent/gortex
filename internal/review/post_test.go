package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/forge"
)

// withStubPoster swaps the package-level forge-poster seam for the duration of a
// test and records the posted comments. No network is touched.
func withStubPoster(t *testing.T) *[]forge.ReviewComment {
	t.Helper()
	var posted []forge.ReviewComment
	orig := postReviewComments
	postReviewComments = func(_ context.Context, _ string, _ int, comments []forge.ReviewComment) error {
		posted = append([]forge.ReviewComment(nil), comments...)
		return nil
	}
	t.Cleanup(func() { postReviewComments = orig })
	return &posted
}

func TestPostFindings_MapsToRightSideComments(t *testing.T) {
	posted := withStubPoster(t)

	findings := []Finding{
		{Rule: "nil-deref", Severity: SevError, Category: "correctness", File: "pkg/a.go", Line: 14, Message: "possible nil deref", Body: "deref of x without a nil check"},
		{Rule: "race", Severity: SevWarning, Category: "concurrency", File: "pkg/b.go", StartLine: 20, EndLine: 25, Message: "unsynchronized access"},
		{Rule: "todo", Severity: SevInfo, Category: "style", File: "pkg/c.go", Line: 7, Message: "leftover TODO"},
	}

	opts := NewPostOptions()
	res, err := PostFindings(context.Background(), "/repo", PostTarget{Owner: "o", Repo: "r", PRNumber: 42}, findings, opts)
	if err != nil {
		t.Fatalf("PostFindings: %v", err)
	}
	if res.Posted != 3 {
		t.Fatalf("posted=%d want 3", res.Posted)
	}
	if len(*posted) != 3 {
		t.Fatalf("forge got %d comments want 3", len(*posted))
	}
	for _, c := range *posted {
		if c.Side != "RIGHT" {
			t.Fatalf("comment side=%q want RIGHT", c.Side)
		}
		if c.Path == "" || c.Line == 0 {
			t.Fatalf("comment missing path/line: %+v", c)
		}
	}
	// Single-line finding: no StartLine.
	if c := (*posted)[0]; c.StartLine != 0 || c.Line != 14 {
		t.Fatalf("single-line comment: start=%d line=%d want start=0 line=14", c.StartLine, c.Line)
	}
	// Multi-line finding: start_line < line.
	if c := (*posted)[1]; c.StartLine != 20 || c.Line != 25 {
		t.Fatalf("multi-line comment: start=%d line=%d want start=20 line=25", c.StartLine, c.Line)
	}
	if (*posted)[1].StartLine >= (*posted)[1].Line {
		t.Fatalf("multi-line comment must have start_line < line")
	}
	if res.ReviewURL == "" {
		t.Fatal("expected a review url on the live path")
	}
}

func TestPostFindings_DryRunReturnsRedactedPayloadNoNetwork(t *testing.T) {
	// The stub poster must NOT be called on a dry run.
	called := false
	orig := postReviewComments
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error {
		called = true
		return nil
	}
	t.Cleanup(func() { postReviewComments = orig })

	const secret = "AKIA0123456789ABCDEF"
	findings := []Finding{
		// A finding whose body quotes an inline secret.
		{Rule: "secret", Severity: SevError, Category: "security", File: "pkg/a.go", Line: 5,
			Message: "hard-coded credential", Body: "this line ships a key: " + secret},
		// A clean finding.
		{Rule: "nil-deref", Severity: SevWarning, Category: "correctness", File: "pkg/b.go", Line: 9,
			Message: "nil deref", Body: "x may be nil here"},
	}

	// RefuseOnSecret=false so the secret finding is redacted-and-kept, letting us
	// assert the secret is absent from the dry-run payload.
	opts := PostOptions{DryRun: true, RefuseOnSecret: false}
	res, err := PostFindings(context.Background(), "/repo", PostTarget{Owner: "o", Repo: "r", PRNumber: 1}, findings, opts)
	if err != nil {
		t.Fatalf("PostFindings dry run: %v", err)
	}
	if called {
		t.Fatal("dry run must not hit the forge poster")
	}
	if res.Redacted != 1 {
		t.Fatalf("redacted=%d want 1", res.Redacted)
	}
	if res.Posted != 2 {
		t.Fatalf("posted(would-post)=%d want 2", res.Posted)
	}
	if len(res.Payloads) != 2 {
		t.Fatalf("payloads=%d want 2", len(res.Payloads))
	}
	// The planted secret must not appear anywhere in the dry-run payload.
	for _, p := range res.Payloads {
		body, _ := p["body"].(string)
		if strings.Contains(body, secret) {
			t.Fatalf("secret leaked into dry-run payload: %q", body)
		}
		if p["side"] != "RIGHT" {
			t.Fatalf("payload side=%v want RIGHT", p["side"])
		}
	}
	// The redacted payload carries the placeholder.
	if !strings.Contains(res.Payloads[0]["body"].(string), redactPlaceholder) {
		t.Fatalf("expected placeholder in redacted payload, got %q", res.Payloads[0]["body"])
	}
}

func TestPostFindings_RefuseOnSecretSkipsFinding(t *testing.T) {
	posted := withStubPoster(t)
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	findings := []Finding{
		{Rule: "secret", Severity: SevError, File: "a.go", Line: 1, Body: "leak: " + secret},
		{Rule: "clean", Severity: SevWarning, File: "b.go", Line: 2, Body: "ordinary finding"},
	}
	res, err := PostFindings(context.Background(), "/repo", PostTarget{PRNumber: 7}, findings, NewPostOptions())
	if err != nil {
		t.Fatalf("PostFindings: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped=%d want 1", res.Skipped)
	}
	if res.Posted != 1 {
		t.Fatalf("posted=%d want 1", res.Posted)
	}
	if len(*posted) != 1 {
		t.Fatalf("forge got %d comments want 1", len(*posted))
	}
	for _, c := range *posted {
		if strings.Contains(c.Body, secret) {
			t.Fatalf("secret leaked into posted comment: %q", c.Body)
		}
	}
}

func TestPostFindings_PublicRepoRequiresAllowPublic(t *testing.T) {
	called := false
	orig := postReviewComments
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error {
		called = true
		return nil
	}
	t.Cleanup(func() { postReviewComments = orig })

	findings := []Finding{{Rule: "x", Severity: SevWarning, File: "a.go", Line: 1, Body: "finding"}}

	// Public target, AllowPublic off → blocked, nothing sent.
	_, err := PostFindings(context.Background(), "/repo", PostTarget{Public: true, PRNumber: 3}, findings, NewPostOptions())
	if !errors.Is(err, ErrPublicPostBlocked) {
		t.Fatalf("err=%v want ErrPublicPostBlocked", err)
	}
	if called {
		t.Fatal("nothing must be sent when posting is blocked")
	}

	// Public target, AllowPublic on → allowed.
	opts := NewPostOptions()
	opts.AllowPublic = true
	res, err := PostFindings(context.Background(), "/repo", PostTarget{Public: true, PRNumber: 3}, findings, opts)
	if err != nil {
		t.Fatalf("PostFindings with AllowPublic: %v", err)
	}
	if !called || res.Posted != 1 {
		t.Fatalf("expected a posted comment with AllowPublic (called=%v posted=%d)", called, res.Posted)
	}
}

func TestPostFindings_ForgeErrorDegradesCleanly(t *testing.T) {
	sentinel := errors.New("forge boom")
	orig := postReviewComments
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error {
		return sentinel
	}
	t.Cleanup(func() { postReviewComments = orig })

	findings := []Finding{{Rule: "x", Severity: SevWarning, File: "a.go", Line: 1, Body: "finding"}}
	res, err := PostFindings(context.Background(), "/repo", PostTarget{PRNumber: 9}, findings, NewPostOptions())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v want forge error", err)
	}
	if res.Posted != 0 {
		t.Fatalf("posted=%d want 0 on forge error", res.Posted)
	}
}

func TestRenderCommentBody_HeaderAndIdentityFooter(t *testing.T) {
	f := Finding{
		Rule: "nil-deref", Severity: SevCritical, Category: "correctness", Confidence: 0.9,
		File: "pkg/x.go", Line: 14, Message: "nil deref", Body: "x is nil here", Suggestion: "guard x",
	}
	body := RenderCommentBody(f)
	if !strings.Contains(body, "[CRITICAL]") {
		t.Fatalf("missing severity badge: %q", body)
	}
	if !strings.Contains(body, "correctness") {
		t.Fatalf("missing category: %q", body)
	}
	if !strings.Contains(body, "x is nil here") {
		t.Fatalf("missing body text: %q", body)
	}
	if !strings.Contains(body, "guard x") {
		t.Fatalf("missing suggestion: %q", body)
	}
	wantKey := IdentityKey(f)
	if !strings.Contains(body, "<!-- gortex-finding: "+wantKey+" -->") {
		t.Fatalf("missing identity-key footer (key=%s): %q", wantKey, body)
	}
}

func TestFindingToReviewComment_ClampsRange(t *testing.T) {
	// End before start → swapped so start_line < line.
	c := findingToReviewComment(Finding{File: "a.go", StartLine: 30, EndLine: 10})
	if c.Line != 30 || c.StartLine != 10 {
		t.Fatalf("expected swapped range start=10 line=30, got start=%d line=%d", c.StartLine, c.Line)
	}
	if c.Side != "RIGHT" {
		t.Fatalf("side=%q want RIGHT", c.Side)
	}
	// Single line (start==end) → no StartLine.
	c = findingToReviewComment(Finding{File: "a.go", Line: 5})
	if c.StartLine != 0 || c.Line != 5 {
		t.Fatalf("single-line: start=%d line=%d want start=0 line=5", c.StartLine, c.Line)
	}
}
