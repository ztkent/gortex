package store_ladybug

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store satisfies graph.BulkLoader, so the
// indexer's BulkLoader probe picks up the COPY-FROM-CSV fast path
// instead of falling through to per-batch UNWIND.
var _ graph.BulkLoader = (*Store)(nil)

// BeginBulkLoad enters buffer-mode write. Subsequent AddBatch calls
// append into in-memory slices without round-tripping to Kuzu; the
// buffer is committed via Kuzu's COPY FROM primitive when FlushBulk
// is called.
//
// When two callers race (concurrent per-repo Indexers draining their
// shadows into the same Store), the second blocks on bulkSlot until
// the first FlushBulk releases it — drains serialise instead of
// panicking. The matching FlushBulk MUST run on the same goroutine
// (the IndexCtx defer pattern guarantees this).
func (s *Store) BeginBulkLoad() {
	s.bulkSlot.Lock()
	s.bulkMu.Lock()
	defer s.bulkMu.Unlock()
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
	// Release the per-Store bulk slot so the next concurrent drain
	// (a different per-repo Indexer waiting in BeginBulkLoad) can
	// take it. Held across the COPY below in the original design;
	// releasing here lets the next caller start staging rows into
	// its own buffer while this one's COPY is still in flight. The
	// underlying COPY queries themselves still serialise on
	// writeMu via runCopyPooled — that's where Ladybug's
	// single-writer constraint actually bites — so unblocking the
	// staging window is pure latency win, not a concurrency
	// hazard.
	s.bulkSlot.Unlock()

	// Always take the COPY path. The prior fallback to per-row
	// upsertNodeLocked when the store was non-empty existed to
	// dodge PRIMARY KEY conflicts between concurrent FlushBulks
	// (and between streaming-flush chunks within a single
	// IndexCtx). With per-repo-prefixed stubs (internal/graph/stub.go)
	// no two per-repo Indexers can emit the same Node ID, so the
	// fallback is now dead weight — it forced the gortex repo
	// onto 190k per-row MERGEs holding writeMu for minutes while
	// every other repo's FlushBulk queued behind it.
	//
	// copyBulkLocked itself runs its COPY queries through the
	// connection pool, so two concurrent FlushBulks parallelise
	// instead of serialising on a single Connection handle.
	if err := s.copyBulkLocked(nodes, edges); err != nil {
		return err
	}
	if len(nodes) > 0 || len(edges) > 0 {
		s.writeGen.Add(1)
	}
	if len(nodes)+len(edges) >= mallocTrimRowThreshold {
		mallocTrim()
	}
	return nil
}

