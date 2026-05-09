package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/java"
)

// qJavaAll is a single tree-sitter query alternating over every pattern
// the Java extractor needs. One tree walk per file replaces the 13
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class/interface/enum
// membership is resolved via a parent walk on the captured node rather
// than nested queries — same behaviour, one cursor pass.
const qJavaAll = `
[
  (class_declaration
    name: (identifier) @class.name) @class.def

  (interface_declaration
    name: (identifier) @iface.name) @iface.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (method_declaration
    name: (identifier) @method.name) @method.def

  (constructor_declaration
    name: (identifier) @ctor.name) @ctor.def

  (enum_constant
    name: (identifier) @enum_member.name) @enum_member.def

  (field_declaration
    type: (_) @fvar.type
    declarator: (variable_declarator
      name: (identifier) @fvar.name)) @fvar.def

  (local_variable_declaration
    type: (_) @lvar.type
    declarator: (variable_declarator
      name: (identifier) @lvar.name)) @lvar.def

  (import_declaration
    (scoped_identifier) @import.path) @import.def

  (method_invocation
    name: (identifier) @call.name) @call.expr

  (method_invocation
    object: (_) @callm.receiver
    name: (identifier) @callm.method) @callm.expr
]
`

// JavaExtractor extracts Java source files into graph nodes and edges.
type JavaExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewJavaExtractor() *JavaExtractor {
	lang := java.GetLanguage()
	return &JavaExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qJavaAll, lang),
	}
}

func (e *JavaExtractor) Language() string     { return "java" }
func (e *JavaExtractor) Extensions() []string { return []string{".java"} }

// --- Deferred match buffers ----------------------------------------

type javaDeferredCall struct {
	name       string // method name
	receiver   string // selector receiver text (empty for plain call)
	line       int    // 1-based call_expression start line
	isSelector bool
}

// javaDeferredVar buffers a variable declaration for the post-pass
// type-environment build. The legacy extractor materialised the env in
// three ordered tiers (lvar explicit, then fvar explicit-no-overwrite,
// then lvar `new Foo()` inference); document-order dispatch alone can't
// reproduce that precedence, so we buffer and resolve at the end.
type javaDeferredVar struct {
	name     string
	explicit string // normalized type from explicit annotation, "" if none
	defNode  *sitter.Node
	isLocal  bool
}

