package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/csharp"
)

// qCSharpAll is a single tree-sitter query alternating over every
// pattern the C# extractor needs. One tree walk per file replaces the
// 13 `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class / struct / interface
// membership for methods, constructors, fields, and properties is
// resolved via a parent walk on the captured node — the legacy nested
// queries duplicated each member pattern across class_declaration and
// struct_declaration; the parent walk collapses them into a single
// pattern per member kind.
const qCSharpAll = `
[
  (namespace_declaration
    name: (_) @ns.name) @ns.def

  (class_declaration
    name: (identifier) @class.name) @class.def

  (interface_declaration
    name: (identifier) @iface.name) @iface.def

  (struct_declaration
    name: (identifier) @struct.name) @struct.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (method_declaration
    name: (identifier) @method.name) @method.def

  (constructor_declaration
    name: (identifier) @ctor.name) @ctor.def

  (field_declaration
    (variable_declaration
      (variable_declarator
        name: (identifier) @field.name))) @field.def

  (property_declaration
    name: (identifier) @prop.name) @prop.def

  (using_directive (_) @using.path) @using.def

  (invocation_expression
    function: (identifier) @call.name) @call.expr

  (invocation_expression
    function: (member_access_expression
      expression: (_) @callm.receiver
      name: (identifier) @callm.method)) @callm.expr

  (local_declaration_statement
    (variable_declaration
      type: (_) @lvar.type
      (variable_declarator
        (identifier) @lvar.name))) @lvar.def
]
`

// CSharpExtractor extracts C# source files into graph nodes and edges.
type CSharpExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCSharpExtractor() *CSharpExtractor {
	lang := csharp.GetLanguage()
	return &CSharpExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCSharpAll, lang),
	}
}

func (e *CSharpExtractor) Language() string     { return "csharp" }
func (e *CSharpExtractor) Extensions() []string { return []string{".cs"} }

// --- Deferred match buffers ----------------------------------------

type csharpDeferredCall struct {
	name     string
	receiver string
	line     int
	isMember bool
}

// csharpDeferredLocal buffers a local variable declaration for the
// post-pass type-env build. Matches the legacy two-stage pass: Tier 0
// records explicit types (`Foo svc = ...`); Tier 1 walks the def node
// for `var svc = new Foo()` to recover the type when Tier 0 left a
// "var" key without a real annotation.
type csharpDeferredLocal struct {
	name    string
	rawType string
	defNode *sitter.Node
}

