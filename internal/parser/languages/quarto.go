package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Quarto (.qmd) documents weave YAML frontmatter, prose, and executable
// code chunks into one literate file. This extractor is deliberately
// dependency-free and line-based — robust to CRLF and to ``` / ~~~
// fences of length >= 3 — and emits three families of nodes:
//
//   - KindConfigKey, one per top-level frontmatter key, so a doc's
//     declared metadata (title, format, params, …) is searchable.
//   - KindDoc, one per heading-delimited prose section, mirroring the
//     Markdown prose convention (breadcrumb name, stripped section body,
//     heading-derived stable ID).
//   - KindFunction, one per executable code chunk (```{r}, ```{python},
//     …), named by its chunk label when present. Plain fenced blocks
//     without a {lang} brace info string are ordinary documentation code
//     and are skipped.
//
// Each emitted node is linked from the file node by an EdgeDefines edge.

// QuartoExtractor extracts Quarto literate-programming documents.
type QuartoExtractor struct{}

// NewQuartoExtractor constructs a QuartoExtractor.
func NewQuartoExtractor() *QuartoExtractor { return &QuartoExtractor{} }

func (e *QuartoExtractor) Language() string     { return "quarto" }
func (e *QuartoExtractor) Extensions() []string { return []string{".qmd"} }

// quartoSection accumulates one heading-delimited prose region while the
// scanner walks the document in source order.
type quartoSection struct {
	level     int      // heading depth (1..6)
	crumbs    []string // heading-path breadcrumb, root-first
	startLine int      // 1-based line of the opening heading
	endLine   int      // 1-based line of the last content line seen
	body      strings.Builder
}

func (e *QuartoExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	// Normalise line endings so CRLF files scan identically to LF ones.
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	result := &parser.ExtractionResult{}
	fileID := filePath
	fileNode := &graph.Node{
		ID: fileID, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(lines),
		Language: "quarto",
	}
	result.Nodes = append(result.Nodes, fileNode)

	emitDefine := func(node *graph.Node, line int) {
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: node.ID, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
	}

	// --- 1. YAML frontmatter: top-level keys --------------------------
	// Frontmatter is the leading `---` fence and its closing `---`/`...`.
	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			tr := strings.TrimSpace(lines[i])
			if tr == "---" || tr == "..." {
				bodyStart = i + 1
				break
			}
			// Only top-level keys: a `key:` line with no leading
			// indentation (so nested mapping/sequence members are skipped).
			if key := topLevelYAMLKey(lines[i]); key != "" {
				id := filePath + "::frontmatter::" + key
				emitDefine(&graph.Node{
					ID: id, Kind: graph.KindConfigKey, Name: key,
					FilePath: filePath, StartLine: i + 1, EndLine: i + 1,
					Language: "quarto",
					Meta:     map[string]any{"source": "quarto_frontmatter"},
				}, i+1)
			}
		}
	}

	// --- 2 & 3. Prose sections and executable code chunks -------------
	// A single fence-aware pass: track whether we're inside a fenced
	// block so `#` lines inside code aren't mistaken for headings, and
	// so chunk bodies aren't scanned for prose.
	var (
		sections   []*quartoSection
		stack      []*quartoSection
		seenSecID  = make(map[string]bool)
		chunkCount = make(map[string]int) // per-language chunk counter
	)

	closeTo := func(level int) {
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			sections = append(sections, stack[len(stack)-1])
			stack = stack[:len(stack)-1]
		}
	}
	appendProse := func(line int, raw string) {
		txt := strings.TrimSpace(stripMarkdownInline(raw))
		if txt == "" || len(stack) == 0 {
			return
		}
		top := stack[len(stack)-1]
		if top.body.Len() > 0 {
			top.body.WriteByte(' ')
		}
		top.body.WriteString(txt)
		if line > top.endLine {
			top.endLine = line
		}
	}

	i := bodyStart
	for i < len(lines) {
		line := lines[i]
		if fence, ok := fenceOpener(line); ok {
			// A fenced block. Collect its body up to the matching closer.
			info := fenceInfo(line, fence)
			chunkStart := i + 1 // 1-based line of the opening fence
			j := i + 1
			var body []string
			for j < len(lines) {
				if fenceCloser(lines[j], fence) {
					break
				}
				body = append(body, lines[j])
				j++
			}
			chunkEnd := j + 1 // 1-based line of the closing fence (or EOF)
			if j >= len(lines) {
				chunkEnd = len(lines)
			}

			if lang, ok := chunkLanguage(info); ok {
				// Executable code chunk → KindFunction.
				label := chunkLabel(info, body)
				name := label
				if name == "" {
					chunkCount[lang]++
					name = fmt.Sprintf("chunk-%s-%d", lang, chunkCount[lang])
				}
				id := filePath + "::chunk:" + name
				emitDefine(&graph.Node{
					ID: id, Kind: graph.KindFunction, Name: name,
					FilePath: filePath, StartLine: chunkStart, EndLine: chunkEnd,
					Language: "quarto",
					Meta:     map[string]any{"chunk_language": lang},
				}, chunkStart)
			}
			// Plain fenced block (no {lang} brace) → ordinary doc code, skip.

			// Advance past the closing fence.
			i = j + 1
			continue
		}

		// Outside any fence: a `#`-prefixed line is a heading.
		if level, headingText, ok := atxHeading(line); ok {
			closeTo(level)
			var crumbs []string
			if len(stack) > 0 {
				crumbs = append(crumbs, stack[len(stack)-1].crumbs...)
			}
			crumbs = append(crumbs, headingText)
			stack = append(stack, &quartoSection{
				level: level, crumbs: crumbs, startLine: i + 1, endLine: i + 1,
			})
			i++
			continue
		}

		appendProse(i+1, line)
		i++
	}
	closeTo(1)
	sections = append(sections, stack...)

	base := proseFileBase(filePath)
	for _, sec := range sections {
		if strings.TrimSpace(sec.body.String()) == "" {
			continue // no prose body → no search signal
		}
		id := proseSectionID(filePath, base, sec.crumbs)
		if seenSecID[id] {
			id = proseSectionID(filePath, base, append(sec.crumbs, "")) + idSlug(itoaSmall(len(seenSecID)))
		}
		seenSecID[id] = true
		emitDefine(&graph.Node{
			ID: id, Kind: graph.KindDoc, Name: proseBreadcrumb(base, sec.crumbs),
			FilePath: filePath, StartLine: sec.startLine, EndLine: sec.endLine,
			Language: "quarto",
			Meta: map[string]any{
				"section_text":  sec.body.String(),
				"heading_path":  sec.crumbs,
				"heading_level": sec.level,
			},
		}, sec.startLine)
	}

	return result, nil
}