// copyBulkLocked dedupes the bulk buffers, writes them to temp CSV
// files, and runs COPY FROM for each table. Must be called with
// s.writeMu held.
//
// Multi-repo wrinkle: extractors emit `unresolved::<name>` targets
// before the resolver runs. Most are resolved in the per-repo
// shadow, but a residue always remains (truly unresolved symbols,
// or names the language extractor can't bind without semantic
// context). Across repos those `unresolved::*` ids collide on the
// COPY's PRIMARY KEY. Rewrite them to `<repoPrefix>::unresolved::*`
// using the repo prefix taken from any node in the batch (one
// per-repo Indexer's drain carries nodes from a single repo).
func (s *Store) copyBulkLocked(nodes []*graph.Node, edges []*graph.Edge) error {
	repoPrefix := ""
	for _, n := range nodes {
		if n != nil && n.RepoPrefix != "" {
			repoPrefix = n.RepoPrefix
			break
		}
	}
	if repoPrefix != "" {
		const unresolvedTag = "unresolved::"
		// Encoding: prepend the repo prefix to the bare
		// `unresolved::Name` form so cross-repo emitters don't
		// collide on the COPY PK. Result: `<repoPrefix>::unresolved::<name>`.
		// The Go-level per-edge resolver's EdgesWithUnresolvedTarget
		// uses a literal `STARTS WITH 'unresolved::'` scan, which
		// intentionally MISSES these multi-repo stubs — the Cypher
		// backend resolver runs a batched pass that handles every
		// form via kind/name normalisation, so we save the per-edge
		// Cypher round-trip cost on the Go side and let the engine
		// resolve the whole population in one shot.
		rewrite := func(id string) string {
			if id == "" || !strings.HasPrefix(id, unresolvedTag) {
				return id
			}
			return repoPrefix + "::" + id
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			e.From = rewrite(e.From)
			e.To = rewrite(e.To)
		}
		for _, n := range nodes {
			if n == nil {
				continue
			}
			n.ID = rewrite(n.ID)
		}
	}
	// Dedup nodes by SANITIZED ID (last write wins). The TSV writer
	// strips tab/CR/LF — so two raw IDs that differ only in those
	// characters (e.g. extractor output with embedded newlines in an
	// inline TypeScript object-type literal: `unresolved::{   foo:
	// X[]\n   bar: () => Y }`) collapse to the same column-0 value at
	// COPY time, and Kuzu rejects the run with "duplicated primary
	// key value". Using the sanitized form here keeps the dedup map's
	// view of "same node" aligned with what the COPY parser sees. We
	// also normalize n.ID to the sanitized form so the auto-stub and
	// edge endpoints match, and so the eventual writeNodesTSV /
	// writeEdgesTSV pair emit identical strings on both sides of the
	// rel-table FK.
	//
	// The in-memory store's AddBatch overwrites on duplicate ID; this
	// preserves the same semantics modulo the sanitization mapping.
	nodePos := make(map[string]int, len(nodes))
	dedupedNodes := nodes[:0]
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		san := sanitizeTSV(n.ID)
		if san != n.ID {
			n.ID = san
		}
		if pos, ok := nodePos[n.ID]; ok {
			dedupedNodes[pos] = n
		} else {
			nodePos[n.ID] = len(dedupedNodes)
			dedupedNodes = append(dedupedNodes, n)
		}
	}
	nodes = dedupedNodes
	// Feed the file→id accelerator from the deduped buffer. Done here
	// (before COPY) so we don't have to re-scan after the write — the
	// COPY appends every row anyway, success-or-failure handling
	// upstream already rolls writeGen back on a fatal error.
	if s.fileIDs != nil {
		s.fileIDs.addNodes(nodes)
	}
	if s.nameIdx != nil {
		s.nameIdx.addNodes(nodes)
	}

	// Dedup edges by identity tuple (last write wins). Same rationale
	// as the in-memory store's MERGE semantics. Endpoints are
	// sanitized to match the node-ID sanitization above — otherwise
	// an edge pointing at `unresolved::Writer\n}` references a node
	// the CSV writer collapses to `unresolved::Writer }`, and Kuzu's
	// COPY Edge fails with "unable to find primary key value".
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
		if san := sanitizeTSV(e.From); san != e.From {
			e.From = san
		}
		if san := sanitizeTSV(e.To); san != e.To {
			e.To = san
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
	// NOTE: an earlier revision pre-filtered nodes against the live
	// Node table here via a `MATCH (n:Node) WHERE n.id IN $ids` probe
	// to make COPY idempotent against duplicate primary keys. That
	// query crashed the daemon with `IO exception: Cannot read from
	// file ... position: <bytes>` because it issued a read on the
	// same .lbug file that a concurrent COPY (from a sibling
	// per-repo IndexCtx whose FlushBulk had already released
	// bulkSlot but still held writeMu inside runCopyPooled) was
	// extending — Kuzu's MVCC can't serve a buffer-pool read while
	// the file is being grown by another transaction in the same
	// process. The sanitize-aware dedup above is the cheaper and
	// safer fix for the duplicate-PK class this filter was meant to
	// catch; cross-bulk collisions are now rare enough that the
	// per-COPY error message (handled by the caller's retry) is
	// acceptable when they happen.

	if len(nodes) == 0 && len(edges) == 0 {
		return nil
	}

	// Write CSV files to a per-flush temp dir. Cleaned up regardless
	// of COPY success/failure.
	dir, err := os.MkdirTemp("", "kuzu-bulk-")
	if err != nil {
		return fmt.Errorf("mkdir bulk tmp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

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
		if err := s.runCopyPooled(copyQ); err != nil {
			if !isNonEmptyNodeCopyErr(err) {
				return fmt.Errorf("copy nodes: %w", err)
			}
			// Kuzu rejects COPY into a non-empty primary-key node table
			// unless its PK hash index is currently materialised — and
			// that depends on auto-checkpoint timing, so on a fresh
			// store every per-repo drain after the first fails here
			// (only the first repo, COPYing into the empty table,
			// persisted). The bulk path used to fall back to per-row
			// MERGEs for the non-empty case; that was dropped on the
			// assumption per-repo-prefixed stub IDs removed all PK
			// collisions — true for collisions, but it overlooked this
			// empty-table precondition. Re-load via LOAD FROM ... MERGE:
			// a DML write with no empty-table precondition, one
			// statement, no per-row Go round-trip. Mirrors the
			// SymbolFTS re-bulk. CAST the two INT64 columns; the rest
			// are STRING. column0..11 are the positional names Ladybug
			// assigns under header=false, matching writeNodesTSV order.
			mergeQ := fmt.Sprintf(
				"LOAD FROM '%s' (header=false, delim='\\t') "+
					"MERGE (n:Node {id: column0}) "+
					"SET n.kind = column1, n.name = column2, n.qual_name = column3, "+
					"n.file_path = column4, n.start_line = CAST(column5 AS INT64), "+
					"n.end_line = CAST(column6 AS INT64), n.language = column7, "+
					"n.repo_prefix = column8, n.workspace_id = column9, "+
					"n.project_id = column10, n.meta = column11",
				escapeCypherStringLit(nodesPath))
			if err := s.runCopyPooled(mergeQ); err != nil {
				return fmt.Errorf("load nodes (merge fallback after non-empty copy): %w", err)
			}
		}
	}

	if len(edges) > 0 {
		edgesPath := filepath.Join(dir, "edges.csv")
		if err := writeEdgesTSV(edgesPath, edges); err != nil {
			return fmt.Errorf("write edges tsv: %w", err)
		}
		copyQ := fmt.Sprintf("COPY Edge FROM '%s' (HEADER=false, DELIM='\t')", escapeCypherStringLit(edgesPath))
		if err := s.runCopyPooled(copyQ); err != nil {
			return fmt.Errorf("copy edges: %w", err)
		}
	}

	return nil
}

// isNonEmptyNodeCopyErr reports whether err is Kuzu's rejection of a
// COPY into a non-empty primary-key node table whose hash index isn't
// materialised. The string is verbatim from liblbug 0.17.0; it is the
// one error the COPY→MERGE fallback in copyBulkLocked recovers from
// (any other COPY failure is propagated). Coupled to the engine
// message by necessity — liblbug exposes no typed error for it.
func isNonEmptyNodeCopyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "non-empty primary-key node table")
}

