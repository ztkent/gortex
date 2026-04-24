package languages

import (
	"fmt"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/golang"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// qGoAll is a single tree-sitter query that alternates over every pattern
// the Go extractor needs. One tree walk per file replaces the 20+
// `parser.RunQuery` calls the previous design made per Extract call
// (each of which recompiled its query and ran an independent cursor
// over the whole tree). Capture names are disjoint across patterns so
// the dispatch in Extract can branch on which name is set.
const qGoAll = `
[
  (package_clause (package_identifier) @pkg.name)

  (function_declaration
    name: (identifier) @func.name
    parameters: (parameter_list) @func.params
    result: (_)? @func.result) @func.def

  (method_declaration
    receiver: (parameter_list) @method.receiver
    name: (field_identifier) @method.name
    parameters: (parameter_list) @method.params
    result: (_)? @method.result) @method.def

  (type_declaration
    (type_spec
      name: (type_identifier) @typedef.name
      type: (_) @typedef.body)) @typedef.def

  (type_declaration
    (type_alias
      name: (type_identifier) @alias.name
      type: (_) @alias.type)) @alias.def

  (import_spec
    name: (package_identifier)? @import.alias
    path: (interpreted_string_literal) @import.path) @import.spec

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (selector_expression
      operand: (_) @callm.receiver
      field: (field_identifier) @callm.method)) @callm.expr

  (var_declaration
    (var_spec
      name: (identifier) @var.name
      type: (_)? @var.type)) @var.def

  (const_declaration
    (const_spec
      name: (identifier) @const.name)) @const.def

  (short_var_declaration
    left: (expression_list (identifier) @svar.name)
    right: (expression_list (_) @svar.value)) @svar.def

  (composite_literal
    type: (type_identifier) @comp.type) @comp.expr

  (composite_literal
    type: (qualified_type
      package: (package_identifier) @compq.pkg
      name: (type_identifier) @compq.type)) @compq.expr

  (field_declaration
    type: (type_identifier) @ftype.name) @ftype.decl

  (const_spec
    type: (type_identifier) @ctype.name) @ctype.decl

  (var_spec
    type: (type_identifier) @vtype.name) @vtype.decl

  (parameter_declaration
    type: (type_identifier) @ptype.name) @ptype.decl

  (argument_list
    (selector_expression
      operand: (_) @selarg.receiver
      field: (field_identifier) @selarg.field)) @selarg.list

  (argument_list
    (identifier) @identarg.name) @identarg.list

  (keyed_element
    (literal_element (identifier) @fieldval.key)
    (literal_element (identifier) @fieldval.value)) @fieldval.elem

  (keyed_element
    (literal_element (identifier) @fieldsel.key)
    (literal_element
      (selector_expression
        operand: (_) @fieldsel.receiver
        field: (field_identifier) @fieldsel.method))) @fieldsel.elem
]
`

// GoExtractor extracts Go source files into graph nodes and edges.
type GoExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewGoExtractor() *GoExtractor {
	lang := golang.GetLanguage()
	return &GoExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qGoAll, lang),
	}
}

func (e *GoExtractor) Language() string     { return "go" }
func (e *GoExtractor) Extensions() []string { return []string{".go"} }

// --- Deferred match buffers ----------------------------------------

type goDeferredCall struct {
	callName    string // plain call
	method      string // selector call method name
	receiver    string // selector call receiver text
	line        int    // 1-based line of call_expression
	isSelector  bool
}

type goDeferredTypeRef struct {
	typeName string
	pkg      string // optional qualifier
	line     int
	kind     graph.EdgeKind
}

type goDeferredValueSel struct {
	field    string
	receiver string
	line     int
	kind     graph.EdgeKind
}

type goDeferredValueIdent struct {
	name string
	line int
}

