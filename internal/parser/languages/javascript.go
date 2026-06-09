package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/javascript"
)

// qJSAll is a single tree-sitter query alternating over every pattern
// the JavaScript extractor needs. One tree walk per file replaces the
// 9+ `parser.RunQuery` calls (counting the per-class jsQMethod re-run).
// Capture names are disjoint across patterns so the dispatch in Extract
// can branch on which name is set. Method-to-class membership uses a
// parent walk on method_definition; the const-arrow-vs-var dedupe is
// handled by emitting arrow first and skipping the var pattern when
// the name is already owned by an arrow.
const qJSAll = `
[
  (function_declaration
    name: (identifier) @func.name) @func.def

  (lexical_declaration
    (variable_declarator
      name: (identifier) @arrow.name
      value: (arrow_function))) @arrow.def

  (class_declaration
    name: (identifier) @class.name) @class.def

  (pair
    key: (property_identifier) @objfn.name
    value: (arrow_function)) @objfn.def

  (method_definition
    name: (property_identifier) @method.name) @method.def

  (import_statement
    source: (string (string_fragment) @import.path)) @import.def

  (call_expression
    function: (identifier) @req.name
    arguments: (arguments (string (string_fragment) @req.path))) @req.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (member_expression
      object: (_) @callm.receiver
      property: (property_identifier) @callm.method)) @callm.expr

  (lexical_declaration
    (variable_declarator
      name: (identifier) @var.name)) @var.def

  (variable_declaration
    (variable_declarator
      name: (identifier) @varDecl.name)) @varDecl.def
]
`

// JavaScriptExtractor extracts JavaScript source files.
type JavaScriptExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewJavaScriptExtractor() *JavaScriptExtractor {
	lang := javascript.GetLanguage()
	return &JavaScriptExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qJSAll, lang),
	}
}

func (e *JavaScriptExtractor) Language() string { return "javascript" }
func (e *JavaScriptExtractor) Extensions() []string {
	return []string{".js", ".jsx", ".mjs", ".cjs"}
}

// --- Deferred match buffers ----------------------------------------

type jsDeferredCall struct {
	name     string
	receiver string // receiver text for member calls
	line     int
	isMember bool
	// expr is the call_expression node, kept for member calls so the
	// post-pass can inspect arguments for pub/sub topic detection.
	expr *sitter.Node
}

type jsDeferredVar struct {
	name    string
	defNode *sitter.Node
	line    int
	endLine int
}

