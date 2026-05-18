package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/docs"
	"github.com/zzet/gortex/internal/exporter"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm"
	llmprovider "github.com/zzet/gortex/internal/llm/provider"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/wiki"
)

var (
	wikiOutputDir      string
	wikiFormat         string
	wikiWikilinks      bool
	wikiRepo           string
	wikiProject        string
	wikiWorkspace      string
	wikiMinCommunity   int
	wikiMaxCommunities int
	wikiNoProcesses    bool
	wikiNoContracts    bool
	wikiNoDocs         bool
	wikiEnhance        bool
	wikiForce          bool
)

var wikiCmd = &cobra.Command{
	Use:   "wiki [path]",
	Short: "Generate a markdown wiki of the indexed graph",
	Long: `Render a multi-page markdown wiki from the indexed graph.

The wiki is template-driven (no LLM required). Pass --enhance to add
narrative summaries via the configured LLM provider — claudecli only
in the MVP. Output layout:

  wiki/
    index.md                  # top-level repo index
    <repo>/
      index.md                # community navigation
      architecture.md         # system overview
      communities/...
      processes/...
      contracts/api-surface.md
      analysis/{hotspots,cycles,semantic}.md
      _assets/community-graph.mermaid
    _workspace/               # reserved for cross-repo pages

The current working directory is indexed first; pass [path] to point
at a different repo. The graph is not persisted between runs.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runWiki,
}

func init() {
	wikiCmd.Flags().StringVarP(&wikiOutputDir, "output", "o", "wiki", "output directory")
	wikiCmd.Flags().StringVarP(&wikiFormat, "format", "f", "markdown", "output format: markdown | html")
	wikiCmd.Flags().BoolVar(&wikiWikilinks, "wikilinks", false, "use [[wikilink]] style links (Obsidian-compatible)")
	wikiCmd.Flags().StringVar(&wikiRepo, "repo", "", "per-repo slug under wiki/ (default: basename of path)")
	wikiCmd.Flags().StringVar(&wikiProject, "project", "", "project label (multi-repo mode hint)")
	wikiCmd.Flags().StringVar(&wikiWorkspace, "workspace", "", "restrict emitted nodes to this WorkspaceID")
	wikiCmd.Flags().IntVar(&wikiMinCommunity, "min-community", 3, "minimum community size to document")
	wikiCmd.Flags().IntVar(&wikiMaxCommunities, "max-communities", 20, "max number of communities to document")
	wikiCmd.Flags().BoolVar(&wikiNoProcesses, "no-processes", false, "skip process pages")
	wikiCmd.Flags().BoolVar(&wikiNoContracts, "no-contracts", false, "skip contracts page")
	wikiCmd.Flags().BoolVar(&wikiNoDocs, "no-docs", false, "skip docs bundle (changelog/ownership/stale)")
	wikiCmd.Flags().BoolVar(&wikiEnhance, "enhance", false, "use the LLM provider (claudecli MVP) to enrich narrative sections")
	wikiCmd.Flags().BoolVar(&wikiForce, "force", false, "suppress any 'already exists' diagnostics (writer is always idempotent)")
	rootCmd.AddCommand(wikiCmd)
}

func runWiki(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs %q: %w", path, err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	t0 := time.Now()
	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %q: %w", path, err)
	}
	idx.ResolveAll()
	indexDur := time.Since(t0)

	stats := g.Stats()
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"[gortex wiki] indexed %s: %d nodes, %d edges in %dms\n",
		path, stats.TotalNodes, stats.TotalEdges, indexDur.Milliseconds())

	communities := analysis.DetectCommunities(g)
	processes := analysis.DiscoverProcesses(g)
	hotspots := analysis.FindHotspots(g, communities, 0)
	cycles := analysis.DetectCycles(g, communities, "")

	var contractList []contracts.Contract
	if cr := idx.ContractRegistry(); cr != nil {
		contractList = cr.All()
	}

	repoSlug := wikiRepo
	if repoSlug == "" {
		repoSlug = wiki.RepoSlugFromPath(absPath)
	}

	// Optional: docs bundle is included unless --no-docs.
	var docsMarkdown string
	if !wikiNoDocs {
		bundle, derr := docs.Generate(docs.Deps{Graph: g}, docs.Options{
			WorkspaceID: wikiWorkspace,
		})
		if derr != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex wiki] docs bundle skipped: %v\n", derr)
		} else {
			docsMarkdown = docs.RenderMarkdown(bundle)
		}
	}

	var enhancer wiki.Enhancer
	if wikiEnhance {
		if e, err := makeWikiEnhancer(cfg, logger); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"[gortex wiki] --enhance disabled: %v (falling back to template-only)\n", err)
		} else if e != nil {
			enhancer = e
		}
	}

	gen := wiki.New(wiki.Inputs{
		Graph:        g,
		Communities:  communities,
		Processes:    processes,
		Hotspots:     hotspots,
		Cycles:       cycles,
		Contracts:    contractList,
		DocsBundle:   docsMarkdown,
	}, wiki.Options{
		OutputDir:      wikiOutputDir,
		Format:         wikiFormat,
		Wikilinks:      wikiWikilinks,
		Repo:           repoSlug,
		Project:        wikiProject,
		WorkspaceID:    wikiWorkspace,
		MinCommunity:   wikiMinCommunity,
		MaxCommunities: wikiMaxCommunities,
		NoProcesses:    wikiNoProcesses,
		NoContracts:    wikiNoContracts,
		NoDocs:         wikiNoDocs,
		Enhance:        wikiEnhance && enhancer != nil,
		Enhancer:       enhancer,
		Force:          wikiForce,
	})
	result, _, err := gen.Generate(context.Background())
	if err != nil {
		return fmt.Errorf("generate wiki: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"[gortex wiki] wrote %d files under %s\n", len(result.Files), result.OutputDir)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "open: %s/%s/index.md\n", result.OutputDir, repoSlug)

	// Silence unused-export warnings — exporter is referenced for
	// cross-package availability assertions.
	_ = exporter.Stats{}
	return nil
}

// makeWikiEnhancer constructs the LLM-backed enhancer. Only claudecli
// is wired for MVP — if the active provider isn't claudecli we still
// attempt construction, but the spec only requires claudecli to be
// supported. Errors here are returned so the CLI can fall back to
// NoopEnhancer with a warning.
func makeWikiEnhancer(cfg *config.Config, _ *zap.Logger) (wiki.Enhancer, error) {
	llmCfg := cfg.LLM.MergeEnv()
	if !llmCfg.IsEnabled() {
		return nil, fmt.Errorf("LLM provider %q is not configured", llmCfg.ProviderName())
	}
	prov, err := llmprovider.New(llmCfg)
	if err != nil {
		return nil, err
	}
	_ = llm.RoleSystem // keep llm import referenced.
	cache := wiki.NewEnhanceCache(wiki.DefaultEnhanceCacheDir())
	return wiki.NewClaudeCLIEnhancer(prov, cache), nil
}

// Keep os referenced for future use.
var _ = os.Stdin