func (e *GoExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filePath,
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   int(root.EndPoint().Row) + 1,
		Language:  "go",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	imports := map[string]string{} // alias → importPath
	tenv := make(typeEnv)
	seenTypeName := map[string]bool{} // dedup when alias + typedef match same name

	var calls []goDeferredCall
	var typeRefs []goDeferredTypeRef
	var instantiates []goDeferredTypeRef
	var valueSels []goDeferredValueSel
	var valueIdents []goDeferredValueIdent
	var fieldValSels []goDeferredValueSel
	var fieldValIdents []goDeferredValueIdent

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		// Package: just a marker; not emitted as a graph node.
		case m.Captures["pkg.name"] != nil:
			// No-op (the package name is not currently surfaced as a node).

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result)

		case m.Captures["typedef.def"] != nil:
			e.emitTypeDecl(m, filePath, fileID, src, result, seenTypeName)

		case m.Captures["alias.def"] != nil:
			e.emitTypeAlias(m, filePath, fileID, result, seenTypeName)

		case m.Captures["import.spec"] != nil:
			e.emitImport(m, filePath, fileID, result, imports)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, goDeferredCall{
				callName: m.Captures["call.name"].Text,
				line:     expr.StartLine + 1,
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, goDeferredCall{
				method:     m.Captures["callm.method"].Text,
				receiver:   m.Captures["callm.receiver"].Text,
				line:       expr.StartLine + 1,
				isSelector: true,
			})

		case m.Captures["var.def"] != nil:
			e.emitVar(m, filePath, fileID, result, tenv)

		case m.Captures["const.def"] != nil:
			e.emitConst(m, filePath, fileID, result)

		case m.Captures["svar.def"] != nil:
			e.recordShortVarType(m, src, tenv)

		case m.Captures["comp.expr"] != nil:
			expr := m.Captures["comp.expr"]
			instantiates = append(instantiates, goDeferredTypeRef{
				typeName: m.Captures["comp.type"].Text,
				line:     expr.StartLine + 1,
				kind:     graph.EdgeInstantiates,
			})

		case m.Captures["compq.expr"] != nil:
			expr := m.Captures["compq.expr"]
			instantiates = append(instantiates, goDeferredTypeRef{
				typeName: m.Captures["compq.type"].Text,
				pkg:      m.Captures["compq.pkg"].Text,
				line:     expr.StartLine + 1,
				kind:     graph.EdgeInstantiates,
			})

		case m.Captures["ftype.decl"] != nil:
			decl := m.Captures["ftype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ftype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["ctype.decl"] != nil:
			decl := m.Captures["ctype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ctype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["vtype.decl"] != nil:
			decl := m.Captures["vtype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["vtype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["ptype.decl"] != nil:
			decl := m.Captures["ptype.decl"]
			typeRefs = append(typeRefs, goDeferredTypeRef{
				typeName: m.Captures["ptype.name"].Text,
				line:     decl.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["selarg.list"] != nil:
			list := m.Captures["selarg.list"]
			valueSels = append(valueSels, goDeferredValueSel{
				field:    m.Captures["selarg.field"].Text,
				receiver: m.Captures["selarg.receiver"].Text,
				line:     list.StartLine + 1,
				kind:     graph.EdgeReferences,
			})

		case m.Captures["identarg.list"] != nil:
			list := m.Captures["identarg.list"]
			valueIdents = append(valueIdents, goDeferredValueIdent{
				name: m.Captures["identarg.name"].Text,
				line: list.StartLine + 1,
			})

		case m.Captures["fieldval.elem"] != nil:
			elem := m.Captures["fieldval.elem"]
			fieldValIdents = append(fieldValIdents, goDeferredValueIdent{
				name: m.Captures["fieldval.value"].Text,
				line: elem.StartLine + 1,
			})

		case m.Captures["fieldsel.elem"] != nil:
			elem := m.Captures["fieldsel.elem"]
			fieldValSels = append(fieldValSels, goDeferredValueSel{
				field:    m.Captures["fieldsel.method"].Text,
				receiver: m.Captures["fieldsel.receiver"].Text,
				line:     elem.StartLine + 1,
				kind:     graph.EdgeReferences,
			})
		}
	})

	// All function/method nodes have been emitted; now map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)

	// --- Calls ---
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if !c.isSelector {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + c.callName,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			continue
		}
		if importPath, ok := imports[c.receiver]; ok {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::extern::" + importPath + "::" + c.method,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			continue
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + c.method,
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
	}

	// --- Composite literals (instantiations) ---
	for _, r := range instantiates {
		callerID := findEnclosingFunc(funcRanges, r.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + r.typeName,
			Kind: r.kind, FilePath: filePath, Line: r.line,
		})
	}

	// --- Type assertions + declaration type references ---
	for _, r := range typeRefs {
		callerID := findEnclosingFunc(funcRanges, r.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + r.typeName,
			Kind: r.kind, FilePath: filePath, Line: r.line,
		})
	}

	// --- Function/method values passed as arguments ---
	// Selector expression in arg position: h.handleHealth
	for _, v := range valueSels {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := tenv[v.receiver]; ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	// Bare identifier in arg position: funcName as a value.
	for _, v := range valueIdents {
		if isGoBuiltinOrKeyword(v.name) {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + v.name,
			Kind: graph.EdgeReferences, FilePath: filePath, Line: v.line,
		})
	}

	// Bare identifier as struct field value: &X{RunE: runClean}
	for _, v := range fieldValIdents {
		if isGoBuiltinOrKeyword(v.name) {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + v.name,
			Kind: graph.EdgeReferences, FilePath: filePath, Line: v.line,
		})
	}

	// Selector as struct field value: {Handler: h.handleHealth}
	for _, v := range fieldValSels {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := tenv[v.receiver]; ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *GoExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id := filePath + "::" + name
	node := &graph.Node{
		ID:        id,
		Kind:      graph.KindFunction,
		Name:      name,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "go",
		Meta:      make(map[string]any),
	}
	node.Meta["signature"] = buildFuncSignature(name, m.Captures["func.params"], m.Captures["func.result"])
	if resultCap, ok := m.Captures["func.result"]; ok && resultCap.Text != "" {
		if rt := normalizeGoTypeName(resultCap.Text); rt != "" {
			node.Meta["return_type"] = rt
		}
	}
	scanGoPragmas(src, def.StartLine, node)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *GoExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	receiverText := m.Captures["method.receiver"].Text
	receiverType := extractReceiverType(receiverText)

	id := filePath + "::" + receiverType + "." + name
	node := &graph.Node{
		ID:        id,
		Kind:      graph.KindMethod,
		Name:      name,
		FilePath:  filePath,
		StartLine: def.StartLine + 1,
		EndLine:   def.EndLine + 1,
		Language:  "go",
		Meta: map[string]any{
			"receiver": receiverType,
		},
	}
	node.Meta["signature"] = buildMethodSignature(receiverText, name, m.Captures["method.params"], m.Captures["method.result"])
	if resultCap, ok := m.Captures["method.result"]; ok && resultCap.Text != "" {
		if rt := normalizeGoTypeName(resultCap.Text); rt != "" {
			node.Meta["return_type"] = rt
		}
	}
	scanGoPragmas(src, def.StartLine, node)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})

	typeID := filePath + "::" + receiverType
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitTypeDecl handles the generic `type X <body>` form. The body node
// discriminates struct vs interface vs named primitive — interfaces
// carry their method-signature set in Meta for structural inference.
func (e *GoExtractor) emitTypeDecl(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["typedef.name"].Text
	if seen[name] {
		return
	}
	seen[name] = true
	def := m.Captures["typedef.def"]
	body := m.Captures["typedef.body"]
	id := filePath + "::" + name

	node := &graph.Node{
		ID: id, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
	}
	if body != nil && body.Node != nil && body.Node.Type() == "interface_type" {
		node.Kind = graph.KindInterface
		node.Meta = map[string]any{"methods": extractInterfaceMethods(body.Node, src)}
	} else {
		node.Kind = graph.KindType
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *GoExtractor) emitTypeAlias(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["alias.name"].Text
	if seen[name] {
		return
	}
	seen[name] = true
	def := m.Captures["alias.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitImport records an import edge and populates the alias→path map
// used when classifying selector calls against imported packages. Blank
// and dot imports are skipped in the map (they don't introduce a
// callable identifier) but still produce EdgeImports.
func (e *GoExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, imports map[string]string) {
	pathCap := m.Captures["import.path"]
	importPath := strings.Trim(pathCap.Text, `"`)
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + importPath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
	})
	alias := ""
	if a, ok := m.Captures["import.alias"]; ok {
		alias = strings.TrimSpace(a.Text)
	}
	switch alias {
	case "_", ".":
		return
	case "":
		alias = importPath
		if i := strings.LastIndex(importPath, "/"); i >= 0 {
			alias = importPath[i+1:]
		}
	}
	imports[alias] = importPath
}

func (e *GoExtractor) emitVar(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, tenv typeEnv) {
	nameCap := m.Captures["var.name"]
	def := m.Captures["var.def"]
	if nameCap == nil || nameCap.Text == "" || nameCap.Text == "_" {
		return
	}
	name := nameCap.Text
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if typeCap, ok := m.Captures["var.type"]; ok && typeCap.Text != "" {
		if typeName := normalizeGoTypeName(typeCap.Text); typeName != "" {
			tenv[name] = typeName
		}
	}
}

func (e *GoExtractor) emitConst(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	nameCap := m.Captures["const.name"]
	def := m.Captures["const.def"]
	if nameCap == nil || nameCap.Text == "" || nameCap.Text == "_" {
		return
	}
	name := nameCap.Text
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// recordShortVarType infers a type from the RHS expression of a short
// variable declaration (`x := NewFoo()` or `x := &Foo{...}`) and records
// it in tenv so subsequent selector-call edges attach a receiver_type
// meta. No graph output — short-var-declared locals are not first-class
// nodes in the current schema.
func (e *GoExtractor) recordShortVarType(m parser.QueryResult, src []byte, tenv typeEnv) {
	name := m.Captures["svar.name"].Text
	valueCap := m.Captures["svar.value"]
	if valueCap == nil || valueCap.Node == nil {
		return
	}
	if inferred := inferTypeFromGoExpr(valueCap.Node, src); inferred != "" {
		tenv[name] = inferred
	}
}

// --- Helpers -------------------------------------------------------

type funcRange struct {
	id        string
	startLine int
	endLine   int
}

func buildFuncRanges(result *parser.ExtractionResult) []funcRange {
	var ranges []funcRange
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
			ranges = append(ranges, funcRange{
				id: n.ID, startLine: n.StartLine, endLine: n.EndLine,
			})
		}
	}
	return ranges
}

