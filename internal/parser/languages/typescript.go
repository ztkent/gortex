package languages

import (
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/typescript"
)

// tsQAll is one tree-sitter query with 13 alternation patterns covering
// everything the TypeScript extractor needs at the root of a file. One
// query = one tree walk per Extract call, down from 14 walks in the
// per-query-per-call design. Each pattern uses a disjoint set of
// capture names so the dispatch below can branch on which name is set.
//
// method_definition and public_field_definition are captured at file
// scope here (hoisted out of per-class subqueries); the dispatcher
// walks up from the matched node to find the enclosing class.
const tsQAll = `
[
  (function_declaration
    name: (identifier) @func.name) @func.def

  (lexical_declaration
    (variable_declarator
      name: (identifier) @arrow.name
      value: (arrow_function) @arrow.body)) @arrow.def

  (class_declaration
    name: (type_identifier) @class.name) @class.def

  (interface_declaration
    name: (type_identifier) @iface.name) @iface.def

  (type_alias_declaration
    name: (type_identifier) @type.name) @type.def

  (enum_declaration
    name: (identifier) @enum.name) @enum.def

  (import_statement
    source: (string) @import.path) @import.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (call_expression
    function: (member_expression
      object: (_) @callm.receiver
      property: (property_identifier) @callm.method)) @callm.expr

  (lexical_declaration
    (variable_declarator
      name: (identifier) @tvar.name
      type: (type_annotation (_) @tvar.type))) @tvar.def

  (lexical_declaration
    (variable_declarator
      name: (identifier) @var.name)) @var.def

  (method_definition
    name: (property_identifier) @method.name) @method.def

  (public_field_definition
    name: (property_identifier) @prop.name) @prop.def
]
`

// TypeScriptExtractor extracts TypeScript/JavaScript source files.
type TypeScriptExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewTypeScriptExtractor() *TypeScriptExtractor {
	lang := typescript.GetLanguage()
	return &TypeScriptExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(tsQAll, lang),
	}
}

func (e *TypeScriptExtractor) Language() string     { return "typescript" }
func (e *TypeScriptExtractor) Extensions() []string { return []string{".ts", ".tsx"} }

// deferredCall holds a call_expression match whose edge can only be
// emitted after every function/method node exists (so funcRanges can
// attribute the call to its enclosing definition) and after the type
// env is fully populated.
type deferredCall struct {
	name     string // identifier callee name (or "" for member calls)
	method   string // method name for member calls
	receiver string // receiver text for member calls
	line     int    // 1-based line of the call_expression
	isMember bool
}

// deferredVar holds a lexical_declaration match whose emission is
// delayed until arrow-function names for the whole file are known
// (otherwise a `const foo = () => {}` would emit both a function and a
// variable node for foo).
type deferredVar struct {
	name    string
	defNode *sitter.Node
	startLn int // 0-based
	endLn   int // 0-based
}

