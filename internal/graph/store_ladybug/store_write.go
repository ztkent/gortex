package store_ladybug

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// AddNode inserts (or upserts) a node. Idempotent on the id PK — a
// second AddNode for the same id is a no-op except for any column
// updates the new value carries, matching the in-memory store's
// "last write wins" behaviour.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	// Bulk-load fast path: if a drain has called BeginBulkLoad, route
	// this write into the bulk buffer instead of taking writeMu and
	// running an UNWIND-MERGE. Otherwise contracts / clones / DI
	// emission paths (commitInlinedContractToGraph and friends) that
	// call AddNode directly during the bulk window would slip a live
	// Node row in past the bulk's view, the bulk's subsequent COPY
	// Node would re-insert the same ID, and Kuzu's COPY rejects the
	// duplicate primary key — torpedoing the entire repo's index.
	// AddBatch already uses this routing; AddNode/AddEdge needed to
	// match.
	s.bulkMu.Lock()
	if s.bulkActive {
		s.bulkNodes = append(s.bulkNodes, n)
		s.bulkMu.Unlock()
		return
	}
	s.bulkMu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.upsertNodeLocked(n)
	s.writeGen.Add(1)
}

func (s *Store) upsertNodeLocked(n *graph.Node) {
	metaStr, err := encodeMeta(n.Meta)
	if err != nil {
		panicOnFatal(fmt.Errorf("encode meta: %w", err))
		return
	}
	if s.fileIDs != nil {
		s.fileIDs.add(n.FilePath, n.ID)
	}
	if s.nameIdx != nil {
		s.nameIdx.addNode(n)
	}
	// MERGE on id, then SET every column. This is the upsert pattern
	// for KuzuDB — a bare CREATE on a duplicate PK raises a
	// uniqueness violation; MERGE matches-or-creates without error.
	const q = `
MERGE (n:Node {id: $id})
SET n.kind = $kind,
    n.name = $name,
    n.qual_name = $qual_name,
    n.file_path = $file_path,
    n.start_line = $start_line,
    n.end_line = $end_line,
    n.language = $language,
    n.repo_prefix = $repo_prefix,
    n.workspace_id = $workspace_id,
    n.project_id = $project_id,
    n.meta = $meta`
	args := map[string]any{
		"id":           n.ID,
		"kind":         string(n.Kind),
		"name":         n.Name,
		"qual_name":    n.QualName,
		"file_path":    n.FilePath,
		"start_line":   int64(n.StartLine),
		"end_line":     int64(n.EndLine),
		"language":     n.Language,
		"repo_prefix":  n.RepoPrefix,
		"workspace_id": n.WorkspaceID,
		"project_id":   n.ProjectID,
		"meta":         metaStr,
	}
	s.runWriteLocked(q, args)
}

// AddEdge inserts an edge. Idempotent on the (from, to, kind,
// file_path, line) tuple via MERGE.
func (s *Store) AddEdge(e *graph.Edge) {
	if e == nil {
		return
	}
	// Bulk-load fast path: mirror AddNode — during a drain's
	// BeginBulkLoad / FlushBulk window, contract / clones / DI emission
	// code calls AddEdge directly. Letting those slip through as a live
	// MERGE while the bulk buffer still holds a duplicate of the same
	// edge would re-trigger the COPY-Edge "duplicate primary key" /
	// "unable to find primary key" classes the AddNode fix addresses.
	s.bulkMu.Lock()
	if s.bulkActive {
		s.bulkEdges = append(s.bulkEdges, e)
		s.bulkMu.Unlock()
		return
	}
	s.bulkMu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.upsertEdgeLocked(e)
	s.writeGen.Add(1)
}

