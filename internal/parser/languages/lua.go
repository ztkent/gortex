package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/lua"
)

// LuaExtractor extracts Lua source files.
type LuaExtractor struct {
	lang *sitter.Language
}

func NewLuaExtractor() *LuaExtractor {
	return &LuaExtractor{lang: lua.GetLanguage()}
}

func (e *LuaExtractor) Language() string     { return "lua" }
func (e *LuaExtractor) Extensions() []string { return []string{".lua"} }

func (e *LuaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "lua",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk top-level children. The new tree-sitter-lua grammar emits
	// `function_declaration` for both `function foo()` and
	// `local function foo()` — the `local` prefix is consumed at the
	// chunk level as a field on the child rather than a keyword inside
	// the declaration. Same story for `local x = 1` which becomes a
	// `variable_declaration` that's a `local_declaration` field of the
	// chunk.
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "function_declaration":
			e.extractFunction(child, src, filePath, fileNode, result, seen)

		case "variable_declaration":
			e.extractVariable(child, src, filePath, fileNode, result, seen)

		case "assignment_statement":
			// `M.foo = function() ... end` — emit methods too.
			e.extractAssignmentFunc(child, src, filePath, fileNode, result, seen)

		case "function_call":
			// Top-level calls (require, etc.)
			e.extractTopLevelCall(child, src, filePath, fileNode, result)
		}
	}

	// Call sites inside functions.
	funcRanges := buildFuncRanges(result)
	e.extractCalls(root, src, filePath, result, funcRanges)

	return result, nil
}

// extractFunction handles `function name(...)` / `function M.name(...)`
// / `function M:name(...)` / `local function name(...)`.
// AST: function_declaration { name: identifier | dot_index_expression | method_index_expression,
//                             parameters: parameters,
//                             body: block }
func (e *LuaExtractor) extractFunction(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := ""
	receiver := ""

	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		switch nameNode.Type() {
		case "identifier":
			name = nameNode.Content(src)
		case "dot_index_expression", "method_index_expression":
			full := nameNode.Content(src)
			sep := "."
			if nameNode.Type() == "method_index_expression" {
				sep = ":"
			}
			if idx := strings.Index(full, sep); idx > 0 {
				receiver = strings.TrimSpace(full[:idx])
				name = strings.TrimSpace(full[idx+len(sep):])
			} else {
				name = full
			}
		default:
			name = nameNode.Content(src)
		}
	}

	if name == "" {
		return
	}

	kind := graph.KindFunction
	var id string
	if receiver != "" {
		kind = graph.KindMethod
		id = filePath + "::" + receiver + "." + name
	} else {
		id = filePath + "::" + name
	}

	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	n := &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "lua",
	}
	if receiver != "" {
		n.Meta = map[string]any{"receiver": receiver}
	}

	result.Nodes = append(result.Nodes, n)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})

	if receiver != "" {
		typeID := filePath + "::" + receiver
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: typeID, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	}

	// Extract nested function definitions inside the body.
	walkNodes(node, func(inner *sitter.Node) {
		if inner.Equal(node) || inner.Type() != "function_declaration" {
			return
		}
		e.extractFunction(inner, src, filePath, fileNode, result, seen)
	})
}

// extractVariable handles `local name = value`, represented in the new
// grammar as `variable_declaration → assignment_statement → variable_list`.
func (e *LuaExtractor) extractVariable(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	var assign *sitter.Node
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "assignment_statement" {
			assign = c
			break
		}
	}
	if assign == nil {
		return
	}

	var varList *sitter.Node
	for i := 0; i < int(assign.NamedChildCount()); i++ {
		c := assign.NamedChild(i)
		if c != nil && c.Type() == "variable_list" {
			varList = c
			break
		}
	}
	if varList == nil {
		return
	}

	for i := 0; i < int(varList.NamedChildCount()); i++ {
		ident := varList.NamedChild(i)
		if ident == nil || ident.Type() != "identifier" {
			continue
		}
		name := ident.Content(src)
		if name == "" {
			continue
		}
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
			Language: "lua",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}
}

