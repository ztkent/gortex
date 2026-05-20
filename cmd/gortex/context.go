package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

var (
	contextTask       string
	contextEntryPoint string
	contextMaxSymbols int
	contextFormat     string
	contextBudget     int
	contextIndex      string
)

var contextCmd = &cobra.Command{
	Use:   "context [flags]",
	Short: "Generate a portable context briefing for a task",
	Long: `Indexes a repository, runs smart_context for the given task, and renders
the result as a self-contained markdown or JSON briefing. Use for sharing
context outside MCP — paste into Slack, PRs, docs, or other AI tools.`,
	RunE: runContext,
}

func init() {
	contextCmd.Flags().StringVarP(&contextTask, "task", "t", "", "task description (required)")
	contextCmd.Flags().StringVarP(&contextEntryPoint, "entry-point", "e", "", "symbol ID or file path to start from")
	contextCmd.Flags().IntVarP(&contextMaxSymbols, "max-symbols", "n", 5, "max symbols to include")
	contextCmd.Flags().StringVarP(&contextFormat, "format", "f", "markdown", "output format: markdown or json")
	contextCmd.Flags().IntVar(&contextBudget, "token-budget", 2000, "approximate token budget for output")
	contextCmd.Flags().StringVar(&contextIndex, "index", "", "repository path to index (default: current directory)")
	_ = contextCmd.MarkFlagRequired("task")
	rootCmd.AddCommand(contextCmd)
}

func runContext(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	repoPath := "."
	if contextIndex != "" {
		repoPath = contextIndex
	} else if len(args) > 0 {
		repoPath = args[0]
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)

	fmt.Fprintf(os.Stderr, "[gortex] indexing %s...\n", absPath)
	result, idxErr := idx.Index(absPath)
	if idxErr != nil {
		return fmt.Errorf("index: %w", idxErr)
	}
	fmt.Fprintf(os.Stderr, "[gortex] indexed %d files, %d nodes, %d edges (%dms)\n",
		result.FileCount, g.NodeCount(), g.EdgeCount(), result.DurationMs)

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)

	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules)
	srv.SetArchitecture(cfg.Architecture)
	srv.SetArtifacts(cfg.Artifacts)
	srv.SetNamedQueries(cfg.Queries)

	toolResult, err := srv.ExportContext(cmd.Context(), contextTask, contextEntryPoint, contextFormat, contextMaxSymbols, contextBudget)
	if err != nil {
		return fmt.Errorf("export_context: %w", err)
	}

	for _, content := range toolResult.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			fmt.Print(tc.Text)
		}
	}
	fmt.Println()

	return nil
}