func (s *Store) upsertEdgeLocked(e *graph.Edge) {
	metaStr, err := encodeMeta(e.Meta)
	if err != nil {
		panicOnFatal(fmt.Errorf("encode edge meta: %w", err))
		return
	}
	var crossRepo int64
	if e.CrossRepo {
		crossRepo = 1
	}
	// The in-memory store happily inserts edges whose endpoints
	// haven't been registered with AddNode yet (the resolver writes
	// edges to "unresolved::*" stubs that never have a corresponding
	// node, and AllEdges is expected to surface them so the resolver
	// can iterate them). KuzuDB's rel tables require both endpoints
	// to exist in the node table, so we MERGE-stub the endpoints
	// first; the MERGE is a no-op for ids the caller has already
	// registered via AddNode. The stub nodes carry empty
	// kind/name/file_path; if the caller later AddNode's them with
	// real metadata, that upsert overwrites the columns in place.
	s.mergeStubNodeLocked(e.From)
	s.mergeStubNodeLocked(e.To)
	// MERGE the rel on the identity tuple (from, to, kind, file_path,
	// line). Idempotent — a second AddEdge with the same tuple
	// updates the per-edge columns (confidence / origin / tier /
	// meta) in place without creating a duplicate row.
	const q = `
MATCH (a:Node {id: $from}), (b:Node {id: $to})
MERGE (a)-[e:Edge {kind: $kind, file_path: $file_path, line: $line}]->(b)
SET e.confidence = $confidence,
    e.confidence_label = $confidence_label,
    e.origin = $origin,
    e.tier = $tier,
    e.cross_repo = $cross_repo,
    e.meta = $meta`
	args := map[string]any{
		"from":             e.From,
		"to":               e.To,
		"kind":             string(e.Kind),
		"file_path":        e.FilePath,
		"line":             int64(e.Line),
		"confidence":       e.Confidence,
		"confidence_label": e.ConfidenceLabel,
		"origin":           e.Origin,
		"tier":             e.Tier,
		"cross_repo":       crossRepo,
		"meta":             metaStr,
	}
	s.runWriteLocked(q, args)
}

// mergeStubNodeLocked ensures a Node row exists for id without
// overwriting any columns the caller may have set via a previous
// AddNode. We use MERGE … ON CREATE SET so an existing fully-
// populated node keeps its kind / name / file_path / etc., and a
// brand-new stub gets blank defaults the columns the schema
// initialises.
func (s *Store) mergeStubNodeLocked(id string) {
	if id == "" {
		return
	}
	const q = `
MERGE (n:Node {id: $id})
ON CREATE SET n.kind = '',
              n.name = '',
              n.qual_name = '',
              n.file_path = '',
              n.start_line = 0,
              n.end_line = 0,
              n.language = '',
              n.repo_prefix = '',
              n.workspace_id = '',
              n.project_id = '',
              n.meta = ''`
	s.runWriteLocked(q, map[string]any{"id": id})
}

// AddBatch inserts a batch of nodes and edges. KuzuDB does not expose
// an explicit transaction API through the Go binding, and the
// conformance suite only verifies the post-batch counts — looping
// the per-call mutators is the safe path that satisfies the
// contract. Indexing scale will favour a UNWIND-driven batched
// MERGE once we wire the bench harness up; the per-loop variant
// keeps the conformance suite passing today.
// kuzuBatchChunkSize bounds the row count per UNWIND-driven
// Cypher statement. The Go binding round-trip is ~ms; per-record
// loops at indexer scale (124k+ nodes, 524k+ edges) take tens of
// minutes. UNWIND lets one statement carry a list of rows, so a
// 5000-row chunk amortises one Cypher parse + plan + Execute
// across N MERGEs.
const kuzuBatchChunkSize = 5000

