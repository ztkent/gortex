package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/hcl"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// HCLExtractor extracts HCL/Terraform files into graph nodes and edges.
// Blocks (resource, data, module, variable, output) become KindType with block labels as name.
type HCLExtractor struct {
	lang *sitter.Language
}

func NewHCLExtractor() *HCLExtractor {
	return &HCLExtractor{lang: hcl.GetLanguage()}
}

func (e *HCLExtractor) Language() string     { return "hcl" }
func (e *HCLExtractor) Extensions() []string { return []string{".tf", ".tfvars", ".hcl"} }

func (e *HCLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "hcl",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	e.walk(root, src, filePath, fileNode.ID, result, seen)

	return result, nil
}

func (e *HCLExtractor) walk(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	if node == nil {
		return
	}

	nodeType := node.Type()

	// HCL tree-sitter grammar uses "block" for top-level blocks.
	if nodeType == "block" {
		e.extractBlock(node, src, filePath, fileID, result, seen)
	}

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child != nil {
			e.walk(child, src, filePath, fileID, result, seen)
		}
	}
}

func (e *HCLExtractor) extractBlock(node *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	// A block has: identifier (block type), then string_lit labels, then block_body.
	// E.g., resource "aws_instance" "web" { ... }
	// Children: "resource", "\"aws_instance\"", "\"web\"", body

	var blockType string
	var labels []string

	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		switch ct {
		case "identifier":
			if blockType == "" {
				blockType = child.Content(src)
			}
		case "string_lit":
			text := child.Content(src)
			// Strip quotes.
			text = trimQuotes(text)
			if text != "" {
				labels = append(labels, text)
			}
		}
	}

	if blockType == "" {
		return
	}

	// Build name from block type and labels.
	name := blockType
	for _, l := range labels {
		name += "." + l
	}

	if seen[name] {
		return
	}
	seen[name] = true

	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "hcl",
		Meta: map[string]any{
			"block_type": blockType,
			"labels":     labels,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
