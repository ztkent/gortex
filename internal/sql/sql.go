// Package sql parses SQL string literals into the table references
// they touch. Used by language extractors that detect calls into a
// SQL exec API (db.Query, db.Exec, sqlx.NamedExec, etc.) with a
// string-literal first arg — the literal goes through ExtractTables
// to get the names; the caller emits KindTable nodes plus EdgeQueries
// edges.
//
// Scope (v1): regex-based table extraction from FROM / JOIN /
// INSERT INTO / UPDATE / DELETE FROM clauses. The regex picks up
// the canonical patterns without spinning up a full SQL parser.
// Trade-offs:
//
//   - Dynamic SQL built by string concatenation or query builders
//     is invisible. Agents who care about that will fall back to
//     grep — same v1 stance the broader spec takes for noisy
//     extractions.
//
//   - Quoted identifiers (`"foo"`, `[foo]`) and case-sensitive
//     schema-qualified names (`schema.table`) are handled — the
//     regex strips quoting and keeps the trailing identifier, with
//     schema preserved in the meta when present.
//
//   - SQL keywords used as identifiers (`FROM "from"`) misclassify
//     as the keyword. A future enhancement could feed the regex
//     output through a SQL keyword list to filter them; v1 accepts
//     the noise.
//
//   - Default-off via the `sql` coverage gate per the spec — string-
//     literal SQL is noisy enough that opt-in is the right shape.
package sql

import (
	"regexp"
	"sort"
	"strings"
)

