package skills

import (
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// BuildOpts tune community detection + skill generation in Build.
// Zero values fall back to the same defaults New() applies.
type BuildOpts struct {
	MinSize   int
	MaxSkills int
}

// Build runs community detection and process discovery on g, then
// renders per-community SKILL.md files plus the routing block that
// `gortex init` injects into per-repo instructions files.
//
// Returns (nil, "") when no community meets the MinSize threshold —
// callers treat both outputs as opaque payloads and pass them through
// to adapters via agents.Env.
func Build(g graph.Store, opts BuildOpts) ([]GeneratedSkill, string) {
	if g == nil {
		return nil, ""
	}
	communities := analysis.DetectCommunities(g)
	if communities == nil || len(communities.Communities) == 0 {
		return nil, ""
	}
	processes := analysis.DiscoverProcesses(g)

	gen := New(communities, processes, g)
	if opts.MinSize > 0 {
		gen.SetMinSize(opts.MinSize)
	}
	if opts.MaxSkills > 0 {
		gen.SetMaxSkills(opts.MaxSkills)
	}
	generated := gen.GenerateAll()
	if len(generated) == 0 {
		return nil, ""
	}
	routing := gen.GenerateRouting(generated)
	return generated, routing
}