func findEnclosingFunc(ranges []funcRange, line int) string {
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			return r.id
		}
	}
	return ""
}

// extractReceiverType extracts the type name from a Go receiver parameter list.
// "(s *Server)" -> "Server", "(s Server)" -> "Server".
func extractReceiverType(receiver string) string {
	receiver = strings.Trim(receiver, "()")
	parts := strings.Fields(receiver)
	if len(parts) == 0 {
		return ""
	}
	typePart := parts[len(parts)-1]
	typePart = strings.TrimPrefix(typePart, "*")
	return typePart
}

func buildFuncSignature(name string, params, result *parser.CapturedNode) string {
	sig := fmt.Sprintf("func %s%s", name, captureText(params))
	if result != nil && result.Text != "" {
		sig += " " + result.Text
	}
	return sig
}

func buildMethodSignature(receiver, name string, params, result *parser.CapturedNode) string {
	sig := fmt.Sprintf("func (%s) %s%s", receiver, name, captureText(params))
	if result != nil && result.Text != "" {
		sig += " " + result.Text
	}
	return sig
}

// extractInterfaceMethods walks the children of an interface_type node
// and returns the names of all method_spec / method_elem entries.
func extractInterfaceMethods(ifaceNode *sitter.Node, src []byte) []string {
	var methods []string
	for i := 0; i < int(ifaceNode.NamedChildCount()); i++ {
		child := ifaceNode.NamedChild(i)
		if child.Type() == "method_elem" || child.Type() == "method_spec" {
			for j := 0; j < int(child.NamedChildCount()); j++ {
				nameNode := child.NamedChild(j)
				if nameNode.Type() == "field_identifier" {
					methods = append(methods, nameNode.Content(src))
					break
				}
			}
		}
	}
	return methods
}

