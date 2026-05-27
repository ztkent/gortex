package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/churn"
)

// registerEnrichChurnTool exposes the churn enricher as an MCP tool so
// agents (and the post-commit / post-merge git hook driving `gortex
// enrich churn`) can refresh per-symbol churn data without going
// through the daemon control socket. The handler runs the enricher
// in-process against s.graph, so it inherits whatever backend the
// daemon was launched with — LadyBug for persistence, in-memory for
// CI / one-off invocations.
//
// The accompanying `get_churn_rate` tool reads from the same
// meta.churn fields this tool writes; pre-computation here is what
// makes the read path a sub-second graph scan.
func (s *Server) registerEnrichChurnTool() {
	s.addTool(
		mcp.NewTool("enrich_churn",
			mcp.WithDescription("Pre-compute per-file and per-symbol git churn data and stamp it on graph nodes so `get_churn_rate` can answer without a git subprocess. Walks `git log <branch>` and `git blame <branch>` once per file, then projects line-range commit counts onto every function/method node. The branch is the repository's default branch (origin/main, then origin/master, then local main/master/trunk) unless `branch` overrides. Idempotent: re-running updates the same Meta fields in place. Daemons backed by LadyBug persist the result across restarts; in-memory daemons recompute on next call."),
			mcp.WithString("branch", mcp.Description("Branch / tag / SHA to compute churn against. Empty means resolve the repository's default branch.")),
			mcp.WithString("path", mcp.Description("Optional path or repo prefix to scope the enrichment. Multi-repo daemons enrich every tracked repo when empty.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleEnrichChurn,
	)
}

func (s *Server) handleEnrichChurn(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("graph not initialized"), nil
	}
	branch := strings.TrimSpace(req.GetString("branch", ""))
	pathArg := strings.TrimSpace(req.GetString("path", ""))

	// Resolve targets: one repo root per tracked repo, optionally
	// filtered by path (matched as either prefix or absolute root).
	type target struct {
		prefix string
		root   string
	}
	var targets []target
	if s.multiIndexer != nil {
		for prefix, meta := range s.multiIndexer.AllMetadata() {
			if pathArg != "" && pathArg != prefix && pathArg != meta.RootPath {
				continue
			}
			targets = append(targets, target{prefix: prefix, root: meta.RootPath})
		}
	}
	if len(targets) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no tracked repo matches %q", pathArg)), nil
	}

	started := time.Now()
	type perRepo struct {
		Prefix  string `json:"prefix"`
		Branch  string `json:"branch"`
		HeadSHA string `json:"head_sha"`
		Files   int    `json:"files"`
		Symbols int    `json:"symbols"`
		Skipped string `json:"skipped,omitempty"`
	}
	var per []perRepo
	totalFiles, totalSymbols := 0, 0
	for _, t := range targets {
		b := branch
		if b == "" {
			b = churn.DefaultBranch(t.root)
		}
		if b == "" {
			per = append(per, perRepo{Prefix: t.prefix, Skipped: "no default branch resolvable"})
			continue
		}
		res, err := churn.EnrichGraph(ctx, s.graph, t.root, churn.Options{Branch: b})
		if err != nil {
			per = append(per, perRepo{Prefix: t.prefix, Branch: b, Skipped: err.Error()})
			continue
		}
		per = append(per, perRepo{
			Prefix: t.prefix, Branch: res.Branch, HeadSHA: res.HeadSHA,
			Files: res.Files, Symbols: res.Symbols,
		})
		totalFiles += res.Files
		totalSymbols += res.Symbols
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"repos":       per,
		"files":       totalFiles,
		"symbols":     totalSymbols,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}
