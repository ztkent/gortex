package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// This file implements graph.SymbolSearcher + graph.SymbolBundleSearcher
// on the SQLite backend using the FTS5 virtual table declared in
// schema.go (symbol_fts). It is the on-disk replacement for the
// multi-GB in-heap Bleve/BM25 index: the FTS5 inverted index lives in
// the same .sqlite file as the graph, and a tier-0 exact-name boost
// (mirroring the Ladybug backend) short-circuits identifier queries so
// search quality holds or improves while the heap shrinks.
//
// Semantics mirror internal/graph/store_ladybug/fts.go:
//
//   - BulkUpsertSymbolFTS wipes only the rows owned by repoPrefix
//     before re-inserting, so sibling repos sharing one store don't
//     clobber each other's corpus. Empty prefix wipes the whole table
//     (single-repo / conformance behaviour).
//
//   - SearchSymbols tier 0: an identifier query (no whitespace / path
//     separators) that resolves to one or more nodes by exact name is
//     returned directly with a fixed dominant score, skipping FTS.
//     Misses fall through to the FTS5 MATCH path.
//
//   - SearchSymbolBundles composes the same hit list with batched
//     node + in/out edge fetches the rerank pipeline reads from.
//
// FTS5 maintains its index incrementally on every insert, so the
// Store struct needs no extra state and BuildSymbolIndex is a no-op
// (it only opportunistically merges segments).

// Compile-time assertions: *Store satisfies the symbol-search
// capabilities. The indexer auto-engages these when the active backend
// implements them, routing search_symbols through on-disk FTS5 instead
// of the in-process BM25 index.
var (
	_ graph.SymbolSearcher       = (*Store)(nil)
	_ graph.SymbolBundleSearcher = (*Store)(nil)
)

// ftsInsertChunkRows bounds the rows per multi-row INSERT. Each row
// binds 3 host params (node_id, repo_prefix, tokens); 300 rows is 900
// params, comfortably under SQLite's default 999-variable limit so the
// statement stays portable across builds.
const ftsInsertChunkRows = 300

// UpsertSymbolFTS records (or replaces) the pre-tokenised text for
// nodeID. FTS5 offers no UPSERT on a table with UNINDEXED columns, so
// the write is delete-then-insert: drop any prior row for nodeID, then
// insert the new tokens. The repo_prefix is derived from the owning
// node (nodes.repo_prefix) so the per-repo staleness wipe in
// BulkUpsertSymbolFTS can scope by prefix; if the node is absent the
// prefix defaults to "".
func (s *Store) UpsertSymbolFTS(nodeID, tokens string) error {
	if nodeID == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var repoPrefix string
	row := s.db.QueryRow(`SELECT repo_prefix FROM nodes WHERE id = ?`, nodeID)
	// A missing node (or a scan error) leaves repoPrefix == "" — the
	// row is still indexable, it just won't be reachable by a per-repo
	// prefix wipe. The graph.Store contract has no error channel for
	// the indexer's incremental writes, so we don't surface this.
	_ = row.Scan(&repoPrefix)

	if _, err := s.db.Exec(`DELETE FROM symbol_fts WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES (?, ?, ?)`,
		nodeID, repoPrefix, tokens,
	); err != nil {
		return err
	}
	return nil
}

// BulkUpsertSymbolFTS is the cold-start fast path: wipe this repo's
// stale rows, then chunked multi-row INSERT of the deduped items. The
// whole thing runs in one transaction under writeMu so a concurrent
// reader never observes the table mid-wipe.
//
// repoPrefix scopes the pre-insert wipe exactly like the Ladybug
// backend: a non-empty prefix deletes only rows owned by that repo,
// leaving siblings untouched; an empty prefix wipes the whole table
// (single-repo / conformance behaviour — the conformance suite calls
// this with ""). Items are deduped by NodeID with last-write-wins,
// matching UpsertSymbolFTS's replace semantics.
func (s *Store) BulkUpsertSymbolFTS(repoPrefix string, items []graph.SymbolFTSItem) error {
	if len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Dedup by ID — last write wins, mirroring UpsertSymbolFTS's
	// delete-then-insert. Guards the edge case where a re-parse of a
	// file emitted the same ID twice.
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

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	commit := false
	defer func() {
		if !commit {
			_ = tx.Rollback()
		}
	}()

	// Wipe this repo's prior rows so a clean rebuild of repo A doesn't
	// leave phantom hits, while sibling repo B's corpus survives. The
	// repo_prefix column is UNINDEXED but still stored, so the equality
	// filter is a literal compare over the row set. Empty repoPrefix
	// clears the whole table — the legacy single-repo wipe.
	if _, err := tx.Exec(`DELETE FROM symbol_fts WHERE repo_prefix = ?`, repoPrefix); err != nil {
		return err
	}

	for start := 0; start < len(items); start += ftsInsertChunkRows {
		end := minInt(start+ftsInsertChunkRows, len(items))
		chunk := items[start:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES `)
		args := make([]any, 0, len(chunk)*3)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?,?,?)`)
			args = append(args, it.NodeID, repoPrefix, it.Tokens)
		}
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	commit = true
	return nil
}

