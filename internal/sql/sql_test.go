package sql

import (
	"testing"
)

func TestExtractTables_BasicSelect(t *testing.T) {
	refs := ExtractTables(`SELECT * FROM users WHERE id = 1`)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d: %+v", len(refs), refs)
	}
	if refs[0].Table != "users" || refs[0].Op != "select" {
		t.Errorf("got %+v", refs[0])
	}
}

func TestExtractTables_JoinClauses(t *testing.T) {
	refs := ExtractTables(`SELECT u.name FROM users u JOIN orders o ON u.id = o.user_id`)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %+v", len(refs), refs)
	}
	got := map[string]bool{}
	for _, r := range refs {
		got[r.Table] = true
	}
	if !got["users"] || !got["orders"] {
		t.Errorf("missing refs: %v", got)
	}
}

func TestExtractTables_InsertUpdateDelete(t *testing.T) {
	cases := []struct {
		query string
		op    string
		table string
	}{
		{`INSERT INTO users (id, name) VALUES (1, 'a')`, "insert", "users"},
		{`UPDATE accounts SET balance = 0 WHERE id = 1`, "update", "accounts"},
		{`DELETE FROM sessions WHERE expired = true`, "delete", "sessions"},
		{`TRUNCATE TABLE logs`, "truncate", "logs"},
		{`TRUNCATE logs2`, "truncate", "logs2"},
	}
	for _, c := range cases {
		refs := ExtractTables(c.query)
		if len(refs) != 1 {
			t.Fatalf("%q → %d refs, want 1", c.query, len(refs))
		}
		if refs[0].Op != c.op || refs[0].Table != c.table {
			t.Errorf("%q → %+v, want op=%q table=%q", c.query, refs[0], c.op, c.table)
		}
	}
}

func TestExtractTables_QuotedIdentifiers(t *testing.T) {
	cases := []string{
		`SELECT * FROM "users" WHERE id = 1`,    // ANSI
		"SELECT * FROM `users` WHERE id = 1",    // MySQL backticks
		`SELECT * FROM [users] WHERE id = 1`,    // T-SQL brackets
	}
	for _, q := range cases {
		refs := ExtractTables(q)
		if len(refs) != 1 || refs[0].Table != "users" {
			t.Errorf("%q → %+v, want table=users", q, refs)
		}
	}
}

func TestExtractTables_SchemaQualified(t *testing.T) {
	refs := ExtractTables(`SELECT * FROM public.users JOIN auth.sessions ON id = session_id`)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	got := map[string]string{} // table → schema
	for _, r := range refs {
		got[r.Table] = r.Schema
	}
	if got["users"] != "public" {
		t.Errorf("users schema = %q, want public", got["users"])
	}
	if got["sessions"] != "auth" {
		t.Errorf("sessions schema = %q, want auth", got["sessions"])
	}
}

func TestExtractTables_DeduplicatesSameOpAndTable(t *testing.T) {
	refs := ExtractTables(`SELECT * FROM users JOIN users u2 ON 1=1`)
	// Both users references share op=select, schema="" — should dedupe.
	if len(refs) != 1 {
		t.Errorf("expected dedup to single users ref, got %d", len(refs))
	}
}

func TestExtractTables_MixedOps(t *testing.T) {
	q := `WITH x AS (SELECT * FROM source)
INSERT INTO target SELECT * FROM x`
	refs := ExtractTables(q)
	// `source` (select) + `target` (insert) + `x` (select) = 3
	if len(refs) != 3 {
		t.Errorf("expected 3 refs, got %d: %+v", len(refs), refs)
	}
}

func TestExtractTables_Empty(t *testing.T) {
	if r := ExtractTables(""); len(r) != 0 {
		t.Errorf("empty query should yield no refs")
	}
	if r := ExtractTables("SELECT 1"); len(r) != 0 {
		t.Errorf("no-table query should yield no refs")
	}
}