func (e *JavaScriptExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "javascript",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	arrowNames := make(map[string]bool)

	// objLiteralMembers maps a top-level object-literal owner name to the
	// member-function node IDs declared inside it — method shorthand
	// (`{ process() {...} }`) and arrow fields (`{ health: () => ... }`).
	// A later `owner.method()` member call binds straight to the
	// registered node instead of falling through to name-only resolution,
	// which would otherwise mis-bind to an unrelated free function of the
	// same name elsewhere in the repo.
	objLiteralMembers := map[string]map[string]string{}
	registerObjMember := func(owner, member, id string) {
		if owner == "" || member == "" || id == "" {
			return
		}
		set := objLiteralMembers[owner]
		if set == nil {
			set = map[string]string{}
			objLiteralMembers[owner] = set
		}
		set[member] = id
	}

	var calls []jsDeferredCall
	var vars []jsDeferredVar
	// importPaths collects every imported / required module path so the
	// post-pass can disambiguate generic pub/sub method names (emit / on
	// / send) and infer the broker transport.
	var importPaths []string

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result)

		case m.Captures["arrow.def"] != nil:
			e.emitArrow(m, filePath, fileID, src, result, arrowNames)

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, result)

		case m.Captures["objfn.def"] != nil:
			registerObjMember(e.emitObjectArrowField(m, filePath, fileID, src, result))

		case m.Captures["method.def"] != nil:
			registerObjMember(e.emitMethod(m, filePath, fileID, src, result))

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, result)
			if p := m.Captures["import.path"]; p != nil {
				importPaths = append(importPaths, p.Text)
			}

		case m.Captures["req.def"] != nil:
			e.emitRequire(m, filePath, fileID, result)
			if m.Captures["req.name"] != nil && m.Captures["req.name"].Text == "require" {
				if p := m.Captures["req.path"]; p != nil {
					importPaths = append(importPaths, p.Text)
				}
			}

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			dc := jsDeferredCall{
				name:     m.Captures["callm.method"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
				expr:     expr.Node,
			}
			if r := m.Captures["callm.receiver"]; r != nil {
				dc.receiver = r.Text
			}
			calls = append(calls, dc)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, jsDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["var.def"] != nil:
			def := m.Captures["var.def"]
			vars = append(vars, jsDeferredVar{
				name:    m.Captures["var.name"].Text,
				defNode: def.Node,
				line:    def.StartLine + 1,
				endLine: def.EndLine + 1,
			})

		case m.Captures["varDecl.def"] != nil:
			def := m.Captures["varDecl.def"]
			vars = append(vars, jsDeferredVar{
				name:    m.Captures["varDecl.name"].Text,
				defNode: def.Node,
				line:    def.StartLine + 1,
				endLine: def.EndLine + 1,
			})
		}
	})

	// Module-level variable emission — skip names already emitted as
	// arrow functions (const-arrow-vs-var dedupe).
	for _, v := range vars {
		if arrowNames[v.name] {
			continue
		}
		parent := v.defNode.Parent()
		if parent != nil && parent.Type() == "export_statement" {
			parent = parent.Parent()
		}
		if parent == nil || parent.Type() != "program" {
			continue
		}
		id := filePath + "::" + v.name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: v.name,
			FilePath: filePath, StartLine: v.line, EndLine: v.endLine,
			Language: "javascript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: v.line,
		})
	}

	// Store-factory destructuring: `const {a,b} = useStore.getState()` binds
	// later bare `a()` / `b()` calls to the store's actions.
	destructured := map[string]string{} // local action name → store binding
	for _, dm := range jsDestructureGetStateRE.FindAllStringSubmatch(string(src), -1) {
		binding := dm[2]
		for _, nm := range strings.Split(dm[1], ",") {
			nm = strings.TrimSpace(nm)
			if i := strings.IndexByte(nm, ':'); i >= 0 { // {a: alias} → key on alias
				nm = strings.TrimSpace(nm[i+1:])
			}
			if nm != "" {
				destructured[nm] = binding
			}
		}
	}

	// Resolve calls against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if c.isMember {
			// Object-literal member call (`api.process()` where
			// `const api = { process() {...} }` is in this file): bind
			// straight to the member-function node. Resolved at
			// extraction — the resolver never sees an `unresolved::`
			// target, so a free function also named `process` in another
			// package can't capture this edge in the name-only fallback.
			if members, ok := objLiteralMembers[c.receiver]; ok {
				if memberID, ok := members[c.name]; ok {
					result.Edges = append(result.Edges, &graph.Edge{
						From: callerID, To: memberID,
						Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
						Origin: graph.OriginASTResolved, Confidence: 0.92,
					})
					continue
				}
			}
			// Store-factory chained call: `useStore.getState().action()`.
			if binding, ok := jsParseGetStateChain(c.receiver); ok {
				if memberID := objLiteralMembers[binding][c.name]; memberID != "" {
					result.Edges = append(result.Edges, &graph.Edge{
						From: callerID, To: memberID,
						Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
						Origin: graph.OriginASTResolved, Confidence: 0.9,
					})
					continue
				}
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::*." + c.name,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": c.name},
				})
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			continue
		}
		// Store-factory destructured call: `const {a}=useStore.getState(); a()`.
		if binding, ok := destructured[c.name]; ok {
			if memberID := objLiteralMembers[binding][c.name]; memberID != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: memberID,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Origin: graph.OriginASTResolved, Confidence: 0.9,
				})
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": c.name},
			})
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	// --- Event pub/sub edges ---
	var pubsubEvents []pubsubEvent
	for _, c := range calls {
		if !c.isMember || c.expr == nil {
			continue
		}
		if ev, ok := detectJSPubsubCall(c.expr, c.name, src, importPaths, c.line); ok {
			pubsubEvents = append(pubsubEvents, ev)
		}
	}
	// WebSocket / EventSource real-time client channels.
	pubsubEvents = append(pubsubEvents, detectJSRealtimeEvents(src)...)
	emitPubsubEvents(pubsubEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, "javascript", result)

	// SQL function call sites (Supabase/PostgREST .rpc('fn')).
	emitSQLCallsiteEdges(src, "javascript",
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, result)

	// --- React Native native-module bridge calls ---
	rnVars := rnNativeModuleVars(src)
	for _, c := range calls {
		if !c.isMember {
			continue
		}
		module := detectRNNativeModule(c.receiver, rnVars)
		if module == "" {
			continue
		}
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: rnNativePlaceholder(module, c.name),
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			Meta: map[string]any{"via": rnNativeVia, "rn_module": module, "rn_method": c.name},
		})
	}

	// Test-runner classification (Mocha / Bun-test / Jest / Vitest /
	// node:test / Playwright / Cypress). Stamped on the file node so
	// the indexer's test-edge pass can propagate it to every is_test
	// function/method without re-reading the file.
	if runner := DetectJSTSTestRunner(filePath, src, importPaths); runner != "" {
		if fileNode.Meta == nil {
			fileNode.Meta = map[string]any{}
		}
		fileNode.Meta["test_runner"] = runner
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *JavaScriptExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id := filePath + "::" + name
	node := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("function %s()", name)},
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if body := tsFunctionBody(def.Node); body != nil {
		StampFunctionMetrics(node, body, "javascript")
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
}

