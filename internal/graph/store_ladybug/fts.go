package store_ladybug

import (
	"fmt"
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