// topLevelYAMLKey returns the key name of a top-level `key:` frontmatter
// line, or "" when the line is indented (a nested member), blank, a
// comment, a sequence item, or carries no `:` separator.
func topLevelYAMLKey(line string) string {
	// Top-level keys have no leading indentation.
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return ""
	}
	tr := strings.TrimRight(line, " \t")
	if tr == "" || strings.HasPrefix(tr, "#") || strings.HasPrefix(tr, "-") {
		return ""
	}
	idx := strings.IndexByte(tr, ':')
	if idx <= 0 {
		return ""
	}
	key := strings.TrimSpace(tr[:idx])
	if key == "" || strings.ContainsAny(key, " \t") {
		// A bare scalar / multi-word line is not a mapping key.
		return ""
	}
	return key
}

// atxHeading reports whether line is an ATX heading (1..6 leading `#`
// followed by whitespace) and returns its level and trimmed text.
func atxHeading(line string) (level int, text string, ok bool) {
	s := strings.TrimLeft(line, " ")
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	if n >= len(s) || (s[n] != ' ' && s[n] != '\t') {
		return 0, "", false // `#foo` is not a heading
	}
	text = strings.TrimSpace(strings.TrimRight(s[n:], "#"))
	if text == "" {
		return 0, "", false
	}
	return n, text, true
}

// fenceOpener reports whether line opens a fenced block and returns the
// fence run (the leading ``` / ~~~ of length >= 3) used to match its
// closer.
func fenceOpener(line string) (fence string, ok bool) {
	return fenceRun(line)
}

