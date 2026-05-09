package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/rust"
)

// qRustAll is a single tree-sitter query alternating over every pattern
// the Rust extractor needs. One tree walk per file replaces the 14
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Impl/trait membership is
// resolved via a parent walk on the captured node rather than nested
// queries — same behaviour, one cursor pass.
const qRustAll = `
[
  (function_item
    name: (identifier) @func.name) @func.def

  (function_signature_item
    name: (identifier) @sig.name) @sig.def

  (struct_item
    name: (type_identifier) @struct.name) @struct.def

  (enum_item
    name: (type_identifier) @enum.name) @enum.def

  (trait_item
    name: (type_identifier) @trait.name) @trait.def

  (enum_variant
    name: (identifier) @variant.name) @variant.def

  (field_declaration
    name: (field_identifier) @field.name) @field.def

  (const_item
    name: (identifier) @const.name) @const.def

  (static_item
    name: (identifier) @static.name) @static.def

  (use_declaration
    argument: (_) @use.path) @use.def

  (let_declaration
    pattern: (identifier) @lvar.name
    type: (_)? @lvar.type
    value: (_)? @lvar.value) @lvar.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (scoped_identifier
      name: (identifier) @callp.name)) @callp.expr

  (call_expression
    function: (field_expression
      value: (_) @callm.receiver
      field: (field_identifier) @callm.method)) @callm.expr
]
`

// RustExtractor extracts Rust source files into graph nodes and edges.
type RustExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewRustExtractor() *RustExtractor {
	lang := rust.GetLanguage()
	return &RustExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qRustAll, lang),
	}
}

func (e *RustExtractor) Language() string     { return "rust" }
func (e *RustExtractor) Extensions() []string { return []string{".rs"} }

// --- Deferred match buffers ----------------------------------------

type rustDeferredCall struct {
	name       string
	receiver   string // selector receiver text (empty for plain/path calls)
	line       int
	isSelector bool
}

// rustDeferredLet buffers a let_declaration for the post-pass type-env
// build. Tier 0 (explicit type) is processed across all lets first;
// Tier 1 (`new()` / struct expression on the RHS) only fills in keys
// the explicit pass did not — same precedence the legacy two-query
// version produced.
type rustDeferredLet struct {
	name     string
	explicit string       // normalized type from explicit annotation, "" if none
	value    *sitter.Node // RHS expression node, or nil
}