// AddBatch fans node and edge inserts into UNWIND-driven Cypher
// statements — one Execute per ≤kuzuBatchChunkSize rows instead of
// one per record. The MERGE semantics match upsertNodeLocked /
// upsertEdgeLocked exactly so the conformance idempotency contract
// is preserved.
func (s *Store) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	// Bulk-load fast path: buffer in memory, defer Cypher to FlushBulk.
	// The buffer lock is held briefly only across the slice append —
	// the indexer's parse workers can hammer AddBatch in parallel with
	// minimal contention.
	s.bulkMu.Lock()
	if s.bulkActive {
		s.bulkNodes = append(s.bulkNodes, nodes...)
		s.bulkEdges = append(s.bulkEdges, edges...)
		s.bulkMu.Unlock()
		return
	}
	s.bulkMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Nodes use the UNWIND-MERGE batching path — safe because nodes
	// carry no FK references, so the "unordered_map::at: key not
	// found" crash that bites edge UNWIND can't fire here. Batching
	// turns N upserts into ceil(N/chunk) Cypher calls — meaningful on
	// Ladybug where each cgo round-trip costs ~1 ms.
	if len(nodes) > 0 {
		s.addNodesUnwindLocked(nodes)
	}
	// Edges stay on the per-call upsertEdgeLocked path: it stubs the
	// endpoints with explicit MERGE before MERGEing the edge, which
	// dodges the C++ panic the fork raises when UNWIND-MERGE sees an
	// edge row whose endpoint id isn't yet in the node table.
	for _, e := range edges {
		if e == nil {
			continue
		}
		s.upsertEdgeLocked(e)
	}
	s.writeGen.Add(1)
}

// addNodesUnwindLocked materialises nodes as a list of structs and
// runs them through one UNWIND + MERGE per chunk.
func (s *Store) addNodesUnwindLocked(nodes []*graph.Node) {
	if s.fileIDs != nil {
		s.fileIDs.addNodes(nodes)
	}
	if s.nameIdx != nil {
		s.nameIdx.addNodes(nodes)
	}
	for i := 0; i < len(nodes); i += kuzuBatchChunkSize {
		end := i + kuzuBatchChunkSize
		if end > len(nodes) {
			end = len(nodes)
		}
		chunk := nodes[i:end]
		rows := make([]map[string]any, 0, len(chunk))
		for _, n := range chunk {
			if n == nil || n.ID == "" {
				continue
			}
			metaStr, err := encodeMeta(n.Meta)
			if err != nil {
				panicOnFatal(fmt.Errorf("encode meta: %w", err))
				return
			}
			rows = append(rows, map[string]any{
				"id":           n.ID,
				"kind":         string(n.Kind),
				"name":         n.Name,
				"qual_name":    n.QualName,
				"file_path":    n.FilePath,
				"start_line":   int64(n.StartLine),
				"end_line":     int64(n.EndLine),
				"language":     n.Language,
				"repo_prefix":  n.RepoPrefix,
				"workspace_id": n.WorkspaceID,
				"project_id":   n.ProjectID,
				"meta":         metaStr,
			})
		}
		if len(rows) == 0 {
			continue
		}
		const q = `
UNWIND $rows AS row
MERGE (n:Node {id: row.id})
SET n.kind = row.kind,
    n.name = row.name,
    n.qual_name = row.qual_name,
    n.file_path = row.file_path,
    n.start_line = row.start_line,
    n.end_line = row.end_line,
    n.language = row.language,
    n.repo_prefix = row.repo_prefix,
    n.workspace_id = row.workspace_id,
    n.project_id = row.project_id,
    n.meta = row.meta`
		s.runWriteLocked(q, map[string]any{"rows": rows})
	}
}

// SetEdgeProvenance mutates an existing edge's origin in-place and
// bumps the identity-revision counter when the origin actually
// changes. Returns true iff a change was applied.
func (s *Store) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if e == nil {
		return false
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.setEdgeProvenanceLocked(e, newOrigin)
}

func (s *Store) setEdgeProvenanceLocked(e *graph.Edge, newOrigin string) bool {
	// Look up the currently stored origin so we can skip the update
	// when the value is already at the target tier (the caller-
	// supplied *Edge may be a detached copy whose Origin already
	// matches even though the row still has the old value).
	const sel = `
MATCH (a:Node {id: $from})-[e:Edge {kind: $kind, file_path: $file_path, line: $line}]->(b:Node {id: $to})
RETURN e.origin LIMIT 1`
	selArgs := map[string]any{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      int64(e.Line),
	}
	rows := s.querySelectLocked(sel, selArgs)
	if len(rows) == 0 {
		return false
	}
	storedOrigin, _ := rows[0][0].(string)
	if storedOrigin == newOrigin {
		return false
	}
	newTier := e.Tier
	if newTier != "" {
		newTier = graph.ResolvedBy(newOrigin)
	}
	const upd = `
MATCH (a:Node {id: $from})-[e:Edge {kind: $kind, file_path: $file_path, line: $line}]->(b:Node {id: $to})
SET e.origin = $origin, e.tier = $tier`
	updArgs := map[string]any{
		"from":      e.From,
		"to":        e.To,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      int64(e.Line),
		"origin":    newOrigin,
		"tier":      newTier,
	}
	s.runWriteLocked(upd, updArgs)
	e.Origin = newOrigin
	if e.Tier != "" {
		e.Tier = newTier
	}
	s.edgeIdentityRevs.Add(1)
	s.writeGen.Add(1)
	return true
}

