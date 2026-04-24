package languages

import (
	"fmt"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/php"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// PHPExtractor extracts PHP source files.
type PHPExtractor struct {
	lang *sitter.Language
}

func NewPHPExtractor() *PHPExtractor {
	return &PHPExtractor{lang: php.GetLanguage()}
}

func (e *PHPExtractor) Language() string     { return "php" }
func (e *PHPExtractor) Extensions() []string { return []string{".php"} }

func (e *PHPExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "php",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST manually since PHP tree-sitter queries can be tricky.
	e.walkNode(root, src, filePath, fileNode, result, seen, "")

	return result, nil
}

func (e *PHPExtractor) walkNode(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	nodeType := node.Type()

	switch nodeType {
	case "namespace_definition":
		e.extractNamespace(node, src, filePath, fileNode, result, seen)

	case "class_declaration":
		e.extractClass(node, src, filePath, fileNode, result, seen)

	case "interface_declaration":
		e.extractInterface(node, src, filePath, fileNode, result, seen)

	case "function_definition":
		e.extractFunction(node, src, filePath, fileNode, result, seen)

	case "namespace_use_declaration":
		e.extractUseImport(node, src, filePath, fileNode, result)

	case "expression_statement":
		// Check for require/include calls.
		e.extractRequireInclude(node, src, filePath, fileNode, result)
		// Also walk children for call expressions.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return

	default:
		// For class/interface bodies, walk into children with class context.
		e.walkChildren(node, src, filePath, fileNode, result, seen, currentClass)
		return
	}
}

func (e *PHPExtractor) walkChildren(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	currentClass string,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		e.walkNode(child, src, filePath, fileNode, result, seen, currentClass)
	}
}

func (e *PHPExtractor) extractNamespace(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := e.findChildByType(node, "namespace_name")
	if name == nil {
		return
	}
	nsName := name.Content(src)
	id := filePath + "::" + nsName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindPackage, Name: nsName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Walk children of namespace body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		for i := 0; i < int(body.NamedChildCount()); i++ {
			child := body.NamedChild(i)
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
	// Some namespaces don't use braces; walk remaining children.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() != "namespace_name" && child.Type() != "compound_statement" {
			e.walkNode(child, src, filePath, fileNode, result, seen, "")
		}
	}
}

func (e *PHPExtractor) extractClass(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	className := nameNode.Content(src)
	id := filePath + "::" + className
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: className,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract methods inside the class body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	methodNodes := make(map[string]*sitter.Node) // name → method_declaration node
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "method_declaration" {
			if n := e.findChildByFieldName(child, "name"); n != nil {
				methodNodes[n.Content(src)] = child
			}
			e.extractMethod(child, src, filePath, fileNode, result, seen, className)
		}
	}
	// Laravel-specific dispatch passes — run after methods are in the
	// graph so action-method IDs are resolvable by name:
	//   1. Controller middleware: `$this->middleware(X)->only([...])`
	//      in the constructor → edges from each action to X.handle.
	//   2. Service provider bindings: `$this->app->bind/singleton(A,B)`
	//      in register() → EdgeProvides from the provider class.
	e.emitLaravelMiddleware(methodNodes, src, filePath, className, result)
	e.emitLaravelBindings(methodNodes, src, filePath, className, result)
	// Symfony attribute-based dispatch: #[AsEventListener] binds a
	// method (or whole class) to an event class as a listener. The
	// framework calls the listener at event-dispatch time with no
	// explicit call site; the attribute is the only static signal.
	e.emitSymfonyAttributeDispatch(node, methodNodes, src, filePath, className, result)
}

func (e *PHPExtractor) extractInterface(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	ifaceName := nameNode.Content(src)
	id := filePath + "::" + ifaceName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: ifaceName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract method signatures inside the interface body.
	body := e.findChildByType(node, "declaration_list")
	if body == nil {
		return
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() == "method_declaration" {
			e.extractMethod(child, src, filePath, fileNode, result, seen, ifaceName)
		}
	}
}