func (e *RustExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "rust",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	traitMethods := make(map[string][]string) // trait name → declared method names

	var calls []rustDeferredCall
	var lets []rustDeferredLet

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["sig.def"] != nil:
			e.recordTraitMethod(m, src, traitMethods)

		case m.Captures["struct.def"] != nil:
			e.emitStruct(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["trait.def"] != nil:
			e.emitTrait(m, filePath, fileID, src, result, seen, annotationSeen, traitMethods)

		case m.Captures["variant.def"] != nil:
			e.emitVariant(m, filePath, src, result)

		case m.Captures["field.def"] != nil:
			e.emitField(m, filePath, src, result)

		case m.Captures["const.def"] != nil:
			e.emitNamed(m, "const", filePath, fileID, result, seen)

		case m.Captures["static.def"] != nil:
			e.emitNamed(m, "static", filePath, fileID, result, seen)

		case m.Captures["use.def"] != nil:
			e.emitUse(m, filePath, fileID, result)

		case m.Captures["lvar.def"] != nil:
			d := rustDeferredLet{
				name: m.Captures["lvar.name"].Text,
			}
			if t, ok := m.Captures["lvar.type"]; ok {
				d.explicit = normalizeRustTypeName(t.Text)
			}
			if v, ok := m.Captures["lvar.value"]; ok && v.Node != nil {
				d.value = v.Node
			}
			lets = append(lets, d)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, rustDeferredCall{
				name:       m.Captures["callm.method"].Text,
				receiver:   m.Captures["callm.receiver"].Text,
				line:       expr.StartLine + 1,
				isSelector: true,
			})

		case m.Captures["callp.expr"] != nil:
			expr := m.Captures["callp.expr"]
			calls = append(calls, rustDeferredCall{
				name: m.Captures["callp.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, rustDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})
		}
	})

	// Stamp trait method names onto trait nodes' Meta["methods"]. Some
	// trait nodes were emitted before their function_signature_item
	// children fired through the alternation, so we backfill at the end.
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := traitMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Build type environment in the legacy precedence:
	//   Tier 0 — explicit `let x: Type = ...` annotations (overwrite)
	//   Tier 1 — RHS inference (`StructExpr {...}`, `Type::new(...)`),
	//            only when Tier 0 didn't supply a type.
	tenv := make(typeEnv)
	for _, l := range lets {
		if l.explicit != "" {
			tenv[l.name] = l.explicit
		}
	}
	for _, l := range lets {
		if _, exists := tenv[l.name]; exists {
			continue
		}
		if l.value == nil {
			continue
		}
		if inferred := inferTypeFromRustExpr(l.value, src); inferred != "" {
			tenv[l.name] = inferred
		}
	}

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if c.isSelector {
			edge := &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			}
			if recvType, ok := tenv[c.receiver]; ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				if chainType := resolveChainType(c.receiver, tenv, result); chainType != "" {
					edge.Meta = map[string]any{"receiver_type": chainType}
				}
			}
			result.Edges = append(result.Edges, edge)
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	// Cross-language interop sentinel: when any function in this
	// file carries the `#[wasm_bindgen]` attribute, stamp the file
	// node so audit / porting queries can filter by it. The check
	// runs as a post-pass against the already-emitted annotation
	// edges, avoiding the need to thread fileNode through every
	// emit helper that calls emitRustAnnotationEdges.
	const wasmAnnotationID = "annotation::rust::wasm_bindgen"
	for _, e := range result.Edges {
		if e.Kind == graph.EdgeAnnotated && e.To == wasmAnnotationID {
			if fileNode.Meta == nil {
				fileNode.Meta = map[string]any{}
			}
			fileNode.Meta["uses_wasm_bindgen"] = true
			break
		}
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitFunction handles every function_item in the file, classifying it
// by its enclosing container:
//   - direct child of an impl_item's body → KindMethod with receiver
//   - direct child of a trait_item's body → KindFunction (default impl,
//     legacy parity — the old extractor's class-method query did not
//     match trait bodies, so these landed in the free-function pass)
//   - anything else → free function
func (e *RustExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1

	doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash)
	visibility := rustVisibility(def.Node, src)
	typeParams := rustTypeParams(def.Node, src)
	complexity := 0
	if def.Node != nil {
		if body := def.Node.ChildByFieldName("body"); body != nil {
			complexity = RustComplexity(body)
		}
	}

	implType := rustImplMethodReceiver(def.Node, src)
	if implType != "" {
		id := filePath + "::" + implType + "." + name
		if seen[id] {
			return
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":   implType,
			"signature":  "fn " + name + "(...)",
			"visibility": visibility,
		}
		if doc != "" {
			meta["doc"] = doc
		}
		if rt := extractRustReturnType(def.Node, src); rt != "" {
			meta["return_type"] = rt
		}
		if len(typeParams) > 0 {
			meta["type_params"] = typeParams
		}
		if complexity > 1 {
			meta["complexity"] = complexity
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "rust", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		typeID := filePath + "::" + implType
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
		emitRustThrowsEdges(def.Node, src, id, filePath, startLine1, result)
		emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
		return
	}

	// Free function (or trait default-impl). Mirror the legacy
	// rsQFunction pass — emit as KindFunction.
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"signature":  "fn " + name + "(...)",
		"visibility": visibility,
	}
	if doc != "" {
		meta["doc"] = doc
	}
	if len(typeParams) > 0 {
		meta["type_params"] = typeParams
	}
	if complexity > 1 {
		meta["complexity"] = complexity
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustThrowsEdges(def.Node, src, id, filePath, startLine1, result)
	emitRustFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

// rustTypeParams reads the `type_parameters` child of a Rust item
// (function_item, struct_item, enum_item, trait_item, impl_item) and
// returns each declared type parameter as {name, bound} where bound
// is optional. Multi-trait bounds (`T: Send + Sync`) are joined with
// " + ".
func rustTypeParams(item *sitter.Node, src []byte) []map[string]string {
	if item == nil {
		return nil
	}
	tps := item.ChildByFieldName("type_parameters")
	if tps == nil {
		// Some grammar versions don't expose the field name; fall
		// back to a child-type scan.
		for i := 0; i < int(item.ChildCount()); i++ {
			c := item.Child(i)
			if c != nil && c.Type() == "type_parameters" {
				tps = c
				break
			}
		}
	}
	if tps == nil {
		return nil
	}
	var out []map[string]string
	for i := 0; i < int(tps.NamedChildCount()); i++ {
		tp := tps.NamedChild(i)
		if tp == nil {
			continue
		}
		// Lifetimes (`'a`) have their own node type; skip.
		if tp.Type() == "lifetime" {
			continue
		}
		// Plain identifier — `<T>`.
		if tp.Type() == "type_identifier" {
			out = append(out, map[string]string{"name": tp.Content(src)})
			continue
		}
		// Constrained — `<T: Clone>` or `<T = u8>`. Older grammar
		// versions emit these as `constrained_type_parameter`;
		// newer ones flatten them under a `type_parameter` node.
		entry := map[string]string{}
		for j := 0; j < int(tp.ChildCount()); j++ {
			c := tp.Child(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "type_identifier":
				if entry["name"] == "" {
					entry["name"] = c.Content(src)
				}
			case "trait_bounds":
				txt := strings.TrimSpace(c.Content(src))
				txt = strings.TrimPrefix(txt, ":")
				entry["bound"] = strings.TrimSpace(txt)
			}
		}
		// As a final fallback for grammars that store the constraint
		// as raw text alongside the identifier, try to parse the full
		// child content if we got a name but no bound.
		if entry["name"] != "" && entry["bound"] == "" {
			text := strings.TrimSpace(tp.Content(src))
			if i := strings.Index(text, ":"); i >= 0 {
				entry["bound"] = strings.TrimSpace(text[i+1:])
			}
		}
		if entry["name"] != "" {
			out = append(out, entry)
		}
	}
	return out
}

// emitRustThrowsEdges inspects a function's return type for a Result
// wrapper and emits an EdgeThrows to the error type when found. Idiom:
//
//   fn parse(s: &str) -> Result<i32, ParseError> {…}     → throws ParseError
//   fn open(p: &Path) -> Result<File, std::io::Error> {…} → throws Error
//   fn no_error() -> i32 {…}                              → no edge
//
// `Origin: ASTInferred` because we're pattern-matching the return
// type text, not type-checking it.
func emitRustThrowsEdges(funcNode *sitter.Node, src []byte, fromID, filePath string, line int, result *parser.ExtractionResult) {
	if funcNode == nil {
		return
	}
	rt := rustRawReturnType(funcNode, src)
	if rt == "" {
		return
	}
	errType := rustErrorTypeFromResult(rt)
	if errType == "" {
		return
	}
	target := "unresolved::" + errType
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fromID,
		To:       target,
		Kind:     graph.EdgeThrows,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
	})
}

