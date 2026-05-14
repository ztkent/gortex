package languages

import (
	"maps"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/sql"
)

// dbt / SQLMesh column-level search (roadmap B10).
//
// Three ingest paths land here, each dispatched into by a host
// extractor that already owns the file extension:
//
//   - dbt SQL models       — Jinja-templated `.sql` under a dbt
//                            project's models/ tree. Dispatched from
//                            SQLExtractor.Extract via classifySQLFile.
//   - SQLMesh SQL models   — `.sql` files that open with a `MODEL (…)`
//                            block. Same dispatch point.
//   - dbt schema YAML      — `schema.yml` / properties files declaring
//                            `models:` / `sources:` / `seeds:` /
//                            `snapshots:` with their columns. Dispatched
//                            from YAMLExtractor.Extract.
//
// Graph shape. Every model / seed / snapshot / source becomes a
// KindTable node; every declared or projected column becomes a
// KindColumn node linked to its model with EdgeMemberOf — that is the
// "column-level search" surface (search_symbols kind:column). Model
// lineage — dbt `ref()` / `source()`, SQLMesh `FROM` / `JOIN` — becomes
// EdgeDependsOn edges between the KindTable nodes. IDs are deterministic
// and name-derived so lineage edges resolve through node identity
// without a resolver pass:
//
//   dbt::model::<name>             model, seed, or snapshot (the dbt
//                                  `ref()` namespace is flat across
//                                  all three resource types)
//   dbt::source::<source>.<table>  dbt source (the `source()` namespace)
//   sqlmesh::model::<qualified>    SQLMesh model, keyed by its declared
//                                  (schema-qualified) name
//   <model-id>::<column>           a column of the model above
var (
	// sqlmeshModelRe matches a SQLMesh model file: a `MODEL (` block
	// at the start of the file, tolerating leading line / block
	// comments. The match end sits just past the opening paren.
	sqlmeshModelRe = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n\s*|/\*.*?\*/\s*)*MODEL\s*\(`)
	// dbtJinjaMarkerRe recognises the load-bearing dbt Jinja calls.
	// Any of these in a `.sql` file is a definitive dbt-model signal.
	dbtJinjaMarkerRe = regexp.MustCompile(`(?is)\{\{[-\s]*(?:ref|source|config|this)\b|\{%[-\s]*(?:snapshot|materialization)\b`)

	dbtRefRe      = regexp.MustCompile(`(?is)\{\{[-\s]*ref\s*\(\s*('[^']+'|"[^"]+")\s*(?:,\s*('[^']+'|"[^"]+")\s*)?\)[-\s]*\}\}`)
	dbtSourceRe   = regexp.MustCompile(`(?is)\{\{[-\s]*source\s*\(\s*('[^']+'|"[^"]+")\s*,\s*('[^']+'|"[^"]+")\s*\)[-\s]*\}\}`)
	dbtConfigRe   = regexp.MustCompile(`(?is)\{\{[-\s]*config\s*\((.*?)\)[-\s]*\}\}`)
	dbtSnapshotRe = regexp.MustCompile(`(?i)\{%[-\s]*snapshot\s+([A-Za-z_]\w*)\s*[-\s]*%\}`)

	jinjaExprRe    = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	jinjaStmtRe    = regexp.MustCompile(`(?s)\{%.*?%\}`)
	jinjaCommentRe = regexp.MustCompile(`(?s)\{#.*?#\}`)

	sqlLineCommentRe  = regexp.MustCompile(`(?m)--[^\n]*`)
	sqlBlockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlmeshMacroRe    = regexp.MustCompile(`@\w+`)

	// dbtConfigKVRe pulls a single `key='value'` / `key="value"` pair
	// out of a config(...) argument blob.
	dbtConfigKVRe = regexp.MustCompile(`(?i)\b(materialized|schema|alias|database|incremental_strategy|unique_key)\s*=\s*('[^']*'|"[^"]*")`)
)

// classifySQLFile decides whether a `.sql` file is a dbt model, a
// SQLMesh model, or neither. Returns "dbt", "sqlmesh", or "".
//
// Order matters: the SQLMesh `MODEL (` block is structurally
// unambiguous so it is checked first; dbt Jinja markers are next; a
// path-based fallback then catches pure-SQL dbt models (a dbt model
// need not contain any Jinja) — but only when the file actually opens
// with a query, so a stray DDL script under a models/ directory is not
// misclassified.
func classifySQLFile(filePath string, src []byte) string {
	if sqlmeshModelRe.Match(src) {
		return "sqlmesh"
	}
	if dbtJinjaMarkerRe.Match(src) {
		return "dbt"
	}
	if isDbtModelPath(filePath) && opensWithQuery(src) {
		return "dbt"
	}
	return ""
}

// isDbtModelPath reports whether filePath sits in the conventional dbt
// models/ or snapshots/ tree. Seeds are `.csv` and never reach the SQL
// extractor; sources have no SQL file at all.
func isDbtModelPath(filePath string) bool {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	if strings.Contains(lower, "/models/") || strings.Contains(lower, "/snapshots/") {
		return true
	}
	return strings.HasPrefix(lower, "models/") || strings.HasPrefix(lower, "snapshots/")
}

// opensWithQuery reports whether the first significant token of a SQL
// file is `SELECT`, `WITH`, or a Jinja delimiter — the shapes a dbt
// model file takes. A file that opens with `CREATE` / `ALTER` / `INSERT`
// / etc. is a DDL or DML script, not a dbt model.
func opensWithQuery(src []byte) bool {
	s := strings.TrimSpace(stripSQLComments(string(src)))
	if s == "" {
		return false
	}
	up := strings.ToUpper(s)
	return strings.HasPrefix(up, "SELECT") || strings.HasPrefix(up, "WITH") ||
		strings.HasPrefix(s, "{{") || strings.HasPrefix(s, "{%") || strings.HasPrefix(s, "(")
}

// ---------------------------------------------------------------------------
// dbt SQL models
// ---------------------------------------------------------------------------

// extractDbtSQLModel emits the graph fragment for a single dbt SQL
// model file. The host SQLExtractor has already appended the file node
// (fileID); this function adds the model KindTable node, its columns,
// and its `ref()` / `source()` lineage.
func extractDbtSQLModel(filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	resourceType := "model"
	name := dbtModelNameFromPath(filePath)
	if m := dbtSnapshotRe.FindSubmatch(src); m != nil {
		resourceType = "snapshot"
		name = string(m[1])
	}
	if name == "" {
		return
	}

	modelID := dbtModelID(name)
	endLine := lineCount(src)
	meta := map[string]any{
		"framework":     "dbt",
		"sql_type":      "dbt_model",
		"resource_type": resourceType,
		"model":         name,
	}
	cfg := parseDbtConfig(src)
	materialized := cfg["materialized"]
	if materialized == "" {
		if resourceType == "snapshot" {
			materialized = "snapshot"
		} else {
			materialized = "view"
		}
	}
	meta["materialized"] = materialized
	if v := cfg["schema"]; v != "" {
		meta["schema"] = v
	}
	if v := cfg["alias"]; v != "" {
		meta["alias"] = v
	}
	if v := cfg["database"]; v != "" {
		meta["database"] = v
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: modelID, Kind: graph.KindTable, Name: name,
		QualName: "dbt.model." + name,
		FilePath: filePath, StartLine: 1, EndLine: endLine,
		Language: "sql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: modelID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: 1,
	})

	emitDbtLineage(filePath, modelID, src, result)
	emitDbtModelColumns(filePath, modelID, name, deJinja(src), result)
}

// emitDbtLineage walks every `{{ ref(...) }}` and `{{ source(...) }}`
// call in src and emits an EdgeDependsOn from the model to the upstream
// node. `source()` references also materialise a minimal KindTable
// source node so the edge always lands on a real node even when no
// schema YAML declares the source; a later schema-YAML pass enriches it.
func emitDbtLineage(filePath, modelID string, src []byte, result *parser.ExtractionResult) {
	seen := make(map[string]bool)
	emitDep := func(targetID, link, detail string, line int) {
		key := targetID + "\x00" + link
		if seen[key] {
			return
		}
		seen[key] = true
		m := map[string]any{"link": link}
		if detail != "" {
			m["ref_package"] = detail
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: modelID, To: targetID, Kind: graph.EdgeDependsOn,
			FilePath: filePath, Line: line, Meta: m,
		})
	}

	for _, m := range dbtRefRe.FindAllSubmatchIndex(src, -1) {
		arg1 := unquoteSQL(string(src[m[2]:m[3]]))
		pkg := ""
		refName := arg1
		if m[4] >= 0 { // two-arg form: ref('package', 'model')
			pkg = arg1
			refName = unquoteSQL(string(src[m[4]:m[5]]))
		}
		if refName == "" {
			continue
		}
		emitDep(dbtModelID(refName), "ref", pkg, lineAt(src, m[0]))
	}

	for _, m := range dbtSourceRe.FindAllSubmatchIndex(src, -1) {
		sourceName := unquoteSQL(string(src[m[2]:m[3]]))
		tableName := unquoteSQL(string(src[m[4]:m[5]]))
		if sourceName == "" || tableName == "" {
			continue
		}
		srcID := dbtSourceID(sourceName, tableName)
		line := lineAt(src, m[0])
		ensureDbtSourceNode(filePath, srcID, sourceName, tableName, line, result)
		emitDep(srcID, "source", "", line)
	}
}

// emitDbtModelColumns records a model's own output columns. dejinjaed
// is the model body with Jinja delimiters neutralised so the SQL
// projection parser can read the final SELECT.
func emitDbtModelColumns(filePath, modelID, modelName, dejinjaed string, result *parser.ExtractionResult) {
	cols := sql.ProjectionColumns(stripSQLComments(dejinjaed))
	for _, col := range cols {
		emitDbtColumn(filePath, modelID, modelName, col, "sql", nil, result)
	}
}

// ---------------------------------------------------------------------------
// SQLMesh SQL models
// ---------------------------------------------------------------------------

// extractSQLMeshSQLModel emits the graph fragment for a SQLMesh `.sql`
// model: the `MODEL (…)` block's properties become node Meta, the
// `columns (…)` property (or the body's final SELECT projection when
// absent) becomes KindColumn nodes, and the body's FROM / JOIN table
// references become EdgeDependsOn lineage edges.
func extractSQLMeshSQLModel(filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	props, columnDefs, bodyStart, ok := parseSQLMeshModel(src)
	if !ok {
		return
	}
	name := props["name"]
	if name == "" {
		name = dbtModelNameFromPath(filePath)
	}
	if name == "" {
		return
	}

	modelID := sqlmeshModelID(name)
	meta := map[string]any{
		"framework": "sqlmesh",
		"sql_type":  "sqlmesh_model",
		"model":     name,
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		meta["schema"] = name[:i]
	}
	if v := props["kind"]; v != "" {
		// `kind` can be `FULL` or `INCREMENTAL_BY_TIME_RANGE(...)`; the
		// leading token is the stable classifier.
		meta["materialized"] = strings.ToLower(firstToken(v))
		meta["kind"] = v
	} else {
		meta["materialized"] = "unknown"
	}
	for _, k := range []string{"cron", "grain", "owner", "dialect", "description"} {
		if v := props[k]; v != "" {
			meta[k] = unquoteSQL(v)
		}
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: modelID, Kind: graph.KindTable, Name: sqlmeshLeafName(name),
		QualName: "sqlmesh.model." + name,
		FilePath: filePath, StartLine: 1, EndLine: lineCount(src),
		Language: "sql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: modelID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: 1,
	})

	// Columns: the explicit `columns (...)` property wins; otherwise
	// fall back to the body's final SELECT projection.
	if len(columnDefs) > 0 {
		for _, cd := range columnDefs {
			var cm map[string]any
			if cd.dataType != "" {
				cm = map[string]any{"data_type": cd.dataType}
			}
			emitDbtColumn(filePath, modelID, sqlmeshLeafName(name), cd.name, "model_block", cm, result)
		}
	} else if bodyStart < len(src) {
		body := stripSQLComments(sqlmeshMacroRe.ReplaceAllString(string(src[bodyStart:]), " "))
		for _, col := range sql.ProjectionColumns(body) {
			emitDbtColumn(filePath, modelID, sqlmeshLeafName(name), col, "sql", nil, result)
		}
	}

	// Lineage: every table the body reads from is an upstream model.
	if bodyStart < len(src) {
		body := stripSQLComments(sqlmeshMacroRe.ReplaceAllString(string(src[bodyStart:]), " "))
		seen := make(map[string]bool)
		for _, ref := range sql.ExtractTables(body) {
			upstream := ref.Table
			if ref.Schema != "" {
				upstream = ref.Schema + "." + ref.Table
			}
			upstreamID := sqlmeshModelID(upstream)
			if upstreamID == modelID || seen[upstreamID] {
				continue
			}
			seen[upstreamID] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From: modelID, To: upstreamID, Kind: graph.EdgeDependsOn,
				FilePath: filePath, Line: 1,
				Meta: map[string]any{"link": "from", "op": ref.Op},
			})
		}
	}
}

// sqlmeshColumn is one entry of a SQLMesh `columns (...)` property.
type sqlmeshColumn struct {
	name     string
	dataType string
}

// parseSQLMeshModel locates and parses the leading `MODEL (…)` block.
// Returns the top-level scalar properties, the parsed `columns (...)`
// list (nil when the property is absent), the byte offset where the
// post-block SQL body starts, and ok=false when no MODEL block is found.
func parseSQLMeshModel(src []byte) (props map[string]string, columns []sqlmeshColumn, bodyStart int, ok bool) {
	loc := sqlmeshModelRe.FindIndex(src)
	if loc == nil {
		return nil, nil, 0, false
	}
	openParen := loc[1] - 1 // sqlmeshModelRe ends just past `(`
	closeParen := scanMatchingParen(src, openParen)
	if closeParen < 0 {
		return nil, nil, 0, false
	}
	block := string(src[openParen+1 : closeParen])

	props = make(map[string]string)
	for _, prop := range splitTopLevelCommas(block) {
		prop = strings.TrimSpace(prop)
		if prop == "" {
			continue
		}
		key, value := splitFirstToken(prop)
		key = strings.ToLower(key)
		if key == "columns" {
			columns = parseSQLMeshColumns(value)
			continue
		}
		props[key] = strings.TrimSpace(value)
	}

	bodyStart = closeParen + 1
	for bodyStart < len(src) {
		c := src[bodyStart]
		if c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			bodyStart++
			continue
		}
		break
	}
	return props, columns, bodyStart, true
}

