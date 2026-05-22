package languages

import (
	"bytes"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	tree_sitter_markdown "github.com/zzet/gortex/internal/parser/tsitter/markdown"
)

// linkPattern matches inline markdown links: [text](target)
var linkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// wikiLinkPattern matches wiki-style links: [[target]], [[target|label]],
// [[target#heading]].
var wikiLinkPattern = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

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

	// YAML frontmatter relations — wiki-links, document paths, and tags
	// declared in the `---` fenced block at the top of the file.
	e.extractFrontmatterRelations(src, filePath, fileNode.ID, seenLinks, result)

	// Walk the AST for headings, code blocks, and inline content.
	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		switch node.Type() {
		case "atx_heading":
			e.extractHeading(node, src, filePath, fileNode.ID, seen, result)
		case "fenced_code_block":
			e.extractCodeBlock(node, src, filePath, fileNode.ID, seen, result)
		case "paragraph", "inline":
			// Extract inline and wiki links from inline text.
			text := node.Content(src)
			line := int(node.StartPoint().Row) + 1
			e.extractLinks(text, filePath, fileNode.ID, seenLinks, result, line)
			e.extractWikiLinks(text, filePath, fileNode.ID, seenLinks, result, line)
		}

		for i := 0; i < int(node.NamedChildCount()); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)

	// Second pass: emit first-class KindDoc prose-section nodes --
	// one per heading-delimited region, carrying the section body so
	// it is searchable, not just the heading text.
	e.extractProseSections(root, src, filePath, fileNode.ID, result)

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
		if target == "" {
			continue
		}
		emitMarkdownLink(fileID, filePath, target, line, seen, result)
	}
}

// extractWikiLinks emits a link edge for every wiki-style [[target]]
// reference in text — the Obsidian / wiki convention the inline
// [text](path) form does not cover.
func (e *MarkdownExtractor) extractWikiLinks(text, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult, line int) {
	for _, m := range wikiLinkPattern.FindAllStringSubmatch(text, -1) {
		if target := normalizeWikiTarget(m[1]); target != "" {
			emitMarkdownLink(fileID, filePath, target, line, seen, result)
		}
	}
}

// normalizeWikiTarget reduces a wiki-link body to its target page:
// [[target|label]] → target, [[target#heading]] → target. A pure
// same-page anchor ([[#heading]]) yields "".
func normalizeWikiTarget(inner string) string {
	t := strings.TrimSpace(inner)
	if i := strings.IndexByte(t, '|'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// emitMarkdownLink appends a document-to-document link edge, deduped
// by target so the same relation is recorded once per file.
func emitMarkdownLink(fileID, filePath, target string, line int, seen map[string]bool, result *parser.ExtractionResult) {
	to := "unresolved::import::" + target
	if seen[to] {
		return
	}
	seen[to] = true
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: to, Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	})
}

// extractFrontmatterRelations parses a leading YAML frontmatter block
// and emits a relation edge for every wiki-link, markdown-document
// path, and tag it declares.
func (e *MarkdownExtractor) extractFrontmatterRelations(src []byte, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	fm := frontmatterBlock(src)
	if len(fm) == 0 {
		return
	}
	var doc map[string]any
	if err := yaml.Unmarshal(fm, &doc); err != nil || doc == nil {
		return
	}
	for key, val := range doc {
		e.emitFrontmatterValue(key, val, filePath, fileID, seen, result)
	}
}

// frontmatterBlock returns the YAML text between a leading `---` fence
// and its closing `---` / `...` fence, or nil when the file carries no
// frontmatter.
func frontmatterBlock(src []byte) []byte {
	s := bytes.TrimPrefix(src, []byte{0xEF, 0xBB, 0xBF})
	lines := bytes.SplitAfter(s, []byte("\n"))
	if len(lines) == 0 || strings.TrimRight(string(lines[0]), "\r\n") != "---" {
		return nil
	}
	var body [][]byte
	for _, ln := range lines[1:] {
		switch strings.TrimRight(string(ln), "\r\n") {
		case "---", "...":
			return bytes.Join(body, nil)
		}
		body = append(body, ln)
	}
	return nil
}

// emitFrontmatterValue walks a decoded frontmatter value — scalar,
// sequence, or nested mapping — emitting a relation edge for each
// wiki-link, markdown path, or tag string it finds.
func (e *MarkdownExtractor) emitFrontmatterValue(key string, val any, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	switch v := val.(type) {
	case string:
		e.emitFrontmatterString(key, v, filePath, fileID, seen, result)
	case []any:
		for _, item := range v {
			e.emitFrontmatterValue(key, item, filePath, fileID, seen, result)
		}
	case map[string]any:
		for k, item := range v {
			e.emitFrontmatterValue(k, item, filePath, fileID, seen, result)
		}
	}
}

// emitFrontmatterString classifies one frontmatter scalar: a `tags`
// entry becomes a topic relation, a value carrying a [[wiki-link]] or
// a bare markdown-document path becomes a document relation.
func (e *MarkdownExtractor) emitFrontmatterString(key, val, filePath, fileID string, seen map[string]bool, result *parser.ExtractionResult) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	// `tags:` / `tag:` — each value is a topic relation.
	if key == "tags" || key == "tag" {
		to := "unresolved::tag::" + val
		if !seen[to] {
			seen[to] = true
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileID, To: to, Kind: graph.EdgeReferences, FilePath: filePath, Line: 1,
			})
		}
		return
	}
	// A wiki-link relation expressed in frontmatter (related: "[[Foo]]").
	if matches := wikiLinkPattern.FindAllStringSubmatch(val, -1); matches != nil {
		for _, m := range matches {
			if t := normalizeWikiTarget(m[1]); t != "" {
				emitMarkdownLink(fileID, filePath, t, 1, seen, result)
			}
		}
		return
	}
	// A bare markdown-document path (parent: ../index.md).
	if isMarkdownDocPath(val) {
		emitMarkdownLink(fileID, filePath, val, 1, seen, result)
	}
}

// isMarkdownDocPath reports whether val is a local path to a markdown
// document — used to treat a frontmatter scalar as a document relation.
func isMarkdownDocPath(val string) bool {
	if strings.Contains(val, "://") {
		return false
	}
	lower := strings.ToLower(val)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".mdx")
}

var _ parser.Extractor = (*MarkdownExtractor)(nil)