func (e *TypeScriptExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "typescript",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	imports := map[string]string{}
	arrowNames := map[string]bool{}
	tenv := make(typeEnv)

	var calls []deferredCall
	var vars []deferredVar
	// classCarries accumulates (class_declaration node, classID) pairs
	// as they're matched, so the post-pass can run a single
	// walkClassMembers per class — covering @Inject consumer edges,
	// NestJS dynamic-module factory providers, and `this.<field>`
	// tenv seeding all in one walk. Replaces three separate walkNodes
	// passes (emitInjectConsumers, emitDynamicModuleBindings,
	// collectThisParamTypesInClass).
	var classCarries []classCarry

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {
		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, result)

		case m.Captures["arrow.def"] != nil:
			if name := e.emitArrow(m, filePath, fileID, result); name != "" {
				arrowNames[name] = true
			}

		case m.Captures["class.def"] != nil:
			classID := e.emitClass(m, filePath, fileID, src, result)
			if def := m.Captures["class.def"]; def.Node != nil && classID != "" {
				classCarries = append(classCarries, classCarry{node: def.Node, id: classID})
			}

		case m.Captures["iface.def"] != nil:
			e.emitInterface(m, filePath, fileID, src, result)

		case m.Captures["type.def"] != nil:
			e.emitTypeAlias(m, filePath, fileID, result)

		case m.Captures["enum.def"] != nil:
			e.emitEnum(m, filePath, fileID, src, result)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, src, result, imports)

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, src, result)

		case m.Captures["prop.def"] != nil:
			e.emitClassProperty(m, filePath, src, result)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, deferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["callm.expr"] != nil:
			expr := m.Captures["callm.expr"]
			calls = append(calls, deferredCall{
				method:   m.Captures["callm.method"].Text,
				receiver: m.Captures["callm.receiver"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
			})

		case m.Captures["tvar.def"] != nil:
			name := m.Captures["tvar.name"].Text
			if tn := normalizeTypeName(m.Captures["tvar.type"].Text); tn != "" {
				tenv[name] = tn
			}

		case m.Captures["var.def"] != nil:
			def := m.Captures["var.def"]
			vars = append(vars, deferredVar{
				name:    m.Captures["var.name"].Text,
				defNode: def.Node,
				startLn: def.StartLine,
				endLn:   def.EndLine,
			})
		}
	})

	// Tier 0b: single per-class walk dispatching three concerns at
	// once — @Inject consumer edges, NestJS dynamic-module factory
	// providers (forRoot / forFeature / register / *Async), and
	// `this.<field>` tenv seeding from constructor parameter-property
	// shorthand or class field annotations / inject() initializers.
	// Folding the three previous walks into one cuts per-class
	// walkNodes cost by ~3× on NestJS-style class-heavy files.
	for _, cc := range classCarries {
		walkClassMembers(cc.node, src, cc.id, filePath, result, tenv)
	}

	// Tier 1: type inference from `new Expr()` initializers in plain
	// variable declarations. Runs against the already-buffered var
	// matches so we don't re-query the tree.
	for _, v := range vars {
		if _, seen := tenv[v.name]; seen {
			continue
		}
		if v.defNode == nil {
			continue
		}
		walkNodes(v.defNode, func(n *sitter.Node) {
			if n.Type() != "variable_declarator" {
				return
			}
			for i := 0; i < int(n.NamedChildCount()); i++ {
				child := n.NamedChild(i)
				if child.Type() == "new_expression" {
					if tn := inferTypeFromNewExpr(child, src); tn != "" {
						tenv[v.name] = tn
					}
					return
				}
			}
		})
	}

	// Now every function/method node is in result; build the line
	// range map used to attribute calls to their caller.
	funcRanges := buildFuncRanges(result)

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if !c.isMember {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
			})
			continue
		}
		// Namespace/default import receiver (e.g. `fs.readFile`): attach
		// the module path so the resolver can classify externally.
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

	for _, v := range vars {
		if arrowNames[v.name] || v.defNode == nil {
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
			FilePath: filePath, StartLine: v.startLn + 1, EndLine: v.endLn + 1,
			Language: "typescript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: v.startLn + 1,
		})
	}

	return result, nil
}

// --- per-match emit helpers ------------------------------------------

