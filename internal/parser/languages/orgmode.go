package languages

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	tree_sitter_orgmode "github.com/zzet/gortex/internal/parser/tsitter/orgmode"
)

// headingPattern matches an Org headline: stars, optional TODO/DONE keyword,
// optional priority cookie, optional COMMENT, and the remaining title text.
var headingPattern = regexp.MustCompile(
	`^(\*+)\s+(?:(TODO|DONE)\s+)?(?:(\[#[A-Za-z0-9]\])\s+)?(?:(COMMENT)\s*)?(.*?)\s*$`,
)

// keywordKeyPattern strips the leading `#+` and trailing `:` from a
// keyword_key node so we can match against canonical names like TITLE.
var keywordKeyPattern = regexp.MustCompile(`^#\+(.+?):?$`)

// OrgModeExtractor extracts Org-mode document structure: headlines (with
// TODO state and priority cookies), greater blocks (BEGIN_SRC, BEGIN_QUOTE,
// ...), file-level keywords (#+TITLE, #+AUTHOR, ...), and intra-repo links.
type OrgModeExtractor struct {
	lang *sitter.Language
}

func NewOrgModeExtractor() *OrgModeExtractor {
	return &OrgModeExtractor{lang: tree_sitter_orgmode.GetLanguage()}
}

func (e *OrgModeExtractor) Language() string     { return "orgmode" }
func (e *OrgModeExtractor) Extensions() []string { return []string{".org"} }

func (e *OrgModeExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	// Upstream grammar fails to recognize a level-1 heading when the file
	// starts at byte 0 with `*`. Prepend a newline and shift row numbers
	// back by one when emitting graph positions.
	rowOffset := 0
	parseSrc := src
	if len(src) == 0 || src[0] != '\n' {
		parseSrc = append([]byte{'\n'}, src...)
		rowOffset = 1
	}

	tree, err := parser.ParseFile(parseSrc, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	endRow := max(int(root.EndPoint().Row)-rowOffset, 0)
	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: endRow + 1,
		Language: "orgmode",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	seenLinks := make(map[string]bool)

	startLine := func(n *sitter.Node) int {
		return max(int(n.StartPoint().Row)-rowOffset, 0) + 1
	}
	endLine := func(n *sitter.Node) int {
		return max(int(n.EndPoint().Row)-rowOffset, 0) + 1
	}

	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		switch node.Type() {
		case "heading":
			e.extractHeading(node, parseSrc, filePath, fileNode.ID, seen, result, startLine, endLine)
		case "greater_block":
			e.extractBlock(node, parseSrc, filePath, fileNode.ID, seen, result, startLine, endLine)
		case "keyword":
			// Top-level #+KEY: VALUE. Stash #+TITLE on the file node.
			e.maybeAttachKeyword(node, parseSrc, fileNode)
		case "regular_link":
			e.extractLink(node, parseSrc, filePath, fileNode.ID, seenLinks, result, startLine)
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)

	return result, nil
}

// extractHeading parses an Org headline from its raw text. The grammar
// exposes only the `stars` node as a named child — TODO/priority/title come
// from anonymous string aliases and aren't accessible as nodes — so we apply
// a regex to the line itself.
func (e *OrgModeExtractor) extractHeading(
	node *sitter.Node, src []byte, filePath, fileID string,
	seen map[string]bool, result *parser.ExtractionResult,
	startLine, endLine func(*sitter.Node) int,
) {
	// Heading content includes trailing whitespace / newline. Trim and take
	// the first line — the headline is always single-line.
	raw := node.Content(src)
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimRight(raw, " \t")

	m := headingPattern.FindStringSubmatch(raw)
	if m == nil {
		return
	}
	level := len(m[1])
	todo := m[2]
	priority := m[3]
	isComment := m[4] == "COMMENT"
	title := strings.TrimSpace(m[5])

	if level == 0 {
		return
	}
	if title == "" && !isComment {
		// A bare `*` line with nothing after it isn't a useful node.
		return
	}
	displayName := title
	if displayName == "" {
		displayName = "(comment)"
	}

	id := fmt.Sprintf("%s::h%d:%s", filePath, level, displayName)
	if seen[id] {
		return
	}
	seen[id] = true

	meta := map[string]any{"heading_level": level}
	if todo != "" {
		meta["todo_state"] = todo
	}
	if priority != "" {
		meta["priority"] = priority
	}
	if isComment {
		meta["comment"] = true
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: displayName,
		FilePath: filePath, StartLine: startLine(node), EndLine: endLine(node),
		Language: "orgmode", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine(node),
	})
}

