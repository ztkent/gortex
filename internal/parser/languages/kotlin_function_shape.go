package languages

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitKotlinAsyncSpawns walks a Kotlin function body for coroutine
// builders (`launch`, `async`, `runBlocking`, `withContext`) and for
// `.await()` postfix calls. Mode is "coroutine" for builders,
// "async" for await.
func emitKotlinAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
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
	walkKotlinNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "anonymous_function":
			// Don't descend into nested function bodies. Lambda
			// literals are intentionally walked: Kotlin
			// extractors don't materialise lambdas as graph
			// nodes, so calls inside `launch { compute() }`
			// belong to the enclosing function.
			return false
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				// Some grammar shapes set the callee as the first
				// named child without the field name.
				if n.NamedChildCount() > 0 {
					fn = n.NamedChild(0)
				}
			}
			if fn == nil {
				return true
			}
			name := ""
			switch fn.Type() {
			case "simple_identifier":
				name = fn.Content(src)
			case "navigation_expression":
				// `obj.method` — pick the suffix selector.
				for i := int(fn.NamedChildCount()) - 1; i >= 0; i-- {
					c := fn.NamedChild(i)
					if c == nil {
						continue
					}
					if c.Type() == "navigation_suffix" {
						for j := 0; j < int(c.NamedChildCount()); j++ {
							cc := c.NamedChild(j)
							if cc != nil && cc.Type() == "simple_identifier" {
								name = cc.Content(src)
								break
							}
						}
						break
					}
				}
			}
			if name == "" {
				return true
			}
			line := int(n.StartPoint().Row) + 1
			switch name {
			case "launch", "async", "runBlocking", "withContext", "supervisorScope", "coroutineScope":
				emit(name, "coroutine", line)
			case "await":
				// `someDeferred.await()` — emit a generic await
				// edge; the target is just `await` since we have
				// no deeper resolution without type info.
				emit("await", "async", line)
			}
		}
		return true
	})
}

func walkKotlinNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkKotlinNodes(n.NamedChild(i), visit)
	}
}

// emitKotlinGenericParamNodes emits KindGenericParam nodes plus
// EdgeMemberOf edges for the type_parameters of a Kotlin class /
// interface / function declaration.
func emitKotlinGenericParamNodes(ownerID string, decl *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	if decl == nil {
		return
	}
	tparams := decl.ChildByFieldName("type_parameters")
	if tparams == nil {
		for i := 0; i < int(decl.NamedChildCount()); i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type() == "type_parameters" {
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
			if c != nil && (c.Type() == "type_identifier" || c.Type() == "simple_identifier") && name == "" {
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
			Language:  "kotlin",
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

// kotlinFunctionBody returns the body block of a Kotlin function
// declaration, or nil for abstract / expression-bodied functions
// without explicit braces (those don't have spawn-style calls in
// practice).
func kotlinFunctionBody(funcNode *sitter.Node) *sitter.Node {
	if funcNode == nil {
		return nil
	}
	if b := funcNode.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := 0; i < int(funcNode.NamedChildCount()); i++ {
		c := funcNode.NamedChild(i)
		if c != nil && (c.Type() == "function_body" || c.Type() == "block") {
			return c
		}
	}
	return nil
}
