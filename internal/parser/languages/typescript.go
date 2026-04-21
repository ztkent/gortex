package languages

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	tsQFunction = `(function_declaration
		name: (identifier) @func.name) @func.def`

	tsQArrow = `(lexical_declaration
		(variable_declarator
			name: (identifier) @func.name
			value: (arrow_function) @func.body)) @func.def`

	tsQClass = `(class_declaration
		name: (type_identifier) @class.name) @class.def`

	tsQInterface = `(interface_declaration
		name: (type_identifier) @iface.name) @iface.def`

	tsQTypeAlias = `(type_alias_declaration
		name: (type_identifier) @type.name) @type.def`

	tsQMethod = `(method_definition
		name: (property_identifier) @method.name) @method.def`

	tsQImport = `(import_statement
		source: (string) @import.path) @import.def`

	tsQCall = `(call_expression
		function: (identifier) @call.name) @call.expr`

	tsQCallMember = `(call_expression
		function: (member_expression
			object: (_) @call.receiver
			property: (property_identifier) @call.method)) @call.expr`

	tsQVar = `(lexical_declaration
		(variable_declarator
			name: (identifier) @var.name)) @var.def`

	tsQVarTyped = `(lexical_declaration
		(variable_declarator
			name: (identifier) @tvar.name
			type: (type_annotation (_) @tvar.type))) @tvar.def`

	tsQExport = `(export_statement
		(function_declaration
			name: (identifier) @func.name)) @func.def`

	// Enums and their members. Tree-sitter-typescript represents an
	// enum body as `enum_body` containing either bare
	// `property_identifier` values or `enum_assignment` nodes with a
	// name field. We capture both via the parent enum_declaration so
	// a single query walks both patterns.
	tsQEnum = `(enum_declaration
		name: (identifier) @enum.name) @enum.def`

	// Class property (field) declarations — `readonly foo: string`,
	// `private _bar = 42`, etc. These are typed, visible members that
	// agents should be able to search for. Distinct from method
	// definitions (already handled by tsQMethod).
	tsQClassProperty = `(public_field_definition
		name: (property_identifier) @prop.name) @prop.def`
)

// TypeScriptExtractor extracts TypeScript/JavaScript source files.
type TypeScriptExtractor struct {
	lang *sitter.Language
}

func NewTypeScriptExtractor() *TypeScriptExtractor {
	return &TypeScriptExtractor{lang: typescript.GetLanguage()}
}

func (e *TypeScriptExtractor) Language() string     { return "typescript" }
func (e *TypeScriptExtractor) Extensions() []string { return []string{".ts", ".tsx"} }

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
	result.Nodes = append(result.Nodes, fileNode)

	// Functions.
	for _, q := range []string{tsQFunction, tsQExport} {
		e.extractFuncs(q, root, src, filePath, fileNode.ID, result)
	}

	// Arrow functions assigned to variables.
	e.extractArrowFuncs(root, src, filePath, fileNode.ID, result)

	// Classes.
	e.extractClasses(root, src, filePath, fileNode.ID, result)

	// Interfaces.
	e.extractInterfaces(root, src, filePath, fileNode.ID, result)

	// Enums (declaration + members).
	e.extractEnums(root, src, filePath, fileNode.ID, result)

	// Type aliases.
	e.extractTypeAliases(root, src, filePath, fileNode.ID, result)

	// Imports. Returned alias→path map threads into extractCalls so
	// selector calls like `json.parse` attribute to the owning module.
	imports := e.extractImports(root, src, filePath, fileNode.ID, result)

	// Build type environment for receiver type inference.
	tenv := e.buildTypeEnv(root, src)

	// Call sites (with type env + imports).
	e.extractCalls(root, src, filePath, result, tenv, imports)

	// Variables.
	e.extractVariables(root, src, filePath, fileNode.ID, result)

	return result, nil
}