// parseSQLMeshColumns parses a `columns (...)` property value — the
// outer parens still attached — into (name, type) pairs.
func parseSQLMeshColumns(value string) []sqlmeshColumn {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "(")
	value = strings.TrimSuffix(value, ")")
	var out []sqlmeshColumn
	for _, entry := range splitTopLevelCommas(value) {
		name, rest := splitFirstToken(strings.TrimSpace(entry))
		name = strings.TrimSpace(stripSQLIdentQuotes(name))
		if name == "" {
			continue
		}
		out = append(out, sqlmeshColumn{name: name, dataType: strings.TrimSpace(rest)})
	}
	return out
}

// ---------------------------------------------------------------------------
// dbt schema YAML
// ---------------------------------------------------------------------------

// extractDbtSchemaYAML detects and extracts a dbt schema / properties
// YAML file — the `schema.yml`-style files that describe `models:`,
// `sources:`, `seeds:`, and `snapshots:` along with their columns.
// Returns true when the file was a dbt schema file (so the YAML
// extractor skips its generic top-level-keys fallback), false otherwise.
func extractDbtSchemaYAML(filePath, fileID string, src []byte, result *parser.ExtractionResult) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return false
	}
	root := documentMapping(&doc)
	if root == nil || !isDbtSchemaYAML(root, filePath) {
		return false
	}

	// models / seeds / snapshots: all live in the flat dbt `ref()`
	// namespace, so they share the dbt::model:: ID prefix and are
	// distinguished only by Meta["resource_type"].
	for _, group := range []struct{ key, resourceType, sqlType string }{
		{"models", "model", "dbt_model"},
		{"seeds", "seed", "dbt_seed"},
		{"snapshots", "snapshot", "dbt_snapshot"},
	} {
		for _, item := range sequenceItems(mappingGet(root, group.key)) {
			if item.Kind != yaml.MappingNode {
				continue
			}
			name := scalarOf(mappingGet(item, "name"))
			if name == "" {
				continue
			}
			modelID := dbtModelID(name)
			line := item.Line
			if line <= 0 {
				line = 1
			}
			meta := map[string]any{
				"framework":     "dbt",
				"sql_type":      group.sqlType,
				"resource_type": group.resourceType,
				"model":         name,
			}
			if desc := scalarOf(mappingGet(item, "description")); desc != "" {
				meta["description"] = truncateText(desc, 280)
			}
			if mat := dbtConfigMaterialized(item); mat != "" {
				meta["materialized"] = mat
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: modelID, Kind: graph.KindTable, Name: name,
				QualName: "dbt.model." + name,
				FilePath: filePath, StartLine: line, EndLine: line,
				Language: "yaml", Meta: meta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: modelID, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: line,
			})
			emitDbtSchemaColumns(filePath, modelID, name, item, result)
		}
	}

	// sources: a `sources:` entry groups one or more `tables:`, each of
	// which is an independently ref-able (via `source()`) relation.
	for _, srcItem := range sequenceItems(mappingGet(root, "sources")) {
		if srcItem.Kind != yaml.MappingNode {
			continue
		}
		sourceName := scalarOf(mappingGet(srcItem, "name"))
		if sourceName == "" {
			continue
		}
		sourceSchema := scalarOf(mappingGet(srcItem, "schema"))
		sourceDB := scalarOf(mappingGet(srcItem, "database"))
		for _, tbl := range sequenceItems(mappingGet(srcItem, "tables")) {
			if tbl.Kind != yaml.MappingNode {
				continue
			}
			tableName := scalarOf(mappingGet(tbl, "name"))
			if tableName == "" {
				continue
			}
			srcID := dbtSourceID(sourceName, tableName)
			line := tbl.Line
			if line <= 0 {
				line = 1
			}
			meta := map[string]any{
				"framework":     "dbt",
				"sql_type":      "dbt_source",
				"resource_type": "source",
				"source":        sourceName,
				"table":         tableName,
			}
			if sourceSchema != "" {
				meta["schema"] = sourceSchema
			}
			if sourceDB != "" {
				meta["database"] = sourceDB
			}
			if desc := scalarOf(mappingGet(tbl, "description")); desc != "" {
				meta["description"] = truncateText(desc, 280)
			}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: srcID, Kind: graph.KindTable, Name: sourceName + "." + tableName,
				QualName: "dbt.source." + sourceName + "." + tableName,
				FilePath: filePath, StartLine: line, EndLine: line,
				Language: "yaml", Meta: meta,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: srcID, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: line,
			})
			emitDbtSchemaColumns(filePath, srcID, sourceName+"."+tableName, tbl, result)
		}
	}

	return true
}

