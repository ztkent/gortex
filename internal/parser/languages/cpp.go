package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/cpp"
)

// qCppAll is a single tree-sitter query alternating over every pattern
// the C++ extractor needs. One tree walk per file replaces the 8
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch
// in Extract can branch on which name is set. Class-method extraction
// still walks the class_specifier body inline — C++ methods can have
// declarators other than bare identifiers (destructor_name,
// field_identifier, qualified_identifier), which the legacy code
// handled via extractFuncName and an explicit body walk; keeping that
// walk inside the class.def dispatch preserves behaviour while still
// collapsing the repeated whole-tree scans into one.
const qCppAll = `
[
  (namespace_definition
    name: (namespace_identifier) @ns.name) @ns.def

  (class_specifier
    name: (type_identifier) @class.name) @class.def

  (struct_specifier
    name: (type_identifier) @struct.name) @struct.def

  (enum_specifier
    name: (type_identifier) @enum.name) @enum.def

  (function_definition
    declarator: (function_declarator
      declarator: (identifier) @func.name)) @func.def

  (preproc_include
    path: (_) @include.path) @include.def

  (preproc_def
    name: (identifier) @macro.name) @macro.def

  (preproc_function_def
    name: (identifier) @macrofn.name) @macrofn.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (field_expression
      field: (field_identifier) @callm.method)) @callm.expr
]
`

// CppExtractor extracts C++ source files into graph nodes and edges.
type CppExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCppExtractor() *CppExtractor {
	lang := cpp.GetLanguage()
	return &CppExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCppAll, lang),
	}
}

func (e *CppExtractor) Language() string     { return "cpp" }
func (e *CppExtractor) Extensions() []string { return []string{".cpp", ".cc", ".cxx", ".hpp"} }

// --- Deferred call buffer ----------------------------------------

// cppDeferredCall buffers a call site discovered during the
// per-match walk so the post-pass can attribute it to the enclosing
// function once funcRanges is built. argTypes carries the C++ ADL
// hint set populated by extractCppCallArgTypes.
type cppDeferredCall struct {
	name     string
	line     int
	isMember bool
	argTypes []string
}

