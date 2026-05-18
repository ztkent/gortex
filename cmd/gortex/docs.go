package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/docs"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var (
	docsOut         string
	docsSince       time.Duration
	docsTop         int
	docsInclude     []string
	docsFormat      string
	docsPathPrefix  string
	docsWorkspace   string
	docsIncludeRun  bool
)

var docsCmd = &cobra.Command{
	Use:   "docs [path]",
	Short: "Generate a docs bundle (recent changes + ownership + stale code + blame)",
	Long: `Produce a markdown (or JSON) bundle of the four "living changelog"
sections: recent file changes (requires --watch on the daemon), per-
author ownership, stale code older than 365 days, and an on-demand
blame re-run.

The current working directory is indexed first. Without --watch on
the indexer the recent-changes section will be empty — point this at
the running daemon's watcher when you need that data.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDocs,
}

func init() {
	docsCmd.Flags().StringVarP(&docsOut, "out", "o", "", `output path; empty or "-" → stdout`)
	docsCmd.Flags().DurationVar(&docsSince, "since", 7*24*time.Hour, "include recent changes within this window")
	docsCmd.Flags().IntVar(&docsTop, "top", 20, "cap each section's row count")
	docsCmd.Flags().StringSliceVar(&docsInclude, "include", []string{"recent", "ownership", "stale", "blame"},
		"sections to include (comma-separated)")
	docsCmd.Flags().StringVarP(&docsFormat, "format", "f", "markdown", "output format: markdown | json")
	docsCmd.Flags().StringVar(&docsPathPrefix, "path-prefix", "", "filter ownership/stale to this file prefix")
	docsCmd.Flags().StringVar(&docsWorkspace, "workspace", "", "restrict nodes to this WorkspaceID")
	docsCmd.Flags().BoolVar(&docsIncludeRun, "run-blame", false, "re-run git blame across the indexed repo before rendering")
	rootCmd.AddCommand(docsCmd)
}

func runDocs(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) == 1 {
		path = args[0]
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %q: %w", path, err)
	}
	idx.ResolveAll()

	// Blame enrichment (so ownership / stale tables have data). The
	// flag controls whether we *re-run* blame; we always *try* to
	// enrich at least once so the tables aren't empty.
	if _, err := blame.EnrichGraph(g, path); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex docs] blame enrichment skipped: %v\n", err)
	}

	bundle, err := docs.Generate(docs.Deps{
		Graph: g,
		Blame: docsBlameRunner(g, path),
	}, docs.Options{
		Since:        docsSince,
		Top:          docsTop,
		Sections:     normalizeSections(docsInclude),
		PathPrefix:   docsPathPrefix,
		WorkspaceID:  docsWorkspace,
		IncludeBlame: docsIncludeRun,
	})
	if err != nil {
		return fmt.Errorf("generate docs bundle: %w", err)
	}

	var body string
	switch strings.ToLower(docsFormat) {
	case "json":
		data, err := docs.RenderJSON(bundle)
		if err != nil {
			return err
		}
		body = string(data) + "\n"
	default:
		body = docs.RenderMarkdown(bundle)
	}

	if docsOut == "" || docsOut == "-" {
		_, err = cmd.OutOrStdout().Write([]byte(body))
		return err
	}
	if err := os.WriteFile(docsOut, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", docsOut, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex docs] wrote %d bytes to %s\n", len(body), docsOut)
	return nil
}

func normalizeSections(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		for _, part := range strings.Split(s, ",") {
			p := strings.TrimSpace(strings.ToLower(part))
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// docsBlameRunner returns a closure that re-runs blame across the
// indexed repo. Called by docs.Generate only when IncludeBlame is true.
func docsBlameRunner(g *graph.Graph, repoRoot string) docs.BlameRunner {
	return func() (int, map[string]int, error) {
		n, err := blame.EnrichGraph(g, repoRoot)
		if err != nil {
			return 0, nil, err
		}
		return n, map[string]int{"": n}, nil
	}
}
