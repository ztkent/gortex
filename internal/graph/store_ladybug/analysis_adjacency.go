package store_ladybug

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies the adjacency-shaped
// pushdown capabilities for the betweenness + hotspots wave. A drift
// in any signature fails the build here instead of silently dropping
// to the Go-loop fallback.
var (
	_ graph.EdgeAdjacencyForKinds    = (*Store)(nil)
	_ graph.CommunityCrossingsByKind = (*Store)(nil)
	_ graph.NodeIDsByKinds           = (*Store)(nil)
)

// EdgeAdjacencyForKinds returns (from, to) id pairs for every edge
// whose Kind is in edgeKinds AND whose endpoints both have a Kind in
// nodeKinds. Replaces the EdgesByKinds-then-filter pass the
// betweenness adjacency build used to run — every per-edge row
// carried ~10 string columns over cgo just for the From/To pair, and
// the cross-kind edges (where one endpoint isn't a function/method)
// flowed through cgo too even though the caller discarded them.
//
// The capability returns a 2-column projection from a single Cypher
// join. The IN-list dedup matches the EdgesByKinds contract.
func (s *Store) EdgeAdjacencyForKinds(edgeKinds []graph.EdgeKind, nodeKinds []graph.NodeKind) iter.Seq[[2]string] {
	if len(edgeKinds) == 0 || len(nodeKinds) == 0 {
		return func(yield func([2]string) bool) {}
	}
	eKinds := edgeKindSliceToAny(dedupeEdgeKinds(edgeKinds))
	if len(eKinds) == 0 {
		return func(yield func([2]string) bool) {}
	}
	nKinds := nodeKindSliceToAny(dedupeNodeKinds(nodeKinds))
	if len(nKinds) == 0 {
		return func(yield func([2]string) bool) {}
	}
	const q = `
MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE e.kind IN $ekinds
  AND a.kind IN $nkinds
  AND b.kind IN $nkinds
RETURN a.id, b.id`
	rows := s.querySelect(q, map[string]any{
		"ekinds": eKinds,
		"nkinds": nKinds,
	})
	return func(yield func([2]string) bool) {
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			from, _ := r[0].(string)
			to, _ := r[1].(string)
			if from == "" || to == "" {
				continue
			}
			if !yield([2]string{from, to}) {
				return
			}
		}
	}
}

// CommunityCrossingsByKind ships only the (from, to) projection of
// edges whose Kind is in the supplied set and lets the Go side do
// the community comparison. Community membership is not a Node
// column — it's computed at runtime by the analyzer — so the
// comparison can't live in Cypher today. The win is the column
// projection: where FindHotspots.countCrossings used to pull the
// full edge row (~10 columns) twice (once per kind) over cgo, this
// single call returns 2 columns from one IN-list join.
//
// Zero-count sources are dropped so callers can probe existence
// without a >0 check.
func (s *Store) CommunityCrossingsByKind(kinds []graph.EdgeKind, nodeToComm map[string]string) map[string]int {
	if len(kinds) == 0 || len(nodeToComm) == 0 {
		return nil
	}
	allowed := edgeKindSliceToAny(dedupeEdgeKinds(kinds))
	if len(allowed) == 0 {
		return nil
	}
	const q = `
MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE e.kind IN $kinds
RETURN a.id, b.id`
	rows := s.querySelect(q, map[string]any{"kinds": allowed})
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		from, _ := r[0].(string)
		to, _ := r[1].(string)
		if from == "" || to == "" {
			continue
		}
		fc := nodeToComm[from]
		tc := nodeToComm[to]
		if fc == "" || tc == "" || fc == tc {
			continue
		}
		out[from]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NodeIDsByKinds returns the IDs of every node whose Kind is in the
// supplied set. Identical filter shape to NodesByKinds, but ships
// only the id column — one C string per row instead of ~10. On the
// gortex workspace the betweenness/hotspots candidate set is ~4k
// rows; the projection cuts the cgo string-alloc count by an order
// of magnitude per call.
func (s *Store) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	if len(kinds) == 0 {
		return nil
	}
	allowed := nodeKindSliceToAny(dedupeNodeKinds(kinds))
	if len(allowed) == 0 {
		return nil
	}
	const q = `MATCH (n:Node) WHERE n.kind IN $kinds RETURN n.id`
	rows := s.querySelect(q, map[string]any{"kinds": allowed})
	if len(rows) == 0 {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) < 1 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

// dedupeNodeKinds is the node-kind counterpart of dedupeEdgeKinds —
// the kinds-IN scanners use it to collapse repeats so the Cypher
// IN-list matches the in-memory reference's behaviour.
func dedupeNodeKinds(kinds []graph.NodeKind) []graph.NodeKind {
	if len(kinds) == 0 {
		return nil
	}
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	out := make([]graph.NodeKind, 0, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// nodeKindSliceToAny converts a deduped node-kind slice into the
// []any shape the Cypher binding expects for IN-list parameters.
func nodeKindSliceToAny(kinds []graph.NodeKind) []any {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]any, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
