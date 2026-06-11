package store_sqlite

// schemaSQL is the canonical DDL applied on Open. Statements are
// idempotent (IF NOT EXISTS) so they run cleanly against a fresh DB
// and against an existing one.
//
// Schema choices
//
//   - nodes.id is the primary key; INSERT OR REPLACE on the id column
//     gives idempotent re-adds with last-write-wins on every other
//     column, matching the in-memory store's behaviour.
//
//   - edges has a synthetic INTEGER PRIMARY KEY plus a UNIQUE
//     constraint over (from_id, to_id, kind, file_path, line) -- the
//     logical edge key the in-memory store uses for dedup. INSERT OR
//     IGNORE on that constraint matches the in-memory "second AddEdge
//     for the same key is a no-op" semantics.
//
//   - meta is a gob-encoded blob. nil / empty Meta is stored as NULL.
//
//   - Secondary indexes mirror the in-memory store's hot lookup paths:
//     nodes_by_name      -- FindNodesByName / FindNodesByNameInRepo
//     nodes_by_kind      -- Stats (group-by-kind)
//     nodes_by_file      -- GetFileNodes, EvictFile
//     nodes_by_repo      -- GetRepoNodes, RepoStats, EvictRepo
//     (partial index -- empty repo_prefix is
//     the common case and indexing it would
//     be pure overhead)
//     nodes_by_qual      -- GetNodeByQualName, unique so duplicate
//     qual_names surface as constraint errors
//     edges_by_from      -- GetOutEdges (kind included so RemoveEdge
//     can probe by (from, kind) without a
//     second hop)
//     edges_by_to        -- GetInEdges
const schemaSQL = `
CREATE TABLE IF NOT EXISTS nodes (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    qual_name     TEXT NOT NULL DEFAULT '',
    file_path     TEXT NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    language      TEXT NOT NULL DEFAULT '',
    repo_prefix   TEXT NOT NULL DEFAULT '',
    workspace_id  TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    meta          BLOB
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS nodes_by_name ON nodes(name);
CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS nodes_by_file ON nodes(file_path);
CREATE INDEX IF NOT EXISTS nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> '';
CREATE UNIQUE INDEX IF NOT EXISTS nodes_by_qual ON nodes(qual_name) WHERE qual_name <> '';

CREATE TABLE IF NOT EXISTS edges (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    meta             BLOB,
    UNIQUE(from_id, to_id, kind, file_path, line)
);

CREATE INDEX IF NOT EXISTS edges_by_from ON edges(from_id, kind);
CREATE INDEX IF NOT EXISTS edges_by_to   ON edges(to_id, kind);
-- edges_by_kind backs EdgesByKind / EdgesByKinds (resolver whole-graph
-- passes probe single kinds like provides/imports on every file save);
-- without it those are full edges-table scans — edges_by_from/to lead
-- with an id column and the partial edges_external index only covers
-- its own predicate.
CREATE INDEX IF NOT EXISTS edges_by_kind ON edges(kind);

CREATE TABLE IF NOT EXISTS file_mtimes (
    repo_prefix TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID;

-- clone_shingles is the per-symbol MinHash shingle-set sidecar. Each
-- function/method node's []uint64 shingle set is stored as a little-
-- endian BLOB (8 bytes/elem) keyed by node_id so the maintained clone-
-- detection count-min sketch can be rebuilt after a warm restart from
-- the snapshot instead of re-parsing every body. repo_prefix carries
-- the owning repo so per-repo reseeds (SELECT … WHERE repo_prefix = ?)
-- and per-repo wipes don't clobber other repos' shingle sets. node_id
-- is the PK (the join key back to nodes.id); like file_mtimes this is a
-- WITHOUT ROWID sidecar so the PK index IS the table.
CREATE TABLE IF NOT EXISTS clone_shingles (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    shingles    BLOB
) WITHOUT ROWID;

-- ref_facts is the resolved-reference sidecar: one row per reference edge
-- that resolved to a concrete target, recording the target + the provenance
-- tier that resolved it. Denormalized file_path + lang make "all reference
-- facts originating in file X" a single indexed query (the scope unit for
-- incremental re-resolution and the audit/diff surface). repo_prefix scopes
-- per-repo. PK is (repo_prefix, from_id, to_id, kind, line) so re-resolving a
-- file replaces its facts in place; WITHOUT ROWID — the PK index IS the table.
CREATE TABLE IF NOT EXISTS ref_facts (
    repo_prefix TEXT NOT NULL DEFAULT '',
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref_name    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    origin      TEXT NOT NULL DEFAULT '',
    tier        TEXT NOT NULL DEFAULT '',
    candidates  TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    lang        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, from_id, to_id, kind, line)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ref_facts_by_file ON ref_facts(repo_prefix, file_path);

CREATE TABLE IF NOT EXISTS vectors (
    node_id TEXT PRIMARY KEY,
    dims    INTEGER NOT NULL,
    vec     BLOB NOT NULL
) WITHOUT ROWID;

-- churn_enrichment is the per-node git-churn sidecar (change A: move
-- enrichment OUT of nodes.meta so the node hot path stops gob-encoding
-- rarely-read data and get_churn_rate does an indexed read instead of an
-- AllNodes+gob scan). One typed row per enriched file/function/method
-- node, keyed by node_id (join key back to nodes.id); repo_prefix scopes
-- per-repo reseeds/wipes. head_sha/branch/computed_at are file-level only
-- (empty for symbols). WITHOUT ROWID: the PK index IS the table.
CREATE TABLE IF NOT EXISTS churn_enrichment (
    node_id        TEXT PRIMARY KEY,
    repo_prefix    TEXT NOT NULL DEFAULT '',
    commit_count   INTEGER NOT NULL DEFAULT 0,
    age_days       INTEGER NOT NULL DEFAULT 0,
    churn_rate     REAL NOT NULL DEFAULT 0,
    last_author    TEXT NOT NULL DEFAULT '',
    last_commit_at TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    computed_at    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS churn_by_repo ON churn_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- coverage_enrichment: per-symbol coverage sidecar (change A). Typed
-- columns keyed by node_id; repo_prefix scopes per-repo wipes.
CREATE TABLE IF NOT EXISTS coverage_enrichment (
    node_id      TEXT PRIMARY KEY,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    coverage_pct REAL NOT NULL DEFAULT 0,
    num_stmt     INTEGER NOT NULL DEFAULT 0,
    hit          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS coverage_by_repo ON coverage_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- release_enrichment: per-file "added_in <tag>" sidecar (change A).
CREATE TABLE IF NOT EXISTS release_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    added_in    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS release_by_repo ON release_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- blame_enrichment: per-symbol latest-author sidecar (change A).
CREATE TABLE IF NOT EXISTS blame_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    ts          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS blame_by_repo ON blame_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- symbol_fts is the FTS5 full-text index over pre-tokenised symbol
-- names. It replaces the multi-GB in-heap Bleve/BM25 index with an
-- on-disk inverted index the SymbolSearcher / SymbolBundleSearcher
-- query through. A standard (NOT contentless) FTS5 table so we can
-- DELETE individual rows by node_id without an external content
-- shadow. node_id is the join key back to nodes.id; repo_prefix is
-- carried UNINDEXED so per-repo staleness wipes (DELETE … WHERE
-- repo_prefix = ?) hit a literal column without a separate b-tree.
-- Only "tokens" is indexed for matching. IF NOT EXISTS makes this
-- idempotent on every Open, so an existing .sqlite gains the vtable
-- on its next open + reindex.
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens);
`
