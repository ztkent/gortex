package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// Speculative dynamic-dispatch synthesis. Some call shapes are genuine static
// blind spots that no precision-first framework rule covers: computed member
// calls `obj["foo"]()`, getattr-style dispatch `getattr(o, "foo")()`, and
// decorator registries. The extractor stamps these dropped calls with
// Meta["dyn_shape"] + Meta["dyn_key"] (the method name when it is a literal),
// and this opt-in pass mints LOW-confidence best-guess `calls` edges to the
// plausible same-name targets — tagged OriginSpeculative + Meta[speculative]
// so they are hidden from every default query and surfaced only on demand.
//
// This is what codegraph's playbook refused as "partial coverage worse than
// none": gortex can ship it because the edges are present-but-hidden-by-default
// (zero pollution) and explicitly auditable via `analyze kind=speculative`.

const (
	// speculativeFanoutCap: above this many candidates the confidence floors;
	// above the hard cap the whole set is dropped as noise (codegraph's rule).
	speculativeFanoutCap = 12
	speculativeHardCap    = 40
)

// ResolveSpeculativeDispatch mints speculative call edges for the tagged
// blind-spot call shapes. No-op when disabled. Returns the edge count.
func ResolveSpeculativeDispatch(g graph.Store, enabled bool) int {
	if g == nil || !enabled {
		return 0
	}

	// Existing resolved call pairs, so a speculative edge never duplicates a
	// real one from the same caller to the same target.
	existing := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.IsSpeculative() {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			existing[e.From+"\x00"+e.To] = true
		}
	}

	type spec struct {
		from, to, file, shape, key string
		line, n                    int
	}
	var specs []spec
	seen := map[string]bool{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		shape, _ := e.Meta["dyn_shape"].(string)
		if shape == "" {
			continue
		}
		key, _ := e.Meta["dyn_key"].(string)
		if key == "" {
			continue // v1: literal-key shapes only (variable-key is unbounded)
		}
		callerLang := ""
		if c := g.GetNode(e.From); c != nil {
			callerLang = c.Language
		}
		cands := speculativeCandidates(g, key, callerLang)
		if len(cands) == 0 || len(cands) > speculativeHardCap {
			continue
		}
		for _, c := range cands {
			if existing[e.From+"\x00"+c.ID] {
				continue
			}
			k := e.From + "\x00" + c.ID
			if seen[k] {
				continue
			}
			seen[k] = true
			specs = append(specs, spec{from: e.From, to: c.ID, file: e.FilePath, line: e.Line, shape: shape, key: key, n: len(cands)})
		}
	}

	resolved := 0
	for _, s := range specs {
		conf := 1.0 / float64(s.n)
		if s.n > speculativeFanoutCap {
			conf = 0.05
		}
		if conf > 0.45 {
			conf = 0.45
		}
		if conf < 0.05 {
			conf = 0.05
		}
		g.AddEdge(&graph.Edge{
			From: s.from, To: s.to, Kind: graph.EdgeCalls,
			FilePath: s.file, Line: s.line,
			Origin:          graph.OriginSpeculative,
			Confidence:      conf,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, conf),
			Meta: map[string]any{
				graph.MetaSpeculative: true,
				MetaSynthesizedBy:     SynthSpeculative,
				MetaProvenance:        ProvenanceHeuristic,
				"via":                 "speculative." + s.shape,
				"candidate_count":     s.n,
				"dyn_key":             s.key,
			},
		})
		resolved++
	}
	return resolved
}

// speculativeCandidates returns the plausible targets for a literal dispatch
// key: function/method definitions of that exact name, in the caller's
// language family, excluding stubs.
func speculativeCandidates(g graph.Store, key, callerLang string) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.FindNodesByName(key) {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) {
			continue
		}
		if callerLang != "" && n.Language != "" && n.Language != callerLang {
			continue
		}
		out = append(out, n)
	}
	return out
}