func (e *JavaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "java",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	annotationSeen := make(map[string]bool)
	ifaceMethods := make(map[string][]string) // interface name → declared method names

	var calls []javaDeferredCall
	var varBuf []javaDeferredVar

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["iface.def"] != nil:
			e.emitInterface(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result, seen, annotationSeen)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, seen, annotationSeen, ifaceMethods)

		case m.Captures["ctor.def"] != nil:
			e.emitConstructor(m, filePath, fileID, src, result, seen)

		case m.Captures["enum_member.def"] != nil:
			e.emitEnumMember(m, filePath, src, result)

		case m.Captures["fvar.def"] != nil:
			e.emitField(m, filePath, fileID, src, result, seen)
			// Always buffer for tenv post-pass — interface and enum
			// fields contribute to the type env even though they're
			// not emitted as graph nodes.
			varBuf = append(varBuf, javaDeferredVar{
				name:     m.Captures["fvar.name"].Text,
				explicit: normalizeJavaTypeName(m.Captures["fvar.type"].Text),
				defNode:  m.Captures["fvar.def"].Node,
				isLocal:  false,
			})

		case m.Captures["lvar.def"] != nil:
			varBuf = append(varBuf, javaDeferredVar{
				name:     m.Captures["lvar.name"].Text,
				explicit: normalizeJavaTypeName(m.Captures["lvar.type"].Text),
				defNode:  m.Captures["lvar.def"].Node,
				isLocal:  true,
			})

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, javaDeferredCall{
				name:       m.Captures["callm.method"].Text,
				receiver:   m.Captures["callm.receiver"].Text,
				line:       expr.StartLine + 1,
				isSelector: true,
			})

		case m.Captures["call.expr"] != nil:
			// Plain-call pattern fires for `bar()` AND for the inner
			// `bar` of `foo.bar()` — the legacy extractor emitted both
			// edges, so we mirror that here.
			expr := m.Captures["call.expr"]
			calls = append(calls, javaDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})
		}
	})

	// Stamp interface method names onto interface nodes' Meta["methods"]
	// for IMPLEMENTS inference.
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

	// Build type environment in the same precedence the legacy code used:
	//   1. lvar Tier 0 — explicit annotation (overwrites prior key)
	//   2. fvar Tier 0 — explicit annotation (no overwrite)
	//   3. lvar Tier 1 — walk defNode for object_creation_expression
	tenv := make(typeEnv)
	for _, v := range varBuf {
		if v.isLocal && v.explicit != "" {
			tenv[v.name] = v.explicit
		}
	}
	for _, v := range varBuf {
		if v.isLocal {
			continue
		}
		if v.explicit == "" {
			continue
		}
		if _, exists := tenv[v.name]; exists {
			continue
		}
		tenv[v.name] = v.explicit
	}
	for _, v := range varBuf {
		if !v.isLocal {
			continue
		}
		if _, exists := tenv[v.name]; exists {
			continue
		}
		if v.defNode == nil {
			continue
		}
		walkNodes(v.defNode, func(n *sitter.Node) {
			if n.Type() == "object_creation_expression" {
				typeName := inferTypeFromJavaNewExpr(n, src)
				if typeName != "" {
					tenv[v.name] = typeName
				}
			}
		})
	}

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if c.isSelector {
			if recvType, ok := tenv[c.receiver]; ok {
				edge.Meta = map[string]any{"receiver_type": recvType}
			} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
				if chainType := resolveChainType(c.receiver, tenv, result); chainType != "" {
					edge.Meta = map[string]any{"receiver_type": chainType}
				}
			}
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *JavaExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": javaVisibility(def.Node, src, VisibilityPackage)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitJavaGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

// javaVisibility scans the `modifiers` child of a Java declaration for
// a public/private/protected token. Returns defaultVis when no
// modifier is present (e.g. package-private at top level).
func javaVisibility(decl *sitter.Node, src []byte, defaultVis string) string {
	if decl == nil {
		return defaultVis
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j := 0; j < int(c.ChildCount()); j++ {
			tok := c.Child(j)
			if tok == nil {
				continue
			}
			switch tok.Type() {
			case "public":
				return VisibilityPublic
			case "private":
				return VisibilityPrivate
			case "protected":
				return VisibilityProtected
			}
		}
	}
	return defaultVis
}

func (e *JavaExtractor) emitInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["iface.name"].Text
	def := m.Captures["iface.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{"visibility": javaVisibility(def.Node, src, VisibilityPackage)}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitJavaGenericParamNodes(id, def.Node, src, filePath, def.StartLine+1, result)
}

func (e *JavaExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"kind":       "enum",
		"visibility": javaVisibility(def.Node, src, VisibilityPackage),
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
}

