package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
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

func init() {
	enrichBlameCmd.Flags().StringVar(&enrichBlameSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichCoverageCmd.Flags().StringVar(&enrichCoverageSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichReleasesCmd.Flags().StringVar(&enrichReleasesSnapshot, "snapshot", "",
		"write the enriched graph as a gob.gz snapshot to this path")
	enrichCmd.AddCommand(enrichBlameCmd)
	enrichCmd.AddCommand(enrichCoverageCmd)
	enrichCmd.AddCommand(enrichReleasesCmd)
	rootCmd.AddCommand(enrichCmd)
}

func runEnrichReleases(_ *cobra.Command, args []string) error {
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
	idx := indexer.New(g, reg, cfg.Index, logger)

	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %s: %w", path, err)
	}

	count, err := releases.EnrichGraph(g, idx.RootPath())
	if err != nil {
		return fmt.Errorf("releases: %w", err)
	}

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

func runEnrichBlame(_ *cobra.Command, args []string) error {
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
	idx := indexer.New(g, reg, cfg.Index, logger)

	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %s: %w", path, err)
	}

	count, err := blame.EnrichGraph(g, idx.RootPath())
	if err != nil {
		return fmt.Errorf("blame: %w", err)
	}

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

func runEnrichCoverage(_ *cobra.Command, args []string) error {
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
	idx := indexer.New(g, reg, cfg.Index, logger)

	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %s: %w", path, err)
	}

	segments, err := coverage.ParseFile(profilePath)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	modulePath := coverage.ReadModulePath(idx.RootPath())
	count := coverage.EnrichGraph(g, segments, modulePath)

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
// when invoked interactively. Today we always emit JSON — keeps
// the parse path simple and matches the format the matching MCP
// analyze tools return.
func printEnrichResult(payload map[string]any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