func (e *TypeScriptExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"signature": fmt.Sprintf("function %s()", name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *TypeScriptExtractor) emitArrow(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) string {
	name := m.Captures["arrow.name"].Text
	def := m.Captures["arrow.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"signature": fmt.Sprintf("const %s = () =>", name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	return name
}

// emitClass writes the class node + Defines edge and runs the
// shallow @Module(...) decorator scan. The deeper per-class walk
// (constructor / fields / static factories) is deferred to the
// post-pass walkClassMembers loop so all three concerns can share
// one walkNodes traversal.
func (e *TypeScriptExtractor) emitClass(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) string {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	if def.Node != nil {
		// @Module(...) lives on the class's direct decorator children
		// (a shallow scan, not a walkNodes), so handling it inline
		// keeps the @Module Provides edges grouped with the class node
		// they originate from. @Inject consumer edges and dynamic-
		// module factory providers are dispatched from walkClassMembers
		// in the post-pass.
		emitModuleBindings(def.Node, src, id, filePath, result)
	}
	return id
}

func (e *TypeScriptExtractor) emitInterface(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["iface.name"].Text
	def := m.Captures["iface.def"]
	id := filePath + "::" + name
	var methods []string
	if def.Node != nil {
		methods = extractTSInterfaceMethods(def.Node, src)
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"methods": methods},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *TypeScriptExtractor) emitTypeAlias(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	name := m.Captures["type.name"].Text
	def := m.Captures["type.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *TypeScriptExtractor) emitEnum(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["enum.name"].Text
	def := m.Captures["enum.def"]
	id := filePath + "::" + name

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"kind": "enum"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: def.StartLine + 1,
	})

	if def.Node == nil {
		return
	}
	// Walk the enum body for member names. The grammar yields enum_body
	// → (property_identifier | enum_assignment) children; handle both.
	for i := 0; i < int(def.Node.ChildCount()); i++ {
		child := def.Node.Child(i)
		if child == nil || child.Type() != "enum_body" {
			continue
		}
		for j := 0; j < int(child.ChildCount()); j++ {
			mem := child.Child(j)
			if mem == nil {
				continue
			}
			var memberName string
			var memberNode *sitter.Node
			switch mem.Type() {
			case "property_identifier":
				memberName = mem.Content(src)
				memberNode = mem
			case "enum_assignment":
				nameNode := mem.ChildByFieldName("name")
				if nameNode != nil {
					memberName = nameNode.Content(src)
					memberNode = mem
				}
			}
			if memberName == "" || memberNode == nil {
				continue
			}
			memberID := id + "." + memberName
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: memberID, Kind: graph.KindVariable, Name: memberName,
				FilePath:  filePath,
				StartLine: int(memberNode.StartPoint().Row) + 1,
				EndLine:   int(memberNode.EndPoint().Row) + 1,
				Language:  "typescript",
				Meta:      map[string]any{"receiver": name, "kind": "enum_member"},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: memberID, To: id, Kind: graph.EdgeMemberOf,
				FilePath: filePath, Line: int(memberNode.StartPoint().Row) + 1,
			})
		}
	}
}

// emitImport records the import edge and, for default/namespace imports,
// populates the alias→path map used when classifying member-call
// receivers against imported modules.
//
// Named imports are intentionally skipped — `a(x)` is already a plain
// call matched by the call-expression pattern and doesn't go through
// the selector-call path.
func (e *TypeScriptExtractor) emitImport(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, imports map[string]string) {
	path := m.Captures["import.path"]
	importPath := strings.Trim(path.Text, `"'`)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
	})
	defCap, ok := m.Captures["import.def"]
	if !ok || defCap.Node == nil {
		return
	}
	for i := 0; i < int(defCap.Node.NamedChildCount()); i++ {
		child := defCap.Node.NamedChild(i)
		if child.Type() != "import_clause" {
			continue
		}
		for j := 0; j < int(child.NamedChildCount()); j++ {
			c := child.NamedChild(j)
			switch c.Type() {
			case "identifier": // default: `import Foo from ...`
				imports[c.Content(src)] = importPath
			case "namespace_import": // `import * as Foo from ...`
				for k := 0; k < int(c.NamedChildCount()); k++ {
					if id := c.NamedChild(k); id.Type() == "identifier" {
						imports[id.Content(src)] = importPath
					}
				}
			}
		}
	}
}

// emitMethod is called once per method_definition captured at root
// scope. The enclosing class is found by walking up the parent chain
// to the nearest class_declaration; methods that don't live inside a
// class (defensively: object literal method shorthand would parse as
// a `method_definition` in some grammar variants, but tree-sitter's
// TS grammar classifies those as `pair` — in practice this branch
// skips nothing that the legacy per-class scan caught).
func (e *TypeScriptExtractor) emitMethod(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["method.def"]
	if def.Node == nil {
		return
	}
	classNode := findEnclosingClass(def.Node)
	if classNode == nil {
		return
	}
	classNameNode := classNode.ChildByFieldName("name")
	if classNameNode == nil {
		return
	}
	className := classNameNode.Content(src)
	classID := filePath + "::" + className
	name := m.Captures["method.name"].Text
	id := classID + "." + name
	node := &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"receiver": className},
	}
	if rt := extractTSMethodReturnType(def.Node, src); rt != "" {
		node.Meta["return_type"] = rt
	}
	result.Nodes = append(result.Nodes, node)
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
	// NestJS-style dispatch decorators (@UseGuards/@UseInterceptors/...)
	// are SIBLINGS of method_definition inside class_body — walk backward.
	for sib := def.Node.PrevSibling(); sib != nil && sib.Type() == "decorator"; sib = sib.PrevSibling() {
		emitDispatchFromDecorator(sib, src, id, filePath, result)
	}
}

