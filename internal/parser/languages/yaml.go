package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/yaml"
)

// YAMLExtractor extracts YAML files into graph nodes and edges.
// It focuses on top-level keys as KindVariable.
type YAMLExtractor struct {
	lang *sitter.Language
}

func NewYAMLExtractor() *YAMLExtractor {
	return &YAMLExtractor{lang: yaml.GetLanguage()}
}

func (e *YAMLExtractor) Language() string { return "yaml" }
func (e *YAMLExtractor) Extensions() []string {
	// `.yaml` / `.yml` cover the bulk of YAML files (including
	// `kustomization.yaml`). `Kustomization` is the bare-basename
	// form Kustomize accepts when no extension is desired — it
	// must be registered as a basename so the registry routes it
	// to the YAML extractor (which then dispatches into the
	// kustomize path inside Extract).
	return []string{".yaml", ".yml", "Kustomization"}
}

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

	// Specialised YAML dispatch. Order matters:
	//   1. Kustomize files have a fixed basename — short-circuit.
	//   2. K8s manifests are detected by content (apiVersion+kind).
	//   3. dbt schema / properties files are detected by content
	//      fingerprint (models/sources/seeds/snapshots + columns).
	//   4. Otherwise fall through to the generic top-level-keys
	//      walker so plain config YAMLs still index.
	if isKustomizationFile(filePath) {
		extractKustomizeYAML(filePath, fileNode.ID, src, result)
		return result, nil
	}
	if extractKubernetesYAML(filePath, fileNode.ID, src, result) {
		return result, nil
	}
	if extractDbtSchemaYAML(filePath, fileNode.ID, src, result) {
		return result, nil
	}

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
