package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	reviewScope         string
	reviewBase          string
	reviewDiff          string
	reviewUseLLM        bool
	reviewFormat        string
	reviewRepo          string
	reviewPost          bool
	reviewPR            int
	reviewConfirmPublic bool
	reviewDryRun        bool
)

// reviewDaemonTool is the daemon-tool relay seam. It is indirected through a
// package var so a test can stub the daemon call without a running daemon.
var reviewDaemonTool = requireDaemonTool

var reviewCmd = &cobra.Command{
	Use:   "review [path]",
	Short: "Review a changeset and print line-anchored inline comments + a verdict",
	Long: `Reviews a changeset against the daemon that owns the repo and prints the
review verdict (BLOCK / REVIEW / APPROVE), a one-line summary, the per-file risk
ranking, and the line-anchored inline comments — each anchored to an exact
file + line so it reads like a PR review.

The changeset is selected by --scope (unstaged / staged / all / compare), or by
--base <ref> (shorthand for comparing against that ref), or from a pasted
unified diff via --diff <file> (use "-" to read the diff from stdin). The
deterministic correctness rulepack always runs; pass --use-llm to additionally
fold in LLM-found findings (requires a configured LLM provider).

Requires a running daemon that tracks the repo. Use --format json for the
structured report.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runReview,
}

func init() {
	reviewCmd.Flags().StringVarP(&reviewScope, "scope", "s", "unstaged", "changeset scope: unstaged, staged, all, or compare")
	reviewCmd.Flags().StringVarP(&reviewBase, "base", "b", "", "base git ref to compare against (forces compare scope)")
	reviewCmd.Flags().StringVarP(&reviewDiff, "diff", "d", "", "review a pasted unified diff from a file (\"-\" for stdin) instead of git")
	reviewCmd.Flags().BoolVar(&reviewUseLLM, "use-llm", false, "fold in LLM-found findings (requires a configured LLM provider)")
	reviewCmd.Flags().StringVarP(&reviewFormat, "format", "f", "text", "output format: text or json")
	reviewCmd.Flags().StringVar(&reviewRepo, "repo", "", "repository path the daemon must track (default: current directory)")
	reviewCmd.Flags().BoolVar(&reviewPost, "post", false, "post the findings as inline comments on a PR (requires --pr); secrets are redacted before egress")
	reviewCmd.Flags().IntVar(&reviewPR, "pr", 0, "the PR / MR number to post comments on (with --post)")
	reviewCmd.Flags().BoolVar(&reviewConfirmPublic, "confirm-public", false, "confirm posting to a public / fork PR (world-readable comments)")
	reviewCmd.Flags().BoolVar(&reviewDryRun, "dry-run", false, "with --post, build and print the would-post (already-redacted) payloads without posting")
	rootCmd.AddCommand(reviewCmd)
}

func runReview(cmd *cobra.Command, args []string) error {
	repoPath := "."
	if reviewRepo != "" {
		repoPath = reviewRepo
	} else if len(args) > 0 {
		repoPath = args[0]
	}

	var diffText string
	if reviewDiff != "" {
		dt, err := readReviewDiff(cmd, reviewDiff)
		if err != nil {
			return err
		}
		diffText = dt
	}

	// Posting path: relay to the post_review daemon tool, which derives the
	// findings server-side (running the review rulepack over the same changeset),
	// redacts every comment body before egress, and gates a public / fork PR.
	if reviewPost {
		return runReviewPost(cmd, repoPath, diffText)
	}

	toolArgs := map[string]any{
		"scope":   reviewScope,
		"use_llm": reviewUseLLM,
		"format":  "json", // the CLI always parses JSON; rendering is local
	}
	if reviewBase != "" {
		toolArgs["base"] = reviewBase
	}
	if reviewRepo != "" {
		toolArgs["repo"] = reviewRepo
	}
	if diffText != "" {
		toolArgs["diff"] = diffText
	}

	raw, err := reviewDaemonTool(repoPath, "review", toolArgs)
	if err != nil {
		return err
	}

	if reviewFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	return printReview(cmd, raw)
}

// runReviewPost relays a review-and-post request to the post_review daemon tool.
// The daemon derives the findings from the changeset, redacts every comment body
// before any payload leaves the machine, and refuses a public / fork PR unless
// --confirm-public is set.
func runReviewPost(cmd *cobra.Command, repoPath, diffText string) error {
	if reviewPR <= 0 {
		return fmt.Errorf("--post requires --pr <number> (the PR / MR to comment on)")
	}
	toolArgs := map[string]any{
		"number":         reviewPR,
		"scope":          reviewScope,
		"confirm_public": reviewConfirmPublic,
		"dry_run":        reviewDryRun,
		"format":         "json",
	}
	if reviewBase != "" {
		toolArgs["base"] = reviewBase
	}
	if reviewRepo != "" {
		toolArgs["repo"] = reviewRepo
	}
	if diffText != "" {
		toolArgs["diff"] = diffText
	}

	raw, err := reviewDaemonTool(repoPath, "post_review", toolArgs)
	if err != nil {
		return err
	}
	return emitDaemonJSON(cmd, raw)
}

// readReviewDiff reads the pasted-diff source: "-" reads from stdin, anything
// else is a file path.
func readReviewDiff(cmd *cobra.Command, src string) (string, error) {
	if src == "-" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("reading diff from stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading diff file %s: %w", src, err)
	}
	return string(data), nil
}

// reviewPayloadCLI mirrors the review tool's wire shape the CLI renders.
type reviewPayloadCLI struct {
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Total    int    `json:"total"`
	Comments []struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
		Rule     string `json:"rule"`
		Category string `json:"category"`
		Source   string `json:"source"`
	} `json:"comments"`
	FileRisk []struct {
		File     string `json:"file"`
		Risk     string `json:"risk"`
		Findings int    `json:"findings"`
	} `json:"file_risk"`
	// degradation / error shape
	Error string `json:"error"`
}

// printReview renders the verdict, summary, per-file risk, and the line-anchored
// inline comments grouped by file and ordered by line.
func printReview(cmd *cobra.Command, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	var p reviewPayloadCLI
	if err := json.Unmarshal(raw, &p); err != nil {
		// Unknown shape — fall back to pretty JSON rather than fail.
		return emitDaemonJSON(cmd, raw)
	}
	if p.Error != "" {
		_, _ = fmt.Fprintf(out, "review failed: %s\n", p.Error)
		return nil
	}

	verdict := p.Verdict
	if verdict == "" {
		verdict = "APPROVE"
	}
	_, _ = fmt.Fprintf(out, "Verdict: %s\n", verdict)
	if p.Summary != "" {
		_, _ = fmt.Fprintf(out, "%s\n", p.Summary)
	}

	if len(p.FileRisk) > 0 {
		_, _ = fmt.Fprintln(out, "\nFile risk:")
		for _, fr := range p.FileRisk {
			_, _ = fmt.Fprintf(out, "  %-8s %s (%d finding(s))\n", fr.Risk, fr.File, fr.Findings)
		}
	}

	if len(p.Comments) == 0 {
		_, _ = fmt.Fprintln(out, "\nNo inline findings.")
		return nil
	}

	// Group the comments by file, ordered within a file by line, so the output
	// reads like a per-file review pass.
	byFile := map[string][]int{}
	for i, c := range p.Comments {
		byFile[c.File] = append(byFile[c.File], i)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	_, _ = fmt.Fprintf(out, "\nFindings (%d):\n", len(p.Comments))
	for _, f := range files {
		idxs := byFile[f]
		sort.Slice(idxs, func(a, b int) bool {
			return p.Comments[idxs[a]].Line < p.Comments[idxs[b]].Line
		})
		_, _ = fmt.Fprintf(out, "\n%s\n", f)
		for _, i := range idxs {
			c := p.Comments[i]
			rule := c.Rule
			if c.Category != "" {
				rule = strings.TrimSpace(c.Category + "/" + c.Rule)
			}
			_, _ = fmt.Fprintf(out, "  L%-5d %-8s %s", c.Line, c.Severity, c.Message)
			if rule != "" {
				_, _ = fmt.Fprintf(out, "  [%s]", rule)
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	return nil
}