func (e *TypeScriptExtractor) emitClassProperty(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["prop.def"]
	if def.Node == nil {
		return
	}
	classNode := findEnclosingClass(def.Node)
	if classNode == nil {
		return
	}
	classNameNode := classNode.ChildByFieldName("name")
	if classNameNode == nil {
		return
	}
	className := classNameNode.Content(src)
	classID := filePath + "::" + className
	name := m.Captures["prop.name"].Text
	id := classID + "." + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "typescript",
		Meta:     map[string]any{"receiver": className, "kind": "class_property"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf,
		FilePath: filePath, Line: def.StartLine + 1,
	})
}

// findEnclosingClass walks up the parent chain looking for the nearest
// class_declaration ancestor. Returns nil when the node is not inside
// a class.
func findEnclosingClass(n *sitter.Node) *sitter.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == "class_declaration" {
			return p
		}
	}
	return nil
}

// --- helpers (unchanged) ---------------------------------------------

// extractTSInterfaceMethods walks children of an interface_declaration
// node to find method_signature and property_signature entries and
// returns their names.
func extractTSInterfaceMethods(ifaceNode *sitter.Node, src []byte) []string {
	var methods []string
	var body *sitter.Node
	for i := 0; i < int(ifaceNode.NamedChildCount()); i++ {
		child := ifaceNode.NamedChild(i)
		if child.Type() == "interface_body" || child.Type() == "object_type" {
			body = child
			break
		}
	}
	if body == nil {
		return methods
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		switch child.Type() {
		case "method_signature", "property_signature":
			for j := 0; j < int(child.NamedChildCount()); j++ {
				nameNode := child.NamedChild(j)
				if nameNode.Type() == "property_identifier" {
					methods = append(methods, nameNode.Content(src))
					break
				}
			}
		}
	}
	return methods
}

// emitModuleBindings walks a class_declaration's decorators, finds the
// @Module(...) decorator (if any), and emits one EdgeProvides edge per
// `{ provide: X, useClass: Y }` entry in its `providers` array. The
// edge points from the module class to the concrete implementation Y;
// Meta["provides_for"] names the abstract type X so the resolver can
// prefer Y when a call's receiver_type is X. Non-useClass providers
// (useValue / useFactory / useExisting / bare class references) are
// skipped here — they'll be handled by the @Inject(TOKEN) feature.
func emitModuleBindings(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult) {
	decorators := classDecorators(classNode)
	for _, dec := range decorators {
		call := nestDecoratorCall(dec)
		if call == nil {
			continue
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "Module" {
			continue
		}
		args := call.ChildByFieldName("arguments")
		if args == nil {
			continue
		}
		var config *sitter.Node
		for i := 0; i < int(args.NamedChildCount()); i++ {
			c := args.NamedChild(i)
			if c != nil && c.Type() == "object" {
				config = c
				break
			}
		}
		if config == nil {
			continue
		}
		emitProvidersFromObject(config, src, classID, filePath, result, "@Module")
	}
	// Dynamic-module factory bindings (forRoot/forFeature/register/*Async)
	// are emitted by walkClassMembers in the post-pass — same walkNodes
	// pass that handles @Inject consumers and `this.<field>` tenv
	// seeding.
}

func emitProvidersFromObject(config *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, originTag string) {
	providersNode := objectFieldValue(config, src, "providers")
	if providersNode == nil || providersNode.Type() != "array" {
		return
	}
	for i := 0; i < int(providersNode.NamedChildCount()); i++ {
		entry := providersNode.NamedChild(i)
		if entry == nil || entry.Type() != "object" {
			continue
		}
		abstract := objectFieldToken(entry, src, "provide")
		if abstract == "" {
			continue
		}
		if concrete := objectFieldIdentifier(entry, src, "useClass"); concrete != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     classID,
				To:       "unresolved::" + concrete,
				Kind:     graph.EdgeProvides,
				FilePath: filePath,
				Line:     int(entry.StartPoint().Row) + 1,
				Meta: map[string]any{
					"provides_for": abstract,
					"binding":      "useClass",
					"origin":       originTag,
				},
			})
			continue
		}
		for _, variant := range []string{"useValue", "useFactory", "useExisting"} {
			if objectFieldValue(entry, src, variant) == nil {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     classID,
				To:       "unresolved::" + abstract,
				Kind:     graph.EdgeProvides,
				FilePath: filePath,
				Line:     int(entry.StartPoint().Row) + 1,
				Meta: map[string]any{
					"di_token": abstract,
					"binding":  variant,
					"origin":   originTag,
				},
			})
			break
		}
	}
}

