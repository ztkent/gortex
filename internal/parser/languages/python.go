package languages

import (
	"strings"
	"unicode"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/python"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// qPyAll is a single tree-sitter query alternating over every pattern
// the Python extractor needs. One tree walk per file replaces the 10
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set. Class membership is
// resolved via a strict parent walk on the captured node — the function
// must be a direct child of `block` whose direct parent is
// `class_definition`. This mirrors the legacy `pyQClassMethod` nesting
// (decorated methods land in the free-function bucket — same bug, same
// behaviour).
const qPyAll = `
[
  (function_definition
    name: (identifier) @func.name) @func.def

  (class_definition
    name: (identifier) @class.name) @class.def

  (import_statement
    name: (dotted_name) @import.name) @import.def

  (import_from_statement
    module_name: (dotted_name) @import.module) @importfrom.def

  (call
    function: (identifier) @call.name) @call.expr

  (call
    function: (attribute
      object: (_) @callattr.receiver
      attribute: (identifier) @callattr.method)) @callattr.expr

  (assignment
    left: (identifier) @var.name) @var.def

  (assignment
    left: (identifier) @tvar.name
    type: (type (identifier) @tvar.type)) @tvar.def

  (assignment
    left: (identifier) @uvar.name
    right: (call
      function: (identifier) @uvar.callee)) @uvar.def
]
`

// PythonExtractor extracts Python source files into graph nodes and edges.
type PythonExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewPythonExtractor() *PythonExtractor {
	lang := python.GetLanguage()
	return &PythonExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qPyAll, lang),
	}
}

func (e *PythonExtractor) Language() string     { return "python" }
func (e *PythonExtractor) Extensions() []string { return []string{".py"} }

// --- Deferred match buffers ----------------------------------------

type pyDeferredCall struct {
	name       string
	receiver   string // attribute receiver text
	line       int
	isAttr     bool
	expr       *sitter.Node // for FastAPI Depends() arg lookup
}

func (e *PythonExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "python",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	imports := map[string]string{} // alias → module path
	tenv := make(typeEnv)
	tenvHasExplicit := make(map[string]bool) // names with Tier 0 type, lock from Tier 1 overwrite

	var calls []pyDeferredCall

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen)

		case m.Captures["class.def"] != nil:
			e.emitClass(m, filePath, fileID, result, seen)

		case m.Captures["import.def"] != nil:
			e.emitImport(m, filePath, fileID, src, result, imports)

		case m.Captures["importfrom.def"] != nil:
			e.emitImportFrom(m, filePath, fileID, result)

		case m.Captures["callattr.expr"] != nil:
			expr := m.Captures["callattr.expr"]
			calls = append(calls, pyDeferredCall{
				name:     m.Captures["callattr.method"].Text,
				receiver: m.Captures["callattr.receiver"].Text,
				line:     expr.StartLine + 1,
				isAttr:   true,
			})

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, pyDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
				expr: expr.Node,
			})

		case m.Captures["tvar.def"] != nil:
			// Tier 0: explicit type annotation — overwrite tenv.
			name := m.Captures["tvar.name"].Text
			typeName := normalizePyTypeName(m.Captures["tvar.type"].Text)
			if typeName != "" {
				tenv[name] = typeName
				tenvHasExplicit[name] = true
			}

		case m.Captures["uvar.def"] != nil:
			// Tier 1: constructor-call inference. Only fills in keys
			// that didn't get an explicit type — match the legacy
			// `if _, exists := tenv[name]; exists { continue }` guard.
			name := m.Captures["uvar.name"].Text
			if tenvHasExplicit[name] {
				return
			}
			if _, exists := tenv[name]; exists {
				return
			}
			callee := m.Captures["uvar.callee"].Text
			if callee != "" && unicode.IsUpper(rune(callee[0])) {
				tenv[name] = callee
			}

		case m.Captures["var.def"] != nil:
			e.emitTopLevelVar(m, filePath, fileID, result, seen)
		}
	})

	// All function/method nodes have been emitted; map call sites to
	// their enclosing definition.
	funcRanges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		if c.isAttr {
			// Module-qualified call (requests.get, np.array, os.path.join):
			// attach the import path so resolver can classify externally.
			if importPath, ok := lookupPyImport(c.receiver, imports); ok {
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::extern::" + importPath + "::" + c.name,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
				})
				continue
			}

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

		// Plain call.
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})

		// FastAPI dependency injection: Depends(target) — emit a direct
		// edge from the enclosing function to the first identifier
		// argument of Depends so the target shows up as a caller
		// relationship.
		if c.name == "Depends" && c.expr != nil {
			if dep := firstIdentifierArg(c.expr, src); dep != "" {
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::" + dep,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
					Meta: map[string]any{"via": "fastapi.Depends"},
				})
			}
		}
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

// emitFunction handles every function_definition. Strict parent check
// (function → block → class_definition) mirrors the legacy nested
// pyQClassMethod pattern: only direct children of a class body emit as
// receiver-qualified methods. Decorated methods (wrapped in
// decorated_definition) and nested functions land in the free-function
// bucket — same as the legacy code.
func (e *PythonExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	startLine1 := def.StartLine + 1

	className := pyDirectClassParent(def.Node, src)
	if className != "" {
		id := filePath + "::" + className + "." + name
		if seen[id] {
			return
		}
		seen[id] = true
		node := &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
			Language: "python", Meta: map[string]any{
				"receiver":  className,
				"signature": "def " + name + "(...)",
			},
		}
		if def.Node != nil {
			if rt := extractPyReturnType(def.Node, src); rt != "" {
				node.Meta["return_type"] = rt
			}
		}
		result.Nodes = append(result.Nodes, node)
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
		})
		typeID := filePath + "::" + className
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine1,
		})
		return
	}

	// Free function (top-level or nested in another function).
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine1, EndLine: def.EndLine + 1,
		Language: "python", Meta: map[string]any{"signature": "def " + name + "(...)"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine1,
	})
}