// rustRawReturnType returns the verbatim return-type text of a Rust
// function_item, including generic parameters. Unlike
// extractRustReturnType, it does not normalize the type — preserves
// Result<T, E> shape so the throws extractor can read both arguments.
func rustRawReturnType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	pastArrow := false
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "->" {
			pastArrow = true
			continue
		}
		if pastArrow {
			if child.Type() == "block" {
				return ""
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// rustErrorTypeFromResult parses a Rust return-type string and returns
// the trailing identifier of the error parameter when the type is a
// Result. Handles:
//
//   Result<T, E>            → E
//   Result<T, std::io::E>   → E (trailing ident)
//   Result<T, Box<dyn E>>   → E
//   anyhow::Result<T>       → "" (single-arg form: error type elided)
//
// Returns "" for non-Result returns or when the error type can't be
// extracted unambiguously.
func rustErrorTypeFromResult(rt string) string {
	rt = strings.TrimSpace(rt)
	// Strip leading qualifier like `std::result::` to land on `Result`.
	if i := strings.LastIndex(rt, "::"); i >= 0 {
		head := rt[:i]
		// Only strip qualifier when what follows starts with Result.
		if strings.HasPrefix(rt[i+2:], "Result<") {
			_ = head
			rt = rt[i+2:]
		}
	}
	if !strings.HasPrefix(rt, "Result<") {
		return ""
	}
	inner := strings.TrimPrefix(rt, "Result<")
	inner = strings.TrimSuffix(inner, ">")
	// Split on the top-level comma — depth tracking for nested
	// generics like Result<Vec<T>, MyError>.
	depth := 0
	parts := []string{}
	start := 0
	for i, r := range inner {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, inner[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, inner[start:])
	if len(parts) < 2 {
		return ""
	}
	errPart := strings.TrimSpace(parts[1])
	// Box<dyn ErrType> / Box<dyn ErrType + Send> → ErrType
	for _, prefix := range []string{"Box<dyn ", "Box<", "Arc<dyn ", "Arc<"} {
		if strings.HasPrefix(errPart, prefix) {
			errPart = strings.TrimPrefix(errPart, prefix)
			errPart = strings.TrimSuffix(errPart, ">")
			break
		}
	}
	if i := strings.Index(errPart, "+"); i >= 0 {
		errPart = errPart[:i]
	}
	errPart = strings.TrimSpace(errPart)
	// Strip qualifier like std::io::Error.
	if i := strings.LastIndex(errPart, "::"); i >= 0 {
		errPart = errPart[i+2:]
	}
	// Strip generic instantiation like Error<Foo>.
	if i := strings.Index(errPart, "<"); i >= 0 {
		errPart = errPart[:i]
	}
	errPart = strings.TrimSpace(errPart)
	if errPart == "" || errPart == "_" {
		return ""
	}
	return errPart
}

// rustCollectAttributes walks the previous siblings of an item node
// and returns each `attribute_item` (#[...]) attached to it. Walks
// stop on the first non-attribute sibling. Outer attributes are the
// only form we handle — inner attributes (`#![foo]`) don't apply to a
// specific item.
func rustCollectAttributes(item *sitter.Node) []*sitter.Node {
	if item == nil {
		return nil
	}
	var out []*sitter.Node
	for sib := item.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
		if sib.Type() != "attribute_item" {
			break
		}
		out = append(out, sib)
	}
	return out
}

// emitRustAnnotationEdges turns a slice of attribute_item nodes into
// EdgeAnnotated edges. `#[derive(Trait1, Trait2)]` is expanded into
// one edge per trait — that's the form that lets agents query "find
// every type that derives Debug" with one hop.
func emitRustAnnotationEdges(attrs []*sitter.Node, fromID, filePath string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	for _, attr := range attrs {
		var attrNode *sitter.Node
		for i := 0; i < int(attr.NamedChildCount()); i++ {
			c := attr.NamedChild(i)
			if c != nil && c.Type() == "attribute" {
				attrNode = c
				break
			}
		}
		if attrNode == nil {
			continue
		}
		name, args := rustAttributeNameAndArgs(attrNode, src)
		if name == "" {
			continue
		}
		line := int(attr.StartPoint().Row) + 1
		if name == "derive" && args != "" {
			for _, t := range strings.Split(args, ",") {
				traitName := strings.TrimSpace(t)
				if traitName != "" {
					EmitAnnotationEdge(fromID, "rust", traitName, "", filePath, line, result, seen)
				}
			}
			continue
		}
		EmitAnnotationEdge(fromID, "rust", name, args, filePath, line, result, seen)
	}
}

// rustAttributeNameAndArgs reads an `attribute` AST node (the body of
// an attribute_item) and returns the attribute path's text plus any
// args inside the token_tree.
func rustAttributeNameAndArgs(attr *sitter.Node, src []byte) (string, string) {
	if attr == nil {
		return "", ""
	}
	var name, args string
	for i := 0; i < int(attr.ChildCount()); i++ {
		c := attr.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier", "scoped_identifier", "path":
			if name == "" {
				name = c.Content(src)
			}
		case "token_tree":
			txt := c.Content(src)
			if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
				txt = txt[1 : len(txt)-1]
			}
			args = txt
		}
	}
	return name, args
}

