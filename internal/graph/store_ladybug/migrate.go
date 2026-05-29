package store_ladybug

// Forward-only schema migration ladder for the Ladybug backend.
//
// The Node/Edge/SymbolFTS/FileMtime tables are a derived cache — every
// row is re-buildable by re-indexing — so this is deliberately NOT a
// golang-migrate / Flyway framework (no up/down files, no rollback, no
// per-instance lock table). It is the embedded-store equivalent of
// SQLite's PRAGMA user_version + a switch: read a single version int,
// apply the ordered steps above it, stamp the new version.
//
// Two kinds of step (see migrationStep):
//   - additive ALTER (ALTER TABLE ... ADD IF NOT EXISTS ...): preserves
//     the warm cache, which is the whole reason this persistence layer
//     exists. The default for anything ALTER can express. (Empirically
//     verified against liblbug v0.13.1: ADD [IF NOT EXISTS] <col> <type>
//     [DEFAULT v], DROP, and existing-row backfill all work.)
//   - rebuild: a change ALTER cannot express (a Meta-payload reshape — the
//     in-memory store holds Meta as a live map[string]any the disk backend
//     round-trips through encodeMeta, which a STRING-column ALTER cannot
//     reshape — or a table restructure). Open surfaces it via
//     NeedsRebuild() and the caller treats the cache as absent.

import (
	"fmt"

	lbug "github.com/LadybugDB/go-ladybug"
)

// currentSchemaVersion is the schema version this build expects on disk.
// Bump it by exactly one for every shipped schema change and add the
// matching migrationStep to ladybugMigrations.
//
// Version 1 is the baseline (the Node/Edge/SymbolFTS/FileMtime schema as
// of the first versioned build). Versioning was introduced without
// touching any existing table, so a database created before SchemaMeta
// existed already matches the v1 columns — applyLadybugMigrations treats
// such a DB as v1 and skips straight to stamping.
const currentSchemaVersion = 1

// migrationStep upgrades the on-disk schema TO version `to`. Steps MUST be
// listed in ascending `to` order. Exactly one of apply / rebuild is
// meaningful per step: an apply func runs additive DDL on the setup conn;
// rebuild==true means the change needs a full re-index instead.
type migrationStep struct {
	to      int
	apply   func(conn *lbug.Connection) error
	rebuild bool
}

// ladybugMigrations is the forward-only ladder. Empty until the schema
// first changes. When it does, add a step here AND (for additive changes)
// the new column to the relevant CREATE in schemaDDL, so fresh databases
// are born at the latest schema and the ADD IF NOT EXISTS step is a
// harmless no-op on them. Examples:
//
//	// Additive column — keeps the warm cache:
//	{to: 2, apply: func(c *lbug.Connection) error {
//		res, err := c.Query("ALTER TABLE Node ADD IF NOT EXISTS owner STRING")
//		if err != nil {
//			return err
//		}
//		res.Close()
//		return nil
//	}},
//	// Meta-payload reshape ALTER can't express — force a rebuild:
//	{to: 3, rebuild: true},
var ladybugMigrations []migrationStep

// applyLadybugMigrations brings the on-disk schema up to
// currentSchemaVersion using the package ladder. Called from Open on the
// raw setup connection, before the pool exists (single-threaded, no
// writeMu). Returns whether any crossed step requires a full re-index.
func applyLadybugMigrations(conn *lbug.Connection) (needsRebuild bool, err error) {
	return migrateSchema(conn, currentSchemaVersion, ladybugMigrations)
}