func (e *JavaExtractor) emitEnumMember(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["enum_member.def"]
	enumNode := findEnclosingJavaContainer(def.Node, "enum_declaration")
	if enumNode == nil {
		return
	}
	enumName := javaIdentifierName(enumNode, src)
	if enumName == "" {
		return
	}
	memberName := m.Captures["enum_member.name"].Text
	enumID := filePath + "::" + enumName
	memberID := enumID + "." + memberName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: memberID, Kind: graph.KindVariable, Name: memberName,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "java",
		Meta:      map[string]any{"receiver": enumName, "kind": "enum_member"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: memberID, To: enumID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *JavaExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen, annotationSeen map[string]bool, ifaceMethods map[string][]string) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	startLine1 := def.StartLine + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine1)
	if seen[lineKey] {
		return
	}
	seen[lineKey] = true

	enclosing := findEnclosingJavaContainerAny(def.Node, "class_declaration", "interface_declaration", "enum_declaration")

	// Inside a class — emit as receiver-qualified method (the only
	// container the legacy extractor's class-method query matched).
	if enclosing != nil && enclosing.Type() == "class_declaration" {
		className := javaIdentifierName(enclosing, src)
		if className == "" {
			return
		}
		id := filePath + "::" + className + "." + name
		if seen[id] {
			id = filePath + "::" + className + "." + name + "_L" + fmt.Sprint(startLine1)
		}
		if seen[id] {
			return
		}
		seen[id] = true
		node := &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "java",
			Meta: map[string]any{
				"receiver":   className,
				"visibility": javaVisibility(def.Node, src, VisibilityPackage),
			},
		}
		if def.Node != nil {
			if rt := extractJavaMethodReturnType(def.Node, src); rt != "" {
				node.Meta["return_type"] = rt
			}
		}
		if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
			node.Meta["doc"] = doc
		}
		if def.Node != nil {
			if body := def.Node.ChildByFieldName("body"); body != nil {
				if c := JavaComplexity(body); c > 1 {
					node.Meta["complexity"] = c
				}
			}
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		classID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})

		// Spring @Bean factory methods: when a method in a
		// @Configuration class is decorated with @Bean, Spring calls
		// it at context-init to produce a bean of the method's return
		// type. Emit an EdgeProvides from the config class to the
		// method so the indexer's DI post-pass links consumers typed
		// as the return type back to this factory.
		if def.Node != nil && javaMethodHasAnnotation(def.Node, src, "Bean") {
			if rt, _ := node.Meta["return_type"].(string); rt != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     classID,
					To:       id,
					Kind:     graph.EdgeProvides,
					FilePath: filePath,
					Line:     startLine1,
					Meta: map[string]any{
						"provides_for": rt,
						"binding":      "bean",
					},
				})
			}
		}
		emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
		emitJavaThrowsEdges(def.Node, src, id, filePath, startLine1, result)
		emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
		return
	}

	// Interface method — record the name for IMPLEMENTS inference and
	// emit a flat method node (mirrors legacy fallback).
	if enclosing != nil && enclosing.Type() == "interface_declaration" {
		ifaceName := javaIdentifierName(enclosing, src)
		if ifaceName != "" {
			ifaceMethods[ifaceName] = append(ifaceMethods[ifaceName], name)
		}
	}

	// Fallback: enum method, interface method, or method outside any
	// container — emit flat (legacy `javaQMethod` fallback path).
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "java",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	emitJavaAnnotationEdges(javaCollectAnnotations(def.Node, src), id, filePath, result, annotationSeen)
	emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *JavaExtractor) emitConstructor(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["ctor.def"]
	startLine1 := def.StartLine + 1
	lineKey := filePath + "::_ctor_L" + fmt.Sprint(startLine1)
	if seen[lineKey] {
		return
	}
	seen[lineKey] = true

	enclosing := findEnclosingJavaContainer(def.Node, "class_declaration")
	if enclosing == nil {
		// Legacy fallback path — constructor outside a class. The
		// tree-sitter-java grammar makes this unreachable in valid
		// source, but keep parity with the old extractor.
		name := m.Captures["ctor.name"].Text
		id := filePath + "::" + name + ".<init>"
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name + ".<init>",
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "java",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		return
	}

	className := javaIdentifierName(enclosing, src)
	if className == "" {
		return
	}
	id := filePath + "::" + className + ".<init>"
	if seen[id] {
		id = filePath + "::" + className + ".<init>_L" + fmt.Sprint(startLine1)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	// Stash param-type text so the indexer's Spring-bean post-pass can
	// match consumers to factory methods by type name.
	meta := map[string]any{"receiver": className}
	if params := javaParamsSource(def.Node, src); params != "" {
		meta["params_src"] = params
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: className + ".<init>",
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
	})
	// Constructor params land in the same shape as method params:
	// EdgeParamOf + EdgeTypedAs are how Spring's @Autowired CDI
	// post-pass figures out which beans flow in.
	emitJavaFunctionShape(id, def.Node, src, filePath, startLine1, result)
}

