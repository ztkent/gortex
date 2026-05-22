package embedding

import (
	"context"
	"strings"
	"time"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	clang "github.com/zzet/gortex/internal/parser/tsitter/c"
	cpplang "github.com/zzet/gortex/internal/parser/tsitter/cpp"
	golang "github.com/zzet/gortex/internal/parser/tsitter/golang"
	javalang "github.com/zzet/gortex/internal/parser/tsitter/java"
	jslang "github.com/zzet/gortex/internal/parser/tsitter/javascript"
	pylang "github.com/zzet/gortex/internal/parser/tsitter/python"
	rustlang "github.com/zzet/gortex/internal/parser/tsitter/rust"
	tsxlang "github.com/zzet/gortex/internal/parser/tsitter/tsx"
	tslang "github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// Chunk is one AST window cut out of a symbol's source span. A symbol
// short enough to embed whole produces exactly one Chunk; a large
// function or type produces several, each covering a contiguous run of
// top-level statements / field declarations.
type Chunk struct {
	// Text is the chunk's source text — the substring of the symbol's
	// span the window covers. It is what gets embedded.
	Text string
	// ParentID is the graph node ID of the symbol the chunk belongs to.
	// Every chunk of a symbol carries the same ParentID; the de-chunk
	// step at query time maps a chunk hit back through it.
	ParentID string
	// WindowIndex is the 0-based position of this window within the
	// symbol. A single-chunk symbol has WindowIndex 0.
	WindowIndex int
}

// ChunkOptions tunes the AST-window splitter.
type ChunkOptions struct {
	// ThresholdLines is the line count above which a symbol is split
	// into windows. At or below it the symbol is embedded whole.
	ThresholdLines int
	// WindowLines caps the line span of each emitted window. A single
	// top-level statement larger than this still forms its own window
	// (the splitter never cuts inside a statement).
	WindowLines int
}

const (
	// DefaultChunkThresholdLines is the built-in split threshold used
	// when ChunkOptions.ThresholdLines is zero.
	DefaultChunkThresholdLines = 60
	// DefaultChunkWindowLines is the built-in window cap used when
	// ChunkOptions.WindowLines is zero.
	DefaultChunkWindowLines = 40
	// chunkParseTimeout bounds the tree-sitter parse of one symbol's
	// span. Generous — a symbol body is small, but a pathological
	// grammar should still not stall the index pass.
	chunkParseTimeout = 3 * time.Second
)

// normalized fills in zero-valued options with the package defaults.
func (o ChunkOptions) normalized() ChunkOptions {
	if o.ThresholdLines <= 0 {
		o.ThresholdLines = DefaultChunkThresholdLines
	}
	if o.WindowLines <= 0 {
		o.WindowLines = DefaultChunkWindowLines
	}
	// A window can never be smaller than a single line, and a window
	// larger than the threshold would never split anything.
	if o.WindowLines < 1 {
		o.WindowLines = 1
	}
	return o
}

// ChunkSymbol splits a symbol's source span into AST windows. src is
// the exact source text of the symbol (signature through closing
// brace). language is the tree-sitter language name. parentID is the
// graph node ID stamped on every returned chunk.
//
// The result is always non-empty. A symbol at or below the line
// threshold, in a language with no splitter, or whose source fails to
// parse, yields a single chunk holding the whole span. A large
// function is split on the top-level statements of its body; a large
// type on its field declarations. Windows never cut inside a
// statement, so one oversized statement forms its own window.
func ChunkSymbol(src []byte, language, parentID string, opts ChunkOptions) []Chunk {
	opts = opts.normalized()
	whole := []Chunk{{Text: string(src), ParentID: parentID, WindowIndex: 0}}

	if len(src) == 0 {
		return whole
	}
	if countLines(src) <= opts.ThresholdLines {
		return whole
	}
	spec := chunkSpecFor(language)
	if spec == nil {
		return whole
	}
	grammar := spec.grammar()
	if grammar == nil {
		return whole
	}

	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(grammar)
	ctx, cancel := context.WithTimeout(context.Background(), chunkParseTimeout)
	defer cancel()
	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil || tree == nil {
		return whole
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		return whole
	}

	// Locate the splittable container — a function/method body block or
	// a type's field-declaration list — and collect the byte ranges of
	// its top-level children.
	container := spec.findContainer(root)
	if container == nil {
		return whole
	}
	container = unwrapStatementList(container)
	pieces := topLevelChildRanges(container)
	if len(pieces) < 2 {
		// Nothing to split into more than one window.
		return whole
	}

	windows := packWindows(src, pieces, opts.WindowLines)
	if len(windows) < 2 {
		return whole
	}
	chunks := make([]Chunk, len(windows))
	for i, w := range windows {
		chunks[i] = Chunk{Text: w, ParentID: parentID, WindowIndex: i}
	}
	return chunks
}

// byteRange is a half-open [start,end) byte span within the symbol src.
type byteRange struct {
	start, end uint32
}

// unwrapStatementList descends through wrapper nodes that hold the
// real split points one level deeper. Tree-sitter-go wraps a function
// body's statements in a `statement_list` inside the `block`; the
// `block` itself then has only that one named child. Unwrapping it
// (and any chain of such single-child list wrappers) exposes the
// statements as the container's direct children. A wrapper with more
// than one named child, or a non-list child, is left as the container.
func unwrapStatementList(container *sitter.Node) *sitter.Node {
	for i := 0; i < 4; i++ { // bounded — real grammars nest at most once
		if container == nil || container.NamedChildCount() != 1 {
			return container
		}
		only := container.NamedChild(0)
		if only == nil {
			return container
		}
		t := only.Type()
		if t == "statement_list" || strings.HasSuffix(t, "_declaration_list") {
			container = only
			continue
		}
		return container
	}
	return container
}

// topLevelChildRanges returns the byte ranges of the named children of
// a container node (statements of a block, field declarations of a
// field list). Anonymous tokens (braces, commas) are skipped.
func topLevelChildRanges(container *sitter.Node) []byteRange {
	n := int(container.NamedChildCount())
	ranges := make([]byteRange, 0, n)
	for i := 0; i < n; i++ {
		c := container.NamedChild(i)
		if c == nil {
			continue
		}
		ranges = append(ranges, byteRange{start: c.StartByte(), end: c.EndByte()})
	}
	return ranges
}

// packWindows groups consecutive child ranges into windows of at most
// windowLines lines each, and returns the source text of every window.
// The first window also captures the symbol's signature (everything
// before the first child); the last captures the trailing bytes
// (closing brace) so the rejoined windows still cover the whole span.
// A single child larger than windowLines forms its own window.
func packWindows(src []byte, pieces []byteRange, windowLines int) []string {
	if len(pieces) == 0 {
		return []string{string(src)}
	}

	var windows []string
	groupStart := uint32(0) // first window starts at the symbol's own start (keeps the signature)
	cur := 0                // index of the first piece in the current group
	curLines := 0

	flush := func(endByte uint32, upto int) {
		if upto <= cur {
			return
		}
		windows = append(windows, string(src[groupStart:endByte]))
		groupStart = endByte
		cur = upto
		curLines = 0
	}

	for i, p := range pieces {
		pieceLines := countLines(src[p.start:p.end])
		// If adding this piece would overflow the window and the
		// window already holds something, close the window before it.
		if curLines > 0 && curLines+pieceLines > windowLines {
			flush(pieces[i-1].end, i)
		}
		curLines += pieceLines
	}
	// Final window runs to the end of the symbol span so the trailing
	// closing brace is never dropped.
	if cur < len(pieces) {
		windows = append(windows, string(src[groupStart:]))
	}
	if len(windows) == 0 {
		return []string{string(src)}
	}
	return windows
}

// countLines returns the number of source lines a byte slice spans (a
// non-empty slice with no newline is one line).
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	return strings.Count(string(b), "\n") + 1
}