func (e *PHPExtractor) extractFunction(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	funcName := nameNode.Content(src)
	id := filePath + "::" + funcName
	if seen[id] {
		return
	}
	seen[id] = true
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: funcName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})

	// Extract call sites within the function body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		e.extractCallSites(body, src, filePath, id, result)
	}
}

func (e *PHPExtractor) extractMethod(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	className string,
) {
	nameNode := e.findChildByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	methodName := nameNode.Content(src)
	id := filePath + "::" + className + "." + methodName
	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1
	if seen[id] {
		id = filePath + "::" + className + "." + methodName + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: methodName,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "php",
		Meta:     map[string]any{"receiver": className},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
	})
	// MemberOf edge to containing class/interface.
	classID := filePath + "::" + className
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine,
	})

	// Extract call sites within the method body.
	body := e.findChildByType(node, "compound_statement")
	if body != nil {
		e.extractCallSites(body, src, filePath, id, result)
	}
}

func (e *PHPExtractor) extractUseImport(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// use_declaration children can be namespace_use_clause or namespace_name.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		var importPath string
		switch child.Type() {
		case "namespace_use_clause":
			nameNode := e.findChildByType(child, "qualified_name")
			if nameNode == nil {
				nameNode = e.findChildByType(child, "namespace_name")
			}
			if nameNode != nil {
				importPath = nameNode.Content(src)
			} else {
				importPath = child.Content(src)
			}
		case "qualified_name", "namespace_name":
			importPath = child.Content(src)
		default:
			continue
		}
		if importPath == "" {
			continue
		}
		importPath = strings.TrimLeft(importPath, "\\")
		importPath = strings.ReplaceAll(importPath, "\\", "/")
		line := int(child.StartPoint().Row) + 1
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + importPath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: line,
		})
	}
}

func (e *PHPExtractor) extractRequireInclude(
	node *sitter.Node, src []byte,
	filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// Look for require, require_once, include, include_once expressions.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		ct := child.Type()
		if ct == "require_expression" || ct == "require_once_expression" ||
			ct == "include_expression" || ct == "include_once_expression" {
			// The path is a string/encapsed_string containing a string_content child.
			path := e.extractStringContent(child, src)
			if path != "" {
				line := int(child.StartPoint().Row) + 1
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: "unresolved::import::" + path,
					Kind: graph.EdgeImports, FilePath: filePath, Line: line,
				})
			}
		}
	}
}

func (e *PHPExtractor) extractCallSites(
	node *sitter.Node, src []byte,
	filePath string, callerID string,
	result *parser.ExtractionResult,
) {
	switch node.Type() {
	case "function_call_expression":
		funcNode := node.ChildByFieldName("function")
		if funcNode != nil {
			name := funcNode.Content(src)
			if idx := strings.LastIndex(name, "\\"); idx >= 0 {
				name = name[idx+1:]
			}
			line := int(node.StartPoint().Row) + 1
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	case "member_call_expression", "scoped_call_expression":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			name := nameNode.Content(src)
			line := int(node.StartPoint().Row) + 1
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		}
	}

	// Recurse into children.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		e.extractCallSites(child, src, filePath, callerID, result)
	}
}

// findChildByType finds the first named child with the given type.
func (e *PHPExtractor) findChildByType(node *sitter.Node, typeName string) *sitter.Node {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == typeName {
			return child
		}
	}
	return nil
}

// findChildByFieldName finds a child by its field name.
func (e *PHPExtractor) findChildByFieldName(node *sitter.Node, fieldName string) *sitter.Node {
	return node.ChildByFieldName(fieldName)
}

// extractStringContent recursively finds the first string_content node and returns its text.
func (e *PHPExtractor) extractStringContent(node *sitter.Node, src []byte) string {
	if node.Type() == "string_content" {
		return node.Content(src)
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		if result := e.extractStringContent(node.NamedChild(i), src); result != "" {
			return result
		}
	}
	return ""
}

