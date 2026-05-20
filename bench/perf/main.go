// Command perf runs the reference-repo perf benchmark. See
// bench/perf/README.md for the contract; this is the substrate
// `gortex bench perf` shells out to (mirroring the bench/wire-format
// pattern).
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// budgets carries the per-metric pass/fail thresholds. Zero values
// disable a check, so partial budgets (e.g. only the impact-p95
// claim) work out of the box.
type budgets struct {
	ColdIndexMs float64
	SearchP95Ms float64
	ImpactP95Ms float64
}

var benchCacheDir string

func main() {
	repos := flag.String("repos", "gin,nestjs,react", "comma-separated repo set. Forms: preset slug (gin/nestjs/react/linux), owner/repo, https URL, or local:/path")
	includeLinux := flag.Bool("include-linux", false, "include the linux kernel preset (multi-GB clone; skipped by default)")
	cacheDir := flag.String("cache-dir", "", "cache directory for clones (default ~/.cache/gortex/bench)")
	queriesPath := flag.String("queries", "bench/perf/queries.json", "JSON file with the search-bench query set")
	out := flag.String("out", "", "output table path (default stdout)")
	format := flag.String("format", "markdown", "markdown | csv | json")
	csvOut := flag.String("csv", "", "optional CSV output path (writes in addition to --out)")
	jsonOut := flag.String("json", "", "optional JSON output path (writes in addition to --out)")
	budgetImpactMs := flag.Float64("budget-impact-p95-ms", 1.0, "fail when impact p95 exceeds this (0 disables)")
	budgetSearchMs := flag.Float64("budget-search-p95-ms", 50.0, "fail when search p95 exceeds this (0 disables)")
	budgetIndexMs := flag.Float64("budget-cold-index-ms", 0, "fail when cold-index exceeds this (0 disables, default off — repos vary too widely for a one-size budget)")
	strict := flag.Bool("strict", false, "exit 1 when any repo trips a budget gate")
	flag.Parse()

	// Resolve cache dir up front so runner.go can read it as a
	// package global (avoids threading through every helper).
	benchCacheDir = resolveCacheDir(*cacheDir)

	// Load query set.
	queries, err := loadQueries(*queriesPath)
	if err != nil {
		die("queries: %v", err)
	}
	if len(queries) == 0 {
		die("queries: empty set at %s", *queriesPath)
	}

	// Resolve repo specs.
	var specs []repoSpec
	if strings.TrimSpace(*repos) == "" {
		specs = defaultRepoSet(*includeLinux)
	} else {
		for tok := range strings.SplitSeq(*repos, ",") {
			s, err := resolveRepoSpec(tok)
			if err != nil {
				die("--repos: %v", err)
			}
			specs = append(specs, s)
		}
	}
	if !*includeLinux {
		filtered := make([]repoSpec, 0, len(specs))
		for _, s := range specs {
			if s.Slug == "linux" {
				continue
			}
			filtered = append(filtered, s)
		}
		specs = filtered
	}
	if len(specs) == 0 {
		die("no repos to benchmark (after --include-linux filter)")
	}

	// Run each repo.
	rows := make([]repoRow, 0, len(specs))
	for _, spec := range specs {
		fmt.Fprintf(os.Stderr, "[perf] %s ... ", spec.Slug)
		t := time.Now()
		row := runRepo(spec, queries, budgets{
			ColdIndexMs: *budgetIndexMs,
			SearchP95Ms: *budgetSearchMs,
			ImpactP95Ms: *budgetImpactMs,
		})
		fmt.Fprintf(os.Stderr, "%.1fs\n", time.Since(t).Seconds())
		rows = append(rows, row)
	}

	// Render primary output.
	var primary []byte
	switch strings.ToLower(*format) {
	case "markdown", "md":
		primary = []byte(renderMarkdown(rows))
	case "csv":
		primary = []byte(renderCSV(rows))
	case "json":
		primary = mustMarshalJSON(rows)
	default:
		die("unknown --format %q (markdown | csv | json)", *format)
	}
	if err := writeOutput(*out, primary); err != nil {
		die("write primary output: %v", err)
	}

	// Optional companion outputs.
	if *csvOut != "" {
		if err := writeOutput(*csvOut, []byte(renderCSV(rows))); err != nil {
			die("write csv: %v", err)
		}
	}
	if *jsonOut != "" {
		if err := writeOutput(*jsonOut, mustMarshalJSON(rows)); err != nil {
			die("write json: %v", err)
		}
	}

	// Budget gate.
	if *strict {
		total := 0
		for _, r := range rows {
			total += r.BudgetViolations
		}
		if total > 0 {
			die("perf-budget: %d violation(s) across %d repos", total, len(rows))
		}
	}
}

// resolveCacheDir picks the cache directory, honouring --cache-dir,
// then GORTEX_BENCH_CACHE, then a sensible default under the user
// cache root.
func resolveCacheDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("GORTEX_BENCH_CACHE"); v != "" {
		return v
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "gortex", "bench")
	}
	return filepath.Join(os.TempDir(), "gortex-bench")
}

// loadQueries reads the JSON query set. The file shape is
// {"queries": [...]}; an unrecognised top-level shape returns an
// error so a typo doesn't silently zero out the bench.
func loadQueries(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Queries, nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "perf-bench: "+format+"\n", args...)
	os.Exit(1)
}