// chunkSpec describes how to find a splittable container in one
// tree-sitter grammar.
type chunkSpec struct {
	grammar func() *sitter.Language
	// findContainer locates the node whose named children are the
	// split points: a function/method body block, or a type's field
	// list. Returns nil when the parsed span has no such container.
	findContainer func(root *sitter.Node) *sitter.Node
}

// chunkSpecFor returns the splitter spec for a language, or nil when
// the language has no splitter (the symbol is then embedded whole).
func chunkSpecFor(language string) *chunkSpec {
	switch strings.ToLower(language) {
	case "go", "golang":
		return &chunkSpec{
			grammar: golang.GetLanguage,
			findContainer: braceContainer(
				containerSpec{decl: "function_declaration", body: "block", bodyField: "body"},
				containerSpec{decl: "method_declaration", body: "block", bodyField: "body"},
				// A Go struct: split on its field declarations. The
				// struct_type node is reached through type_declaration →
				// type_spec; walkNodes descends to it.
				containerSpec{decl: "struct_type", body: "field_declaration_list"},
			),
		}
	case "typescript", "ts":
		return &chunkSpec{
			grammar:       tslang.GetLanguage,
			findContainer: tsLikeContainer,
		}
	case "tsx":
		return &chunkSpec{
			grammar:       tsxlang.GetLanguage,
			findContainer: tsLikeContainer,
		}
	case "javascript", "js", "jsx":
		return &chunkSpec{
			grammar:       jslang.GetLanguage,
			findContainer: tsLikeContainer,
		}
	case "java":
		return &chunkSpec{
			grammar: javalang.GetLanguage,
			findContainer: braceContainer(
				containerSpec{decl: "method_declaration", body: "block", bodyField: "body"},
				containerSpec{decl: "constructor_declaration", body: "constructor_body", bodyField: "body"},
				containerSpec{decl: "class_declaration", body: "class_body", bodyField: "body"},
			),
		}
	case "c":
		return &chunkSpec{
			grammar: clang.GetLanguage,
			findContainer: braceContainer(
				containerSpec{decl: "function_definition", body: "compound_statement", bodyField: "body"},
			),
		}
	case "cpp", "c++":
		return &chunkSpec{
			grammar: cpplang.GetLanguage,
			findContainer: braceContainer(
				containerSpec{decl: "function_definition", body: "compound_statement", bodyField: "body"},
			),
		}
	case "rust":
		return &chunkSpec{
			grammar: rustlang.GetLanguage,
			findContainer: braceContainer(
				containerSpec{decl: "function_item", body: "block", bodyField: "body"},
			),
		}
	case "python", "py":
		return &chunkSpec{
			grammar:       pylang.GetLanguage,
			findContainer: pythonContainer,
		}
	default:
		return nil
	}
}