// SetEdgeProvenanceBatch UNWIND-batches origin promotions. Each
// chunk does one Cypher MATCH-WHERE-SET with a list of (key, new
// origin) rows; the WHERE clause filters down to edges whose
// stored origin actually differs, and the RETURN count gives us
// the changed-row total to bump the revision counter.
func (s *Store) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	if len(batch) == 0 {
		return 0
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	totalChanged := 0
	for i := 0; i < len(batch); i += kuzuBatchChunkSize {
		end := i + kuzuBatchChunkSize
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[i:end]
		rows := make([]map[string]any, 0, len(chunk))
		// Maintain a side-index from row position → caller's *Edge so
		// we can mirror the in-memory contract (the caller's pointer's
		// Origin/Tier field is updated when the row actually changed).
		callerEdges := make([]*graph.Edge, 0, len(chunk))
		for _, u := range chunk {
			if u.Edge == nil {
				continue
			}
			newTier := u.Edge.Tier
			if newTier != "" {
				newTier = graph.ResolvedBy(u.NewOrigin)
			}
			rows = append(rows, map[string]any{
				"from":      u.Edge.From,
				"to":        u.Edge.To,
				"kind":      string(u.Edge.Kind),
				"file_path": u.Edge.FilePath,
				"line":      int64(u.Edge.Line),
				"origin":    u.NewOrigin,
				"tier":      newTier,
			})
			callerEdges = append(callerEdges, u.Edge)
		}
		if len(rows) == 0 {
			continue
		}
		const q = `
UNWIND $rows AS row
MATCH (a:Node {id: row.from})-[e:Edge {kind: row.kind, file_path: row.file_path, line: row.line}]->(b:Node {id: row.to})
WHERE e.origin <> row.origin
SET e.origin = row.origin, e.tier = row.tier
RETURN row.from, row.to, row.kind, row.file_path, row.line, row.origin, row.tier`
		res := s.querySelectLocked(q, map[string]any{"rows": rows})
		// The SELECT-style result lists every edge the SET actually
		// touched (the WHERE filter dropped rows whose origin already
		// matched). Mirror the per-call SetEdgeProvenance contract by
		// updating the caller's Edge pointer in-place for those rows.
		changed := len(res)
		// Build a (from|to|kind|file|line) → *Edge map so we can map
		// returned rows back to caller-supplied pointers without
		// quadratic scanning.
		idx := make(map[string]*graph.Edge, len(callerEdges))
		for _, e := range callerEdges {
			idx[provKey(e)] = e
		}
		for _, row := range res {
			from, _ := row[0].(string)
			to, _ := row[1].(string)
			kind, _ := row[2].(string)
			file, _ := row[3].(string)
			line, _ := row[4].(int64)
			origin, _ := row[5].(string)
			tier, _ := row[6].(string)
			key := from + "\x00" + to + "\x00" + kind + "\x00" + file + "\x00" + strconvI64(line)
			if e := idx[key]; e != nil {
				e.Origin = origin
				if e.Tier != "" {
					e.Tier = tier
				}
			}
		}
		totalChanged += changed
		if changed > 0 {
			s.edgeIdentityRevs.Add(int64(changed))
			s.writeGen.Add(1)
		}
	}
	return totalChanged
}

// provKey builds the (from, to, kind, file, line) identity string
// used to map Cypher RETURN rows back to caller Edge pointers
// inside SetEdgeProvenanceBatch.
func provKey(e *graph.Edge) string {
	return e.From + "\x00" + e.To + "\x00" + string(e.Kind) + "\x00" + e.FilePath + "\x00" + strconvI64(int64(e.Line))
}