func (e *PythonExtractor) emitClass(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["class.name"].Text
	def := m.Captures["class.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "python",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitImport handles `import os`, `import os.path`, `import numpy as np`.
// Walks the import_statement node to populate the alias→module map used
// by attribute-call classification.
func (e *PythonExtractor) emitImport(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, imports map[string]string) {
	name := m.Captures["import.name"]
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + name.Text,
		Kind: graph.EdgeImports, FilePath: filePath, Line: name.StartLine + 1,
	})
	def, ok := m.Captures["import.def"]
	if !ok || def.Node == nil {
		return
	}
	stmt := def.Node
	for i := 0; i < int(stmt.NamedChildCount()); i++ {
		child := stmt.NamedChild(i)
		switch child.Type() {
		case "dotted_name":
			dotted := child.Content(src)
			alias := dotted
			if j := strings.Index(dotted, "."); j >= 0 {
				alias = dotted[:j] // `import os.path` binds `os`
			}
			imports[alias] = dotted
		case "aliased_import":
			var modulePath, alias string
			for j := 0; j < int(child.NamedChildCount()); j++ {
				cc := child.NamedChild(j)
				switch cc.Type() {
				case "dotted_name":
					modulePath = cc.Content(src)
				case "identifier":
					alias = cc.Content(src)
				}
			}
			if alias != "" && modulePath != "" {
				imports[alias] = modulePath
			}
		}
	}
}

func (e *PythonExtractor) emitImportFrom(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	mod := m.Captures["import.module"]
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: "unresolved::import::" + mod.Text,
		Kind: graph.EdgeImports, FilePath: filePath, Line: mod.StartLine + 1,
	})
}

func (e *PythonExtractor) emitTopLevelVar(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["var.name"].Text
	def := m.Captures["var.def"]
	if def.Node == nil || def.Node.Parent() == nil || def.Node.Parent().Type() != "module" {
		return
	}
	id := filePath + "::" + name
	if seen[id] || strings.HasPrefix(name, "_") {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindVariable, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "python",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// pyDirectClassParent returns the enclosing class name when fn is a
// direct child of a class_definition's body block, mirroring the
// legacy nested pyQClassMethod pattern. Returns "" for decorated
// methods (wrapped in decorated_definition), nested functions, and
// top-level functions — preserving legacy behaviour exactly.
func pyDirectClassParent(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	parent := fn.Parent()
	if parent == nil || parent.Type() != "block" {
		return ""
	}
	grand := parent.Parent()
	if grand == nil || grand.Type() != "class_definition" {
		return ""
	}
	nameNode := grand.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(src)
}

// firstIdentifierArg returns the string content of the first positional
// argument to a Python call_expression when that argument is a bare
// identifier (function name or class name), or "" otherwise. Used for
// FastAPI's Depends(target) where we want the target, not Depends
// itself, to show up as the called symbol. Non-identifier arguments —
// lambdas, attribute access, calls — are skipped because they can't
// be statically resolved to a graph node.
func firstIdentifierArg(callNode *sitter.Node, src []byte) string {
	args := callNode.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i := 0; i < int(args.NamedChildCount()); i++ {
		arg := args.NamedChild(i)
		if arg == nil {
			continue
		}
		// Skip keyword arguments — Depends(use_cache=False, ...) shouldn't
		// produce a call edge to `False`.
		if arg.Type() == "keyword_argument" {
			continue
		}
		if arg.Type() == "identifier" {
			return arg.Content(src)
		}
		return ""
	}
	return ""
}

// extractPyReturnType walks a function_definition node for a return_type child
// (the `-> Type` annotation) and returns the normalized type name.
func extractPyReturnType(funcNode *sitter.Node, src []byte) string {
	for i := 0; i < int(funcNode.NamedChildCount()); i++ {
		child := funcNode.NamedChild(i)
		if child.Type() == "type" {
			// Check if preceding sibling token is "->".
			// In tree-sitter Python grammar, the return type is a "type" child
			// that appears after the parameters.
			return normalizePyTypeName(child.Content(src))
		}
	}
	return ""
}

// normalizePyTypeName strips Optional[], List[], etc. and skips builtins.
func normalizePyTypeName(t string) string {
	t = strings.TrimSpace(t)
	// Strip Optional[...], List[...], etc.
	if idx := strings.Index(t, "["); idx > 0 {
		t = t[:idx]
	}
	switch t {
	case "int", "float", "str", "bool", "bytes", "None", "list", "dict", "set", "tuple", "object":
		return ""
	}
	if t == "" || (t[0] >= 'a' && t[0] <= 'z') {
		return ""
	}
	return t
}

// lookupPyImport resolves a dotted Python receiver to an import path.
// Tries the full receiver first, then progressively shorter prefixes
// (os.path → os), so `os.path.join(x)` with `import os.path` finds the
// right module.
func lookupPyImport(receiver string, imports map[string]string) (string, bool) {
	if p, ok := imports[receiver]; ok {
		return p, true
	}
	for i := strings.LastIndex(receiver, "."); i > 0; i = strings.LastIndex(receiver[:i], ".") {
		prefix := receiver[:i]
		if p, ok := imports[prefix]; ok {
			return p, true
		}
	}
	if i := strings.Index(receiver, "."); i > 0 {
		if p, ok := imports[receiver[:i]]; ok {
			return p, true
		}
	}
	return "", false
}
