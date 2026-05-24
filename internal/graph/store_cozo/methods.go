//go:build cozo


package store_cozo

import (
	"fmt"
	"iter"
	"strings"

	cozo "github.com/cozodb/cozo-lib-go"

	"github.com/zzet/gortex/internal/graph"
)

// -- writes --------------------------------------------------------------

const putNodeQ = `
?[id, kind, name, qual_name, file_path, start_line, end_line, language,
  repo_prefix, workspace_id, project_id, absolute_file_path, meta] <- $rows
:put node {
    id =>
    kind, name, qual_name, file_path, start_line, end_line, language,
    repo_prefix, workspace_id, project_id, absolute_file_path, meta
}`

const putEdgeQ = `
?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
  origin, tier, cross_repo, meta] <- $rows
:put edge {
    from_id, to_id, kind, file_path, line =>
    confidence, confidence_label, origin, tier, cross_repo, meta
}`

// AddNode inserts (or upserts) a node.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.putNodesLocked([]*graph.Node{n})
}

// AddEdge inserts an edge.
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.putEdgesLocked([]*graph.Edge{e})
}

// AddBatch inserts a batch of nodes and edges via two :put statements.
// The shadow swap routes the entire cold-load through a single
// AddBatch call, so this is the hot path on cold start.
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.putNodesLocked(nodes)
	s.putEdgesLocked(edges)
}

const cozoBatchChunkSize = 5000

func (s *Store) putNodesLocked(nodes []*graph.Node) {
	// Dedup by id (last-write-wins). Cozo's :put fails on duplicate
	// key within the same batch, so we collapse first.
	seen := make(map[string]int, len(nodes))
	deduped := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if idx, ok := seen[n.ID]; ok {
			deduped[idx] = n
			continue
		}
		seen[n.ID] = len(deduped)
		deduped = append(deduped, n)
	}
	for i := 0; i < len(deduped); i += cozoBatchChunkSize {
		end := i + cozoBatchChunkSize
		if end > len(deduped) {
			end = len(deduped)
		}
		rows := make([][]any, 0, end-i)
		for _, n := range deduped[i:end] {
			row, err := nodeToRow(n)
			if err != nil {
				panicOnFatal(err)
				return
			}
			rows = append(rows, row)
		}
		if _, err := s.db.Run(putNodeQ, cozo.Map{"rows": rows}); err != nil {
			panicOnFatal(fmt.Errorf("put nodes: %w", err))
		}
	}
}

func (s *Store) putEdgesLocked(edges []*graph.Edge) {
	type edgeKey struct {
		from, to, kind, file string
		line                 int
	}
	seen := make(map[edgeKey]int, len(edges))
	deduped := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		k := edgeKey{e.From, e.To, string(e.Kind), e.FilePath, e.Line}
		if idx, ok := seen[k]; ok {
			deduped[idx] = e
			continue
		}
		seen[k] = len(deduped)
		deduped = append(deduped, e)
	}
	for i := 0; i < len(deduped); i += cozoBatchChunkSize {
		end := i + cozoBatchChunkSize
		if end > len(deduped) {
			end = len(deduped)
		}
		rows := make([][]any, 0, end-i)
		for _, e := range deduped[i:end] {
			row, err := edgeToRow(e)
			if err != nil {
				panicOnFatal(err)
				return
			}
			rows = append(rows, row)
		}
		if _, err := s.db.Run(putEdgeQ, cozo.Map{"rows": rows}); err != nil {
			panicOnFatal(fmt.Errorf("put edges: %w", err))
		}
	}
}

func panicOnFatal(err error) {
	if err == nil {
		return
	}
	panic(fmt.Errorf("store_cozo: %w", err))
}

