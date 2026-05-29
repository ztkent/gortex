package store_ladybug

import (
	"path/filepath"
	"testing"

	lbug "github.com/LadybugDB/go-ladybug"
)

func openMigrateTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.lbug"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// addCol returns an apply func that runs one DDL statement on the conn.
func addCol(ddl string) func(*lbug.Connection) error {
	return func(c *lbug.Connection) error {
		res, err := c.Query(ddl)
		if err != nil {
			return err
		}
		res.Close()
		return nil
	}
}

// mustExec runs a Cypher statement on the conn and fails the test on error.
func mustExec(t *testing.T, conn *lbug.Connection, q string) {
	t.Helper()
	res, err := conn.Query(q)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	res.Close()
}

// failIfCalled returns an apply func that fails the test if the version
// gate ever lets it run.
func failIfCalled(t *testing.T) func(*lbug.Connection) error {
	return func(*lbug.Connection) error {
		t.Error("a gated migration step ran when it should have been skipped")
		return nil
	}
}

// A fresh Open stamps the current version and never needs a rebuild.
func TestSchemaVersion_FreshOpenStampsCurrent(t *testing.T) {
	s := openMigrateTestStore(t)
	v, ok, err := readSchemaVersion(s.conn)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if !ok {
		t.Fatal("fresh open left no schema_version row")
	}
	if v != currentSchemaVersion {
		t.Fatalf("schema_version = %d, want currentSchemaVersion %d", v, currentSchemaVersion)
	}
	if s.NeedsRebuild() {
		t.Fatal("fresh open reported NeedsRebuild() = true")
	}
}

// The stamped version survives close/reopen (the daemon-restart path,
// which is the whole reason it is persisted), and a reopen neither
// re-migrates nor flags a rebuild.
func TestSchemaVersion_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lbug")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	v1, _, _ := readSchemaVersion(s1.conn)
	if err := s1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	v2, ok, err := readSchemaVersion(s2.conn)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if !ok || v2 != v1 || v2 != currentSchemaVersion {
		t.Fatalf("version after reopen = %d (ok=%v), want %d (== first open %d)", v2, ok, currentSchemaVersion, v1)
	}
	if s2.NeedsRebuild() {
		t.Fatal("reopen reported NeedsRebuild() = true")
	}
}

// An additive ALTER step runs and the version advances; re-running is a
// no-op (the version gate skips already-applied steps).
func TestMigrateSchema_AdditiveStepThenGate(t *testing.T) {
	s := openMigrateTestStore(t) // starts at version 1

	steps := []migrationStep{
		{to: 2, apply: addCol("ALTER TABLE Node ADD IF NOT EXISTS probe_owner STRING")},
	}
	rebuild, err := migrateSchema(s.conn, 2, steps)
	if err != nil {
		t.Fatalf("migrate to v2: %v", err)
	}
	if rebuild {
		t.Fatal("additive step reported needsRebuild = true")
	}
	if v, _, _ := readSchemaVersion(s.conn); v != 2 {
		t.Fatalf("after migrate, version = %d, want 2", v)
	}
	// The column must now exist (referencing it must not error).
	if res, err := s.conn.Query("MATCH (n:Node) RETURN n.probe_owner LIMIT 1"); err != nil {
		t.Fatalf("new column probe_owner not queryable: %v", err)
	} else {
		res.Close()
	}

	// Re-run at the same target with a step whose apply MUST NOT fire —
	// stored (2) is not < to (2), so the gate skips it.
	gate := []migrationStep{
		{to: 2, apply: func(*lbug.Connection) error {
			t.Error("already-applied step re-ran (version gate failed)")
			return nil
		}},
	}
	if _, err := migrateSchema(s.conn, 2, gate); err != nil {
		t.Fatalf("gate re-run: %v", err)
	}
}

// A pre-versioning DB (no schema_version row) that has only SIDECAR data
// — an empty Node table but a populated FileMtime — must be classed as the
// v1 baseline, not as fresh/current, so a v1->v2 rebuild step still fires.
// Guards against probing Node alone (FileMtime has an independent write
// path and can outlive Node).
func TestMigrateSchema_PreVersioningSidecarOnly(t *testing.T) {
	s := openMigrateTestStore(t)
	// Sidecar row present, Node empty, schema_version row removed →
	// indistinguishable from a real pre-SchemaMeta database.
	mustExec(t, s.conn, "MERGE (m:FileMtime {file_id: 'f1'}) SET m.mtime_ns = 1")
	mustExec(t, s.conn, "MATCH (m:SchemaMeta) DELETE m")

	rebuild, err := migrateSchema(s.conn, 2, []migrationStep{
		{to: 1, apply: failIfCalled(t)}, // to <= stored(1) → must be skipped
		{to: 2, rebuild: true},          // to > stored(1) → must fire
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !rebuild {
		t.Fatal("sidecar-only pre-versioning DB misclassified as fresh; the v2 rebuild step was skipped")
	}
	if v, _, _ := readSchemaVersion(s.conn); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
}

// A genuinely fresh/empty DB (no schema_version row, no data in any table)
// is born at the current version, so a rebuild step must NOT fire.
func TestMigrateSchema_FreshEmptyDBSkipsRebuild(t *testing.T) {
	s := openMigrateTestStore(t)
	mustExec(t, s.conn, "MATCH (m:SchemaMeta) DELETE m") // simulate no version row; all data tables empty

	rebuild, err := migrateSchema(s.conn, 2, []migrationStep{{to: 2, rebuild: true}})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rebuild {
		t.Fatal("fresh empty DB wrongly fired a rebuild step (should be born at current version)")
	}
	if v, _, _ := readSchemaVersion(s.conn); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
}

// A rebuild step sets needsRebuild and still advances the version, while a
// preceding additive step on the same ladder run also applies.
func TestMigrateSchema_RebuildStep(t *testing.T) {
	s := openMigrateTestStore(t) // version 1

	steps := []migrationStep{
		{to: 2, apply: addCol("ALTER TABLE Node ADD IF NOT EXISTS probe_x STRING")},
		{to: 3, rebuild: true},
	}
	rebuild, err := migrateSchema(s.conn, 3, steps)
	if err != nil {
		t.Fatalf("migrate to v3: %v", err)
	}
	if !rebuild {
		t.Fatal("rebuild step did not set needsRebuild")
	}
	if v, _, _ := readSchemaVersion(s.conn); v != 3 {
		t.Fatalf("after migrate, version = %d, want 3", v)
	}
}
