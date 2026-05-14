package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/sql"
)

// SQLExtractor extracts SQL source files.
type SQLExtractor struct {
	lang *sitter.Language
}

func NewSQLExtractor() *SQLExtractor {
	return &SQLExtractor{lang: sql.GetLanguage()}
}

func (e *SQLExtractor) Language() string     { return "sql" }
func (e *SQLExtractor) Extensions() []string { return []string{".sql"} }

func (e *SQLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "sql",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// Specialised SQL dispatch: dbt and SQLMesh models are `.sql`
	// files but carry their own model/column/lineage graph shape, so
	// they short-circuit the generic DDL walk below.
	switch classifySQLFile(filePath, src) {
	case "dbt":
		extractDbtSQLModel(filePath, fileNode.ID, src, result)
		return result, nil
	case "sqlmesh":
		extractSQLMeshSQLModel(filePath, fileNode.ID, src, result)
		return result, nil
	}

	seen := make(map[string]bool)

	// Walk top-level statements.
	for i := 0; i < int(root.NamedChildCount()); i++ {
		stmt := root.NamedChild(i)
		if stmt.Type() != "statement" {
			continue
		}
		for j := 0; j < int(stmt.NamedChildCount()); j++ {
			child := stmt.NamedChild(j)
			switch child.Type() {
			case "create_table":
				e.extractCreateTable(child, src, filePath, fileNode.ID, seen, result)
			case "create_view":
				e.extractCreateView(child, src, filePath, fileNode.ID, seen, result)
			case "create_function":
				e.extractCreateFunction(child, src, filePath, fileNode.ID, seen, result)
			case "create_index":
				e.extractCreateIndex(child, src, filePath, fileNode.ID, seen, result)
			case "create_trigger":
				e.extractCreateTrigger(child, src, filePath, fileNode.ID, seen, result)
			}
		}
	}

	return result, nil
}

func (e *SQLExtractor) extractCreateTable(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findObjectName(node, src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: map[string]any{"sql_type": "table"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})

	// Extract column names as variables with EdgeMemberOf.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "column_definitions" {
			for k := 0; k < int(child.NamedChildCount()); k++ {
				col := child.NamedChild(k)
				if col.Type() == "column_definition" {
					colName := firstNamedChildOfType(col, "identifier", src)
					if colName != "" {
						colID := id + "." + colName
						if !seen[colID] {
							seen[colID] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: colID, Kind: graph.KindVariable, Name: colName,
								FilePath: filePath, StartLine: int(col.StartPoint().Row) + 1, EndLine: int(col.EndPoint().Row) + 1,
								Language: "sql",
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: colID, To: id, Kind: graph.EdgeMemberOf,
								FilePath: filePath, Line: int(col.StartPoint().Row) + 1,
							})
						}
					}
				}
			}
		}
	}
}

func (e *SQLExtractor) extractCreateView(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findObjectName(node, src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: map[string]any{"sql_type": "view"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *SQLExtractor) extractCreateFunction(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findObjectName(node, src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *SQLExtractor) extractCreateIndex(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findObjectName(node, src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: map[string]any{"sql_type": "index"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *SQLExtractor) extractCreateTrigger(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findObjectName(node, src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "sql", Meta: map[string]any{"sql_type": "trigger"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

// findObjectName extracts the name from a CREATE statement by finding
// the first object_reference > identifier child.
func findObjectName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "object_reference" {
			return firstNamedChildOfType(child, "identifier", src)
		}
	}
	// Fallback: first identifier child directly.
	return firstNamedChildOfType(node, "identifier", src)
}

func firstNamedChildOfType(node *sitter.Node, nodeType string, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == nodeType {
			return child.Content(src)
		}
	}
	return ""
}

var _ parser.Extractor = (*SQLExtractor)(nil)