// isDbtSchemaYAML fingerprints a dbt schema / properties YAML file. It
// requires a `models` / `seeds` / `snapshots` sequence of name-bearing
// mappings (with at least one descriptive sub-key, or a top-level
// `version`), or a `sources` sequence whose entries carry `tables` —
// a shape that does not collide with dbt_project.yml (whose `models:`
// is a config *mapping*) or with arbitrary config YAML.
func isDbtSchemaYAML(root *yaml.Node, filePath string) bool {
	if root == nil {
		return false
	}
	switch strings.ToLower(filepath.Base(filePath)) {
	case "dbt_project.yml", "dbt_project.yaml", "packages.yml", "packages.yaml",
		"dependencies.yml", "dependencies.yaml", "profiles.yml", "profiles.yaml",
		"selectors.yml", "selectors.yaml":
		return false
	}
	hasVersion := scalarOf(mappingGet(root, "version")) != ""
	for _, key := range []string{"models", "seeds", "snapshots"} {
		for _, item := range sequenceItems(mappingGet(root, key)) {
			if item.Kind != yaml.MappingNode {
				continue
			}
			if scalarOf(mappingGet(item, "name")) == "" {
				continue
			}
			if hasVersion ||
				mappingGet(item, "columns") != nil ||
				mappingGet(item, "description") != nil ||
				mappingGet(item, "config") != nil ||
				mappingGet(item, "tests") != nil ||
				mappingGet(item, "data_tests") != nil {
				return true
			}
		}
	}
	for _, item := range sequenceItems(mappingGet(root, "sources")) {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if scalarOf(mappingGet(item, "name")) != "" && mappingGet(item, "tables") != nil {
			return true
		}
	}
	return false
}

