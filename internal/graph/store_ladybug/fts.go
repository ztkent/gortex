package store_ladybug

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// ftsIndexName is the canonical name for the FTS index built over
// SymbolFTS.tokens. Hard-coded because the index is internal to the
// store — callers only ever query it through SearchSymbols.
const ftsIndexName = "idx_symbol_fts_tokens"

// fts holds the per-store FTS state. The extension only needs to be
// installed + loaded once per database lifetime; built tracks whether
// CREATE_FTS_INDEX has run so SearchSymbols can lazily build on the
// first query in case BuildSymbolIndex hasn't been called yet.
type ftsState struct {
	extensionLoaded atomic.Bool
	indexBuilt      atomic.Bool
}

// ensureFTSExtension loads the FTS extension into the current
// connection. Idempotent — the second call is a no-op via the
// extensionLoaded sentinel. Cypher's INSTALL fails when the
// extension is already known (per the upstream error message we
// surface), so we wrap with a recovery and treat
// already-installed as success.
//
// Held under writeMu by the caller so concurrent connections don't
// race the load.
func (s *Store) ensureFTSExtensionLocked() error {
	if s.fts.extensionLoaded.Load() {
		return nil
	}
	if err := runCypherSafe(s, `INSTALL FTS`); err != nil &&
		!strings.Contains(err.Error(), "is already installed") {
		// Ignore "already installed" — every fresh open re-runs
		// this and we don't want it to be a hard failure.
		_ = err
	}
	if err := runCypherSafe(s, `LOAD EXTENSION FTS`); err != nil {
		return fmt.Errorf("load fts extension: %w", err)
	}
	s.fts.extensionLoaded.Store(true)
	return nil
}

// UpsertSymbolFTS records (or replaces) the pre-tokenised text for
// nodeID in the SymbolFTS sidecar table. Called by the indexer for
// every node that passes shouldIndexForSearch — non-searchable
// kinds (KindFile, KindImport, KindLocal, KindBuiltin) never reach
// here, so the FTS corpus stays a clean subset of the graph.
//
// Idempotent on nodeID via MERGE so a re-index of the same file
// replaces the prior row in place rather than appending.
//
// Per-call cost is ~one MERGE; the bulk path (FlushBulk) skips this
// and instead emits a COPY-FROM TSV in copyBulkLocked for the cold-
// start fast path.
func (s *Store) UpsertSymbolFTS(nodeID, tokens string) error {
	if nodeID == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureFTSExtensionLocked(); err != nil {
		return err
	}
	const q = `MERGE (f:SymbolFTS {id: $id}) SET f.tokens = $tokens`
	if err := runCypherWithArgs(s, q, map[string]any{
		"id":     nodeID,
		"tokens": tokens,
	}); err != nil {
		return fmt.Errorf("upsert SymbolFTS: %w", err)
	}
	return nil
}