func (e *TypeScriptExtractor) extractFuncs(q string, root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(q, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["func.name"].Text
		def := m.Captures["func.def"]
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript", Meta: map[string]any{"signature": fmt.Sprintf("function %s()", name)},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

func (e *TypeScriptExtractor) extractArrowFuncs(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQArrow, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["func.name"].Text
		def := m.Captures["func.def"]
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript", Meta: map[string]any{"signature": fmt.Sprintf("const %s = () =>", name)},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

func (e *TypeScriptExtractor) extractClasses(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQClass, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["class.name"].Text
		def := m.Captures["class.def"]
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})

		// Methods inside the class.
		e.extractMethods(def.Node, src, filePath, id, result)

		// Fields / properties inside the class. Without this,
		// class_body's public_field_definition children are
		// invisible to search; a typical VSCode class (10-30
		// fields) loses most of its surface area in the graph.
		e.extractClassProperties(def.Node, src, filePath, id, result)

		// NestJS module providers: `@Module({ providers: [{ provide: X,
		// useClass: Y }] })` declares that when a consumer asks for X it
		// receives Y. Emit an EdgeProvides from the module to Y tagged
		// with provides_for=X so the resolver can pick the bound
		// implementation when receiver_type is abstract.
		if def.Node != nil {
			emitModuleBindings(def.Node, src, id, filePath, result)
		}
	}
}

// extractEnums adds enum declarations (as KindType) plus each member
// (as KindVariable with a member_of edge back to the enum). Enums are
// first-class value namespaces in TypeScript: `KeybindingWeight.EditorCore`
// resolves to a member, so users should be able to search for both the
// enum and its cases.
func (e *TypeScriptExtractor) extractEnums(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQEnum, e.lang, root, src)
	for _, m := range matches {
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

		// Walk the enum body for member names. The grammar yields
		// enum_body → (property_identifier | enum_assignment) children.
		// Handle both so `FOO` and `FOO = 1` style members both land.
		if def.Node == nil {
			continue
		}
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
}

