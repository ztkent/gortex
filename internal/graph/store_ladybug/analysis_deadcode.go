package store_ladybug

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies the dead-code-related
// graph capabilities so analysis.FindDeadCode picks the server-side
// path via type assertion. If a signature drifts the build fails
// here instead of silently falling through to the Go-loop fallback.
var (
	_ graph.DeadCodeCandidator     = (*Store)(nil)
	_ graph.IfaceImplementsScanner = (*Store)(nil)
)

// DeadCodeCandidates evaluates the dead-code candidate filter
// entirely inside Ladybug. The Go-side fallback (analysis.FindDeadCode
// without this capability) materialises ~133k Node + ~1.3M in-edge
// rows over cgo per call — 49s wall on the gortex workspace; this
// path keeps the per-row materialisation on the server and only
// returns the surviving ~hundreds of candidates.
//
// Strategy: one Cypher per requested node kind. A single combined
// query that switches the allowlist per row is harder to express in
// Kuzu Cypher than the ~6-8 per-kind queries cost (and the per-query
// cgo overhead is amortised against the rows that DO ship back).
// Shape: WHERE NOT EXISTS { MATCH ()-[e:Edge]->(n) WHERE e.kind IN
// $allowed }, confirmed via TestDeadCode_Probe.
func (s *Store) DeadCodeCandidates(allowedNodeKinds []graph.NodeKind, allowedInEdgeKinds map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	if len(allowedNodeKinds) == 0 {
		return nil
	}
	// Dedup the kind set so an over-eager caller doesn't double-scan.
	seen := make(map[graph.NodeKind]struct{}, len(allowedNodeKinds))
	kinds := make([]graph.NodeKind, 0, len(allowedNodeKinds))
	for _, k := range allowedNodeKinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		kinds = append(kinds, k)
	}

	var out []*graph.Node
	for _, k := range kinds {
		allow := allowedInEdgeKinds[k]
		out = append(out, s.deadCodeCandidatesForKind(k, allow)...)
	}
	return out
}

// deadCodeCandidatesForKind runs the per-node-kind Cypher and
// materialises the matching nodes. When allow is empty the query
// degenerates to "no incoming edges of any kind" — the in-memory
// reference implementation does the same.
func (s *Store) deadCodeCandidatesForKind(kind graph.NodeKind, allow []graph.EdgeKind) []*graph.Node {
	if len(allow) == 0 {
		// Fast path: any incoming edge counts as usage. Cypher
		// without the IN $allowed filter — slightly cheaper plan.
		const q = `
MATCH (n:Node {kind: $kind})
WHERE NOT EXISTS { MATCH (:Node)-[:Edge]->(n) }
RETURN ` + nodeReturnCols
		rows := s.querySelect(q, map[string]any{"kind": string(kind)})
		return rowsToNodes(rows)
	}
	allowed := make([]any, 0, len(allow))
	dedup := make(map[graph.EdgeKind]struct{}, len(allow))
	for _, ek := range allow {
		if _, ok := dedup[ek]; ok {
			continue
		}
		dedup[ek] = struct{}{}
		allowed = append(allowed, string(ek))
	}
	const q = `
MATCH (n:Node {kind: $kind})
WHERE NOT EXISTS {
    MATCH (:Node)-[e:Edge]->(n)
    WHERE e.kind IN $allowed
}
RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{
		"kind":    string(kind),
		"allowed": allowed,
	})
	return rowsToNodes(rows)
}

// IfaceImplementsRows joins KindInterface nodes carrying
// Meta["methods"] with their EdgeImplements predecessors in one
// Cypher round-trip. Replaces the Go-side iterate-then-filter loop
// the analyzer used before this capability landed — that loop
// pulled every interface node, then ranged g.EdgesByKind(implements)
// for the whole graph, every analyze(dead_code) call.
//
// `iface.meta <> ''` excludes interfaces with no encoded Meta
// payload (encodeMeta serialises an empty map to ""). Rows that
// survive are decoded Go-side via decodeMeta.
func (s *Store) IfaceImplementsRows() []graph.IfaceImplementsRow {
	const q = `
MATCH (t:Node)-[e:Edge]->(iface:Node)
WHERE e.kind = $impl
  AND iface.kind = $iface
  AND iface.meta <> ''
RETURN t.id, iface.id, iface.meta`
	rows := s.querySelect(q, map[string]any{
		"impl":  string(graph.EdgeImplements),
		"iface": string(graph.KindInterface),
	})
	if len(rows) == 0 {
		return nil
	}
	out := make([]graph.IfaceImplementsRow, 0, len(rows))
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		typeID, _ := r[0].(string)
		ifaceID, _ := r[1].(string)
		metaStr, _ := r[2].(string)
		if typeID == "" || ifaceID == "" || metaStr == "" {
			continue
		}
		m, err := decodeMeta(metaStr)
		if err != nil || m == nil {
			continue
		}
		out = append(out, graph.IfaceImplementsRow{
			TypeID:    typeID,
			IfaceID:   ifaceID,
			IfaceMeta: m,
		})
	}
	return out
}
