package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/css"
)

// Tree-sitter query patterns for CSS files.
const (
	qCssImport = `(import_statement) @import.def`
	qCssClass  = `(class_selector (class_name) @class.name) @class.def`
	qCssId     = `(id_selector (id_name) @id.name) @id.def`
)

// CSSExtractor extracts CSS files into graph nodes and edges. Only
// three queries, so the merged-alternation pattern wouldn't pay;
// precompiling each query once at init still removes the per-file
// sitter.NewQuery cost.
type CSSExtractor struct {
	lang    *sitter.Language
	qImport *parser.PreparedQuery
	qClass  *parser.PreparedQuery
	qID     *parser.PreparedQuery
}

func NewCSSExtractor() *CSSExtractor {
	lang := css.GetLanguage()
	return &CSSExtractor{
		lang:    lang,
		qImport: parser.MustPreparedQuery(qCssImport, lang),
		qClass:  parser.MustPreparedQuery(qCssClass, lang),
		qID:     parser.MustPreparedQuery(qCssId, lang),
	}
}

func (e *CSSExtractor) Language() string     { return "css" }
func (e *CSSExtractor) Extensions() []string { return []string{".css"} }

func (e *CSSExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "css",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// @import rules.
	e.extractImports(root, src, filePath, fileNode.ID, result)

	// Class selectors.
	e.extractClasses(root, src, filePath, fileNode.ID, result, seen)

	// ID selectors.
	e.extractIDs(root, src, filePath, fileNode.ID, result, seen)

	// CSS custom properties (--variable-name).
	e.extractCustomProperties(root, src, filePath, fileNode.ID, result, seen)

	return result, nil
}

func (e *CSSExtractor) extractImports(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	parser.EachMatch(e.qImport, root, src, func(m parser.QueryResult) {
		def := m.Captures["import.def"]
		importText := def.Text
		// Extract the path from @import url("...") or @import "..."
		importPath := extractCSSImportPath(importText)
		if importPath == "" {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     fileID,
			To:       "unresolved::import::" + importPath,
			Kind:     graph.EdgeImports,
			FilePath: filePath,
			Line:     def.StartLine + 1,
		})
	})
}

func (e *CSSExtractor) extractClasses(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	parser.EachMatch(e.qClass, root, src, func(m parser.QueryResult) {
		name := m.Captures["class.name"].Text
		def := m.Captures["class.def"]
		id := filePath + "::." + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: "." + name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "css",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: def.StartLine + 1,
		})
	})
}

func (e *CSSExtractor) extractIDs(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	parser.EachMatch(e.qID, root, src, func(m parser.QueryResult) {
		name := m.Captures["id.name"].Text
		def := m.Captures["id.def"]
		id := filePath + "::#" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: "#" + name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "css",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: def.StartLine + 1,
		})
	})
}

func (e *CSSExtractor) extractCustomProperties(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	// Walk the AST to find declaration nodes with property_name starting with "--".
	e.walkForCustomProps(root, src, filePath, fileID, result, seen)
}

func (e *CSSExtractor) walkForCustomProps(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	if node.Type() == "declaration" {
		// Look for property_name child that starts with "--".
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child != nil && child.Type() == "property_name" {
				propName := child.Content(src)
				if strings.HasPrefix(propName, "--") {
					id := filePath + "::" + propName
					if !seen[id] {
						seen[id] = true
						result.Nodes = append(result.Nodes, &graph.Node{
							ID: id, Kind: graph.KindVariable, Name: propName,
							FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
							Language: "css", Meta: map[string]any{
								"custom_property": true,
							},
						})
						result.Edges = append(result.Edges, &graph.Edge{
							From: fileID, To: id, Kind: graph.EdgeDefines,
							FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
						})
					}
				}
				break
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil {
			e.walkForCustomProps(child, src, filePath, fileID, result, seen)
		}
	}
}

// extractCSSImportPath extracts the path from an @import statement.
// Handles: @import url("path"); @import url('path'); @import "path"; @import 'path';
func extractCSSImportPath(text string) string {
	text = strings.TrimPrefix(text, "@import")
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(text, ";")
	text = strings.TrimSpace(text)

	// Handle url("...") or url('...')
	if strings.HasPrefix(text, "url(") {
		text = strings.TrimPrefix(text, "url(")
		text = strings.TrimSuffix(text, ")")
		text = strings.TrimSpace(text)
	}

	text = strings.Trim(text, `"'`)
	return text
}