func (e *JavaExtractor) emitField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	def := m.Captures["fvar.def"]
	enclosing := findEnclosingJavaContainer(def.Node, "class_declaration")
	if enclosing == nil {
		return
	}
	className := javaIdentifierName(enclosing, src)
	if className == "" {
		return
	}
	name := m.Captures["fvar.name"].Text
	id := filePath + "::" + className + "." + name
	if seen[id] {
		return
	}
	seen[id] = true
	meta := map[string]any{
		"receiver":   className,
		"visibility": javaVisibility(def.Node, src, VisibilityPackage),
	}
	if t := def.Node.ChildByFieldName("type"); t != nil {
		meta["field_type"] = strings.TrimSpace(t.Content(src))
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangBlockStar); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "java",
		Meta:     meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *JavaExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	path := m.Captures["import.path"]
	importPath := strings.ReplaceAll(path.Text, ".", "/")
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingJavaContainer walks the parent chain of n looking for
// the nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingJavaContainer(n *sitter.Node, t string) *sitter.Node {
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

// findEnclosingJavaContainerAny walks the parent chain of n looking for
// the nearest ancestor whose Type() matches any of types. Returns nil
// if none.
func findEnclosingJavaContainerAny(n *sitter.Node, types ...string) *sitter.Node {
	if n == nil {
		return nil
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		pt := p.Type()
		for _, t := range types {
			if pt == t {
				return p
			}
		}
	}
	return nil
}

// javaIdentifierName returns the source text of the `name` field
// (typically an `identifier`) on a Java declaration node, or "" if
// missing.
func javaIdentifierName(declNode *sitter.Node, src []byte) string {
	if declNode == nil {
		return ""
	}
	nameNode := declNode.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// normalizeJavaTypeName strips generics and array markers from a Java type name.
// "User" -> "User", "List<User>" -> "List", "User[]" -> "User"
func normalizeJavaTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Remove array suffix.
	t = strings.TrimSuffix(t, "[]")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Skip Java primitives and common non-class types.
	switch t {
	case "int", "long", "short", "byte", "float", "double", "boolean", "char", "void", "var", "String":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return "" // skip lowercase type names (primitives)
	}
	return t
}

// javaParamsSource returns the raw source text of a constructor or
// method's formal_parameters child, including the parentheses. Used by
// the DI post-pass to string-match parameter types without a full
// re-parse of the method signature.
func javaParamsSource(methodNode *sitter.Node, src []byte) string {
	if methodNode == nil {
		return ""
	}
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		c := methodNode.NamedChild(i)
		if c != nil && c.Type() == "formal_parameters" {
			return c.Content(src)
		}
	}
	return ""
}

// javaCollectAnnotations walks the `modifiers` child of a Java
// declaration and returns each annotation's bare name and verbatim
// argument text. Mirrors javaMethodHasAnnotation's traversal but
// returns every annotation rather than checking a single name.
func javaCollectAnnotations(decl *sitter.Node, src []byte) []javaAnnotation {
	if decl == nil {
		return nil
	}
	var out []javaAnnotation
	for i := 0; i < int(decl.NamedChildCount()); i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			m := c.NamedChild(j)
			if m == nil {
				continue
			}
			if m.Type() != "marker_annotation" && m.Type() != "annotation" {
				continue
			}
			nameNode := m.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			ann := javaAnnotation{
				name: nameNode.Content(src),
				line: int(m.StartPoint().Row) + 1,
			}
			if argNode := m.ChildByFieldName("arguments"); argNode != nil {
				txt := argNode.Content(src)
				if len(txt) >= 2 && txt[0] == '(' && txt[len(txt)-1] == ')' {
					txt = txt[1 : len(txt)-1]
				}
				ann.args = txt
			}
			out = append(out, ann)
		}
	}
	return out
}

