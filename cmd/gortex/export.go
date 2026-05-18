package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/exporter"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var (
	exportFormat         string
	exportOut            string
	exportOutDir         string
	exportRepo           string
	exportKinds          []string
	exportLanguages      []string
	exportDropSynthetic  bool
	exportMermaidScope   string
	exportMermaidMinComm int
	exportMermaidMaxComm int
	exportOnCommit       bool
)

var exportCmd = &cobra.Command{
	Use:   "export [path]",
	Short: "Export the graph to Cypher (Neo4j/Memgraph) or GraphML (yEd/Gephi/Cytoscape)",
	Long: `Export the in-memory graph to a portable file for visualization or
external query. The current working directory is indexed first; pass [path] to
index a specific repo. The graph is not persisted between runs — each export
re-indexes.

Loading a Cypher export into Neo4j:
  cypher-shell -u neo4j -p <password> -f graph.cypher

Loading a Cypher export into Memgraph (Docker):
  docker run -it -p 7687:7687 -p 3000:3000 memgraph/memgraph-platform
  # then in Memgraph Lab → Query Modules → Run query: load the file

Loading a GraphML export into Gephi: File → Open → graph.graphml
`,
	Args: cobra.MaximumNArgs(1),
	RunE: runExport,
}

func init() {
	exportCmd.Flags().StringVar(&exportFormat, "format", "cypher", "output format: cypher | graphml | mermaid")
	exportCmd.Flags().StringVar(&exportOut, "out", "", "output file (default: stdout)")
	exportCmd.Flags().StringVar(&exportOutDir, "out-dir", "",
		"output directory (mermaid scope=all writes one file per scope here)")
	exportCmd.Flags().StringVar(&exportRepo, "repo", "", "filter to one repo prefix (default: all)")
	exportCmd.Flags().StringSliceVar(&exportKinds, "kinds", nil,
		"comma-separated node kinds to include (function,method,field,type,interface,...). Default: all.")
	exportCmd.Flags().StringSliceVar(&exportLanguages, "languages", nil,
		"comma-separated languages to include. Default: all.")
	exportCmd.Flags().BoolVar(&exportDropSynthetic, "no-synthetic", false,
		"drop synthetic stub nodes for unresolved/external/annotation endpoints (default: keep them so call topology stays intact)")
	exportCmd.Flags().StringVar(&exportMermaidScope, "scope", "architecture",
		"(mermaid) diagram scope: architecture | communities | processes | all")
	exportCmd.Flags().IntVar(&exportMermaidMinComm, "min-community", 3,
		"(mermaid) minimum community size to include")
	exportCmd.Flags().IntVar(&exportMermaidMaxComm, "max-communities", 20,
		"(mermaid) maximum communities to include")
	exportCmd.Flags().BoolVar(&exportOnCommit, "on-commit", false,
		"informational marker: this run was triggered by a post-commit hook")
	rootCmd.AddCommand(exportCmd)
}