// emitLaravelMiddleware scans the constructor body of a class for
// `$this->middleware(X)` calls, optionally followed by ->only([...])
// or ->except([...]) chains. For each discovered middleware class
// it emits one EdgeCalls per (action_method, middleware.handle)
// pair, honouring the only:/except: filters. Laravel invokes the
// middleware's handle() before each action at request time with no
// explicit call site.
func (e *PHPExtractor) emitLaravelMiddleware(methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	ctor, ok := methodNodes["__construct"]
	if !ok {
		return
	}
	body := e.findChildByType(ctor, "compound_statement")
	if body == nil {
		return
	}
	// Collect a set of action methods — every public-looking method
	// on the class except __construct, magic methods, and methods
	// that look like middleware-helpers themselves. In a real
	// Laravel controller, most non-ctor methods are actions; we
	// filter by leading underscore (magic methods start with __)
	// and the constructor.
	var actions []string
	for name := range methodNodes {
		if name == "__construct" || len(name) == 0 {
			continue
		}
		if len(name) >= 2 && name[0] == '_' && name[1] == '_' {
			continue
		}
		actions = append(actions, name)
	}
	if len(actions) == 0 {
		return
	}

	// Walk the ctor body for member_call_expression chains whose
	// outermost call participates in the middleware()[->only/except]
	// sequence. We process only the outermost call in each chain —
	// otherwise a chain like `middleware(X)->only([...])` would be
	// visited twice: once as the outer only() call (which descends
	// inward to find the middleware class) and once more as the inner
	// middleware() call on its own (producing a duplicate, UNfiltered
	// edge set).
	processed := make(map[uintptr]struct{})
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "member_call_expression" {
			if _, dup := processed[n.Id()]; !dup {
				// Only process if the parent isn't another
				// member_call_expression whose object is us —
				// that would mean we're the inner call of a filter
				// chain, already handled above.
				parent := n.Parent()
				if parent == nil || parent.Type() != "member_call_expression" || !parent.ChildByFieldName("object").Equal(n) {
					mw, chainCalls := parseLaravelMiddlewareCall(n, src)
					for _, c := range chainCalls {
						processed[c.Id()] = struct{}{}
					}
					if mw.class != "" {
						for _, action := range actions {
							if len(mw.only) > 0 {
								if _, ok := mw.only[action]; !ok {
									continue
								}
							}
							if len(mw.except) > 0 {
								if _, ok := mw.except[action]; ok {
									continue
								}
							}
							actionID := filePath + "::" + className + "." + action
							result.Edges = append(result.Edges, &graph.Edge{
								From:     actionID,
								To:       "unresolved::*.handle",
								Kind:     graph.EdgeCalls,
								FilePath: filePath,
								Line:     int(n.StartPoint().Row) + 1,
								Meta: map[string]any{
									"receiver_type":      mw.class,
									"dispatch_macro":     "middleware",
									"laravel_middleware": mw.class,
								},
							})
						}
					}
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
}

type laravelMiddleware struct {
	class  string
	only   map[string]struct{}
	except map[string]struct{}
}

// parseLaravelMiddlewareCall walks a member_call_expression. If the
// expression is of the form `$this->middleware(X)` (possibly chained
// with ->only([...]) / ->except([...])), returns the middleware
// class name and any action filters. The outermost call is passed in;
// we unwrap nested member_call_expressions to find both the filter
// chain and the original middleware() call.
func parseLaravelMiddlewareCall(outer *sitter.Node, src []byte) (laravelMiddleware, []*sitter.Node) {
	var out laravelMiddleware
	var chain []*sitter.Node
	cur := outer
	for cur != nil && cur.Type() == "member_call_expression" {
		chain = append(chain, cur)
		name := phpMethodName(cur, src)
		if name == "middleware" {
			// Found the base call. Extract the first argument.
			args := phpCallArgs(cur)
			if args != nil && args.NamedChildCount() > 0 {
				out.class = phpExtractClassRef(args.NamedChild(0), src)
			}
			return out, chain
		}
		if name == "only" || name == "except" {
			args := phpCallArgs(cur)
			if args != nil && args.NamedChildCount() > 0 {
				set := phpExtractStringArray(args.NamedChild(0), src)
				if name == "only" {
					out.only = set
				} else {
					out.except = set
				}
			}
		}
		// Follow the chain — the inner object of this call is the
		// next member_call_expression to inspect.
		inner := cur.ChildByFieldName("object")
		if inner == nil {
			return laravelMiddleware{}, chain
		}
		cur = inner
	}
	return laravelMiddleware{}, chain
}

// emitLaravelBindings scans a service provider's register() method for
// $this->app->bind / ->singleton / ->instance calls and emits an
// EdgeProvides from the provider class to the second argument's class
// (the implementation or binding target). Tagged binding="useClass"
// so the contracts tool treats these alongside NestJS useClass edges.
func (e *PHPExtractor) emitLaravelBindings(methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	register, ok := methodNodes["register"]
	if !ok {
		return
	}
	body := e.findChildByType(register, "compound_statement")
	if body == nil {
		return
	}
	classID := filePath + "::" + className
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "member_call_expression" {
			method := phpMethodName(n, src)
			switch method {
			case "bind", "singleton", "instance":
				args := phpCallArgs(n)
				if args == nil || args.NamedChildCount() < 1 {
					break
				}
				first := phpExtractClassRef(args.NamedChild(0), src)
				if first == "" {
					break
				}
				// Second arg may be an identifier (class ref) or a
				// closure — we only emit the useClass-style edge
				// when it's a class reference. Factory closures have
				// their own call edges from the closure body.
				var impl string
				if args.NamedChildCount() >= 2 {
					impl = phpExtractClassRef(args.NamedChild(1), src)
				}
				line := int(n.StartPoint().Row) + 1
				if impl != "" && impl != first {
					// useClass-style: AppServiceProvider → Concrete,
					// tagged with provides_for=Abstract. The resolver
					// uses this to rewrite abstract-typed call sites
					// to the concrete implementation.
					result.Edges = append(result.Edges, &graph.Edge{
						From:     classID,
						To:       "unresolved::" + impl,
						Kind:     graph.EdgeProvides,
						FilePath: filePath,
						Line:     line,
						Meta: map[string]any{
							"provides_for": first,
							"binding":      "useClass",
							"origin":       method,
						},
					})
				}
				// find_usages visibility: also emit a provider edge
				// pointing at the binding target itself (the abstract
				// / token). Lets callers ask "who provides this
				// interface?" and get the service provider class
				// directly, regardless of the concrete impl.
				result.Edges = append(result.Edges, &graph.Edge{
					From:     classID,
					To:       "unresolved::" + first,
					Kind:     graph.EdgeProvides,
					FilePath: filePath,
					Line:     line,
					Meta: map[string]any{
						"di_token": first,
						"binding":  method,
						"origin":   method,
					},
				})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
}

// phpMethodName returns the method identifier in a member_call_expression.
func phpMethodName(call *sitter.Node, src []byte) string {
	nameNode := call.ChildByFieldName("name")
	if nameNode != nil {
		return nameNode.Content(src)
	}
	return ""
}

// phpCallArgs returns the arguments node of a member_call_expression.
func phpCallArgs(call *sitter.Node) *sitter.Node {
	return call.ChildByFieldName("arguments")
}

// phpExtractClassRef pulls a class name out of an argument node. Handles
// the `X::class` shape (scoped_call_expression / class_constant_access),
// bare identifiers, and simple string literals (middleware aliases).
// Returns "" when the argument isn't a static class reference.
func phpExtractClassRef(arg *sitter.Node, src []byte) string {
	if arg == nil {
		return ""
	}
	switch arg.Type() {
	case "argument":
		// Argument wrapper — unwrap to the contained expression.
		if arg.NamedChildCount() > 0 {
			return phpExtractClassRef(arg.NamedChild(0), src)
		}
		return ""
	case "class_constant_access_expression", "scoped_call_expression":
		// X::class — the class name is the first named child.
		if arg.NamedChildCount() > 0 {
			return arg.NamedChild(0).Content(src)
		}
	case "name", "qualified_name":
		return arg.Content(src)
	case "string":
		// middleware('auth') alias form — return the alias verbatim.
		for i := 0; i < int(arg.NamedChildCount()); i++ {
			c := arg.NamedChild(i)
			if c != nil && c.Type() == "string_content" {
				return c.Content(src)
			}
		}
	}
	return ""
}

// phpExtractStringArray collects string values or X::class names out
// of an `array(...)` / `[...]` expression for `->only([...])` /
// `->except([...])` filter chains. Returns a set keyed by method name.
func phpExtractStringArray(arg *sitter.Node, src []byte) map[string]struct{} {
	out := make(map[string]struct{})
	if arg == nil {
		return out
	}
	// Unwrap argument wrapper.
	if arg.Type() == "argument" {
		if arg.NamedChildCount() > 0 {
			return phpExtractStringArray(arg.NamedChild(0), src)
		}
		return out
	}
	if arg.Type() != "array_creation_expression" {
		return out
	}
	for i := 0; i < int(arg.NamedChildCount()); i++ {
		el := arg.NamedChild(i)
		if el == nil {
			continue
		}
		if el.Type() == "array_element_initializer" {
			// The actual value is the first named child.
			if el.NamedChildCount() > 0 {
				el = el.NamedChild(0)
			}
		}
		if el == nil {
			continue
		}
		switch el.Type() {
		case "string":
			for j := 0; j < int(el.NamedChildCount()); j++ {
				c := el.NamedChild(j)
				if c != nil && c.Type() == "string_content" {
					out[c.Content(src)] = struct{}{}
				}
			}
		case "class_constant_access_expression":
			if el.NamedChildCount() > 0 {
				out[el.NamedChild(0).Content(src)] = struct{}{}
			}
		}
	}
	return out
}

// emitSymfonyAttributeDispatch walks a class's attribute_list and the
// attribute_list of each method_declaration, emitting EdgeConsumes
// edges for every #[AsEventListener(event: X)] binding. The edge
// points from the listener method (or the class itself when the
// attribute is class-level) to the event class, so find_usages on
// the event surfaces the listener as a consumer. Meta identifies the
// attribute name so callers can distinguish these from plain
// references.
func (e *PHPExtractor) emitSymfonyAttributeDispatch(classNode *sitter.Node, methodNodes map[string]*sitter.Node, src []byte, filePath, className string, result *parser.ExtractionResult) {
	classID := filePath + "::" + className

	// Class-level attributes apply to the class as a whole. Symfony
	// treats a class-level #[AsEventListener] as shorthand for "this
	// class has one or more listener methods" — the dispatch
	// signature is discovered via the method attributes. We still
	// emit a class-level consumer edge so find_usages(Event) returns
	// the class even when every listener lives on a method.
	if clsAttrs := collectPhpAttributes(classNode, src); len(clsAttrs) > 0 {
		emitAttributeEdges(clsAttrs, classID, filePath, int(classNode.StartPoint().Row)+1, result)
	}
	// Method-level attributes. Each method_declaration has its own
	// attribute_list immediately before its modifiers.
	for name, methodNode := range methodNodes {
		attrs := collectPhpAttributes(methodNode, src)
		if len(attrs) == 0 {
			continue
		}
		methodID := filePath + "::" + className + "." + name
		emitAttributeEdges(attrs, methodID, filePath, int(methodNode.StartPoint().Row)+1, result)
	}
}

// phpAttribute captures the parsed form of one #[Attr(...)] instance —
// just what we need for dispatch extraction.
type phpAttribute struct {
	name   string
	line   int
	args   map[string]string // key → value (class ref / string literal)
}

// collectPhpAttributes scans a class_declaration or method_declaration
// for its attribute_list children and parses every attribute inside
// each group. Returns nil when the node carries no attributes.
func collectPhpAttributes(node *sitter.Node, src []byte) []phpAttribute {
	var out []phpAttribute
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil || c.Type() != "attribute_list" {
			continue
		}
		for j := 0; j < int(c.NamedChildCount()); j++ {
			group := c.NamedChild(j)
			if group == nil || group.Type() != "attribute_group" {
				continue
			}
			for k := 0; k < int(group.NamedChildCount()); k++ {
				attr := group.NamedChild(k)
				if attr == nil || attr.Type() != "attribute" {
					continue
				}
				parsed := parsePhpAttribute(attr, src)
				if parsed.name != "" {
					out = append(out, parsed)
				}
			}
		}
	}
	return out
}

// parsePhpAttribute turns an `attribute` node (the part inside the
// `#[ ]` wrapper) into a phpAttribute. Pulls named arguments the
// attribute's constructor expects (event:, method:, priority:) when
// they're simple class refs or string literals.
func parsePhpAttribute(attr *sitter.Node, src []byte) phpAttribute {
	var out phpAttribute
	nameNode := attr.ChildByFieldName("name")
	if nameNode == nil {
		// Grammar sometimes lacks the field name; fall back to the
		// first child of type "name".
		for i := 0; i < int(attr.NamedChildCount()); i++ {
			c := attr.NamedChild(i)
			if c != nil && (c.Type() == "name" || c.Type() == "qualified_name") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return out
	}
	out.name = nameNode.Content(src)
	out.line = int(attr.StartPoint().Row) + 1
	args := attr.ChildByFieldName("arguments")
	if args == nil {
		// Some grammars expose arguments as a positional NamedChild.
		for i := 0; i < int(attr.NamedChildCount()); i++ {
			c := attr.NamedChild(i)
			if c != nil && c.Type() == "arguments" {
				args = c
				break
			}
		}
	}
	if args == nil {
		return out
	}
	out.args = make(map[string]string)
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil || arg.Type() != "argument" {
			continue
		}
		// Named arguments have a `name` child first, then the value.
		var key string
		var valNode *sitter.Node
		for j := 0; j < int(arg.NamedChildCount()); j++ {
			c := arg.NamedChild(j)
			if c == nil {
				continue
			}
			if c.Type() == "name" && key == "" {
				key = c.Content(src)
				continue
			}
			valNode = c
			break
		}
		if valNode == nil {
			continue
		}
		if v := phpExtractClassRef(valNode, src); v != "" {
			out.args[key] = v
		}
	}
	return out
}

// symfonyDispatchAttributes names the attributes we extract dispatch
// edges for. Currently only AsEventListener — the most common Symfony
// DI attribute and the one where static dispatch makes a real
// difference. AsController / AsCommand are recognisable but don't
// produce dispatch edges (AsController is a marker; AsCommand binds
// a command name that isn't a node in the graph).
var symfonyDispatchAttributes = map[string]string{
	"AsEventListener": "event",
}

func emitAttributeEdges(attrs []phpAttribute, fromID, filePath string, fallbackLine int, result *parser.ExtractionResult) {
	for _, a := range attrs {
		targetKey, ok := symfonyDispatchAttributes[a.name]
		if !ok {
			continue
		}
		target := a.args[targetKey]
		if target == "" {
			continue
		}
		line := a.line
		if line == 0 {
			line = fallbackLine
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     fromID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeConsumes,
			FilePath: filePath,
			Line:     line,
			Meta: map[string]any{
				"dispatch_attribute": a.name,
				"symfony_event":      target,
			},
		})
	}
}