// SetEdgeProvenance mutates an existing edge's origin in-place.
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const sel = `
?[origin] := *edge{from_id: $from, to_id: $to, kind: $kind,
                   file_path: $file_path, line: $line, origin}`
	res, err := s.db.Run(sel, cozo.Map{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      e.Line,
	})
	if err != nil || len(res.Rows) == 0 {
		return false
	}
	storedOrigin := asString(res.Rows[0][0])
	if storedOrigin == newOrigin {
		return false
	}
	newTier := e.Tier
	if newTier != "" {
		newTier = graph.ResolvedBy(newOrigin)
	}
	const upd = `
?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
  origin, tier, cross_repo, meta] :=
    *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
          origin: _, tier: _, cross_repo, meta},
    from_id = $from, to_id = $to, kind = $kind,
    file_path = $file_path, line = $line,
    origin = $origin, tier = $tier
:put edge {from_id, to_id, kind, file_path, line =>
           confidence, confidence_label, origin, tier, cross_repo, meta}`
	if _, err := s.db.Run(upd, cozo.Map{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      e.Line,
		"origin":    newOrigin,
		"tier":      newTier,
	}); err != nil {
		return false
	}
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	return true
}

// SetEdgeProvenanceBatch is the batched form.
func (s *Store) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	if len(batch) == 0 {
		return 0
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	changed := 0
	for _, u := range batch {
		if u.Edge == nil {
			continue
		}
		if s.setEdgeProvenanceLockedUnsafe(u.Edge, u.NewOrigin) {
			changed++
		}
	}
	return changed
}

// setEdgeProvenanceLockedUnsafe is the locked-by-caller version of
// SetEdgeProvenance, called inside the SetEdgeProvenanceBatch loop.
func (s *Store) setEdgeProvenanceLockedUnsafe(e *graph.Edge, newOrigin string) bool {
	const sel = `
?[origin] := *edge{from_id: $from, to_id: $to, kind: $kind,
                   file_path: $file_path, line: $line, origin}`
	res, err := s.db.Run(sel, cozo.Map{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      e.Line,
	})
	if err != nil || len(res.Rows) == 0 {
		return false
	}
	storedOrigin := asString(res.Rows[0][0])
	if storedOrigin == newOrigin {
		return false
	}
	newTier := e.Tier
	if newTier != "" {
		newTier = graph.ResolvedBy(newOrigin)
	}
	const upd = `
?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
  origin, tier, cross_repo, meta] :=
    *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
          origin: _, tier: _, cross_repo, meta},
    from_id = $from, to_id = $to, kind = $kind,
    file_path = $file_path, line = $line,
    origin = $origin, tier = $tier
:put edge {from_id, to_id, kind, file_path, line =>
           confidence, confidence_label, origin, tier, cross_repo, meta}`
	if _, err := s.db.Run(upd, cozo.Map{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      e.Line,
		"origin":    newOrigin,
		"tier":      newTier,
	}); err != nil {
		return false
	}
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	return true
}

// ReindexEdge updates the edge's to_id (after the caller mutated e.To).
// In Cozo we need to delete the old composite key row and insert the
// new one — the to_id isn't part of the key but the row identity
// includes the (from, to, kind, file, line) tuple in our graph layer.
func (s *Store) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || oldTo == e.To {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.reindexEdgeLockedUnsafe(e, oldTo)
}

func (s *Store) reindexEdgeLockedUnsafe(e *graph.Edge, oldTo string) {
	// Delete old row (key includes to_id).
	const del = `
?[from_id, to_id, kind, file_path, line] <- [[$from, $oldTo, $kind, $file, $line]]
:rm edge {from_id, to_id, kind, file_path, line}`
	if _, err := s.db.Run(del, cozo.Map{
		"from":  e.From,
		"oldTo": oldTo,
		"kind":  string(e.Kind),
		"file":  e.FilePath,
		"line":  e.Line,
	}); err != nil {
		// Don't panic — the row may simply not be present (e.g.
		// resolver re-runs).
	}
	s.putEdgesLocked([]*graph.Edge{e})
	s.edgeIdentityRevs.Add(1)
}