var dynamicModuleMethods = map[string]struct{}{
	"forRoot":         {},
	"forRootAsync":    {},
	"forFeature":      {},
	"forFeatureAsync": {},
	"register":        {},
	"registerAsync":   {},
}

// classCarry pairs a class_declaration node with the classID
// derived from the main-pass match, so the post-pass can dispatch
// per-member work without re-deriving the ID. See walkClassMembers.
type classCarry struct {
	node *sitter.Node
	id   string
}

// walkClassMembers walks a class subtree exactly once and dispatches
// every per-member concern in one pass:
//
//   - constructor parameters → @Inject(TOKEN) decorators emit
//     EdgeConsumes; TS parameter-property shorthand seeds
//     tenv["this.<name>"] from the explicit type annotation.
//   - public_field_definition → field-level @Inject decorators
//     emit EdgeConsumes; field type annotation OR `inject(...)`
//     initializer seeds tenv["this.<name>"].
//   - static factory methods named forRoot / forFeature /
//     register / *Async (NestJS DynamicModule pattern) → emit
//     EdgeProvides for each `{ provide, useClass }` entry in the
//     returned object.
//
// Replaces three previous walkNodes passes (emitInjectConsumers,
// emitDynamicModuleBindings, collectThisParamTypesInClass).
func walkClassMembers(classNode *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv) {
	seenInject := make(map[string]struct{})
	walkNodes(classNode, func(n *sitter.Node) {
		switch n.Type() {
		case "method_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			methodName := nameNode.Content(src)
			if methodName == "constructor" {
				dispatchConstructorMembers(n, src, classID, filePath, result, tenv, seenInject)
				return
			}
			// NestJS DynamicModule factory: only static methods named
			// from the dynamicModuleMethods set qualify.
			if _, ok := dynamicModuleMethods[methodName]; !ok {
				return
			}
			if !methodIsStatic(n) {
				return
			}
			body := n.ChildByFieldName("body")
			if body == nil {
				return
			}
			cfg := findReturnedConfigObject(body)
			if cfg == nil {
				return
			}
			emitProvidersFromObject(cfg, src, classID, filePath, result, methodName)

		case "public_field_definition":
			dispatchClassField(n, src, classID, filePath, result, tenv, seenInject)
		}
	})
}

// dispatchConstructorMembers handles both @Inject(TOKEN) decorators
// on constructor parameters AND tenv seeding from TS parameter-
// property shorthand (`constructor(private readonly foo: Bar) {}`).
func dispatchConstructorMembers(method *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv, seenInject map[string]struct{}) {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return
	}
	for i := 0; i < int(params.NamedChildCount()); i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		// @Inject decorator on the parameter → consumer edge.
		for j := 0; j < int(p.ChildCount()); j++ {
			c := p.Child(j)
			if c != nil && c.Type() == "decorator" {
				emitInjectFromDecorator(c, src, classID, filePath, result, seenInject)
			}
		}
		// Parameter-property shorthand → tenv["this.<name>"].
		if p.Type() != "required_parameter" && p.Type() != "optional_parameter" {
			continue
		}
		if !hasParameterPropertyModifier(p) {
			continue
		}
		paramName := paramIdentifier(p, src)
		if paramName == "" {
			continue
		}
		typeName := paramTypeAnnotation(p, src)
		if typeName == "" {
			continue
		}
		tenv["this."+paramName] = typeName
	}
}

// dispatchClassField handles both @Inject decorators and tenv
// seeding for a public_field_definition. tenv prefers an explicit
// type annotation; falls back to the type inferred from an
// `inject(Foo)` initializer.
func dispatchClassField(field *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, tenv typeEnv, seenInject map[string]struct{}) {
	for i := 0; i < int(field.ChildCount()); i++ {
		c := field.Child(i)
		if c != nil && c.Type() == "decorator" {
			emitInjectFromDecorator(c, src, classID, filePath, result, seenInject)
		}
	}
	name := classFieldName(field, src)
	if name == "" {
		return
	}
	if t := classFieldTypeAnnotation(field, src); t != "" {
		tenv["this."+name] = t
		return
	}
	if t := injectInitializerType(field, src); t != "" {
		tenv["this."+name] = t
	}
}

