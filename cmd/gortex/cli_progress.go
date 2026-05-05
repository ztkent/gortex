package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/progress"
)

// newCLISpinner constructs a Spinner bound to cmd's stderr and starts it. The
// caller is responsible for Done()/Fail(); when the global --no-progress flag
// is set the spinner falls back to plain text.
func newCLISpinner(cmd *cobra.Command, label string) *progress.Spinner {
	sp := progress.NewSpinner(cmd.ErrOrStderr())
	if noProgress {
		sp.Disable()
	}
	sp.Start(label)
	return sp
}

// loggerForSpinner returns a Nop logger when the cozy mesh spinner is going
// to render (TTY stderr + spinner not disabled), otherwise the real logger.
// Used by callers that hand a logger to the indexer — silences structured
// info logs that would otherwise interleave with the animation.
func loggerForSpinner(cmd *cobra.Command, real *zap.Logger) *zap.Logger {
	if noProgress {
		return real
	}
	if !progress.IsTTY(cmd.ErrOrStderr()) {
		return real
	}
	return zap.NewNop()
}

// indexWithSpinner runs the indexer with a progress spinner attached, reporting
// stage transitions as the sub-status. Used by every enrich subcommand that
// needs an in-memory graph before running its enrichment pass.
func indexWithSpinner(cmd *cobra.Command, idx *indexer.Indexer, path string) error {
	sp := newCLISpinner(cmd, "Indexing repository")
	sp.Set("", path)
	ctx := progress.WithReporter(context.Background(), sp)
	result, err := idx.IndexCtx(ctx, path)
	if err != nil {
		sp.Fail(err)
		return fmt.Errorf("index %s: %w", path, err)
	}
	sp.Set("", fmt.Sprintf("%s files · %s nodes · %s edges · %dms",
		humanizeInt(result.FileCount), humanizeInt(result.NodeCount), humanizeInt(result.EdgeCount), result.DurationMs))
	sp.Done()
	return nil
}
