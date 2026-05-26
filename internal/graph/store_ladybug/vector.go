package store_ladybug

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// vecIndexName is the canonical name for the HNSW index built over
// SymbolVec.emb. Hard-coded because the index is internal to the
// store — callers only ever query it through SimilarTo.
const vecIndexName = "idx_symbol_vec_emb"

// vectorState tracks the per-store vector-side state: extension
// load, schema declaration (deferred until we know the dim), and
// index build sentinel.
type vectorState struct {
	extensionLoaded atomic.Bool
	dim             atomic.Int32 // 0 until the SymbolVec table is created
	indexBuilt      atomic.Bool
}

// ensureVectorExtensionLocked loads Ladybug's VECTOR extension into
// the current connection. Same dance as ensureFTSExtensionLocked
// (INSTALL + LOAD EXTENSION); idempotent via the sentinel.
//
// Held under writeMu by the caller so concurrent connections don't
// race the load.
func (s *Store) ensureVectorExtensionLocked() error {
	if s.vec.extensionLoaded.Load() {
		return nil
	}
	if err := runCypherSafe(s, `INSTALL VECTOR`); err != nil &&
		!strings.Contains(err.Error(), "is already installed") {
		// Ignore "already installed" — every fresh open re-runs
		// this and the soft failure shouldn't abort startup.
		_ = err
	}
	if err := runCypherSafe(s, `LOAD EXTENSION VECTOR`); err != nil {
		return fmt.Errorf("load vector extension: %w", err)
	}
	s.vec.extensionLoaded.Store(true)
	return nil
}

// ensureSymbolVecSchemaLocked lazily creates the SymbolVec table
// once we know the embedding dimension. Ladybug requires a
// fixed-width column (`FLOAT[N]`) declared at table-creation time
// — we can't preallocate the schema in the static DDL because
// the dim is model-dependent and only known when the first
// embedding lands. Re-creating with a different dim drops and
// re-declares the table; existing rows are wiped (a different
// embedding model means the old vectors are meaningless anyway).
//
// Held under writeMu by the caller.
func (s *Store) ensureSymbolVecSchemaLocked(dim int) error {
	if dim <= 0 {
		return fmt.Errorf("ensureSymbolVecSchema: invalid dim %d", dim)
	}
	cur := int(s.vec.dim.Load())
	if cur == dim {
		return nil
	}
	if cur != 0 {
		// Dim changed (e.g. different embedding model on this
		// fresh daemon process). Drop the existing table so the
		// FLOAT[N] column gets re-declared at the right width.
		_ = runCypherSafe(s, `DROP TABLE IF EXISTS SymbolVec`)
		s.vec.indexBuilt.Store(false)
	}
	ddl := fmt.Sprintf(
		`CREATE NODE TABLE IF NOT EXISTS SymbolVec(id STRING, emb FLOAT[%d], PRIMARY KEY(id))`,
		dim,
	)
	if err := runCypherSafe(s, ddl); err != nil {
		return fmt.Errorf("create SymbolVec schema (dim=%d): %w", dim, err)
	}
	s.vec.dim.Store(int32(dim))
	return nil
}

// UpsertEmbedding writes (or replaces) the embedding for nodeID.
// Mirrors UpsertSymbolFTS shape: per-call MERGE for incremental
// reindex; the cold-start fast path is BulkUpsertEmbeddings.
//
// Auto-creates the SymbolVec table on first call (using
// len(vec) as the declared dim). Subsequent calls with a
// different-length vec error out — callers that change embedding
// model must drop the store first.
func (s *Store) UpsertEmbedding(nodeID string, vec []float32) error {
	if nodeID == "" {
		return nil
	}
	if len(vec) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureVectorExtensionLocked(); err != nil {
		return err
	}
	// Per-call upserts must NOT auto-migrate to a new dim — that
	// would silently drop the existing corpus when one wrong-dim
	// upsert sneaks through. BulkUpsertEmbeddings is the cold-start
	// path that's allowed to wipe and re-declare. Here we either
	// match the declared dim or refuse.
	if cur := int(s.vec.dim.Load()); cur != 0 && cur != len(vec) {
		return fmt.Errorf("vector length %d does not match declared dim %d", len(vec), cur)
	}
	if err := s.ensureSymbolVecSchemaLocked(len(vec)); err != nil {
		return err
	}
	const q = `MERGE (v:SymbolVec {id: $id}) SET v.emb = $emb`
	if err := runCypherWithArgs(s, q, map[string]any{
		"id":  nodeID,
		"emb": vec,
	}); err != nil {
		return fmt.Errorf("upsert SymbolVec: %w", err)
	}
	// An upsert invalidates the prior HNSW index — Ladybug does
	// auto-update on inserts but a freshly-written vector might
	// not be visible to ANN queries until the next index rebuild.
	// Mark dirty; SimilarTo lazy-rebuilds.
	s.vec.indexBuilt.Store(false)
	return nil
}

