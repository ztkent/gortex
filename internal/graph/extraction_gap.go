package graph

// ZeroEdgeClass classifies why a symbol's graph query came back empty.
// An empty result has two very different causes that an agent cannot
// otherwise tell apart, and a pre-edit safety check that trusts a
// false "0 usages" is silently disarmed.
type ZeroEdgeClass string

const (
	// ZeroEdgeNone means the symbol has incoming call/reference edges:
	// the query was not empty and no caveat is warranted.
	ZeroEdgeNone ZeroEdgeClass = "none"

	// ZeroEdgeLikelyUnused means the symbol has no incoming
	// call/reference edges but DOES carry other graph edges — the
	// structural edge from its file (`defines`), a method's
	// `member_of`, or outgoing calls / references / type references.
	// This is consistent with genuine dead code: the extractor saw
	// the symbol, nothing uses it.
	ZeroEdgeLikelyUnused ZeroEdgeClass = "likely_unused"

	// ZeroEdgePossibleExtractionGap means the symbol has zero edges of
	// any kind. A normally indexed function or method always carries
	// at least one structural edge — the file `defines` it, a method
	// is `member_of` its type — so zero total edges most likely means
	// the extractor never processed the symbol or its file. The
	// symbol may well be live; the graph just does not know. This is
	// the dangerous case for a delete-or-rewrite decision.
	ZeroEdgePossibleExtractionGap ZeroEdgeClass = "possible_extraction_gap"
)

// ZeroEdgeCaveat is the structured caveat attached to an empty graph
// query result. Class is machine-checkable so a safety gate can branch
// on it; Message is a short human-readable explanation.
type ZeroEdgeCaveat struct {
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// ZeroImpactCaveat is the per-symbol caveat attached to an empty impact
// analysis result, which is computed over a list of symbols. It carries
// the symbol ID alongside the same machine-checkable classification.
type ZeroImpactCaveat struct {
	ID      string        `json:"id" toon:"id"`
	Class   ZeroEdgeClass `json:"class" toon:"class"`
	Message string        `json:"message" toon:"message"`
}

// usageEdgeKinds are the incoming edge kinds that count as a symbol
// being "used" — calls, references, and the type/instantiation edges
// that find_usages itself treats as usages. An incoming edge of any of
// these kinds means the symbol is not dead code.
var usageEdgeKinds = map[EdgeKind]bool{
	EdgeCalls:        true,
	EdgeReferences:   true,
	EdgeInstantiates: true,
	EdgeImplements:   true,
	EdgeExtends:      true,
	EdgeReads:        true,
	EdgeWrites:       true,
	EdgeTests:        true,
}

// ClassifyZeroEdge inspects a symbol's incoming and outgoing edges and
// returns how an empty usage/caller/impact query for it should be read.
//
//   - ZeroEdgeNone — the symbol has at least one incoming usage edge.
//   - ZeroEdgeLikelyUnused — no incoming usage edge, but the symbol has
//     other graph edges (structural defines/member_of, or any outgoing
//     edge). Consistent with genuine dead code.
//   - ZeroEdgePossibleExtractionGap — the symbol has no edges at all,
//     which a normally indexed symbol never has; the extractor most
//     likely missed it.
//
// An unknown symbol ID is reported as an extraction gap: a query whose
// target is not even in the graph is exactly as untrustworthy as one
// whose target was never wired up.
func ClassifyZeroEdge(g Store, symbolID string) ZeroEdgeClass {
	if g == nil || symbolID == "" {
		return ZeroEdgePossibleExtractionGap
	}
	if g.GetNode(symbolID) == nil {
		return ZeroEdgePossibleExtractionGap
	}

	in := g.GetInEdges(symbolID)
	out := g.GetOutEdges(symbolID)

	if len(in) == 0 && len(out) == 0 {
		return ZeroEdgePossibleExtractionGap
	}
	for _, e := range in {
		if usageEdgeKinds[e.Kind] {
			return ZeroEdgeNone
		}
	}
	return ZeroEdgeLikelyUnused
}

// zeroEdgeMessages maps each classification to its human-readable
// caveat text.
var zeroEdgeMessages = map[ZeroEdgeClass]string{
	ZeroEdgeLikelyUnused: "no incoming call or reference edges, but the symbol is " +
		"indexed (it has structural or outgoing edges) — consistent with genuine " +
		"unused code that is safe to remove.",
	ZeroEdgePossibleExtractionGap: "the symbol has no graph edges of any kind. A " +
		"normally indexed symbol always has at least a structural edge, so the " +
		"extractor most likely did not process it — treat this empty result as " +
		"unverified, not as proof the symbol is unused.",
}

// CaveatForZeroEdge builds the structured caveat for an empty graph
// query result on symbolID. It returns nil when the symbol has
// incoming usage edges (ZeroEdgeNone) — a non-empty result carries no
// caveat — so callers can attach the return value unconditionally.
func CaveatForZeroEdge(g Store, symbolID string) *ZeroEdgeCaveat {
	class := ClassifyZeroEdge(g, symbolID)
	if class == ZeroEdgeNone {
		return nil
	}
	return &ZeroEdgeCaveat{Class: class, Message: zeroEdgeMessages[class]}
}
