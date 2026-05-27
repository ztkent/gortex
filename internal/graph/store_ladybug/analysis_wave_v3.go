package store_ladybug

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies the per-tool pushdown
// capabilities introduced by the wave-3 MCP-tool perf push. A drift
// in any signature fails the build here instead of silently dropping
// to the in-memory fallback path.
var (
	_ graph.ExtractCandidatesScanner = (*Store)(nil)
	_ graph.FileSymbolNamesByPaths   = (*Store)(nil)
	_ graph.ClassHierarchyTraverser  = (*Store)(nil)
	_ graph.FileEditingContext       = (*Store)(nil)
	_ graph.NodeDegreeByKinds        = (*Store)(nil)
)

// ExtractCandidates evaluates per-function caller-count + fan-out
// directly inside Ladybug. Two Cypher aggregates by node ID over the
// requested edge-kind set, joined to the node table on the function /
// method kind set, with the three threshold gates applied server-
// side. Replaces the AllNodes + per-node GetInEdges + GetOutEdges loop
// the handler ran previously — that fired 2N cgo round-trips on a
// 30k-function graph, where each per-call materialised the full edge
// bucket just to count distinct endpoints.
//
// DISTINCT counts mirror the in-memory reference: one caller counted
// once per (From) value, one callee once per (To) value.
func (s *Store) ExtractCandidates(
	kinds []graph.EdgeKind,
	minLines, minCallers, minFanOut int,
	pathPrefix string,
) []graph.ExtractCandidateRow {
	if len(kinds) == 0 {
		return nil
	}
	ek := edgeKindSliceToAny(dedupeEdgeKinds(kinds))
	if len(ek) == 0 {
		return nil
	}
	// Two aggregations are cheaper than one COUNT { … } per node when
	// the result set is small after the threshold gates: matching the
	// edge table once and grouping by anchor gives the planner a
	// chance to drop nodes with zero callers / zero fan-out before the
	// join, which the COUNT { … } shape can't express.
	const callerQ = `
MATCH (n:Node)<-[e:Edge]-(c:Node)
WHERE n.kind IN ['function', 'method']
  AND e.kind IN $kinds
RETURN n.id, COUNT(DISTINCT c.id)`
	const calleeQ = `
MATCH (n:Node)-[e:Edge]->(c:Node)
WHERE n.kind IN ['function', 'method']
  AND e.kind IN $kinds
RETURN n.id, COUNT(DISTINCT c.id)`

	callerRows := s.querySelect(callerQ, map[string]any{"kinds": ek})
	calleeRows := s.querySelect(calleeQ, map[string]any{"kinds": ek})

	type counts struct{ callers, fanOut int }
	merged := make(map[string]*counts, len(callerRows))
	getOrCreate := func(id string) *counts {
		c, ok := merged[id]
		if !ok {
			c = &counts{}
			merged[id] = c
		}
		return c
	}
	for _, r := range callerRows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		getOrCreate(id).callers = int(asInt64(r[1]))
	}
	for _, r := range calleeRows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		getOrCreate(id).fanOut = int(asInt64(r[1]))
	}

	// Threshold-filter the candidate IDs Go-side first — minCallers /
	// minFanOut shave the IN-list before we look up the node columns.
	keep := make([]string, 0, len(merged))
	for id, c := range merged {
		if c.callers < minCallers || c.fanOut < minFanOut {
			continue
		}
		keep = append(keep, id)
	}
	if len(keep) == 0 {
		return nil
	}

	// Single Cypher pull for the node columns the row needs.
	const nodeQ = `
MATCH (n:Node)
WHERE n.id IN $ids
RETURN n.id, n.name, n.file_path, n.start_line, n.end_line`
	nodeRows := s.querySelect(nodeQ, map[string]any{"ids": stringSliceToAny(keep)})

	out := make([]graph.ExtractCandidateRow, 0, len(nodeRows))
	for _, r := range nodeRows {
		if len(r) < 5 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		name, _ := r[1].(string)
		fp, _ := r[2].(string)
		if pathPrefix != "" && !strings.HasPrefix(fp, pathPrefix) {
			continue
		}
		start := int(asInt64(r[3]))
		end := int(asInt64(r[4]))
		if start == 0 || end == 0 {
			continue
		}
		lineCount := end - start + 1
		if lineCount < minLines {
			continue
		}
		c := merged[id]
		if c == nil {
			continue
		}
		out = append(out, graph.ExtractCandidateRow{
			NodeID:      id,
			Name:        name,
			FilePath:    fp,
			StartLine:   start,
			EndLine:     end,
			LineCount:   lineCount,
			CallerCount: c.callers,
			FanOut:      c.fanOut,
		})
	}
	return out
}