func runExport(_ *cobra.Command, args []string) error {
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

	indexStart := time.Now()
	if _, err := idx.IndexCtx(context.Background(), path); err != nil {
		return fmt.Errorf("index %q: %w", path, err)
	}
	idx.ResolveAll()
	indexDuration := time.Since(indexStart)

	stats := g.Stats()
	_, _ = fmt.Fprintf(os.Stderr, "[gortex export] indexed %s: %d nodes, %d edges in %dms\n",
		path, stats.TotalNodes, stats.TotalEdges, indexDuration.Milliseconds())

	opts := exporter.Options{
		Repo:          exportRepo,
		Languages:     exportLanguages,
		DropSynthetic: exportDropSynthetic,
	}
	for _, k := range exportKinds {
		opts.Kinds = append(opts.Kinds, graph.NodeKind(strings.ToLower(strings.TrimSpace(k))))
	}

	format := strings.ToLower(exportFormat)
	mermaidOpts := exporter.MermaidOpts{
		Scope:          exportMermaidScope,
		MaxCommunities: exportMermaidMaxComm,
		MinCommunity:   exportMermaidMinComm,
		Kinds:          opts.Kinds,
		Languages:      opts.Languages,
	}

	// Mermaid multi-file (scope=all + --out-dir) path: writes one
	// file per scope into out-dir.
	if format == "mermaid" && exportOutDir != "" {
		exportStart := time.Now()
		scopes := []string{"architecture", "communities", "processes"}
		if exportMermaidScope != "all" {
			scopes = []string{exportMermaidScope}
		}
		if err := os.MkdirAll(exportOutDir, 0o755); err != nil {
			return fmt.Errorf("mkdir out-dir: %w", err)
		}
		var total exporter.Stats
		for _, sc := range scopes {
			scopeOpts := mermaidOpts
			scopeOpts.Scope = sc
			path := exportOutDir + "/" + sc + ".mermaid"
			f, err := os.Create(path)
			if err != nil {
				return fmt.Errorf("create %q: %w", path, err)
			}
			st, err := exporter.WriteMermaid(f, g, scopeOpts)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("write mermaid %s: %w", sc, err)
			}
			total.NodesWritten += st.NodesWritten
			total.EdgesWritten += st.EdgesWritten
			total.BytesWritten += st.BytesWritten
		}
		_, _ = fmt.Fprintf(os.Stderr,
			"[gortex export] mermaid: wrote %d files under %s (%d bytes) in %dms\n",
			len(scopes), exportOutDir, total.BytesWritten, time.Since(exportStart).Milliseconds())
		return nil
	}

	out, closeFn, err := openOutput(exportOut)
	if err != nil {
		return fmt.Errorf("open output: %w", err)
	}
	defer closeFn()

	exportStart := time.Now()
	var exportStats exporter.Stats
	switch format {
	case "cypher":
		exportStats, err = exporter.WriteCypher(out, g, opts)
	case "graphml":
		exportStats, err = exporter.WriteGraphML(out, g, opts)
	case "mermaid":
		exportStats, err = exporter.WriteMermaid(out, g, mermaidOpts)
	default:
		return fmt.Errorf("unknown format %q (expected cypher | graphml | mermaid)", exportFormat)
	}
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	exportDuration := time.Since(exportStart)

	dest := exportOut
	if dest == "" {
		dest = "stdout"
	}
	_, _ = fmt.Fprintf(os.Stderr,
		"[gortex export] wrote %d nodes, %d edges (%d bytes) to %s in %dms\n",
		exportStats.NodesWritten, exportStats.EdgesWritten, exportStats.BytesWritten,
		dest, exportDuration.Milliseconds(),
	)

	// Print load-instructions for the format the user picked. The output
	// file itself is kept comment-free for portability with Memgraph's
	// .cypherl loader, so we surface the docs here instead.
	if exportOut != "" {
		printLoadInstructions(strings.ToLower(exportFormat), exportOut)
	}
	return nil
}

func printLoadInstructions(format, path string) {
	w := os.Stderr
	switch format {
	case "cypher":
		_, _ = fmt.Fprintf(w,"\n[gortex export] To load into Memgraph (recommended for ad-hoc exploration):\n")
		_, _ = fmt.Fprintf(w,"    docker run -p 7687:7687 -p 3000:3000 memgraph/memgraph-platform\n")
		_, _ = fmt.Fprintf(w,"    # then in Memgraph Lab (http://localhost:3000) → Datasets → Import\n")
		_, _ = fmt.Fprintf(w,"    # OR via mgconsole:  cat %s | mgconsole\n", path)
		_, _ = fmt.Fprintf(w,"    # First, create an id index for fast edge MATCHes:\n")
		_, _ = fmt.Fprintf(w,"    #   CREATE INDEX ON :GortexNode(id);\n")
		_, _ = fmt.Fprintf(w,"\n[gortex export] To load into Neo4j:\n")
		_, _ = fmt.Fprintf(w,"    cypher-shell -u neo4j -p <pw> -f %s\n", path)
		_, _ = fmt.Fprintf(w,"    # First, create the index:\n")
		_, _ = fmt.Fprintf(w,"    #   CREATE INDEX FOR (n:GortexNode) ON (n.id);\n")
		_, _ = fmt.Fprintf(w,"\n[gortex export] To wipe a previous export before re-loading:\n")
		_, _ = fmt.Fprintf(w,"    MATCH (n:GortexNode) DETACH DELETE n;\n")
	case "graphml":
		_, _ = fmt.Fprintf(w,"\n[gortex export] Open %s in:\n", path)
		_, _ = fmt.Fprintf(w,"    Gephi:     File → Open\n")
		_, _ = fmt.Fprintf(w,"    yEd:       File → Open\n")
		_, _ = fmt.Fprintf(w,"    Cytoscape: File → Import → Network from File\n")
	}
}

// openOutput returns a writer for path, or os.Stdout when path is empty.
// The returned closer is always non-nil (it's a no-op for stdout).
func openOutput(path string) (*os.File, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}