func captureText(c *parser.CapturedNode) string {
	if c == nil {
		return "()"
	}
	return c.Text
}

// --- Type environment -----------------------------------------------

// typeEnv maps variable name → inferred type name within a file.
type typeEnv map[string]string

// normalizeGoTypeName strips pointer prefix and package qualifier.
// "*pkg.Foo" → "Foo", "[]*Foo" → "Foo", "map[K]V" → "" (skipped —
// receiver typing doesn't help for map/slice/chan types).
func normalizeGoTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Strip array / slice prefixes.
	t = strings.TrimPrefix(t, "[]")
	if strings.HasPrefix(t, "[") {
		if end := strings.Index(t, "]"); end >= 0 {
			t = t[end+1:]
		}
	}
	// Skip map/chan/func types — can't meaningfully resolve a method call
	// through them at the grain we support.
	if strings.HasPrefix(t, "map[") || strings.HasPrefix(t, "chan ") || strings.HasPrefix(t, "func(") {
		return ""
	}
	// Strip pointer prefix.
	t = strings.TrimPrefix(t, "*")
	// Keep only last segment of a package-qualified name.
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	// Skip generics.
	if i := strings.Index(t, "["); i >= 0 {
		t = t[:i]
	}
	// Skip Go primitives — a method call receiver is never a primitive in
	// code we can link to.
	switch t {
	case "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any":
		return ""
	}
	if t == "" {
		return ""
	}
	return t
}

