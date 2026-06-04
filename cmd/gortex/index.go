package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/progress"

	"go.uber.org/zap"
)

var (
	indexLanguages []string
	indexExclude   []string
	indexWorkers   int
	indexOutput    string
	indexWatch     bool
	indexProfile   bool
	indexSnapshot  string
	indexWorkspace string
)

var indexCmd = &cobra.Command{
	Use:   "index [path...]",
	Short: "Index one or more repositories and print stats",
	Args:  cobra.MinimumNArgs(0),
	RunE:  runIndex,
}

func init() {
	indexCmd.Flags().StringSliceVar(&indexLanguages, "languages", nil, "languages to parse (default: auto-detect)")
	indexCmd.Flags().StringSliceVar(&indexExclude, "exclude", nil, "additional glob patterns to exclude")
	indexCmd.Flags().IntVar(&indexWorkers, "workers", 0, "parallel parsing workers (default: NumCPU)")
	indexCmd.Flags().StringVar(&indexOutput, "output", "text", "output format: text|json")
	indexCmd.Flags().BoolVar(&indexWatch, "watch", false, "stay running and reindex on file changes")
	indexCmd.Flags().BoolVar(&indexProfile, "profile", false, "print per-stage timings + peak RSS for battle-testing")
	indexCmd.Flags().StringVar(&indexSnapshot, "snapshot", "", "write a snapshot.gob.gz file at the given path (used by gortex-cloud's indexer worker)")
	indexCmd.Flags().StringVar(&indexWorkspace, "workspace", "", "stamp emitted nodes with this WorkspaceID (defaults to repo name)")
	rootCmd.AddCommand(indexCmd)
}

func runIndex(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// Default to current directory if no paths given.
	paths := args
	if len(paths) == 0 {
		paths = []string{"."}
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	if indexWorkers > 0 {
		cfg.Index.Workers = indexWorkers
	}
	if len(indexExclude) > 0 {
		cfg.Index.Exclude = append(cfg.Index.Exclude, indexExclude...)
	}

	// Index each path as a separate repository.
	for _, path := range paths {
		// Construct the spinner first so we know whether to silence the
		// indexer's zap logger. When the cozy view is live, structured
		// info logs would interleave with the mesh frame.
		sp := progress.NewSpinner(cmd.ErrOrStderr())
		if noProgress {
			sp.Disable()
		}
		idxLogger := logger
		if sp.Enabled() {
			idxLogger = zap.NewNop()
		}

		g := graph.New()
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		languages.RegisterCustomGrammars(reg, cfg.Index.Grammars, idxLogger)
		languages.RegisterExtractorPlugins(reg, cfg.Index.ExtractorPlugins, idxLogger)
		idx := indexer.New(g, reg, cfg.Index, idxLogger)

		// --profile attaches a timing reporter via the progress API.
		// The indexer emits stage markers for walking files, parsing,
		// resolving, semantic enrichment, search build, and contract
		// extraction — we turn those into a breakdown at the end.
		ctx := context.Background()
		var timer *progress.TimingReporter
		var memBefore runtime.MemStats
		if indexProfile {
			timer = progress.NewTimingReporter()
			runtime.GC()
			runtime.ReadMemStats(&memBefore)
		}

		sp.Start("Indexing repository")
		sp.Set("", path)
		var reporter progress.Reporter = sp
		if timer != nil {
			reporter = progress.Multi(sp, timer)
		}
		ctx = progress.WithReporter(ctx, reporter)

		result, err := idx.IndexCtx(ctx, path)
		if err != nil {
			sp.Fail(err)
			return fmt.Errorf("indexing %s: %w", path, err)
		}
		sp.Set("", fmt.Sprintf("%s files · %s nodes · %s edges · %dms",
			humanizeInt(result.FileCount), humanizeInt(result.NodeCount), humanizeInt(result.EdgeCount), result.DurationMs))
		sp.Done()

		switch indexOutput {
		case "json":
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if err := enc.Encode(result); err != nil {
				return err
			}
		default:
			// On a TTY the spinner's ✓ line already conveyed the same
			// stats — suppress the stdout duplicate. Pipes still get the
			// canonical machine-readable line.
			if !progress.IsTTY(cmd.OutOrStdout()) {
				writeIndexTextSummary(cmd.OutOrStdout(), path, result)
			}
		}

		if indexProfile {
			writeProfileReport(cmd.OutOrStdout(), path, timer, result, memBefore)
		}

		// --snapshot writes a gob+gzip snapshot suitable for `gortex
		// server --snapshot <path>`. The cloud indexer worker (in
		// github.com/gortexhq/gortex-cloud) shells this out to produce
		// per-workspace snapshots that the per-workspace gortex server
		// later loads at boot.
		if indexSnapshot != "" {
			if err := saveSnapshotTo(g, nil, nil, snapshotVector{}, "gortex-index", indexSnapshot, logger); err != nil {
				return fmt.Errorf("write snapshot %s: %w", indexSnapshot, err)
			}
		}
	}

	if indexWatch {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "[gortex] watch mode not yet implemented")
	}

	return nil
}

// writeProfileReport emits the per-stage breakdown, throughput summary,
// and memory delta that a profile run cares about. Called once per
// indexed path so multi-path runs get side-by-side breakdowns.
func writeProfileReport(w interface {
	Write([]byte) (int, error)
}, path string, timer *progress.TimingReporter, result *indexer.IndexResult, memBefore runtime.MemStats) {
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	_, _ = fmt.Fprintf(w, "\nprofile: %s\n", path)
	timer.WriteReport(w, time.Time{})

	elapsed := time.Duration(result.DurationMs) * time.Millisecond
	filesPerSec := 0.0
	if elapsed > 0 {
		filesPerSec = float64(result.FileCount) / elapsed.Seconds()
	}
	_, _ = fmt.Fprintf(w, "\nthroughput: %d files in %s  (%.0f files/s)\n",
		result.FileCount, elapsed.Round(time.Millisecond), filesPerSec)
	_, _ = fmt.Fprintf(w, "nodes:      %d\n", result.NodeCount)
	_, _ = fmt.Fprintf(w, "edges:      %d\n", result.EdgeCount)

	// Heap delta captures what indexing retained. Peak RSS requires OS
	// hooks that aren't portable — callers who want RSS should wrap the
	// binary in `/usr/bin/time -l` (macOS) or `/usr/bin/time -v` (Linux).
	_, _ = fmt.Fprintf(w, "heap:       +%s (%s → %s)\n",
		humanBytes(memAfter.HeapAlloc-memBefore.HeapAlloc),
		humanBytes(memBefore.HeapAlloc),
		humanBytes(memAfter.HeapAlloc))
	_, _ = fmt.Fprintf(w, "gc:         %d cycles during index\n",
		memAfter.NumGC-memBefore.NumGC)
}

// humanBytes renders a byte count with an SI-ish unit so the profile
// output is readable. Not meant for strict accuracy — binary MiB/GiB
// is fine here since the reader just wants a ballpark.
func humanBytes(n uint64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
