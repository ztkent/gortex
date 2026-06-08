package graph

import (
	"sort"
	"strings"
)

// EpistemicBoundary names one unresolved/dynamic-dispatch site that makes a
// traversal count a *floor* rather than an exact number. It is the honest
// answer to "could the real blast radius / reachable set be larger?" — yes,
// because the resolver could not bind this site, so an unknown number of
// callers/callees hide behind it.
//
// It is an attribute of a *traversal*, not a graph element: no new NodeKind or
// EdgeKind is introduced. Boundaries are recorded at exactly the sites a walk
// would otherwise silently drop (an out-edge to an `unresolved::*` target) or
// where dynamic dispatch is structurally possible (the seed implements an
// interface, so callers may invoke it through that interface).
type EpistemicBoundary struct {
	SeedID    string         `json:"seed_id"`
	SeedName  string         `json:"seed_name,omitempty"`
	Target    string         `json:"target,omitempty"`
	EdgeKind  string         `json:"edge_kind,omitempty"`
	Reason    BoundaryReason `json:"reason"`
	Direction string         `json:"direction"` // "callers" | "callees"
}

// BoundaryReason classifies why a boundary makes the count a floor. The
// vocabulary aligns with the resolution-outcomes taxonomy's dynamic-dispatch
// concept while staying graph-local (no name-resolution needed to compute it).
type BoundaryReason string

const (
	// BoundaryDynamicDispatch: an out call/reference edge whose target the
	// resolver left as `unresolved::*` — the callee set could be larger.
	BoundaryDynamicDispatch BoundaryReason = "dynamic_dispatch"
	// BoundaryInterfaceDispatch: the node implements/overrides an interface
	// method, so callers may invoke it through the interface via dispatch that
	// is not attributed to this node — the caller set could be larger.
	BoundaryInterfaceDispatch BoundaryReason = "interface_dispatch"
	// BoundaryExternal: an edge into the `external::` namespace — the chain
	// leaves the indexed code. Listed for transparency; not floor-making.
	BoundaryExternal BoundaryReason = "external_boundary"
	// BoundaryStub: an edge into a stdlib/builtin/module stub. Listed; not
	// floor-making (an external stdlib call adds no in-repo callers/callees).
	BoundaryStub BoundaryReason = "stub"
)

// maxBoundaries caps the per-result boundary list so a pathological hub cannot
// bloat a response. Mirrors impact.go's maxPerTier.
const maxBoundaries = 50

// ClassifyDroppedTarget classifies an edge target a traversal could not follow.
// ok=false means it is an ordinary in-graph node (follow it normally).
func ClassifyDroppedTarget(targetID string, kind EdgeKind) (BoundaryReason, bool) {
	if IsUnresolvedTarget(targetID) {
		return BoundaryDynamicDispatch, true
	}
	if strings.HasPrefix(targetID, "external::") {
		return BoundaryExternal, true
	}
	if IsStub(targetID) {
		return BoundaryStub, true
	}
	return "", false
}

// CalleeBoundaries scans the out-edges of the given nodes for call/reference
// targets a forward (callee-direction) walk could not follow. Each such target
// means the reachable callee set could be larger than what was returned.
func CalleeBoundaries(g Store, nodeIDs []string, limit int) []EpistemicBoundary {
	if g == nil {
		return nil
	}
	if limit <= 0 {
		limit = maxBoundaries
	}
	seen := map[string]bool{}
	var out []EpistemicBoundary
	for _, id := range nodeIDs {
		for _, e := range g.GetOutEdges(id) {
			if e.Kind != EdgeCalls && e.Kind != EdgeReferences {
				continue
			}
			reason, ok := ClassifyDroppedTarget(e.To, e.Kind)
			if !ok {
				continue
			}
			key := id + "\x00" + e.To
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, EpistemicBoundary{
				SeedID:    id,
				SeedName:  nameForID(g, id),
				Target:    boundaryTargetName(e.To),
				EdgeKind:  string(e.Kind),
				Reason:    reason,
				Direction: "callees",
			})
			if len(out) >= limit {
				return sortBoundaries(out)
			}
		}
	}
	return sortBoundaries(out)
}

// CallerBoundaries flags nodes whose *caller* count is a floor because dynamic
// dispatch into them is structurally possible: each node that implements or
// overrides an interface method may be reached through that interface by
// callers not attributed to it directly. It names the interface so an agent
// can run find_implementations / get_callers on it to widen the picture.
func CallerBoundaries(g Store, nodeIDs []string, limit int) []EpistemicBoundary {
	if g == nil {
		return nil
	}
	if limit <= 0 {
		limit = maxBoundaries
	}
	seen := map[string]bool{}
	var out []EpistemicBoundary
	for _, id := range nodeIDs {
		for _, e := range g.GetOutEdges(id) {
			if e.Kind != EdgeImplements && e.Kind != EdgeOverrides {
				continue
			}
			key := id + "\x00" + e.To
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, EpistemicBoundary{
				SeedID:    id,
				SeedName:  nameForID(g, id),
				Target:    boundaryTargetName(e.To),
				EdgeKind:  string(e.Kind),
				Reason:    BoundaryInterfaceDispatch,
				Direction: "callers",
			})
			if len(out) >= limit {
				return sortBoundaries(out)
			}
		}
	}
	return sortBoundaries(out)
}

// LowerBoundCaveat reports whether the boundary set makes the count a genuine
// floor. Only dynamic-dispatch / interface-dispatch boundaries qualify: an
// external/stdlib stub edge is listed for transparency but adds no hidden
// in-repo callers/callees, so by itself it must not raise the flag (otherwise
// nearly every symbol with a stdlib call would be flagged — see the design's
// over-flagging guard).
func LowerBoundCaveat(boundaries []EpistemicBoundary) bool {
	for _, b := range boundaries {
		if b.Reason == BoundaryDynamicDispatch || b.Reason == BoundaryInterfaceDispatch {
			return true
		}
	}
	return false
}

func boundaryTargetName(id string) string {
	if IsUnresolvedTarget(id) {
		return UnresolvedName(id)
	}
	return id
}

func nameForID(g Store, id string) string {
	if n := g.GetNode(id); n != nil {
		return n.Name
	}
	return ""
}

func sortBoundaries(bs []EpistemicBoundary) []EpistemicBoundary {
	sort.SliceStable(bs, func(i, j int) bool {
		if bs[i].SeedID != bs[j].SeedID {
			return bs[i].SeedID < bs[j].SeedID
		}
		return bs[i].Target < bs[j].Target
	})
	return bs
}