// rustVisibility inspects an item node for a visibility_modifier child
// and returns the canonical visibility string. Default for items
// without a modifier is "private" (Rust default).
func rustVisibility(item *sitter.Node, src []byte) string {
	if item == nil {
		return VisibilityPrivate
	}
	for i := 0; i < int(item.ChildCount()); i++ {
		c := item.Child(i)
		if c == nil {
			continue
		}
		if c.Type() != "visibility_modifier" {
			continue
		}
		text := strings.TrimSpace(c.Content(src))
		switch {
		case text == "pub":
			return VisibilityPublic
		case strings.HasPrefix(text, "pub(crate"):
			return VisibilityInternal
		case strings.HasPrefix(text, "pub(super"), strings.HasPrefix(text, "pub(in"):
			return VisibilityInternal
		case strings.HasPrefix(text, "pub("):
			return VisibilityPublic
		}
	}
	return VisibilityPrivate
}

func (e *RustExtractor) recordTraitMethod(m parser.QueryResult, src []byte, traitMethods map[string][]string) {
	def := m.Captures["sig.def"]
	traitNode := findEnclosingRustContainer(def.Node, "trait_item")
	if traitNode == nil {
		return
	}
	traitName := rustDeclName(traitNode, src)
	if traitName == "" {
		return
	}
	traitMethods[traitName] = append(traitMethods[traitName], m.Captures["sig.name"].Text)
}

