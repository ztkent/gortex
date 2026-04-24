package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/toml"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// TOMLExtractor extracts TOML files into graph nodes and edges.
// Tables ([section]) become KindType, key-value pairs become KindVariable.
type TOMLExtractor struct {
	lang *sitter.Language
}

func NewTOMLExtractor() *TOMLExtractor {
	return &TOMLExtractor{lang: toml.GetLanguage()}
}

func (e *TOMLExtractor) Language() string     { return "toml" }
func (e *TOMLExtractor) Extensions() []string { return []string{".toml"} }

func (e *TOMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "toml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	e.walk(root, src, filePath, fileNode.ID, result, seen)

	return result, nil
}

func (e *TOMLExtractor) walk(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	switch nodeType {
	case "table":
		// Table header: first child is typically "[" then the key/dotted_key.
		// Look for a child that is a key or bare_key.
		name := e.extractTableName(node, src)
		if name != "" && !seen["table::"+name] {
			seen["table::"+name] = true
			id := filePath + "::[" + name + "]"
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindType, Name: "[" + name + "]",
				FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
				Language: "toml",
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: id, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
			})
		}

	case "pair":
		// Key-value pair. First named child should be the key.
		keyNode := e.findChild(node, "bare_key")
		if keyNode == nil {
			keyNode = e.findChild(node, "dotted_key")
		}
		if keyNode == nil {
			keyNode = e.findChild(node, "quoted_key")
		}
		if keyNode != nil {
			keyName := keyNode.Content(src)
			if keyName != "" && !seen["pair::"+keyName] {
				seen["pair::"+keyName] = true
				id := filePath + "::" + keyName
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: graph.KindVariable, Name: keyName,
					FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
					Language: "toml",
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileID, To: id, Kind: graph.EdgeDefines,
					FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
				})
			}
		}
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil {
			e.walk(child, src, filePath, fileID, result, seen)
		}
	}
}

func (e *TOMLExtractor) extractTableName(tableNode *sitter.Node, src []byte) string {
	// Walk children looking for bare_key or dotted_key inside brackets.
	for i := 0; i < int(tableNode.ChildCount()); i++ {
		child := tableNode.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "bare_key" || ct == "dotted_key" || ct == "quoted_key" {
			return child.Content(src)
		}
		// Some grammars wrap the key in a different node.
		if ct != "[" && ct != "]" && ct != "comment" {
			text := child.Content(src)
			if text != "" && text != "[" && text != "]" {
				return text
			}
		}
	}
	return ""
}

func (e *TOMLExtractor) findChild(node *sitter.Node, childType string) *sitter.Node {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil && child.Type() == childType {
			return child
		}
	}
	return nil
}