// emitInjectFromDecorator emits one EdgeConsumes for an @Inject(TOKEN)
// decorator, deduping against seenInject so multiple @Inject sites
// for the same token (constructor + field) don't double-emit.
func emitInjectFromDecorator(dec *sitter.Node, src []byte, classID, filePath string, result *parser.ExtractionResult, seenInject map[string]struct{}) {
	tok := injectDecoratorArg(dec, src)
	if tok == "" {
		return
	}
	if _, dup := seenInject[tok]; dup {
		return
	}
	seenInject[tok] = struct{}{}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     classID,
		To:       "unresolved::" + tok,
		Kind:     graph.EdgeConsumes,
		FilePath: filePath,
		Line:     int(dec.StartPoint().Row) + 1,
		Meta: map[string]any{
			"di_token": tok,
			"via":      "@Inject",
		},
	})
}

// findReturnedConfigObject finds the first object literal returned
// from a static factory method's body (NestJS DynamicModule shape).
// Inner walkNodes is bounded by method body, which is small.
func findReturnedConfigObject(body *sitter.Node) *sitter.Node {
	var cfg *sitter.Node
	walkNodes(body, func(m *sitter.Node) {
		if cfg != nil || m.Type() != "return_statement" {
			return
		}
		for i := 0; i < int(m.NamedChildCount()); i++ {
			c := m.NamedChild(i)
			if c != nil && c.Type() == "object" {
				cfg = c
				return
			}
		}
	})
	return cfg
}

func methodIsStatic(m *sitter.Node) bool {
	for i := 0; i < int(m.ChildCount()); i++ {
		c := m.Child(i)
		if c != nil && c.Type() == "static" {
			return true
		}
	}
	return false
}

func classDecorators(classNode *sitter.Node) []*sitter.Node {
	var decs []*sitter.Node
	for i := 0; i < int(classNode.ChildCount()); i++ {
		c := classNode.Child(i)
		if c != nil && c.Type() == "decorator" {
			decs = append(decs, c)
		}
	}
	parent := classNode.Parent()
	if parent != nil && parent.Type() == "export_statement" {
		for i := 0; i < int(parent.ChildCount()); i++ {
			c := parent.Child(i)
			if c != nil && c.Type() == "decorator" {
				decs = append(decs, c)
			}
		}
	}
	return decs
}

func objectFieldValue(objNode *sitter.Node, src []byte, name string) *sitter.Node {
	for i := 0; i < int(objNode.NamedChildCount()); i++ {
		p := objNode.NamedChild(i)
		if p == nil || p.Type() != "pair" {
			continue
		}
		key := p.ChildByFieldName("key")
		if key == nil {
			continue
		}
		if key.Content(src) != name {
			continue
		}
		return p.ChildByFieldName("value")
	}
	return nil
}

func objectFieldIdentifier(objNode *sitter.Node, src []byte, name string) string {
	v := objectFieldValue(objNode, src, name)
	if v == nil || v.Type() != "identifier" {
		return ""
	}
	return v.Content(src)
}

func objectFieldToken(objNode *sitter.Node, src []byte, name string) string {
	v := objectFieldValue(objNode, src, name)
	if v == nil {
		return ""
	}
	switch v.Type() {
	case "identifier":
		return v.Content(src)
	case "string":
		s := v.Content(src)
		if len(s) >= 2 {
			return s[1 : len(s)-1]
		}
	}
	return ""
}

func injectDecoratorArg(dec *sitter.Node, src []byte) string {
	call := nestDecoratorCall(dec)
	if call == nil {
		return ""
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "Inject" {
		return ""
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		switch arg.Type() {
		case "identifier":
			return arg.Content(src)
		case "string":
			s := arg.Content(src)
			if len(s) >= 2 {
				return s[1 : len(s)-1]
			}
		}
	}
	return ""
}

var nestDispatchDecorators = map[string]string{
	"UseGuards":       "canActivate",
	"UseInterceptors": "intercept",
	"UseFilters":      "catch",
	"UsePipes":        "transform",
}

func emitDispatchFromDecorator(dec *sitter.Node, src []byte, methodID, filePath string, result *parser.ExtractionResult) {
	callNode := nestDecoratorCall(dec)
	if callNode == nil {
		return
	}
	fn := callNode.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" {
		return
	}
	entryMethod, ok := nestDispatchDecorators[fn.Content(src)]
	if !ok {
		return
	}
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	for j := 0; j < int(args.NamedChildCount()); j++ {
		arg := args.NamedChild(j)
		if arg == nil || arg.Type() != "identifier" {
			continue
		}
		argClass := arg.Content(src)
		result.Edges = append(result.Edges, &graph.Edge{
			From:     methodID,
			To:       "unresolved::*." + entryMethod,
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     int(dec.StartPoint().Row) + 1,
			Meta: map[string]any{
				"receiver_type":      argClass,
				"dispatch_decorator": fn.Content(src),
			},
		})
	}
}