// ReindexEdges is the batched form.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, r := range batch {
		if r.Edge == nil || r.OldTo == r.Edge.To {
			continue
		}
		s.reindexEdgeLockedUnsafe(r.Edge, r.OldTo)
	}
}

// RemoveEdge removes an edge by its identity tuple.
func (s *Store) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Find every row matching (from, to, kind) — file_path / line vary
	// per call so we need to enumerate first then delete each.
	const sel = `
?[file_path, line] := *edge{from_id: $from, to_id: $to, kind: $kind,
                            file_path, line}`
	res, err := s.db.Run(sel, cozo.Map{
		"from": from, "to": to, "kind": string(kind),
	})
	if err != nil || len(res.Rows) == 0 {
		return false
	}
	rowsAny := make([][]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		fp := asString(r[0])
		ln := asInt(r[1])
		rowsAny = append(rowsAny, []any{from, to, string(kind), fp, ln})
	}
	const del = `?[from_id, to_id, kind, file_path, line] <- $rows
:rm edge {from_id, to_id, kind, file_path, line}`
	if _, err := s.db.Run(del, cozo.Map{"rows": rowsAny}); err != nil {
		return false
	}
	return true
}

// EvictFile removes every node with the given file_path plus every
// edge whose endpoint is a node from that file (cascade).
func (s *Store) EvictFile(filePath string) (int, int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Collect node IDs for the file.
	const nsel = `?[id] := *node{id, file_path: $fp}`
	nres, _ := s.db.Run(nsel, cozo.Map{"fp": filePath})

	var nodesRemoved, edgesRemoved int
	ids := map[string]struct{}{}
	if nres.Ok && len(nres.Rows) > 0 {
		rows := make([][]any, 0, len(nres.Rows))
		for _, r := range nres.Rows {
			id := asString(r[0])
			ids[id] = struct{}{}
			rows = append(rows, []any{id})
		}
		const ndel = `?[id] <- $rows :rm node {id}`
		if _, err := s.db.Run(ndel, cozo.Map{"rows": rows}); err == nil {
			nodesRemoved = len(rows)
		}
	}

	// Cascade edges whose from_id OR to_id was in the file. Walk all
	// edges, filter in Go — Cozo lacks a tidy "id IN $set" predicate.
	// Acceptable: EvictFile isn't on the indexer hot path.
	const esel = `?[from_id, to_id, kind, file_path, line] :=
        *edge{from_id, to_id, kind, file_path, line}`
	eres, _ := s.db.Run(esel, cozo.Map{})
	if eres.Ok {
		toDelete := make([][]any, 0)
		for _, r := range eres.Rows {
			from := asString(r[0])
			to := asString(r[1])
			_, fromIn := ids[from]
			_, toIn := ids[to]
			if fromIn || toIn || asString(r[3]) == filePath {
				toDelete = append(toDelete, []any{
					from, to, asString(r[2]), asString(r[3]), asInt(r[4]),
				})
			}
		}
		if len(toDelete) > 0 {
			const edel = `?[from_id, to_id, kind, file_path, line] <- $rows
:rm edge {from_id, to_id, kind, file_path, line}`
			if _, err := s.db.Run(edel, cozo.Map{"rows": toDelete}); err == nil {
				edgesRemoved = len(toDelete)
			}
		}
	}
	return nodesRemoved, edgesRemoved
}