func (e *RustExtractor) emitStruct(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["struct.name"].Text
	def := m.Captures["struct.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": rustVisibility(def.Node, src)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"kind":       "enum",
		"visibility": rustVisibility(def.Node, src),
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitTrait(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, traitMethods map[string][]string) {
	name := m.Captures["trait.name"].Text
	def := m.Captures["trait.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": rustVisibility(def.Node, src)}
	if methods, ok := traitMethods[name]; ok {
		meta["methods"] = methods
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitRustAnnotationEdges(rustCollectAttributes(def.Node), id, filePath, src, result, annotationSeen)
	emitRustGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *RustExtractor) emitVariant(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["variant.def"]
	enumNode := findEnclosingRustContainer(def.Node, "enum_item")
	if enumNode == nil {
		return
	}
	enumName := rustDeclName(enumNode, src)
	if enumName == "" {
		return
	}
	variantName := m.Captures["variant.name"].Text
	enumID := filePath + "::" + enumName
	variantID := enumID + "." + variantName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: variantID, Kind: graph.KindVariable, Name: variantName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "rust",
		Meta:      map[string]any{"receiver": enumName, "kind": "enum_variant"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: variantID, To: enumID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RustExtractor) emitField(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["field.def"]
	// Legacy only emitted struct fields. tree-sitter-rust also produces
	// field_declaration nodes inside union_item; skip those.
	structNode := findEnclosingRustContainer(def.Node, "struct_item")
	if structNode == nil {
		return
	}
	structName := rustDeclName(structNode, src)
	if structName == "" {
		return
	}
	fieldName := m.Captures["field.name"].Text
	structID := filePath + "::" + structName
	fieldID := structID + "." + fieldName
	meta := map[string]any{
		"receiver":   structName,
		"visibility": rustVisibility(def.Node, src),
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: fieldID, Kind: graph.KindField, Name: fieldName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "rust",
		Meta:      meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fieldID, To: structID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitNamed handles const_item / static_item — they share the same
// node shape so we collapse them into one helper that takes the
// capture-name prefix.
func (e *RustExtractor) emitNamed(m parser.QueryResult, kind, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	nameCap := m.Captures[kind+".name"]
	def := m.Captures[kind+".def"]
	if nameCap == nil || def == nil {
		return
	}
	name := nameCap.Text
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RustExtractor) emitUse(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["use.path"]
	usePath := strings.ReplaceAll(path.Text, "::", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + usePath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingRustContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingRustContainer(n *sitter.Node, t string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == t {
			return p
		}
	}
	return nil
}

// rustImplMethodReceiver returns the receiver type name when fn is a
// direct member of an impl_item's declaration_list, mirroring the
// legacy rsQImplMethod pattern. Returns "" for trait default impls,
// nested fns, and free functions — the legacy code only treated the
// direct-child case as a method.
func rustImplMethodReceiver(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "impl_item" {
		return ""
	}
	typeNode := grand.ChildByFieldName("type")
	if typeNode == nil {
		return ""
	}
	return typeNode.Content(src)
}

// rustDeclName returns the source text of the `name` field on a Rust
// declaration node (struct_item, enum_item, trait_item, etc.).
func rustDeclName(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return ""
	}
	nameNode := decl.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// extractRustReturnType walks a function_item node to find the return type after `->`.
func extractRustReturnType(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	// In tree-sitter-rust, function_item has children: fn, name, parameters, ->, type, block.
	// Look for a type child after "->".
	pastArrow := false
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		text := string(src[child.StartByte():child.EndByte()])
		if text == "->" {
			pastArrow = true
			continue
		}
		if pastArrow {
			if child.Type() == "block" {
				return ""
			}
			// This should be the return type node.
			rawType := string(src[child.StartByte():child.EndByte()])
			if rt := normalizeRustTypeName(rawType); rt != "" {
				return rt
			}
			return ""
		}
	}
	return ""
}

// normalizeRustTypeName strips references, generics, and module paths from a Rust type.
func normalizeRustTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove reference prefixes.
	t = strings.TrimPrefix(t, "&mut ")
	t = strings.TrimPrefix(t, "&")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Take last segment of module path.
	if idx := strings.LastIndex(t, "::"); idx >= 0 {
		t = t[idx+2:]
	}
	// Skip primitives.
	switch t {
	case "i8", "i16", "i32", "i64", "i128", "isize",
		"u8", "u16", "u32", "u64", "u128", "usize",
		"f32", "f64", "bool", "char", "str", "String",
		"Self", "self":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// inferTypeFromRustExpr inspects a tree-sitter expression node to infer
// the type of a let declaration's RHS.
func inferTypeFromRustExpr(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "struct_expression":
		// Config { port: 8080 } — first named child is the type name.
		if node.NamedChildCount() > 0 {
			typeNode := node.NamedChild(0)
			name := typeNode.Content(src)
			// Strip module path.
			if idx := strings.LastIndex(name, "::"); idx >= 0 {
				name = name[idx+2:]
			}
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}

	case "call_expression":
		// Type::new() — scoped_identifier with path containing ::new
		if node.NamedChildCount() > 0 {
			funcNode := node.NamedChild(0)
			if funcNode.Type() == "scoped_identifier" {
				funcText := funcNode.Content(src)
				// e.g. "Config::new" or "module::Config::new"
				if strings.HasSuffix(funcText, "::new") {
					typePart := strings.TrimSuffix(funcText, "::new")
					// Take last segment.
					if idx := strings.LastIndex(typePart, "::"); idx >= 0 {
						typePart = typePart[idx+2:]
					}
					if len(typePart) > 0 && typePart[0] >= 'A' && typePart[0] <= 'Z' {
						return typePart
					}
				}
			}
		}
	}

	return ""
}