func writeOutput(path string, body []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(body)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// --- rendering ------------------------------------------------------

// renderMarkdown produces the published table shape. Columns:
// cold-index, search p95, impact p95/p99, incremental, DB size —
// plus loc / files / nodes / edges as context.
func renderMarkdown(rows []repoRow) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Reference-repo perf benchmark")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| repo | LoC | files | nodes | edges | cold-index | search p95 | impact p95 | impact p99 | incremental | DB size | RSS | budget |")
	fmt.Fprintln(&b, "|------|----:|------:|------:|------:|-----------:|-----------:|-----------:|-----------:|------------:|--------:|----:|:------:|")
	for _, r := range rows {
		if r.Error != "" && r.Files == 0 {
			fmt.Fprintf(&b, "| %s | — | — | — | — | — | — | — | — | — | — | — | ✗ (%s) |\n", r.Slug, r.Error)
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Slug,
			humanInt(int64(r.LoC)),
			humanInt(int64(r.Files)),
			humanInt(int64(r.Nodes)),
			humanInt(int64(r.Edges)),
			fmtMs(r.ColdIndexMs),
			fmtMs(r.SearchP95Ms),
			fmtMs(r.ImpactP95Ms),
			fmtMs(r.ImpactP99Ms),
			fmtMs(r.IncrementalMs),
			fmtBytes(r.DBBytes),
			fmtBytes(r.RSSBytes),
			budgetMark(r),
		)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, summaryLine(rows))
	return b.String()
}

func renderCSV(rows []repoRow) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{
		"slug", "path", "loc", "files", "nodes", "edges",
		"cold_index_ms", "search_p50_ms", "search_p95_ms",
		"impact_p50_ms", "impact_p95_ms", "impact_p99_ms",
		"incremental_ms", "db_bytes", "rss_bytes", "budget_violations", "skipped", "error",
	})
	for _, r := range rows {
		_ = w.Write([]string{
			r.Slug, r.Path,
			fmt.Sprintf("%d", r.LoC),
			fmt.Sprintf("%d", r.Files),
			fmt.Sprintf("%d", r.Nodes),
			fmt.Sprintf("%d", r.Edges),
			fmt.Sprintf("%.3f", r.ColdIndexMs),
			fmt.Sprintf("%.3f", r.SearchP50Ms),
			fmt.Sprintf("%.3f", r.SearchP95Ms),
			fmt.Sprintf("%.3f", r.ImpactP50Ms),
			fmt.Sprintf("%.3f", r.ImpactP95Ms),
			fmt.Sprintf("%.3f", r.ImpactP99Ms),
			fmt.Sprintf("%.3f", r.IncrementalMs),
			fmt.Sprintf("%d", r.DBBytes),
			fmt.Sprintf("%d", r.RSSBytes),
			fmt.Sprintf("%d", r.BudgetViolations),
			r.Skipped, r.Error,
		})
	}
	w.Flush()
	return b.String()
}

func mustMarshalJSON(rows []repoRow) []byte {
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		die("marshal json: %v", err)
	}
	return append(b, '\n')
}

// --- formatting helpers ---------------------------------------------

func fmtMs(v float64) string {
	switch {
	case v == 0:
		return "—"
	case v < 1.0:
		return fmt.Sprintf("%.2fms", v)
	case v < 1000:
		return fmt.Sprintf("%.1fms", v)
	default:
		return fmt.Sprintf("%.2fs", v/1000.0)
	}
}

func fmtBytes(b int64) string {
	if b <= 0 {
		return "—"
	}
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(b)/1024.0)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1fMB", float64(b)/(1024.0*1024.0))
	default:
		return fmt.Sprintf("%.2fGB", float64(b)/(1024.0*1024.0*1024.0))
	}
}

func budgetMark(r repoRow) string {
	if r.Error != "" {
		return "✗"
	}
	if r.BudgetViolations > 0 {
		return fmt.Sprintf("⚠ %d", r.BudgetViolations)
	}
	return "✓"
}

// humanInt renders an int64 with thousands separators.
func humanInt(n int64) string {
	if n < 0 {
		return "-" + humanInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	prefix := len(s) % 3
	if prefix > 0 {
		b.WriteString(s[:prefix])
		if len(s) > prefix {
			b.WriteByte(',')
		}
	}
	for i := prefix; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// summaryLine condenses the run into a single headline.
func summaryLine(rows []repoRow) string {
	// A run is "successful" when it produced index numbers — Files
	// rather than LoC because the LoC counter relies on Meta["lines"]
	// being populated by the parser, which isn't universal. Files > 0
	// is a cheaper invariant that catches the same failures.
	successful := make([]repoRow, 0, len(rows))
	for _, r := range rows {
		if r.Error == "" && r.Files > 0 {
			successful = append(successful, r)
		}
	}
	if len(successful) == 0 {
		return "_no successful runs_"
	}

	medianFloat := func(values []float64) float64 {
		if len(values) == 0 {
			return 0
		}
		sort.Float64s(values)
		return values[len(values)/2]
	}
	colds := make([]float64, len(successful))
	searches := make([]float64, len(successful))
	impacts := make([]float64, len(successful))
	for i, r := range successful {
		colds[i] = r.ColdIndexMs
		searches[i] = r.SearchP95Ms
		impacts[i] = r.ImpactP95Ms
	}

	totalViol := 0
	for _, r := range rows {
		totalViol += r.BudgetViolations
	}
	mark := "✓"
	if totalViol > 0 {
		mark = fmt.Sprintf("⚠ %d budget violation(s)", totalViol)
	}
	return fmt.Sprintf("**Summary:** %d/%d repos succeeded. Median cold-index: %s. Median search p95: %s. Median impact p95: %s. %s.",
		len(successful), len(rows),
		fmtMs(medianFloat(colds)),
		fmtMs(medianFloat(searches)),
		fmtMs(medianFloat(impacts)),
		mark,
	)
}