type javaAnnotation struct {
	name string
	args string
	line int
}

// emitJavaThrowsEdges walks a method_declaration's `throws_clause`
// child and emits one EdgeThrows per declared exception type. Java's
// throws clause is the canonical compiler-checked source of an
// exception contract — every checked exception that can propagate
// must appear here, so the resulting edges form a complete
// error-surface for downstream queries.
func emitJavaThrowsEdges(methodNode *sitter.Node, src []byte, fromID, filePath string, line int, result *parser.ExtractionResult) {
	if methodNode == nil {
		return
	}
	for i := 0; i < int(methodNode.ChildCount()); i++ {
		c := methodNode.Child(i)
		if c == nil || c.Type() != "throws" {
			continue
		}
		for j := 0; j < int(c.ChildCount()); j++ {
			t := c.Child(j)
			if t == nil {
				continue
			}
			tt := t.Type()
			if tt != "type_identifier" && tt != "scoped_type_identifier" && tt != "generic_type" {
				continue
			}
			name := strings.TrimSpace(t.Content(src))
			// For scoped_type_identifier (java.io.IOException), keep
			// the trailing identifier — that's what the type-resolver
			// can land on.
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			if i := strings.Index(name, "<"); i >= 0 {
				name = name[:i]
			}
			if name == "" {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     fromID,
				To:       "unresolved::" + name,
				Kind:     graph.EdgeThrows,
				FilePath: filePath,
				Line:     line,
				Origin:   graph.OriginASTResolved,
			})
		}
	}
}

func emitJavaAnnotationEdges(anns []javaAnnotation, fromID, filePath string, result *parser.ExtractionResult, seen map[string]bool) {
	for _, a := range anns {
		if a.name == "" {
			continue
		}
		EmitAnnotationEdge(fromID, "java", a.name, a.args, filePath, a.line, result, seen)
	}
}

// javaMethodHasAnnotation reports whether a method_declaration node
// carries a top-level annotation of the given name (e.g. "Bean",
// "Autowired"). The tree-sitter-java grammar places annotations inside
// a `modifiers` wrapper as either `marker_annotation` (no args) or
// `annotation` (with args). Name is the bare identifier after @.
func javaMethodHasAnnotation(methodNode *sitter.Node, src []byte, name string) bool {
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		c := methodNode.NamedChild(i)
		if c == nil || c.Type() != "modifiers" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			m := c.NamedChild(j)
			if m == nil {
				continue
			}
			if m.Type() != "marker_annotation" && m.Type() != "annotation" {
				continue
			}
			nameNode := m.ChildByFieldName("name")
			if nameNode != nil && nameNode.Content(src) == name {
				return true
			}
		}
	}
	return false
}

// extractJavaMethodReturnType walks a method_declaration node to find
// the return type child (typically a type_identifier) and returns the
// normalized type name.
func extractJavaMethodReturnType(methodNode *sitter.Node, src []byte) string {
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		child := methodNode.NamedChild(i)
		switch child.Type() {
		case "type_identifier":
			return normalizeJavaTypeName(child.Content(src))
		case "generic_type":
			// e.g., List<User> — take the first named child (the base type).
			if child.NamedChildCount() > 0 {
				return normalizeJavaTypeName(child.NamedChild(0).Content(src))
			}
		case "array_type":
			return normalizeJavaTypeName(child.Content(src))
		}
	}
	return ""
}

// inferTypeFromJavaNewExpr extracts the class name from an object_creation_expression node.
// new User(...) -> "User", new ArrayList<String>() -> "ArrayList"
func inferTypeFromJavaNewExpr(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "type_identifier" {
			name := child.Content(src)
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}