func (e *CSharpExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "csharp",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	ifaceMethods := make(map[string][]string) // interface name → method names

	var calls []csharpDeferredCall
	var locals []csharpDeferredLocal

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["ns.def"] != nil:
			e.emitNamespace(m, filePath, fileID, result, seen)

		case m.Captures["class.def"] != nil:
			e.emitContainer(m, "class", graph.KindType, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["iface.def"] != nil:
			e.emitContainer(m, "iface", graph.KindInterface, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["struct.def"] != nil:
			e.emitContainer(m, "struct", graph.KindType, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["enum.def"] != nil:
			e.emitContainer(m, "enum", graph.KindType, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen, annotationSeen, ifaceMethods)

		case m.Captures["ctor.def"] != nil:
			e.emitConstructor(m, filePath, fileID, src, result, seen)

		case m.Captures["field.def"] != nil:
			e.emitField(m, filePath, fileID, src, result, seen)

		case m.Captures["prop.def"] != nil:
			e.emitProperty(m, filePath, fileID, src, result, seen)

		case m.Captures["using.def"] != nil:
			e.emitUsing(m, filePath, fileID, result)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, csharpDeferredCall{
				name:     m.Captures["callm.method"].Text,
				receiver: m.Captures["callm.receiver"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, csharpDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["lvar.def"] != nil:
			locals = append(locals, csharpDeferredLocal{
				name:    m.Captures["lvar.name"].Text,
				rawType: m.Captures["lvar.type"].Text,
				defNode: m.Captures["lvar.def"].Node,
			})
		}
	})

	// Stamp interface method names onto interface nodes' Meta["methods"].
	for _, n := range result.Nodes {
		if n.Kind != graph.KindInterface {
			continue
		}
		if methods, ok := ifaceMethods[n.Name]; ok {
			if n.Meta == nil {
				n.Meta = make(map[string]any)
			}
			n.Meta["methods"] = methods
		}
	}

	// Build type environment in legacy precedence:
	//   Tier 0 — explicit type annotations (skip "var" placeholder)
	//   Tier 1 — `var x = new Foo()` walk for `var`-keyed locals only
	tenv := make(typeEnv)
	for _, l := range locals {
		typeName := normalizeCSharpTypeName(l.rawType)
		if typeName != "" && typeName != "var" {
			tenv[l.name] = typeName
		}
	}
	for _, l := range locals {
		if _, exists := tenv[l.name]; exists {
			continue
		}
		if l.rawType != "var" {
			continue
		}
		if l.defNode == nil {
			continue
		}
		walkNodes(l.defNode, func(n *sitter.Node) {
			if n.Type() == "object_creation_expression" {
				typeName := inferTypeFromCSharpNew(n, src)
				if typeName != "" {
					tenv[l.name] = typeName
				}
			}
		})
	}

	// Resolve calls against funcRanges + tenv.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if c.isMember {
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

func (e *CSharpExtractor) emitNamespace(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["ns.name"].Text
	def := m.Captures["ns.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitContainer collapses the per-kind class/interface/struct/enum
// node emission. The capture-name prefix selects which capture set to
// read from (the legacy code repeated this body four times).
func (e *CSharpExtractor) emitContainer(m parser.QueryResult, kind string, nodeKind graph.NodeKind, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures[kind+".name"].Text
	def := m.Captures[kind+".def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": csharpVisibility(def.Node, src, VisibilityInternal)}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: nodeKind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitCSharpAnnotationEdges(csharpCollectAttributes(def.Node, src), id, filePath, result, annotationSeen)
	emitCSharpGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

// csharpVisibility scans a declaration's modifier children for an
// access modifier. C# defaults are container-dependent — defaultVis is
// "internal" for top-level types and "private" for class members.
func csharpVisibility(decl *sitter.Node, src []byte, defaultVis string) string {
	if decl == nil {
		return defaultVis
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil {
			continue
		}
		if c.Type() != "modifier" {
			continue
		}
		switch strings.TrimSpace(c.Content(src)) {
		case "public":
			return VisibilityPublic
		case "private":
			return VisibilityPrivate
		case "protected":
			return VisibilityProtected
		case "internal":
			return VisibilityInternal
		}
	}
	return defaultVis
}

// csharpCollectAttributes walks a declaration's children for
// `attribute_list` nodes ([Attr1, Attr2(...)]) and returns each
// attribute's bare name plus verbatim args. Multiple attributes can
// appear inside one bracket pair, and multiple bracket pairs can
// stack on the same declaration.
func csharpCollectAttributes(decl *sitter.Node, src []byte) []javaAnnotation {
	if decl == nil {
		return nil
	}
	var out []javaAnnotation
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "attribute_list" {
			continue
		}
		for j := 0; j < int(c.ChildCount()); j++ {
			a := c.Child(j)
			if a == nil || a.Type() != "attribute" {
				continue
			}
			var name, args string
			line := int(a.StartPoint().Row) + 1
			if nm := a.ChildByFieldName("name"); nm != nil {
				name = nm.Content(src)
			}
			for k := 0; k < int(a.ChildCount()); k++ {
				inner := a.Child(k)
				if inner == nil {
					continue
				}
				if inner.Type() == "attribute_argument_list" {
					txt := inner.Content(src)
					if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
						txt = txt[1 : len(txt)-1]
					}
					args = txt
				}
			}
			if name != "" {
				out = append(out, javaAnnotation{name: name, args: args, line: line})
			}
		}
	}
	return out
}

func emitCSharpAnnotationEdges(anns []javaAnnotation, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range anns {
		if a.name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "csharp", a.name, a.args, filePath, a.line, result, seen)
	}
}

// extractCSharpDoc tries the XML-doc form first (/// <summary>…) and
// falls back to /** … */ block comments (less common in C# but valid).
func extractCSharpDoc(src []byte, startRow int) string {
	if d := ExtractDocAbove(src, startRow, DocLangCSharpXML); d != "" {
		return d
	}
	return ExtractDocAbove(src, startRow, DocLangBlockStar)
}

func (e *CSharpExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, ifaceMethods map[string][]string) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1

	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration", "interface_declaration")
	if owner.kind == "" {
		// Method outside a recognised container — legacy didn't emit
		// these (its nested queries required class/struct/interface
		// parentage), so skip.
		return
	}

	// Interface methods: legacy only collected names; no graph node was
	// emitted for them. Mirror that.
	if owner.kind == "interface_declaration" {
		ifaceMethods[owner.name] = append(ifaceMethods[owner.name], name)
		return
	}

	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		id = filePath + "::" + owner.name + "." + name + "_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, VisibilityPrivate),
	}
	if rt := extractCSharpMethodReturnType(def.Node, src, name); rt != "" {
		meta["return_type"] = rt
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	emitCSharpAnnotationEdges(csharpCollectAttributes(def.Node, src), id, filePath, result, annotationSeen)
	if body := csharpFunctionBody(def.Node); body != nil {
		emitCSharpAsyncSpawns(id, body, src, filePath, result)
	}
	emitCSharpFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *CSharpExtractor) emitConstructor(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["ctor.def"]
	startLine1 := def.StartLine + 1
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration")
	if owner.kind == "" {
		return
	}
	id := filePath + "::" + owner.name + ".<init>"
	if seen[id] {
		id = filePath + "::" + owner.name + ".<init>_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: owner.name + ".<init>",
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     map[string]any{"receiver": owner.name},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	// Constructor params: same shape as methods so DI containers and
	// codegen tooling see the dependencies they need.
	if body := csharpFunctionBody(def.Node); body != nil {
		emitCSharpAsyncSpawns(id, body, src, filePath, result)
	}
	emitCSharpFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *CSharpExtractor) emitField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["field.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration")
	if owner.kind == "" {
		return
	}
	name := m.Captures["field.name"].Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, VisibilityPrivate),
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CSharpExtractor) emitProperty(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["prop.def"]
	owner := csharpDirectMemberOwner(def.Node, src, "class_declaration", "struct_declaration")
	if owner.kind == "" {
		return
	}
	name := m.Captures["prop.name"].Text
	id := filePath + "::" + owner.name + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":   owner.name,
		"visibility": csharpVisibility(def.Node, src, VisibilityPrivate),
		"kind":       "property",
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := extractCSharpDoc(src, def.StartLine); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "csharp",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	ownerID := filePath + "::" + owner.name
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CSharpExtractor) emitUsing(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["using.path"]
	importPath := strings.ReplaceAll(path.Text, ".", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

type csharpOwner struct {
	kind string // class_declaration / struct_declaration / interface_declaration
	name string
}

// csharpDirectMemberOwner mirrors the legacy nested queries: the
// member must be a direct child of the container's declaration_list.
// Returns kind == "" when the member isn't directly inside one of the
// allowed container kinds (skipping nested types, top-level statements,
// etc. — none of which the legacy extractor handled).
func csharpDirectMemberOwner(member *sitter.Node, src []byte, allowed ...string) csharpOwner {
	if member == nil {
		return csharpOwner{}
	}
	parent := member.Parent()
	if parent == nil || parent.Type() != "declaration_list" {
		return csharpOwner{}
	}
	grand := parent.Parent()
	if grand == nil {
		return csharpOwner{}
	}
	gtype := grand.Type()
	for _, a := range allowed {
		if gtype == a {
			nameNode := grand.ChildByFieldName("name")
			if nameNode == nil {
				return csharpOwner{}
			}
			return csharpOwner{kind: gtype, name: nameNode.Content(src)}
		}
	}
	return csharpOwner{}
}

// extractCSharpMethodReturnType walks a method_declaration node for
// the type child preceding the method name.
func extractCSharpMethodReturnType(methodNode *sitter.Node, src []byte, methodName string) string {
	if methodNode == nil {
		return ""
	}
	for i := 0; i < int(methodNode.ChildCount()); i++ {
		child := methodNode.Child(i)
		if child.Type() == "identifier" && string(src[child.StartByte():child.EndByte()]) == methodName {
			break
		}
		switch child.Type() {
		case "predefined_type", "identifier", "qualified_name", "generic_name",
			"nullable_type", "array_type", "tuple_type":
			rawType := string(src[child.StartByte():child.EndByte()])
			if rt := normalizeCSharpTypeName(rawType); rt != "" && rt != "var" {
				return rt
			}
		}
	}
	return ""
}

// normalizeCSharpTypeName strips generics and nullable markers from a C# type name.
func normalizeCSharpTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove nullable suffix.
	t = strings.TrimSuffix(t, "?")
	// Remove array suffix.
	if idx := strings.Index(t, "["); idx > 0 {
		t = t[:idx]
	}
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Skip C# primitives and keywords.
	switch t {
	case "var", "int", "long", "short", "byte", "float", "double", "decimal",
		"bool", "char", "string", "object", "void", "dynamic":
		if t == "var" {
			return "var" // caller handles this specially
		}
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// inferTypeFromCSharpNew extracts the type name from a C# object_creation_expression.
// new UserService(...) -> "UserService"
func inferTypeFromCSharpNew(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "identifier" || child.Type() == "type_identifier" ||
			child.Type() == "generic_name" || child.Type() == "qualified_name" {
			name := child.Content(src)
			// Strip generics from generic_name.
			if idx := strings.Index(name, "<"); idx > 0 {
				name = name[:idx]
			}
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}
