package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/rust"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
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
	traitMethods := make(map[string][]string) // trait name → declared method names

	var calls []rustDeferredCall
	var lets []rustDeferredLet

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen)

		case m.Captures["sig.def"] != nil:
			e.recordTraitMethod(m, src, traitMethods)

		case m.Captures["struct.def"] != nil:
			e.emitStruct(m, filePath, fileID, result, seen)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, result, seen)

		case m.Captures["trait.def"] != nil:
			e.emitTrait(m, filePath, fileID, result, seen, traitMethods)

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
func (e *RustExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1

	implType := rustImplMethodReceiver(def.Node, src)
	if implType != "" {
		id := filePath + "::" + implType + "." + name
		if seen[id] {
			return
		}
		seen[id] = true
		meta := map[string]any{
			"receiver":  implType,
			"signature": "fn " + name + "(...)",
		}
		if rt := extractRustReturnType(def.Node, src); rt != "" {
			meta["return_type"] = rt
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
		return
	}

	// Free function (or trait default-impl). Mirror the legacy
	// rsQFunction pass — emit as KindFunction.
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: map[string]any{"signature": "fn " + name + "(...)"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
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

func (e *RustExtractor) emitStruct(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["struct.name"].Text
	def := m.Captures["struct.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RustExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust",
		Meta:     map[string]any{"kind": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *RustExtractor) emitTrait(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool, traitMethods map[string][]string) {
	name := m.Captures["trait.name"].Text
	def := m.Captures["trait.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{}
	if methods, ok := traitMethods[name]; ok {
		meta["methods"] = methods
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "rust", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
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
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: fieldID, Kind: graph.KindVariable, Name: fieldName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "rust",
		Meta:      map[string]any{"receiver": structName, "kind": "struct_field"},
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
