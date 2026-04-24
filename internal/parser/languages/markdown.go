package languages

import (
	"regexp"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	tree_sitter_markdown "github.com/zzet/gortex/internal/parser/tsitter/markdown"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// linkPattern matches markdown links: [text](target)
var linkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// MarkdownExtractor extracts Markdown document structure.
type MarkdownExtractor struct {
	lang *sitter.Language
}

func NewMarkdownExtractor() *MarkdownExtractor {
	return &MarkdownExtractor{lang: tree_sitter_markdown.GetLanguage()}
}

func (e *MarkdownExtractor) Language() string     { return "markdown" }
func (e *MarkdownExtractor) Extensions() []string { return []string{".md", ".mdx"} }

func (e *MarkdownExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "markdown",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	seenLinks := make(map[string]bool)

	// Walk the AST for headings, code blocks, and inline content.
	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		switch node.Type() {
		case "atx_heading":
			e.extractHeading(node, src, filePath, fileNode.ID, seen, result)
		case "fenced_code_block":
			e.extractCodeBlock(node, src, filePath, fileNode.ID, seen, result)
		case "paragraph", "inline":
			// Extract links from inline text.
			text := node.Content(src)
			e.extractLinks(text, filePath, fileNode.ID, seenLinks, result, int(node.StartPoint().Row)+1)
		}

		for i := 0; i < int(node.NamedChildCount()); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)

	return result, nil
}

func (e *MarkdownExtractor) extractHeading(node *sitter.Node, src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	// Get heading level from marker type.
	level := 0
	var headingText string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "atx_h1_marker":
			level = 1
		case "atx_h2_marker":
			level = 2
		case "atx_h3_marker":
			level = 3
		case "atx_h4_marker":
			level = 4
		case "atx_h5_marker":
			level = 5
		case "atx_h6_marker":
			level = 6
		case "inline":
			headingText = strings.TrimSpace(child.Content(src))
		}
	}

	if headingText == "" || level == 0 {
		return
	}

	id := filePath + "::h" + string(rune('0'+level)) + ":" + headingText
	if seen[id] {
		return
	}
	seen[id] = true

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: headingText,
		FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
		Language: "markdown", Meta: map[string]any{"heading_level": level},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
	})
}

func (e *MarkdownExtractor) extractCodeBlock(node *sitter.Node, src []byte, filePath, _ string, seen map[string]bool, result *parser.ExtractionResult) {
	// Extract language from info_string.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "info_string" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				langNode := child.NamedChild(j)
				if langNode.Type() == "language" {
					lang := strings.TrimSpace(langNode.Content(src))
					if lang != "" {
						id := filePath + "::codeblock:" + lang + ":" + string(rune('0'+int(node.StartPoint().Row)))
						if !seen[id] {
							seen[id] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: id, Kind: graph.KindVariable, Name: lang + " code block",
								FilePath: filePath, StartLine: int(node.StartPoint().Row) + 1, EndLine: int(node.EndPoint().Row) + 1,
								Language: "markdown", Meta: map[string]any{"code_language": lang},
							})
						}
					}
				}
			}
		}
	}
}

func (e *MarkdownExtractor) extractLinks(text, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult, line int) {
	matches := linkPattern.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		target := m[2]
		// Skip external URLs and anchors.
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "#") {
			continue
		}
		// Strip anchor from local links: "file.md#section" → "file.md"
		if idx := strings.Index(target, "#"); idx > 0 {
			target = target[:idx]
		}
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::import::" + target,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
}

var _ parser.Extractor = (*MarkdownExtractor)(nil)
