package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Prose extraction turns a Markdown document into first-class,
// searchable KindDoc section nodes. Each heading opens a section that
// runs until the next heading of equal-or-shallower level; the
// section's paragraph text (markdown syntax stripped) becomes the
// node body, and the heading path forms a breadcrumb name. Section
// node IDs are derived from the heading path, never line numbers, so
// an incremental reindex of an edited file keeps stable identity.

// proseSection accumulates one heading-delimited region while the
// extractor walks the document in source order.
type proseSection struct {
	level     int      // heading depth (1..6); 0 is the synthetic preamble
	crumbs    []string // heading-path breadcrumb, root-first
	startLine int      // 1-based line of the opening heading
	endLine   int      // 1-based line of the last content line seen
	body      strings.Builder
}

// mdInlineLink / mdImage / mdEmphasis strip the common inline
// markdown constructs so the section body indexed for search is
// plain prose, not syntax.
var (
	mdInlineLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdImage      = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdInlineCode = regexp.MustCompile("`+([^`]*)`+")
)

// extractProseSections walks the Markdown AST and appends one KindDoc
// node per heading-delimited section, plus an EdgeDefines edge from
// the file node. It is called by MarkdownExtractor.Extract after the
// structural (heading / code-block / link) pass.
func (e *MarkdownExtractor) extractProseSections(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	// A stack of open sections, deepest last. A new heading closes
	// every open section of equal-or-greater level before opening
	// its own.
	var stack []*proseSection
	var finished []*proseSection

	closeTo := func(level int) {
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			finished = append(finished, stack[len(stack)-1])
			stack = stack[:len(stack)-1]
		}
	}
	appendText := func(line int, text string) {
		text = strings.TrimSpace(stripMarkdownInline(text))
		if text == "" {
			return
		}
		// Text before the first heading belongs to a synthetic
		// preamble section so a doc with leading prose is not lost.
		if len(stack) == 0 {
			stack = append(stack, &proseSection{level: 0, crumbs: nil, startLine: line})
		}
		top := stack[len(stack)-1]
		if top.body.Len() > 0 {
			top.body.WriteByte(' ')
		}
		top.body.WriteString(text)
		if line > top.endLine {
			top.endLine = line
		}
	}

	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		switch node.Type() {
		case "atx_heading":
			level, text := headingLevelText(node, src)
			if level == 0 || text == "" {
				return // malformed heading; children carry nothing useful
			}
			line := int(node.StartPoint().Row) + 1
			closeTo(level)
			var crumbs []string
			if len(stack) > 0 {
				crumbs = append(crumbs, stack[len(stack)-1].crumbs...)
			}
			crumbs = append(crumbs, text)
			stack = append(stack, &proseSection{
				level: level, crumbs: crumbs, startLine: line, endLine: line,
			})
			return
		case "paragraph":
			appendText(int(node.StartPoint().Row)+1, node.Content(src))
			return
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)
	closeTo(1) // flush every still-open section
	finished = append(finished, stack...)

	base := proseFileBase(filePath)
	seen := make(map[string]bool, len(finished))
	for _, sec := range finished {
		// A section with no captured prose carries no search signal.
		if strings.TrimSpace(sec.body.String()) == "" {
			continue
		}
		id := proseSectionID(filePath, base, sec.crumbs)
		// A duplicated heading path would collide; disambiguate by
		// appending an occurrence index so identity stays stable per
		// (heading-path, occurrence).
		if seen[id] {
			id = proseSectionID(filePath, base, append(sec.crumbs, "")) + idSlug(itoaSmall(len(seen)))
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        id,
			Kind:      graph.KindDoc,
			Name:      proseBreadcrumb(base, sec.crumbs),
			FilePath:  filePath,
			StartLine: sec.startLine,
			EndLine:   sec.endLine,
			Language:  "markdown",
			Meta: map[string]any{
				"section_text":  sec.body.String(),
				"heading_path":  sec.crumbs,
				"heading_level": sec.level,
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: sec.startLine,
		})
	}
}

// headingLevelText returns the depth (1..6) and trimmed text of an
// atx_heading node, or (0, "") when either cannot be determined.
func headingLevelText(node *sitter.Node, src []byte) (int, string) {
	level := 0
	var text string
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
			text = strings.TrimSpace(child.Content(src))
		}
	}
	return level, text
}

// stripMarkdownInline removes the common inline markdown constructs
// (images, links, inline code, emphasis markers) so the indexed
// section body is plain prose. Link/image text is kept; the URL is
// dropped.
func stripMarkdownInline(s string) string {
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdInlineLink.ReplaceAllString(s, "$1")
	s = mdInlineCode.ReplaceAllString(s, "$1")
	// Drop emphasis / strong markers and stray heading hashes.
	s = strings.NewReplacer("**", "", "__", "", "*", "", "_", "", "`", "").Replace(s)
	// Collapse internal whitespace runs.
	return strings.Join(strings.Fields(s), " ")
}

// proseFileBase returns the file's base name -- the breadcrumb root
// and the seed of the stable section ID.
func proseFileBase(filePath string) string {
	p := strings.ReplaceAll(filePath, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// proseBreadcrumb renders the human-facing section name -- the file
// base followed by the heading path, joined with " > ".
func proseBreadcrumb(base string, crumbs []string) string {
	parts := make([]string, 0, len(crumbs)+1)
	parts = append(parts, base)
	parts = append(parts, crumbs...)
	return strings.Join(parts, " > ")
}

// proseSectionID builds the stable section node ID. The ID is keyed
// on the slugified heading path -- NOT line numbers -- so editing the
// prose of a section, or inserting text above it, leaves its
// identity unchanged across an incremental reindex.
func proseSectionID(filePath, base string, crumbs []string) string {
	slug := idSlug(base)
	for _, c := range crumbs {
		if s := idSlug(c); s != "" {
			slug += "-" + s
		}
	}
	return filePath + "::doc:" + slug
}

// idSlug lowercases an identifier fragment and collapses every run of
// non-alphanumeric characters to a single hyphen -- a filesystem- and
// ID-safe slug.
func idSlug(s string) string {
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// itoaSmall renders a small non-negative int without importing
// strconv into this file.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
