package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitCSharpFunctionShape emits KindParam/EdgeParamOf/EdgeTypedAs/
// EdgeReturns/KindGenericParam edges for a C# method or constructor
// declaration. Parameter shapes (`Foo Bar`, `Foo Bar = default`,
// `params Foo[] xs`, generic `Foo<Bar> baz`) all flow through this
// helper so DI containers and codegen tooling see dependencies.
func emitCSharpFunctionShape(ownerID string, methodNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if methodNode == nil {
		return
	}
	if params := methodNode.ChildByFieldName("parameters"); params != nil {
		emitCSharpParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := csharpReturnTypeRaw(methodNode, src); rt != "" {
		emitCSharpReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitCSharpGenericParamNodes(ownerID, methodNode, src, filePath, declLine, result)
}

func emitCSharpParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	pos := 0
	for i := 0; i < int(params.NamedChildCount()); i++ {
		decl := params.NamedChild(i)
		if decl == nil {
			continue
		}
		if decl.Type() != "parameter" {
			continue
		}
		var name, typeRaw string
		variadic := false
		if n := decl.ChildByFieldName("name"); n != nil {
			name = n.Content(src)
		}
		if t := decl.ChildByFieldName("type"); t != nil {
			typeRaw = strings.TrimSpace(t.Content(src))
		}
		// Look for `params` modifier — variadic in C#.
		for j := 0; j < int(decl.NamedChildCount()); j++ {
			c := decl.NamedChild(j)
			if c != nil && c.Type() == "parameter_modifier" && strings.Contains(c.Content(src), "params") {
				variadic = true
				break
			}
		}
		if name == "" || name == "_" {
			continue
		}
		paramID := ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
		meta := map[string]any{"position": pos}
		if variadic {
			meta["variadic"] = true
		}
		if typeRaw != "" {
			meta["type"] = typeRaw
		}
		startLine := int(decl.StartPoint().Row) + 1
		if startLine == 0 {
			startLine = declLine
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        paramID,
			Kind:      graph.KindParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   int(decl.EndPoint().Row) + 1,
			Language:  "csharp",
			Meta:      meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     paramID,
			To:       ownerID,
			Kind:     graph.EdgeParamOf,
			FilePath: filePath,
			Line:     startLine,
			Origin:   graph.OriginASTResolved,
		})
		if canon := canonicalizeCSharpTypeRef(typeRaw); canon != "" && !isCSharpPrimitive(canon) {
			result.Edges = append(result.Edges, &graph.Edge{
				From:     paramID,
				To:       "unresolved::" + canon,
				Kind:     graph.EdgeTypedAs,
				FilePath: filePath,
				Line:     startLine,
				Origin:   graph.OriginASTInferred,
			})
		}
		pos++
	}
}

func csharpReturnTypeRaw(methodNode *sitter.Node, src []byte) string {
	// In tree-sitter-c-sharp, method_declaration has a `type` field
	// for the return type. Constructors don't (the type is implicitly
	// the enclosing class).
	if rt := methodNode.ChildByFieldName("type"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	if rt := methodNode.ChildByFieldName("returns"); rt != nil {
		return strings.TrimSpace(rt.Content(src))
	}
	return ""
}

func emitCSharpReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	t := canonicalizeCSharpTypeRef(returnText)
	if t == "" || isCSharpPrimitive(t) {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     ownerID,
		To:       "unresolved::" + t,
		Kind:     graph.EdgeReturns,
		FilePath: filePath,
		Line:     line,
		Origin:   graph.OriginASTInferred,
		Meta: map[string]any{
			"position": 0,
		},
	})
}

func emitCSharpGenericParamNodes(ownerID string, methodNode *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := methodNode.ChildByFieldName("type_parameters")
	if tparams == nil {
		for i := 0; i < int(methodNode.NamedChildCount()); i++ {
			c := methodNode.NamedChild(i)
			if c != nil && c.Type() == "type_parameter_list" {
				tparams = c
				break
			}
		}
	}
	if tparams == nil {
		return
	}
	for i := 0; i < int(tparams.NamedChildCount()); i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		var name string
		for j := 0; j < int(tp.NamedChildCount()); j++ {
			c := tp.NamedChild(j)
			if c != nil && c.Type() == "identifier" {
				name = c.Content(src)
				break
			}
		}
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From:     gpID,
			To:       ownerID,
			Kind:     graph.EdgeMemberOf,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTResolved,
		})
	}
}

func canonicalizeCSharpTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip nullable suffix `?`.
	t = strings.TrimSuffix(t, "?")
	t = strings.TrimSpace(t)
	// Strip array suffix.
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSuffix(t, "[]")
		t = strings.TrimSpace(t)
	}
	// Unwrap container generics.
	for _, wrapper := range []string{
		"Task", "ValueTask", "List", "IList", "IEnumerable",
		"ICollection", "IReadOnlyList", "IReadOnlyCollection",
		"IAsyncEnumerable", "Nullable", "Span", "ReadOnlySpan",
	} {
		prefix := wrapper + "<"
		if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ">") {
			inner := t[len(prefix) : len(t)-1]
			return canonicalizeCSharpTypeRef(inner)
		}
	}
	if idx := strings.Index(t, "<"); idx > 0 {
		t = t[:idx]
	}
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	return strings.TrimSpace(t)
}

func isCSharpPrimitive(t string) bool {
	switch t {
	case "", "void", "bool", "byte", "sbyte", "short", "ushort",
		"int", "uint", "long", "ulong", "float", "double", "decimal",
		"char", "string", "object", "dynamic":
		return true
	}
	return false
}

// emitCSharpAsyncSpawns walks a C# method body for `await` expressions
// and Task.Run / Task.Factory.StartNew / ThreadPool.QueueUserWorkItem
// calls. Mode is "async" for await, "task" for Task.Run, "thread" for
// thread-pool queues.
func emitCSharpAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	seen := map[string]bool{}
	emit := func(target, mode string, line int) {
		if target == "" {
			return
		}
		key := mode + "\x00" + target
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + target,
			Kind:     graph.EdgeSpawns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"mode": mode,
			},
		})
	}
	walkCSharpNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "method_declaration", "lambda_expression", "anonymous_method_expression",
			"local_function_statement":
			return false
		case "await_expression":
			line := int(n.StartPoint().Row) + 1
			// Look for an inner invocation_expression to grab the
			// callee name.
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(i)
				if c == nil {
					continue
				}
				if c.Type() == "invocation_expression" {
					if name := csharpInvocationTargetName(c, src); name != "" {
						emit(name, "async", line)
					}
				}
			}
		case "invocation_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				return true
			}
			line := int(n.StartPoint().Row) + 1
			if fn.Type() == "member_access_expression" {
				expr := fn.ChildByFieldName("expression")
				name := fn.ChildByFieldName("name")
				if expr != nil && name != nil {
					obj := expr.Content(src)
					meth := name.Content(src)
					switch obj {
					case "Task":
						switch meth {
						case "Run", "Factory":
							emit("Task."+meth, "task", line)
						}
					case "Task.Factory":
						if meth == "StartNew" {
							emit("Task.Factory.StartNew", "task", line)
						}
					case "ThreadPool":
						if meth == "QueueUserWorkItem" {
							emit("ThreadPool.QueueUserWorkItem", "thread", line)
						}
					case "Parallel":
						switch meth {
						case "ForEach", "For", "Invoke":
							emit("Parallel."+meth, "parallel", line)
						}
					}
				}
			}
		}
		return true
	})
}

func walkCSharpNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkCSharpNodes(n.NamedChild(i), visit)
	}
}

func csharpInvocationTargetName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "member_access_expression":
		if name := fn.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	case "generic_name":
		if name := fn.ChildByFieldName("name"); name != nil {
			return name.Content(src)
		}
	}
	return ""
}

// csharpFunctionBody returns the body block of a C# method
// declaration. Expression-bodied methods (`=> expr`) have a different
// shape that we don't walk (no spawn-like calls in idiomatic
// expression bodies).
func csharpFunctionBody(methodNode *sitter.Node) *sitter.Node {
	if methodNode == nil {
		return nil
	}
	if b := methodNode.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := 0; i < int(methodNode.NamedChildCount()); i++ {
		c := methodNode.NamedChild(i)
		if c != nil && c.Type() == "block" {
			return c
		}
	}
	return nil
}
