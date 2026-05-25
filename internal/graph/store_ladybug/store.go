package store_ladybug

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	lbug "github.com/LadybugDB/go-ladybug"

	"github.com/zzet/gortex/internal/graph"
)

// Store is the KuzuDB-backed graph.Store implementation.
type Store struct {
	db   *lbug.Database
	conn *lbug.Connection

	// writeMu serialises every mutation. KuzuDB's C engine is
	// thread-safe internally but the Go binding shares a single
	// kuzu_connection handle across goroutines; serialising at the
	// Go layer keeps semantics predictable under the conformance
	// suite's 8-goroutine concurrency test and turns Cypher
	// statements into the same sequential trace the in-memory
	// store sees.
	writeMu sync.Mutex

	// resolveMu is the resolver-coordination mutex returned by
	// ResolveMutex. Held by cross-repo / temporal / external resolver
	// passes to keep their edge mutations from interleaving. Separate
	// from writeMu so the resolver can hold it across multiple writes
	// without blocking unrelated steady-state mutations.
	resolveMu sync.Mutex

	edgeIdentityRevs atomic.Int64

	// Bulk-load fast path. When the indexer brackets its parse loop
	// with BeginBulkLoad/FlushBulk, AddBatch routes incoming rows
	// into these slices instead of round-tripping through Cypher per
	// call. FlushBulk dedupes the buffers and commits via Kuzu's
	// COPY FROM CSV — one INSERT-only statement per table, no MERGE
	// cost, no per-row Cypher parse/plan. See BeginBulkLoad doc.
	bulkMu     sync.Mutex
	bulkActive bool
	bulkNodes  []*graph.Node
	bulkEdges  []*graph.Edge

	// fts tracks whether the native FTS extension is loaded and
	// whether the symbol FTS index has been built. See fts.go for
	// the SymbolSearcher implementation.
	fts ftsState
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// Open opens (or creates) a KuzuDB database at path and applies the
// schema. The path is a directory KuzuDB owns end-to-end; an empty
// directory is initialised on first open and reused on every
// subsequent open.
func Open(path string) (*Store, error) {
	db, err := lbug.OpenDatabase(path, lbug.DefaultSystemConfig())
	if err != nil {
		return nil, fmt.Errorf("store_ladybug: open %q: %w", path, err)
	}
	conn, err := lbug.OpenConnection(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store_ladybug: open connection: %w", err)
	}
	for _, stmt := range schemaDDL {
		res, err := conn.Query(stmt)
		if err != nil {
			conn.Close()
			db.Close()
			return nil, fmt.Errorf("store_ladybug: schema %q: %w", firstLine(stmt), err)
		}
		res.Close()
	}
	return &Store{db: db, conn: conn}, nil
}

// Close closes the underlying connection and database.
func (s *Store) Close() error {
	if s.conn != nil {
		s.conn.Close()
	}
	if s.db != nil {
		s.db.Close()
	}
	return nil
}

// ResolveMutex returns the resolver-coordination mutex.
func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// -- meta encode/decode (gob → base64 STRING) ----------------------------

// encodeMeta serialises a Meta map to a base64-encoded gob frame.
// Empty / nil maps become the empty string so the common case stays
// cheap to store. base64 is required because the Go binding reads
// BLOB columns through strlen(), which would truncate at the first
// NUL byte that gob encoding routinely emits.
func encodeMeta(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// decodeMeta is the inverse of encodeMeta.
func decodeMeta(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// -- writes ---------------------------------------------------------------

// AddNode inserts (or upserts) a node. Idempotent on the id PK — a
// second AddNode for the same id is a no-op except for any column
// updates the new value carries, matching the in-memory store's
// "last write wins" behaviour.
func (s *Store) AddNode(n *graph.Node) {
	if n == nil || n.ID == "" {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.upsertNodeLocked(n)
}

func (s *Store) upsertNodeLocked(n *graph.Node) {
	metaStr, err := encodeMeta(n.Meta)
	if err != nil {
		panicOnFatal(fmt.Errorf("encode meta: %w", err))
		return
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.upsertEdgeLocked(e)
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
	// Per-call AddNode/AddEdge loop instead of the Kuzu-style UNWIND
	// path. The fork's UNWIND-MERGE statement triggers a C++
	// "unordered_map::at: key not found" panic when a row references
	// a node id that doesn't yet exist; the per-call form's explicit
	// stub-then-MERGE pattern in upsertEdgeLocked sidesteps it.
	// Bulk indexing routes through the BulkLoader COPY path above, so
	// this loop only runs on the small/incremental write surface
	// (conformance tests, daemon's reactive re-indexes).
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		s.upsertNodeLocked(n)
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		s.upsertEdgeLocked(e)
	}
}

// addNodesUnwindLocked materialises nodes as a list of structs and
// runs them through one UNWIND + MERGE per chunk.
func (s *Store) addNodesUnwindLocked(nodes []*graph.Node) {
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

// addEdgesUnwindLocked materialises edges as a list of structs and
// inserts them with endpoint stubs in one UNWIND per chunk.
// upsertEdgeLocked's per-edge stub-then-MERGE pattern is preserved:
// each UNWIND row MERGE-stubs both endpoint nodes (no-ops if they
// already exist), then MERGEs the edge with the full identity tuple,
// then SETs every edge column.
func (s *Store) addEdgesUnwindLocked(edges []*graph.Edge) {
	for i := 0; i < len(edges); i += kuzuBatchChunkSize {
		end := i + kuzuBatchChunkSize
		if end > len(edges) {
			end = len(edges)
		}
		chunk := edges[i:end]
		rows := make([]map[string]any, 0, len(chunk))
		for _, e := range chunk {
			if e == nil {
				continue
			}
			metaStr, err := encodeMeta(e.Meta)
			if err != nil {
				panicOnFatal(fmt.Errorf("encode edge meta: %w", err))
				return
			}
			var crossRepo int64
			if e.CrossRepo {
				crossRepo = 1
			}
			rows = append(rows, map[string]any{
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
			})
		}
		if len(rows) == 0 {
			continue
		}
		const q = `
UNWIND $rows AS row
MERGE (a:Node {id: row.from})
MERGE (b:Node {id: row.to})
MERGE (a)-[e:Edge {kind: row.kind, file_path: row.file_path, line: row.line}]->(b)
SET e.confidence = row.confidence,
    e.confidence_label = row.confidence_label,
    e.origin = row.origin,
    e.tier = row.tier,
    e.cross_repo = row.cross_repo,
    e.meta = row.meta`
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

// ReindexEdges UNWIND-batches the delete-old + insert-new pattern:
// one MATCH-DELETE for the old-To rows, then the standard
// UNWIND-based edge insert for the new-To rows. Both use chunked
// statements so a 10k-row resolver pass fires ~4 Cypher Execs
// instead of ~10k.
func (s *Store) ReindexEdges(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Per-call ReindexEdge loop instead of the Kuzu-style UNWIND
	// double-pass. Ladybug's UNWIND-MATCH-DELETE-then-UNWIND-MERGE
	// pattern triggers the same "unordered_map::at: key not found"
	// C++ panic as AddBatch's UNWIND-MERGE. The per-call form's
	// explicit DELETE/MATCH/MERGE sequence sidesteps the engine bug.
	// Bulk indexing routes through the BulkLoader COPY path so the
	// resolver hot path doesn't pay this loop's cost on cold start.
	for _, r := range batch {
		if r.Edge == nil || r.OldTo == r.Edge.To {
			continue
		}
		s.reindexEdgeLocked(r.Edge, r.OldTo)
	}
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
	return true
}

// EvictFile removes every node anchored to filePath and every edge
// that touches one of those nodes. DETACH DELETE handles the edge
// cleanup as part of the node delete, so a single Cypher statement
// is enough.
func (s *Store) EvictFile(filePath string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked("file_path", filePath)
}

// EvictRepo removes every node in repoPrefix and every edge that
// touches one.
func (s *Store) EvictRepo(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.evictByScopeLocked("repo_prefix", repoPrefix)
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
	return int(nNodes), int(nEdges)
}

// -- reads (point lookups) ----------------------------------------------

// GetNode returns the node with the given id, or nil if absent.
func (s *Store) GetNode(id string) *graph.Node {
	const q = `MATCH (n:Node {id: $id}) RETURN ` + nodeReturnCols + ` LIMIT 1`
	rows := s.querySelect(q, map[string]any{"id": id})
	if len(rows) == 0 {
		return nil
	}
	return rowToNode(rows[0])
}

// GetNodeByQualName returns the first node whose qual_name matches,
// or nil if absent / empty.
func (s *Store) GetNodeByQualName(qualName string) *graph.Node {
	if qualName == "" {
		return nil
	}
	const q = `MATCH (n:Node {qual_name: $q}) RETURN ` + nodeReturnCols + ` LIMIT 1`
	rows := s.querySelect(q, map[string]any{"q": qualName})
	if len(rows) == 0 {
		return nil
	}
	return rowToNode(rows[0])
}

// FindNodesByName returns every node whose Name matches.
func (s *Store) FindNodesByName(name string) []*graph.Node {
	const q = `MATCH (n:Node {name: $name}) RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"name": name})
	return rowsToNodes(rows)
}

// FindNodesByNameInRepo restricts FindNodesByName to one repo prefix.
func (s *Store) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	const q = `MATCH (n:Node {name: $name, repo_prefix: $repo}) RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"name": name, "repo": repoPrefix})
	return rowsToNodes(rows)
}

// GetFileNodes returns every node anchored to filePath.
func (s *Store) GetFileNodes(filePath string) []*graph.Node {
	const q = `MATCH (n:Node {file_path: $f}) RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"f": filePath})
	return rowsToNodes(rows)
}

// GetRepoNodes returns every node in the given repo prefix.
func (s *Store) GetRepoNodes(repoPrefix string) []*graph.Node {
	const q = `MATCH (n:Node {repo_prefix: $r}) RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"r": repoPrefix})
	return rowsToNodes(rows)
}

// GetOutEdges returns every edge whose From matches nodeID.
func (s *Store) GetOutEdges(nodeID string) []*graph.Edge {
	const q = `MATCH (a:Node {id: $id})-[e:Edge]->(b:Node) RETURN ` + edgeReturnCols
	rows := s.querySelect(q, map[string]any{"id": nodeID})
	return rowsToEdges(rows)
}

// GetInEdges returns every edge whose To matches nodeID.
func (s *Store) GetInEdges(nodeID string) []*graph.Edge {
	const q = `MATCH (a:Node)-[e:Edge]->(b:Node {id: $id}) RETURN ` + edgeReturnCols
	rows := s.querySelect(q, map[string]any{"id": nodeID})
	return rowsToEdges(rows)
}

// AllNodes materialises every node into a slice.
func (s *Store) AllNodes() []*graph.Node {
	const q = `MATCH (n:Node) RETURN ` + nodeReturnCols
	rows := s.querySelect(q, nil)
	return rowsToNodes(rows)
}

// AllEdges materialises every edge into a slice.
func (s *Store) AllEdges() []*graph.Edge {
	const q = `MATCH (a:Node)-[e:Edge]->(b:Node) RETURN ` + edgeReturnCols
	rows := s.querySelect(q, nil)
	return rowsToEdges(rows)
}

// -- predicate-shaped reads ---------------------------------------------

// EdgesByKind yields every edge whose Kind matches. The query
// materialises into a slice before yielding so the caller's body is
// free to make re-entrant store calls (the connection is held
// exclusively by an open kuzu_query_result and a re-entrant write
// would deadlock).
func (s *Store) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		const q = `MATCH (a:Node)-[e:Edge {kind: $kind}]->(b:Node) RETURN ` + edgeReturnCols
		rows := s.querySelect(q, map[string]any{"kind": string(kind)})
		for _, r := range rows {
			e := rowToEdge(r)
			if e == nil {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// NodesByKind yields every node whose Kind matches.
func (s *Store) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		const q = `MATCH (n:Node {kind: $kind}) RETURN ` + nodeReturnCols
		rows := s.querySelect(q, map[string]any{"kind": string(kind)})
		for _, r := range rows {
			n := rowToNode(r)
			if n == nil {
				continue
			}
			if !yield(n) {
				return
			}
		}
	}
}

// EdgesWithUnresolvedTarget yields every edge whose To begins with
// "unresolved::". KuzuDB has a STARTS WITH operator that compiles to
// a contiguous prefix scan when the column is indexed.
func (s *Store) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		const q = `MATCH (a:Node)-[e:Edge]->(b:Node) WHERE b.id STARTS WITH 'unresolved::' RETURN ` + edgeReturnCols
		rows := s.querySelect(q, nil)
		for _, r := range rows {
			e := rowToEdge(r)
			if e == nil {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// -- batched point lookups ----------------------------------------------

// GetNodesByIDs returns a map id→*Node for every input ID present.
// IDs not in the store are absent from the returned map.
func (s *Store) GetNodesByIDs(ids []string) map[string]*graph.Node {
	if len(ids) == 0 {
		return nil
	}
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	// IN $ids on the indexed PK collapses N point lookups into one
	// Cypher statement.
	const q = `MATCH (n:Node) WHERE n.id IN $ids RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"ids": stringSliceToAny(uniq)})
	out := make(map[string]*graph.Node, len(uniq))
	for _, r := range rows {
		n := rowToNode(r)
		if n == nil {
			continue
		}
		out[n.ID] = n
	}
	return out
}

// FindNodesByNames returns a map name→[]*Node for every input name.
// Names that match no node are absent from the returned map.
func (s *Store) FindNodesByNames(names []string) map[string][]*graph.Node {
	if len(names) == 0 {
		return nil
	}
	uniq := dedupeNonEmpty(names)
	if len(uniq) == 0 {
		return nil
	}
	const q = `MATCH (n:Node) WHERE n.name IN $names RETURN ` + nodeReturnCols
	rows := s.querySelect(q, map[string]any{"names": stringSliceToAny(uniq)})
	out := make(map[string][]*graph.Node, len(uniq))
	for _, r := range rows {
		n := rowToNode(r)
		if n == nil {
			continue
		}
		out[n.Name] = append(out[n.Name], n)
	}
	return out
}

// -- counts and stats ---------------------------------------------------

func (s *Store) NodeCount() int {
	rows := s.querySelect(`MATCH (n:Node) RETURN count(n)`, nil)
	if len(rows) == 0 {
		return 0
	}
	n, _ := rows[0][0].(int64)
	return int(n)
}

func (s *Store) EdgeCount() int {
	rows := s.querySelect(`MATCH ()-[e:Edge]->() RETURN count(e)`, nil)
	if len(rows) == 0 {
		return 0
	}
	n, _ := rows[0][0].(int64)
	return int(n)
}

func (s *Store) Stats() graph.GraphStats {
	st := graph.GraphStats{
		ByKind:     map[string]int{},
		ByLanguage: map[string]int{},
	}
	st.TotalNodes = s.NodeCount()
	st.TotalEdges = s.EdgeCount()

	rows := s.querySelect(`MATCH (n:Node) RETURN n.kind, count(n)`, nil)
	for _, r := range rows {
		kind, _ := r[0].(string)
		n, _ := r[1].(int64)
		if kind == "" {
			continue
		}
		st.ByKind[kind] = int(n)
	}
	rows = s.querySelect(`MATCH (n:Node) RETURN n.language, count(n)`, nil)
	for _, r := range rows {
		lang, _ := r[0].(string)
		n, _ := r[1].(int64)
		if lang == "" {
			continue
		}
		st.ByLanguage[lang] = int(n)
	}
	return st
}

func (s *Store) RepoStats() map[string]graph.GraphStats {
	out := map[string]graph.GraphStats{}
	rows := s.querySelect(`MATCH (n:Node) WHERE n.repo_prefix <> '' RETURN n.repo_prefix, n.kind, n.language, count(n)`, nil)
	for _, r := range rows {
		repo, _ := r[0].(string)
		kind, _ := r[1].(string)
		lang, _ := r[2].(string)
		n, _ := r[3].(int64)
		if repo == "" {
			continue
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalNodes += int(n)
		st.ByKind[kind] += int(n)
		st.ByLanguage[lang] += int(n)
		out[repo] = st
	}
	rows = s.querySelect(`
MATCH (a:Node)-[e:Edge]->(:Node)
WHERE a.repo_prefix <> ''
RETURN a.repo_prefix, count(e)`, nil)
	for _, r := range rows {
		repo, _ := r[0].(string)
		n, _ := r[1].(int64)
		if repo == "" {
			continue
		}
		st, ok := out[repo]
		if !ok {
			st = graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
		}
		st.TotalEdges = int(n)
		out[repo] = st
	}
	return out
}

func (s *Store) RepoPrefixes() []string {
	rows := s.querySelect(`MATCH (n:Node) WHERE n.repo_prefix <> '' RETURN DISTINCT n.repo_prefix`, nil)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		p, _ := r[0].(string)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// -- provenance verification --------------------------------------------

func (s *Store) EdgeIdentityRevisions() int {
	return int(s.edgeIdentityRevs.Load())
}

// VerifyEdgeIdentities is a no-op for the KuzuDB backend: there is a
// single canonical row per edge in the rel table, so the "same
// pointer in both adjacency views" invariant the in-memory store
// upholds is trivially satisfied here — no walk can find a
// divergence to report.
func (s *Store) VerifyEdgeIdentities() error { return nil }

// -- memory estimation (advisory) ---------------------------------------

const (
	perNodeByteEstimate = 256
	perEdgeByteEstimate = 128
)

func (s *Store) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	var est graph.RepoMemoryEstimate
	rows := s.querySelect(`MATCH (n:Node {repo_prefix: $r}) RETURN count(n)`, map[string]any{"r": repoPrefix})
	if len(rows) == 0 {
		return est
	}
	n, _ := rows[0][0].(int64)
	rows = s.querySelect(`
MATCH (a:Node {repo_prefix: $r})-[e:Edge]->(:Node)
RETURN count(e)`, map[string]any{"r": repoPrefix})
	var e int64
	if len(rows) > 0 {
		e, _ = rows[0][0].(int64)
	}
	est.NodeCount = int(n)
	est.EdgeCount = int(e)
	est.NodeBytes = uint64(n) * perNodeByteEstimate
	est.EdgeBytes = uint64(e) * perEdgeByteEstimate
	return est
}

func (s *Store) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	out := map[string]graph.RepoMemoryEstimate{}
	rows := s.querySelect(`MATCH (n:Node) WHERE n.repo_prefix <> '' RETURN n.repo_prefix, count(n)`, nil)
	for _, r := range rows {
		repo, _ := r[0].(string)
		n, _ := r[1].(int64)
		if repo == "" {
			continue
		}
		est := out[repo]
		est.NodeCount = int(n)
		est.NodeBytes = uint64(n) * perNodeByteEstimate
		out[repo] = est
	}
	rows = s.querySelect(`
MATCH (a:Node)-[e:Edge]->(:Node)
WHERE a.repo_prefix <> ''
RETURN a.repo_prefix, count(e)`, nil)
	for _, r := range rows {
		repo, _ := r[0].(string)
		n, _ := r[1].(int64)
		if repo == "" {
			continue
		}
		est := out[repo]
		est.EdgeCount = int(n)
		est.EdgeBytes = uint64(n) * perEdgeByteEstimate
		out[repo] = est
	}
	return out
}

// -- helpers ------------------------------------------------------------

// nodeReturnCols is the canonical projection for Node rows, ordered
// to match rowToNode's index reads.
const nodeReturnCols = `n.id, n.kind, n.name, n.qual_name, n.file_path, n.start_line, n.end_line, n.language, n.repo_prefix, n.workspace_id, n.project_id, n.meta`

// edgeReturnCols is the canonical projection for Edge rows, ordered
// to match rowToEdge's index reads.
const edgeReturnCols = `a.id, b.id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo, e.meta`

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

// -- query plumbing -----------------------------------------------------

// runWriteLocked executes a write-shaped Cypher statement under the
// caller-held writeMu. Panics on a genuine engine error (closed
// connection / schema mismatch / disk-full) — graph.Store has no
// error channel and the in-memory store can't fail either, so a
// fatal storage failure cannot be ignored.
func (s *Store) runWriteLocked(query string, args map[string]any) {
	res, err := s.executeOrQuery(query, args)
	if err != nil {
		panicOnFatal(err)
		return
	}
	res.Close()
}

// querySelect runs a read-shaped Cypher statement and materialises
// every row before returning. We deliberately consume the iterator
// to release the connection — open iterators hold the kuzu_query
// handle and re-entrant store calls would deadlock waiting for it.
func (s *Store) querySelect(query string, args map[string]any) [][]any {
	res, err := s.executeOrQuery(query, args)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer res.Close()
	var rows [][]any
	for res.HasNext() {
		tup, err := res.Next()
		if err != nil {
			panicOnFatal(err)
			return rows
		}
		vals, err := tup.GetAsSlice()
		if err != nil {
			tup.Close()
			panicOnFatal(err)
			return rows
		}
		rows = append(rows, vals)
		tup.Close()
	}
	return rows
}

// querySelectLocked is querySelect for callers that already hold
// writeMu and so must not call into the public querySelect (which
// does not lock — but the underlying connection is shared, so the
// distinction matters only as a documentation aid).
func (s *Store) querySelectLocked(query string, args map[string]any) [][]any {
	return s.querySelect(query, args)
}

// executeOrQuery hides the prepared-vs-direct distinction. KuzuDB
// requires the Prepare → Execute path for parameterised statements;
// a bare Query with `$arg` placeholders is rejected. Statements
// without parameters fall through to a direct Query for clarity.
func (s *Store) executeOrQuery(query string, args map[string]any) (*lbug.QueryResult, error) {
	if len(args) == 0 {
		return s.conn.Query(query)
	}
	stmt, err := s.conn.Prepare(query)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	return s.conn.Execute(stmt, args)
}

// panicOnFatal turns a non-nil engine error into a panic so callers
// see catastrophic failures. The graph.Store interface deliberately
// does not surface errors — it mirrors the in-memory store's
// "everything succeeds" contract — so a fatal storage failure
// cannot be silently dropped.
func panicOnFatal(err error) {
	if err == nil {
		return
	}
	panic(fmt.Errorf("store_ladybug: %w", err))
}

// firstLine is a small helper for trimming a multi-line Cypher
// statement to its first non-empty line for use in error messages.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// -- BulkLoader implementation -------------------------------------------

// Compile-time assertion: *Store satisfies graph.BulkLoader, so the
// indexer's BulkLoader probe picks up the COPY-FROM-CSV fast path
// instead of falling through to per-batch UNWIND.
var _ graph.BulkLoader = (*Store)(nil)

// BeginBulkLoad enters buffer-mode write. Subsequent AddBatch calls
// append into in-memory slices without round-tripping to Kuzu; the
// buffer is committed via Kuzu's COPY FROM primitive when FlushBulk
// is called. Calling twice without an intervening FlushBulk panics.
func (s *Store) BeginBulkLoad() {
	s.bulkMu.Lock()
	defer s.bulkMu.Unlock()
	if s.bulkActive {
		panic("store_ladybug: BeginBulkLoad called twice without FlushBulk")
	}
	s.bulkActive = true
}

// FlushBulk commits the accumulated bulk buffer via Kuzu's COPY FROM
// CSV path — one INSERT-only statement per table, no MERGE cost, no
// per-row Cypher parse/plan. After FlushBulk, AddBatch returns to its
// regular per-call UNWIND path.
//
// Dedup contract: nodes are deduped by ID (last write wins, matching
// the in-memory store's AddBatch semantics); edges are deduped by the
// identity tuple (from, to, kind, file_path, line). Edge endpoints
// not present in the node buffer are auto-stubbed so the rel-table
// foreign-key constraint is satisfied (mirrors the per-call
// mergeStubNodeLocked path).
func (s *Store) FlushBulk() error {
	s.bulkMu.Lock()
	if !s.bulkActive {
		s.bulkMu.Unlock()
		return fmt.Errorf("store_ladybug: FlushBulk without BeginBulkLoad")
	}
	nodes := s.bulkNodes
	edges := s.bulkEdges
	s.bulkNodes = nil
	s.bulkEdges = nil
	s.bulkActive = false
	s.bulkMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// COPY FROM is INSERT-only — fast on an empty table, but a
	// duplicate primary key collides (unresolved::* stubs cross
	// chunks under streaming-flush). When the store already has
	// data, fall back to the per-call AddNode/AddEdge loop which
	// is idempotent on duplicate keys via MERGE semantics.
	if s.nodeCountLocked() > 0 || s.edgeCountLocked() > 0 {
		for _, n := range nodes {
			if n == nil || n.ID == "" {
				continue
			}
			s.upsertNodeLocked(n)
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			s.upsertEdgeLocked(e)
		}
		return nil
	}
	return s.copyBulkLocked(nodes, edges)
}

func (s *Store) nodeCountLocked() int {
	rows := s.querySelectLocked(`MATCH (n:Node) RETURN count(n)`, nil)
	if len(rows) == 0 {
		return 0
	}
	n, _ := rows[0][0].(int64)
	return int(n)
}

func (s *Store) edgeCountLocked() int {
	rows := s.querySelectLocked(`MATCH ()-[e:Edge]->() RETURN count(e)`, nil)
	if len(rows) == 0 {
		return 0
	}
	n, _ := rows[0][0].(int64)
	return int(n)
}

// copyBulkLocked dedupes the bulk buffers, writes them to temp CSV
// files, and runs COPY FROM for each table. Must be called with
// s.writeMu held.
func (s *Store) copyBulkLocked(nodes []*graph.Node, edges []*graph.Edge) error {
	// Dedup nodes by ID (last write wins). The in-memory store's
	// AddBatch overwrites on duplicate ID; mirror that here.
	nodePos := make(map[string]int, len(nodes))
	dedupedNodes := nodes[:0]
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if pos, ok := nodePos[n.ID]; ok {
			dedupedNodes[pos] = n
		} else {
			nodePos[n.ID] = len(dedupedNodes)
			dedupedNodes = append(dedupedNodes, n)
		}
	}
	nodes = dedupedNodes

	// Dedup edges by identity tuple (last write wins). Same rationale
	// as the in-memory store's MERGE semantics.
	type edgeKey struct {
		from, to, kind, file string
		line                 int
	}
	edgePos := make(map[edgeKey]int, len(edges))
	dedupedEdges := edges[:0]
	for _, e := range edges {
		if e == nil {
			continue
		}
		k := edgeKey{e.From, e.To, string(e.Kind), e.FilePath, e.Line}
		if pos, ok := edgePos[k]; ok {
			dedupedEdges[pos] = e
		} else {
			edgePos[k] = len(dedupedEdges)
			dedupedEdges = append(dedupedEdges, e)
		}
	}
	edges = dedupedEdges

	// Auto-stub endpoints not in the node buffer. The rel-table
	// foreign-key constraint requires both endpoints to exist in the
	// node table; per-call AddEdge handles this via
	// mergeStubNodeLocked. For COPY there's no per-row hook, so we
	// pre-stub here.
	for _, e := range edges {
		if e.From != "" {
			if _, ok := nodePos[e.From]; !ok {
				nodePos[e.From] = len(nodes)
				nodes = append(nodes, &graph.Node{ID: e.From})
			}
		}
		if e.To != "" {
			if _, ok := nodePos[e.To]; !ok {
				nodePos[e.To] = len(nodes)
				nodes = append(nodes, &graph.Node{ID: e.To})
			}
		}
	}

	if len(nodes) == 0 && len(edges) == 0 {
		return nil
	}

	// Write CSV files to a per-flush temp dir. Cleaned up regardless
	// of COPY success/failure.
	dir, err := os.MkdirTemp("", "kuzu-bulk-")
	if err != nil {
		return fmt.Errorf("mkdir bulk tmp: %w", err)
	}
	defer os.RemoveAll(dir)

	if len(nodes) > 0 {
		nodesPath := filepath.Join(dir, "nodes.csv")
		if err := writeNodesTSV(nodesPath, nodes); err != nil {
			return fmt.Errorf("write nodes tsv: %w", err)
		}
		// HEADER=false maps columns by position (no chance of a
		// header-name mismatch silently dropping rows). DELIM='\t'
		// because Kuzu's CSV parser does not handle RFC-4180-style
		// quoted strings containing commas — it splits on the
		// delimiter naively. Code identifiers and names never contain
		// tabs, so TSV sidesteps the quoting problem entirely.
		copyQ := fmt.Sprintf("COPY Node FROM '%s' (HEADER=false, DELIM='\t')", escapeCypherStringLit(nodesPath))
		res, err := s.conn.Query(copyQ)
		if err != nil {
			return fmt.Errorf("copy nodes: %w", err)
		}
		res.Close()
	}

	if len(edges) > 0 {
		edgesPath := filepath.Join(dir, "edges.csv")
		if err := writeEdgesTSV(edgesPath, edges); err != nil {
			return fmt.Errorf("write edges tsv: %w", err)
		}
		copyQ := fmt.Sprintf("COPY Edge FROM '%s' (HEADER=false, DELIM='\t')", escapeCypherStringLit(edgesPath))
		res, err := s.conn.Query(copyQ)
		if err != nil {
			return fmt.Errorf("copy edges: %w", err)
		}
		res.Close()
	}

	return nil
}

// writeNodesTSV writes nodes to a tab-separated values file in
// schema-column order. Kuzu's COPY FROM parser does not honour
// RFC-4180 quoted-string escaping (a quoted field with embedded
// commas is naively split on the delimiter), so TSV with a sanitised
// payload is the safe transport for arbitrary user data. Tabs in
// any text column are replaced with a single space; newlines with a
// space — these characters never appear in code identifiers,
// qualified names, or file paths, and base64-encoded meta is
// tab-/newline-free by construction.
func writeNodesTSV(path string, nodes []*graph.Node) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer bw.Flush()

	for _, n := range nodes {
		metaStr := ""
		if len(n.Meta) > 0 {
			s, err := encodeMeta(n.Meta)
			if err != nil {
				return fmt.Errorf("encode meta for %q: %w", n.ID, err)
			}
			metaStr = s
		}
		fields := [12]string{
			sanitizeTSV(n.ID),
			sanitizeTSV(string(n.Kind)),
			sanitizeTSV(n.Name),
			sanitizeTSV(n.QualName),
			sanitizeTSV(n.FilePath),
			strconv.Itoa(n.StartLine),
			strconv.Itoa(n.EndLine),
			sanitizeTSV(n.Language),
			sanitizeTSV(n.RepoPrefix),
			sanitizeTSV(n.WorkspaceID),
			sanitizeTSV(n.ProjectID),
			metaStr,
		}
		for i, f := range fields {
			if i > 0 {
				if err := bw.WriteByte('\t'); err != nil {
					return err
				}
			}
			if _, err := bw.WriteString(f); err != nil {
				return err
			}
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// writeEdgesTSV writes edges to a TSV file with FROM/TO ids in the
// first two columns (matching Kuzu's REL CSV convention) followed by
// the rel-table property columns in schema order.
func writeEdgesTSV(path string, edges []*graph.Edge) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer bw.Flush()

	for _, e := range edges {
		metaStr := ""
		if len(e.Meta) > 0 {
			s, err := encodeMeta(e.Meta)
			if err != nil {
				return fmt.Errorf("encode meta for edge %q→%q: %w", e.From, e.To, err)
			}
			metaStr = s
		}
		crossRepo := "0"
		if e.CrossRepo {
			crossRepo = "1"
		}
		fields := [11]string{
			sanitizeTSV(e.From),
			sanitizeTSV(e.To),
			sanitizeTSV(string(e.Kind)),
			sanitizeTSV(e.FilePath),
			strconv.Itoa(e.Line),
			strconv.FormatFloat(e.Confidence, 'g', -1, 64),
			sanitizeTSV(e.ConfidenceLabel),
			sanitizeTSV(e.Origin),
			sanitizeTSV(e.Tier),
			crossRepo,
			metaStr,
		}
		for i, f := range fields {
			if i > 0 {
				if err := bw.WriteByte('\t'); err != nil {
					return err
				}
			}
			if _, err := bw.WriteString(f); err != nil {
				return err
			}
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// sanitizeTSV strips bytes that would corrupt a tab-separated record —
// tabs become spaces, CR/LF become spaces. Code identifiers, qualified
// names, file paths, and base64-encoded meta strings never contain
// these in practice; the sanitiser exists to guarantee a malformed
// extractor output can't break the cold-load path.
func sanitizeTSV(s string) string {
	if !strings.ContainsAny(s, "\t\r\n") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\t', '\r', '\n':
			b = append(b, ' ')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// escapeCypherStringLit escapes a string for safe use inside a Cypher
// single-quoted literal — turns ' into \' and \ into \\. Used for
// COPY FROM paths, which are templated into the Cypher query (no
// parameter binding for COPY paths in the current Kuzu binding).
func escapeCypherStringLit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

// -- BackendResolver implementation --------------------------------------

// Compile-time assertion: *Store satisfies graph.BackendResolver.
var _ graph.BackendResolver = (*Store)(nil)

// ResolveUniqueNames pushes the largest trivially-correct subset of
// the resolver's work into the Kuzu engine via a single Cypher
// MATCH+SET. For every Edge whose to_id starts with "unresolved::",
// strip the prefix to recover the embedded identifier name; if
// exactly one Node carries that name (no ambiguity), rewrite the
// edge in place to point at the resolved node and bump its origin
// to "ast_resolved". Edges with zero or multiple candidates are
// untouched — they fall through to the Go resolver which has the
// language/scope/visibility rules needed to disambiguate.
//
// The query runs as one statement on the server; the Go side does
// nothing per resolved edge. On a 50k-file repo this collapses
// what would otherwise be ~30k per-edge round-trips into a single
// Cypher Execute.
func (s *Store) ResolveUniqueNames() (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Strategy: for each unresolved edge, derive the name by
	// stripping the "unresolved::" prefix. Match it against Node.name.
	// If exactly one candidate, swap the edge's to-pointer (DELETE +
	// CREATE a new edge with the same properties but the resolved
	// to-endpoint — Kuzu rel edges are immutable on their endpoint
	// pair so a direct SET of from/to is not supported).
	const q = `
MATCH (caller:Node)-[e:Edge]->(stub:Node)
WHERE stub.id STARTS WITH 'unresolved::'
WITH e, caller, stub, substring(stub.id, 13, size(stub.id) - 12) AS name
OPTIONAL MATCH (cnd:Node {name: name})
WITH e, caller, stub, name, count(cnd) AS cnt
WHERE cnt = 1
MATCH (target:Node {name: name})
DELETE e
CREATE (caller)-[newE:Edge {
    kind: e.kind,
    file_path: e.file_path,
    line: e.line,
    confidence: e.confidence,
    confidence_label: e.confidence_label,
    origin: 'ast_resolved',
    tier: 'ast_resolved',
    cross_repo: e.cross_repo,
    meta: e.meta
}]->(target)
RETURN count(newE) AS resolved`
	res, err := s.conn.Query(q)
	if err != nil {
		return 0, fmt.Errorf("backend-resolver: %w", err)
	}
	defer res.Close()
	if !res.HasNext() {
		return 0, nil
	}
	row, err := res.Next()
	if err != nil {
		return 0, fmt.Errorf("backend-resolver: read result: %w", err)
	}
	defer row.Close()
	vals, err := row.GetAsSlice()
	if err != nil || len(vals) == 0 {
		return 0, err
	}
	n, _ := vals[0].(int64)
	if n > 0 {
		s.edgeIdentityRevs.Add(n)
	}
	return int(n), nil
}
