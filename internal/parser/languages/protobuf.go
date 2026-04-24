package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/protobuf"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type ProtobufExtractor struct {
	lang *sitter.Language
}

func NewProtobufExtractor() *ProtobufExtractor {
	return &ProtobufExtractor{lang: protobuf.GetLanguage()}
}

func (e *ProtobufExtractor) Language() string     { return "protobuf" }
func (e *ProtobufExtractor) Extensions() []string { return []string{".proto"} }

func (e *ProtobufExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "protobuf",
	}
	result.Nodes = append(result.Nodes, fileNode)
	seen := make(map[string]bool)

	for i := 0; i < int(root.NamedChildCount()); i++ {
		child := root.NamedChild(i)
		switch child.Type() {
		case "message":
			e.extractMessage(child, src, filePath, fileNode.ID, seen, result)
		case "service":
			e.extractService(child, src, filePath, fileNode.ID, seen, result)
		case "enum":
			e.extractEnum(child, src, filePath, fileNode.ID, seen, result)
		case "import":
			e.extractImport(child, src, filePath, fileNode.ID, result)
		}
	}

	return result, nil
}

func (e *ProtobufExtractor) extractMessage(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findProtoName(node, "message_name", src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "protobuf", Meta: map[string]any{"proto_type": "message"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})

	for j := 0; j < int(node.NamedChildCount()); j++ {
		body := node.NamedChild(j)
		if body.Type() == "message_body" {
			for k := 0; k < int(body.NamedChildCount()); k++ {
				field := body.NamedChild(k)
				if field.Type() == "field" {
					fieldName := findDirectIdent(field, src)
					if fieldName != "" {
						fieldID := id + "." + fieldName
						if !seen[fieldID] {
							seen[fieldID] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: fieldID, Kind: graph.KindVariable, Name: fieldName,
								FilePath: filePath, StartLine: int(field.StartPoint().Row) + 1, EndLine: int(field.EndPoint().Row) + 1,
								Language: "protobuf",
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: fieldID, To: id, Kind: graph.EdgeMemberOf,
								FilePath: filePath, Line: int(field.StartPoint().Row) + 1,
							})
						}
					}
				}
			}
		}
	}
}

func (e *ProtobufExtractor) extractService(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findProtoName(node, "service_name", src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name

	var methods []string
	for j := 0; j < int(node.NamedChildCount()); j++ {
		child := node.NamedChild(j)
		if child.Type() == "rpc" {
			rpcName := findProtoName(child, "rpc_name", src)
			if rpcName != "" {
				methods = append(methods, rpcName)
			}
		}
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "protobuf", Meta: map[string]any{"methods": methods},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})

	for j := 0; j < int(node.NamedChildCount()); j++ {
		child := node.NamedChild(j)
		if child.Type() == "rpc" {
			rpcName := findProtoName(child, "rpc_name", src)
			if rpcName != "" {
				rpcID := id + "." + rpcName
				if !seen[rpcID] {
					seen[rpcID] = true
					result.Nodes = append(result.Nodes, &graph.Node{
						ID: rpcID, Kind: graph.KindMethod, Name: rpcName,
						FilePath: filePath, StartLine: int(child.StartPoint().Row) + 1, EndLine: int(child.EndPoint().Row) + 1,
						Language: "protobuf",
					})
					result.Edges = append(result.Edges, &graph.Edge{
						From: fileID, To: rpcID, Kind: graph.EdgeDefines,
						FilePath: filePath, Line: int(child.StartPoint().Row) + 1,
					})
					result.Edges = append(result.Edges, &graph.Edge{
						From: rpcID, To: id, Kind: graph.EdgeMemberOf,
						FilePath: filePath, Line: int(child.StartPoint().Row) + 1,
					})
				}
			}
		}
	}
}

func (e *ProtobufExtractor) extractEnum(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	name := findProtoName(node, "enum_name", src)
	if name == "" || seen[name] {
		return
	}
	seen[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "protobuf", Meta: map[string]any{"proto_type": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *ProtobufExtractor) extractImport(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	for j := 0; j < int(node.NamedChildCount()); j++ {
		child := node.NamedChild(j)
		if child.Type() == "string" {
			path := child.Content(src)
			if len(path) >= 2 {
				path = path[1 : len(path)-1]
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: "unresolved::import::" + path,
				Kind: graph.EdgeImports, FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
			})
		}
	}
}

func findProtoName(node *sitter.Node, nameType string, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == nameType {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				id := child.NamedChild(j)
				if id.Type() == "identifier" {
					return id.Content(src)
				}
			}
			return child.Content(src)
		}
	}
	return ""
}

func findDirectIdent(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

var _ parser.Extractor = (*ProtobufExtractor)(nil)
