package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/golang"
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

  ; Assignment LHS selector. EdgeWrites from the enclosing function
  ; to the field. Covers plain "s.field = x" and "s.field += x".
  (assignment_statement
    left: (expression_list
      (selector_expression
        operand: (_) @assign.receiver
        field: (field_identifier) @assign.field))) @assign.def

  ; Inc/dec selector statements: s.field++ / s.field-- both write.
  (inc_statement
    (selector_expression
      operand: (_) @incsel.receiver
      field: (field_identifier) @incsel.field)) @incsel.def

  (dec_statement
    (selector_expression
      operand: (_) @decsel.receiver
      field: (field_identifier) @decsel.field)) @decsel.def
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
	callName   string // plain call
	method     string // selector call method name
	receiver   string // selector call receiver text
	line       int    // 1-based line of call_expression
	isSelector bool
	spawn      bool // call is launched via `go` — emit EdgeSpawns alongside EdgeCalls
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
	// Wrap in a ref-counted handle and stamp it on the result so the
	// indexer can hand it to contract enrichers. The wrapper owns
	// the tree from here; callers Release() once they're done. We
	// don't defer Close — closing happens in the indexer after the
	// contract pass releases its ref.
	pt := parser.NewParseTree(tree, src, "go")
	result := &parser.ExtractionResult{Tree: pt}

	root := tree.RootNode()

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
	// paramsByFunc: enclosing-function ID → (param/receiver name → type).
	// Function parameters and method receivers shadow file-wide tenv at
	// call resolution time so each function's locals stay sandboxed.
	paramsByFunc := map[string]typeEnv{}
	seenTypeName := map[string]bool{} // dedup when alias + typedef match same name

	var calls []goDeferredCall
	var typeRefs []goDeferredTypeRef
	var instantiates []goDeferredTypeRef
	var valueSels []goDeferredValueSel
	var valueIdents []goDeferredValueIdent
	var fieldValSels []goDeferredValueSel
	var fieldValIdents []goDeferredValueIdent
	var observabilityEvents []goObservabilityEvent
	var flagEvents []goFlagEvent
	var configEvents []goConfigEvent
	var sqlEvents []goSQLEvent
	// writes buffers selector LHS of assignment / inc / dec
	// statements. Emitted in the post-pass once funcRanges and tenv
	// are settled so each EdgeWrites is attributed to its enclosing
	// function and (when known) carries the receiver type for the
	// resolver to land on the right field node.
	var writes []goDeferredValueSel

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		// Package: just a marker; not emitted as a graph node.
		case m.Captures["pkg.name"] != nil:
			// No-op (the package name is not currently surfaced as a node).

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, paramsByFunc)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, fileID, src, result, paramsByFunc)

		case m.Captures["typedef.def"] != nil:
			e.emitTypeDecl(m, filePath, fileID, src, result, seenTypeName)

		case m.Captures["alias.def"] != nil:
			e.emitTypeAlias(m, filePath, fileID, src, result, seenTypeName)

		case m.Captures["import.spec"] != nil:
			e.emitImport(m, filePath, fileID, result, imports, fileNode)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, goDeferredCall{
				callName: m.Captures["call.name"].Text,
				line:     expr.StartLine + 1,
				spawn:    isGoroutineSpawn(expr.Node),
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			method := m.Captures["callm.method"].Text
			calls = append(calls, goDeferredCall{
				method:     method,
				receiver:   m.Captures["callm.receiver"].Text,
				line:       expr.StartLine + 1,
				isSelector: true,
				spawn:      isGoroutineSpawn(expr.Node),
			})
			if name, ok := detectGoLogEvent(expr.Node, method, src); ok {
				observabilityEvents = append(observabilityEvents, goObservabilityEvent{
					method: method,
					name:   name,
					line:   expr.StartLine + 1,
				})
			}
			if provider, flagName, ok := detectGoFlagCheck(expr.Node, method, src); ok {
				flagEvents = append(flagEvents, goFlagEvent{
					provider: provider,
					method:   method,
					name:     flagName,
					line:     expr.StartLine + 1,
				})
			}
			if source, op, key, ok := detectGoConfigKey(expr.Node, m.Captures["callm.receiver"].Text, method, src); ok {
				configEvents = append(configEvents, goConfigEvent{
					source: source,
					op:     op,
					method: method,
					key:    key,
					line:   expr.StartLine + 1,
				})
			}
			if tables, ok := detectGoSQLCall(expr.Node, method, src); ok {
				sqlEvents = append(sqlEvents, goSQLEvent{
					method: method,
					tables: tables,
					line:   expr.StartLine + 1,
				})
			}

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

		case m.Captures["assign.def"] != nil:
			def := m.Captures["assign.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["assign.field"].Text,
				receiver: m.Captures["assign.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
			})

		case m.Captures["incsel.def"] != nil:
			def := m.Captures["incsel.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["incsel.field"].Text,
				receiver: m.Captures["incsel.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
			})

		case m.Captures["decsel.def"] != nil:
			def := m.Captures["decsel.def"]
			writes = append(writes, goDeferredValueSel{
				field:    m.Captures["decsel.field"].Text,
				receiver: m.Captures["decsel.receiver"].Text,
				line:     def.StartLine + 1,
				kind:     graph.EdgeWrites,
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

	// lookupRecvType resolves a receiver-expression name first against
	// the enclosing function's parameter scope, then the file-wide tenv.
	// Per-function scope is needed so calls like `func F(reg *Registry)
	// { reg.Register(...) }` get the right `receiver_type` even when
	// another function in the same file has a parameter or local named
	// `reg` with a different type.
	lookupRecvType := func(callerID, name string) (string, bool) {
		if callerID != "" {
			if scope, ok := paramsByFunc[callerID]; ok {
				if t, ok := scope[name]; ok {
					return t, true
				}
			}
		}
		t, ok := tenv[name]
		return t, ok
	}

	// --- Calls ---
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		var target string
		if !c.isSelector {
			target = "unresolved::" + c.callName
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: target,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			emitGoSpawnEdge(c, callerID, target, filePath, result)
			continue
		}
		if importPath, ok := imports[c.receiver]; ok {
			target = "unresolved::extern::" + importPath + "::" + c.method
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: target,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			emitGoSpawnEdge(c, callerID, target, filePath, result)
			continue
		}
		target = "unresolved::*." + c.method
		edge := &graph.Edge{
			From: callerID, To: target,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		}
		if recvType, ok := lookupRecvType(callerID, c.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		} else if strings.Contains(c.receiver, ".") || strings.Contains(c.receiver, "(") {
			// Chain walk needs both the file-wide tenv and the
			// enclosing function's parameter scope as one map. Compose
			// without mutating either.
			composed := tenv
			if scope, ok := paramsByFunc[callerID]; ok && len(scope) > 0 {
				composed = make(typeEnv, len(tenv)+len(scope))
				for k, v := range tenv {
					composed[k] = v
				}
				for k, v := range scope {
					composed[k] = v
				}
			}
			if chainType := resolveChainType(c.receiver, composed, result); chainType != "" {
				edge.Meta = map[string]any{"receiver_type": chainType}
			}
		}
		result.Edges = append(result.Edges, edge)
		emitGoSpawnEdge(c, callerID, target, filePath, result)
	}

	// --- Observability events ---
	emitGoObservabilityEvents(observabilityEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Feature flag checks ---
	emitGoFlagChecks(flagEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- Config keys (viper) ---
	emitGoConfigKeys(configEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- SQL queries ---
	// Dialect inference uses the same imports map the call walker
	// already builds. When the file imports a recognised SQL
	// driver, KindTable nodes get db::<dialect>::... IDs; otherwise
	// they fall back to db::generic::...
	emitGoSQLEvents(sqlEvents, inferGoSQLDialect(imports),
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

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
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
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
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	// Assignment / inc / dec selector LHS — EdgeWrites from the
	// enclosing function to the assigned field. Same resolution path
	// as the value-side selectors: the resolver lands on the field
	// node when we know the receiver's type, otherwise the edge stays
	// unresolved::*.field for downstream cleanup.
	for _, v := range writes {
		callerID := findEnclosingFunc(funcRanges, v.line)
		if callerID == "" {
			callerID = filePath
		}
		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + v.field,
			Kind: v.kind, FilePath: filePath, Line: v.line,
		}
		if recvType, ok := lookupRecvType(callerID, v.receiver); ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		}
		result.Edges = append(result.Edges, edge)
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *GoExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, paramsByFunc map[string]typeEnv) {
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
	if paramsCap, ok := m.Captures["func.params"]; ok && paramsCap != nil {
		if scope := extractGoParamTypes(paramsCap.Node, src); len(scope) > 0 {
			paramsByFunc[id] = scope
		}
	}
	if resultCap, ok := m.Captures["func.result"]; ok && resultCap.Text != "" {
		if rt := normalizeGoTypeName(resultCap.Text); rt != "" {
			node.Meta["return_type"] = rt
		}
	}
	if tp := goTypeParams(def.Node, src); len(tp) > 0 {
		node.Meta["type_params"] = tp
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	if body := goFuncBody(def.Node); body != nil {
		if c := GoComplexity(body); c > 1 {
			node.Meta["complexity"] = c
		}
	}
	scanGoPragmas(src, def.StartLine, node)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	emitGoThrowsEdges(node, m.Captures["func.result"], filePath, result)
	emitGoFunctionShape(id, def.Node, m.Captures["func.params"], m.Captures["func.result"],
		src, filePath, def.StartLine+1, result)
}

// goFuncBody returns the `block` body child of a function/method
// declaration node, or nil for declarations without a body (interface
// method shapes, abstract decls). Used by complexity counting.
func goFuncBody(decl *sitter.Node) *sitter.Node {
	if decl == nil {
		return nil
	}
	if b := decl.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c != nil && c.Type() == "block" {
			return c
		}
	}
	return nil
}

func (e *GoExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, paramsByFunc map[string]typeEnv) {
	name := m.Captures["method.name"].Text
	def := m.Captures["method.def"]
	receiverText := m.Captures["method.receiver"].Text
	receiverType := extractReceiverType(receiverText)

	id := filePath + "::" + receiverType + "." + name
	scope := typeEnv{}
	if recvName := extractReceiverName(receiverText); recvName != "" && receiverType != "" {
		scope[recvName] = receiverType
	}
	if paramsCap, ok := m.Captures["method.params"]; ok && paramsCap != nil {
		for k, v := range extractGoParamTypes(paramsCap.Node, src) {
			scope[k] = v
		}
	}
	if len(scope) > 0 {
		paramsByFunc[id] = scope
	}
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
	if tp := goTypeParams(def.Node, src); len(tp) > 0 {
		node.Meta["type_params"] = tp
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	if body := goFuncBody(def.Node); body != nil {
		if c := GoComplexity(body); c > 1 {
			node.Meta["complexity"] = c
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
	emitGoThrowsEdges(node, m.Captures["method.result"], filePath, result)
	emitGoFunctionShape(id, def.Node, m.Captures["method.params"], m.Captures["method.result"],
		src, filePath, def.StartLine+1, result)
}

// goTypeParams reads the `type_parameters` child of a Go declaration
// node (function_declaration, method_declaration, type_spec) and
// returns the declared type parameters as a list of {name, bound}
// maps. Multi-name parameter declarations like `[T, U comparable]`
// produce one entry per name, each with the same bound. Returns nil
// when the declaration is not generic.
func goTypeParams(decl *sitter.Node, src []byte) []map[string]string {
	if decl == nil {
		return nil
	}
	tps := decl.ChildByFieldName("type_parameters")
	if tps == nil {
		// type_spec uses a different field shape — fall back to a
		// child-type scan.
		for i := 0; i < int(decl.ChildCount()); i++ {
			c := decl.Child(i)
			if c != nil && c.Type() == "type_parameter_list" {
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
		pd := tps.NamedChild(i)
		if pd == nil {
			continue
		}
		if pd.Type() != "parameter_declaration" && pd.Type() != "type_parameter_declaration" {
			continue
		}
		// One parameter_declaration may carry multiple identifier
		// names that share a single type/bound:
		// `[T, U comparable]` → two names, one bound.
		var names []string
		var bound string
		// Names appear via ChildByFieldName("name") — Go grammar uses
		// field 'name' for the leading identifier list. For multi-name
		// declarations the grammar emits multiple entries with the
		// same field name; we walk children to find them all.
		for j := 0; j < int(pd.ChildCount()); j++ {
			c := pd.Child(j)
			if c == nil {
				continue
			}
			t := c.Type()
			if t == "identifier" || t == "field_identifier" || t == "type_identifier" {
				names = append(names, c.Content(src))
			}
		}
		if tn := pd.ChildByFieldName("type"); tn != nil {
			bound = strings.TrimSpace(tn.Content(src))
		}
		for _, n := range names {
			entry := map[string]string{"name": n}
			if bound != "" {
				entry["bound"] = bound
			}
			out = append(out, entry)
		}
	}
	return out
}

// emitGoThrowsEdges inspects the result-type capture and emits an
// EdgeThrows edge when the function returns an error. Two cases:
//
//   - last return type is the bare `error` interface → edge to the
//     synthetic external::error sentinel so reverse-walks land on
//     a single node regardless of file/repo.
//   - last return type is a custom error type (`*MyErr`, `MyErr`)
//     → edge to that type, resolved by name later.
//
// Functions that return only non-error types produce no edge.
func emitGoThrowsEdges(node *graph.Node, resultCap *parser.CapturedNode, filePath string, result *parser.ExtractionResult) {
	if resultCap == nil || resultCap.Text == "" {
		return
	}
	errType := parseLastReturnTypeForError(resultCap.Text)
	if errType == "" {
		return
	}
	target := "external::error"
	if errType != "error" {
		target = "unresolved::" + errType
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     node.ID,
		To:       target,
		Kind:     graph.EdgeThrows,
		FilePath: filePath,
		Line:     node.StartLine,
		Origin:   graph.OriginASTInferred,
	})
}

// parseLastReturnTypeForError pulls the last identifier from a Go
// result type expression and returns it when it looks like an error
// type. Recognises the bare `error` interface plus the conventional
// `*MyError` / `MyError` shapes. Returns "" for non-error returns.
func parseLastReturnTypeForError(result string) string {
	s := strings.TrimSpace(result)
	if s == "" {
		return ""
	}
	// Strip parens for tuple returns like `(int, error)`.
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = s[1 : len(s)-1]
	}
	parts := strings.Split(s, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return ""
	}
	// Strip leading `*` for pointer returns.
	last = strings.TrimPrefix(last, "*")
	// Take the rightmost identifier of `pkg.Foo`.
	if i := strings.LastIndex(last, "."); i >= 0 {
		last = last[i+1:]
	}
	// Strip generic instantiation suffix `[T]`.
	if i := strings.Index(last, "["); i >= 0 {
		last = last[:i]
	}
	last = strings.TrimSpace(last)
	if last == "error" {
		return "error"
	}
	// Heuristic: identifier ending in "Error" or "Err" — common Go
	// error type convention. Avoids false positives on things like
	// `Result` or `Response` while catching MyError, ParseErr, etc.
	if strings.HasSuffix(last, "Error") || strings.HasSuffix(last, "Err") {
		return last
	}
	return ""
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
	isStruct := false
	isInterface := false
	if body != nil && body.Node != nil && body.Node.Type() == "interface_type" {
		node.Kind = graph.KindInterface
		node.Meta = map[string]any{"methods": extractInterfaceMethods(body.Node, src)}
		isInterface = true
	} else {
		node.Kind = graph.KindType
		node.Meta = map[string]any{}
		if body != nil && body.Node != nil && body.Node.Type() == "struct_type" {
			isStruct = true
		}
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if isStruct {
		emitGoStructFields(body.Node, src, id, name, filePath, fileID, result)
	}
	if isInterface {
		for _, embed := range extractEmbeddedInterfaceTypes(body.Node, src) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     id,
				To:       "unresolved::" + embed,
				Kind:     graph.EdgeComposes,
				FilePath: filePath,
				Line:     def.StartLine + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
	// Newtype `type X Y` where Y is a named type produces an
	// EdgeExtends so blast-radius walks catch the dependency. Struct
	// and interface bodies don't get an extends edge — their member
	// fields/methods carry the reference set instead.
	if !isStruct && body != nil && body.Node != nil {
		if isExtendsNewtypeBody(body.Node.Type()) {
			if target := canonicalizeGoTypeRef(body.Text); target != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From:     id,
					To:       "unresolved::" + target,
					Kind:     graph.EdgeExtends,
					FilePath: filePath,
					Line:     def.StartLine + 1,
					Origin:   graph.OriginASTInferred,
				})
			}
		}
	}
}

// isExtendsNewtypeBody reports whether a type_spec body's AST node
// is a named-type reference that should produce an EdgeExtends edge.
// Composite anonymous bodies (struct_type / interface_type / map_type
// / func_type) are excluded — their member set carries the
// references; emitting an extends edge to "anonymous" provides no
// additional signal.
func isExtendsNewtypeBody(t string) bool {
	switch t {
	case "type_identifier", "qualified_type", "pointer_type",
		"slice_type", "array_type", "channel_type", "generic_type":
		return true
	}
	return false
}

// emitGoStructFields walks a struct_type node's field_declaration list
// and emits one KindField node per declared field. Each field gets a
// MemberOf edge to its owner type and a Defines edge from the file.
// Embedded fields (anonymous struct/interface inclusion) emit a single
// field node named after the embedded type.
func emitGoStructFields(structNode *sitter.Node, src []byte, ownerID, ownerName, filePath, fileID string, result *parser.ExtractionResult) {
	if structNode == nil {
		return
	}
	var fieldList *sitter.Node
	for i := 0; i < int(structNode.ChildCount()); i++ {
		c := structNode.Child(i)
		if c != nil && c.Type() == "field_declaration_list" {
			fieldList = c
			break
		}
	}
	if fieldList == nil {
		return
	}
	for i := 0; i < int(fieldList.NamedChildCount()); i++ {
		decl := fieldList.NamedChild(i)
		if decl == nil || decl.Type() != "field_declaration" {
			continue
		}
		// Walk the field_declaration's children once: collect
		// field_identifier names and the trailing type node. The
		// grammar exposes both via ChildByFieldName, but real-world
		// trees contain a mix of named/positional children for
		// embedded vs explicit fields, so a manual walk is the
		// reliable form.
		var nameNodes []*sitter.Node
		var typeNode *sitter.Node
		for j := 0; j < int(decl.NamedChildCount()); j++ {
			c := decl.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "field_identifier":
				nameNodes = append(nameNodes, c)
			case "type_identifier", "qualified_type", "pointer_type",
				"generic_type", "slice_type", "array_type", "map_type",
				"channel_type", "function_type", "interface_type",
				"struct_type":
				if typeNode == nil {
					typeNode = c
				}
			}
		}
		var fieldType string
		if typeNode != nil {
			// Keep the verbatim type text — primitives ("string",
			// "int", "[]byte", etc.) are valid field types and
			// agents want to see them. normalizeGoTypeName drops
			// primitives because it's tuned for receiver-type
			// resolution; field metadata has different needs.
			fieldType = strings.TrimSpace(typeNode.Content(src))
		}
		if len(nameNodes) > 0 {
			for _, nm := range nameNodes {
				emitGoFieldNode(decl, nm, nm.Content(src), fieldType, ownerID, ownerName, filePath, fileID, src, result)
			}
			continue
		}
		// Embedded field: type itself is the field name.
		if typeNode != nil {
			if fieldName := embeddedFieldName(typeNode, src); fieldName != "" {
				emitGoFieldNode(decl, typeNode, fieldName, fieldType, ownerID, ownerName, filePath, fileID, src, result)
				// Composition signal: the owning type embeds another
				// type. Distinct from EdgeExtends (newtype) and from
				// the field's MemberOf — gives blast-radius walks
				// "what types are composed into X" in one hop.
				if target := canonicalizeGoTypeRef(fieldType); target != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From:     ownerID,
						To:       "unresolved::" + target,
						Kind:     graph.EdgeComposes,
						FilePath: filePath,
						Line:     int(typeNode.StartPoint().Row) + 1,
						Origin:   graph.OriginASTInferred,
					})
				}
			}
		}
	}
}

func emitGoFieldNode(decl, anchor *sitter.Node, fieldName, fieldType, ownerID, ownerName, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	id := filePath + "::" + ownerName + "." + fieldName
	startLine := int(anchor.StartPoint().Row) + 1
	endLine := int(decl.EndPoint().Row) + 1
	meta := map[string]any{
		"receiver":   ownerName,
		"visibility": VisibilityByCase(fieldName),
	}
	if fieldType != "" {
		meta["field_type"] = fieldType
	}
	if doc := ExtractDocAbove(src, int(anchor.StartPoint().Row), DocLangSlashSlash); doc != "" {
		meta["doc"] = doc
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindField, Name: fieldName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "go", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})
}

// embeddedFieldName returns the trailing identifier of a Go embedded
// field type. Strips one leading `*` for pointer-embedded fields and
// drops the package qualifier. Returns "" when no identifier is found.
func embeddedFieldName(typeNode *sitter.Node, src []byte) string {
	if typeNode == nil {
		return ""
	}
	switch typeNode.Type() {
	case "type_identifier":
		return typeNode.Content(src)
	case "pointer_type":
		// Recurse into the pointed-to type.
		for i := 0; i < int(typeNode.NamedChildCount()); i++ {
			if n := embeddedFieldName(typeNode.NamedChild(i), src); n != "" {
				return n
			}
		}
	case "qualified_type":
		// pkg.Foo — take the trailing identifier.
		for i := int(typeNode.NamedChildCount()) - 1; i >= 0; i-- {
			c := typeNode.NamedChild(i)
			if c != nil && c.Type() == "type_identifier" {
				return c.Content(src)
			}
		}
	case "generic_type":
		// Foo[T] — name is the inner type_identifier.
		for i := 0; i < int(typeNode.NamedChildCount()); i++ {
			if n := embeddedFieldName(typeNode.NamedChild(i), src); n != "" {
				return n
			}
		}
	}
	return ""
}

func (e *GoExtractor) emitTypeAlias(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["alias.name"].Text
	if seen[name] {
		return
	}
	seen[name] = true
	def := m.Captures["alias.def"]
	id := filePath + "::" + name
	node := &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
		Meta: map[string]any{
			// Mark this node as an alias so consumers can scope queries
			// without inspecting the edge set. Newtypes get no flag —
			// the absence is the signal.
			"alias": true,
		},
	}
	if doc := ExtractDocAbove(src, def.StartLine, DocLangSlashSlash); doc != "" {
		node.Meta["doc"] = doc
	}
	node.Meta["visibility"] = VisibilityByCase(name)
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	// EdgeAliases captures the `type X = Y` relationship distinctly
	// from EdgeExtends (newtype). Renaming Y is a no-op for X's
	// callers in the alias case but breaks promoted method sets in
	// the newtype case — agents need to distinguish.
	if aliasType, ok := m.Captures["alias.type"]; ok && aliasType != nil {
		if target := canonicalizeGoTypeRef(aliasType.Text); target != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     id,
				To:       "unresolved::" + target,
				Kind:     graph.EdgeAliases,
				FilePath: filePath,
				Line:     def.StartLine + 1,
				Origin:   graph.OriginASTInferred,
			})
		}
	}
}

