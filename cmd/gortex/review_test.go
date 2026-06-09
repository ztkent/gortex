package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// cannedReviewJSON is a representative review-tool response: a BLOCK verdict with
// two line-anchored findings across two files plus a per-file risk ranking.
const cannedReviewJSON = `{
  "verdict": "BLOCK",
  "summary": "BLOCK: 2 finding(s) across 2 changed file(s)",
  "total": 2,
  "comments": [
    {"file":"internal/svc/handler.go","line":7,"severity":"error","message":"inverted error check","rule":"go-inverted-err-check","category":"go-inverted-err-check","source":"rulepack"},
    {"file":"internal/svc/loop.go","line":12,"severity":"warning","message":"query in loop","rule":"go-loop-query-call","category":"go-loop-query-call","source":"rulepack"}
  ],
  "file_risk": [
    {"file":"internal/svc/handler.go","risk":"high","findings":1},
    {"file":"internal/svc/loop.go","risk":"medium","findings":1}
  ],
  "stats": {"rulepack":2,"llm":0,"total":2}
}`

// newReviewTestCmd builds a fresh review command bound to a buffer, mirroring the
// real init() wiring so flag defaults match production.
func newReviewTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	// Reset the package-level flag state to defaults so tests don't leak.
	reviewScope = "unstaged"
	reviewBase = ""
	reviewDiff = ""
	reviewUseLLM = false
	reviewFormat = "text"
	reviewRepo = ""
	reviewPost = false
	reviewPR = 0
	reviewConfirmPublic = false
	reviewDryRun = false

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "review", RunE: runReview}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// TestReviewCLI_RendersVerdictAndFindings asserts the text renderer prints the
// verdict, the per-file risk, and the line-anchored findings grouped by file.
func TestReviewCLI_RendersVerdictAndFindings(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })

	var gotTool string
	var gotArgs map[string]any
	reviewDaemonTool = func(repoPath, tool string, args map[string]any) (json.RawMessage, error) {
		gotTool = tool
		gotArgs = args
		return json.RawMessage(cannedReviewJSON), nil
	}

	cmd, buf := newReviewTestCmd(t)
	reviewBase = "main"
	require.NoError(t, runReview(cmd, nil))

	require.Equal(t, "review", gotTool, "the CLI must call the daemon review tool")
	require.Equal(t, "main", gotArgs["base"], "the --base flag must reach the tool args")
	require.Equal(t, "json", gotArgs["format"], "the CLI requests JSON so it can render locally")

	out := buf.String()
	require.Contains(t, out, "Verdict: BLOCK")
	require.Contains(t, out, "BLOCK: 2 finding(s)")
	// Per-file risk section.
	require.Contains(t, out, "File risk:")
	require.Contains(t, out, "high")
	require.Contains(t, out, "internal/svc/handler.go")
	// Line-anchored findings, grouped per file.
	require.Contains(t, out, "L7")
	require.Contains(t, out, "inverted error check")
	require.Contains(t, out, "L12")
	require.Contains(t, out, "query in loop")
	// Files are rendered in sorted order: handler.go before loop.go.
	require.Less(t, strings.Index(out, "handler.go"), strings.Index(out, "loop.go"))
}

// TestReviewCLI_FormatJSON asserts --format json round-trips the structured
// report verbatim (re-indented but field-preserving).
func TestReviewCLI_FormatJSON(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })
	reviewDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(cannedReviewJSON), nil
	}

	cmd, buf := newReviewTestCmd(t)
	reviewFormat = "json"
	require.NoError(t, runReview(cmd, nil))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got), "JSON output must round-trip")
	require.Equal(t, "BLOCK", got["verdict"])
	comments, ok := got["comments"].([]any)
	require.True(t, ok)
	require.Len(t, comments, 2)
}

// TestReviewCLI_DiffFlag feeds a pasted diff from a file and asserts it reaches
// the tool args as `diff`.
func TestReviewCLI_DiffFlag(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })

	var gotArgs map[string]any
	reviewDaemonTool = func(_, _ string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(cannedReviewJSON), nil
	}

	diffFile := t.TempDir() + "/change.diff"
	require.NoError(t, os.WriteFile(diffFile, []byte("diff --git a/x b/x\n+added\n"), 0o644))

	cmd, _ := newReviewTestCmd(t)
	reviewDiff = diffFile
	require.NoError(t, runReview(cmd, nil))
	require.Equal(t, "diff --git a/x b/x\n+added\n", gotArgs["diff"])
}

// TestReviewCLI_DiffFromStdin reads the pasted diff from stdin via "-".
func TestReviewCLI_DiffFromStdin(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })

	var gotArgs map[string]any
	reviewDaemonTool = func(_, _ string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(cannedReviewJSON), nil
	}

	cmd, _ := newReviewTestCmd(t)
	cmd.SetIn(strings.NewReader("diff --git a/y b/y\n+from-stdin\n"))
	reviewDiff = "-"
	require.NoError(t, runReview(cmd, nil))
	require.Equal(t, "diff --git a/y b/y\n+from-stdin\n", gotArgs["diff"])
}

// TestReviewCLI_PostRelaysToPostReview asserts --post relays to the post_review
// daemon tool with the PR number and the public-confirmation / dry-run flags.
func TestReviewCLI_PostRelaysToPostReview(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })

	var gotTool string
	var gotArgs map[string]any
	reviewDaemonTool = func(_, tool string, args map[string]any) (json.RawMessage, error) {
		gotTool = tool
		gotArgs = args
		return json.RawMessage(`{"posted":2,"skipped":0,"redacted":1,"dry_run":true}`), nil
	}

	cmd, _ := newReviewTestCmd(t)
	reviewPost = true
	reviewPR = 42
	reviewBase = "main"
	reviewConfirmPublic = true
	reviewDryRun = true
	require.NoError(t, runReview(cmd, nil))

	require.Equal(t, "post_review", gotTool, "--post must call the post_review daemon tool")
	require.Equal(t, 42, gotArgs["number"])
	require.Equal(t, "main", gotArgs["base"])
	require.Equal(t, true, gotArgs["confirm_public"])
	require.Equal(t, true, gotArgs["dry_run"])
}

// TestReviewCLI_PostRequiresPRNumber asserts --post without --pr is an error and
// never relays to the daemon.
func TestReviewCLI_PostRequiresPRNumber(t *testing.T) {
	orig := reviewDaemonTool
	t.Cleanup(func() { reviewDaemonTool = orig })

	called := false
	reviewDaemonTool = func(_, _ string, _ map[string]any) (json.RawMessage, error) {
		called = true
		return nil, nil
	}

	cmd, _ := newReviewTestCmd(t)
	reviewPost = true
	reviewPR = 0
	err := runReview(cmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--pr")
	require.False(t, called, "must not relay to the daemon without a PR number")
}