func (e *CppExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "cpp",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []cppDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["ns.def"] != nil:
			e.emitNamespace(m, filePath, fileID, result)

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, src, result, seen)

		case m.Captures["struct.def"] != nil:
			e.emitStruct(m, filePath, fileID, result, seen)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, result, seen)

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen)

		case m.Captures["include.def"] != nil:
			e.emitInclude(m, filePath, fileID, result)

		case m.Captures["macro.def"] != nil:
			emitCMacro(m.Captures["macro.def"].Node, false, filePath, fileID, "cpp", src, result, seen)

		case m.Captures["macrofn.def"] != nil:
			emitCMacro(m.Captures["macrofn.def"].Node, true, filePath, fileID, "cpp", src, result, seen)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, cppDeferredCall{
				name:     m.Captures["callm.method"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
				argTypes: extractCppCallArgTypes(expr.Node, src),
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, cppDeferredCall{
				name:     m.Captures["call.name"].Text,
				line:     expr.StartLine + 1,
				argTypes: extractCppCallArgTypes(expr.Node, src),
			})
		}
	})

	// Resolve call edges against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			From: callerID,
		}
		if c.isMember {
			edge.To = "unresolved::*." + c.name
		} else {
			edge.To = "unresolved::" + c.name
		}
		if len(c.argTypes) > 0 {
			edge.Meta = map[string]any{
				"scope_arg_types": strings.Join(c.argTypes, ","),
			}
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *CppExtractor) emitNamespace(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	name := m.Captures["ns.name"].Text
	def := m.Captures["ns.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitClass emits the class node and walks its body inline for methods.
// The inline body walk replaces legacy extractClassMethods and catches
// declarators the outer function_definition query misses
// (field_identifier, destructor_name, qualified_identifier).
func (e *CppExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	className := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	classID := filePath + "::" + className
	if seen[classID] {
		return
	}
	seen[classID] = true
	meta := map[string]any{}
	if ns := enclosingCppNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	if parent := extractCppParentClass(def.Node, src); parent != "" {
		meta["scope_parent"] = parent
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: classID, Kind: graph.KindType, Name: className,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: classID, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	e.walkClassBody(def.Node, src, filePath, fileID, className, classID, seen, result)
}

func (e *CppExtractor) walkClassBody(classNode *sitter.Node, src []byte, filePath, fileID, className, classID string, seen map[string]bool, result *parser.ExtractionResult) {
	var body *sitter.Node
	for i := 0; i < int(classNode.NamedChildCount()); i++ {
		child := classNode.NamedChild(i)
		if child.Type() == "field_declaration_list" {
			body = child
			break
		}
	}
	if body == nil {
		return
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "access_specifier":
			continue
		case "function_definition":
			e.addMethodFromNode(child, src, filePath, fileID, className, classID, seen, result)
		case "declaration_list":
			for j := 0; j < int(child.NamedChildCount()); j++ {
				gc := child.NamedChild(j)
				if gc.Type() == "function_definition" {
					e.addMethodFromNode(gc, src, filePath, fileID, className, classID, seen, result)
				}
			}
		}
	}
}

func (e *CppExtractor) addMethodFromNode(funcNode *sitter.Node, src []byte, filePath, fileID, className, classID string, seen map[string]bool, result *parser.ExtractionResult) {
	methodName := extractFuncName(funcNode, src)
	if methodName == "" {
		return
	}
	startLine := int(funcNode.StartPoint().Row) + 1
	endLine := int(funcNode.EndPoint().Row) + 1

	id := filePath + "::" + className + "." + methodName
	if seen[id] {
		id = filePath + "::" + className + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Mark line so the function_definition dispatcher skips this.
	seen[filePath+"::_method_L"+fmt.Sprint(startLine)] = true

	meta := map[string]any{"receiver": className, "scope_class": className}
	if ns := enclosingCppNamespace(funcNode, src); ns != "" {
		meta["scope_ns"] = ns
	}
	stampCppSignature(meta, funcNode, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "cpp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})
}

func (e *CppExtractor) emitStruct(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
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
		Language: "cpp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CppExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
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
		Language: "cpp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitFunction emits a free function. When the same line was already
// claimed by the class-body walk (seen "_method_L<line>"), this is a
// class method with a bare identifier declarator that was emitted
// through addMethodFromNode — skip the duplicate.
func (e *CppExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine := def.StartLine + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine)
	if seen[lineKey] {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{}
	if ns := enclosingCppNamespace(def.Node, src); ns != "" {
		meta["scope_ns"] = ns
	}
	stampCppSignature(meta, def.Node, src)
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: def.EndLine + 1,
		Language: "cpp", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
}

func (e *CppExtractor) emitInclude(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	pathCap := m.Captures["include.path"]
	includePath := strings.Trim(pathCap.Text, `"<>`)
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + includePath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// extractFuncName walks a function_definition node to find the function name.
// It handles both `identifier` (free functions) and `field_identifier` (methods).
func extractFuncName(funcNode *sitter.Node, src []byte) string {
	for i := 0; i < int(funcNode.NamedChildCount()); i++ {
		child := funcNode.NamedChild(i)
		if child.Type() == "function_declarator" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				gc := child.NamedChild(j)
				switch gc.Type() {
				case "identifier", "field_identifier", "destructor_name":
					return gc.Content(src)
				case "qualified_identifier":
					return lastIdentifier(gc, src)
				}
			}
		}
	}
	return ""
}

// lastIdentifier extracts the last identifier from a qualified_identifier.
func lastIdentifier(node *sitter.Node, src []byte) string {
	name := ""
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "identifier", "field_identifier", "destructor_name":
			name = child.Content(src)
		}
	}
	return name
}

// enclosingCppNamespace walks node up through the tree-sitter AST
// looking for namespace_definition ancestors and concatenates their
// names with "::" (so `namespace a { namespace b { void foo() {} } }`
// produces "a::b"). Anonymous namespaces are skipped — a function
// inside one still belongs to the surrounding namespace for ADL.
//
// Stamped onto every function / method / type node so the resolver's
// scope-based static resolver can prefer same-namespace candidates
// before falling back to directory-locality.
func enclosingCppNamespace(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	var parts []string
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Type() != "namespace_definition" {
			continue
		}
		nameNode := p.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := strings.TrimSpace(nameNode.Content(src))
		if name == "" {
			continue
		}
		parts = append([]string{name}, parts...)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "::")
}

// extractCppParentClass returns the name of the direct base class for
// a C++ class_specifier, or "" if the class has no base. Used by the
// scope-based static resolver to walk the inheritance chain when
// resolving `super`-style calls (C++ doesn't have a literal `super`
// keyword, but Base::method() qualifications follow the same chain).
func extractCppParentClass(classNode *sitter.Node, src []byte) string {
	if classNode == nil {
		return ""
	}
	for i := 0; i < int(classNode.NamedChildCount()); i++ {
		child := classNode.NamedChild(i)
		if child.Type() != "base_class_clause" {
			continue
		}
		for j := 0; j < int(child.NamedChildCount()); j++ {
			sub := child.NamedChild(j)
			switch sub.Type() {
			case "type_identifier", "qualified_identifier":
				return strings.TrimSpace(sub.Content(src))
			}
		}
	}
	return ""
}

// extractCppCallArgTypes returns the type-name hints harvested from a
// C++ call_expression's argument list, used to seed Argument-Dependent
// Lookup. We restrict the harvest to the cases where the argument
// type is structurally unambiguous from the call site alone:
//
//   - `new Type(...)`  → "Type"
//   - `Type{...}`      → "Type" (compound literal / temporary)
//   - `Type(arg)`      → "Type" (functional cast / explicit ctor)
//
// Anything else (bare variables, method-chain returns, expressions)
// is skipped — ADL is best-effort here, and a partial type list is
// strictly better than guessing. The resolver treats an empty hint
// set as "no ADL evidence" and falls through to the regular cascade.
func extractCppCallArgTypes(callNode *sitter.Node, src []byte) []string {
	if callNode == nil {
		return nil
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	var out []string
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		typeName := cppArgTypeHint(arg, src)
		if typeName == "" {
			// Positional placeholder so the overload ranker keeps argument
			// alignment (an unknown arg is compatible with any param). The ADL
			// namespace pass ignores "?" (it yields no namespace).
			typeName = "?"
		}
		out = append(out, typeName)
	}
	return out
}

func cppArgTypeHint(arg *sitter.Node, src []byte) string {
	if arg == nil {
		return ""
	}
	switch arg.Type() {
	case "number_literal":
		if strings.ContainsAny(arg.Content(src), ".eE") && !strings.HasPrefix(arg.Content(src), "0x") {
			return "double"
		}
		return "int"
	case "string_literal", "raw_string_literal", "concatenated_string":
		return "string"
	case "char_literal":
		return "char"
	case "true", "false":
		return "bool"
	case "null", "nullptr":
		return "null"
	case "new_expression":
		if t := arg.ChildByFieldName("type"); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
	case "compound_literal_expression":
		if t := arg.ChildByFieldName("type"); t != nil {
			return strings.TrimSpace(t.Content(src))
		}
	case "call_expression":
		// Functional-cast `Type(arg)`: the function position is a
		// type_identifier or qualified_identifier whose text is the
		// type itself. Distinguishes from regular method calls by
		// checking that the function position resolves to a type
		// name (proper-cased, single-segment) — heuristic but safe
		// because the resolver treats ADL hints as evidence, not
		// truth.
		if f := arg.ChildByFieldName("function"); f != nil {
			switch f.Type() {
			case "type_identifier", "qualified_identifier":
				return strings.TrimSpace(f.Content(src))
			}
		}
	}
	return ""
}