// BulkUpsertSymbolFTS is the cold-start fast path: write a TSV of
// (id, tokens) pairs to a temp file and COPY FROM into SymbolFTS in
// one shot. Per-row cost ≈ 1µs on Ladybug's columnar storage,
// vs ~1ms for the Cypher MERGE path UpsertSymbolFTS takes —
// ~1000x cheaper at 600k-node scale.
//
// The COPY destination is wiped first via `MATCH (f:SymbolFTS)
// DELETE f` so a re-run replaces the corpus rather than appending.
// This is safe because the indexer always calls
// BulkUpsertSymbolFTS once per IndexCtx (after the shadow drain
// completes), not on the daemon's incremental reindex path.
//
// Idempotent under empty input — no-ops cleanly so callers don't
// need to length-check.
func (s *Store) BulkUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureFTSExtensionLocked(); err != nil {
		return err
	}

	// Dedup by ID — last write wins, mirroring the per-call
	// UpsertSymbolFTS's MERGE semantics. The indexer's drain
	// shouldn't produce duplicates at the searchable-node layer
	// (every Node ID is unique), but guard against the edge case
	// where a re-parse of a file emitted the same ID twice.
	pos := make(map[string]int, len(items))
	deduped := items[:0]
	for _, it := range items {
		if it.NodeID == "" {
			continue
		}
		if p, ok := pos[it.NodeID]; ok {
			deduped[p] = it
		} else {
			pos[it.NodeID] = len(deduped)
			deduped = append(deduped, it)
		}
	}
	items = deduped
	if len(items) == 0 {
		return nil
	}

	// Wipe prior FTS rows so the cold-load fast path is a clean
	// rebuild. Costs O(N) on the existing row set — acceptable
	// because this only runs at IndexCtx commit, not on every
	// incremental update.
	if err := runCypherSafe(s, `MATCH (f:SymbolFTS) DELETE f`); err != nil {
		return fmt.Errorf("clear SymbolFTS before bulk upsert: %w", err)
	}

	dir, err := os.MkdirTemp("", "lbug-fts-bulk-")
	if err != nil {
		return fmt.Errorf("mkdir bulk tmp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Ladybug's COPY binder rejects ".tsv" with "Cannot load from file
	// type tsv"; the parser dispatches on extension. ".csv" + DELIM='\t'
	// is the convention the Node / Edge / SymbolVec bulk loaders use.
	path := filepath.Join(dir, "symbolfts.csv")
	if err := writeSymbolFTSTSV(path, items); err != nil {
		return fmt.Errorf("write SymbolFTS tsv: %w", err)
	}
	// HEADER=false maps columns by position (no chance of a
	// header-name mismatch silently dropping rows). DELIM='\t'
	// because Ladybug's CSV parser does not handle RFC-4180-style
	// quoted strings containing commas — same convention the
	// Node / Edge COPY paths use. Tokens never contain tabs (we
	// strip them in writeSymbolFTSTSV) so this is safe.
	copyQ := fmt.Sprintf("COPY SymbolFTS FROM '%s' (HEADER=false, DELIM='\\t')", escapeCypherStringLit(path))
	if err := runCypherSafe(s, copyQ); err != nil {
		return fmt.Errorf("copy SymbolFTS: %w", err)
	}
	// Bulk-load invalidated the prior index; force a rebuild on
	// next SearchSymbols.
	s.fts.indexBuilt.Store(false)
	return nil
}

// writeSymbolFTSTSV writes items to a tab-separated file in
// (id, tokens) order. Tabs / newlines in tokens are normalised to
// spaces so the COPY parser doesn't misalign rows.
func writeSymbolFTSTSV(path string, items []graph.SymbolFTSItem) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var b strings.Builder
	clean := func(s string) string {
		// Strip / replace TSV-toxic characters. Replace tabs and
		// newlines with spaces; collapse runs of whitespace later
		// if needed (FTS tokeniser already splits on whitespace
		// so consecutive spaces are harmless).
		if !strings.ContainsAny(s, "\t\r\n") {
			return s
		}
		r := strings.NewReplacer("\t", " ", "\r", " ", "\n", " ")
		return r.Replace(s)
	}
	for _, it := range items {
		b.Reset()
		b.WriteString(clean(it.NodeID))
		b.WriteByte('\t')
		b.WriteString(clean(it.Tokens))
		b.WriteByte('\n')
		if _, err := f.WriteString(b.String()); err != nil {
			return err
		}
	}
	return nil
}