// emitDbtSchemaColumns walks a model / source `columns:` sequence and
// emits a KindColumn node per entry, carrying description, data type,
// and the declared tests in Meta.
func emitDbtSchemaColumns(filePath, ownerID, ownerName string, item *yaml.Node, result *parser.ExtractionResult) {
	for _, col := range sequenceItems(mappingGet(item, "columns")) {
		if col.Kind != yaml.MappingNode {
			continue
		}
		colName := scalarOf(mappingGet(col, "name"))
		if colName == "" {
			continue
		}
		cm := map[string]any{}
		if desc := scalarOf(mappingGet(col, "description")); desc != "" {
			cm["description"] = truncateText(desc, 280)
		}
		dataType := scalarOf(mappingGet(col, "data_type"))
		if dataType == "" {
			dataType = scalarOf(mappingGet(col, "type"))
		}
		if dataType != "" {
			cm["data_type"] = dataType
		}
		if tests := dbtColumnTests(col); len(tests) > 0 {
			cm["tests"] = tests
		}
		emitDbtColumn(filePath, ownerID, ownerName, colName, "schema_yaml", cm, result)
	}
}

// dbtColumnTests collects the test names declared on a column. dbt
// accepts `tests:` (legacy) and `data_tests:` (1.8+); each entry is
// either a scalar test name or a single-key mapping (`relationships:`,
// `accepted_values:`, …).
func dbtColumnTests(col *yaml.Node) []string {
	var out []string
	for _, key := range []string{"tests", "data_tests"} {
		for _, t := range sequenceItems(mappingGet(col, key)) {
			switch t.Kind {
			case yaml.ScalarNode:
				if t.Value != "" {
					out = append(out, t.Value)
				}
			case yaml.MappingNode:
				if len(t.Content) >= 1 && t.Content[0] != nil && t.Content[0].Value != "" {
					out = append(out, t.Content[0].Value)
				}
			}
		}
	}
	return out
}