// inferTypeFromGoExpr inspects the AST of a short-var RHS and returns
// the inferred type name — supports composite literals (`Foo{...}`),
// pointer composite literals (`&Foo{...}`), qualified composite
// literals (`pkg.Foo{...}`), and the Go constructor convention
// (`NewFoo(...)` → `Foo`). Returns "" when the expression doesn't
// unambiguously name a type.
func inferTypeFromGoExpr(node *sitter.Node, src []byte) string {
	switch node.Type() {
	case "composite_literal":
		return compositeLiteralType(node, src)
	case "unary_expression":
		// `&Foo{...}` — operator is "&" (first child), operand is
		// the composite literal.
		for i := 0; i < int(node.NamedChildCount()); i++ {
			c := node.NamedChild(i)
			if c != nil && c.Type() == "composite_literal" {
				return compositeLiteralType(c, src)
			}
		}
	case "call_expression":
		// Constructor convention: NewFoo(...) → Foo. Only applies
		// when the called identifier starts with "New".
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return ""
		}
		var callName string
		switch fn.Type() {
		case "identifier":
			callName = fn.Content(src)
		case "selector_expression":
			field := fn.ChildByFieldName("field")
			if field != nil {
				callName = field.Content(src)
			}
		}
		if strings.HasPrefix(callName, "New") && len(callName) > 3 {
			return callName[3:]
		}
	}
	return ""
}