// EvictRepo removes every node + edge with the given repo_prefix.
func (s *Store) EvictRepo(repoPrefix string) (int, int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	const nsel = `?[id] := *node{id, repo_prefix: $rp}`
	nres, _ := s.db.Run(nsel, cozo.Map{"rp": repoPrefix})

	var nodesRemoved, edgesRemoved int
	if nres.Ok && len(nres.Rows) > 0 {
		// Build id set for edge cascade.
		ids := make(map[string]struct{}, len(nres.Rows))
		rows := make([][]any, 0, len(nres.Rows))
		for _, r := range nres.Rows {
			id := asString(r[0])
			ids[id] = struct{}{}
			rows = append(rows, []any{id})
		}
		const ndel = `?[id] <- $rows :rm node {id}`
		if _, err := s.db.Run(ndel, cozo.Map{"rows": rows}); err == nil {
			nodesRemoved = len(rows)
		}
		// Cascade edges where from_id or to_id is in the repo.
		const esel = `?[from_id, to_id, kind, file_path, line] := *edge{from_id, to_id, kind, file_path, line}`
		eres, _ := s.db.Run(esel, cozo.Map{})
		if eres.Ok {
			toDelete := make([][]any, 0, len(eres.Rows))
			for _, r := range eres.Rows {
				from := asString(r[0])
				to := asString(r[1])
				if _, ok := ids[from]; ok {
					toDelete = append(toDelete, []any{from, to, asString(r[2]), asString(r[3]), asInt(r[4])})
					continue
				}
				if _, ok := ids[to]; ok {
					toDelete = append(toDelete, []any{from, to, asString(r[2]), asString(r[3]), asInt(r[4])})
				}
			}
			if len(toDelete) > 0 {
				const edel = `?[from_id, to_id, kind, file_path, line] <- $rows
:rm edge {from_id, to_id, kind, file_path, line}`
				if _, err := s.db.Run(edel, cozo.Map{"rows": toDelete}); err == nil {
					edgesRemoved = len(toDelete)
				}
			}
		}
	}
	return nodesRemoved, edgesRemoved
}

// -- reads ---------------------------------------------------------------

const nodeReturnCols = `id, kind, name, qual_name, file_path, start_line, end_line, language, repo_prefix, workspace_id, project_id, absolute_file_path, meta`

const edgeReturnCols = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta`

func (s *Store) GetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        id = $id`
	res, err := s.db.Run(q, cozo.Map{"id": id})
	if err != nil || len(res.Rows) == 0 {
		return nil
	}
	return rowToNode(res.Rows[0])
}

func (s *Store) GetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        qual_name = $q`
	res, err := s.db.Run(q, cozo.Map{"q": qualName})
	if err != nil || len(res.Rows) == 0 {
		return nil
	}
	return rowToNode(res.Rows[0])
}

func (s *Store) FindNodesByName(name string) []*graph.Node {
	if name == "" {
		return nil
	}
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        name = $n`
	res, _ := s.db.Run(q, cozo.Map{"n": name})
	out := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	if name == "" {
		return nil
	}
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        name = $n, repo_prefix = $r`
	res, _ := s.db.Run(q, cozo.Map{"n": name, "r": repoPrefix})
	out := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) GetFileNodes(filePath string) []*graph.Node {
	if filePath == "" {
		return nil
	}
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        file_path = $fp`
	res, _ := s.db.Run(q, cozo.Map{"fp": filePath})
	out := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) GetRepoNodes(repoPrefix string) []*graph.Node {
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        repo_prefix = $r`
	res, _ := s.db.Run(q, cozo.Map{"r": repoPrefix})
	out := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	const q = `?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
                  origin, tier, cross_repo, meta] :=
        *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
              origin, tier, cross_repo, meta},
        from_id = $id`
	res, _ := s.db.Run(q, cozo.Map{"id": nodeID})
	out := make([]*graph.Edge, 0, len(res.Rows))
	for _, r := range res.Rows {
		if e := rowToEdge(r); e != nil {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	if nodeID == "" {
		return nil
	}
	const q = `?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
                  origin, tier, cross_repo, meta] :=
        *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
              origin, tier, cross_repo, meta},
        to_id = $id`
	res, _ := s.db.Run(q, cozo.Map{"id": nodeID})
	out := make([]*graph.Edge, 0, len(res.Rows))
	for _, r := range res.Rows {
		if e := rowToEdge(r); e != nil {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) AllNodes() []*graph.Node {
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta}`
	res, _ := s.db.Run(q, cozo.Map{})
	out := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) AllEdges() []*graph.Edge {
	const q = `?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
                  origin, tier, cross_repo, meta] :=
        *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
              origin, tier, cross_repo, meta}`
	res, _ := s.db.Run(q, cozo.Map{})
	out := make([]*graph.Edge, 0, len(res.Rows))
	for _, r := range res.Rows {
		if e := rowToEdge(r); e != nil {
			out = append(out, e)
		}
	}
	return out
}

