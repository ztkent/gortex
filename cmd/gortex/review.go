package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/review"
)

var (
	reviewScope         string
	reviewBase          string
	reviewDiff          string
	reviewUseLLM        bool
	reviewFormat        string
	reviewAudience      string
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
	reviewCmd.Flags().StringVar(&reviewAudience, "audience", "human", "text-render audience: human (readable packet) or agent (terse machine-first summary); --format json overrides")
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
		File      string `json:"file"`
		Risk      string `json:"risk"`
		Findings  int    `json:"findings"`
		Affected  int    `json:"affected"`
		Symbols   int    `json:"symbols"`
		Uncovered int    `json:"uncovered"`
	} `json:"file_risk"`
	Depth string `json:"depth"`
	Cost  *struct {
		InputTokens      int     `json:"input_tokens"`
		OutputTokens     int     `json:"output_tokens"`
		CacheReadTokens  int     `json:"cache_read_tokens"`
		CacheWriteTokens int     `json:"cache_write_tokens"`
		USD              float64 `json:"usd"`
		Estimated        bool    `json:"estimated"`
		ElapsedMs        int64   `json:"elapsed_ms"`
	} `json:"cost"`
	// degradation / error shape
	Error string `json:"error"`
}

// reviewReportFromPayload reconstructs the canonical review.ReviewReport from the
// daemon's wire payload so the CLI can drive review.RenderSummary — the single
// source of truth for both the human and agent renderings. The wire uses
// `comments` for the findings and `risk` strings for the per-file ranking; this
// rebuilds the Finding / FileRisk / CostBreakdown shapes the renderer consumes.
func reviewReportFromPayload(p reviewPayloadCLI) *review.ReviewReport {
	report := &review.ReviewReport{
		Verdict: review.Verdict(p.Verdict),
		Summary: p.Summary,
		Depth:   p.Depth,
	}
	if report.Verdict == "" {
		report.Verdict = review.VerdictApprove
	}
	for _, c := range p.Comments {
		report.Findings = append(report.Findings, review.Finding{
			File:     c.File,
			Line:     c.Line,
			Severity: review.Severity(c.Severity),
			Message:  c.Message,
			Rule:     c.Rule,
			Category: c.Category,
			Source:   c.Source,
		})
	}
	for _, fr := range p.FileRisk {
		report.FileRisk = append(report.FileRisk, review.FileRisk{
			File:      fr.File,
			Risk:      fr.Risk,
			Findings:  fr.Findings,
			Affected:  fr.Affected,
			Symbols:   fr.Symbols,
			Uncovered: fr.Uncovered,
		})
	}
	if p.Cost != nil {
		report.Cost = &review.CostBreakdown{
			InputTokens:      p.Cost.InputTokens,
			OutputTokens:     p.Cost.OutputTokens,
			CacheReadTokens:  p.Cost.CacheReadTokens,
			CacheWriteTokens: p.Cost.CacheWriteTokens,
			USD:              p.Cost.USD,
			Estimated:        p.Cost.Estimated,
			ElapsedMs:        p.Cost.ElapsedMs,
		}
	}
	return report
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

	// Both audiences render through the canonical review renderer so the CLI,
	// the MCP text rendering, and any sub-agent shelling the verb see the
	// same packet — the human path used to keep a hand-rolled duplicate here,
	// which silently dropped every field the renderer learned after it forked
	// (coverage evidence, the rulepack-passed line).
	if strings.EqualFold(reviewAudience, "agent") {
		_, _ = io.WriteString(out, review.RenderSummary(reviewReportFromPayload(p), review.AudienceAgent))
		return nil
	}
	_, _ = io.WriteString(out, review.RenderSummary(reviewReportFromPayload(p), review.AudienceHuman))
	return nil
}