// BulkUpsertEmbeddings is the cold-start fast path: write a TSV of
// (id, vec) pairs to a temp file and COPY FROM into SymbolVec in
// one shot. Mirrors BulkUpsertSymbolFTS for the FTS side.
//
// Wipe-and-rewrite semantics: a re-run replaces the prior corpus
// (the indexer always calls this once per IndexCtx after the
// embedding pass completes; incremental updates go through
// UpsertEmbedding which preserves prior rows).
//
// Idempotent under empty input.
func (s *Store) BulkUpsertEmbeddings(items []graph.VectorItem) error {
	if len(items) == 0 {
		return nil
	}
	dim := 0
	for _, it := range items {
		if len(it.Vec) > 0 {
			dim = len(it.Vec)
			break
		}
	}
	if dim == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureVectorExtensionLocked(); err != nil {
		return err
	}
	if err := s.ensureSymbolVecSchemaLocked(dim); err != nil {
		return err
	}

	// Dedup by ID, validate vector dim. Reject rows with the
	// wrong width up-front rather than failing the COPY mid-batch.
	pos := make(map[string]int, len(items))
	deduped := items[:0]
	for _, it := range items {
		if it.NodeID == "" || len(it.Vec) == 0 {
			continue
		}
		if len(it.Vec) != dim {
			return fmt.Errorf("vector length %d does not match batch dim %d (id %q)", len(it.Vec), dim, it.NodeID)
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

	if err := runCypherSafe(s, `MATCH (v:SymbolVec) DELETE v`); err != nil {
		return fmt.Errorf("clear SymbolVec before bulk upsert: %w", err)
	}

	dir, err := os.MkdirTemp("", "lbug-vec-bulk-")
	if err != nil {
		return fmt.Errorf("mkdir bulk tmp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Ladybug's COPY parser picks the format from the file
	// extension; `.csv` with DELIM='\t' is the convention the
	// existing Node/Edge bulk loader uses, and `.tsv` is rejected
	// at bind time with "Cannot load from file type tsv".
	path := filepath.Join(dir, "symbolvec.csv")
	if err := writeSymbolVecTSV(path, items); err != nil {
		return fmt.Errorf("write SymbolVec tsv: %w", err)
	}
	copyQ := fmt.Sprintf("COPY SymbolVec FROM '%s' (HEADER=false, DELIM='\\t')", escapeCypherStringLit(path))
	if err := runCypherSafe(s, copyQ); err != nil {
		return fmt.Errorf("copy SymbolVec: %w", err)
	}
	s.vec.indexBuilt.Store(false)
	return nil
}

// writeSymbolVecTSV writes items to a tab-separated file. The
// FLOAT[N] column is serialised as a Ladybug array literal
// `[v0,v1,...,vN-1]` — no surrounding quotes (the COPY parser
// reads array-shaped tokens directly when DELIM is `\t`).
func writeSymbolVecTSV(path string, items []graph.VectorItem) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var b strings.Builder
	for _, it := range items {
		b.Reset()
		b.WriteString(it.NodeID)
		b.WriteByte('\t')
		b.WriteByte('[')
		for i, v := range it.Vec {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
		}
		b.WriteByte(']')
		b.WriteByte('\n')
		if _, err := f.WriteString(b.String()); err != nil {
			return err
		}
	}
	return nil
}

// BuildVectorIndex creates the HNSW index over SymbolVec.emb. The
// dim arg must match the FLOAT[N] column the table was declared
// with; if the table doesn't exist yet, this call lazily creates
// it.
//
// Idempotent: the second call with the same dim is a no-op via
// the indexBuilt sentinel. A dim change drops and re-creates the
// schema (and invalidates the sentinel).
func (s *Store) BuildVectorIndex(dim int) error {
	if dim <= 0 {
		return fmt.Errorf("BuildVectorIndex: invalid dim %d", dim)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ensureVectorExtensionLocked(); err != nil {
		return err
	}
	if err := s.ensureSymbolVecSchemaLocked(dim); err != nil {
		return err
	}
	if s.vec.indexBuilt.Load() && int(s.vec.dim.Load()) == dim {
		return nil
	}
	// Drop-and-recreate: CREATE_VECTOR_INDEX is fatal if the
	// index already exists (same pattern as the FTS path).
	_ = runCypherSafe(s, fmt.Sprintf(`CALL DROP_VECTOR_INDEX('SymbolVec', '%s')`, vecIndexName))
	if err := runCypherSafe(s, fmt.Sprintf(`CALL CREATE_VECTOR_INDEX('SymbolVec', '%s', 'emb')`, vecIndexName)); err != nil {
		return fmt.Errorf("create vector index: %w", err)
	}
	s.vec.indexBuilt.Store(true)
	return nil
}

// SimilarTo runs a k-NN ANN query against the SymbolVec HNSW
// index. Returns hits in ascending distance order (lower =
// closer under cosine distance).
//
// If the index hasn't been built yet, this lazy-builds it using
// the query vector's length as the dim — saves callers from
// having to call BuildVectorIndex explicitly when the embedder
// has already populated SymbolVec via per-call upserts.
func (s *Store) SimilarTo(vec []float32, limit int) ([]graph.VectorHit, error) {
	if len(vec) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if !s.vec.indexBuilt.Load() {
		if err := s.BuildVectorIndex(len(vec)); err != nil {
			return nil, err
		}
	}
	if want := int(s.vec.dim.Load()); want != len(vec) {
		return nil, fmt.Errorf("query vector length %d does not match index dim %d", len(vec), want)
	}
	const cypher = `
CALL QUERY_VECTOR_INDEX('SymbolVec', '` + vecIndexName + `', $vec, $k)
RETURN node.id AS id, distance
ORDER BY distance ASC`
	rows, err := querySelectSafe(s, cypher, map[string]any{
		"vec": vec,
		"k":   int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("query vector: %w", err)
	}
	hits := make([]graph.VectorHit, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		id, _ := row[0].(string)
		if id == "" {
			continue
		}
		d, _ := row[1].(float64)
		hits = append(hits, graph.VectorHit{NodeID: id, Distance: d})
	}
	return hits, nil
}
