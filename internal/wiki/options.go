// Package wiki renders a multi-page markdown wiki of the indexed graph.
//
// The wiki is template-driven (no LLM required) and reuses the same
// pattern as internal/skills/generator.go: filter → sort → render
// per community with strings.Builder + embedded const headers.
//
// Layout (multi-repo ready, single-repo today):
//
//	wiki/
//	  index.md                  # top-level repo index
//	  <repo-slug>/              # one directory per repo
//	    index.md                # community navigation
//	    architecture.md         # system overview
//	    semantic.md             # semantic coverage report
//	    changelog.md            # gortex docs bundle (optional)
//	    communities/            # one page per community
//	      <n>-<slug>.md
//	    processes/              # one page per process
//	      <slug>.md
//	    contracts/
//	      api-surface.md
//	    analysis/
//	      hotspots.md
//	      cycles.md
//	      semantic.md
//	    _assets/
//	      community-graph.mermaid
//	  _workspace/               # reserved for future cross-repo pages
package wiki

import (
	"context"
)

// Enhancer turns a section's raw markdown into a richer narrative.
// The NoopEnhancer returns the input unchanged — used when --enhance
// is off or the configured backend is unavailable.
type Enhancer interface {
	Enhance(ctx context.Context, section EnhanceSection) (string, error)
}

// EnhanceSection carries the inputs an Enhancer needs to write prose
// without ever loading raw source code.
type EnhanceSection struct {
	// Kind is one of "community", "process", "architecture".
	Kind string
	// PageTitle is the human-readable section title.
	PageTitle string
	// AnchorSymbolIDs are the graph node IDs the section centres on.
	AnchorSymbolIDs []string
	// RawMarkdown is the template-rendered body the enhancer is
	// asked to improve. The enhancer may return it verbatim if it
	// has nothing to add.
	RawMarkdown string
	// Context is a free-form string holding signatures, fan-in/out,
	// and other structured hints the enhancer can paste into a prompt.
	Context string
}

// NoopEnhancer satisfies Enhancer by always returning the input
// markdown unchanged. It is the default when --enhance is off.
type NoopEnhancer struct{}

// Enhance implements Enhancer.
func (NoopEnhancer) Enhance(_ context.Context, s EnhanceSection) (string, error) {
	return s.RawMarkdown, nil
}

// Options control wiki generation. Zero values fall back to sensible
// defaults applied by Generator.
type Options struct {
	// OutputDir is the root directory; the writer materialises
	// wiki/<repo-slug>/... underneath. Defaults to "wiki".
	OutputDir string
	// Format is "markdown" (default) or "html".
	Format string
	// Wikilinks enables [[slug]] style links instead of relative
	// markdown links — useful for Obsidian users.
	Wikilinks bool
	// Repo is the slug used for the per-repo directory under
	// OutputDir. Defaults to the cleaned basename of the indexed
	// repo root path.
	Repo string
	// Project is the project name carried into the page header
	// (multi-repo mode hint). Empty in single-repo mode.
	Project string
	// WorkspaceID restricts emitted nodes to a single workspace.
	WorkspaceID string
	// MinCommunity filters out communities below this size.
	// Defaults to 3.
	MinCommunity int
	// MaxCommunities caps how many communities to document.
	// Defaults to 20.
	MaxCommunities int
	// NoProcesses skips process page generation.
	NoProcesses bool
	// NoContracts skips contract page generation.
	NoContracts bool
	// NoDocs skips the changelog/ownership/stale/blame bundle.
	NoDocs bool
	// Enhance switches on LLM enhancement; the Enhancer below must
	// also be non-nil for it to take effect.
	Enhance bool
	// Enhancer is the LLM backend. Required when Enhance is true.
	// NoopEnhancer is assumed when nil.
	Enhancer Enhancer
	// Force suppresses any "already exists" diagnostics — the
	// writer is idempotent regardless.
	Force bool
}

// withDefaults returns a copy of o with zero fields populated.
func (o Options) withDefaults() Options {
	if o.OutputDir == "" {
		o.OutputDir = "wiki"
	}
	if o.Format == "" {
		o.Format = "markdown"
	}
	if o.MinCommunity == 0 {
		o.MinCommunity = 3
	}
	if o.MaxCommunities == 0 {
		o.MaxCommunities = 20
	}
	if o.Enhancer == nil {
		o.Enhancer = NoopEnhancer{}
	}
	return o
}