// tableRefPatterns enumerates the SQL clauses that introduce a
// table reference. Each pattern uses a single capture group on the
// table identifier. Case-insensitive match — SQL conventionally
// uppercases keywords but we tolerate either form.
var tableRefPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bFROM\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
	regexp.MustCompile(`(?i)\bJOIN\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
	regexp.MustCompile(`(?i)\bINSERT\s+INTO\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
	regexp.MustCompile(`(?i)\bUPDATE\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
	regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
	regexp.MustCompile(`(?i)\bTRUNCATE\s+(?:TABLE\s+)?([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`),
}

// TableRef is a single resolved table reference.
type TableRef struct {
	Table  string // unquoted table name (last segment if schema.table)
	Schema string // optional schema prefix; "" when none
	Op     string // canonical operation: select, insert, update, delete, truncate
}

// canonicalOp maps a clause keyword to a stable operation tag for
// downstream queries that scope by op (e.g. "find every site that
// truncates X").
func canonicalOp(clauseHead string) string {
	switch strings.ToUpper(strings.Fields(clauseHead)[0]) {
	case "FROM", "JOIN":
		return "select"
	case "INSERT":
		return "insert"
	case "UPDATE":
		return "update"
	case "DELETE":
		return "delete"
	case "TRUNCATE":
		return "truncate"
	}
	return ""
}

// ExtractTables walks query and returns the de-duplicated set of
// table references found. Order follows source-text occurrence so
// the result is diff-able across runs of the same query string.
func ExtractTables(query string) []TableRef {
	if query == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var refs []TableRef

	// `DELETE FROM` matches both the DELETE FROM pattern (correct)
	// and the bare FROM pattern (wrong — we'd report the same
	// table as both a select and a delete). Process compound
	// keywords first, mask out their match ranges so the FROM
	// regex doesn't see them, then process the remaining ones.
	working := maskDeleteFromForFromPattern(query)

	for i, re := range tableRefPatterns {
		// The FROM pattern (index 0) sees the masked text; the
		// DELETE FROM pattern (index 4) sees the original to find
		// its own matches first.
		text := query
		if i == 0 {
			text = working
		}
		matches := re.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			schema, table := splitSchemaTable(stripQuoting(m[1]))
			if table == "" {
				continue
			}
			op := canonicalOp(m[0])
			key := op + "::" + schema + "::" + table
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, TableRef{
				Table:  table,
				Schema: schema,
				Op:     op,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Op != refs[j].Op {
			return refs[i].Op < refs[j].Op
		}
		if refs[i].Schema != refs[j].Schema {
			return refs[i].Schema < refs[j].Schema
		}
		return refs[i].Table < refs[j].Table
	})
	return refs
}

// maskDeleteFromForFromPattern substitutes the FROM keyword in
// "DELETE FROM" with a non-keyword sentinel so the bare FROM
// regex doesn't double-match the same table reference. The
// sentinel `__GFOX_FROM__` won't appear in real SQL and is
// valid in the regex's character class so it gets ignored
// silently. The DELETE FROM pattern still operates on the
// original query string and finds its own match.
var deleteFromMaskRe = regexp.MustCompile(`(?i)\b(DELETE)\s+FROM\b`)

func maskDeleteFromForFromPattern(query string) string {
	return deleteFromMaskRe.ReplaceAllString(query, "$1 __GFOX_FROM__")
}

// stripQuoting removes the four shapes of SQL identifier quoting:
// double quotes (ANSI), backticks (MySQL), brackets (T-SQL). The
// inner content is returned unchanged otherwise.
func stripQuoting(name string) string {
	name = strings.TrimSpace(name)
	if len(name) >= 2 {
		first, last := name[0], name[len(name)-1]
		switch {
		case first == '"' && last == '"',
			first == '`' && last == '`',
			first == '[' && last == ']':
			return name[1 : len(name)-1]
		}
	}
	return name
}

// splitSchemaTable separates `schema.table` into its parts.
// Multi-dot identifiers (`db.schema.table`) collapse to schema=
// "schema", table="table" — the leftmost segment is database-
// scoped and rarely useful for graph queries.
func splitSchemaTable(name string) (schema, table string) {
	if i := strings.LastIndex(name, "."); i >= 0 {
		schema = name[:i]
		table = name[i+1:]
		// If the schema piece itself has a database segment, keep
		// only the immediate parent.
		if j := strings.LastIndex(schema, "."); j >= 0 {
			schema = schema[j+1:]
		}
		return strings.TrimSpace(stripQuoting(schema)), strings.TrimSpace(stripQuoting(table))
	}
	return "", strings.TrimSpace(name)
}

// createTableRe matches CREATE TABLE [IF NOT EXISTS] declarations
// across the four canonical identifier-quoting styles. Used by
// ExtractCreateTables for migration-file extraction — distinct
// from ExtractTables, which scans query strings rather than DDL.
var createTableRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:GLOBAL\s+TEMPORARY\s+|LOCAL\s+TEMPORARY\s+|TEMPORARY\s+|TEMP\s+|UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)`)

// ExtractCreateTables returns the tables declared by CREATE TABLE
// statements in a SQL source file. Schema-qualified names retain
// their schema in TableRef.Schema; identifier quoting is stripped.
// Op is always "create".
//
// Used by migration-file extraction where the SQL source is a DDL
// script rather than an embedded query string. Drop / alter
// statements are deliberately not extracted — a migration that
// drops a table doesn't *provide* the table to the rest of the
// repo, and modeling alter-as-delta would require maintaining
// per-migration ordering that's out of scope for the v1.
func ExtractCreateTables(source string) []TableRef {
	if source == "" {
		return nil
	}
	matches := createTableRe.FindAllStringSubmatch(source, -1)
	seen := make(map[string]struct{})
	var refs []TableRef
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		schema, table := splitSchemaTable(stripQuoting(m[1]))
		if table == "" {
			continue
		}
		key := schema + "::" + table
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, TableRef{
			Table:  table,
			Schema: schema,
			Op:     "create",
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Schema != refs[j].Schema {
			return refs[i].Schema < refs[j].Schema
		}
		return refs[i].Table < refs[j].Table
	})
	return refs
}

// ColumnRef is a single resolved column reference. Op is "read" for
// columns appearing in SELECT projections, WHERE clauses, ORDER BY,
// or GROUP BY; "write" for columns in INSERT INTO (col-list) and
// UPDATE … SET col = … assignments. Table identifies the table the
// column belongs to; multi-table queries (joins) are restricted to
// the first table reference because column-table association would
// otherwise require a real SQL parser.
type ColumnRef struct {
	Schema string
	Table  string
	Column string
	Op     string // "read" | "write"
}

// insertColsRe matches `INSERT INTO tbl (col1, col2, …)` with
// optional schema-qualified table.
var insertColsRe = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)\s*\(([^)]*)\)`)

// updateSetRe matches `UPDATE tbl SET col = …, col2 = …` capturing
// the table name and the SET clause's content (greedy until WHERE
// or end of statement).
var updateSetRe = regexp.MustCompile(`(?is)\bUPDATE\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)\s+SET\s+(.+?)(?:\bWHERE\b|\bRETURNING\b|;|$)`)

// selectFromRe matches `SELECT cols FROM tbl` for single-table
// queries (no JOIN). Multi-table SELECTs return no column edges
// because v1 can't disambiguate which table each column lives on.
var selectFromRe = regexp.MustCompile(`(?is)\bSELECT\s+(.+?)\s+FROM\s+([a-zA-Z_"\x60\[][a-zA-Z0-9_."\x60\]]*)\b`)

// joinDetectRe is used to suppress SELECT column extraction when
// the query has any kind of JOIN — preserves correctness over
// completeness in v1.
var joinDetectRe = regexp.MustCompile(`(?i)\bJOIN\b`)

// ExtractColumns walks a query string and returns the column
// references it touches. The "Op" field distinguishes reads from
// writes so the caller can emit EdgeReadsCol vs EdgeWritesCol.
//
// Limitations (intentional for v1):
//   - SELECT * does not produce edges (wildcard).
//   - Multi-table SELECTs (with JOIN) produce no column edges.
//   - Functions, expressions, and CASE statements degrade to no edge
//     for that particular projection slot.
//   - WHERE-clause column reads are extracted only when the value-side
//     reference is a bare identifier.
func ExtractColumns(query string) []ColumnRef {
	if query == "" {
		return nil
	}
	out := []ColumnRef{}
	seen := map[string]struct{}{}
	add := func(c ColumnRef) {
		key := c.Op + "::" + c.Schema + "::" + c.Table + "::" + c.Column
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}

	// INSERT INTO tbl (col1, col2, …) → writes.
	for _, m := range insertColsRe.FindAllStringSubmatch(query, -1) {
		schema, table := splitSchemaTable(stripQuoting(m[1]))
		if table == "" {
			continue
		}
		for _, c := range splitColumnList(m[2]) {
			add(ColumnRef{Schema: schema, Table: table, Column: c, Op: "write"})
		}
	}

	// UPDATE tbl SET col = …, col2 = … → writes.
	for _, m := range updateSetRe.FindAllStringSubmatch(query, -1) {
		schema, table := splitSchemaTable(stripQuoting(m[1]))
		if table == "" {
			continue
		}
		for _, c := range splitSetAssignments(m[2]) {
			add(ColumnRef{Schema: schema, Table: table, Column: c, Op: "write"})
		}
	}

	// SELECT col1, col2 FROM tbl (single-table) → reads.
	if !joinDetectRe.MatchString(query) {
		for _, m := range selectFromRe.FindAllStringSubmatch(query, -1) {
			projection := strings.TrimSpace(m[1])
			schema, table := splitSchemaTable(stripQuoting(m[2]))
			if table == "" {
				continue
			}
			if projection == "*" || projection == "" {
				continue
			}
			for _, c := range splitColumnList(projection) {
				add(ColumnRef{Schema: schema, Table: table, Column: c, Op: "read"})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Column < out[j].Column
	})
	return out
}

// splitColumnList parses a comma-separated column list (used for
// INSERT and SELECT projections), returning bare column identifiers.
// Aliases (`col AS alias`) collapse to the source column. Function
// calls and expressions return "" and are dropped.
func splitColumnList(list string) []string {
	out := []string{}
	depth := 0
	cur := strings.Builder{}
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" {
			return
		}
		// Strip trailing AS alias.
		if idx := strings.LastIndex(strings.ToUpper(s), " AS "); idx >= 0 {
			s = strings.TrimSpace(s[:idx])
		}
		// Strip table prefix `tbl.col` → `col`.
		if idx := strings.LastIndex(s, "."); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSpace(stripQuoting(s))
		// Reject bare expressions (function calls, arithmetic, *).
		if s == "" || s == "*" || !isPlainSQLIdent(s) {
			return
		}
		out = append(out, s)
	}
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush()
				continue
			}
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// splitSetAssignments parses a SET clause body (`col = ?, col2 =
// fn(x)`) and returns the column names being written. The right-
// hand expressions are skipped; column refs deeper than `tbl.col` are
// reduced to `col`.
func splitSetAssignments(set string) []string {
	out := []string{}
	depth := 0
	cur := strings.Builder{}
	flush := func() {
		seg := strings.TrimSpace(cur.String())
		cur.Reset()
		if seg == "" {
			return
		}
		eq := strings.Index(seg, "=")
		if eq < 0 {
			return
		}
		name := strings.TrimSpace(seg[:eq])
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		name = strings.TrimSpace(stripQuoting(name))
		if name != "" && isPlainSQLIdent(name) {
			out = append(out, name)
		}
	}
	for i := 0; i < len(set); i++ {
		c := set[i]
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush()
				continue
			}
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// isPlainSQLIdent returns true when s is a single bare identifier
// (letter/underscore start, alphanum/underscore body). Filters out
// function-call shells (`fn(`), wildcards, arithmetic, and the like.
func isPlainSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 {
			if !isAlpha {
				return false
			}
		} else if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

// selectNoFromRe matches a `SELECT <projection>` that has no FROM
// clause (e.g. `SELECT 1 AS id`). Used only as the fallback when a
// query contains no FROM at all — selectFromRe takes precedence.
var selectNoFromRe = regexp.MustCompile(`(?is)\bSELECT\s+(.+?)\s*(?:;|$)`)

// ProjectionColumns returns the output column names a query produces —
// the columns of the relation the query *materialises*, as opposed to
// ExtractColumns which attributes columns to the source tables a query
// reads or writes. Used by the dbt / SQLMesh model extractor to record
// a model's own columns.
//
// The output column of a projection slot is its alias when one is
// present (`total AS order_total` → `order_total`) and the bare column
// identifier otherwise (`customers.id` → `id`). Slots that are pure
// expressions / function calls with no alias, and `*` / `tbl.*`
// wildcards, produce no name.
//
// Heuristic (mirrors the v1 regex stance of the rest of this package):
// the last `SELECT <projection> FROM` occurrence is taken as the
// query's output projection — CTEs and subqueries appear earlier in
// source order, so the final top-level SELECT is what the query
// returns. When the query has no FROM clause at all the trailing
// `SELECT <projection>` is used. Subquery-valued projection slots and
// JOIN-bearing final SELECTs degrade gracefully to whatever bare
// identifiers can still be recovered.
func ProjectionColumns(query string) []string {
	if query == "" {
		return nil
	}
	var projection string
	if all := selectFromRe.FindAllStringSubmatch(query, -1); len(all) > 0 {
		projection = strings.TrimSpace(all[len(all)-1][1])
	} else if m := selectNoFromRe.FindStringSubmatch(query); m != nil {
		projection = strings.TrimSpace(m[1])
	}
	if projection == "" || projection == "*" {
		return nil
	}
	cols := splitProjectionList(projection)
	seen := make(map[string]struct{}, len(cols))
	out := cols[:0]
	for _, c := range cols {
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// splitProjectionList parses a SELECT projection list and returns the
// output column names — the alias when one is present, otherwise the
// bare column identifier. Distinct from splitColumnList, which collapses
// `col AS alias` to the source `col` because it attributes reads to a
// source table; here the alias *is* the produced column name.
func splitProjectionList(list string) []string {
	out := []string{}
	depth := 0
	cur := strings.Builder{}
	flush := func() {
		seg := strings.TrimSpace(cur.String())
		cur.Reset()
		if seg == "" {
			return
		}
		if idx := lastTopLevelAS(seg); idx >= 0 {
			alias := strings.TrimSpace(stripQuoting(strings.TrimSpace(seg[idx+4:])))
			if isPlainSQLIdent(alias) {
				out = append(out, alias)
			}
			return
		}
		// No alias: strip a `tbl.` qualifier, require a bare identifier.
		s := seg
		if i := strings.LastIndex(s, "."); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(stripQuoting(s))
		if s != "" && s != "*" && isPlainSQLIdent(s) {
			out = append(out, s)
		}
	}
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch c {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				flush()
				continue
			}
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}

// lastTopLevelAS returns the byte index of the last ` AS ` keyword in
// seg that sits at paren depth zero, or -1 when there is none. Case-
// insensitive. Keeps a cast's inner ` AS ` (`CAST(x AS int)`) from
// being mistaken for a projection alias.
func lastTopLevelAS(seg string) int {
	upper := strings.ToUpper(seg)
	depth := 0
	last := -1
	for i := 0; i+4 <= len(upper); i++ {
		switch upper[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && upper[i:i+4] == " AS " {
			last = i
		}
	}
	return last
}

// ColumnNodeID returns the canonical synthetic ID for a column.
func ColumnNodeID(dialect, schema, table, column string) string {
	if dialect == "" {
		dialect = "generic"
	}
	prefix := "col::" + dialect + "::"
	if schema == "" {
		return prefix + table + "." + column
	}
	return prefix + schema + "." + table + "." + column
}

// MigrationNodeID is the canonical synthetic ID for a migration
// node. The path component lets agents reach the originating file
// in one step; the prefix matches the synthetic-ID convention
// used by db:: tables and module:: dependencies.
func MigrationNodeID(path string) string {
	return "migration::" + path
}

// IsMigrationPath returns true when filePath looks like a SQL
// migration file. Recognised conventions: any .sql file under a
// directory whose name contains "migrate" or "migration"
// (case-insensitive). Matches Rails (db/migrate/), golang-migrate
// (migrations/), Alembic when wrapped (we mostly handle alembic
// via Python parsers separately), and most ORM generators.
func IsMigrationPath(filePath string) bool {
	if !strings.HasSuffix(strings.ToLower(filePath), ".sql") {
		return false
	}
	lower := strings.ToLower(filePath)
	for _, segment := range []string{"/migrate/", "/migrations/", "/migrate.", "/migrations."} {
		if strings.Contains(lower, segment) {
			return true
		}
	}
	return strings.HasPrefix(lower, "migrate/") ||
		strings.HasPrefix(lower, "migrations/") ||
		strings.HasPrefix(lower, "db/migrate/") ||
		strings.HasPrefix(lower, "db/migrations/")
}

// TableNodeID returns the canonical synthetic ID for a table
// reference. Mirrors the ecosystem-prefix convention used by
// module:: / external:: / annotation:: / event:: nodes — `db::`
// keeps the table namespace distinct.
//
// dialect is the SQL dialect tag (postgres, mysql, sqlite,
// generic) — included on the ID so cross-dialect projects can
// distinguish a Postgres `users` table from a MySQL one in the
// same graph. The default dialect is "generic" when the caller
// doesn't know.
func TableNodeID(dialect, schema, table string) string {
	if dialect == "" {
		dialect = "generic"
	}
	prefix := "db::" + dialect + "::"
	if schema == "" {
		return prefix + table
	}
	return prefix + schema + "." + table
}