// BuildSymbolIndex is a no-op for FTS5: the index is maintained
// incrementally on every insert, so there is nothing to build after the
// bulk parse phase. We opportunistically run the FTS5 'optimize'
// command to merge segments (purely a read-latency improvement); any
// error is ignored because the index is already correct without it.
// Idempotent — safe to call any number of times.
func (s *Store) BuildSymbolIndex() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO symbol_fts(symbol_fts) VALUES('optimize')`)
	return nil
}

// SearchSymbols runs a symbol query and returns hits ordered by
// descending relevance (higher Score = more relevant).
//
// Tier 0 (exact-name boost, mirroring the Ladybug backend): when the
// query looks like a literal identifier and resolves to one or more
// nodes by exact name, return those directly with a fixed dominant
// score (100.0) — an O(1)-ish index seek that beats FTS ranking for
// the common "type the symbol name" case. Misses fall through to FTS5.
//
// Otherwise tokenise on the read side with the SAME splitter as the
// write side (search.Tokenize) so a camelCase query lands on the
// split corpus, build a prefix-OR MATCH expression, and rank by BM25.
// SQLite's bm25() returns lower-is-better, so the stored Score is its
// negation (higher-is-better, matching the SymbolHit contract).
func (s *Store) SearchSymbols(query string, limit int) ([]graph.SymbolHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	// Tier 0: exact-name lookup. Only engage for identifier-shaped
	// queries (no whitespace / path separators); multi-word queries are
	// concept searches that need BM25 ranking. We only short-circuit
	// when the lookup hits at least one node — misses fall through so a
	// partial-identifier query still reaches FTS.
	if isIdentifierQuery(query) {
		ns := s.FindNodesByName(query)
		if len(ns) > 0 {
			out := make([]graph.SymbolHit, 0, minInt(len(ns), limit))
			for _, n := range ns {
				if n == nil || n.ID == "" {
					continue
				}
				out = append(out, graph.SymbolHit{NodeID: n.ID, Score: 100.0})
				if len(out) >= limit {
					break
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	match := s.buildFTSMatch(query)
	if match == "" {
		return nil, nil
	}

	const q = `SELECT node_id, bm25(symbol_fts) FROM symbol_fts WHERE symbol_fts MATCH ? ORDER BY bm25(symbol_fts) LIMIT ?`
	rows, err := s.db.Query(q, match, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var hits []graph.SymbolHit
	for rows.Next() {
		var (
			id    string
			score float64
		)
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		// bm25() is negative-better in SQLite; negate so higher = better,
		// matching the SymbolHit contract. Rows already arrive in bm25
		// (best-first) order from the ORDER BY.
		hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// buildFTSMatch tokenises the query with the write-side splitter and
// builds an FTS5 MATCH expression: each token becomes a quoted prefix
// term ("tok"*) and the terms are OR-joined so any token match counts.
// Returns "" when the query degenerates to no tokens.
func (s *Store) buildFTSMatch(query string) string {
	tokens := search.Tokenize(query)
	if len(tokens) == 0 {
		// Fallback: when Tokenize drops everything (e.g. a single
		// sub-2-char token like "go"), use the looser query tokeniser so
		// the search still reaches the engine instead of returning empty.
		tokens = search.TokenizeQuery(query)
		if len(tokens) == 0 {
			return ""
		}
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		parts = append(parts, `"`+escapeFTSQuote(t)+`"*`)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

// escapeFTSQuote escapes a token for use inside an FTS5 double-quoted
// string literal: a literal double quote is doubled ("" inside "...").
func escapeFTSQuote(t string) string {
	return strings.ReplaceAll(t, `"`, `""`)
}

// SearchSymbolBundles is the rerank-shaped fast path: it runs
// SearchSymbols to get the ranked id list (preserving order) plus a
// score-by-id map, then materialises the nodes and their in/out edges
// in batched fetches the rerank pipeline reads from. The engine routes
// through this when the backend implements SymbolBundleSearcher,
// pre-seeding rerank.Context's edge caches.
func (s *Store) SearchSymbolBundles(query string, limit int) ([]graph.SymbolBundle, error) {
	hits, err := s.SearchSymbols(query, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(hits))
	scoreByID := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.NodeID == "" {
			continue
		}
		if _, dup := scoreByID[h.NodeID]; dup {
			// First hit keeps the score / position; defend against a
			// future ranker that returns an id more than once.
			continue
		}
		scoreByID[h.NodeID] = h.Score
		ids = append(ids, h.NodeID)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	nodes := s.GetNodesByIDs(ids)
	out := s.GetOutEdgesByNodeIDs(ids)
	in := s.GetInEdgesByNodeIDs(ids)

	bundles := make([]graph.SymbolBundle, 0, len(ids))
	for _, id := range ids {
		n := nodes[id]
		if n == nil {
			// Hit references a node evicted between the search and the
			// node fetch — skip; the caller does its own dedup / filter.
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

// isIdentifierQuery reports whether a query looks like a literal symbol
// name (no whitespace, no path separators, no dots, no colons, no
// commas). The tier-0 exact-name fast path engages only on such
// queries; multi-token / path / qualified queries always go to FTS.
// Copied from the Ladybug backend's name_index.go so the two backends
// share the identical tier-0 gate.
func isIdentifierQuery(q string) bool {
	if q == "" {
		return false
	}
	for _, r := range q {
		switch r {
		case ' ', '\t', '\n', '/', '.', ':', ',':
			return false
		}
	}
	return true
}