// migrateSchema is the testable core of applyLadybugMigrations: it takes
// the target version and step list explicitly so tests can exercise the
// ladder without mutating package globals.
func migrateSchema(conn *lbug.Connection, current int, steps []migrationStep) (needsRebuild bool, err error) {
	stored, ok, err := readSchemaVersion(conn)
	if err != nil {
		return false, err
	}
	if !ok {
		// No version row. A fresh (empty) DB is born at the current
		// schema; an existing DB predates versioning and matches the v1
		// baseline. Either way its columns are correct for that version —
		// we only need the right starting rung so later steps don't
		// re-run (additive steps are idempotent anyway, but rebuild steps
		// must NOT fire on an already-current fresh DB).
		hasData, err := dbHasPriorData(conn)
		if err != nil {
			return false, err
		}
		if hasData {
			stored = 1
		} else {
			stored = current
		}
	}
	for _, m := range steps {
		if m.to <= stored || m.to > current {
			continue
		}
		if m.rebuild {
			needsRebuild = true
			continue
		}
		if m.apply == nil {
			continue
		}
		if err := m.apply(conn); err != nil {
			return needsRebuild, fmt.Errorf("schema migration to v%d: %w", m.to, err)
		}
	}
	// Stamp the new schema version. NOTE for the first rebuild step: this
	// stamps `current` even when a rebuild rung was crossed, but the actual
	// data re-index happens LATER (the daemon forces it via NeedsRebuild at
	// warm restart — see cmd/gortex/daemon_state.go storeNeedsRebuild). A
	// crash after this stamp but before that re-index finishes would leave
	// version=current over old-shape rows. When the first rebuild migration
	// lands, make it crash-safe — e.g. defer the stamp until the daemon
	// confirms the rebuild rather than stamping here.
	if err := writeSchemaVersion(conn, current); err != nil {
		return needsRebuild, err
	}
	return needsRebuild, nil
}

// readSchemaVersion returns the stored schema_version and whether a row
// existed (a fresh or pre-versioning DB has none). Uses the WHERE-clause
// match form, not inline {k: ...}, per the ladybug read-path convention.
func readSchemaVersion(conn *lbug.Connection) (version int, ok bool, err error) {
	res, err := conn.Query("MATCH (m:SchemaMeta) WHERE m.k = 'schema_version' RETURN m.v")
	if err != nil {
		return 0, false, err
	}
	defer res.Close()
	if !res.HasNext() {
		return 0, false, nil
	}
	tup, err := res.Next()
	if err != nil {
		return 0, false, err
	}
	v, err := tup.GetValue(0)
	if err != nil {
		return 0, false, err
	}
	// SchemaMeta.v is INT64; the binding surfaces it as a Go int64.
	iv, _ := v.(int64)
	return int(iv), true, nil
}

// writeSchemaVersion upserts the schema_version row. MERGE keeps it
// idempotent (last-write-wins), mirroring the FileMtime upsert. The MERGE
// pattern requires the key inline; the integer is formatted directly (no
// injection surface — it is an int).
func writeSchemaVersion(conn *lbug.Connection, version int) error {
	res, err := conn.Query(fmt.Sprintf("MERGE (m:SchemaMeta {k: 'schema_version'}) SET m.v = %d", version))
	if err != nil {
		return err
	}
	res.Close()
	return nil
}

// dbHasPriorData reports whether the database shows any evidence of prior
// use, to tell a brand-new (empty) DB from one created before SchemaMeta
// existed. Node, FileMtime, and SymbolFTS each have INDEPENDENT write
// paths (e.g. BulkSetFileMtimes MERGEs FileMtime with no Node dependency),
// so a pre-versioning DB can carry sidecar rows even with an empty Node
// table — a repo that indexed to zero symbols, or a partial index that
// recorded mtimes first. Probing only Node would misclassify such a DB as
// fresh and stamp it current, skipping a future rebuild it needs. Edge is
// omitted on purpose: a rel row cannot exist without its endpoint Node
// rows, so Node already subsumes it.
func dbHasPriorData(conn *lbug.Connection) (bool, error) {
	for _, table := range []string{"Node", "FileMtime", "SymbolFTS"} {
		has, err := tableHasRows(conn, table)
		if err != nil {
			return false, err
		}
		if has {
			return true, nil
		}
	}
	return false, nil
}

// tableHasRows reports whether the named node table holds at least one
// row. Returns a literal (not a column) so it works for any node table
// regardless of its column names (FileMtime keys on file_id, not id).
func tableHasRows(conn *lbug.Connection, table string) (bool, error) {
	res, err := conn.Query("MATCH (n:" + table + ") RETURN 1 LIMIT 1")
	if err != nil {
		return false, err
	}
	defer res.Close()
	return res.HasNext(), nil
}

// NeedsRebuild reports whether opening the store crossed a migration rung
// ALTER could not satisfy, so the caller should treat the on-disk graph as
// stale and re-index. False on every fresh open and after purely additive
// migrations. (Wiring this into the daemon warmup path lands with the
// first rebuild-requiring migration; the ladder is empty today.)
func (s *Store) NeedsRebuild() bool { return s.needsRebuild }
