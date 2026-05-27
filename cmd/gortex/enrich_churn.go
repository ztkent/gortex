package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/churn"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var (
	enrichChurnBranch   string
	enrichChurnSnapshot string
)

var enrichChurnCmd = &cobra.Command{
	Use:   "churn [path]",
	Short: "Pre-compute per-symbol git churn from a fixed branch (default: origin/main)",
	Long: `Walks the indexed repo and stamps meta.churn on every file and
function/method with the commit_count / age_days / churn_rate /
last_author / last_commit_at metrics the get_churn_rate MCP tool reads.

The signal is computed against a single branch — typically the
repository's default branch — so feature-branch work-in-progress
doesn't pollute the persisted data. Pass --branch to override.

When a daemon is running on the default socket, this command sends a
control RPC and the daemon does the enrichment against its in-process
graph (avoiding the LadyBug write-lock collision a direct write would
cause). Without a daemon, the command falls back to a one-shot in-
memory pass that can be persisted with --snapshot.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnrichChurn,
}

func init() {
	enrichChurnCmd.Flags().StringVar(&enrichChurnBranch, "branch", "",
		"branch / tag / SHA to compute churn against (default: origin/main, falls back to local main/master)")
	enrichChurnCmd.Flags().StringVar(&enrichChurnSnapshot, "snapshot", "",
		"when no daemon is running, write the enriched in-memory graph as a gob.gz snapshot to this path")
	enrichCmd.AddCommand(enrichChurnCmd)
}

func runEnrichChurn(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	path := "."
	if len(args) >= 1 {
		path = args[0]
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("abs path %q: %w", path, err)
	}

	// Daemon path: forward to the running daemon so the enrichment
	// runs against its in-process (and possibly LadyBug-backed)
	// graph. The daemon already owns the write lock; routing
	// through it sidesteps the "can't open the same LadyBug
	// directory twice" failure mode.
	if daemon.IsRunning() {
		return forwardEnrichChurnToDaemon(cmd, abs)
	}

	// Standalone path: index in-memory, enrich, optionally snapshot.
	// Useful in CI where no daemon is around and the caller wants a
	// snapshot artefact.
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

	branch := enrichChurnBranch
	if branch == "" {
		branch = gitDefaultBranch(idx.RootPath())
	}
	if branch == "" {
		return fmt.Errorf("could not resolve default branch in %s; pass --branch <ref>", idx.RootPath())
	}

	sp := newCLISpinner(cmd, "Stamping churn")
	sp.Set("", branch)
	started := time.Now()
	res, err := churn.EnrichGraph(context.Background(), g, idx.RootPath(), churn.Options{Branch: branch})
	if err != nil {
		sp.Fail(err)
		return fmt.Errorf("churn: %w", err)
	}
	sp.Set("", fmt.Sprintf("%d files · %d symbols", res.Files, res.Symbols))
	sp.Done()

	result := map[string]any{
		"files":       res.Files,
		"symbols":     res.Symbols,
		"branch":      res.Branch,
		"head_sha":    res.HeadSHA,
		"duration_ms": time.Since(started).Milliseconds(),
		"root":        idx.RootPath(),
		"mode":        "standalone",
	}
	if enrichChurnSnapshot != "" {
		if err := saveSnapshotTo(g, nil, nil, snapshotVector{}, "gortex-enrich-churn", enrichChurnSnapshot, logger); err != nil {
			return fmt.Errorf("write snapshot %s: %w", enrichChurnSnapshot, err)
		}
		result["snapshot"] = enrichChurnSnapshot
	}
	return printEnrichResult(result)
}

// forwardEnrichChurnToDaemon sends a ControlEnrichChurn RPC to the
// running daemon and renders the response. Returns a clear error if
// the daemon rejects the request — including the case where the
// caller's path doesn't match any tracked repo.
func forwardEnrichChurnToDaemon(cmd *cobra.Command, absPath string) error {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli-enrich-churn"})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			return fmt.Errorf("daemon socket detected but dial failed; restart the daemon or run with no daemon (it falls back to in-memory)")
		}
		return fmt.Errorf("dial daemon: %w", err)
	}
	defer func() { _ = c.Close() }()

	resp, err := c.Control(daemon.ControlEnrichChurn, daemon.EnrichChurnParams{
		Path:   absPath,
		Branch: enrichChurnBranch,
	})
	if err != nil {
		return fmt.Errorf("control enrich_churn: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon rejected enrich_churn [%s]: %s", resp.ErrorCode, resp.ErrorMsg)
	}

	var out daemon.EnrichChurnResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			return fmt.Errorf("parse daemon response: %w", err)
		}
	}
	sp := newCLISpinner(cmd, "Enriched via daemon")
	sp.Set("", fmt.Sprintf("%d files · %d symbols · %s", out.Files, out.Symbols, out.Branch))
	sp.Done()
	payload := map[string]any{
		"files":       out.Files,
		"symbols":     out.Symbols,
		"branch":      out.Branch,
		"head_sha":    out.HeadSHA,
		"duration_ms": out.DurationMS,
		"mode":        "daemon",
	}
	if absPath != "" {
		payload["path"] = absPath
	}
	if _, err := os.Getwd(); err == nil {
		// `printEnrichResult` reads payload["root"] for the TTY caption.
		// We don't have a concrete root here (the daemon spans every
		// tracked repo); leave it unset so the caption is silent.
	}
	return printEnrichResult(payload)
}