// BuildSymbolIndex creates the FTS index over SymbolFTS.tokens.
// Idempotent — the second call is a no-op via the indexBuilt
// sentinel. Ladybug auto-updates the index on later inserts /
// updates to the underlying table, so this is a one-shot
// cold-start call and the daemon's incremental writes (a file
// change triggering a re-parse) don't need to drop and rebuild.
//
// Must be called AFTER the SymbolFTS table has at least one row,
// because CREATE_FTS_INDEX scans the table to build the index. An
// empty table makes the index trivially empty but still valid; a
// subsequent UpsertSymbolFTS will land on it.
func (s *Store) BuildSymbolIndex() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.fts.indexBuilt.Load() {
		return nil
	}
	if err := s.ensureFTSExtensionLocked(); err != nil {
		return err
	}
	// CREATE_FTS_INDEX is fatal if the index already exists, so guard
	// it with a DROP first. The DROP is also fatal if the index
	// doesn't exist, so swallow that case. Net effect: idempotent
	// build with at most one extra catalog round-trip on the first
	// call.
	_ = runCypherSafe(s, fmt.Sprintf(`CALL DROP_FTS_INDEX('SymbolFTS', '%s')`, ftsIndexName))
	const ddl = `CALL CREATE_FTS_INDEX('SymbolFTS', '%s', ['tokens'])`
	if err := runCypherSafe(s, fmt.Sprintf(ddl, ftsIndexName)); err != nil {
		return fmt.Errorf("create fts index: %w", err)
	}
	s.fts.indexBuilt.Store(true)
	return nil
}