// FileSymbolNamesByPaths runs one Cypher MATCH with the path + kind
// IN-lists, returning (file_path, name) pairs. Replaces the per-path
// GetFileNodes loop find_co_changing_symbols ran after a positive
// match — that's 20 separate Cypher queries against the file_path
// secondary index in the previous shape.
func (s *Store) FileSymbolNamesByPaths(paths []string, kinds []graph.NodeKind) []graph.FileSymbolNameRow {
	if len(paths) == 0 {
		return nil
	}
	uniqPaths := dedupeNonEmpty(paths)
	if len(uniqPaths) == 0 {
		return nil
	}
	const qAll = `
MATCH (n:Node)
WHERE n.file_path IN $paths
RETURN n.file_path, n.name`
	const qKinds = `
MATCH (n:Node)
WHERE n.file_path IN $paths
  AND n.kind IN $kinds
RETURN n.file_path, n.name`
	q := qAll
	args := map[string]any{"paths": stringSliceToAny(uniqPaths)}
	if len(kinds) > 0 {
		nk := nodeKindSliceToAny(dedupeNodeKinds(kinds))
		if len(nk) == 0 {
			return nil
		}
		q = qKinds
		args["kinds"] = nk
	}
	rows := s.querySelect(q, args)
	if len(rows) == 0 {
		return nil
	}
	type pair struct{ p, n string }
	seen := make(map[pair]struct{}, len(rows))
	out := make([]graph.FileSymbolNameRow, 0, len(rows))
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		fp, _ := r[0].(string)
		name, _ := r[1].(string)
		if fp == "" || name == "" {
			continue
		}
		key := pair{fp, name}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, graph.FileSymbolNameRow{FilePath: fp, Name: name})
	}
	return out
}