// emitImport records an import edge and populates the alias→path map
// used when classifying selector calls against imported packages. Blank
// and dot imports are skipped in the map (they don't introduce a
// callable identifier) but still produce EdgeImports.
//
// In addition to the existing file→import edge, emits a per-import
// node (KindImport) with Meta carrying the import path, alias (if
// renamed), and is_external flag. Lets agents query "what does this
// file import from <pkg>" with one graph hop.
func (e *GoExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, imports map[string]string, fileNode *graph.Node) {
	pathCap := m.Captures["import.path"]
	importPath := strings.Trim(pathCap.Text, `"`)
	line := pathCap.StartLine + 1

	// Mark the file as a cgo user when it imports "C" — the
	// pseudo-import that triggers Go's cgo preprocessor and
	// signals the file has a C source dependency. Agents asking
	// "which files would break if we removed cgo" or "what's the
	// porting surface to a non-cgo build" can then filter by
	// this meta flag without parsing source.
	if importPath == "C" && fileNode != nil {
		if fileNode.Meta == nil {
			fileNode.Meta = map[string]any{}
		}
		fileNode.Meta["uses_cgo"] = true
	}

	rawAlias := ""
	if a, ok := m.Captures["import.alias"]; ok {
		rawAlias = strings.TrimSpace(a.Text)
	}
	displayName := rawAlias
	mapAlias := rawAlias
	switch rawAlias {
	case "_", ".":
		// Blank and dot imports keep their special behaviour for
		// the alias map (no callable identifier introduced) but the
		// import node still gets emitted so reverse-walks work.
		displayName = importPath
		if i := strings.LastIndex(importPath, "/"); i >= 0 {
			displayName = importPath[i+1:]
		}
	case "":
		displayName = importPath
		if i := strings.LastIndex(importPath, "/"); i >= 0 {
			displayName = importPath[i+1:]
		}
		mapAlias = displayName
	}

	importNodeID := filePath + "::import::" + importPath
	importMeta := map[string]any{
		"path":        importPath,
		"is_external": isExternalGoImport(importPath),
	}
	if rawAlias != "" {
		importMeta["alias"] = rawAlias
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID:        importNodeID,
		Kind:      graph.KindImport,
		Name:      displayName,
		FilePath:  filePath,
		StartLine: line,
		EndLine:   line,
		Language:  "go",
		Meta:      importMeta,
	})
	// File → import-node edge (Defines), so get_file_summary picks
	// it up under the file's children.
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       importNodeID,
		Kind:     graph.EdgeDefines,
		FilePath: filePath,
		Line:     line,
	})
	// Existing file → unresolved import-path edge for resolver
	// behaviour (downstream code resolves the path to the imported
	// repo's file node). Kept additive so consumers that read
	// EdgeImports keep working unchanged.
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + importPath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     line,
	})

	if rawAlias == "_" || rawAlias == "." {
		return
	}
	imports[mapAlias] = importPath
}