// dbtConfigMaterialized pulls the materialization out of a schema-YAML
// `config:` block (`config: { materialized: table }`).
func dbtConfigMaterialized(item *yaml.Node) string {
	cfg := mappingGet(item, "config")
	if cfg == nil {
		return ""
	}
	return scalarOf(mappingGet(cfg, "materialized"))
}

// ---------------------------------------------------------------------------
// shared node emission + helpers
// ---------------------------------------------------------------------------

// emitDbtColumn appends one KindColumn node and its EdgeMemberOf edge,
// merging the framework-agnostic Meta with any source-specific extra.
func emitDbtColumn(filePath, ownerID, ownerName, colName, source string, extra map[string]any, result *parser.ExtractionResult) {
	colName = strings.TrimSpace(stripSQLIdentQuotes(colName))
	if colName == "" {
		return
	}
	colID := ownerID + "::" + colName
	meta := map[string]any{
		"table":  ownerName,
		"column": colName,
		"source": source,
	}
	maps.Copy(meta, extra)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: colID, Kind: graph.KindColumn, Name: colName,
		FilePath: filePath, StartLine: 1, EndLine: 1,
		Language: "sql", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: colID, To: ownerID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: 1,
	})
}

// ensureDbtSourceNode appends a minimal KindTable node for a dbt source
// referenced by `source()` but not (yet) seen in a schema YAML. The
// schema-YAML pass overwrites it with the richer node when it runs.
func ensureDbtSourceNode(filePath, srcID, sourceName, tableName string, line int, result *parser.ExtractionResult) {
	for _, n := range result.Nodes {
		if n.ID == srcID {
			return
		}
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: srcID, Kind: graph.KindTable, Name: sourceName + "." + tableName,
		QualName: "dbt.source." + sourceName + "." + tableName,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "sql",
		Meta: map[string]any{
			"framework":     "dbt",
			"sql_type":      "dbt_source",
			"resource_type": "source",
			"source":        sourceName,
			"table":         tableName,
		},
	})
}

