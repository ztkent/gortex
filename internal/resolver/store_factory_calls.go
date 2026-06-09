package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// storeFactoryVia is the Meta["via"] tag the JS/TS extractors stamp on a
// store-factory call placeholder the same-file pass could not bind (the store
// lives in another file).
const storeFactoryVia = "store-factory"

// ResolveStoreFactoryCalls binds Zustand/Redux/Pinia/MobX indirect action
// calls — `useStore.getState().fetchUser()` and `const {fetchUser} =
// useStore.getState(); fetchUser()` — to the precise action node, across files.
//
// The extractors stamp each store-action node with Meta["store_factory"]=<binding>
// + Meta["store_member"]=<member>, and stamp the cross-file call placeholder
// with Meta["via"]="store-factory" + store_binding + store_action. This pass
// joins them by (binding, member). It is strictly more precise than the
// competitor's bare-name matching: two stores that each define `reset` do not
// collide, because the placeholder carries the specific store binding and the
// pass prefers the candidate in the caller's file (then a unique binding match,
// then a singleton). Returns the number of call edges landed on an action.
func ResolveStoreFactoryCalls(g graph.Store) int {
	if g == nil {
		return 0
	}
	// index: binding → member → action nodes (multiple files may reuse the
	// same binding name, hence a slice).
	index := map[string]map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindFunction) {
		if n == nil || n.Meta == nil {
			continue
		}
		binding, _ := n.Meta["store_factory"].(string)
		if binding == "" {
			continue
		}
		member, _ := n.Meta["store_member"].(string)
		if member == "" {
			member = n.Name
		}
		if index[binding] == nil {
			index[binding] = map[string][]*graph.Node{}
		}
		index[binding][member] = append(index[binding][member], n)
	}
	if len(index) == 0 {
		return 0
	}

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != storeFactoryVia {
			continue
		}
		binding, _ := e.Meta["store_binding"].(string)
		action, _ := e.Meta["store_action"].(string)
		if binding == "" || action == "" {
			continue
		}
		cands := index[binding][action]
		target := pickStoreAction(g, e, cands)

		want := "unresolved::*." + action
		if target != nil {
			want = target.ID
		}
		if e.To == want {
			if target != nil {
				resolved++
			}
			continue
		}
		oldTo := e.To
		e.To = want
		if target != nil {
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0.75
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.75)
			StampSynthesized(e, SynthStoreFactory)
			resolved++
		} else {
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0
			e.ConfidenceLabel = ""
			UnstampSynthesized(e)
		}
		reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// pickStoreAction disambiguates action candidates for a call: prefer the
// candidate in the caller's file, then a unique binding match, then a
// singleton. Returns nil when the choice is ambiguous (never guesses).
func pickStoreAction(g graph.Store, call *graph.Edge, cands []*graph.Node) *graph.Node {
	switch len(cands) {
	case 0:
		return nil
	case 1:
		return cands[0]
	}
	// Multiple stores share this binding name across files. Prefer the one in
	// the caller's file; if the caller's file imports exactly one candidate's
	// file, prefer that; otherwise it's ambiguous.
	callerFile := ""
	if cn := g.GetNode(call.From); cn != nil {
		callerFile = cn.FilePath
	}
	var sameFile *graph.Node
	for _, c := range cands {
		if c.FilePath == callerFile && callerFile != "" {
			if sameFile != nil {
				return nil // two same-file candidates: ambiguous
			}
			sameFile = c
		}
	}
	return sameFile
}