// extractClassProperties walks a class_body for public_field_definition
// nodes and adds them as KindVariable with member_of edges to the class.
// TS classes routinely carry 10-30 typed fields; missing them bleeds a
// lot of useful graph surface area.
func (e *TypeScriptExtractor) extractClassProperties(classNode *sitter.Node, src []byte, filePath, classID string, result *parser.ExtractionResult) {
	className := classID[strings.LastIndex(classID, "::")+2:]
	matches, _ := parser.RunQuery(tsQClassProperty, e.lang, classNode, src)
	for _, m := range matches {
		name := m.Captures["prop.name"].Text
		def := m.Captures["prop.def"]
		id := filePath + "::" + className + "." + name
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
}

func (e *TypeScriptExtractor) extractMethods(classNode *sitter.Node, src []byte, filePath, classID string, result *parser.ExtractionResult) {
	className := classID[strings.LastIndex(classID, "::")+2:]
	matches, _ := parser.RunQuery(tsQMethod, e.lang, classNode, src)
	for _, m := range matches {
		name := m.Captures["method.name"].Text
		def := m.Captures["method.def"]
		id := filePath + "::" + className + "." + name
		node := &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript",
			Meta:     map[string]any{"receiver": className},
		}
		// Walk the method_definition node's children for a return type annotation.
		if def.Node != nil {
			if rt := extractTSMethodReturnType(def.Node, src); rt != "" {
				node.Meta["return_type"] = rt
			}
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
		// NestJS-style decorator dispatch: `@UseGuards(AuthGuard)` on a
		// method binds `AuthGuard.canActivate` to framework-side invocation
		// when the method handles a request. No source call site exists,
		// so the graph is blind to it without this synthetic edge. Also
		// covers @UseInterceptors / @UseFilters / @UsePipes.
		//
		// In tree-sitter-typescript, method decorators are SIBLINGS of the
		// method_definition inside class_body (not children), so we walk
		// prev siblings backward until we hit a non-decorator node.
		if def.Node != nil {
			for sib := def.Node.PrevSibling(); sib != nil && sib.Type() == "decorator"; sib = sib.PrevSibling() {
				emitDispatchFromDecorator(sib, src, id, filePath, result)
			}
		}
	}
}

func (e *TypeScriptExtractor) extractInterfaces(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQInterface, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["iface.name"].Text
		def := m.Captures["iface.def"]
		id := filePath + "::" + name

		// Walk the interface body to extract method/property signature names.
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
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

func (e *TypeScriptExtractor) extractTypeAliases(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQTypeAlias, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["type.name"].Text
		def := m.Captures["type.def"]
		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

// extractImports emits one EdgeImports per import_statement and returns
// a per-file alias→importPath map. We only record aliases that appear
// as receivers in selector calls downstream:
//
//   import foo from 'mod'          → foo → mod   (default)
//   import * as foo from 'mod'     → foo → mod   (namespace)
//   import { a, b as c } from 'x'  → not tracked (called as bare identifier)
//
// Named imports are intentionally skipped — `a(x)` is already a plain
// call matched by tsQCall and doesn't go through the selector-call path.
func (e *TypeScriptExtractor) extractImports(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) map[string]string {
	imports := map[string]string{}
	matches, _ := parser.RunQuery(tsQImport, e.lang, root, src)
	for _, m := range matches {
		path := m.Captures["import.path"]
		importPath := strings.Trim(path.Text, `"'`)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: "unresolved::import::" + importPath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
		})
		// Walk the import_statement node to find aliases. tsQImport
		// captures the whole statement as `import.def` so we already
		// have the AST node.
		defCap, ok := m.Captures["import.def"]
		if !ok || defCap.Node == nil {
			continue
		}
		for i := 0; i < int(defCap.Node.NamedChildCount()); i++ {
			child := defCap.Node.NamedChild(i)
			if child.Type() != "import_clause" {
				continue
			}
			for j := 0; j < int(child.NamedChildCount()); j++ {
				c := child.NamedChild(j)
				switch c.Type() {
				case "identifier": // default import: `import Foo from ...`
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
	return imports
}

func (e *TypeScriptExtractor) extractVariables(root *sitter.Node, src []byte, filePath, fileID string, result *parser.ExtractionResult) {
	matches, _ := parser.RunQuery(tsQVar, e.lang, root, src)

	// Collect names already extracted as arrow functions so we skip them.
	arrowNames := make(map[string]bool)
	for _, n := range result.Nodes {
		if n.Kind == graph.KindFunction && n.FilePath == filePath {
			arrowNames[n.Name] = true
		}
	}

	for _, m := range matches {
		name := m.Captures["var.name"].Text
		def := m.Captures["var.def"]

		// Skip variables already captured as arrow functions.
		if arrowNames[name] {
			continue
		}

		// Only extract module-level variables: the lexical_declaration's parent
		// should be the program (root) node or an export_statement whose parent
		// is the program node.
		parent := def.Node.Parent()
		if parent != nil && parent.Type() == "export_statement" {
			parent = parent.Parent()
		}
		if parent == nil || parent.Type() != "program" {
			continue
		}

		id := filePath + "::" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "typescript",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

// extractTSInterfaceMethods walks children of an interface_declaration node
// to find method_signature and property_signature entries and returns their names.
func extractTSInterfaceMethods(ifaceNode *sitter.Node, src []byte) []string {
	var methods []string
	// Find the interface_body child.
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
			// The first named child is typically the property_identifier (name).
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

func (e *TypeScriptExtractor) extractCalls(root *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult, tenv typeEnv, imports map[string]string) {
	funcRanges := buildFuncRanges(result)

	matches, _ := parser.RunQuery(tsQCall, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["call.name"].Text
		expr := m.Captures["call.expr"]
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	}

	matches, _ = parser.RunQuery(tsQCallMember, e.lang, root, src)
	for _, m := range matches {
		method := m.Captures["call.method"].Text
		receiverText := m.Captures["call.receiver"].Text
		expr := m.Captures["call.expr"]
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}

		// Namespace/default import receiver (e.g. `fs.readFile`): attach
		// the module path so the resolver can classify externally.
		if importPath, ok := imports[receiverText]; ok {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::extern::" + importPath + "::" + method,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
			})
			continue
		}

		edge := &graph.Edge{
			From: callerID, To: "unresolved::*." + method,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		}
		if recvType, ok := tenv[receiverText]; ok {
			edge.Meta = map[string]any{"receiver_type": recvType}
		} else if strings.Contains(receiverText, ".") || strings.Contains(receiverText, "(") {
			if chainType := resolveChainType(receiverText, tenv, result); chainType != "" {
				edge.Meta = map[string]any{"receiver_type": chainType}
			}
		}
		result.Edges = append(result.Edges, edge)
	}
}

// buildTypeEnv scans TypeScript variable declarations for type annotations (Tier 0)
// and new expressions (Tier 1) to build a variable→type map.
func (e *TypeScriptExtractor) buildTypeEnv(root *sitter.Node, src []byte) typeEnv {
	tenv := make(typeEnv)

	// Tier 0: explicit type annotations — const x: Type = ...
	matches, _ := parser.RunQuery(tsQVarTyped, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["tvar.name"].Text
		typeName := normalizeTypeName(m.Captures["tvar.type"].Text)
		if typeName != "" {
			tenv[name] = typeName
		}
	}

	// Tier 0b: constructor parameters with visibility modifiers
	// (`constructor(private readonly svc: UsersService) {}`) are stored
	// as class members accessible through `this.svc`. Without this the
	// extractor can't type `this.svc.foo()` call expressions, so the
	// resolver falls back to "caller's receiver type" and method-name
	// collisions inside the containing class cause self-loops — as
	// seen with UsersController.create → UsersController.create.
	collectThisParamTypes(root, src, tenv)

	// Tier 1: new expressions — const x = new Type(...)
	// Walk all variable declarators and check if RHS is a new_expression.
	matches, _ = parser.RunQuery(tsQVar, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["var.name"].Text
		if _, exists := tenv[name]; exists {
			continue // already have explicit type
		}
		// Find the variable_declarator node to check its value child.
		defNode := m.Captures["var.def"].Node
		if defNode == nil {
			continue
		}
		walkNodes(defNode, func(n *sitter.Node) {
			if n.Type() == "variable_declarator" {
				for i := 0; i < int(n.NamedChildCount()); i++ {
					child := n.NamedChild(i)
					if child.Type() == "new_expression" {
						typeName := inferTypeFromNewExpr(child, src)
						if typeName != "" {
							tenv[name] = typeName
						}
						return
					}
				}
			}
		})
	}

	return tenv
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
	// Decorators on classes come as siblings via an export_statement
	// wrapper OR as children of class_declaration, depending on the
	// grammar version and whether the class is exported. Walk both
	// directions to be robust.
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
		// The Module decorator takes one object literal arg.
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
		providersNode := objectFieldValue(config, src, "providers")
		if providersNode == nil || providersNode.Type() != "array" {
			continue
		}
		for i := 0; i < int(providersNode.NamedChildCount()); i++ {
			entry := providersNode.NamedChild(i)
			if entry == nil || entry.Type() != "object" {
				continue
			}
			abstract := objectFieldIdentifier(entry, src, "provide")
			concrete := objectFieldIdentifier(entry, src, "useClass")
			if abstract == "" || concrete == "" {
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     classID,
				To:       "unresolved::" + concrete,
				Kind:     graph.EdgeProvides,
				FilePath: filePath,
				Line:     int(entry.StartPoint().Row) + 1,
				Meta: map[string]any{
					"provides_for": abstract,
					"binding":      "useClass",
				},
			})
		}
	}
}

// classDecorators returns decorator nodes applicable to a class_declaration.
// In tree-sitter-typescript, decorators appear either as children of the
// class_declaration directly or, when the class is `export`-ed, as
// children of the enclosing export_statement preceding the class.
func classDecorators(classNode *sitter.Node) []*sitter.Node {
	var decs []*sitter.Node
	// Direct children first.
	for i := 0; i < int(classNode.ChildCount()); i++ {
		c := classNode.Child(i)
		if c != nil && c.Type() == "decorator" {
			decs = append(decs, c)
		}
	}
	// Parent export_statement siblings.
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

// objectFieldValue returns the `value` child of a `pair` node inside the
// given object literal whose key matches the supplied name, or nil when
// absent. Works on tree-sitter's JS/TS object grammar.
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

// objectFieldIdentifier is a thin wrapper on objectFieldValue that only
// returns a value when it's a plain identifier (the shape we care about
// for `provide: X` / `useClass: Y` entries). Returns "" otherwise.
func objectFieldIdentifier(objNode *sitter.Node, src []byte, name string) string {
	v := objectFieldValue(objNode, src, name)
	if v == nil || v.Type() != "identifier" {
		return ""
	}
	return v.Content(src)
}

// nestDispatchDecorators maps a NestJS-style dispatch decorator name to
// the entry-point method name on its argument class. `@UseGuards(X)`
// causes X.canActivate to run; `@UseInterceptors(Y)` → Y.intercept; etc.
// Decorators not in this table are ignored (they don't produce runtime
// dispatch we can link statically).
var nestDispatchDecorators = map[string]string{
	"UseGuards":       "canActivate",
	"UseInterceptors": "intercept",
	"UseFilters":      "catch",
	"UsePipes":        "transform",
}

// emitDispatchFromDecorator inspects one decorator node. If it's one of
// the recognised NestJS dispatch decorators (@UseGuards, @UseInterceptors,
// @UseFilters, @UsePipes), it emits one unresolved edge from methodID to
// the entry-point method on each class argument. Each edge carries
// receiver_type so the resolver's Pass 1 disambiguates by class rather
// than falling back to name-only heuristics. Non-identifier arguments
// (`new X()`, literals) are silently skipped.
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

// nestDecoratorCall returns the call_expression child of a decorator node
// if the decorator has the shape `@Name(args)`. Plain `@Name` without
// parens returns nil (those can't bind arguments, so nothing to emit).
func nestDecoratorCall(dec *sitter.Node) *sitter.Node {
	for i := 0; i < int(dec.NamedChildCount()); i++ {
		c := dec.NamedChild(i)
		if c != nil && c.Type() == "call_expression" {
			return c
		}
	}
	return nil
}

// collectThisParamTypes walks every class_declaration, finds constructors
// that use TypeScript's "parameter property" shorthand
// (`constructor(private readonly svc: UsersService) {}`), and seeds the
// type env so later `this.svc.foo()` call sites can be typed. A parameter
// property is a required_parameter whose first child is an accessibility
// or readonly modifier — that's how the tree-sitter-typescript grammar
// distinguishes them from plain constructor args.
func collectThisParamTypes(root *sitter.Node, src []byte, tenv typeEnv) {
	walkNodes(root, func(n *sitter.Node) {
		if n.Type() != "class_declaration" {
			return
		}
		walkNodes(n, func(m *sitter.Node) {
			if m.Type() != "method_definition" {
				return
			}
			nameNode := m.ChildByFieldName("name")
			if nameNode == nil || nameNode.Content(src) != "constructor" {
				return
			}
			params := m.ChildByFieldName("parameters")
			if params == nil {
				return
			}
			for i := 0; i < int(params.NamedChildCount()); i++ {
				p := params.NamedChild(i)
				if p == nil {
					continue
				}
				// Only required_parameter nodes can carry the parameter-
				// property shorthand; plain identifiers bail out.
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
				// Key by `this.<name>` so extractCalls' receiverText
				// lookup ("this.svc") matches directly.
				tenv["this."+paramName] = typeName
			}
		})
	})
}

// hasParameterPropertyModifier reports whether a required_parameter node
// carries one of the visibility/readonly modifiers that promote a ctor
// arg to a class member in TypeScript.
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
			// `override` can accompany a visibility modifier; keep
			// scanning rather than returning false.
		}
	}
	return false
}

// paramIdentifier finds the underlying identifier name for a required_parameter
// — handles both `foo: T` and `foo?: T` / `foo = default`.
func paramIdentifier(p *sitter.Node, src []byte) string {
	pattern := p.ChildByFieldName("pattern")
	if pattern != nil && pattern.Type() == "identifier" {
		return pattern.Content(src)
	}
	// Fallback: first identifier child.
	for i := 0; i < int(p.NamedChildCount()); i++ {
		c := p.NamedChild(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return ""
}

// paramTypeAnnotation extracts the type name from a required_parameter's
// type_annotation child, applying the same normalization as Tier 0 var
// declarations (strip generics, arrays, nullable unions, primitives).
func paramTypeAnnotation(p *sitter.Node, src []byte) string {
	ta := p.ChildByFieldName("type")
	if ta == nil {
		// Some grammar versions don't expose a "type" field on parameters;
		// fall back to the first type_annotation child.
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

// normalizeTypeName strips generics, arrays, and nullable markers from a type name.
// "User" → "User", "User[]" → "User", "User<T>" → "User", "User | null" → "User"
func normalizeTypeName(t string) string {
	// Strip leading/trailing whitespace.
	t = strings.TrimSpace(t)
	// Remove array suffix.
	t = strings.TrimSuffix(t, "[]")
	// Remove generics.
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	// Remove nullable union.
	if idx := strings.Index(t, " |"); idx > 0 {
		t = t[:idx]
	}
	// Skip primitives.
	switch t {
	case "string", "number", "boolean", "void", "any", "unknown", "never", "null", "undefined":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return "" // skip lowercase type names (primitives, type aliases like 'object')
	}
	return t
}

// extractTSMethodReturnType walks a method_definition node's children to find
// a type_annotation child and returns the normalized type name.
func extractTSMethodReturnType(methodNode *sitter.Node, src []byte) string {
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		child := methodNode.NamedChild(i)
		if child.Type() == "type_annotation" {
			// The type_annotation's first named child is the actual type node.
			if child.NamedChildCount() > 0 {
				typeNode := child.NamedChild(0)
				return normalizeTypeName(typeNode.Content(src))
			}
		}
	}
	return ""
}

// inferTypeFromNewExpr extracts the class name from a new_expression node.
// new User(...) → "User"
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
