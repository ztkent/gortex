package mcp

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// applyRerankBoosts is the I13 entry point that runs the full
// 11-signal rerank.Pipeline over the candidate set with the
// session-aware Context wired in (locality, combo, frecency,
// feedback, churn, community). The structural signals (BM25 rank,
// fan-in / fan-out, MinHash similarity, signature match, recency)
// are computed off the graph + the candidate's current index.
//
// rerankCtx is the per-request Context built by the server; pass nil
// and the pipeline falls back to a structural-only rerank using just
// the graph data on the nodes. lastResults is the optional rich
// candidate slice — when non-nil it carries per-signal contributions
// out to the caller for debug / winnow surfacing; pass nil if the
// caller only wants the sorted nodes.
func applyRerankBoosts(s *Server, nodes []*graph.Node, query string, rerankCtx *rerank.Context, lastResults *[]*rerank.Candidate) []*graph.Node {
	if len(nodes) < 2 || s == nil || s.engine == nil {
		return nodes
	}
	pipeline := s.engine.Rerank()
	if pipeline == nil {
		return nodes
	}
	cands := make([]*rerank.Candidate, 0, len(nodes))
	for i, n := range nodes {
		cands = append(cands, &rerank.Candidate{
			Node: n, TextRank: i, VectorRank: -1,
		})
	}
	if rerankCtx == nil {
		rerankCtx = &rerank.Context{}
	}
	if rerankCtx.Graph == nil {
		rerankCtx.Graph = s.graph
	}
	pipeline.Rerank(query, cands, rerankCtx)
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Node)
	}
	if lastResults != nil {
		*lastResults = cands
	}
	return out
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
