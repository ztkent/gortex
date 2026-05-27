package store_ladybug

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies the resolver-side
// pushdown capabilities used by the global graph passes
// (InferImplements, InferOverrides, DetectCrossRepoEdges). A drift
// in any signature fails the build here instead of silently dropping
// to the Go-loop fallback.
var (
	_ graph.MemberMethodsByType   = (*Store)(nil)
	_ graph.StructuralParentEdges = (*Store)(nil)
	_ graph.CrossRepoCandidates   = (*Store)(nil)
)

// MemberMethodsByType returns the typeID → []MemberMethodInfo
// projection of every EdgeMemberOf edge whose source is a KindMethod
// node, in one Cypher round-trip. Replaces the resolver's
// EdgesByKind(EdgeMemberOf) + per-edge GetNode(e.From) loop — each
// per-edge GetNode pulled ~10 string columns + a Meta blob over cgo
// just to read five scalar fields. The capability ships only the
// (type_id, method_id, method_name, file_path, start_line,
// repo_prefix) tuple.
//
// Per-type rows are deduplicated by MethodID — a method that appears
// twice in the EdgeMemberOf bucket (e.g. emitted from a re-index)
// yields a single info row.
func (s *Store) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	const q = `
MATCH (m:Node)-[e:Edge {kind: 'member_of'}]->(t:Node)
WHERE m.kind = 'method'
RETURN t.id, m.id, m.name, m.file_path, m.start_line, m.repo_prefix`
	rows := s.querySelect(q, nil)
	if len(rows) == 0 {
		return nil
	}
	if len(rows) >= mallocTrimRowThreshold {
		mallocTrim()
	}
	out := make(map[string][]graph.MemberMethodInfo)
	seen := make(map[string]map[string]struct{})
	for _, r := range rows {
		if len(r) < 6 {
			continue
		}
		typeID, _ := r[0].(string)
		methodID, _ := r[1].(string)
		methodName, _ := r[2].(string)
		filePath, _ := r[3].(string)
		startLine := int(asInt64(r[4]))
		repoPrefix, _ := r[5].(string)
		if typeID == "" || methodID == "" {
			continue
		}
		dedup := seen[typeID]
		if dedup == nil {
			dedup = make(map[string]struct{})
			seen[typeID] = dedup
		}
		if _, ok := dedup[methodID]; ok {
			continue
		}
		dedup[methodID] = struct{}{}
		out[typeID] = append(out[typeID], graph.MemberMethodInfo{
			MethodID:   methodID,
			Name:       methodName,
			FilePath:   filePath,
			StartLine:  startLine,
			RepoPrefix: repoPrefix,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StructuralParentEdges returns every EdgeExtends / EdgeImplements /
// EdgeComposes edge whose endpoints are both KindType / KindInterface,
// projected as (FromID, ToID, FromKind, ToKind, Origin) in one Cypher
// round-trip. Replaces the InferOverrides AllEdges + per-edge
// GetNode(e.From) + GetNode(e.To) loop — on the gortex workspace the
// AllEdges scan materialised ~286k edges over cgo just to filter down
// to a few hundred type-to-type rows.
func (s *Store) StructuralParentEdges() []graph.StructuralParentEdgeRow {
	const q = `
MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE e.kind IN ['extends', 'implements', 'composes']
  AND a.kind IN ['type', 'interface']
  AND b.kind IN ['type', 'interface']
RETURN a.id, b.id, a.kind, b.kind, e.origin`
	rows := s.querySelect(q, nil)
	if len(rows) == 0 {
		return nil
	}
	if len(rows) >= mallocTrimRowThreshold {
		mallocTrim()
	}
	out := make([]graph.StructuralParentEdgeRow, 0, len(rows))
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		fromID, _ := r[0].(string)
		toID, _ := r[1].(string)
		if fromID == "" || toID == "" {
			continue
		}
		fromKind, _ := r[2].(string)
		toKind, _ := r[3].(string)
		origin, _ := r[4].(string)
		out = append(out, graph.StructuralParentEdgeRow{
			FromID:   fromID,
			ToID:     toID,
			FromKind: graph.NodeKind(fromKind),
			ToKind:   graph.NodeKind(toKind),
			Origin:   origin,
		})
	}
	return out
}

// CrossRepoCandidates returns every edge whose Kind is in baseKinds
// AND whose endpoints carry two distinct, non-empty RepoPrefix
// values, projected with the underlying edge plus the two repo
// prefixes. Replaces the DetectCrossRepoEdges AllEdges + per-edge
// GetNode(e.From) + GetNode(e.To) loop — the in-memory scan ships
// every edge over cgo plus issues two GetNode round-trips per
// surviving row, while typical cross-repo rows are a small fraction
// of the edge table.
func (s *Store) CrossRepoCandidates(baseKinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	uniq := dedupeEdgeKinds(baseKinds)
	if len(uniq) == 0 {
		return nil
	}
	const q = `
MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE e.kind IN $kinds
  AND a.repo_prefix <> ''
  AND b.repo_prefix <> ''
  AND a.repo_prefix <> b.repo_prefix
RETURN ` + edgeReturnCols + `, a.repo_prefix, b.repo_prefix`
	rows := s.querySelect(q, map[string]any{"kinds": edgeKindSliceToAny(uniq)})
	if len(rows) == 0 {
		return nil
	}
	if len(rows) >= mallocTrimRowThreshold {
		mallocTrim()
	}
	out := make([]graph.CrossRepoCandidateRow, 0, len(rows))
	for _, r := range rows {
		if len(r) < 13 {
			continue
		}
		e := rowToEdge(r[:11])
		if e == nil {
			continue
		}
		fromRepo, _ := r[11].(string)
		toRepo, _ := r[12].(string)
		out = append(out, graph.CrossRepoCandidateRow{
			Edge:     e,
			FromRepo: fromRepo,
			ToRepo:   toRepo,
		})
	}
	return out
}