func TestStripQuoting(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"users"`, "users"},
		{"`users`", "users"},
		{"[users]", "users"},
		{"users", "users"},
		{`"`, `"`},
	}
	for _, c := range cases {
		if got := stripQuoting(c.in); got != c.want {
			t.Errorf("stripQuoting(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitSchemaTable(t *testing.T) {
	cases := []struct {
		in           string
		schema, table string
	}{
		{"users", "", "users"},
		{"public.users", "public", "users"},
		{"db.public.users", "public", "users"}, // database segment dropped
		{`"public"."users"`, "public", "users"},
	}
	for _, c := range cases {
		s, t2 := splitSchemaTable(c.in)
		if s != c.schema || t2 != c.table {
			t.Errorf("splitSchemaTable(%q) = (%q,%q), want (%q,%q)", c.in, s, t2, c.schema, c.table)
		}
	}
}

func TestExtractCreateTables_Basic(t *testing.T) {
	src := `CREATE TABLE users (
    id BIGINT PRIMARY KEY,
    email TEXT UNIQUE
);

CREATE TABLE IF NOT EXISTS sessions (
    user_id BIGINT REFERENCES users(id),
    token TEXT
);
`
	refs := ExtractCreateTables(src)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %+v", len(refs), refs)
	}
	got := map[string]bool{}
	for _, r := range refs {
		got[r.Table] = true
		if r.Op != "create" {
			t.Errorf("op = %q, want create", r.Op)
		}
	}
	if !got["users"] || !got["sessions"] {
		t.Errorf("missing tables: %v", got)
	}
}

func TestExtractCreateTables_Variants(t *testing.T) {
	src := `
CREATE TEMPORARY TABLE t1 (id INT);
CREATE TEMP TABLE t2 (id INT);
CREATE GLOBAL TEMPORARY TABLE t3 (id INT);
CREATE UNLOGGED TABLE t4 (id INT);
CREATE TABLE IF NOT EXISTS t5 (id INT);
CREATE TABLE "quoted_name" (id INT);
CREATE TABLE auth.tokens (id INT);
`
	refs := ExtractCreateTables(src)
	if len(refs) != 7 {
		t.Fatalf("expected 7 refs, got %d: %+v", len(refs), refs)
	}
	gotSchemas := map[string]string{}
	for _, r := range refs {
		gotSchemas[r.Table] = r.Schema
	}
	if gotSchemas["tokens"] != "auth" {
		t.Errorf("auth.tokens schema = %q", gotSchemas["tokens"])
	}
	if gotSchemas["quoted_name"] != "" {
		t.Errorf("quoted-name should have no schema, got %q", gotSchemas["quoted_name"])
	}
}

func TestExtractCreateTables_DedupesSameSchemaTable(t *testing.T) {
	src := `
CREATE TABLE users (id INT);
CREATE TABLE users (id INT);
`
	refs := ExtractCreateTables(src)
	if len(refs) != 1 {
		t.Errorf("expected 1 deduped ref, got %d", len(refs))
	}
}

func TestExtractCreateTables_IgnoresAlterAndDrop(t *testing.T) {
	src := `
ALTER TABLE users ADD COLUMN name TEXT;
DROP TABLE sessions;
`
	if got := ExtractCreateTables(src); len(got) != 0 {
		t.Errorf("alter/drop should not produce CREATE refs, got %+v", got)
	}
}

func TestIsMigrationPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"db/migrate/001_create_users.sql", true},
		{"db/migrations/2024_01_init.sql", true},
		{"migrations/v1.sql", true},
		{"internal/db/migrations/init.sql", true},
		{"pkg/queries/select.sql", false},
		{"main.go", false},
		{"docs/migration_guide.md", false}, // not .sql
	}
	for _, c := range cases {
		if got := IsMigrationPath(c.path); got != c.want {
			t.Errorf("IsMigrationPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMigrationNodeID(t *testing.T) {
	if got := MigrationNodeID("db/migrate/001_init.sql"); got != "migration::db/migrate/001_init.sql" {
		t.Errorf("got %q", got)
	}
}

func TestExtractColumns_InsertWrites(t *testing.T) {
	got := ExtractColumns("INSERT INTO users (id, email, name) VALUES ($1, $2, $3)")
	wantCols := map[string]bool{"id": false, "email": false, "name": false}
	for _, c := range got {
		if c.Op != "write" {
			t.Errorf("expected write op, got %q for %q", c.Op, c.Column)
		}
		if c.Table != "users" {
			t.Errorf("expected table users, got %q", c.Table)
		}
		if _, ok := wantCols[c.Column]; ok {
			wantCols[c.Column] = true
		}
	}
	for col, found := range wantCols {
		if !found {
			t.Errorf("expected column %q in result, got %v", col, got)
		}
	}
}

func TestExtractColumns_UpdateWrites(t *testing.T) {
	got := ExtractColumns("UPDATE users SET email = $1, updated_at = NOW() WHERE id = $2")
	wantCols := map[string]bool{"email": false, "updated_at": false}
	for _, c := range got {
		if c.Op != "write" {
			continue
		}
		if _, ok := wantCols[c.Column]; ok {
			wantCols[c.Column] = true
		}
	}
	for col, found := range wantCols {
		if !found {
			t.Errorf("expected SET column %q to surface as write; got %v", col, got)
		}
	}
}

func TestExtractColumns_SingleTableSelectReads(t *testing.T) {
	got := ExtractColumns("SELECT id, email, name FROM users WHERE id = $1")
	want := map[string]bool{"id": false, "email": false, "name": false}
	for _, c := range got {
		if c.Op != "read" {
			continue
		}
		if c.Table != "users" {
			continue
		}
		if _, ok := want[c.Column]; ok {
			want[c.Column] = true
		}
	}
	for col, found := range want {
		if !found {
			t.Errorf("expected read column %q; got %v", col, got)
		}
	}
}

func TestExtractColumns_StarSelectNoColumns(t *testing.T) {
	got := ExtractColumns("SELECT * FROM users")
	for _, c := range got {
		if c.Op == "read" {
			t.Errorf("SELECT * should not produce read columns; got %v", got)
		}
	}
}

func TestExtractColumns_JoinSelectReturnsNoColumns(t *testing.T) {
	got := ExtractColumns("SELECT u.id, o.id FROM users u JOIN orders o ON o.user_id = u.id")
	for _, c := range got {
		if c.Op == "read" {
			t.Errorf("multi-table SELECT should not emit columns in v1; got %v", got)
		}
	}
}

func TestColumnNodeID(t *testing.T) {
	cases := []struct {
		dialect, schema, table, column, want string
	}{
		{"postgres", "public", "users", "email", "col::postgres::public.users.email"},
		{"", "", "users", "email", "col::generic::users.email"},
	}
	for _, c := range cases {
		if got := ColumnNodeID(c.dialect, c.schema, c.table, c.column); got != c.want {
			t.Errorf("ColumnNodeID(%q,%q,%q,%q) = %q, want %q",
				c.dialect, c.schema, c.table, c.column, got, c.want)
		}
	}
}

func TestTableNodeID(t *testing.T) {
	cases := []struct {
		dialect, schema, table, want string
	}{
		{"postgres", "public", "users", "db::postgres::public.users"},
		{"", "", "users", "db::generic::users"},
		{"mysql", "", "orders", "db::mysql::orders"},
	}
	for _, c := range cases {
		if got := TableNodeID(c.dialect, c.schema, c.table); got != c.want {
			t.Errorf("TableNodeID(%q,%q,%q) = %q, want %q",
				c.dialect, c.schema, c.table, got, c.want)
		}
	}
}

func TestProjectionColumns(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"plain columns", "SELECT id, name FROM users", []string{"id", "name"}},
		{"table-qualified", "SELECT u.id, u.name FROM users u", []string{"id", "name"}},
		{"alias wins", "SELECT total AS order_total, id FROM orders", []string{"order_total", "id"}},
		{"expression alias", "SELECT count(*) AS n, max(x) AS m FROM t", []string{"n", "m"}},
		{"cast inner AS ignored", "SELECT cast(zip AS text) AS zip_code FROM t", []string{"zip_code"}},
		{"unaliased expr dropped", "SELECT count(*), id FROM t", []string{"id"}},
		{"star yields nothing", "SELECT * FROM t", nil},
		{"final select of CTE", "WITH a AS (SELECT p FROM raw) SELECT x, y FROM a", []string{"x", "y"}},
		{"no from clause", "SELECT 1 AS id, 2 AS qty", []string{"id", "qty"}},
		{"empty", "", nil},
		{"dedup", "SELECT id, id FROM t", []string{"id"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProjectionColumns(c.query)
			if len(got) != len(c.want) {
				t.Fatalf("ProjectionColumns(%q) = %v, want %v", c.query, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("ProjectionColumns(%q) = %v, want %v", c.query, got, c.want)
				}
			}
		})
	}
}
