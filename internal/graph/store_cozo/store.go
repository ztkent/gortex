//go:build cozo


// Package store_cozo is the CozoDB-backed implementation of
// graph.Store. CozoDB is an embedded transactional relational +
// graph + vector database with a Datalog query language. The Go
// binding (github.com/cozodb/cozo-lib-go) wraps the cozo_c C API.
//
// Datalog is a strict superset of relational algebra and SQL,
// well-suited for code-graph queries — CodeQL uses Datalog for the
// same reason. The wire-format is JSON for both inputs (parameters
// as JSON map) and outputs (NamedRows with [][]any rows).
//
// Schema is two relations: `node` keyed by id, and `edge` keyed by
// the composite (from_id, to_id, kind, file_path, line) tuple.
package store_cozo

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	cozo "github.com/cozodb/cozo-lib-go"

	"github.com/zzet/gortex/internal/graph"
)

// Store is the CozoDB-backed graph.Store implementation.
type Store struct {
	db cozo.CozoDB

	// writeMu serialises every mutation. Cozo's internal locking is
	// per-relation; Go-side serialisation keeps the per-batch
	// semantics predictable under the conformance suite's 8-goroutine
	// concurrency test.
	writeMu sync.Mutex

	// resolveMu — see graph.Store.ResolveMutex contract.
	resolveMu sync.Mutex

	edgeIdentityRevs atomic.Int64
}

// Compile-time assertion: *Store satisfies graph.Store.
var _ graph.Store = (*Store)(nil)

// Open opens (or creates) a CozoDB at path using the rocksdb engine.
// Pass ":memory:" for an in-memory store.
func Open(path string) (*Store, error) {
	engine := "rocksdb"
	if path == ":memory:" || path == "" {
		engine = "mem"
		path = ""
	}
	db, err := cozo.New(engine, path, cozo.Map{})
	if err != nil {
		return nil, fmt.Errorf("store_cozo: open %q: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.applySchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store_cozo: schema: %w", err)
	}
	return s, nil
}

// Close closes the underlying CozoDB.
func (s *Store) Close() error {
	s.db.Close()
	return nil
}

func (s *Store) ResolveMutex() *sync.Mutex { return &s.resolveMu }

// applySchema creates the node + edge relations idempotently.
func (s *Store) applySchema() error {
	const nodeDDL = `:create node {
    id: String =>
    kind: String,
    name: String,
    qual_name: String,
    file_path: String,
    start_line: Int,
    end_line: Int,
    language: String,
    repo_prefix: String,
    workspace_id: String,
    project_id: String,
    absolute_file_path: String,
    meta: String
}`
	const edgeDDL = `:create edge {
    from_id: String,
    to_id: String,
    kind: String,
    file_path: String,
    line: Int =>
    confidence: Float,
    confidence_label: String,
    origin: String,
    tier: String,
    cross_repo: Bool,
    meta: String
}`
	for _, q := range []string{nodeDDL, edgeDDL} {
		if _, err := s.db.Run(q, cozo.Map{}); err != nil {
			// :create fails if the relation already exists; ignore so
			// re-opens of an existing on-disk path stay idempotent.
			if !strings.Contains(err.Error(), "already exists") &&
				!strings.Contains(err.Error(), "already in use") {
				return fmt.Errorf("schema %q: %w", firstLine(q), err)
			}
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// encodeMeta serialises Meta to a base64-encoded gob frame. Cozo
// strings are byte-safe but the JSON wire we use to send parameters
// is not; base64 sidesteps any encoding concerns at the JSON boundary.
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

func decodeMeta(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// nodeToRow returns the per-row tuple matching the node schema's
// column order (id, kind, name, qual_name, file_path, start_line,
// end_line, language, repo_prefix, workspace_id, project_id,
// absolute_file_path, meta).
func nodeToRow(n *graph.Node) ([]any, error) {
	metaStr, err := encodeMeta(n.Meta)
	if err != nil {
		return nil, err
	}
	return []any{
		n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath,
		n.StartLine, n.EndLine, n.Language, n.RepoPrefix, n.WorkspaceID,
		n.ProjectID, n.AbsoluteFilePath, metaStr,
	}, nil
}

// edgeToRow returns the per-row tuple matching the edge schema's
// column order (from_id, to_id, kind, file_path, line, confidence,
// confidence_label, origin, tier, cross_repo, meta).
func edgeToRow(e *graph.Edge) ([]any, error) {
	metaStr, err := encodeMeta(e.Meta)
	if err != nil {
		return nil, err
	}
	return []any{
		e.From, e.To, string(e.Kind), e.FilePath, e.Line,
		e.Confidence, e.ConfidenceLabel, e.Origin, e.Tier, e.CrossRepo, metaStr,
	}, nil
}

// rowToNode reconstructs a *Node from a NamedRows row.
func rowToNode(r []any) *graph.Node {
	if len(r) < 13 {
		return nil
	}
	n := &graph.Node{
		ID:               asString(r[0]),
		Kind:             graph.NodeKind(asString(r[1])),
		Name:             asString(r[2]),
		QualName:         asString(r[3]),
		FilePath:         asString(r[4]),
		StartLine:        asInt(r[5]),
		EndLine:          asInt(r[6]),
		Language:         asString(r[7]),
		RepoPrefix:       asString(r[8]),
		WorkspaceID:      asString(r[9]),
		ProjectID:        asString(r[10]),
		AbsoluteFilePath: asString(r[11]),
	}
	if metaStr := asString(r[12]); metaStr != "" {
		if m, err := decodeMeta(metaStr); err == nil {
			n.Meta = m
		}
	}
	return n
}

// rowToEdge reconstructs an *Edge from a NamedRows row.
func rowToEdge(r []any) *graph.Edge {
	if len(r) < 11 {
		return nil
	}
	e := &graph.Edge{
		From:            asString(r[0]),
		To:              asString(r[1]),
		Kind:            graph.EdgeKind(asString(r[2])),
		FilePath:        asString(r[3]),
		Line:            asInt(r[4]),
		Confidence:      asFloat(r[5]),
		ConfidenceLabel: asString(r[6]),
		Origin:          asString(r[7]),
		Tier:            asString(r[8]),
		CrossRepo:       asBool(r[9]),
	}
	if metaStr := asString(r[10]); metaStr != "" {
		if m, err := decodeMeta(metaStr); err == nil {
			e.Meta = m
		}
	}
	return e
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// -- BulkLoader implementation -------------------------------------------

// Compile-time assertion: *Store satisfies graph.BulkLoader. AddBatch
// already batches via :put with multi-row $rows; this marker enables
// the indexer's shadow swap, which replaces ~2000 per-file AddBatch
// calls with one AddBatch on the full graph at the end.
var _ graph.BulkLoader = (*Store)(nil)

func (s *Store) BeginBulkLoad()    {}
func (s *Store) FlushBulk() error  { return nil }