// -- predicate-shaped reads ---------------------------------------------

func (s *Store) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	const q = `?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
                  origin, tier, cross_repo, meta] :=
        *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
              origin, tier, cross_repo, meta},
        kind = $k`
	res, _ := s.db.Run(q, cozo.Map{"k": string(kind)})
	edges := make([]*graph.Edge, 0, len(res.Rows))
	for _, r := range res.Rows {
		if e := rowToEdge(r); e != nil {
			edges = append(edges, e)
		}
	}
	return func(yield func(*graph.Edge) bool) {
		for _, e := range edges {
			if !yield(e) {
				return
			}
		}
	}
}

func (s *Store) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	const q = `?[id, kind, name, qual_name, file_path, start_line, end_line, language,
                  repo_prefix, workspace_id, project_id, absolute_file_path, meta] :=
        *node{id, kind, name, qual_name, file_path, start_line, end_line, language,
              repo_prefix, workspace_id, project_id, absolute_file_path, meta},
        kind = $k`
	res, _ := s.db.Run(q, cozo.Map{"k": string(kind)})
	nodes := make([]*graph.Node, 0, len(res.Rows))
	for _, r := range res.Rows {
		if n := rowToNode(r); n != nil {
			nodes = append(nodes, n)
		}
	}
	return func(yield func(*graph.Node) bool) {
		for _, n := range nodes {
			if !yield(n) {
				return
			}
		}
	}
}

func (s *Store) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	const q = `?[from_id, to_id, kind, file_path, line, confidence, confidence_label,
                  origin, tier, cross_repo, meta] :=
        *edge{from_id, to_id, kind, file_path, line, confidence, confidence_label,
              origin, tier, cross_repo, meta},
        starts_with(to_id, 'unresolved::')`
	res, _ := s.db.Run(q, cozo.Map{})
	edges := make([]*graph.Edge, 0, len(res.Rows))
	for _, r := range res.Rows {
		if e := rowToEdge(r); e != nil {
			edges = append(edges, e)
		}
	}
	return func(yield func(*graph.Edge) bool) {
		for _, e := range edges {
			if !yield(e) {
				return
			}
		}
	}
}

// -- batched point lookups ----------------------------------------------

func (s *Store) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if len(ids) == 0 {
		return nil
	}
	// Per-id loop. The Datalog "inline relation from parameter" form
	// isn't documented for Cozo's bindings layer, and the shadow path
	// routes the cold-load through AddBatch, so the batched-read hot
	// path on graph-DB backends only matters for the resolver — which
	// runs against the in-memory shadow, not Cozo, on every workload
	// below shadowMaxFileCount.
	uniq := map[string]struct{}{}
	for _, id := range ids {
		if id != "" {
			uniq[id] = struct{}{}
		}
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]*graph.Node, len(uniq))
	for id := range uniq {
		if n := s.GetNode(id); n != nil {
			out[id] = n
		}
	}
	return out
}

func (s *Store) FindNodesByNames(names []string) map[string][]*graph.Node {
	if len(names) == 0 {
		return nil
	}
	uniq := map[string]struct{}{}
	for _, n := range names {
		if n != "" {
			uniq[n] = struct{}{}
		}
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Node, len(uniq))
	for name := range uniq {
		if hits := s.FindNodesByName(name); len(hits) > 0 {
			out[name] = hits
		}
	}
	return out
}

// -- counts + stats -----------------------------------------------------