// compositeLiteralType returns the type name of a composite literal,
// handling both `Foo{...}` (type_identifier) and `pkg.Foo{...}`
// (qualified_type) shapes.
func compositeLiteralType(lit *sitter.Node, src []byte) string {
	t := lit.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	switch t.Type() {
	case "type_identifier":
		return t.Content(src)
	case "qualified_type":
		nameNode := t.ChildByFieldName("name")
		if nameNode != nil {
			return nameNode.Content(src)
		}
	case "pointer_type":
		// *Foo{...} — rare but defensible.
		for i := 0; i < int(t.NamedChildCount()); i++ {
			c := t.NamedChild(i)
			if c != nil && c.Type() == "type_identifier" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// resolveChainType walks a dotted/chained receiver expression text like
// `svc.GetUser().Save()` and returns the inferred type of the final
// segment when each hop is typed — first segment via tenv, subsequent
// segments via a method's return_type Meta. Returns "" on the first
// unresolvable hop.
func resolveChainType(expr string, tenv typeEnv, result *parser.ExtractionResult) string {
	cleaned := stripCallArgs(expr)

	parts := strings.Split(cleaned, ".")
	if len(parts) < 2 {
		return ""
	}

	currentType, ok := tenv[parts[0]]
	if !ok {
		return ""
	}

	for i := 1; i < len(parts); i++ {
		methodName := parts[i]
		returnType := findMethodReturnType(currentType, methodName, result)
		if returnType == "" {
			return ""
		}
		currentType = returnType
	}

	return currentType
}

// stripCallArgs removes balanced parentheses (and anything inside them)
// from a receiver expression so "svc.GetUser(arg).Save()" collapses to
// "svc.GetUser.Save" for chain walking.
func stripCallArgs(expr string) string {
	var b strings.Builder
	depth := 0
	for _, ch := range expr {
		switch ch {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(ch)
			}
		}
	}
	return b.String()
}

// findMethodReturnType scans result.Nodes for a method (or package-level
// function, for pkg.Func cases) with the given name on the given
// receiver type and returns its return_type Meta. Empty string when
// not found or unannotated.
func findMethodReturnType(receiverType, methodName string, result *parser.ExtractionResult) string {
	for _, n := range result.Nodes {
		if n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
			continue
		}
		if n.Name != methodName {
			continue
		}
		if recv, ok := n.Meta["receiver"].(string); ok && recv == receiverType {
			if rt, ok := n.Meta["return_type"].(string); ok {
				return rt
			}
		}
	}
	return ""
}

// scanGoPragmas inspects up to 5 source lines immediately before a
// function or method declaration looking for `//go:*` or `//export`
// comments and stamps them onto the node's Meta. Lets callers flag
// special Go entry points (cgo exports, linkname) so dead-code
// detection doesn't mark them as dead.
func scanGoPragmas(src []byte, startLine int, node *graph.Node) {
	// startLine is 0-based here (matches tree-sitter's row numbering at
	// the call site).
	if startLine <= 0 {
		return
	}
	// Build a list of line-start byte offsets up to startLine.
	var lineStarts []int
	lineStarts = append(lineStarts, 0)
	lineNum := 1
	for i := 0; i < len(src) && lineNum <= startLine; i++ {
		if src[i] != '\n' {
			continue
		}
		if i == 0 || src[i-1] == '\n' {
			lineStarts = append(lineStarts, i)
			lineNum++
		}
		lineStarts = append(lineStarts, i+1)
		lineNum++
	}

	for scanLine := startLine - 1; scanLine >= 0 && scanLine >= startLine-5; scanLine-- {
		if scanLine >= len(lineStarts) {
			continue
		}
		start := lineStarts[scanLine]
		end := len(src)
		if scanLine+1 < len(lineStarts) {
			end = lineStarts[scanLine+1]
		}
		line := strings.TrimSpace(string(src[start:end]))
		if line != "" && !strings.HasPrefix(line, "//") {
			break
		}
		if strings.HasPrefix(line, "//export ") {
			node.Meta["cgo_export"] = true
			return
		}
		if strings.HasPrefix(line, "//go:linkname ") {
			node.Meta["go_linkname"] = true
			return
		}
	}
}

// isGoBuiltinOrKeyword returns true for identifiers that should not be
// treated as function-value references (common Go builtins, type names,
// literals).
func isGoBuiltinOrKeyword(name string) bool {
	switch name {
	case "nil", "true", "false", "err", "ok", "ctx",
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any",
		"make", "new", "len", "cap", "append", "copy", "delete",
		"panic", "recover", "print", "println", "close":
		return true
	}
	// Skip lowercase single-letter identifiers (loop vars, etc.)
	if len(name) == 1 && name[0] >= 'a' && name[0] <= 'z' {
		return true
	}
	return false
}