func strconvI64(v int64) string {
	return fmt.Sprintf("%d", v)
}

// ReindexEdge updates the stored row after e.To has been mutated
// from oldTo to e.To. Implemented as delete-old + insert-new under
// the same write lock. A no-op when oldTo == e.To.
func (s *Store) ReindexEdge(e *graph.Edge, oldTo string) {
	if e == nil || oldTo == e.To {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.reindexEdgeLocked(e, oldTo)
	s.writeGen.Add(1)
}

func (s *Store) reindexEdgeLocked(e *graph.Edge, oldTo string) {
	const del = `
MATCH (a:Node {id: $from})-[e:Edge {kind: $kind, file_path: $file_path, line: $line}]->(b:Node {id: $oldTo})
DELETE e`
	s.runWriteLocked(del, map[string]any{
		"from":      e.From,
		"oldTo":     oldTo,
		"kind":      string(e.Kind),
		"file_path": e.FilePath,
		"line":      int64(e.Line),
	})
	s.upsertEdgeLocked(e)
}

// reindexBulkThreshold is the batch size at or above which ReindexEdges
// routes through the file-driven bulk path (reindexEdgesBulk) instead of
// the per-edge DELETE+upsert loop. An incremental single-file re-resolve
// touches a handful of edges, where the per-edge loop is cheaper than
// staging temp files; a cold-start global resolve rewrites tens of
// thousands at once, where the per-edge loop serializes ~2 prepared Cypher
// statements per edge through writeMu — the multi-minute cold-warmup tail
// this threshold exists to cut.
const reindexBulkThreshold = 256

// ReindexEdges applies a resolver reindex batch: for each entry, delete
// the stale edge (the row still pointing at OldTo) and upsert the rewritten
// edge (Edge.To now resolved). Large batches go through reindexEdgesBulk
// (three file-driven LOAD-FROM statements); small batches use the per-edge
// loop. Both produce the same graph — see reindexEdgesBulk for why the
// per-edge form can't simply be UNWIND-batched.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	changed := make([]graph.EdgeReindex, 0, len(batch))
	for _, r := range batch {
		if r.Edge == nil || r.OldTo == r.Edge.To {
			continue
		}
		changed = append(changed, r)
	}
	if len(changed) == 0 {
		return
	}
	// Bulk path for large batches; on any failure it returns false and we
	// fall through to the per-edge loop, so a resolver pass never silently
	// drops resolutions.
	if len(changed) >= reindexBulkThreshold && s.reindexEdgesBulk(changed) {
		return
	}
	// Per-call ReindexEdge loop instead of the Kuzu-style UNWIND
	// double-pass. Ladybug's UNWIND-MATCH-DELETE-then-UNWIND-MERGE
	// pattern triggers the same "unordered_map::at: key not found"
	// C++ panic as AddBatch's UNWIND-MERGE. The per-call form's
	// explicit DELETE/MATCH/MERGE sequence sidesteps the engine bug.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, r := range changed {
		s.reindexEdgeLocked(r.Edge, r.OldTo)
	}
	s.writeGen.Add(1)
}

// RemoveEdge deletes every edge between (from, to) with the given
// kind. Returns true iff at least one row was deleted.
func (s *Store) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Count first so we can return the existence boolean — KuzuDB's
	// DELETE statement does not return an affected-rows count
	// through the Go binding.
	const cnt = `
MATCH (a:Node {id: $from})-[e:Edge {kind: $kind}]->(b:Node {id: $to})
RETURN count(e)`
	rows := s.querySelectLocked(cnt, map[string]any{
		"from": from,
		"to":   to,
		"kind": string(kind),
	})
	if len(rows) == 0 {
		return false
	}
	n, _ := rows[0][0].(int64)
	if n == 0 {
		return false
	}
	const del = `
MATCH (a:Node {id: $from})-[e:Edge {kind: $kind}]->(b:Node {id: $to})
DELETE e`
	s.runWriteLocked(del, map[string]any{
		"from": from,
		"to":   to,
		"kind": string(kind),
	})
	s.writeGen.Add(1)
	return true
}

