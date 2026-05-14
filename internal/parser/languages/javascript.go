package languages

import (
	"fmt"

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

func (e *JavaScriptExtractor) Language() string     { return "javascript" }
func (e *JavaScriptExtractor) Extensions() []string { return []string{".js", ".jsx", ".mjs"} }

// --- Deferred match buffers ----------------------------------------

type jsDeferredCall struct {
	name     string
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

		case m.Captures["method.def"] != nil:
			e.emitMethod(m, filePath, src, result)

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
			calls = append(calls, jsDeferredCall{
				name:     m.Captures["callm.method"].Text,
				line:     expr.StartLine + 1,
				isMember: true,
				expr:     expr.Node,
			})

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

	// Resolve calls against funcRanges.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if c.isMember {
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::*." + c.name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
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
	emitPubsubEvents(pubsubEvents,
		func(line int) string { return findEnclosingFunc(funcRanges, line) },
		filePath, "javascript", result)

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *JavaScriptExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("function %s()", name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
	if body := tsFunctionBody(def.Node); body != nil {
		emitJSXRenderEdges(id, body, src, filePath, result)
	}
}

func (e *JavaScriptExtractor) emitArrow(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, arrowNames map[string]bool) {
	name := m.Captures["arrow.name"].Text
	def := m.Captures["arrow.def"]
	arrowNames[name] = true
	id := filePath + "::" + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript", Meta: map[string]any{"signature": fmt.Sprintf("const %s = () =>", name)},
	})
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
// extractMethods re-run of jsQMethod.
func (e *JavaScriptExtractor) emitMethod(m parser.QueryResult, filePath string, src []byte, result *parser.ExtractionResult) {
	def := m.Captures["method.def"]
	classNode := findEnclosingJSContainer(def.Node, "class_declaration")
	if classNode == nil {
		return
	}
	nameNode := classNode.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	className := nameNode.Content(src)
	name := m.Captures["method.name"].Text
	classID := filePath + "::" + className
	id := filePath + "::" + className + "." + name
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMethod, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "javascript",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: id, To: classID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
	})
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
