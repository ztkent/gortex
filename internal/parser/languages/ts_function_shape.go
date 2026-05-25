package languages

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitTSFunctionShape emits the function-shape graph projection for a
// TypeScript / JavaScript function-or-method declaration:
//
//   - one KindParam node + EdgeParamOf + EdgeTypedAs per parameter
//   - one EdgeReturns per declared return type
//   - one KindGenericParam node + EdgeMemberOf per type parameter
//
// declNode is the function_declaration / method_definition / arrow
// function (or its public_field_definition wrapper for class-level
// arrow functions). ownerID is the node ID under which params,
// generics, and return edges should be attributed.
func emitTSFunctionShape(ownerID string, declNode *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	if declNode == nil {
		return
	}
	if params := tsParamsList(declNode); params != nil {
		emitTSParamNodes(ownerID, params, src, filePath, declLine, result)
	}
	if rt := tsReturnTypeRaw(declNode, src); rt != "" {
		emitTSReturnEdges(ownerID, rt, filePath, declLine, result)
	}
	emitTSGenericParamNodes(ownerID, declNode, src, filePath, declLine, result)
	if body := tsFunctionBody(declNode); body != nil {
		emitTSAsyncSpawns(ownerID, body, src, filePath, result)
		emitTSFieldAccess(ownerID, body, src, filePath, result)
		// Materialise let / const / var / range / catch bindings as
		// KindLocal nodes — semantic parity with the Go extractor's
		// #77 work. Idempotent on the binding ID (function-relative
		// offset), excluded from BM25 search by shouldIndexForSearch,
		// and consumed by the resolver's scope-aware bare-name bind
		// (#81) for future dataflow / scope-resolution work.
		emitTSLocalBindings(ownerID, declLine, body, src, filePath, result)
	}
}

// emitTSFieldAccess walks a function body and emits EdgeWrites for
// every assignment whose LHS is a member_expression and EdgeReads
// for every member_expression used as a value (selector use, method
// invocation receiver, expression operand). Mirrors the schema rule
// already implemented in golang.go: LHS-of-assignment writes,
// everything else reads. Nested functions are walked too — TS
// extractors don't always materialise inner closures as separate
// graph nodes, so member accesses anywhere in the enclosing
// function attribute back to it.
func emitTSFieldAccess(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
	if body == nil {
		return
	}
	type record struct {
		field string
		op    graph.EdgeKind // EdgeReads | EdgeWrites
		line  int
	}
	seen := map[string]bool{}
	emit := func(r record) {
		if r.field == "" {
			return
		}
		key := string(r.op) + "\x00" + r.field
		if seen[key] {
			return
		}
		seen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::*." + r.field,
			Kind:     r.op,
			FilePath: filePath,
			Line:     r.line,
			Origin:   graph.OriginASTInferred,
		})
	}
	// Track member expressions that appear on the LHS of an
	// assignment so the value-side walker doesn't double-classify
	// them as reads. Keyed by (line, field) — sufficient because
	// an assignment LHS appears once per line per field.
	written := map[string]bool{}
	walkTSNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "method_definition":
			// Top-level lexical sub-functions own their own
			// member access; attributing them to the parent
			// would conflate scopes.
			return false
		case "assignment_expression":
			left := n.ChildByFieldName("left")
			if left == nil {
				return true
			}
			line := int(n.StartPoint().Row) + 1
			if left.Type() == "member_expression" {
				prop := left.ChildByFieldName("property")
				if prop != nil {
					field := prop.Content(src)
					emit(record{field: field, op: graph.EdgeWrites, line: line})
					written[strconv.Itoa(line)+":"+field] = true
				}
			}
		case "augmented_assignment_expression":
			// `x.y += 1` reads + writes; emit both.
			left := n.ChildByFieldName("left")
			line := int(n.StartPoint().Row) + 1
			if left != nil && left.Type() == "member_expression" {
				prop := left.ChildByFieldName("property")
				if prop != nil {
					field := prop.Content(src)
					emit(record{field: field, op: graph.EdgeWrites, line: line})
					emit(record{field: field, op: graph.EdgeReads, line: line})
					written[strconv.Itoa(line)+":"+field] = true
				}
			}
		case "update_expression":
			// `x.y++` / `x.y--` write.
			arg := n.ChildByFieldName("argument")
			line := int(n.StartPoint().Row) + 1
			if arg != nil && arg.Type() == "member_expression" {
				prop := arg.ChildByFieldName("property")
				if prop != nil {
					field := prop.Content(src)
					emit(record{field: field, op: graph.EdgeWrites, line: line})
					written[strconv.Itoa(line)+":"+field] = true
				}
			}
		}
		return true
	})
	walkTSNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "method_definition":
			return false
		case "member_expression":
			// Skip when this expression is the LHS of an
			// assignment we already classified.
			line := int(n.StartPoint().Row) + 1
			prop := n.ChildByFieldName("property")
			if prop == nil {
				return true
			}
			field := prop.Content(src)
			if written[strconv.Itoa(line)+":"+field] {
				return true
			}
			// Skip method-call receivers — those become
			// EdgeCalls via the existing call-emit pass and
			// shouldn't double-count as reads.
			if parent := n.Parent(); parent != nil && parent.Type() == "call_expression" {
				if fn := parent.ChildByFieldName("function"); fn != nil && fn.Equal(n) {
					return true
				}
			}
			emit(record{field: field, op: graph.EdgeReads, line: line})
		}
		return true
	})
}