// dbtModelID / dbtSourceID / sqlmeshModelID centralise the synthetic
// node-ID conventions documented at the top of this file.
func dbtModelID(name string) string    { return "dbt::model::" + name }
func sqlmeshModelID(name string) string { return "sqlmesh::model::" + strings.ToLower(name) }

func dbtSourceID(source, table string) string {
	return "dbt::source::" + source + "." + table
}

// dbtModelNameFromPath derives a dbt model name from its file path:
// dbt keys a model by its bare filename stem.
func dbtModelNameFromPath(filePath string) string {
	base := filepath.Base(filePath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// sqlmeshLeafName returns the unqualified tail of a (possibly
// schema-qualified) SQLMesh model name.
func sqlmeshLeafName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// parseDbtConfig extracts the recognised keys from every
// `{{ config(...) }}` call in src. Later calls win on key collision.
func parseDbtConfig(src []byte) map[string]string {
	out := map[string]string{}
	for _, block := range dbtConfigRe.FindAllSubmatch(src, -1) {
		for _, kv := range dbtConfigKVRe.FindAllSubmatch(block[1], -1) {
			out[strings.ToLower(string(kv[1]))] = unquoteSQL(string(kv[2]))
		}
	}
	return out
}

// deJinja neutralises Jinja delimiters in a dbt model body so the
// regex SQL parsers can read the underlying query. `ref()` / `source()`
// calls collapse to a placeholder relation identifier (keeping FROM
// clauses parseable); every other `{{ }}` / `{% %}` / `{# #}` construct
// collapses to whitespace.
func deJinja(src []byte) string {
	s := jinjaCommentRe.ReplaceAllString(string(src), " ")
	s = jinjaStmtRe.ReplaceAllString(s, " ")
	s = dbtRefRe.ReplaceAllString(s, " __dbt_rel__ ")
	s = dbtSourceRe.ReplaceAllString(s, " __dbt_rel__ ")
	s = jinjaExprRe.ReplaceAllString(s, " __dbt_expr__ ")
	return s
}

// stripSQLComments removes `--` line comments and `/* */` block
// comments so they cannot leak FROM-like text into the regex parsers.
func stripSQLComments(s string) string {
	s = sqlBlockCommentRe.ReplaceAllString(s, " ")
	s = sqlLineCommentRe.ReplaceAllString(s, "")
	return s
}

// unquoteSQL strips a single matched pair of single or double quotes.
func unquoteSQL(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		f, l := s[0], s[len(s)-1]
		if (f == '\'' && l == '\'') || (f == '"' && l == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// stripSQLIdentQuotes strips ANSI / MySQL / T-SQL identifier quoting.
func stripSQLIdentQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		f, l := s[0], s[len(s)-1]
		switch {
		case f == '"' && l == '"', f == '`' && l == '`', f == '[' && l == ']':
			return s[1 : len(s)-1]
		}
	}
	return s
}

// firstToken returns the first whitespace-delimited token of s.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '('
	}); i >= 0 {
		return s[:i]
	}
	return s
}

// splitFirstToken splits s into its first whitespace-delimited token
// and the trimmed remainder.
func splitFirstToken(s string) (token, rest string) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

// splitTopLevelCommas splits s on commas that sit at paren depth zero
// and outside single/double-quoted string literals.
func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	var quote byte
	cur := strings.Builder{}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// scanMatchingParen returns the index of the `)` that closes the `(`
// at openIdx, skipping single/double-quoted strings and `--` line
// comments. Returns -1 when unbalanced.
func scanMatchingParen(src []byte, openIdx int) int {
	depth := 0
	var quote byte
	for i := openIdx; i < len(src); i++ {
		c := src[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '-':
			if i+1 < len(src) && src[i+1] == '-' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// lineCount returns the number of lines in src (at least 1).
func lineCount(src []byte) int {
	n := 1
	for _, b := range src {
		if b == '\n' {
			n++
		}
	}
	return n
}

// truncateText caps s at max bytes, appending an ellipsis when cut.
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