// ClassHierarchyTraverse evaluates the inheritance subgraph rooted at
// the seed inside Ladybug. One variable-length traversal per
// direction replaces the per-frontier-node GetNode + GetInEdges /
// GetOutEdges loop query.ClassHierarchy ran — that was depth * width
// cgo round-trips on Ladybug, each round-trip materialising the full
// edge bucket just to filter on a handful of kinds.
//
// The result rows carry the Path (visited IDs in BFS order, exclusive
// of the seed) plus the per-hop EdgeKinds so the caller can rebuild
// the visited node set + edge identities without further graph
// traversal.
func (s *Store) ClassHierarchyTraverse(
	seedID string,
	direction string,
	kinds []graph.EdgeKind,
	depth int,
) []graph.ClassHierarchyRow {
	if seedID == "" || depth <= 0 || len(kinds) == 0 {
		return nil
	}
	ek := edgeKindSliceToAny(dedupeEdgeKinds(kinds))
	if len(ek) == 0 {
		return nil
	}
	walkUp := direction == "up"
	walkDown := direction == "down"
	if !walkUp && !walkDown {
		return nil
	}
	if depth > 64 {
		depth = 64
	}
	// BFS Cypher: one query per hop avoids re-walking the same
	// frontier on each iteration. Ladybug's planner handles
	// variable-length patterns, but per-hop is cheaper here because
	// the kind filter restricts the per-hop fanout dramatically (most
	// nodes have <5 hierarchy edges) and we want to enforce the
	// "first reached wins" visited-set semantic the in-memory
	// reference implements.
	visited := map[string]struct{}{seedID: {}}
	type row struct {
		path      []string
		edgeKinds []graph.EdgeKind
	}
	frontier := []row{{path: nil, edgeKinds: nil}}
	frontierIDs := []string{seedID}
	var out []graph.ClassHierarchyRow
	for hop := 0; hop < depth && len(frontierIDs) > 0; hop++ {
		var q string
		if walkUp {
			q = `MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE a.id IN $ids AND e.kind IN $kinds
RETURN a.id, b.id, e.kind`
		} else {
			q = `MATCH (a:Node)-[e:Edge]->(b:Node)
WHERE b.id IN $ids AND e.kind IN $kinds
RETURN b.id, a.id, e.kind`
		}
		rows := s.querySelect(q, map[string]any{
			"ids":   stringSliceToAny(frontierIDs),
			"kinds": ek,
		})
		if len(rows) == 0 {
			break
		}
		// Group neighbours by their predecessor in the frontier so
		// the row reconstruction joins the per-frontier path with the
		// new hop.
		byPred := make(map[string][]struct {
			nb   string
			kind graph.EdgeKind
		}, len(rows))
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			pred, _ := r[0].(string)
			nb, _ := r[1].(string)
			kind, _ := r[2].(string)
			if pred == "" || nb == "" {
				continue
			}
			byPred[pred] = append(byPred[pred], struct {
				nb   string
				kind graph.EdgeKind
			}{nb: nb, kind: graph.EdgeKind(kind)})
		}
		// Map frontier IDs to their accumulated paths.
		predRow := make(map[string]row, len(frontierIDs))
		for i, id := range frontierIDs {
			predRow[id] = frontier[i]
		}
		nextIDs := make([]string, 0)
		nextFrontier := make([]row, 0)
		for pred, neighbours := range byPred {
			pr, ok := predRow[pred]
			if !ok {
				continue
			}
			for _, nbInfo := range neighbours {
				if _, seen := visited[nbInfo.nb]; seen {
					continue
				}
				visited[nbInfo.nb] = struct{}{}
				newPath := append([]string(nil), pr.path...)
				newPath = append(newPath, nbInfo.nb)
				newKinds := append([]graph.EdgeKind(nil), pr.edgeKinds...)
				newKinds = append(newKinds, nbInfo.kind)
				out = append(out, graph.ClassHierarchyRow{
					Path:      newPath,
					EdgeKinds: newKinds,
				})
				nextIDs = append(nextIDs, nbInfo.nb)
				nextFrontier = append(nextFrontier, row{path: newPath, edgeKinds: newKinds})
			}
		}
		frontierIDs = nextIDs
		frontier = nextFrontier
	}
	return out
}

// FileEditingContext bundles every projection get_editing_context
// needs into the smallest backend round-trip count Ladybug allows.
// Replaces the handler's per-symbol GetCallers + GetCallChain loop —
// a 30-function file fired ~60 query-engine entries on Ladybug
// previously; this caps the surface at five Cypher statements
// regardless of file size.
func (s *Store) FileEditingContext(filePath string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	if filePath == "" {
		return nil
	}
	const fileQ = `MATCH (n:Node {file_path: $f}) RETURN ` + nodeReturnCols
	rows := s.querySelect(fileQ, map[string]any{"f": filePath})
	nodes := rowsToNodes(rows)
	if len(nodes) == 0 {
		return nil
	}
	kset := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	res := &graph.FileEditingContextResult{}
	var defIDs []string
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind == graph.KindFile {
			res.FileNode = n
			continue
		}
		res.Defines = append(res.Defines, n)
		if _, ok := kset[n.Kind]; ok {
			defIDs = append(defIDs, n.ID)
		}
	}
	if res.FileNode != nil {
		const importQ = `MATCH (a:Node {id: $id})-[e:Edge]->(b:Node)
WHERE e.kind = 'imports'
RETURN ` + edgeReturnCols
		importRows := s.querySelect(importQ, map[string]any{"id": res.FileNode.ID})
		res.Imports = rowsToEdges(importRows)
	}
	if len(defIDs) == 0 {
		return res
	}
	// One IN-list scan per direction — the caller / callee node columns
	// come back in the same round-trip via a join on the call edge.
	callerQ := `
MATCH (caller:Node)-[e:Edge]->(callee:Node)
WHERE callee.id IN $ids
  AND e.kind = 'calls'
  AND caller.file_path <> $file
RETURN DISTINCT ` + prefixedNodeReturnCols("caller")
	calleeQ := `
MATCH (caller:Node)-[e:Edge]->(callee:Node)
WHERE caller.id IN $ids
  AND e.kind = 'calls'
  AND callee.file_path <> $file
RETURN DISTINCT ` + prefixedNodeReturnCols("callee")
	callerRows := s.querySelect(callerQ, map[string]any{
		"ids":  stringSliceToAny(defIDs),
		"file": filePath,
	})
	res.CalledBy = rowsToNodes(callerRows)
	calleeRows := s.querySelect(calleeQ, map[string]any{
		"ids":  stringSliceToAny(defIDs),
		"file": filePath,
	})
	res.Calls = rowsToNodes(calleeRows)
	return res
}

