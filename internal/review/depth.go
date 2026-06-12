package review

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/llm"
)

// Depth is the adaptive review depth a changeset is classified into. It gates
// how much of the phased flow runs: a tiny change is reviewed by the
// deterministic rules alone (no model call), a medium change gets a single LLM
// pass, and a large / sprawling change gets the full plan → main → relocate
// pipeline with the planner tool catalogue grounded in the prompt.
type Depth int

const (
	// DepthQuick reviews with the deterministic rulepack only — no LLM call.
	// A small, narrowly-scoped change does not need a model pass.
	DepthQuick Depth = iota
	// DepthStandard runs a single LLM review pass over the deterministic
	// substrate. The default for a mid-sized change.
	DepthStandard
	// DepthDeep runs the full plan → main → relocate pipeline and grounds the
	// planner's reference tool catalogue in the prompt. The default for a large
	// or wide change.
	DepthDeep
)

// String returns the lowercase label recorded on the report.
func (d Depth) String() string {
	switch d {
	case DepthQuick:
		return "quick"
	case DepthDeep:
		return "deep"
	default:
		return "standard"
	}
}

// Default depth thresholds applied when the corresponding ReviewConfig field is
// left at its zero value. A change at or under defaultQuickMaxLines is reviewed
// by the rules alone; a change at or above defaultDeepMinLines lines OR
// defaultDeepMinFiles files escalates to the deep pipeline.
const (
	defaultQuickMaxLines = 40
	defaultDeepMinLines  = 400
	defaultDeepMinFiles  = 20
)

// DepthThresholds are the resolved line/file bounds the classifier compares a
// changeset against. They derive from ReviewConfig, substituting the built-in
// defaults for any field left at zero.
type DepthThresholds struct {
	// QuickMaxLines is the changed-line ceiling (inclusive) at or under which
	// a change is classified DepthQuick.
	QuickMaxLines int
	// DeepMinLines is the changed-line floor (inclusive) at or above which a
	// change is classified DepthDeep.
	DeepMinLines int
	// DeepMinFiles is the changed-file floor (inclusive) at or above which a
	// change is classified DepthDeep.
	DeepMinFiles int
}

// ThresholdsFromConfig resolves the depth thresholds from a ReviewConfig,
// substituting the built-in defaults for any field left at its zero value so a
// caller that configures none gets sensible behaviour.
func ThresholdsFromConfig(cfg config.ReviewConfig) DepthThresholds {
	t := DepthThresholds{
		QuickMaxLines: cfg.QuickMaxLines,
		DeepMinLines:  cfg.DeepMinLines,
		DeepMinFiles:  cfg.DeepMinFiles,
	}
	if t.QuickMaxLines <= 0 {
		t.QuickMaxLines = defaultQuickMaxLines
	}
	if t.DeepMinLines <= 0 {
		t.DeepMinLines = defaultDeepMinLines
	}
	if t.DeepMinFiles <= 0 {
		t.DeepMinFiles = defaultDeepMinFiles
	}
	return t
}

// depthConfigured reports whether the caller opted into adaptive depth by
// setting at least one depth threshold. When none are set the review flow keeps
// its pre-adaptive behaviour (UseLLM alone gates the single LLM pass) so the
// legacy callers see the report they always did.
func depthConfigured(cfg config.ReviewConfig) bool {
	return cfg.QuickMaxLines > 0 || cfg.DeepMinLines > 0 || cfg.DeepMinFiles > 0
}

// DepthSignals are the cheap, deterministic inputs the depth classifier scores.
// They are gathered from the diff without an LLM call.
type DepthSignals struct {
	// LinesChanged is the total number of changed (added/removed) lines.
	LinesChanged int
	// FilesChanged is the number of files the change touches.
	FilesChanged int
}

// ClassifyDepth classifies a changeset into its review depth from the raw
// changed-line and changed-file counts and the review config. A change at or
// under QuickMaxLines is DepthQuick; a change at or above DeepMinLines lines OR
// DeepMinFiles files is DepthDeep; everything between is DepthStandard. Config
// fields left at zero fall back to the built-in defaults, so the legacy callers
// that pass a zero-value ReviewConfig get the default ladder.
func ClassifyDepth(linesChanged, filesChanged int, cfg config.ReviewConfig) Depth {
	return ClassifySignals(DepthSignals{LinesChanged: linesChanged, FilesChanged: filesChanged}, ThresholdsFromConfig(cfg))
}