// emitTSAsyncSpawns walks a function body and emits EdgeSpawns for
// every awaited call (`await foo()`, `await this.svc.load()`) and
// every Promise constructor / Promise.all / Promise.then dispatch.
// Mode is "async" for await_expression, "promise" for Promise.x.
//
// Nested function/arrow bodies are skipped — their awaits belong to
// the inner scope; the owning emitFunction/emitArrow pass picks
// them up directly.
func emitTSAsyncSpawns(ownerID string, body *sitter.Node, src []byte, filePath string, result *parser.ExtractionResult) {
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
	walkTSNodes(body, func(n *sitter.Node) bool {
		switch n.Type() {
		case "function_declaration", "function_expression", "arrow_function",
			"method_definition", "generator_function", "generator_function_declaration":
			// Don't descend into nested function bodies.
			return false
		case "await_expression":
			if call := tsFindCallExpression(n); call != nil {
				if name := tsCallTargetName(call, src); name != "" {
					emit(name, "async", int(n.StartPoint().Row)+1)
				}
			}
			return true
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil {
				return true
			}
			// Promise.all / Promise.allSettled / Promise.race —
			// walk the first argument's array elements (each is a
			// call_expression we should attribute to). We only
			// emit a coarse "Promise.all" target so traversals
			// can highlight the dispatch site even when arg
			// resolution is too dynamic to track.
			if fn.Type() == "member_expression" {
				obj := fn.ChildByFieldName("object")
				prop := fn.ChildByFieldName("property")
				if obj != nil && prop != nil {
					if obj.Content(src) == "Promise" {
						emit("Promise."+prop.Content(src), "promise", int(n.StartPoint().Row)+1)
					}
				}
			}
		}
		return true
	})
}

// walkTSNodes is a TS analogue to walkGoNodes: pre-order, returning
// false from visit skips the subtree.
func walkTSNodes(n *sitter.Node, visit func(*sitter.Node) bool) {
	if n == nil {
		return
	}
	if !visit(n) {
		return
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkTSNodes(n.NamedChild(i), visit)
	}
}

func tsFindCallExpression(n *sitter.Node) *sitter.Node {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "call_expression" {
			return c
		}
	}
	return nil
}

// tsCallTargetName extracts the textual function name of a TS call
// expression. Returns "" when the call is too dynamic (e.g. an IIFE
// or a higher-order call result).
func tsCallTargetName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "member_expression":
		// e.g. svc.load, this.repo.find — return the property name
		// so the resolver can land it via the EdgeReads/EdgeWrites
		// receiver-type fallback.
		if prop := fn.ChildByFieldName("property"); prop != nil {
			return prop.Content(src)
		}
	}
	return ""
}

