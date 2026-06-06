package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	"github.com/zzet/gortex/internal/llm/registry"
	llmprovider "github.com/zzet/gortex/internal/llm/provider"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
	"github.com/zzet/gortex/internal/wiki"
)

// Wiki dashboard stage labels. Kept as package vars so the orchestration
// code and the dashboard constructor share one source of truth.
var (
	stageWikiIndex    = "Index repository"
	stageWikiAnalyze  = "Analyze graph"
	stageWikiContract = "Extract contracts"
	stageWikiDocs     = "Build docs bundle"
	stageWikiPages    = "Generate pages"
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
	logger := loggerForSpinner(cmd, newLogger())
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

	w := cmd.ErrOrStderr()
	stages := buildWikiStages(wikiNoContracts, wikiNoDocs)
	dash := startWikiDashboard(w, absPath, stages)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	t0 := time.Now()
	dash.stage(stageWikiIndex, "")
	ctx := dash.context(context.Background())
	if _, err := idx.IndexCtx(ctx, path); err != nil {
		dash.fail(stageWikiIndex, err)
		return fmt.Errorf("index %q: %w", path, err)
	}
	idx.ResolveAll()
	indexDur := time.Since(t0)
	stats := g.Stats()
	dash.done(stageWikiIndex, fmt.Sprintf("%d nodes · %d edges · %dms",
		stats.TotalNodes, stats.TotalEdges, indexDur.Milliseconds()))

	dash.stage(stageWikiAnalyze, "")
	communities := analysis.DetectCommunities(g)
	processes := analysis.DiscoverProcesses(g)
	hotspots := analysis.FindHotspots(g, communities, 0)
	cycles := analysis.DetectCycles(g, communities, "")
	dash.done(stageWikiAnalyze, fmt.Sprintf("%d communities · %d processes · %d hotspots",
		len(communities.Communities), len(processes.Processes), len(hotspots)))

	var contractList []contracts.Contract
	if !wikiNoContracts {
		dash.stage(stageWikiContract, "")
		if cr := idx.ContractRegistry(); cr != nil {
			contractList = cr.All()
		}
		dash.done(stageWikiContract, fmt.Sprintf("%d contracts", len(contractList)))
	}

	repoSlug := wikiRepo
	if repoSlug == "" {
		repoSlug = wiki.RepoSlugFromPath(absPath)
	}

	var docsMarkdown string
	if !wikiNoDocs {
		dash.stage(stageWikiDocs, "")
		bundle, derr := docs.Generate(docs.Deps{Graph: g}, docs.Options{
			WorkspaceID: wikiWorkspace,
		})
		if derr != nil {
			dash.done(stageWikiDocs, "skipped: "+derr.Error())
		} else {
			docsMarkdown = docs.RenderMarkdown(bundle)
			dash.done(stageWikiDocs, fmt.Sprintf("%d bytes", len(docsMarkdown)))
		}
	}

	var enhancer wiki.Enhancer
	if wikiEnhance {
		if e, eerr := makeWikiEnhancer(cfg, logger); eerr != nil {
			dash.sub("LLM enhancer disabled: " + eerr.Error())
		} else if e != nil {
			enhancer = e
		}
	}

	dash.stage(stageWikiPages, "")
	gen := wiki.New(wiki.Inputs{
		Graph:       g,
		Communities: communities,
		Processes:   processes,
		Hotspots:    hotspots,
		Cycles:      cycles,
		Contracts:   contractList,
		DocsBundle:  docsMarkdown,
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
		dash.fail(stageWikiPages, err)
		return fmt.Errorf("generate wiki: %w", err)
	}
	dash.done(stageWikiPages, fmt.Sprintf("%d files → %s", len(result.Files), result.OutputDir))
	dash.finish(nil)

	emitWikiSummary(w, result.OutputDir, repoSlug, len(result.Files))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "open: %s/%s/index.md\n", result.OutputDir, repoSlug)

	// Silence unused-export warnings — exporter is referenced for
	// cross-package availability assertions.
	_ = exporter.Stats{}
	return nil
}

// buildWikiStages returns the ordered dashboard stage list given the per-pass
// flags. Skipped passes (no-contracts, no-docs) drop their stages so the
// dashboard doesn't show ghost rows the orchestration never advances through.
func buildWikiStages(noContracts, noDocs bool) []string {
	stages := []string{stageWikiIndex, stageWikiAnalyze}
	if !noContracts {
		stages = append(stages, stageWikiContract)
	}
	if !noDocs {
		stages = append(stages, stageWikiDocs)
	}
	stages = append(stages, stageWikiPages)
	return stages
}

// wikiDashSession bundles the running tea.Program + controller for the wiki
// dashboard. Closes cleanly on finish; methods are safe no-ops when the
// session was never opened (non-TTY callers).
type wikiDashSession struct {
	prog       *tea.Program
	controller *tui.DashboardController
	doneCh     chan struct{}
	w          io.Writer
}

