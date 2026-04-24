package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/yaml"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// YAMLExtractor extracts YAML files into graph nodes and edges.
// It focuses on top-level keys as KindVariable.
type YAMLExtractor struct {
	lang *sitter.Language
}

func NewYAMLExtractor() *YAMLExtractor {
	return &YAMLExtractor{lang: yaml.GetLanguage()}
}

func (e *YAMLExtractor) Language() string     { return "yaml" }
func (e *YAMLExtractor) Extensions() []string { return []string{".yaml", ".yml"} }

func (e *YAMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "yaml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// Walk only top-level block_mapping_pair nodes.
	e.extractTopLevelKeys(root, src, filePath, fileNode.ID, result)

	return result, nil
}

func (e *YAMLExtractor) extractTopLevelKeys(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	// YAML tree-sitter grammar: stream -> document -> block_node -> block_mapping -> block_mapping_pair
	// We walk looking for block_mapping_pair nodes at the top-level mapping only.
	seen := make(map[string]bool)
	e.findTopLevelPairs(root, src, filePath, fileID, result, seen, 0)
}

func (e *YAMLExtractor) findTopLevelPairs(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool, depth int) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	// If we hit a block_mapping_pair at the top-level mapping, extract the key.
	if nodeType == "block_mapping_pair" && depth <= 5 {
		// The first child is typically the key.
		keyNode := node.ChildByFieldName("key")
		if keyNode == nil && node.ChildCount() > 0 {
			keyNode = node.Child(0)
		}
		if keyNode != nil {
			keyName := keyNode.Content(src)
			if keyName != "" && !seen[keyName] {
				seen[keyName] = true
				id := filePath + "::" + keyName
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: graph.KindVariable, Name: keyName,
					FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
					Language: "yaml",
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileID, To: id, Kind: graph.EdgeDefines,
					FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
				})
			}
		}
		return // Don't recurse into nested mappings.
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil {
			e.findTopLevelPairs(child, src, filePath, fileID, result, seen, depth+1)
		}
	}
}