// extractAssignmentFunc handles `M.foo = function() ... end` at the top
// level — emits a method bound to M. In the new grammar this is an
// `assignment_statement` whose RHS `expression_list` contains a
// `function_definition`.
func (e *LuaExtractor) extractAssignmentFunc(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	var varList, exprList *sitter.Node
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "variable_list":
			varList = c
		case "expression_list":
			exprList = c
		}
	}
	if varList == nil || exprList == nil {
		return
	}
	// Require RHS to be a function_definition (anonymous function literal).
	hasFunc := false
	for i := 0; i < int(exprList.NamedChildCount()); i++ {
		c := exprList.NamedChild(i)
		if c != nil && c.Type() == "function_definition" {
			hasFunc = true
			break
		}
	}
	if !hasFunc {
		return
	}
	// Extract receiver.name from the first LHS target.
	if varList.NamedChildCount() == 0 {
		return
	}
	lhs := varList.NamedChild(0)
	if lhs == nil {
		return
	}
	var name, receiver string
	switch lhs.Type() {
	case "dot_index_expression", "method_index_expression":
		full := lhs.Content(src)
		sep := "."
		if lhs.Type() == "method_index_expression" {
			sep = ":"
		}
		if idx := strings.Index(full, sep); idx > 0 {
			receiver = strings.TrimSpace(full[:idx])
			name = strings.TrimSpace(full[idx+len(sep):])
		}
	case "identifier":
		name = lhs.Content(src)
	default:
		return
	}
	if name == "" {
		return
	}

	kind := graph.KindFunction
	var id string
	if receiver != "" {
		kind = graph.KindMethod
		id = filePath + "::" + receiver + "." + name
	} else {
		id = filePath + "::" + name
	}
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	n := &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "lua",
	}
	if receiver != "" {
		n.Meta = map[string]any{"receiver": receiver}
	}
	result.Nodes = append(result.Nodes, n)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
	if receiver != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + receiver, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	}
}

// extractTopLevelCall handles top-level require() calls as imports.
func (e *LuaExtractor) extractTopLevelCall(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	// Check if it's require("module"). The new grammar labels the
	// callee via the `name` field.
	funcName := ""
	arg := ""
	if fn := node.ChildByFieldName("name"); fn != nil {
		funcName = strings.TrimSpace(fn.Content(src))
	}
	if args := node.ChildByFieldName("arguments"); args != nil {
		for j := 0; j < int(args.NamedChildCount()); j++ {
			argNode := args.NamedChild(j)
			if argNode == nil {
				continue
			}
			if argNode.Type() == "string" {
				arg = strings.Trim(argNode.Content(src), `"'`)
				break
			}
		}
	}

	if funcName == "require" && arg != "" {
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + arg,
			Kind: graph.EdgeImports, FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
		})
	}
}

// extractCalls walks the AST for function_call nodes inside functions.
func (e *LuaExtractor) extractCalls(
	root *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult, funcRanges []funcRange,
) {
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "function_call" {
			return
		}

		line := int(node.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			return
		}

		// The new grammar exposes the callee via the `name` field.
		// Bare identifier → free-function call, dot_index / method_index
		// → method call on a receiver.
		fn := node.ChildByFieldName("name")
		if fn == nil {
			return
		}
		switch fn.Type() {
		case "identifier":
			name := fn.Content(src)
			if name == "require" {
				return // handled separately as import
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: callerID, To: "unresolved::" + name,
				Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			})
		case "dot_index_expression", "method_index_expression":
			text := fn.Content(src)
			sep := "."
			if fn.Type() == "method_index_expression" {
				sep = ":"
			}
			if idx := strings.LastIndex(text, sep); idx > 0 {
				methodName := text[idx+len(sep):]
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::*." + methodName,
					Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
				})
			}
		}
	})
}