// SearchSymbols runs a full-text query against the SymbolFTS index
// and returns the hits ordered by descending BM25 score. The query
// is pre-tokenised by internal/search.TokenizeQuery and re-joined
// with spaces, so a camelCase query (`getUserById`) matches the
// same way a space-separated query (`get user by id`) would —
// matching the recall contract our existing BM25 backend gives.
//
// If the index hasn't been built yet (BuildSymbolIndex not called),
// this attempts to build it lazily on the first query so a daemon
// process that came up before the index landed still serves search
// correctly.
func (s *Store) SearchSymbols(query string, limit int) ([]graph.SymbolHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	// Tokenise on the read side using the SAME splitter as the
	// write side (search.Tokenize). Symmetry matters: the corpus
	// has `ValidateToken` stored as [validate, token], so a
	// user-typed `ValidateToken` query must also split to
	// [validate, token] to land. search.TokenizeQuery would NOT
	// split camelCase (it preserves short tokens at the cost of
	// camelCase recall), which produces a single `validatetoken`
	// token that misses the split corpus.
	tokens := search.Tokenize(query)
	if len(tokens) == 0 {
		// Fallback: when Tokenize drops everything (e.g. query is a
		// single sub-2-char token like "go" / "js"), use the
		// query-tokeniser's looser policy so the search still
		// reaches the engine instead of silently returning empty.
		tokens = search.TokenizeQuery(query)
		if len(tokens) == 0 {
			return nil, nil
		}
	}
	q := strings.Join(tokens, " ")

	// Lazy build: if the index isn't there yet, try to create it
	// now. Failure is non-fatal — we just return no results.
	if !s.fts.indexBuilt.Load() {
		if err := s.BuildSymbolIndex(); err != nil {
			return nil, err
		}
	}
	const cypher = `
CALL QUERY_FTS_INDEX('SymbolFTS', '` + ftsIndexName + `', $q)
RETURN node.id AS id, score
ORDER BY score DESC
LIMIT $k`
	rows, err := querySelectSafe(s, cypher, map[string]any{
		"q": q,
		"k": int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("query fts: %w", err)
	}
	hits := make([]graph.SymbolHit, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		id, _ := row[0].(string)
		if id == "" {
			continue
		}
		score, _ := row[1].(float64)
		hits = append(hits, graph.SymbolHit{NodeID: id, Score: score})
	}
	return hits, nil
}

// SearchSymbolBundles is the rerank-shaped fast path: in one BM25
// fan-out we return the matched node, its score, AND the in/out
// edges the rerank pipeline reads from. The engine routes through
// this method when the backend implements graph.SymbolBundleSearcher,
// pre-seeding rerank.Context's edge caches so the prepare pass skips
// its own batched fetch.
//
// Implementation cost: one FTS Cypher + three batched MATCH-by-ids
// Cypher calls (nodes, outEdges, inEdges) — four cgo round-trips
// total. The prior search path was 1 FTS + 1 nodes-by-ids + 2 edge
// fetches inside the rerank prepare (also 4 cgo, but they live in
// separate timing phases so the cost compounds across the engine
// → rerank boundary). Probe (see bench/ladybug-bundle-probe):
//
//	NewServer (30 hits)         med=87.4ms
//	handleStreamable (30 hits)  med=89.5ms
//	daemon controller (19 hits) med=67.8ms
//
// vs the single-shot combined-Cypher candidate (OPTIONAL MATCH +
// collect twice), which clocked 150-185ms median because Kuzu
// materialises a cross-product between the two collect frames.
//
// Idempotent on a fresh DB: lazy-builds the FTS index if it isn't
// present yet (matching SearchSymbols's behaviour) so a daemon
// process that came up before BuildSymbolIndex finished still serves
// search correctly.
func (s *Store) SearchSymbolBundles(query string, limit int) ([]graph.SymbolBundle, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	tokens := search.Tokenize(query)
	if len(tokens) == 0 {
		tokens = search.TokenizeQuery(query)
		if len(tokens) == 0 {
			return nil, nil
		}
	}
	q := strings.Join(tokens, " ")

	if !s.fts.indexBuilt.Load() {
		if err := s.BuildSymbolIndex(); err != nil {
			return nil, err
		}
	}
	// Phase 1: FTS yields (id, score) ordered by score descending. Skip
	// the round-trip when the query degenerates to no tokens (handled
	// above) — leaving this on the hot path so an empty corpus + empty
	// index returns cleanly.
	const ftsCypher = `
CALL QUERY_FTS_INDEX('SymbolFTS', '` + ftsIndexName + `', $q)
RETURN node.id AS id, score
ORDER BY score DESC
LIMIT $k`
	ftsRows, err := querySelectSafe(s, ftsCypher, map[string]any{
		"q": q,
		"k": int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("query fts: %w", err)
	}
	if len(ftsRows) == 0 {
		return nil, nil
	}

	// Preserve FTS order — the BM25 score determines TextRank, which
	// the rerank pipeline reads. Build a parallel id list and a
	// score map keyed by id for the join step.
	ids := make([]string, 0, len(ftsRows))
	scoreByID := make(map[string]float64, len(ftsRows))
	for _, row := range ftsRows {
		if len(row) < 2 {
			continue
		}
		id, _ := row[0].(string)
		if id == "" {
			continue
		}
		score, _ := row[1].(float64)
		if _, dup := scoreByID[id]; dup {
			// FTS returns each node once for a given query, but defend
			// against future configurations that might not — first hit
			// keeps the score / position.
			continue
		}
		scoreByID[id] = score
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Phase 2: batched node materialise.
	nodes := s.GetNodesByIDs(ids)

	// Phase 3 + 4: batched in/out edge fetch keyed on the same ids.
	// These two are siblings of GetNodesByIDs in terms of cgo cost;
	// the bundle's value is that the engine sees a single result it
	// can hand straight to the rerank pipeline without round-tripping
	// back through Graph for prepare's edge fetch.
	out := s.GetOutEdgesByNodeIDs(ids)
	in := s.GetInEdgesByNodeIDs(ids)

	bundles := make([]graph.SymbolBundle, 0, len(ids))
	for _, id := range ids {
		n := nodes[id]
		if n == nil {
			// FTS hit references a node that was evicted between the
			// FTS call and the node fetch — skip; the caller does its
			// own dedup / kind filter anyway.
			continue
		}
		bundles = append(bundles, graph.SymbolBundle{
			Node:     n,
			Score:    scoreByID[id],
			OutEdges: out[id],
			InEdges:  in[id],
		})
	}
	return bundles, nil
}

// runCypherSafe wraps the panicking runWriteLocked helper and
// returns any runtime / catalog error as a normal Go error so the
// FTS bootstrap can react to (and report) failures instead of
// taking down the process.
func runCypherSafe(s *Store, query string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	s.runWriteLocked(query, nil)
	return nil
}

func runCypherWithArgs(s *Store, query string, args map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	s.runWriteLocked(query, args)
	return nil
}

func querySelectSafe(s *Store, query string, args map[string]any) (rows [][]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	rows = s.querySelectLocked(query, args)
	return rows, nil
}