// EvictFile removes every node anchored to filePath and every edge
// that touches one of those nodes. DETACH DELETE handles the edge
// cleanup as part of the node delete, so a single Cypher statement
// is enough.
func (s *Store) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, e := s.evictByScopeLocked("file_path", filePath)
	if s.fileIDs != nil {
		s.fileIDs.removeFile(filePath)
	}
	return n, e
}

// EvictRepo removes every node in repoPrefix and every edge that
// touches one.
func (s *Store) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Collect the file paths that will be evicted BEFORE the DELETE,
	// so we can drop their entries from the fileIDs accelerator
	// without scanning the whole map ourselves. evictByScopeLocked's
	// DETACH DELETE wipes the rows, after which the file_path column
	// is no longer queryable.
	var affectedPaths []string
	if s.fileIDs != nil {
		const pathsQ = `MATCH (n:Node) WHERE n.repo_prefix = $r AND n.file_path <> '' RETURN DISTINCT n.file_path`
		rows := s.querySelectLocked(pathsQ, map[string]any{"r": repoPrefix})
		affectedPaths = make([]string, 0, len(rows))
		for _, r := range rows {
			if len(r) == 0 {
				continue
			}
			if p, ok := r[0].(string); ok && p != "" {
				affectedPaths = append(affectedPaths, p)
			}
		}
	}
	n, e := s.evictByScopeLocked("repo_prefix", repoPrefix)
	// ALSO evict nodes whose ID is in this repo's namespace (`<prefix>/…`)
	// but whose repo_prefix column is empty. Edge-endpoint stubs created
	// by mergeStubNodeLocked (cross-repo resolution, the global resolve
	// pass) are written with repo_prefix='' even when their ID is
	// `<prefix>/unresolved::Name` — so the repo_prefix-scoped delete above
	// misses them. They then collide on the INSERT-only bulk COPY when
	// this repo is re-tracked (warm-restart reconcile), failing the COPY
	// with "duplicated primary key" and — because the repo's real rows
	// were already evicted — dropping the whole repo from the graph. The
	// trailing slash keeps `gortex/` from matching `gortex-cloud/…`.
	// Skipped for the single-repo (empty-prefix) store, where every ID is
	// already covered by the repo_prefix='' delete shape.
	if repoPrefix != "" {
		const delByID = `MATCH (n:Node) WHERE n.id STARTS WITH $idp DETACH DELETE n`
		s.runWriteLocked(delByID, map[string]any{"idp": repoPrefix + "/"})
		s.writeGen.Add(1)
	}
	if s.fileIDs != nil {
		s.fileIDs.removeFiles(affectedPaths)
	}
	return n, e
}

// evictByScopeLocked is the shared body of EvictFile / EvictRepo.
// We count the affected nodes and edges first so the caller gets
// accurate removal totals (DETACH DELETE does not surface them
// through the Go binding), then issue DETACH DELETE.
func (s *Store) evictByScopeLocked(column, value string) (int, int) {
	cntNodes := fmt.Sprintf(`MATCH (n:Node) WHERE n.%s = $v RETURN count(n)`, column)
	rows := s.querySelectLocked(cntNodes, map[string]any{"v": value})
	if len(rows) == 0 {
		return 0, 0
	}
	nNodes, _ := rows[0][0].(int64)
	if nNodes == 0 {
		return 0, 0
	}

	cntEdges := fmt.Sprintf(`
MATCH (n:Node)-[e:Edge]-(:Node)
WHERE n.%s = $v
RETURN count(DISTINCT e)`, column)
	rows = s.querySelectLocked(cntEdges, map[string]any{"v": value})
	var nEdges int64
	if len(rows) > 0 {
		nEdges, _ = rows[0][0].(int64)
	}

	del := fmt.Sprintf(`MATCH (n:Node) WHERE n.%s = $v DETACH DELETE n`, column)
	s.runWriteLocked(del, map[string]any{"v": value})
	s.writeGen.Add(1)
	return int(nNodes), int(nEdges)
}