// NodeDegreeByKinds computes per-node total in/out edge counts for
// every node whose kind is in the supplied set, server-side. Replaces
// the IN-list-of-30k-IDs shape NodeDegreeCounts uses — the planner has
// to materialise the IN-list before joining, where this query lets it
// pick the kind-filtered node set up front (smaller working set, no
// IN-list bloat).
func (s *Store) NodeDegreeByKinds(kinds []graph.NodeKind, pathPrefix string) []graph.NodeDegreeRow {
	if len(kinds) == 0 {
		return nil
	}
	nk := nodeKindSliceToAny(dedupeNodeKinds(kinds))
	if len(nk) == 0 {
		return nil
	}
	withPrefix := pathPrefix != ""

	// COUNT { … } sub-query is the only way to keep this in a single
	// MATCH while still returning a per-node aggregate. The two sub-
	// queries together cost one extra index probe per node.
	var inQ, outQ string
	if withPrefix {
		inQ = `MATCH (n:Node)
WHERE n.kind IN $kinds
  AND starts_with(n.file_path, $prefix)
RETURN n.id, COUNT { MATCH (:Node)-[:Edge]->(n) }`
		outQ = `MATCH (n:Node)
WHERE n.kind IN $kinds
  AND starts_with(n.file_path, $prefix)
RETURN n.id, COUNT { MATCH (n)-[:Edge]->(:Node) }`
	} else {
		inQ = `MATCH (n:Node)
WHERE n.kind IN $kinds
RETURN n.id, COUNT { MATCH (:Node)-[:Edge]->(n) }`
		outQ = `MATCH (n:Node)
WHERE n.kind IN $kinds
RETURN n.id, COUNT { MATCH (n)-[:Edge]->(:Node) }`
	}
	args := map[string]any{"kinds": nk}
	if withPrefix {
		args["prefix"] = pathPrefix
	}
	inRows := s.querySelect(inQ, args)
	outRows := s.querySelect(outQ, args)
	byID := make(map[string]*graph.NodeDegreeRow, len(inRows))
	ensure := func(id string) *graph.NodeDegreeRow {
		r, ok := byID[id]
		if !ok {
			r = &graph.NodeDegreeRow{NodeID: id}
			byID[id] = r
		}
		return r
	}
	for _, r := range inRows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		ensure(id).InCount = int(asInt64(r[1]))
	}
	for _, r := range outRows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		if id == "" {
			continue
		}
		ensure(id).OutCount = int(asInt64(r[1]))
	}
	out := make([]graph.NodeDegreeRow, 0, len(byID))
	for _, r := range byID {
		out = append(out, *r)
	}
	return out
}

// prefixedNodeReturnCols projects the same node columns nodeReturnCols
// covers but rooted on a custom variable name — needed when the same
// MATCH has more than one node and the row aliases need to mirror
// rowToNode's column order.
func prefixedNodeReturnCols(prefix string) string {
	return prefix + ".id, " + prefix + ".kind, " + prefix + ".name, " +
		prefix + ".qual_name, " + prefix + ".file_path, " +
		prefix + ".start_line, " + prefix + ".end_line, " +
		prefix + ".language, " + prefix + ".repo_prefix, " +
		prefix + ".workspace_id, " + prefix + ".project_id, " +
		prefix + ".meta"
}