// containerSpec names a declaration node kind and the body node kind
// whose named children are the split points.
type containerSpec struct {
	decl string
	body string
	// bodyField, when set, is the field name the body hangs off; when
	// empty the body is found by scanning named children for `body`.
	bodyField string
}

// braceContainer builds a findContainer that walks the AST for the
// first declaration matching any of the specs and returns its body
// node. Used by every brace-bodied grammar.
func braceContainer(specs ...containerSpec) func(*sitter.Node) *sitter.Node {
	byDecl := make(map[string]containerSpec, len(specs))
	for _, s := range specs {
		byDecl[s.decl] = s
	}
	return func(root *sitter.Node) *sitter.Node {
		var found *sitter.Node
		walkNodes(root, func(n *sitter.Node) bool {
			if found != nil {
				return false
			}
			spec, ok := byDecl[n.Type()]
			if !ok {
				return true
			}
			body := bodyOf(n, spec)
			if body != nil {
				found = body
				return false
			}
			return true
		})
		return found
	}
}

// bodyOf locates the body node of a declaration per its containerSpec.
func bodyOf(decl *sitter.Node, spec containerSpec) *sitter.Node {
	if spec.bodyField != "" {
		body := decl.ChildByFieldName(spec.bodyField)
		if body != nil && body.Type() == spec.body {
			return body
		}
	}
	n := int(decl.NamedChildCount())
	for i := 0; i < n; i++ {
		c := decl.NamedChild(i)
		if c != nil && c.Type() == spec.body {
			return c
		}
	}
	return nil
}

// tsLikeContainer finds the splittable container in TypeScript /
// JavaScript / JSX / TSX: a function/method body `statement_block`, an
// arrow function's block body, or a class body / interface body.
func tsLikeContainer(root *sitter.Node) *sitter.Node {
	decls := map[string]struct{}{
		"function_declaration":           {},
		"generator_function_declaration": {},
		"method_definition":              {},
		"arrow_function":                 {},
		"function_expression":            {},
	}
	bodyKinds := map[string]struct{}{
		"statement_block": {},
		"class_body":      {},
	}
	var found *sitter.Node
	walkNodes(root, func(n *sitter.Node) bool {
		if found != nil {
			return false
		}
		t := n.Type()
		if t == "class_declaration" || t == "class" || t == "interface_declaration" {
			if body := firstNamedChildOfKind(n, bodyKinds); body != nil {
				found = body
				return false
			}
		}
		if _, ok := decls[t]; ok {
			if body := n.ChildByFieldName("body"); body != nil {
				if _, ok := bodyKinds[body.Type()]; ok {
					found = body
					return false
				}
			}
		}
		return true
	})
	return found
}

// pythonContainer finds the `block` body of the first function or
// class definition in a parsed Python span.
func pythonContainer(root *sitter.Node) *sitter.Node {
	decls := map[string]struct{}{
		"function_definition": {},
		"class_definition":    {},
	}
	var found *sitter.Node
	walkNodes(root, func(n *sitter.Node) bool {
		if found != nil {
			return false
		}
		if _, ok := decls[n.Type()]; ok {
			if body := n.ChildByFieldName("body"); body != nil && body.Type() == "block" {
				found = body
				return false
			}
		}
		return true
	})
	return found
}

// firstNamedChildOfKind returns the first named child whose kind is in
// the allowlist, or nil.
func firstNamedChildOfKind(n *sitter.Node, kinds map[string]struct{}) *sitter.Node {
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		if _, ok := kinds[c.Type()]; ok {
			return c
		}
	}
	return nil
}

// walkNodes does a pre-order DFS over the tree-sitter tree, calling
// visit on each node. visit returns false to prune the subtree.
func walkNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	cnt := int(n.NamedChildCount())
	for i := 0; i < cnt; i++ {
		walkNodes(n.NamedChild(i), visit)
	}
}
