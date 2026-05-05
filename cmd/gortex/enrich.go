package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/releases"
)

var enrichCmd = &cobra.Command{
	Use:   "enrich",
	Short: "Run one-shot enrichments (blame, coverage) against an indexed repo",
	Long: `Enrich indexes a repository in-process and stamps additional metadata
onto graph nodes from external data sources — git blame for authorship,
Go cover profiles for test coverage. Useful for CI pipelines or one-off
snapshots where the daemon isn't running. Equivalent to invoking the
` + "`analyze kind=blame`" + ` / ` + "`analyze kind=coverage`" + ` MCP tools against a fresh
index.`,
}

var (
	enrichBlameSnapshot    string
	enrichCoverageSnapshot string
	enrichReleasesSnapshot string

	enrichAllSnapshot string
	enrichAllBlame    bool
	enrichAllReleases bool
	enrichAllProfile  string
)

var enrichBlameCmd = &cobra.Command{
	Use:   "blame [path]",
	Short: "Stamp meta.last_authored on every symbol via git blame",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEnrichBlame,
}

var enrichCoverageCmd = &cobra.Command{
	Use:   "coverage <profile> [path]",
	Short: "Stamp meta.coverage_pct on every symbol from a Go cover profile",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runEnrichCoverage,
}

var enrichReleasesCmd = &cobra.Command{
	Use:   "releases [path]",
	Short: "Stamp meta.added_in on every file from git tag history",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEnrichReleases,
}

