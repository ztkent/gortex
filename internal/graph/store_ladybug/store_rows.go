package store_ladybug

import "github.com/zzet/gortex/internal/graph"

// nodeReturnCols is the canonical projection for Node rows, ordered
// to match rowToNode's index reads.
const nodeReturnCols = `n.id, n.kind, n.name, n.qual_name, n.file_path, n.start_line, n.end_line, n.language, n.repo_prefix, n.workspace_id, n.project_id, n.meta`

// edgeReturnCols is the canonical projection for Edge rows, ordered
// to match rowToEdge's index reads.
const edgeReturnCols = `a.id, b.id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo, e.meta`

// frontierEdgeCols is edgeReturnCols without e.meta — bfs / get_callers /
// get_callchain never read Edge.Meta, and gob-decoding it per row is what
// makes a wide fan-out expensive. Index order matches frontierHopFromRow.
const frontierEdgeCols = `a.id, b.id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo`

func rowToNode(row []any) *graph.Node {
	if len(row) < 12 {
		return nil
	}
	n := &graph.Node{}
	n.ID, _ = row[0].(string)
	kind, _ := row[1].(string)
	n.Kind = graph.NodeKind(kind)
	n.Name, _ = row[2].(string)
	n.QualName, _ = row[3].(string)
	n.FilePath, _ = row[4].(string)
	n.StartLine = int(asInt64(row[5]))
	n.EndLine = int(asInt64(row[6]))
	n.Language, _ = row[7].(string)
	n.RepoPrefix, _ = row[8].(string)
	n.WorkspaceID, _ = row[9].(string)
	n.ProjectID, _ = row[10].(string)
	metaStr, _ := row[11].(string)
	if metaStr != "" {
		m, err := decodeMeta(metaStr)
		if err == nil {
			n.Meta = m
		}
	}
	return n
}

func rowsToNodes(rows [][]any) []*graph.Node {
	out := make([]*graph.Node, 0, len(rows))
	for _, r := range rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func rowToEdge(row []any) *graph.Edge {
	if len(row) < 11 {
		return nil
	}
	e := &graph.Edge{}
	e.From, _ = row[0].(string)
	e.To, _ = row[1].(string)
	kind, _ := row[2].(string)
	e.Kind = graph.EdgeKind(kind)
	e.FilePath, _ = row[3].(string)
	e.Line = int(asInt64(row[4]))
	if v, ok := row[5].(float64); ok {
		e.Confidence = v
	}
	e.ConfidenceLabel, _ = row[6].(string)
	e.Origin, _ = row[7].(string)
	e.Tier, _ = row[8].(string)
	e.CrossRepo = asInt64(row[9]) != 0
	metaStr, _ := row[10].(string)
	if metaStr != "" {
		m, err := decodeMeta(metaStr)
		if err == nil {
			e.Meta = m
		}
	}
	return e
}

func rowsToEdges(rows [][]any) []*graph.Edge {
	out := make([]*graph.Edge, 0, len(rows))
	for _, r := range rows {
		if e := rowToEdge(r); e != nil {
			out = append(out, e)
		}
	}
	return out
}

// frontierHopFromRow decodes one ExpandFrontier row: cols 0..9 are the
// edge (frontierEdgeCols, no meta), cols 10..19 the neighbour node's
// columns (kind, name, qual_name, file_path, start_line, end_line,
// language, repo_prefix, workspace_id, project_id — no meta). The
// neighbour id is the far end of the stored edge: To for an outgoing
// (forward) hop, From for incoming.
func frontierHopFromRow(row []any, forward bool) (graph.FrontierHop, bool) {
	if len(row) < 20 {
		return graph.FrontierHop{}, false
	}
	e := &graph.Edge{}
	e.From, _ = row[0].(string)
	e.To, _ = row[1].(string)
	kind, _ := row[2].(string)
	e.Kind = graph.EdgeKind(kind)
	e.FilePath, _ = row[3].(string)
	e.Line = int(asInt64(row[4]))
	if v, ok := row[5].(float64); ok {
		e.Confidence = v
	}
	e.ConfidenceLabel, _ = row[6].(string)
	e.Origin, _ = row[7].(string)
	e.Tier, _ = row[8].(string)
	e.CrossRepo = asInt64(row[9]) != 0

	n := &graph.Node{}
	if forward {
		n.ID = e.To
	} else {
		n.ID = e.From
	}
	knd, _ := row[10].(string)
	n.Kind = graph.NodeKind(knd)
	n.Name, _ = row[11].(string)
	n.QualName, _ = row[12].(string)
	n.FilePath, _ = row[13].(string)
	n.StartLine = int(asInt64(row[14]))
	n.EndLine = int(asInt64(row[15]))
	n.Language, _ = row[16].(string)
	n.RepoPrefix, _ = row[17].(string)
	n.WorkspaceID, _ = row[18].(string)
	n.ProjectID, _ = row[19].(string)
	return graph.FrontierHop{Edge: e, Neighbor: n}, true
}

// asInt64 normalises every integer-shaped value the KuzuDB binding
// might hand back (int8, int16, int32, int64, plus their unsigned
// counterparts and the plain `int`). The rel/node columns we read
// were all declared as INT64 in schema.go, but the binding
// occasionally returns smaller widths for results coming out of
// count() aggregates so we cover the full set.
func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case int16:
		return int64(t)
	case int8:
		return int64(t)
	case int:
		return int64(t)
	case uint64:
		return int64(t)
	case uint32:
		return int64(t)
	case uint16:
		return int64(t)
	case uint8:
		return int64(t)
	case uint:
		return int64(t)
	case float64:
		return int64(t)
	default:
		return 0
	}
}

func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// stringSliceToAny converts a typed string slice into the []any form
// the KuzuDB Go binding expects when binding a Cypher list
// parameter (the binding cannot infer a list type from a strongly
// typed slice — it walks each element through goValueToKuzuValue).
func stringSliceToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
