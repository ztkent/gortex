package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/progress"
)

// emitAgentSummary writes the post-install / post-init summary as a styled
// block: totals strip, configured editors with per-action breakdown, a chip
// line of editors that weren't detected, and a numbered list of next steps.
// Used by both `gortex install` and `gortex init` for visual consistency.
func emitAgentSummary(w io.Writer, results []*agents.Result, opts agents.ApplyOpts, nextSteps []string) {
	var detected []*agents.Result
	var notDetected []string
	var totals actionCounts
	for _, r := range results {
		if r == nil {
			continue
		}
		if !r.Detected {
			notDetected = append(notDetected, r.Name)
			continue
		}
		detected = append(detected, r)
		totals.add(r.Files)
	}

	_, _ = fmt.Fprintln(w)
	if !totals.zero() || opts.DryRun {
		_, _ = fmt.Fprintln(w, "  "+totalsStrip(totals, opts.DryRun))
		_, _ = fmt.Fprintln(w)
	}

	if len(detected) > 0 {
		_, _ = fmt.Fprintln(w, "  "+progress.Heading("configured", strconv.Itoa(len(detected))))
		for _, r := range detected {
			detail := perAdapterDetail(r.Files)
			_, _ = fmt.Fprintln(w, "   "+progress.Row(r.Name, detail, 14))
		}
		_, _ = fmt.Fprintln(w)
	}

	if len(notDetected) > 0 {
		names := progress.SortStrings(notDetected)
		_, _ = fmt.Fprintln(w, "  "+progress.Heading("not detected", strconv.Itoa(len(notDetected))))
		_, _ = fmt.Fprintln(w, "   "+progress.Chips(names, 0))
		_, _ = fmt.Fprintln(w)
	}

	if len(nextSteps) > 0 {
		_, _ = fmt.Fprintln(w, "  "+progress.Heading("next steps"))
		for i, s := range nextSteps {
			_, _ = fmt.Fprintln(w, "   "+progress.Step(i+1, s))
		}
		_, _ = fmt.Fprintln(w)
	}
}

// actionCounts tallies file actions across all adapters.
type actionCounts struct {
	create, merge, skip, wouldCreate, wouldMerge int
}

func (c *actionCounts) add(files []agents.FileAction) {
	for _, f := range files {
		switch f.Action {
		case agents.ActionCreate:
			c.create++
		case agents.ActionMerge:
			c.merge++
		case agents.ActionSkip:
			c.skip++
		case agents.ActionWouldCreate:
			c.wouldCreate++
		case agents.ActionWouldMerge:
			c.wouldMerge++
		}
	}
}

func (c actionCounts) zero() bool {
	return c.create+c.merge+c.skip+c.wouldCreate+c.wouldMerge == 0
}

// totalsStrip renders e.g. "21 would-create  ·  2 would-merge  ·  12 skip   (dry-run)".
func totalsStrip(c actionCounts, dryRun bool) string {
	var stats []string
	if c.wouldCreate > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(c.wouldCreate), "would-create", progress.StatGood))
	}
	if c.create > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(c.create), "created", progress.StatGood))
	}
	if c.wouldMerge > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(c.wouldMerge), "would-merge", progress.StatNeutral))
	}
	if c.merge > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(c.merge), "merged", progress.StatNeutral))
	}
	if c.skip > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(c.skip), "skipped", progress.StatNeutral))
	}
	out := progress.StatStrip(stats...)
	if dryRun {
		out += "      " + progress.Caption("dry-run — no files written")
	}
	return out
}

// perAdapterDetail compresses a single adapter's file list into a readable
// "create 3 · merge 1 · skip 2" string, ordered by significance.
func perAdapterDetail(files []agents.FileAction) string {
	var c actionCounts
	c.add(files)
	parts := []string{}
	if c.wouldCreate > 0 {
		parts = append(parts, fmt.Sprintf("would-create %d", c.wouldCreate))
	}
	if c.create > 0 {
		parts = append(parts, fmt.Sprintf("created %d", c.create))
	}
	if c.wouldMerge > 0 {
		parts = append(parts, fmt.Sprintf("would-merge %d", c.wouldMerge))
	}
	if c.merge > 0 {
		parts = append(parts, fmt.Sprintf("merged %d", c.merge))
	}
	if c.skip > 0 {
		parts = append(parts, fmt.Sprintf("skipped %d", c.skip))
	}
	return strings.Join(parts, "  ·  ")
}
