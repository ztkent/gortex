package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/forge"
	"github.com/zzet/gortex/internal/indexer"
)

var (
	prsBase      string
	prsRepo      string
	prsFormat    string
	prsWorktrees bool
	prsBundleOut string
	prsTriage    bool
	prsConflicts bool
	prsUseLLM    bool
)

// Seams. The forge free functions and the daemon-tool relay are indirected
// through package vars so a test can inject canned PRs / files and stub the
// daemon call without touching the network or a real daemon.
var (
	forgeAvailable = forge.Available
	forgeListPRs   = forge.ListPRs
	forgePRFiles   = forge.PRFiles
	prsDaemonTool  = requireDaemonTool
)

var prsCmd = &cobra.Command{
	Use:   "prs [number]",
	Short: "List open pull requests, or deep-dive a PR's blast radius",
	Long: `Without an argument, lists the repository's open pull requests as a table
with each PR's CI rollup, review decision, age, and a one-shot review-state
classification (DRAFT / BASE_MISMATCH / CHANGES_REQUESTED / APPROVED / STALE
/ READY).

With a PR number, deep-dives that PR: fetches its changed files from the
forge and joins them against the knowledge graph (via the daemon) to print
the changed files, blast radius, and risk score.

--triage renders an AI-ranked review queue (highest-risk first) via the
daemon's triage_prs tool; add --use-llm to re-rank it with one LLM pass.
--conflicts renders merge-order conflict clusters — the graph communities
touched by more than one open PR, with a suggested safe merge order — via
the daemon's conflicts_prs tool. Both honour --format json (raw payload).
When both flags are given, triage runs first, then conflicts.

Listing needs a GitHub token (GH_TOKEN or GITHUB_TOKEN). The deep-dive,
--triage, and --conflicts also need a running daemon that tracks the repo.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPRs,
}

var prsBundleCmd = &cobra.Command{
	Use:   "bundle <number>",
	Short: "Write a reviewer graph bundle for a PR (impact + receipt + reviewers)",
	Long: `Builds a self-contained, reviewer-focused bundle for a pull request and
writes it to a file (--out, default pr-<number>-bundle.json).

The bundle is the PR-review-relevant slice of the knowledge graph: the PR's
changed files, the graph-joined blast radius and PR-risk score (with a
privacy-safe review receipt), and the ranked reviewer suggestions. It joins
the forge's changed-file set against the daemon's graph via the get_pr_impact
and suggest_reviewers tools — no second in-process index.

The bundle is deterministic for an unchanged PR, so it can be uploaded as a CI
artifact and diffed across runs. A committed GitHub Action template that wires
this into per-PR CI lives at .github/workflows/gortex-pr-review.yml.example
(documented in docs/actions/README.md).

Listing needs a GitHub token (GH_TOKEN or GITHUB_TOKEN) to fetch the changed
files; the graph join needs a running daemon that tracks the repo.`,
	Args: cobra.ExactArgs(1),
	RunE: runPRsBundle,
}

func init() {
	prsCmd.Flags().StringVarP(&prsBase, "base", "b", "", "default base branch used to flag BASE_MISMATCH (default: the repo's default branch)")
	prsCmd.Flags().StringVar(&prsRepo, "repo", "", "repository path the forge / daemon must own (default: current directory)")
	prsCmd.Flags().StringVar(&prsFormat, "format", "text", "output format: text or json")
	prsCmd.Flags().BoolVar(&prsWorktrees, "worktrees", false, "annotate each PR whose head branch is checked out in a local worktree")
	prsCmd.Flags().BoolVar(&prsTriage, "triage", false, "render an AI-ranked review queue (highest-risk first) via the daemon's triage_prs tool")
	prsCmd.Flags().BoolVar(&prsConflicts, "conflicts", false, "render merge-order conflict clusters (PRs sharing a graph community) via the daemon's conflicts_prs tool")
	prsCmd.Flags().BoolVar(&prsUseLLM, "use-llm", false, "when used with --triage, re-rank the queue with one LLM pass (passes use_llm:true to triage_prs)")

	prsBundleCmd.Flags().StringVar(&prsRepo, "repo", "", "repository path the forge / daemon must own (default: current directory)")
	prsBundleCmd.Flags().StringVarP(&prsBundleOut, "out", "o", "", "bundle output file (default: pr-<number>-bundle.json)")
	prsCmd.AddCommand(prsBundleCmd)

	rootCmd.AddCommand(prsCmd)
}

func runPRs(cmd *cobra.Command, args []string) error {
	repoPath := "."
	if prsRepo != "" {
		repoPath = prsRepo
	}

	// Deep-dive: `gortex prs <N>`.
	if len(args) == 1 {
		n, err := strconv.Atoi(strings.TrimSpace(args[0]))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid PR number %q", args[0])
		}
		return runPRDeepDive(cmd, repoPath, n)
	}

	// Triage / conflicts dashboards route through the daemon tools. When both
	// flags are given, run triage first then conflicts (triage takes
	// precedence as the primary review-queue view). A missing forge token is
	// not an error: print the GH_TOKEN hint and exit 0, like the base
	// dashboard, since both daemon tools self-serve from the forge.
	if prsTriage || prsConflicts {
		if !forgeAvailable(context.Background()) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(),
				"no GitHub token found — set GH_TOKEN (or GITHUB_TOKEN) to triage pull requests")
			return nil
		}
		if prsTriage {
			if err := runPRTriage(cmd, repoPath); err != nil {
				return err
			}
		}
		if prsConflicts {
			if err := runPRConflicts(cmd, repoPath); err != nil {
				return err
			}
		}
		return nil
	}

	// Dashboard: `gortex prs`.
	return runPRList(cmd, repoPath)
}

// runPRList prints the open-PR table (or its JSON form). A missing forge
// token is not an error: it prints an actionable GH_TOKEN hint and exits 0.
func runPRList(cmd *cobra.Command, repoPath string) error {
	ctx := context.Background()

	if !forgeAvailable(ctx) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(),
			"no GitHub token found — set GH_TOKEN (or GITHUB_TOKEN) to list pull requests")
		return nil
	}

	prs, err := forgeListPRs(ctx, repoPath, forge.ListOpts{
		State:        "open",
		Limit:        30,
		WithDecision: true,
		WithCI:       true,
	})
	if err != nil {
		return fmt.Errorf("listing pull requests: %w", err)
	}

	base := resolvePRBase(repoPath)

	var worktreeBranches map[string]bool
	if prsWorktrees {
		worktreeBranches = localWorktreeBranches(ctx, repoPath)
	}

	rows := classifyPRRows(prs, base)

	if prsFormat == "json" {
		return emitPRListJSON(cmd, rows)
	}
	emitPRListTable(cmd, rows, worktreeBranches)
	return nil
}

// prRow is the projection of a classified PR onto the documented wire shape.
type prRow struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Author   string   `json:"author"`
	AgeDays  int      `json:"age_days"`
	CI       string   `json:"ci"`
	Review   string   `json:"review"`
	State    string   `json:"state"`
	Blockers []string `json:"blockers"`
	headRef  string
}

// classifyPRRows classifies every PR against the resolved default base.
func classifyPRRows(prs []forge.PR, base string) []prRow {
	rows := make([]prRow, 0, len(prs))
	for _, pr := range prs {
		st := forge.ClassifyStatus(pr, base)
		blockers := st.Blockers
		if blockers == nil {
			blockers = []string{}
		}
		rows = append(rows, prRow{
			Number:   pr.Number,
			Title:    pr.Title,
			Author:   pr.Author,
			AgeDays:  st.AgeDays,
			CI:       forge.RollupCI(pr),
			Review:   pr.ReviewDecision,
			State:    st.State,
			Blockers: blockers,
			headRef:  pr.HeadRef,
		})
	}
	return rows
}

// emitPRListJSON renders the documented {prs:[…]} shape.
func emitPRListJSON(cmd *cobra.Command, rows []prRow) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"prs": rows})
}

// emitPRListTable renders the dashboard table. When worktreeBranches is
// non-nil a PR whose head branch is locally checked out is marked.
func emitPRListTable(cmd *cobra.Command, rows []prRow, worktreeBranches map[string]bool) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No open pull requests.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	header := "#\tSTATE\tCI\tREVIEW\tAGE\tAUTHOR\tTITLE"
	if worktreeBranches != nil {
		header += "\tWORKTREE"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, r := range rows {
		review := r.Review
		if review == "" {
			review = "-"
		}
		line := fmt.Sprintf("%d\t%s\t%s\t%s\t%dd\t%s\t%s",
			r.Number, r.State, r.CI, review, r.AgeDays, r.Author, truncate(r.Title, 50))
		if worktreeBranches != nil {
			mark := ""
			if r.headRef != "" && worktreeBranches[r.headRef] {
				mark = "yes"
			}
			line += "\t" + mark
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

// runPRDeepDive fetches a PR's changed files from the forge (when a token is
// available) and runs the daemon's get_pr_impact tool, passing the file set
// so the daemon need not refetch. It prints the changed files, blast radius,
// and risk score.
func runPRDeepDive(cmd *cobra.Command, repoPath string, number int) error {
	ctx := context.Background()

	args := map[string]any{"number": number}
	if prsRepo != "" {
		args["repo"] = prsRepo
	}

	// Best-effort: pass the CLI-fetched file set so the daemon skips a
	// redundant forge fetch. When no token is resolvable we simply omit
	// `files` and let the daemon self-serve (or degrade with its own hint).
	if forgeAvailable(ctx) {
		files, err := forgePRFiles(ctx, repoPath, number)
		if err != nil {
			return fmt.Errorf("fetching PR #%d files: %w", number, err)
		}
		if encoded, merr := json.Marshal(files); merr == nil {
			args["files"] = string(encoded)
		}
	}

	raw, err := prsDaemonTool(repoPath, "get_pr_impact", args)
	if err != nil {
		return err
	}

	if prsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	return printPRImpact(cmd, number, raw)
}

// prImpactPayload mirrors the get_pr_impact wire shape the deep-dive renders.
type prImpactPayload struct {
	Number           int      `json:"number"`
	Risk             string   `json:"risk"`
	Score            float64  `json:"score"`
	ReviewPriorities []struct {
		Axis   string  `json:"axis"`
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	} `json:"review_priorities"`
	ChangedFiles   []string `json:"changed_files"`
	ChangedSymbols []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		File string `json:"file"`
	} `json:"changed_symbols"`
	Communities []string `json:"communities"`
	// degradation shape
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// printPRImpact renders the deep-dive: changed files, blast radius, and risk.
func printPRImpact(cmd *cobra.Command, number int, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	var p prImpactPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		// Unknown shape — fall back to pretty JSON rather than fail.
		return emitDaemonJSON(cmd, raw)
	}
	if p.Error != "" {
		_, _ = fmt.Fprintf(out, "PR #%d: %s", number, p.Error)
		if p.Hint != "" {
			_, _ = fmt.Fprintf(out, " — %s", p.Hint)
		}
		_, _ = fmt.Fprintln(out)
		return nil
	}

	_, _ = fmt.Fprintf(out, "PR #%d — risk %s (score %.1f)\n", p.Number, p.Risk, p.Score)

	_, _ = fmt.Fprintf(out, "\nChanged files (%d):\n", len(p.ChangedFiles))
	for _, f := range p.ChangedFiles {
		_, _ = fmt.Fprintf(out, "  %s\n", f)
	}

	_, _ = fmt.Fprintf(out, "\nBlast radius: %d changed symbol(s), %d communit(ies)\n",
		len(p.ChangedSymbols), len(p.Communities))
	for _, sym := range p.ChangedSymbols {
		_, _ = fmt.Fprintf(out, "  %-8s %s\n", sym.Kind, sym.ID)
	}

	if len(p.ReviewPriorities) > 0 {
		_, _ = fmt.Fprintln(out, "\nReview priorities:")
		for _, pr := range p.ReviewPriorities {
			_, _ = fmt.Fprintf(out, "  %-10s %5.1f  %s\n", pr.Axis, pr.Score, pr.Reason)
		}
	}
	return nil
}

// runPRTriage calls the daemon's triage_prs tool and renders an AI-ranked
// review queue (highest-risk first). --use-llm passes use_llm:true so the
// daemon re-ranks the deterministic queue with one LLM pass. --format json
// emits the raw tool payload.
func runPRTriage(cmd *cobra.Command, repoPath string) error {
	args := map[string]any{}
	if prsRepo != "" {
		args["repo"] = prsRepo
	}
	if prsUseLLM {
		args["use_llm"] = true
	}

	raw, err := prsDaemonTool(repoPath, "triage_prs", args)
	if err != nil {
		return err
	}

	if prsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	return printPRTriage(cmd, raw)
}

// runPRConflicts calls the daemon's conflicts_prs tool and renders the
// merge-order conflict clusters: the graph communities touched by more than
// one open PR, the colliding PRs, a suggested safe merge order, and a
// conflict-risk score. --format json emits the raw tool payload.
func runPRConflicts(cmd *cobra.Command, repoPath string) error {
	args := map[string]any{}
	if prsRepo != "" {
		args["repo"] = prsRepo
	}

	raw, err := prsDaemonTool(repoPath, "conflicts_prs", args)
	if err != nil {
		return err
	}

	if prsFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	return printPRConflicts(cmd, raw)
}

// triagePayload mirrors the triage_prs wire shape the queue table renders.
type triagePayload struct {
	Ranked []struct {
		Number    int     `json:"number"`
		Title     string  `json:"title"`
		Author    string  `json:"author"`
		Risk      string  `json:"risk"`
		Score     float64 `json:"score"`
		Rationale string  `json:"rationale"`
	} `json:"ranked"`
	Total   int  `json:"total"`
	LLMUsed bool `json:"llm_used"`
	// degradation shape
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// printPRTriage renders the ranked review queue: rank, PR#, risk, score, and
// title (highest-risk first — the daemon already orders the queue). When the
// LLM re-rank ran and annotated a PR, its rationale is printed beneath the row.
func printPRTriage(cmd *cobra.Command, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	var p triagePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	if p.Error != "" {
		_, _ = fmt.Fprintf(out, "triage: %s", p.Error)
		if p.Hint != "" {
			_, _ = fmt.Fprintf(out, " — %s", p.Hint)
		}
		_, _ = fmt.Fprintln(out)
		return nil
	}
	if len(p.Ranked) == 0 {
		_, _ = fmt.Fprintln(out, "No open pull requests to triage.")
		return nil
	}

	header := "Review queue (highest-risk first)"
	if p.LLMUsed {
		header += " — LLM-reranked"
	}
	_, _ = fmt.Fprintln(out, header)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RANK\t#\tRISK\tSCORE\tTITLE")
	for i, r := range p.Ranked {
		_, _ = fmt.Fprintf(tw, "%d\t%d\t%s\t%.1f\t%s\n",
			i+1, r.Number, r.Risk, r.Score, truncate(r.Title, 50))
	}
	_ = tw.Flush()

	// Print any LLM rationales below the table so the queue stays scannable.
	for _, r := range p.Ranked {
		if rat := strings.TrimSpace(r.Rationale); rat != "" {
			_, _ = fmt.Fprintf(out, "  PR #%d: %s\n", r.Number, rat)
		}
	}
	return nil
}

// conflictsPayload mirrors the conflicts_prs wire shape the cluster view renders.
type conflictsPayload struct {
	Conflicts []struct {
		Community      string  `json:"community"`
		Size           int     `json:"size"`
		PRs            []int   `json:"prs"`
		SuggestedOrder []int   `json:"suggested_order"`
		Risk           float64 `json:"risk"`
	} `json:"conflicts"`
	Total int `json:"total"`
	// degradation shape
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// printPRConflicts renders the merge-order conflict clusters: each shared
// community, the PRs that collide there, a suggested safe merge order, and
// the conflict-risk score (clusters are already ranked highest-risk first).
func printPRConflicts(cmd *cobra.Command, raw json.RawMessage) error {
	out := cmd.OutOrStdout()
	var p conflictsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	if p.Error != "" {
		_, _ = fmt.Fprintf(out, "conflicts: %s", p.Error)
		if p.Hint != "" {
			_, _ = fmt.Fprintf(out, " — %s", p.Hint)
		}
		_, _ = fmt.Fprintln(out)
		return nil
	}
	if len(p.Conflicts) == 0 {
		_, _ = fmt.Fprintln(out, "No merge-order conflicts: no community is touched by more than one open PR.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "Merge-order conflicts (%d cluster(s), highest-risk first)\n", len(p.Conflicts))
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "COMMUNITY\tSIZE\tRISK\tPRS\tMERGE ORDER")
	for _, c := range p.Conflicts {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%.2f\t%s\t%s\n",
			c.Community, c.Size, c.Risk, joinPRNumbers(c.PRs), joinPRNumbers(c.SuggestedOrder))
	}
	_ = tw.Flush()
	return nil
}

// joinPRNumbers formats a list of PR numbers as "#1, #2, #3" for the
// conflict-cluster table.
func joinPRNumbers(nums []int) string {
	if len(nums) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, "#"+strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// resolvePRBase resolves the default base branch used to flag BASE_MISMATCH:
// the explicit --base flag wins, otherwise the repo's default branch.
func resolvePRBase(repoPath string) string {
	if prsBase != "" {
		return prsBase
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	return churn.DefaultBranch(abs)
}

// localWorktreeBranches returns the set of branch names currently checked out
// in a local worktree of the repo, so the dashboard can mark a PR whose head
// is already on disk. A failure to enumerate worktrees yields an empty set.
func localWorktreeBranches(ctx context.Context, repoPath string) map[string]bool {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	// Anchor to the main checkout so a query from inside a linked worktree
	// still enumerates every sibling worktree.
	if info := indexer.ResolveWorktree(abs); info.MainRepoPath != "" {
		abs = info.MainRepoPath
	}
	branches := map[string]bool{}
	entries, err := forge.LocalWorktrees(ctx, abs)
	if err != nil {
		return branches
	}
	for _, e := range entries {
		if e.Branch != "" {
			branches[e.Branch] = true
		}
	}
	return branches
}

// bundleVersion is the on-the-wire schema version of a reviewer bundle. Bump it
// when the bundle's top-level shape changes incompatibly so consumers can gate.
const bundleVersion = 1

// reviewerBundle is the PR-review-relevant slice of the knowledge graph written
// by `gortex prs bundle`. It pairs the PR's changed file set with the
// graph-joined impact (blast radius + PR-risk score + privacy-safe receipt) and
// the ranked reviewer suggestions, so a CI job can upload one self-contained
// artifact a reviewer (or a downstream agent) consumes without a live daemon.
//
// Impact and Reviewers carry the daemon tools' payloads verbatim (get_pr_impact
// and suggest_reviewers) so the bundle has a single source of truth and no
// re-projection drift. ChangedFiles is lifted out for quick top-level scanning.
type reviewerBundle struct {
	BundleVersion int             `json:"bundle_version"`
	Number        int             `json:"number"`
	ChangedFiles  []string        `json:"changed_files"`
	Impact        json.RawMessage `json:"impact"`
	Reviewers     json.RawMessage `json:"reviewers,omitempty"`
}

// runPRsBundle builds a reviewer bundle for a PR and writes it to --out (or the
// default pr-<number>-bundle.json). It is daemon-first: the forge supplies the
// changed-file set (best-effort), and the daemon joins it against the graph via
// get_pr_impact and suggest_reviewers. The result is deterministic for an
// unchanged PR.
func runPRsBundle(cmd *cobra.Command, args []string) error {
	repoPath := "."
	if prsRepo != "" {
		repoPath = prsRepo
	}

	n, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid PR number %q", args[0])
	}

	ctx := context.Background()

	// Best-effort: fetch the changed file set so the daemon tools skip a
	// redundant forge fetch. With no token we omit it and let the daemon
	// self-serve; a daemon with no token of its own degrades inside the tool.
	var files []string
	if forgeAvailable(ctx) {
		fetched, ferr := forgePRFiles(ctx, repoPath, n)
		if ferr != nil {
			return fmt.Errorf("fetching PR #%d files: %w", n, ferr)
		}
		files = fetched
	}

	impactArgs := map[string]any{"number": n, "receipt": true}
	reviewerArgs := map[string]any{"number": n}
	if prsRepo != "" {
		impactArgs["repo"] = prsRepo
		reviewerArgs["repo"] = prsRepo
	}
	if files != nil {
		if encoded, merr := json.Marshal(files); merr == nil {
			impactArgs["files"] = string(encoded)
		}
	}

	impact, err := prsDaemonTool(repoPath, "get_pr_impact", impactArgs)
	if err != nil {
		return err
	}

	// Reviewers are best-effort: a missing forge token / CODEOWNERS file must
	// not sink the whole bundle. On any daemon error we record an empty
	// reviewers section and still write the impact slice.
	reviewers, rerr := prsDaemonTool(repoPath, "suggest_reviewers", reviewerArgs)
	if rerr != nil {
		reviewers = nil
	}

	// Pull the changed files out of the impact payload so the bundle carries an
	// authoritative top-level list even when the CLI had no token to fetch its
	// own. Fall back to the CLI-fetched set.
	changed := changedFilesFromImpact(impact)
	if len(changed) == 0 {
		changed = files
	}

	out := prsBundleOut
	if out == "" {
		out = fmt.Sprintf("pr-%d-bundle.json", n)
	}

	if err := writeReviewerBundle(out, n, changed, impact, reviewers); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote reviewer bundle for PR #%d to %s\n", n, out)
	return nil
}

// changedFilesFromImpact extracts the changed_files list from a get_pr_impact
// payload. Returns nil when the payload is a degradation envelope or otherwise
// lacks the field, so the caller can fall back to its own fetched set.
func changedFilesFromImpact(impact json.RawMessage) []string {
	if len(impact) == 0 {
		return nil
	}
	var p struct {
		ChangedFiles []string `json:"changed_files"`
	}
	if err := json.Unmarshal(impact, &p); err != nil {
		return nil
	}
	return p.ChangedFiles
}

// writeReviewerBundle assembles a reviewerBundle from the daemon tool payloads
// and writes it deterministically to path. The changed-file list is sorted and
// the JSON is indented so two runs over an unchanged PR produce byte-identical
// output (suitable for CI artifact diffing). A nil/empty reviewers payload is
// omitted from the bundle rather than written as JSON null.
func writeReviewerBundle(path string, number int, changedFiles []string, impact, reviewers json.RawMessage) error {
	if number <= 0 {
		return fmt.Errorf("invalid PR number %d", number)
	}
	if len(impact) == 0 {
		return fmt.Errorf("cannot write bundle for PR #%d: empty impact payload", number)
	}

	files := append([]string(nil), changedFiles...)
	sort.Strings(files)
	if files == nil {
		files = []string{}
	}

	b := reviewerBundle{
		BundleVersion: bundleVersion,
		Number:        number,
		ChangedFiles:  files,
		Impact:        impact,
	}
	if len(bytes.TrimSpace(reviewers)) > 0 {
		b.Reviewers = reviewers
	}

	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding reviewer bundle: %w", err)
	}
	data = append(data, '\n')

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating bundle directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing reviewer bundle to %q: %w", path, err)
	}
	return nil
}
