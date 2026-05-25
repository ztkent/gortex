// Package store_ladybug is the KuzuDB-backed implementation of
// graph.Store. KuzuDB is an embedded property-graph database with a
// Cypher front-end and a columnar storage engine. The Go binding
// (github.com/LadybugDB/go-ladybug) wraps the C API and bundles
// liblbug.dylib / liblbug.so for the host platform.
//
// Schema design — one Node table and one Edge rel table parameterised
// by the `kind` column. We deliberately do not spread the ~50 edge
// kinds across 50 rel tables: every kind would need its own DDL,
// every schema query would multiplex across them, and KuzuDB rel
// tables do not share an identity column. A single Edge table keeps
// the schema small enough to evolve incrementally.
//
// Meta payloads are gob-encoded and base64-encoded, then stored as a
// STRING column. The native BLOB type is technically supported by the
// engine, but the Go binding reads a BLOB by calling strlen() on the
// returned C pointer, which truncates at the first NUL byte — gob
// frames contain arbitrary binary including NUL, so a BLOB column
// would silently lose data. base64 sidesteps both the strlen issue
// and the missing `[]byte → BLOB` parameter coercion (a raw `[]byte`
// is currently bound as `UINT8[]`, which the binder rejects against a
// BLOB column).
package store_ladybug

// schemaDDL is the list of Cypher statements applied on every Open
// call. CREATE … IF NOT EXISTS makes the DDL idempotent so an
// existing on-disk database opens cleanly.
//
// PRIMARY KEY on Node(id) gives us the AddNode-by-id idempotency
// contract for free — a duplicate INSERT would raise a runtime
// uniqueness violation, so writes go through MERGE … SET … which
// upserts in one shot. KuzuDB rel tables do not allow a primary key,
// so Edge dedup is enforced at the Go layer (MERGE on the
// (from, to, kind, file_path, line) tuple).
var schemaDDL = []string{
	`CREATE NODE TABLE IF NOT EXISTS Node(
        id            STRING,
        kind          STRING,
        name          STRING,
        qual_name     STRING,
        file_path     STRING,
        start_line    INT64,
        end_line      INT64,
        language      STRING,
        repo_prefix   STRING,
        workspace_id  STRING,
        project_id    STRING,
        meta          STRING,
        PRIMARY KEY(id)
    )`,
	`CREATE REL TABLE IF NOT EXISTS Edge(
        FROM Node TO Node,
        kind             STRING,
        file_path        STRING,
        line             INT64,
        confidence       DOUBLE,
        confidence_label STRING,
        origin           STRING,
        tier             STRING,
        cross_repo       INT64,
        meta             STRING
    )`,
	// SymbolFTS is the sidecar table the native FTS index is built
	// against. Kept separate from Node so we don't have to touch
	// every read/write path on the main schema, and so the
	// search-side tokenisation (camelCase / snake_case / path-segment
	// splits — see internal/search/tokenizer.go) lives in a clearly
	// search-shaped column instead of polluting Node.
	//
	// id is the foreign anchor back to Node.id; tokens is the
	// space-separated pre-tokenised text that the FTS index
	// matches against. PRIMARY KEY on id makes the per-node
	// UpsertSymbolFTS MERGE call idempotent (re-indexing a file
	// during incremental updates replaces the prior row in place).
	`CREATE NODE TABLE IF NOT EXISTS SymbolFTS(
        id     STRING,
        tokens STRING,
        PRIMARY KEY(id)
    )`,
}