func (e *JavaScriptExtractor) emitArrow(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, arrowNames map[string]bool) {
	name := m.Captures["arrow.name"].Text
	def := m.Captures["arrow.def"]
	arrowNames[name] = true
	id := filePath + "::" + name
	node := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("const %s = () =>", name)},
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	// Walk the lexical_declaration down to the arrow_function body. The
	// query captures `arrow.def` at the lexical-declaration level
	// (because that's where the binding-name + arrow association lives)
	// so the body isn't directly captured. JSX child rendering edges
	// come from inside the arrow's body or expression.
	if arrow := jsArrowFunctionFromDef(def.Node); arrow != nil {
		body := arrow.ChildByFieldName("body")
		if body == nil {
			body = arrow
		}
		StampFunctionMetrics(node, body, "javascript")
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
}

// jsArrowFunctionFromDef descends a lexical_declaration captured at
// arrow.def and returns the arrow_function node it wraps. Returns nil
// when the structure differs (e.g. the value isn't actually an arrow).
func jsArrowFunctionFromDef(def *sitter.Node) *sitter.Node {
	if def == nil {
		return nil
	}
	for i := 0; i < int(def.NamedChildCount()); i++ {
		c := def.NamedChild(i)
		if c == nil || c.Type() != "variable_declarator" {
			continue
		}
		v := c.ChildByFieldName("value")
		if v != nil && v.Type() == "arrow_function" {
			return v
		}
	}
	return nil
}