// tsReturnTypeRaw returns the verbatim source of a function/method's
// return type annotation, without the upstream normalization that
// strips generics and primitives. Returns "" when there's no
// annotation.
func tsReturnTypeRaw(decl *sitter.Node, src []byte) string {
	if decl == nil {
		return ""
	}
	for i := 0; i < int(decl.NamedChildCount()); i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Type() != "type_annotation" {
			continue
		}
		if c.NamedChildCount() > 0 {
			tn := c.NamedChild(0)
			if tn != nil {
				return strings.TrimSpace(tn.Content(src))
			}
		}
	}
	return ""
}

// tsParamName returns the parameter's bound identifier, descending
// into rest_pattern / object_pattern so destructured + variadic
// parameters still surface a name. Returns "" when no simple
// identifier is available (deep destructuring like `{a: {b}}` —
// not a single binding so we drop them).
func tsParamName(p *sitter.Node, src []byte) string {
	if p == nil {
		return ""
	}
	pattern := p.ChildByFieldName("pattern")
	if pattern != nil {
		switch pattern.Type() {
		case "identifier":
			return pattern.Content(src)
		case "rest_pattern":
			// rest_pattern wraps an identifier child.
			for i := 0; i < int(pattern.NamedChildCount()); i++ {
				c := pattern.NamedChild(i)
				if c != nil && c.Type() == "identifier" {
					return c.Content(src)
				}
			}
		}
	}
	// Fallback: scan named children for an identifier (older grammar
	// shapes don't always set the pattern field).
	for i := 0; i < int(p.NamedChildCount()); i++ {
		c := p.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "identifier" {
			return c.Content(src)
		}
		if c.Type() == "rest_pattern" {
			for j := 0; j < int(c.NamedChildCount()); j++ {
				cc := c.NamedChild(j)
				if cc != nil && cc.Type() == "identifier" {
					return cc.Content(src)
				}
			}
		}
	}
	return ""
}

// tsParamTypeRaw returns the verbatim source of a parameter's type
// annotation, without the upstream normalization that strips generics
// and primitives.
func tsParamTypeRaw(p *sitter.Node, src []byte) string {
	if p == nil {
		return ""
	}
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
		return strings.TrimSpace(c.Content(src))
	}
	return ""
}

// tsParamsList returns the formal parameter list child of a TS / JS
// function-shaped node. Function/method/arrow nodes use field name
// "parameters". Returns nil when missing.
func tsParamsList(decl *sitter.Node) *sitter.Node {
	if decl == nil {
		return nil
	}
	if p := decl.ChildByFieldName("parameters"); p != nil {
		return p
	}
	// Some grammar shapes use a formal_parameters child directly
	// without a field name.
	for i := 0; i < int(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if c != nil && (c.Type() == "formal_parameters" || c.Type() == "call_signature") {
			return c
		}
	}
	return nil
}