// ClassifySignals is the structured form of ClassifyDepth: it scores already
// gathered DepthSignals against resolved thresholds. The deep test runs first so
// a wide-but-short change (many files, few lines) still escalates; the quick
// test is last so a deep-by-files change is never demoted to quick.
func ClassifySignals(sig DepthSignals, t DepthThresholds) Depth {
	switch {
	case sig.LinesChanged >= t.DeepMinLines || sig.FilesChanged >= t.DeepMinFiles:
		return DepthDeep
	case sig.LinesChanged <= t.QuickMaxLines:
		return DepthQuick
	default:
		return DepthStandard
	}
}

// DepthToComplexity maps a review depth onto the routing-complexity tier the
// LLM router selects a model from: quick and standard are simple single-pass
// work; deep is the complex, multi-phase pipeline.
func DepthToComplexity(d Depth) llm.Complexity {
	if d == DepthDeep {
		return llm.ComplexityComplex
	}
	return llm.ComplexitySimple
}

// ChangedLinesFromDiff sums the changed-line span over every hunk in the diff
// result. Each hunk contributes (EndLine - StartLine + 1) new-side lines; a
// degenerate hunk (EndLine < StartLine) counts as one line.
func ChangedLinesFromDiff(diff *analysis.DiffResult) int {
	if diff == nil {
		return 0
	}
	total := 0
	for _, h := range diff.Hunks {
		span := h.EndLine - h.StartLine + 1
		if span < 1 {
			span = 1
		}
		total += span
	}
	return total
}

// ChangedFilesFromDiff counts the distinct files a diff touches, unioning the
// explicit changed-file list with the files the hunks and changed symbols land
// in so a diff that only carries hunks (no separate file list) is still counted.
func ChangedFilesFromDiff(diff *analysis.DiffResult) int {
	if diff == nil {
		return 0
	}
	files := map[string]bool{}
	for _, f := range diff.ChangedFiles {
		if f = cleanPath(f); f != "" {
			files[f] = true
		}
	}
	for _, h := range diff.Hunks {
		if f := cleanPath(h.FilePath); f != "" {
			files[f] = true
		}
	}
	for _, cs := range diff.ChangedSymbols {
		if f := cleanPath(cs.FilePath); f != "" {
			files[f] = true
		}
	}
	return len(files)
}

// PlannerTool is one entry in the reference-only planner catalogue: the name of
// a graph tool the planner may REFERENCE when reasoning about what to inspect,
// plus a one-line description of what it surfaces. These are not callable tools
// in the review flow — they ground the model in the analyses that already exist.
type PlannerTool struct {
	Name        string
	Description string
}

// plannerCatalogue is the fixed set of graph tools the deep-path planner can
// reference. They are presented to the model as context — "these analyses
// exist" — never as a callable tool surface, so the flow stays a single
// freeform completion with no tool-call loop.
var plannerCatalogue = []PlannerTool{
	{"detect_changes", "the changed-symbol set + blast scope of this diff"},
	{"diff_context", "the graph-enriched view of each changed symbol and its neighbours"},
	{"explain_change_impact", "the downstream callers and dependents a change ripples to"},
	{"verify_change", "the callers / interface implementors a signature change would break"},
	{"contracts", "the API / behavioural contracts the changed symbols participate in"},
	{"check_guards", "the team-convention guards the change must not violate"},
}

// PlannerCatalogue returns the reference-only graph-tool catalogue the deep
// planner is grounded in. The entries are descriptive references, NOT a callable
// tool surface — the review flow never dispatches them.
func PlannerCatalogue() []PlannerTool {
	out := make([]PlannerTool, len(plannerCatalogue))
	copy(out, plannerCatalogue)
	return out
}

// PlannerCatalogueSpecs projects the reference catalogue onto the provider
// ToolSpec shape for callers that want to surface the same names through the
// llm tooling vocabulary. They remain reference-only — the review flow passes
// them as prompt context, not as a constrained tool enum.
func PlannerCatalogueSpecs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(plannerCatalogue))
	for _, t := range plannerCatalogue {
		out = append(out, llm.ToolSpec{Name: t.Name, Description: t.Description})
	}
	return out
}

// renderPlannerCatalogue renders the reference catalogue as a prose block for
// the prompt. It is explicitly labelled reference-only so the model treats the
// entries as available analyses to reason about, not tools it can call.
func renderPlannerCatalogue() string {
	if len(plannerCatalogue) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Graph analyses available (reference only — not callable here)\n")
	b.WriteString("These graph tools have already characterised this change; reason about ")
	b.WriteString("their results, do not request to call them:\n")
	for _, t := range plannerCatalogue {
		fmt.Fprintf(&b, "- %s — %s (reference only)\n", t.Name, t.Description)
	}
	return b.String()
}
