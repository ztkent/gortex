package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/dockerfile"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// DockerfileExtractor extracts Dockerfile files into graph nodes and edges.
// FROM creates EdgeImports (base images), ENV/ARG become KindVariable,
// RUN/CMD/ENTRYPOINT/COPY are extracted as notable instructions.
type DockerfileExtractor struct {
	lang *sitter.Language
}

func NewDockerfileExtractor() *DockerfileExtractor {
	return &DockerfileExtractor{lang: dockerfile.GetLanguage()}
}

func (e *DockerfileExtractor) Language() string     { return "dockerfile" }
func (e *DockerfileExtractor) Extensions() []string {
	return []string{".dockerfile", "Dockerfile", "Containerfile"}
}

func (e *DockerfileExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "dockerfile",
	}
	result.Nodes = append(result.Nodes, fileNode)

	e.walk(root, src, filePath, fileNode.ID, result)

	return result, nil
}

func (e *DockerfileExtractor) walk(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	switch nodeType {
	case "from_instruction":
		e.extractFrom(node, src, filePath, fileID, result)
	case "env_instruction":
		e.extractEnvArg(node, src, filePath, fileID, result, "ENV")
	case "arg_instruction":
		e.extractEnvArg(node, src, filePath, fileID, result, "ARG")
	case "run_instruction", "cmd_instruction", "entrypoint_instruction", "copy_instruction":
		e.extractInstruction(node, src, filePath, fileID, result, nodeType)
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil {
			e.walk(child, src, filePath, fileID, result)
		}
	}
}

func (e *DockerfileExtractor) extractFrom(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	// FROM instruction children typically include image_spec.
	// We look for the image name in children.
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "image_spec" {
			imageName := child.Content(src)
			imageName = strings.TrimSpace(imageName)
			if imageName != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     fileID,
					To:       "unresolved::import::" + imageName,
					Kind:     graph.EdgeImports,
					FilePath: filePath,
					Line:     int(node.StartPoint().Row) + 1,
				})
			}
			return
		}
	}
}

func (e *DockerfileExtractor) extractEnvArg(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, prefix string) {
	// ENV/ARG instructions have key=value or key value pairs.
	// We extract the variable name from children.
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		// Look for env_pair, unquoted_string, or env_key.
		if ct == "env_pair" {
			// env_pair has name and value children.
			nameNode := e.findChildOfType(child, "unquoted_string")
			if nameNode == nil {
				nameNode = e.findChildOfType(child, "env_key")
			}
			if nameNode != nil {
				varName := nameNode.Content(src)
				e.addVariable(varName, prefix, node, filePath, fileID, result)
			}
		} else if ct == "unquoted_string" && i == 1 {
			// ARG name or ARG name=value — first non-keyword child.
			varName := child.Content(src)
			// Strip =value if present.
			if idx := strings.Index(varName, "="); idx > 0 {
				varName = varName[:idx]
			}
			e.addVariable(varName, prefix, node, filePath, fileID, result)
		}
	}
}

func (e *DockerfileExtractor) addVariable(varName, prefix string, node *sitter.Node, filePath, fileID string, result *parser.ExtractionResult) {
	if varName == "" {
		return
	}
	id := filePath + "::" + prefix + "." + varName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: prefix + " " + varName,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "dockerfile",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *DockerfileExtractor) extractInstruction(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, instrType string) {
	// Extract instruction as a variable for visibility.
	label := strings.TrimSuffix(instrType, "_instruction")
	label = strings.ToUpper(label)
	text := node.Content(src)
	// Truncate long instructions.
	if len(text) > 80 {
		text = text[:77] + "..."
	}
	id := filePath + "::" + label + "::" + strings.ReplaceAll(text, "\n", " ")
	if len(id) > 200 {
		id = id[:200]
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: label,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "dockerfile",
		Meta: map[string]any{
			"instruction": label,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *DockerfileExtractor) findChildOfType(node *sitter.Node, childType string) *sitter.Node {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil && child.Type() == childType {
			return child
		}
	}
	return nil
}
