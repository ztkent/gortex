package mcp

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// applyRerankBoosts reorders nodes by combining locality + combo +
// frecency signals on top of the backend's original BM25-like order.
//
// Locality is the workspace-isolation relevance tier: results are
// already confined to the session's workspace, so within that set a
// same-repo hit outranks a same-project hit, which outranks the rest
// of the workspace. The agent never has to ask for a scope — it gets
// the whole legal workspace, ordered closest-to-home first.
// repoPrefix / projectID are the session's home repo and project;
// both empty (unbound session) disables the locality tier.
//
// All inputs may be inert; if every signal produces a unit multiplier
// the stable sort is a no-op and input order is preserved byte-for-
// byte. Kept in one pass so the comparator sees the final combined
// multiplier per node.
func applyRerankBoosts(nodes []*graph.Node, cm *comboManager, ft *frecencyTracker, query, repoPrefix, projectID string) []*graph.Node {
	if len(nodes) < 2 {
		return nodes
	}
	var comboBoosts map[string]float64
	if cm != nil {
		comboBoosts = cm.BoostMap(query)
	}
	hasLocality := repoPrefix != "" || projectID != ""
	// Only sort when at least one signal has something to say.
	if len(comboBoosts) == 0 && (ft == nil || !ft.HasData()) && !hasLocality {
		return nodes
	}

	// localityBoost ranks same-repo above same-project above the rest
	// of the workspace (the multiplicative baseline, since results are
	// already workspace-bounded).
	localityBoost := func(n *graph.Node) float64 {
		if repoPrefix != "" && n.RepoPrefix == repoPrefix {
			return 3.0
		}
		if projectID != "" {
			proj := n.ProjectID
			if proj == "" {
				proj = n.RepoPrefix
			}
			if proj == projectID {
				return 2.0
			}
		}
		return 1.0
	}

	multiplier := func(n *graph.Node) float64 {
		m := localityBoost(n)
		if b, ok := comboBoosts[n.ID]; ok {
			m *= b
		}
		if ft != nil {
			m *= ft.BoostFor(n.ID)
		}
		return m
	}

	sort.SliceStable(nodes, func(i, j int) bool {
		return multiplier(nodes[i]) > multiplier(nodes[j])
	})
	return nodes
}

// recordLastSearchFromNodes stores the query + top-limit IDs on the session
// so a subsequent get_symbol_source / get_editing_context can credit this
// search. Capped at limit to avoid crediting results the agent never saw.
func recordLastSearchFromNodes(sess *sessionState, query string, nodes []*graph.Node, limit int) {
	if sess == nil || len(nodes) == 0 {
		return
	}
	if limit <= 0 || limit > len(nodes) {
		limit = len(nodes)
	}
	ids := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		ids = append(ids, nodes[i].ID)
	}
	sess.recordLastSearch(query, ids)
}