func nestDecoratorCall(dec *sitter.Node) *sitter.Node {
	for i := 0; i < int(dec.NamedChildCount()); i++ {
		c := dec.NamedChild(i)
		if c != nil && c.Type() == "call_expression" {
			return c
		}
	}
	return nil
}

func classFieldName(field *sitter.Node, src []byte) string {
	nameNode := field.ChildByFieldName("name")
	if nameNode == nil || nameNode.Type() != "property_identifier" {
		return ""
	}
	return nameNode.Content(src)
}

func classFieldTypeAnnotation(field *sitter.Node, src []byte) string {
	for i := 0; i < int(field.NamedChildCount()); i++ {
		c := field.NamedChild(i)
		if c == nil || c.Type() != "type_annotation" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			tn := c.NamedChild(j)
			if tn == nil {
				continue
			}
			return normalizeTypeName(tn.Content(src))
		}
	}
	return ""
}

func injectInitializerType(field *sitter.Node, src []byte) string {
	value := field.ChildByFieldName("value")
	if value == nil || value.Type() != "call_expression" {
		return ""
	}
	fn := value.ChildByFieldName("function")
	if fn == nil || fn.Type() != "identifier" || fn.Content(src) != "inject" {
		return ""
	}
	args := value.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		if arg.Type() == "identifier" {
			return arg.Content(src)
		}
		return ""
	}
	return ""
}

func hasParameterPropertyModifier(p *sitter.Node) bool {
	for i := 0; i < int(p.ChildCount()); i++ {
		c := p.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "accessibility_modifier", "readonly":
			return true
		case "override_modifier":
			// `override` can accompany a visibility modifier; keep scanning.
		}
	}
	return false
}

func paramIdentifier(p *sitter.Node, src []byte) string {
	pattern := p.ChildByFieldName("pattern")
	if pattern != nil && pattern.Type() == "identifier" {
		return pattern.Content(src)
	}
	for i := 0; i < int(p.NamedChildCount()); i++ {
		c := p.NamedChild(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return ""
}

func paramTypeAnnotation(p *sitter.Node, src []byte) string {
	ta := p.ChildByFieldName("type")
	if ta == nil {
		for i := 0; i < int(p.NamedChildCount()); i++ {
			c := p.NamedChild(i)
			if c != nil && c.Type() == "type_annotation" {
				ta = c
				break
			}
		}
	}
	if ta == nil {
		return ""
	}
	for i := 0; i < int(ta.NamedChildCount()); i++ {
		c := ta.NamedChild(i)
		if c == nil {
			continue
		}
		return normalizeTypeName(c.Content(src))
	}
	return ""
}

// normalizeTypeName strips generics, arrays, and nullable markers.
// "User" → "User", "User[]" → "User", "User<T>" → "User",
// "User | null" → "User".
func normalizeTypeName(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimSuffix(t, "[]")
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	if idx := strings.Index(t, " |"); idx > 0 {
		t = t[:idx]
	}
	switch t {
	case "string", "number", "boolean", "void", "any", "unknown", "never", "null", "undefined":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

func extractTSMethodReturnType(methodNode *sitter.Node, src []byte) string {
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		child := methodNode.NamedChild(i)
		if child.Type() == "type_annotation" {
			if child.NamedChildCount() > 0 {
				typeNode := child.NamedChild(0)
				return normalizeTypeName(typeNode.Content(src))
			}
		}
	}
	return ""
}

func inferTypeFromNewExpr(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "identifier" || child.Type() == "type_identifier" {
			name := child.Content(src)
			if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	}
	return ""
}