// runCopyPooled runs a parameter-less COPY query. Holds writeMu
// for the duration: Ladybug only allows ONE write transaction
// at a time per database; concurrent COPYs from different
// connections fail with "Cannot start a new write transaction
// in the system". The pool still parallelises READS (querySelect
// no longer locks), but writes serialise here at the Go layer
// to match ladybug's MVCC contract.
//
// The COPY query itself is parameter-less so we go straight
// through conn.Query on a pooled connection.
func (s *Store) runCopyPooled(copyQ string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, release, err := s.executeOrQuery(copyQ, nil)
	if err != nil {
		return err
	}
	if res != nil {
		res.Close()
	}
	release()
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
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = bw.Flush() }()

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
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = bw.Flush() }()

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

// reindexEdgesBulk applies a resolver reindex batch with three
// file-driven statements instead of the per-edge DELETE+upsert loop:
//
//  1. MERGE-stub every distinct endpoint node (caller + resolved target),
//     parity with upsertEdgeLocked's mergeStubNodeLocked so a resolution
//     to a not-yet-materialised target node isn't silently dropped, and
//     so COPY (which requires both rel endpoints to exist) can't fail.
//  2. COPY the resolved edges into the rel table — a STREAMING bulk load.
//     The earlier LOAD ... MATCH ... MERGE form materialised the whole
//     80k MATCH+join in the buffer pool and OOMed at cold-start scale;
//     COPY streams. newEdges is de-duped by identity first since COPY
//     appends (rel tables have no primary key, so it never rejects).
//  3. DELETE the old stub edges by their exact identity (LOAD-driven).
//
// The LOAD/COPY forms (file scans), NOT UNWIND, are what sidestep the
// "unordered_map::at: key not found" C++ panic that forced ReindexEdges
// onto the per-edge loop in the first place. All three run under one
// writeMu hold.
//
// Returns false on any failure so ReindexEdges falls back to the per-edge
// loop; a partial bulk apply is safe to re-drive per-edge because the
// per-edge upsert MERGEs idempotently over any COPY-inserted rows and the
// DELETE is keyed on the stub's exact identity.
func (s *Store) reindexEdgesBulk(changed []graph.EdgeReindex) (ok bool) {
	dir, err := os.MkdirTemp("", "gortex-reindex-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(dir) }()

	endpoints := make(map[string]struct{}, len(changed)*2)
	newEdges := make([]*graph.Edge, 0, len(changed))
	// COPY appends (no MERGE-style dedup), so de-dup the resolved edges
	// by identity (from,to,kind,file,line) before writing the file —
	// guards against a batch that resolves two stubs at the same call
	// site to the same target emitting a duplicate rel.
	seen := make(map[string]struct{}, len(changed))
	for _, r := range changed {
		if r.Edge.From != "" {
			endpoints[r.Edge.From] = struct{}{}
		}
		if r.Edge.To != "" {
			endpoints[r.Edge.To] = struct{}{}
		}
		key := r.Edge.From + "\x00" + r.Edge.To + "\x00" + string(r.Edge.Kind) + "\x00" + r.Edge.FilePath + "\x00" + strconv.Itoa(r.Edge.Line)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		newEdges = append(newEdges, r.Edge)
	}

	endpointsPath := filepath.Join(dir, "endpoints.csv")
	if err := writeIDsTSV(endpointsPath, endpoints); err != nil {
		return false
	}
	newPath := filepath.Join(dir, "new_edges.csv")
	if err := writeEdgesTSV(newPath, newEdges); err != nil {
		return false
	}
	keysPath := filepath.Join(dir, "old_keys.csv")
	if err := writeReindexDeleteKeysTSV(keysPath, changed); err != nil {
		return false
	}

	stubQ := fmt.Sprintf(
		"LOAD FROM '%s' (header=false, delim='\t') "+
			"MERGE (n:Node {id: column0}) "+
			"ON CREATE SET n.kind='', n.name='', n.qual_name='', n.file_path='', "+
			"n.start_line=0, n.end_line=0, n.language='', n.repo_prefix='', "+
			"n.workspace_id='', n.project_id='', n.meta=''",
		escapeCypherStringLit(endpointsPath))
	// Insert via COPY, not LOAD ... MATCH ... MERGE: COPY streams the file
	// into the rel table, whereas MERGE materialises the entire MATCH+join
	// in the buffer pool and OOMs at cold-start scale ("Buffer manager
	// exception: the buffer pool is full" on an 80k batch). The stub-merge
	// above guarantees both endpoints exist (COPY into a rel needs them),
	// and newEdges is de-duped by identity, so an append-only COPY is
	// correct here. COPY into a non-empty rel table appends (rel tables
	// have no primary key — the non-empty-COPY rejection is node-only).
	copyQ := fmt.Sprintf("COPY Edge FROM '%s' (HEADER=false, DELIM='\t')", escapeCypherStringLit(newPath))
	delQ := fmt.Sprintf(
		"LOAD FROM '%s' (header=false, delim='\t') "+
			"MATCH (a:Node {id: column0})-[e:Edge {kind: column1, file_path: column2, line: CAST(column3 AS INT64)}]->(b:Node {id: column4}) "+
			"DELETE e",
		escapeCypherStringLit(keysPath))

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Order matters: stub endpoints and insert resolved edges before
	// deleting the stub rows. Insert-then-delete keeps the resolved edge
	// distinct from the deleted one (different To) at every step. Each
	// step is timed + logged independently so a slow or failing step is
	// visible (no `||` short-circuit hiding which ran).
	steps := [...]struct {
		label string
		query string
	}{
		{"stub-merge", stubQ},
		{"copy-insert", copyQ},
		{"delete", delQ},
	}
	for _, st := range steps {
		t0 := time.Now()
		res, release, err := s.executeOrQuery(st.query, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[REINDEX-BULK] %s FAILED (edges=%d, %s): %v\n",
				st.label, len(changed), time.Since(t0).Round(time.Millisecond), err)
			return false
		}
		if res != nil {
			res.Close()
		}
		release()
	}
	s.writeGen.Add(1)
	return true
}

// writeIDsTSV writes one sanitised node id per line — the endpoint set
// the bulk reindex MERGE-stubs before inserting rels.
func writeIDsTSV(path string, ids map[string]struct{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = bw.Flush() }()
	for id := range ids {
		if _, err := bw.WriteString(sanitizeTSV(id)); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// writeReindexDeleteKeysTSV writes the identity of each stale stub edge to
// delete: from, kind, file_path, line, oldTo (the row that still points at
// the pre-resolution target).
func writeReindexDeleteKeysTSV(path string, batch []graph.EdgeReindex) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = bw.Flush() }()
	for _, r := range batch {
		e := r.Edge
		fields := [5]string{
			sanitizeTSV(e.From),
			sanitizeTSV(string(e.Kind)),
			sanitizeTSV(e.FilePath),
			strconv.Itoa(e.Line),
			sanitizeTSV(r.OldTo),
		}
		for i, fld := range fields {
			if i > 0 {
				if err := bw.WriteByte('\t'); err != nil {
					return err
				}
			}
			if _, err := bw.WriteString(fld); err != nil {
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