// emitTSParamNodes walks formal parameters and emits one KindParam
// node per name plus EdgeParamOf and (when the type annotation is
// present) EdgeTypedAs.
func emitTSParamNodes(ownerID string, params *sitter.Node, src []byte, filePath string, declLine int, result *parser.ExtractionResult) {
	pos := 0
	for i := 0; i < int(params.NamedChildCount()); i++ {
		decl := params.NamedChild(i)
		if decl == nil {
			continue
		}
		t := decl.Type()
		switch t {
		case "required_parameter", "optional_parameter":
			// fall through
		default:
			continue
		}
		isVariadic := false
		// `...rest: T` is parsed as required_parameter pattern: rest_pattern.
		if pat := decl.ChildByFieldName("pattern"); pat != nil && pat.Type() == "rest_pattern" {
			isVariadic = true
		}
		name := tsParamName(decl, src)
		if name == "" || name == "_" {
			continue
		}
		typeName := tsParamTypeRaw(decl, src)
		paramID := tsParamNodeID(ownerID, name, pos)
		meta := map[string]any{"position": pos}
		if isVariadic {
			meta["variadic"] = true
		}
		if typeName != "" {
			meta["type"] = typeName
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
			Language:  "typescript",
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
		if canon := canonicalizeTSTypeRef(typeName); canon != "" && !isTSPrimitive(canon) {
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

// emitTSReturnEdges parses the source of a return-type annotation
// and emits an EdgeReturns per (top-level) type. Union types
// (`A | B`) emit one edge per branch so traversals can find every
// possible runtime return type.
func emitTSReturnEdges(ownerID, returnText, filePath string, line int, result *parser.ExtractionResult) {
	if returnText == "" {
		return
	}
	for i, raw := range splitTSUnionType(returnText) {
		t := canonicalizeTSTypeRef(raw)
		if t == "" || isTSPrimitive(t) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     ownerID,
			To:       "unresolved::" + t,
			Kind:     graph.EdgeReturns,
			FilePath: filePath,
			Line:     line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"position": i,
			},
		})
	}
}

// emitTSGenericParamNodes turns a TS function/class declaration's
// type_parameters into KindGenericParam nodes plus EdgeMemberOf back
// to the owner. Constraints and defaults are stored as meta.bound /
// meta.default for downstream queries.
func emitTSGenericParamNodes(ownerID string, decl *sitter.Node, src []byte, filePath string, line int, result *parser.ExtractionResult) {
	tparams := tsTypeParams(decl, src)
	if len(tparams) == 0 {
		return
	}
	for _, tp := range tparams {
		name := tp["name"]
		if name == "" {
			continue
		}
		gpID := ownerID + "#tparam:" + name
		meta := map[string]any{}
		if b := tp["bound"]; b != "" {
			meta["bound"] = b
		}
		if d := tp["default"]; d != "" {
			meta["default"] = d
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID:        gpID,
			Kind:      graph.KindGenericParam,
			Name:      name,
			FilePath:  filePath,
			StartLine: line,
			EndLine:   line,
			Language:  "typescript",
			Meta:      meta,
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

// tsParamNodeID builds the unique-per-owner ID for a parameter
// node. Mirrors goParamNodeID.
func tsParamNodeID(ownerID, name string, pos int) string {
	return ownerID + "#param:" + name + "@" + strconv.Itoa(pos)
}

// canonicalizeTSTypeRef strips wrapping noise (Promise<X>, Array<X>,
// X[], readonly X) so the resolver can match the declared type to a
// type node defined in the workspace.
func canonicalizeTSTypeRef(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	// Strip leading colon if the caller didn't already.
	t = strings.TrimPrefix(t, ":")
	t = strings.TrimSpace(t)
	// Strip readonly.
	t = strings.TrimPrefix(t, "readonly ")
	// Recurse-strip generic wrappers we know are pass-through:
	// Promise<T>, Array<T>, ReadonlyArray<T>, Awaited<T>.
	for _, wrapper := range []string{"Promise", "Array", "ReadonlyArray", "Awaited"} {
		if strings.HasPrefix(t, wrapper+"<") && strings.HasSuffix(t, ">") {
			inner := t[len(wrapper)+1 : len(t)-1]
			return canonicalizeTSTypeRef(inner)
		}
	}
	// Strip array suffix.
	for strings.HasSuffix(t, "[]") {
		t = strings.TrimSuffix(t, "[]")
		t = strings.TrimSpace(t)
	}
	// Strip surrounding parens.
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	return t
}

// splitTSUnionType splits a TypeScript type string at top-level `|`
// boundaries (respecting <…>, (…), {…} nesting).
func splitTSUnionType(t string) []string {
	t = strings.TrimSpace(t)
	if t == "" {
		return nil
	}
	t = strings.TrimPrefix(t, ":")
	t = strings.TrimSpace(t)
	depth := 0
	parts := []string{}
	cur := strings.Builder{}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch c {
		case '<', '(', '{', '[':
			depth++
		case '>', ')', '}', ']':
			depth--
		case '|':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		parts = append(parts, strings.TrimSpace(cur.String()))
	}
	return parts
}

// isTSPrimitive returns true when t names a TypeScript builtin / DOM
// primitive that doesn't need an EdgeReturns target — emitting these
// would just clutter the graph with unresolved::string / unresolved::
// number edges that never land.
func isTSPrimitive(t string) bool {
	switch t {
	case "", "void", "any", "unknown", "never", "null", "undefined",
		"string", "number", "boolean", "bigint", "symbol", "object",
		"this", "true", "false":
		return true
	}
	return false
}