func (s *Store) NodeCount() int {
	const q = `?[count(id)] := *node{id}`
	res, _ := s.db.Run(q, cozo.Map{})
	if len(res.Rows) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

func (s *Store) EdgeCount() int {
	const q = `?[count(from_id)] := *edge{from_id}`
	res, _ := s.db.Run(q, cozo.Map{})
	if len(res.Rows) == 0 {
		return 0
	}
	return asInt(res.Rows[0][0])
}

func (s *Store) Stats() graph.GraphStats {
	st := graph.GraphStats{
		TotalNodes: s.NodeCount(),
		TotalEdges: s.EdgeCount(),
		ByKind:     map[string]int{},
		ByLanguage: map[string]int{},
	}
	const kq = `?[kind, count(id)] := *node{id, kind}`
	if r, err := s.db.Run(kq, cozo.Map{}); err == nil {
		for _, row := range r.Rows {
			st.ByKind[asString(row[0])] = asInt(row[1])
		}
	}
	const lq = `?[language, count(id)] := *node{id, language}`
	if r, err := s.db.Run(lq, cozo.Map{}); err == nil {
		for _, row := range r.Rows {
			lang := asString(row[0])
			if lang != "" {
				st.ByLanguage[lang] = asInt(row[1])
			}
		}
	}
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := make(map[string]graph.GraphStats)
	const nq = `?[repo_prefix, count(id)] := *node{id, repo_prefix}`
	if r, err := s.db.Run(nq, cozo.Map{}); err == nil {
		for _, row := range r.Rows {
			rp := asString(row[0])
			st := out[rp]
			st.TotalNodes = asInt(row[1])
			out[rp] = st
		}
	}
	// Edges don't have repo_prefix; attribute by from_id's repo via join.
	const eq = `?[repo_prefix, count(line)] :=
        *edge{from_id, line}, *node{id: from_id, repo_prefix}`
	if r, err := s.db.Run(eq, cozo.Map{}); err == nil {
		for _, row := range r.Rows {
			rp := asString(row[0])
			st := out[rp]
			st.TotalEdges = asInt(row[1])
			out[rp] = st
		}
	}
	return out
}

func (s *Store) RepoPrefixes() []string {
	const q = `?[repo_prefix] := *node{repo_prefix}`
	res, _ := s.db.Run(q, cozo.Map{})
	set := map[string]struct{}{}
	for _, r := range res.Rows {
		set[asString(r[0])] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// -- provenance ----------------------------------------------------------

func (s *Store) EdgeIdentityRevisions() int { return int(s.edgeIdentityRevs.Load()) }

func (s *Store) VerifyEdgeIdentities() error {
	// Trivially satisfied: the schema's composite key enforces uniqueness.
	return nil
}

// -- memory estimation --------------------------------------------------

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	// Memory estimates are inherently in-memory-specific (per the
	// Store interface doc); for disk backends we report NodeCount /
	// EdgeCount as advisory and leave byte sizes at zero.
	est := graph.RepoMemoryEstimate{}
	const nq = `?[count(id)] := *node{id, repo_prefix}, repo_prefix = $r`
	if r, err := s.db.Run(nq, cozo.Map{"r": repoPrefix}); err == nil && len(r.Rows) > 0 {
		est.NodeCount = asInt(r.Rows[0][0])
	}
	const eq = `?[count(line)] := *edge{from_id, line}, *node{id: from_id, repo_prefix}, repo_prefix = $r`
	if r, err := s.db.Run(eq, cozo.Map{"r": repoPrefix}); err == nil && len(r.Rows) > 0 {
		est.EdgeCount = asInt(r.Rows[0][0])
	}
	return est
}

func (s *Store) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := make(map[string]graph.RepoMemoryEstimate)
	for _, rp := range s.RepoPrefixes() {
		out[rp] = s.RepoMemoryEstimate(rp)
	}
	return out
}

// quiet unused-import warning when methods are stubbed out
var _ = strings.Builder{}