// isExternalGoImport returns true when the import path doesn't look
// like a stdlib import. Heuristic: the first path segment contains a
// dot — i.e. it's a module path like `github.com/...` or
// `golang.org/...`. Stdlib paths (`fmt`, `os/exec`) have no dot in
// the first segment.
func isExternalGoImport(path string) bool {
	if path == "" {
		return false
	}
	first := path
	if i := strings.Index(path, "/"); i >= 0 {
		first = path[:i]
	}
	return strings.Contains(first, ".")
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
		Meta:     map[string]any{"visibility": VisibilityByCase(name)},
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
	// Detect Go's enum-by-convention: a const_declaration block that
	// contains the `iota` identifier anywhere within its body marks
	// every spec in that block as an enum member. The heuristic
	// matches both the explicit form (`Red Color = iota`) and the
	// implicit-continuation form (`Red Color = iota; Green; Blue`),
	// which share the same enclosing block. False positives are
	// possible if the user happens to write `iota` inside a const
	// block for an unrelated reason — vanishingly rare in practice.
	kind := graph.KindConstant
	if def != nil && def.Node != nil && containsGoIotaBlock(def.Text) {
		kind = graph.KindEnumMember
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "go",
		Meta:     map[string]any{"visibility": VisibilityByCase(name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// containsGoIotaBlock reports whether a const_declaration's source
// text contains the bare `iota` identifier. Word-boundary anchoring
// keeps `bigiotabig` and similar identifier substrings from
// triggering. Simpler than walking the AST for a one-off detection.
func containsGoIotaBlock(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i+4 <= len(text); i++ {
		if text[i:i+4] != "iota" {
			continue
		}
		before := byte(' ')
		if i > 0 {
			before = text[i-1]
		}
		after := byte(' ')
		if i+4 < len(text) {
			after = text[i+4]
		}
		if !isGoIdentByte(before) && !isGoIdentByte(after) {
			return true
		}
	}
	return false
}

func isGoIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
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

// extractReceiverName extracts the receiver name from a Go receiver
// parameter list. "(s *Server)" -> "s", "(Server)" -> "" (unnamed).
// Used to seed paramsByFunc so calls like `s.foo()` inside the method
// body resolve via the receiver's type.
func extractReceiverName(receiver string) string {
	receiver = strings.Trim(receiver, "()")
	parts := strings.Fields(receiver)
	if len(parts) < 2 {
		return ""
	}
	name := parts[0]
	if name == "" || name == "_" {
		return ""
	}
	return name
}

// extractGoParamTypes walks a parameter_list AST node and returns a
// map from parameter name to its declared (normalized) type. Multi-name
// declarations like `(a, b *Foo)` produce one entry per name. Unnamed
// parameters, blank identifiers, and types that normalizeGoTypeName
// drops (primitives, map/chan/func) are skipped — callers only care
// about names that point at receiver types we could resolve methods on.
func extractGoParamTypes(paramList *sitter.Node, src []byte) typeEnv {
	if paramList == nil {
		return nil
	}
	out := typeEnv{}
	for i := 0; i < int(paramList.NamedChildCount()); i++ {
		decl := paramList.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		if t != "parameter_declaration" && t != "variadic_parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		typeName := normalizeGoTypeName(typeNode.Content(src))
		if typeName == "" {
			continue
		}
		// Walk all identifier children — Go allows multiple names per
		// parameter declaration sharing one type. Skip the type node
		// itself (which may also be reported as a named child).
		for j := 0; j < int(decl.NamedChildCount()); j++ {
			c := decl.NamedChild(j)
			if c == nil || c == typeNode {
				continue
			}
			if c.Type() == "identifier" {
				name := c.Content(src)
				if name == "" || name == "_" {
					continue
				}
				out[name] = typeName
			}
		}
	}
	return out
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

// extractEmbeddedInterfaceTypes walks the children of an
// interface_type node and returns the canonicalised names of
// embedded interface references. The Go grammar wraps embedded
// interfaces and type-set elements inside `type_elem` nodes; older
// grammar versions (and gofmt edge cases) sometimes inline the bare
// type_identifier / qualified_type directly. Both shapes are
// accepted so EdgeComposes is emitted regardless of which the
// grammar produces.
func extractEmbeddedInterfaceTypes(ifaceNode *sitter.Node, src []byte) []string {
	var out []string
	visit := func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "type_identifier", "qualified_type", "generic_type":
			if t := canonicalizeGoTypeRef(n.Content(src)); t != "" {
				out = append(out, t)
			}
		}
	}
	for i := 0; i < int(ifaceNode.NamedChildCount()); i++ {
		child := ifaceNode.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "type_elem":
			// Modern grammar: each embedded type is one type_elem
			// child. The named child of type_elem is the type
			// reference itself.
			for j := 0; j < int(child.NamedChildCount()); j++ {
				visit(child.NamedChild(j))
			}
		case "type_identifier", "qualified_type", "generic_type":
			visit(child)
		}
	}
	return out
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