// fenceCloser reports whether line closes a block opened with the given
// fence: a line consisting only of a fence run of the same character,
// at least as long as the opener, with no trailing info string.
func fenceCloser(line, opener string) bool {
	run, ok := fenceRun(line)
	if !ok || len(run) == 0 || run[0] != opener[0] || len(run) < len(opener) {
		return false
	}
	// A closer carries no info string.
	rest := strings.TrimLeft(line, " ")
	return strings.TrimSpace(rest[len(run):]) == ""
}

// fenceRun returns the leading fence run (3+ backticks or tildes,
// allowing up to 3 spaces of indentation) of a line.
func fenceRun(line string) (string, bool) {
	s := line
	// Up to three leading spaces are permitted before a fence.
	indent := 0
	for indent < len(s) && indent < 4 && s[indent] == ' ' {
		indent++
	}
	if indent >= 4 {
		return "", false
	}
	s = s[indent:]
	if len(s) < 3 {
		return "", false
	}
	ch := s[0]
	if ch != '`' && ch != '~' {
		return "", false
	}
	n := 0
	for n < len(s) && s[n] == ch {
		n++
	}
	if n < 3 {
		return "", false
	}
	return s[:n], true
}

// fenceInfo returns the info string of a fence opener — everything after
// the fence run, trimmed.
func fenceInfo(line, fence string) string {
	s := strings.TrimLeft(line, " ")
	if len(s) < len(fence) {
		return ""
	}
	return strings.TrimSpace(s[len(fence):])
}

// chunkLanguage extracts the executable language from a Quarto brace
// info string like `{r}`, `{python}`, or `{r, label="fig1"}`. Plain
// info strings without a leading `{lang}` brace (e.g. `r` or `python`)
// are NOT executable chunks and yield ok=false.
func chunkLanguage(info string) (lang string, ok bool) {
	if !strings.HasPrefix(info, "{") {
		return "", false
	}
	end := strings.IndexByte(info, '}')
	if end < 0 {
		return "", false
	}
	inner := info[1:end]
	// The language is the first token, before any comma or whitespace.
	if c := strings.IndexByte(inner, ','); c >= 0 {
		inner = inner[:c]
	}
	lang = strings.TrimSpace(inner)
	// A leading `=` (e.g. `{=html}`) marks a raw passthrough block.
	lang = strings.TrimPrefix(lang, "=")
	if lang == "" {
		return "", false
	}
	return lang, true
}

// chunkLabel returns the chunk label from either the brace options
// (`{r, label="fig1"}`) or a leading `#| label: fig1` option line in the
// chunk body. The empty string means the chunk is unlabelled.
func chunkLabel(info string, body []string) string {
	// Brace form: {r, label="fig1"} or {r, label='fig1'} or {r label=fig1}.
	if lbl := braceLabel(info); lbl != "" {
		return lbl
	}
	// `#| label: fig1` option lines at the top of the chunk body.
	for _, ln := range body {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "#|") {
			// Once a non-option line appears, stop scanning — option
			// lines only lead the chunk body.
			if t == "" {
				continue
			}
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "#|"))
		if v, ok := optionValue(rest, "label"); ok {
			return v
		}
	}
	return ""
}

// braceLabel extracts a `label=...` option from a brace info string.
func braceLabel(info string) string {
	end := strings.IndexByte(info, '}')
	if !strings.HasPrefix(info, "{") || end < 0 {
		return ""
	}
	inner := info[1:end]
	idx := strings.Index(inner, "label")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(inner[idx+len("label"):])
	if !strings.HasPrefix(rest, "=") {
		return ""
	}
	return unquote(strings.TrimSpace(strings.TrimPrefix(rest, "=")))
}

// optionValue parses a `key: value` YAML option line (the `#| key: val`
// form) and returns the unquoted value when key matches.
func optionValue(opt, key string) (string, bool) {
	idx := strings.IndexByte(opt, ':')
	if idx < 0 {
		return "", false
	}
	if strings.TrimSpace(opt[:idx]) != key {
		return "", false
	}
	return unquote(strings.TrimSpace(opt[idx+1:])), true
}

// unquote strips a single trailing/leading pair of matching quotes and
// any trailing comma, returning the bare token value.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ",")
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	// A bare token may carry a trailing option separator.
	if c := strings.IndexAny(s, " \t"); c >= 0 {
		s = s[:c]
	}
	return s
}

var _ parser.Extractor = (*QuartoExtractor)(nil)