var enrichAllCmd = &cobra.Command{
	Use:   "all [path]",
	Short: "Index once and run multiple enrichments in a single pass",
	Long: `Combined enrichment that indexes the target path once, then runs
the requested enrichments against the same in-memory graph. Avoids
the ~3x indexing cost of running blame, coverage, and releases as
three separate subcommand invocations.

By default runs blame and releases (both git-only, no extra data
needed). Pass --coverage <profile> to also run coverage enrichment.
Each enrichment is independently optional via --no-blame /
--no-releases flags should you want a subset.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnrichAll,
}

func init() {
	enrichBlameCmd.Flags().StringVar(&enrichBlameSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichCoverageCmd.Flags().StringVar(&enrichCoverageSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichReleasesCmd.Flags().StringVar(&enrichReleasesSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichAllCmd.Flags().StringVar(&enrichAllSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichAllCmd.Flags().BoolVar(&enrichAllBlame, "blame", true,
		"run blame enrichment (default: on)")
	enrichAllCmd.Flags().BoolVar(&enrichAllReleases, "releases", true,
		"run releases enrichment (default: on)")
	enrichAllCmd.Flags().StringVar(&enrichAllProfile, "coverage", "",
		"path to a Go cover.out profile — coverage enrichment is skipped when empty")
	enrichCmd.AddCommand(enrichBlameCmd)
	enrichCmd.AddCommand(enrichCoverageCmd)
	enrichCmd.AddCommand(enrichReleasesCmd)
	enrichCmd.AddCommand(enrichAllCmd)
	rootCmd.AddCommand(enrichCmd)
}

func runEnrichAll(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) >= 1 {
		path = args[0]
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, loggerForSpinner(cmd, logger))

	if err := indexWithSpinner(cmd, idx, path); err != nil {
		return err
	}

	result := map[string]any{
		"root": idx.RootPath(),
	}

	if enrichAllBlame {
		sp := newCLISpinner(cmd, "Stamping blame")
		count, err := blame.EnrichGraph(g, idx.RootPath())
		if err != nil {
			sp.Fail(err)
			return fmt.Errorf("blame: %w", err)
		}
		sp.Set("", fmt.Sprintf("%d nodes stamped", count))
		sp.Done()
		result["blame_enriched"] = count
	}
	if enrichAllReleases {
		sp := newCLISpinner(cmd, "Stamping releases")
		count, err := releases.EnrichGraph(g, idx.RootPath())
		if err != nil {
			sp.Fail(err)
			return fmt.Errorf("releases: %w", err)
		}
		sp.Set("", fmt.Sprintf("%d files stamped", count))
		sp.Done()
		result["releases_enriched"] = count
	}
	if enrichAllProfile != "" {
		sp := newCLISpinner(cmd, "Stamping coverage")
		sp.Set("", enrichAllProfile)
		segments, err := coverage.ParseFile(enrichAllProfile)
		if err != nil {
			sp.Fail(err)
			return fmt.Errorf("read profile: %w", err)
		}
		modulePath := coverage.ReadModulePath(idx.RootPath())
		count := coverage.EnrichGraph(g, segments, modulePath)
		sp.Set("", fmt.Sprintf("%d symbols · %d segments", count, len(segments)))
		sp.Done()
		result["coverage_enriched"] = count
		result["coverage_segments"] = len(segments)
	}

	if enrichAllSnapshot != "" {
		if err := saveSnapshotTo(g, nil, nil, "gortex-enrich-all", enrichAllSnapshot, logger); err != nil {
			return fmt.Errorf("write snapshot %s: %w", enrichAllSnapshot, err)
		}
		result["snapshot"] = enrichAllSnapshot
	}
	return printEnrichResult(result)
}

func runEnrichReleases(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) >= 1 {
		path = args[0]
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, loggerForSpinner(cmd, logger))

	if err := indexWithSpinner(cmd, idx, path); err != nil {
		return err
	}

	sp := newCLISpinner(cmd, "Stamping releases")
	count, err := releases.EnrichGraph(g, idx.RootPath())
	if err != nil {
		sp.Fail(err)
		return fmt.Errorf("releases: %w", err)
	}
	sp.Set("", fmt.Sprintf("%d files stamped", count))
	sp.Done()

	result := map[string]any{
		"enriched": count,
		"root":     idx.RootPath(),
	}
	if enrichReleasesSnapshot != "" {
		if err := saveSnapshotTo(g, nil, nil, "gortex-enrich-releases", enrichReleasesSnapshot, logger); err != nil {
			return fmt.Errorf("write snapshot %s: %w", enrichReleasesSnapshot, err)
		}
		result["snapshot"] = enrichReleasesSnapshot
	}
	return printEnrichResult(result)
}

func runEnrichBlame(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) >= 1 {
		path = args[0]
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, loggerForSpinner(cmd, logger))

	if err := indexWithSpinner(cmd, idx, path); err != nil {
		return err
	}

	sp := newCLISpinner(cmd, "Stamping blame")
	count, err := blame.EnrichGraph(g, idx.RootPath())
	if err != nil {
		sp.Fail(err)
		return fmt.Errorf("blame: %w", err)
	}
	sp.Set("", fmt.Sprintf("%d nodes stamped", count))
	sp.Done()

	result := map[string]any{
		"enriched": count,
		"root":     idx.RootPath(),
	}
	if enrichBlameSnapshot != "" {
		if err := saveSnapshotTo(g, nil, nil, "gortex-enrich-blame", enrichBlameSnapshot, logger); err != nil {
			return fmt.Errorf("write snapshot %s: %w", enrichBlameSnapshot, err)
		}
		result["snapshot"] = enrichBlameSnapshot
	}
	return printEnrichResult(result)
}

func runEnrichCoverage(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	profilePath := args[0]
	path := "."
	if len(args) >= 2 {
		path = args[1]
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, loggerForSpinner(cmd, logger))

	if err := indexWithSpinner(cmd, idx, path); err != nil {
		return err
	}

	sp := newCLISpinner(cmd, "Stamping coverage")
	sp.Set("", profilePath)
	segments, err := coverage.ParseFile(profilePath)
	if err != nil {
		sp.Fail(err)
		return fmt.Errorf("read profile: %w", err)
	}
	modulePath := coverage.ReadModulePath(idx.RootPath())
	count := coverage.EnrichGraph(g, segments, modulePath)
	sp.Set("", fmt.Sprintf("%d symbols · %d segments", count, len(segments)))
	sp.Done()

	result := map[string]any{
		"enriched":    count,
		"segments":    len(segments),
		"profile":     profilePath,
		"module_path": modulePath,
		"root":        idx.RootPath(),
	}
	if enrichCoverageSnapshot != "" {
		if err := saveSnapshotTo(g, nil, nil, "gortex-enrich-coverage", enrichCoverageSnapshot, logger); err != nil {
			return fmt.Errorf("write snapshot %s: %w", enrichCoverageSnapshot, err)
		}
		result["snapshot"] = enrichCoverageSnapshot
	}
	return printEnrichResult(result)
}

// printEnrichResult emits the enrichment summary as JSON when stdout
// is captured by a script and as a one-line human-readable text
// when invoked interactively. On a terminal we keep stdout quiet — the
// spinner already showed the per-pass count — and just caption the root /
// snapshot path. On a pipe / redirect we still emit JSON for scripts.
func printEnrichResult(payload map[string]any) error {
	if progress.IsTTY(os.Stdout) {
		if v, ok := payload["root"]; ok {
			_, _ = fmt.Fprintln(os.Stdout, "  "+progress.Caption("root: "+fmt.Sprint(v)))
		}
		if v, ok := payload["snapshot"]; ok {
			_, _ = fmt.Fprintln(os.Stdout, "  "+progress.Caption("snapshot: "+fmt.Sprint(v)))
		}
		if v, ok := payload["profile"]; ok {
			_, _ = fmt.Fprintln(os.Stdout, "  "+progress.Caption("profile: "+fmt.Sprint(v)))
		}
		return nil
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