func (e *JavaScriptExtractor) emitClass(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitMethod walks up to the enclosing class_declaration and emits the
// method with a MemberOf edge. Mirrors the legacy per-class
// extractMethods re-run of jsQMethod. Method shorthand inside an object
// literal — `const api = { process() {...} }` — also parses as a
// `method_definition` (its container is an `object`, not a
// `class_declaration`); those are routed to emitObjectLiteralMethod so
// they get a real KindFunction node instead of being silently dropped.
// The returned (owner, member, id) triple is non-empty only for an
// object-literal shorthand and lets Extract register the member so a
// later `owner.method()` call resolves to it directly.
func (e *JavaScriptExtractor) emitMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) (owner, member, id string) {
	def := m.Captures["method.def"]
	classNode := findEnclosingJSContainer(def.Node, "class_declaration")
	if classNode == nil {
		return e.emitObjectLiteralMethod(m, filePath, fileID, src, result)
	}
	nameNode := classNode.ChildByFieldName("name")
	if nameNode == nil {
		return "", "", ""
	}
	className := nameNode.Content(src)
	name := m.Captures["method.name"].Text
	classID := filePath + "::" + className
	methodID := filePath + "::" + className + "." + name
	node := &graph.Node{
		ID: methodID, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript",
	}
	if body := tsFunctionBody(def.Node); body != nil {
		StampFunctionMetrics(node, body, "javascript")
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: methodID, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
	return "", "", ""
}

// emitObjectLiteralMethod handles method shorthand inside an object
// literal — `export const api = { process() {...} }`. tree-sitter
// parses `process()` as a `method_definition` whose container is an
// `object`, so emitMethod's class walk finds nothing; without this the
// shorthand method has no graph node and a call `api.process()` either
// resolves to nothing or — worse — to an unrelated free `process`
// function elsewhere in the repo. The node is named `<owner>.<method>`,
// matching the object-arrow-field convention. Returns (owner, member,
// id) when the owner is a top-level `const owner = { ... }` binding;
// empty strings for an inline / anonymous object.
func (e *JavaScriptExtractor) emitObjectLiteralMethod(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) (owner, member, id string) {
	def := m.Captures["method.def"]
	if def.Node == nil {
		return "", "", ""
	}
	parent := def.Node.Parent()
	if parent == nil || parent.Type() != "object" {
		return "", "", ""
	}
	member = m.Captures["method.name"].Text
	if member == "" {
		return "", "", ""
	}
	owner = jsObjectOwnerName(def.Node, src)
	storeFactory := ""
	if owner == "" || jsIsStoreOptionKey(owner) {
		if b, _, ok := jsStoreFactoryBinding(def.Node, src); ok {
			owner, storeFactory = b, b
		}
	}
	name := member
	if owner != "" {
		name = owner + "." + member
	}
	id = fmt.Sprintf("%s::%s@%d", filePath, name, def.StartLine+1)
	node := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("%s()", name)},
	}
	if storeFactory != "" {
		node.Meta["store_factory"] = storeFactory
		node.Meta["store_member"] = member
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if body := tsFunctionBody(def.Node); body != nil {
		StampFunctionMetrics(node, body, "javascript")
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
	if owner == "" {
		return "", "", ""
	}
	return owner, member, id
}

// emitObjectArrowField handles the `pair → property_identifier →
// arrow_function` shape inside an object literal —
// `export const api = { health: () => ... }`. Without this, calls
// inside the arrow body have no enclosing function for findEnclosingFunc
// to attribute them to, so EdgeCalls is silently dropped; and a
// `api.health()` call can mis-bind to an unrelated free `health`.
func (e *JavaScriptExtractor) emitObjectArrowField(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) (owner, member, id string) {
	def := m.Captures["objfn.def"]
	if def.Node == nil {
		return "", "", ""
	}
	member = m.Captures["objfn.name"].Text
	if member == "" {
		return "", "", ""
	}
	owner = jsObjectOwnerName(def.Node, src)
	storeFactory := ""
	if owner == "" || jsIsStoreOptionKey(owner) {
		if b, _, ok := jsStoreFactoryBinding(def.Node, src); ok {
			owner, storeFactory = b, b
		}
	}
	name := member
	if owner != "" {
		name = owner + "." + member
	}
	id = fmt.Sprintf("%s::%s@%d", filePath, name, def.StartLine+1)
	node := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("%s: () =>", name)},
	}
	if storeFactory != "" {
		node.Meta["store_factory"] = storeFactory
		node.Meta["store_member"] = member
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if arrow := def.Node.ChildByFieldName("value"); arrow != nil {
		body := arrow.ChildByFieldName("body")
		if body == nil {
			body = arrow
		}
		StampFunctionMetrics(node, body, "javascript")
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
	if owner == "" {
		return "", "", ""
	}
	return owner, member, id
}

// jsObjectOwnerName walks up from an object-literal member node looking
// for the nearest enclosing name to qualify the member with — the
// binding name of a `const owner = { ... }` declaration or the left
// side of an `owner = { ... }` assignment, or the key of an enclosing
// `pair` for a nested object. Returns "" when no such name is reachable
// (e.g. an inline object passed as an argument) or when a function /
// class / program boundary is crossed first.
func jsObjectOwnerName(member *sitter.Node, src []byte) string {
	if member == nil {
		return ""
	}
	for cur := member.Parent(); cur != nil; cur = cur.Parent() {
		switch cur.Type() {
		case "variable_declarator":
			if name := cur.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
				return name.Content(src)
			}
			return ""
		case "assignment_expression":
			if left := cur.ChildByFieldName("left"); left != nil && left.Type() == "identifier" {
				return left.Content(src)
			}
			return ""
		case "pair":
			if k := cur.ChildByFieldName("key"); k != nil && k.Type() == "property_identifier" {
				return k.Content(src)
			}
		case "program", "class_body", "function_declaration",
			"method_definition", "arrow_function", "function_expression":
			return ""
		}
	}
	return ""
}

func (e *JavaScriptExtractor) emitImport(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	importPath := m.Captures["import.path"].Text
	line := m.Captures["import.def"].StartLine + 1
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	})
}

func (e *JavaScriptExtractor) emitRequire(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	if m.Captures["req.name"].Text != "require" {
		return
	}
	reqPath := m.Captures["req.path"].Text
	line := m.Captures["req.def"].StartLine + 1
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + reqPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: line,
	})
}

// --- Helpers --------------------------------------------------------

// findEnclosingJSContainer walks the parent chain of n looking for the
// nearest ancestor whose Type() matches t. Returns nil if none.
func findEnclosingJSContainer(n *sitter.Node, t string) *sitter.Node {
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