// startWikiDashboard spawns the multi-pane dashboard against w when stderr is
// a TTY. Returns a controller-bearing struct in both cases; on non-TTY it
// falls back to one-line stderr prints, preserving the legacy log shape.
func startWikiDashboard(w io.Writer, repoPath string, stages []string) *wikiDashSession {
	if !progress.IsTTY(w) || noProgress {
		_, _ = fmt.Fprintf(w, "[gortex wiki] indexing %s...\n", repoPath)
		return &wikiDashSession{w: w}
	}
	// Banner first so the dashboard frame doesn't take over before the user
	// sees what's being generated.
	banner := tui.Banner{
		Title:    "gortex wiki",
		Subtitle: "Generating multi-page markdown wiki from the indexed graph.",
	}.Render()
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, banner)
	_, _ = fmt.Fprintln(w)

	model := tui.NewDashboard("gortex wiki", stages)
	prog := tea.NewProgram(model,
		tea.WithOutput(w),
		tea.WithoutSignalHandler(),
	)
	controller := tui.NewDashboardController(prog)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _ = prog.Run()
	}()
	return &wikiDashSession{prog: prog, controller: controller, doneCh: doneCh, w: w}
}

func (s *wikiDashSession) enabled() bool { return s != nil && s.controller != nil }

func (s *wikiDashSession) stage(name, sub string) {
	if s.enabled() {
		s.controller.SetActive(name, sub)
		return
	}
	_, _ = fmt.Fprintf(s.w, "[gortex wiki] %s\n", name)
}
func (s *wikiDashSession) sub(sub string) {
	if s.enabled() {
		s.controller.Sub(sub)
	}
}
func (s *wikiDashSession) done(name, sub string) {
	if s.enabled() {
		s.controller.Done(name, sub)
		return
	}
	if sub != "" {
		_, _ = fmt.Fprintf(s.w, "[gortex wiki] %s — %s\n", name, sub)
	}
}
func (s *wikiDashSession) fail(name string, err error) {
	if s.enabled() {
		s.controller.Fail(name, err)
		<-s.doneCh
		return
	}
	_, _ = fmt.Fprintf(s.w, "[gortex wiki] %s failed: %v\n", name, err)
}
func (s *wikiDashSession) finish(err error) {
	if s.enabled() {
		s.controller.Finish(err)
		<-s.doneCh
	}
}

// context attaches a progress reporter to ctx that forwards indexer stage
// events into the dashboard's current active stage as sub-labels. Falls
// through unchanged on non-TTY callers.
func (s *wikiDashSession) context(ctx context.Context) context.Context {
	if !s.enabled() {
		return ctx
	}
	return progress.WithReporter(ctx, &wikiDashReporter{ctl: s.controller})
}

type wikiDashReporter struct {
	ctl *tui.DashboardController
}

func (r *wikiDashReporter) Report(stage string, current, total int) {
	if r == nil || r.ctl == nil || stage == "" {
		return
	}
	sub := stage
	switch {
	case total > 0:
		sub = fmt.Sprintf("%s · %d / %d", stage, current, total)
	case current > 0:
		sub = fmt.Sprintf("%s · %d", stage, current)
	}
	r.ctl.Sub(sub)
}

// emitWikiSummary prints the post-generation card. TTY-only — non-TTY callers
// already saw the "[gortex wiki] Generate pages — N files → out/" line from
// the dashboard fallback.
func emitWikiSummary(w io.Writer, outDir, repoSlug string, files int) {
	if !progress.IsTTY(w) || noProgress {
		_, _ = fmt.Fprintf(w, "[gortex wiki] wrote %d files under %s\n", files, outDir)
		return
	}
	_, _ = fmt.Fprintln(w)
	stats := []string{
		progress.Stat(strconv.Itoa(files), "files written", progress.StatGood),
	}
	_, _ = fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("wiki generated"))
	_, _ = fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	_, _ = fmt.Fprintln(w, "     "+progress.Row("output", outDir, 8))
	_, _ = fmt.Fprintln(w, "     "+progress.Row("entry", outDir+"/"+repoSlug+"/index.md", 8))
	_, _ = fmt.Fprintln(w)
}

// makeWikiEnhancer constructs the LLM-backed enhancer. Only claudecli
// is wired for MVP — if the active provider isn't claudecli we still
// attempt construction, but the spec only requires claudecli to be
// supported. Errors here are returned so the CLI can fall back to
// NoopEnhancer with a warning.
func makeWikiEnhancer(cfg *config.Config, _ *zap.Logger) (wiki.Enhancer, error) {
	llmCfg := cfg.LLM.MergeEnv()
	llmCfg, _ = registry.Augment(llmCfg)
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