// extractBlock handles #+BEGIN_X ... #+END_X greater blocks. SRC blocks
// record the source language from the begin-line parameters.
func (e *OrgModeExtractor) extractBlock(
	node *sitter.Node, src []byte, filePath, fileID string,
	seen map[string]bool, result *parser.ExtractionResult,
	startLine, endLine func(*sitter.Node) int,
) {
	var blockType, params string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "block_begin_name":
			blockType = strings.ToLower(strings.TrimSpace(child.Content(src)))
		case "value":
			if params == "" {
				params = strings.TrimSpace(child.Content(src))
			}
		}
	}
	if blockType == "" {
		return
	}

	displayName := blockType + " block"
	meta := map[string]any{"block_type": blockType}
	if blockType == "src" && params != "" {
		// First whitespace-separated token is the source language.
		lang := params
		if i := strings.IndexAny(lang, " \t"); i > 0 {
			lang = lang[:i]
		}
		if lang != "" {
			meta["code_language"] = lang
			displayName = lang + " code block"
		}
	}

	sLine := startLine(node)
	id := fmt.Sprintf("%s::block:%s:%d", filePath, blockType, sLine)
	if seen[id] {
		return
	}
	seen[id] = true

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: displayName,
		FilePath: filePath, StartLine: sLine, EndLine: endLine(node),
		Language: "orgmode", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: sLine,
	})
}

// maybeAttachKeyword stores #+TITLE: ... and #+AUTHOR: ... on the file node
// so consumers can show the document title without re-parsing.
func (e *OrgModeExtractor) maybeAttachKeyword(node *sitter.Node, src []byte, fileNode *graph.Node) {
	var rawKey, val string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "keyword_key":
			rawKey = strings.TrimSpace(child.Content(src))
		case "value":
			val = strings.TrimSpace(child.Content(src))
		}
	}
	if rawKey == "" || val == "" {
		return
	}
	m := keywordKeyPattern.FindStringSubmatch(rawKey)
	if m == nil {
		return
	}
	key := strings.ToUpper(m[1])
	switch key {
	case "TITLE", "AUTHOR":
		if fileNode.Meta == nil {
			fileNode.Meta = map[string]any{}
		}
		fileNode.Meta[strings.ToLower(key)] = val
	}
}

// extractLink emits an EdgeImports for intra-repo Org links.
// Skips http/https/mailto/anchor links — only file-style targets become edges.
func (e *OrgModeExtractor) extractLink(
	node *sitter.Node, src []byte, filePath, fileID string,
	seen map[string]bool, result *parser.ExtractionResult,
	startLine func(*sitter.Node) int,
) {
	var target string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "pathreg" {
			target = strings.TrimSpace(child.Content(src))
			break
		}
	}
	if target == "" {
		return
	}

	// Strip the optional "file:" prefix that Org uses for explicit file links.
	target = strings.TrimPrefix(target, "file:")

	// Skip non-file schemes and pure anchors.
	for _, prefix := range []string{"http://", "https://", "mailto:", "ftp://", "news:", "elisp:", "shell:", "id:", "#", "*"} {
		if strings.HasPrefix(target, prefix) {
			return
		}
	}

	// Strip an inline anchor: "foo.org::*Heading" or "foo.org#anchor".
	for _, sep := range []string{"::", "#"} {
		if i := strings.Index(target, sep); i > 0 {
			target = target[:i]
			break
		}
	}

	if target == "" || seen[target] {
		return
	}
	seen[target] = true

	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + target,
		Kind: graph.EdgeImports, FilePath: filePath, Line: startLine(node),
	})
}

var _ parser.Extractor = (*OrgModeExtractor)(nil)
