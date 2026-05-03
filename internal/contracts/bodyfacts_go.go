package contracts

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

func init() {
	RegisterBodyFactsFactory("go", newGoBodyFacts)
}

// goBodyFacts is the Go implementation of BodyFacts. It walks the
// handler's subtree once on construction and populates per-binding,
// per-response-call, and per-query-read tables. Subsequent method
// calls are O(1) map lookups.
//
// Subsumes (and replaces in phase 1):
//   - findVarType / typeOfInlineExpr / readTypeIdent / hasComposite /
//     splitMapLiteralBody / splitTopLevel in schema_enrich_go.go
//   - goCallBindRe / responseHelperCallRe / bindLiteralRegexes /
//     bindLiteralTypeFromBody / traceVarTypeFromBody in indexer.go
type goBodyFacts struct {
	tree     *parser.ParseTree
	src      []byte
	handler  *graph.Node
	body     *sitter.Node // function_declaration / method_declaration body block

	bindings        map[string]Binding
	responseCalls   []ResponseCall
	statusWrites    []int
	queryReads      []string
	requestBindings []RequestBinding
}

// newGoBodyFacts is the factory registered for language "go".
func newGoBodyFacts(tree *parser.ParseTree, handler *graph.Node) BodyFacts {
	if tree == nil || tree.Tree() == nil || handler == nil {
		return nopBodyFacts{}
	}
	bf := &goBodyFacts{
		tree:     tree,
		src:      tree.Source(),
		handler:  handler,
		bindings: map[string]Binding{},
	}
	bf.body = bf.findHandlerBody()
	if bf.body == nil {
		return bf
	}
	bf.walk(bf.body)
	return bf
}

// findHandlerBody locates the function_declaration or method_declaration
// node corresponding to the handler graph node, then returns its
// body block. Two-pronged lookup:
//   1. Exact line match — handler.StartLine - 1 == node start row
//      (works when the handler node was produced by the language
//      extractor in the same indexing pass).
//   2. Name match — node's name field matches handler.Name (works
//      when the handler graph node was hand-built with a slightly
//      off StartLine, e.g. test fixtures).
func (bf *goBodyFacts) findHandlerBody() *sitter.Node {
	root := bf.tree.Tree().RootNode()
	if root == nil {
		return nil
	}
	target := bf.handler.StartLine - 1
	if body := findGoFuncBodyAt(root, target); body != nil {
		return body
	}
	if bf.handler.Name == "" {
		return nil
	}
	return findGoFuncBodyByName(root, bf.handler.Name, bf.src)
}

func findGoFuncBodyAt(node *sitter.Node, targetRow int) *sitter.Node {
	if node == nil {
		return nil
	}
	kind := node.Type()
	if kind == "function_declaration" || kind == "method_declaration" || kind == "func_literal" {
		startRow := int(node.StartPoint().Row)
		if startRow == targetRow {
			body := node.ChildByFieldName("body")
			if body != nil {
				return body
			}
		}
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		ch := node.NamedChild(i)
		if ch == nil {
			continue
		}
		// Don't descend through nodes whose span doesn't contain the
		// target row — saves a lot of work on big files.
		if int(ch.StartPoint().Row) > targetRow || int(ch.EndPoint().Row) < targetRow {
			continue
		}
		if found := findGoFuncBodyAt(ch, targetRow); found != nil {
			return found
		}
	}
	return nil
}

func findGoFuncBodyByName(node *sitter.Node, name string, src []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	kind := node.Type()
	if kind == "function_declaration" || kind == "method_declaration" {
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil && nameNode.Content(src) == name {
			body := node.ChildByFieldName("body")
			if body != nil {
				return body
			}
		}
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		ch := node.NamedChild(i)
		if ch == nil {
			continue
		}
		if found := findGoFuncBodyByName(ch, name, src); found != nil {
			return found
		}
	}
	return nil
}

// walk traverses the body subtree, recording bindings, response
// calls, status writes, and query reads. We only descend into nodes
// that can contain expressions; this avoids querying every leaf
// token.
func (bf *goBodyFacts) walk(node *sitter.Node) {
	if node == nil {
		return
	}
	switch node.Type() {
	case "short_var_declaration":
		bf.recordShortVarDecl(node)
	case "assignment_statement":
		bf.recordAssignment(node)
	case "var_spec":
		bf.recordVarSpec(node)
	case "call_expression":
		bf.recordCall(node)
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		ch := node.NamedChild(i)
		if ch == nil {
			continue
		}
		bf.walk(ch)
	}
}

// recordShortVarDecl handles `x := expr` and `x, y := expr1, expr2`.
// Tree-sitter Go: short_var_declaration has `left: expression_list`
// and `right: expression_list`.
func (bf *goBodyFacts) recordShortVarDecl(node *sitter.Node) {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	bf.recordVarsFromLists(left, right)
}

// recordAssignment handles `x = expr`. Only operator `=` matters; we
// skip `+=`/`-=` etc. since those don't introduce a binding.
func (bf *goBodyFacts) recordAssignment(node *sitter.Node) {
	// The 2nd child of an assignment_statement is the operator token
	// ("=", "+=", etc.). Tree-sitter exposes it as an unnamed child.
	op := ""
	for i := 0; i < int(node.ChildCount()); i++ {
		ch := node.Child(i)
		if ch != nil && !ch.IsNamed() {
			op = ch.Type()
			break
		}
	}
	if op != "=" {
		return
	}
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	// Only record the assignment if no prior binding exists for the
	// LHS — handlers commonly do `var x Foo` then `x = bar()`. The
	// `var` binding already captured x's declared type; the
	// assignment's RHS just confirms or overrides.
	bf.recordVarsFromLists(left, right)
}

// recordVarSpec handles `var x Foo` and `var x = expr`.
func (bf *goBodyFacts) recordVarSpec(node *sitter.Node) {
	name := node.ChildByFieldName("name")
	if name == nil {
		return
	}
	typ := node.ChildByFieldName("type")
	val := node.ChildByFieldName("value")

	// Multi-name `var a, b Foo` exposes all names under the "name"
	// field as identifier siblings via NamedChild iteration; we walk
	// every identifier child of the name slot.
	names := collectIdentifiers(node, "name")
	for _, n := range names {
		nameTxt := n.Content(bf.src)
		if nameTxt == "" || nameTxt == "_" {
			continue
		}
		switch {
		case typ != nil:
			b := bf.bindingFromTypeNode(typ)
			b.RawExpr = strings.TrimSpace(typ.Content(bf.src))
			bf.set(nameTxt, b)
		case val != nil:
			// `var x = expr` — single name supported here; multi-name
			// var with multi-value initialiser is rare in handlers.
			rhs := firstExpressionInList(val)
			if rhs != nil {
				bf.set(nameTxt, bf.bindingFromRHS(rhs))
			}
		}
	}
}

// recordVarsFromLists pairs each LHS identifier with the matching
// RHS expression and records a binding. Skips blank identifiers and
// LHS positions that aren't simple identifiers (selectors, indexes —
// those don't introduce locals).
func (bf *goBodyFacts) recordVarsFromLists(left, right *sitter.Node) {
	lefts := namedChildren(left)
	rights := namedChildren(right)
	if len(lefts) == 0 {
		return
	}

	// Multi-return: `a, b := f()` — every LHS gets the same call's
	// type info but different return-position. We only handle the
	// first non-error return today; the rest get a method_call
	// binding without a resolved type so the caller can still walk
	// the graph.
	if len(rights) == 1 {
		rhs := rights[0]
		shared := bf.bindingFromRHS(rhs)
		for i, l := range lefts {
			ident := identifierName(l, bf.src)
			if ident == "" || ident == "_" {
				continue
			}
			b := shared
			if i > 0 && shared.Kind == BindingMethodCall {
				// Other return positions: keep CallExpr, drop the
				// type — we don't know which return slot maps to
				// which type without signature parsing.
				b.TypeID = ""
				b.Repeated = false
				b.Pointer = false
			}
			bf.set(ident, b)
		}
		return
	}

	// Pairwise: `a, b := f(), g()` — each LHS gets its own RHS.
	for i, l := range lefts {
		if i >= len(rights) {
			break
		}
		ident := identifierName(l, bf.src)
		if ident == "" || ident == "_" {
			continue
		}
		bf.set(ident, bf.bindingFromRHS(rights[i]))
	}
}

// set records a binding only if the name doesn't already have one,
// preserving first-write semantics: `var x Foo; x = bar()` keeps the
// `Foo` type, matching how findVarType worked.
func (bf *goBodyFacts) set(name string, b Binding) {
	if name == "" || b.Kind == BindingUnknown {
		return
	}
	if _, exists := bf.bindings[name]; exists {
		return
	}
	bf.bindings[name] = b
}

// bindingFromRHS classifies an RHS expression node. Strips leading
// `&`/`*` and falls through to the underlying expression. Stamps
// the source line so the BindingResolver upgrade tier can ask
// go/types for the resolved type at the same position.
func (bf *goBodyFacts) bindingFromRHS(node *sitter.Node) Binding {
	if node == nil {
		return Binding{}
	}
	line := int(node.StartPoint().Row) + 1
	raw := strings.TrimSpace(node.Content(bf.src))
	pointer := false

	// Unwrap `&expr` / `*expr` for type detection while remembering
	// the pointer flag so the caller can render `*Foo`.
	cur := node
	for cur != nil && cur.Type() == "unary_expression" {
		op := ""
		for i := 0; i < int(cur.ChildCount()); i++ {
			ch := cur.Child(i)
			if ch != nil && !ch.IsNamed() {
				op = ch.Type()
				break
			}
		}
		if op != "&" && op != "*" {
			break
		}
		if op == "&" {
			pointer = true
		}
		operand := cur.ChildByFieldName("operand")
		if operand == nil {
			break
		}
		cur = operand
	}

	switch cur.Type() {
	case "composite_literal":
		b := bf.bindingFromComposite(cur)
		b.Pointer = b.Pointer || pointer
		b.RawExpr = raw
		b.Line = line
		return b
	case "call_expression":
		b := bf.bindingFromCallExpr(cur)
		b.Pointer = b.Pointer || pointer
		b.RawExpr = raw
		b.Line = line
		return b
	case "interpreted_string_literal", "raw_string_literal":
		return Binding{Kind: BindingStringLit, TypeID: "string", Origin: OriginASTInferred, RawExpr: raw, Line: line}
	case "int_literal":
		return Binding{Kind: BindingIntLit, TypeID: "int", Origin: OriginASTInferred, RawExpr: raw, Line: line}
	case "float_literal":
		return Binding{Kind: BindingFloatLit, TypeID: "float64", Origin: OriginASTInferred, RawExpr: raw, Line: line}
	case "true", "false":
		return Binding{Kind: BindingBoolLit, TypeID: "bool", Origin: OriginASTInferred, RawExpr: raw, Line: line}
	case "identifier":
		// `x := y` — pass-through; no new fact, but record the raw
		// text so callers can chase y separately.
		return Binding{Kind: BindingUnknown, RawExpr: raw, Line: line}
	}
	return Binding{Kind: BindingUnknown, RawExpr: raw, Origin: OriginASTInferred, Line: line}
}

// bindingFromTypeNode handles `var x Foo` style: the type slot
// directly carries the declared type. Returns a Binding whose Kind
// is BindingComposite (matches downstream resolveType callers) with
// TypeID set from the unwrapped type's name.
func (bf *goBodyFacts) bindingFromTypeNode(typ *sitter.Node) Binding {
	if typ == nil {
		return Binding{}
	}
	pointer := false
	repeated := false

	cur := typ
	if cur.Type() == "pointer_type" {
		pointer = true
		if inner := pointerInner(cur); inner != nil {
			cur = inner
		}
	}
	if cur.Type() == "slice_type" || cur.Type() == "array_type" {
		repeated = true
		if elem := sliceElementNode(cur); elem != nil {
			cur = elem
		}
	}
	if cur.Type() == "pointer_type" {
		pointer = true
		if inner := pointerInner(cur); inner != nil {
			cur = inner
		}
	}
	name := unqualify(strings.TrimSpace(cur.Content(bf.src)))
	if name == "" {
		return Binding{}
	}
	return Binding{
		Kind:     BindingComposite,
		TypeID:   name,
		Repeated: repeated,
		Pointer:  pointer,
		Origin:   OriginASTInferred,
	}
}

// bindingFromComposite classifies a composite_literal: `Foo{}`,
// `[]Foo{}`, `map[K]V{}`. The type is the first named child of the
// composite_literal (tree-sitter Go doesn't field-name it "type" in
// the v0.25 grammar).
func (bf *goBodyFacts) bindingFromComposite(node *sitter.Node) Binding {
	typ := compositeTypeNode(node)
	if typ == nil {
		return Binding{Kind: BindingComposite, Origin: OriginASTInferred}
	}
	switch typ.Type() {
	case "slice_type", "array_type":
		elem := sliceElementNode(typ)
		if elem == nil {
			return Binding{Kind: BindingSliceLit, Repeated: true, Origin: OriginASTInferred}
		}
		elemPointer := false
		if elem.Type() == "pointer_type" {
			elemPointer = true
			if inner := pointerInner(elem); inner != nil {
				elem = inner
			}
		}
		name := unqualify(strings.TrimSpace(elem.Content(bf.src)))
		return Binding{
			Kind:     BindingSliceLit,
			TypeID:   name,
			Repeated: true,
			Pointer:  elemPointer,
			Origin:   OriginASTInferred,
		}
	case "map_type":
		// Map literals carry their full type as TypeID so the
		// dashboard can render `map[string]int` verbatim.
		full := strings.TrimSpace(typ.Content(bf.src))
		return Binding{Kind: BindingMapLit, TypeID: full, Origin: OriginASTInferred}
	default:
		// Named or qualified type: `Foo`, `pkg.Foo`, `Foo[T]`, etc.
		// generic_type wraps a base type with type arguments.
		cur := typ
		if cur.Type() == "generic_type" {
			if base := firstNamedChild(cur); base != nil {
				cur = base
			}
		}
		name := unqualify(strings.TrimSpace(cur.Content(bf.src)))
		return Binding{Kind: BindingComposite, TypeID: name, Origin: OriginASTInferred}
	}
}

// bindingFromCallExpr classifies a call. Recognises:
//   - `r.PathValue("id")` / `r.FormValue("id")` / `r.PostFormValue("id")` → string
//   - `r.URL.Query().Get("k")` → string
//   - `r.Header.Get("X-Foo")` → string
//   - `make([]Foo, ...)` → slice of Foo
//   - `make(map[K]V, ...)` → map type
//   - `h.svc.GetRepos(...)` / `pkg.Func(...)` → method/func call (CallExpr set)
func (bf *goBodyFacts) bindingFromCallExpr(node *sitter.Node) Binding {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return Binding{Kind: BindingFuncCall, Origin: OriginASTInferred}
	}
	args := node.ChildByFieldName("arguments")

	// `make(...)` is a builtin call_expression with function = identifier "make".
	if fn.Type() == "identifier" && fn.Content(bf.src) == "make" && args != nil {
		return bf.bindingFromMakeCall(args)
	}

	switch fn.Type() {
	case "selector_expression":
		method := fn.ChildByFieldName("field")
		if method == nil {
			return Binding{Kind: BindingMethodCall, CallExpr: fn.Content(bf.src), Origin: OriginASTInferred}
		}
		methodName := method.Content(bf.src)

		// net/http accessors that return string, regardless of the
		// receiver text — the receiver is `r`, `req`, etc., but the
		// method-name match is sufficient.
		switch methodName {
		case "PathValue":
			return Binding{Kind: BindingPathValue, TypeID: "string", Origin: OriginASTInferred, CallExpr: fn.Content(bf.src)}
		case "FormValue", "PostFormValue":
			return Binding{Kind: BindingFormValue, TypeID: "string", Origin: OriginASTInferred, CallExpr: fn.Content(bf.src)}
		case "Get":
			// Disambiguate Header.Get vs URL.Query().Get vs
			// other methods named Get. Walk the receiver chain.
			recv := fn.ChildByFieldName("operand")
			if recv != nil {
				rt := strings.TrimSpace(recv.Content(bf.src))
				switch {
				case strings.HasSuffix(rt, ".Header"):
					return Binding{Kind: BindingHeaderValue, TypeID: "string", Origin: OriginASTInferred, CallExpr: fn.Content(bf.src)}
				case strings.HasSuffix(rt, ".URL.Query()"), strings.HasSuffix(rt, ".Query()"):
					return Binding{Kind: BindingQueryGet, TypeID: "string", Origin: OriginASTInferred, CallExpr: fn.Content(bf.src)}
				}
			}
		}

		return Binding{
			Kind:     BindingMethodCall,
			CallExpr: fn.Content(bf.src),
			Origin:   OriginASTInferred,
		}
	case "identifier":
		return Binding{Kind: BindingFuncCall, CallExpr: fn.Content(bf.src), Origin: OriginASTInferred}
	}
	return Binding{Kind: BindingFuncCall, CallExpr: fn.Content(bf.src), Origin: OriginASTInferred}
}

// bindingFromMakeCall handles `make([]Foo, n)` and `make(map[K]V, n)`.
// First argument is the type expression.
func (bf *goBodyFacts) bindingFromMakeCall(args *sitter.Node) Binding {
	if args == nil {
		return Binding{Kind: BindingFuncCall, CallExpr: "make", Origin: OriginASTInferred}
	}
	first := firstNamedChild(args)
	if first == nil {
		return Binding{Kind: BindingFuncCall, CallExpr: "make", Origin: OriginASTInferred}
	}
	switch first.Type() {
	case "slice_type", "array_type":
		elem := sliceElementNode(first)
		if elem == nil {
			return Binding{Kind: BindingMakeSlice, Repeated: true, Origin: OriginASTInferred}
		}
		elemPointer := false
		if elem.Type() == "pointer_type" {
			elemPointer = true
			if inner := pointerInner(elem); inner != nil {
				elem = inner
			}
		}
		name := unqualify(strings.TrimSpace(elem.Content(bf.src)))
		return Binding{
			Kind:     BindingMakeSlice,
			TypeID:   name,
			Repeated: true,
			Pointer:  elemPointer,
			Origin:   OriginASTInferred,
		}
	case "map_type":
		full := strings.TrimSpace(first.Content(bf.src))
		return Binding{Kind: BindingMakeMap, TypeID: full, Origin: OriginASTInferred}
	}
	return Binding{Kind: BindingFuncCall, CallExpr: "make", Origin: OriginASTInferred}
}

// recordCall is invoked for every call_expression in the body. It
// recognises response helpers (WriteJSON / respondJSON / Encode /
// c.JSON), WriteHeader status writes, and query accessors — and
// stamps them into the per-handler tables.
func (bf *goBodyFacts) recordCall(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return
	}
	args := node.ChildByFieldName("arguments")
	helper := ""
	switch fn.Type() {
	case "identifier":
		helper = fn.Content(bf.src)
	case "selector_expression":
		field := fn.ChildByFieldName("field")
		if field != nil {
			helper = field.Content(bf.src)
		}
	}
	if helper == "" {
		return
	}

	// json.NewEncoder(w).Encode(value) — chained call with helper
	// = "Encode" and a receiver that's itself a call to NewEncoder.
	// We treat any "Encode" call whose receiver name contains
	// NewEncoder as a response helper.
	switch {
	case isRequestBindingHelper(helper, fn, bf.src):
		bf.recordRequestBinding(node, helper, args)
	case isJSONResponseHelper(helper):
		// First-arg sniff: status helper signatures are (w, code,
		// value) or (w, value); the current contract of
		// EnrichHTTPContract assumes (w, code, value) for WriteJSON
		// family and (value) for Encode. Distinguish by helper.
		bf.recordResponseCall(node, helper, args)
	case helper == "WriteHeader":
		bf.recordStatusWrite(args)
	case isQueryAccessor(fn, bf.src):
		if k := firstStringLiteralArg(args, bf.src); k != "" {
			bf.queryReads = append(bf.queryReads, k)
		}
	}
}

// recordRequestBinding extracts the bound variable from a request
// binding call. Recognises the two common shapes:
//
//	Decode(&req)          → VarName = "req"
//	BodyParser(&req)      → VarName = "req"
//	ShouldBindJSON(&req)  → VarName = "req"
//	Bind(&req)            → VarName = "req"
//	Unmarshal(body, &req) → VarName = "req"  (var is the LAST arg)
//	Decode(&Foo{})        → CompositeType = "Foo" (anonymous bind)
func (bf *goBodyFacts) recordRequestBinding(call *sitter.Node, helper string, args *sitter.Node) {
	if args == nil {
		return
	}
	argList := namedChildren(args)
	if len(argList) == 0 {
		return
	}
	// The variable / composite is the LAST argument for Unmarshal,
	// FIRST for everything else.
	target := argList[0]
	if helper == "Unmarshal" && len(argList) >= 2 {
		target = argList[len(argList)-1]
	}
	rb := RequestBinding{Helper: helper, Line: int(call.StartPoint().Row) + 1}

	cur := target
	if cur.Type() == "unary_expression" {
		// Strip leading & or *.
		op := cur.ChildByFieldName("operand")
		if op != nil {
			cur = op
		}
	}
	switch cur.Type() {
	case "identifier":
		rb.VarName = cur.Content(bf.src)
	case "composite_literal":
		typ := compositeTypeNode(cur)
		if typ != nil {
			rb.CompositeType = unqualify(strings.TrimSpace(typ.Content(bf.src)))
		}
	}
	if rb.VarName == "" && rb.CompositeType == "" {
		return
	}
	bf.requestBindings = append(bf.requestBindings, rb)
}

// isRequestBindingHelper reports whether a call helper is one of the
// known request-binding methods. fn is the call's function expression
// (used to disambiguate Bind: c.Bind is binding, but generic fn.Bind
// might be something else).
//
// Includes Marshal/MarshalIndent (consumer-side: the value being
// serialized to a request body) — the AST overlay distinguishes
// server vs consumer semantics by Contract.Role, then routes the
// helper to request_type or response_type accordingly.
func isRequestBindingHelper(name string, fn *sitter.Node, src []byte) bool {
	switch name {
	case "Decode", "BodyParser", "ShouldBindJSON", "ShouldBindXML",
		"ShouldBindYAML", "ShouldBind", "BindJSON", "BindXML", "BindYAML",
		"Unmarshal", "Marshal", "MarshalIndent":
		return true
	case "Bind":
		// echo's c.Bind(&req) is a binding helper. Rule out generic
		// fn.Bind(args) by checking the receiver looks like a request
		// context: identifier "c" or "ctx".
		if fn == nil || fn.Type() != "selector_expression" {
			return false
		}
		recv := fn.ChildByFieldName("operand")
		if recv == nil || recv.Type() != "identifier" {
			return false
		}
		switch recv.Content(src) {
		case "c", "ctx":
			return true
		}
		return false
	}
	return false
}

// recordResponseCall stamps a ResponseCall entry, recognising the
// (w, code, value) and (value) shapes.
func (bf *goBodyFacts) recordResponseCall(callNode *sitter.Node, helper string, args *sitter.Node) {
	rc := ResponseCall{Helper: helper, Line: int(callNode.StartPoint().Row) + 1}
	if args == nil {
		bf.responseCalls = append(bf.responseCalls, rc)
		return
	}
	argList := namedChildren(args)
	switch helper {
	case "Encode":
		// json.NewEncoder(w).Encode(value)
		if len(argList) >= 1 {
			rc.ValueArg = NewNode(argList[0], bf.src)
			rc.ValueExpr = strings.TrimSpace(argList[0].Content(bf.src))
		}
	case "JSON":
		// gin/fiber: c.JSON(code, value)
		if len(argList) >= 2 {
			rc.StatusArg = NewNode(argList[0], bf.src)
			rc.StatusCode, rc.StatusKnown = bf.resolveStatusCode(argList[0])
			rc.ValueArg = NewNode(argList[1], bf.src)
			rc.ValueExpr = strings.TrimSpace(argList[1].Content(bf.src))
		} else if len(argList) == 1 {
			// fiber: .JSON(value) — status defaults to 200
			rc.ValueArg = NewNode(argList[0], bf.src)
			rc.ValueExpr = strings.TrimSpace(argList[0].Content(bf.src))
		}
	default:
		// WriteJSON / respondJSON / sendJSON / renderJSON — (w, code, value)
		if len(argList) >= 3 {
			rc.StatusArg = NewNode(argList[1], bf.src)
			rc.StatusCode, rc.StatusKnown = bf.resolveStatusCode(argList[1])
			rc.ValueArg = NewNode(argList[2], bf.src)
			rc.ValueExpr = strings.TrimSpace(argList[2].Content(bf.src))
		} else if len(argList) == 2 {
			rc.StatusArg = NewNode(argList[0], bf.src)
			rc.StatusCode, rc.StatusKnown = bf.resolveStatusCode(argList[0])
			rc.ValueArg = NewNode(argList[1], bf.src)
			rc.ValueExpr = strings.TrimSpace(argList[1].Content(bf.src))
		}
	}
	bf.responseCalls = append(bf.responseCalls, rc)
}

// recordStatusWrite captures `WriteHeader(<expr>)`. Resolves
// `http.StatusOK` and friends to their numeric value.
func (bf *goBodyFacts) recordStatusWrite(args *sitter.Node) {
	if args == nil {
		return
	}
	first := firstNamedChild(args)
	if first == nil {
		return
	}
	if code, ok := bf.resolveStatusCode(first); ok {
		bf.statusWrites = append(bf.statusWrites, code)
	}
}

// resolveStatusCode tries to turn a status-code expression into an
// int. Recognises:
//   - `http.StatusOK` → 200, `http.StatusBadRequest` → 400, …
//   - integer literals (`200`, `404`)
//   - `StatusOK` (unqualified)
func (bf *goBodyFacts) resolveStatusCode(expr *sitter.Node) (int, bool) {
	if expr == nil {
		return 0, false
	}
	switch expr.Type() {
	case "int_literal":
		if v, err := strconv.Atoi(expr.Content(bf.src)); err == nil {
			return v, true
		}
	case "selector_expression":
		field := expr.ChildByFieldName("field")
		if field != nil {
			if c, ok := httpStatusCodeForName(field.Content(bf.src)); ok {
				return c, true
			}
		}
	case "identifier":
		if c, ok := httpStatusCodeForName(expr.Content(bf.src)); ok {
			return c, true
		}
	}
	return 0, false
}

// VarBinding returns the recorded binding for `name`, or a zero
// Binding if no fact was found. Strips leading & / * so the caller
// doesn't have to.
func (bf *goBodyFacts) VarBinding(name string) Binding {
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, "&*")
	if name == "" {
		return Binding{}
	}
	return bf.bindings[name]
}

// ResponseCalls returns every recorded JSON-response helper call.
func (bf *goBodyFacts) ResponseCalls() []ResponseCall {
	out := make([]ResponseCall, len(bf.responseCalls))
	copy(out, bf.responseCalls)
	return out
}

// MapLiteralEntries returns the keyed children of a composite map
// literal node — replaces splitMapLiteralBody. Skips non-string-keyed
// entries (composite literals can have non-string keys but JSON
// envelopes always use strings).
//
// AST shape (tree-sitter Go v0.25):
//
//	composite_literal
//	  map_type
//	  literal_value             ← body, positional named child
//	    keyed_element
//	      literal_element       ← key wrapper
//	        interpreted_string_literal
//	      literal_element       ← value wrapper
//	        <expression>
func (bf *goBodyFacts) MapLiteralEntries(node *Node) []KeyValue {
	if node == nil || node.Inner() == nil {
		return nil
	}
	body := compositeBodyNode(node.Inner())
	if body == nil {
		return nil
	}
	var out []KeyValue
	for i := 0; i < int(body.NamedChildCount()); i++ {
		ch := body.NamedChild(i)
		if ch == nil || ch.Type() != "keyed_element" {
			continue
		}
		if ch.NamedChildCount() < 2 {
			continue
		}
		keyNode := unwrapLiteralElement(ch.NamedChild(0))
		valNode := unwrapLiteralElement(ch.NamedChild(1))
		if keyNode == nil || valNode == nil {
			continue
		}
		var keyText string
		switch keyNode.Type() {
		case "interpreted_string_literal", "raw_string_literal":
			keyText = unquoteStringLiteral(keyNode.Content(bf.src))
		default:
			continue
		}
		out = append(out, KeyValue{
			Key:       keyText,
			ValueExpr: strings.TrimSpace(valNode.Content(bf.src)),
			ValueNode: NewNode(valNode, bf.src),
		})
	}
	return out
}

// StatusWrites returns recorded WriteHeader codes.
func (bf *goBodyFacts) StatusWrites() []int {
	out := make([]int, len(bf.statusWrites))
	copy(out, bf.statusWrites)
	return out
}

// QueryReads returns recorded query-accessor keys.
func (bf *goBodyFacts) QueryReads() []string {
	out := make([]string, len(bf.queryReads))
	copy(out, bf.queryReads)
	return out
}

// RequestBindings returns recorded request-body binding calls.
func (bf *goBodyFacts) RequestBindings() []RequestBinding {
	out := make([]RequestBinding, len(bf.requestBindings))
	copy(out, bf.requestBindings)
	return out
}

// ----- helpers -------------------------------------------------------

// compositeTypeNode returns the type expression of a composite_literal.
// The tree-sitter Go v0.25 grammar exposes the type as the first
// named child, not via a "type" field.
func compositeTypeNode(comp *sitter.Node) *sitter.Node {
	if comp == nil {
		return nil
	}
	if t := comp.ChildByFieldName("type"); t != nil {
		return t
	}
	for i := 0; i < int(comp.NamedChildCount()); i++ {
		ch := comp.NamedChild(i)
		if ch == nil {
			continue
		}
		if ch.Type() == "literal_value" {
			continue
		}
		return ch
	}
	return nil
}

// compositeBodyNode returns the literal_value (the `{...}` body) of
// a composite_literal.
func compositeBodyNode(comp *sitter.Node) *sitter.Node {
	if comp == nil {
		return nil
	}
	if b := comp.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := 0; i < int(comp.NamedChildCount()); i++ {
		ch := comp.NamedChild(i)
		if ch != nil && ch.Type() == "literal_value" {
			return ch
		}
	}
	return nil
}

// sliceElementNode returns the element type of a slice_type or
// array_type. Field names in tree-sitter Go vary across grammar
// versions; we try field name first, fall back to the first named
// child that isn't the array length.
func sliceElementNode(typ *sitter.Node) *sitter.Node {
	if typ == nil {
		return nil
	}
	if e := typ.ChildByFieldName("element"); e != nil {
		return e
	}
	// array_type: [length]ElemType — element is the second named child.
	// slice_type: []ElemType — element is the only named child.
	switch typ.Type() {
	case "array_type":
		if typ.NamedChildCount() >= 2 {
			return typ.NamedChild(1)
		}
	}
	return firstNamedChild(typ)
}

// pointerInner returns the pointee type of a pointer_type.
func pointerInner(typ *sitter.Node) *sitter.Node {
	if typ == nil {
		return nil
	}
	if t := typ.ChildByFieldName("type"); t != nil {
		return t
	}
	return firstNamedChild(typ)
}

// unwrapLiteralElement strips the literal_element wrapper that the
// Go grammar puts around composite-literal entries. Idempotent on
// non-wrapper nodes.
func unwrapLiteralElement(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	if node.Type() != "literal_element" {
		return node
	}
	return firstNamedChild(node)
}

// unqualify drops the package qualifier from a name expression.
// `pkg.Foo` → `Foo`; `Foo` → `Foo`.
func unqualify(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

func collectIdentifiers(parent *sitter.Node, fieldName string) []*sitter.Node {
	if parent == nil {
		return nil
	}
	first := parent.ChildByFieldName(fieldName)
	if first == nil {
		return nil
	}
	// Single-name var_spec: name is a direct identifier child.
	if first.Type() == "identifier" {
		return []*sitter.Node{first}
	}
	// Multi-name var_spec wraps in expression_list / similar.
	return namedChildren(first)
}

func namedChildren(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	out := make([]*sitter.Node, 0, n.NamedChildCount())
	for i := 0; i < int(n.NamedChildCount()); i++ {
		ch := n.NamedChild(i)
		if ch == nil {
			continue
		}
		out = append(out, ch)
	}
	return out
}

func firstNamedChild(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if n.NamedChildCount() == 0 {
		return nil
	}
	return n.NamedChild(0)
}

func firstExpressionInList(n *sitter.Node) *sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() == "expression_list" {
		return firstNamedChild(n)
	}
	return n
}

func identifierName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	if n.Type() != "identifier" {
		return ""
	}
	return strings.TrimSpace(n.Content(src))
}

func unquoteStringLiteral(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if q != '"' && q != '`' && q != '\'' {
		return s
	}
	if s[len(s)-1] != q {
		return s
	}
	inner := s[1 : len(s)-1]
	if q == '`' {
		return inner
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return inner
}

func isJSONResponseHelper(name string) bool {
	switch name {
	case "WriteJSON", "RespondJSON", "respondJSON", "WriteJson",
		"writeJSON", "renderJSON", "RenderJSON", "sendJSON", "SendJSON",
		"Render", "Encode", "JSON":
		return true
	}
	// Match Foo + JSON / Json suffixes seen in the wild
	// (returnJSON, replyJSON, …) without enumerating every helper.
	if strings.HasSuffix(name, "JSON") || strings.HasSuffix(name, "Json") {
		return true
	}
	return false
}

func isQueryAccessor(fn *sitter.Node, src []byte) bool {
	if fn == nil {
		return false
	}
	if fn.Type() != "selector_expression" {
		return false
	}
	field := fn.ChildByFieldName("field")
	if field == nil {
		return false
	}
	name := field.Content(src)
	switch name {
	case "FormValue", "PostFormValue":
		return true
	case "Get":
		// Match URL.Query().Get / Header.Get patterns; otherwise it's
		// a generic store.Get(ctx, id) which we don't want to count.
		recv := fn.ChildByFieldName("operand")
		if recv == nil {
			return false
		}
		rt := recv.Content(src)
		return strings.HasSuffix(rt, ".Header") ||
			strings.HasSuffix(rt, ".URL.Query()") ||
			strings.HasSuffix(rt, ".Query()")
	}
	return false
}

func firstStringLiteralArg(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	first := firstNamedChild(args)
	if first == nil {
		return ""
	}
	switch first.Type() {
	case "interpreted_string_literal", "raw_string_literal":
		return unquoteStringLiteral(first.Content(src))
	}
	return ""
}

// httpStatusCodeForName resolves an http.Status* constant name to its
// numeric value. Covers every name in net/http as of Go 1.22 plus
// the historical 418 teapot.
func httpStatusCodeForName(name string) (int, bool) {
	c, ok := httpStatusCodes[name]
	return c, ok
}

var httpStatusCodes = map[string]int{
	"StatusContinue":                      100,
	"StatusSwitchingProtocols":            101,
	"StatusProcessing":                    102,
	"StatusEarlyHints":                    103,
	"StatusOK":                            200,
	"StatusCreated":                       201,
	"StatusAccepted":                      202,
	"StatusNonAuthoritativeInfo":          203,
	"StatusNoContent":                     204,
	"StatusResetContent":                  205,
	"StatusPartialContent":                206,
	"StatusMultiStatus":                   207,
	"StatusAlreadyReported":               208,
	"StatusIMUsed":                        226,
	"StatusMultipleChoices":               300,
	"StatusMovedPermanently":              301,
	"StatusFound":                         302,
	"StatusSeeOther":                      303,
	"StatusNotModified":                   304,
	"StatusUseProxy":                      305,
	"StatusTemporaryRedirect":             307,
	"StatusPermanentRedirect":             308,
	"StatusBadRequest":                    400,
	"StatusUnauthorized":                  401,
	"StatusPaymentRequired":               402,
	"StatusForbidden":                     403,
	"StatusNotFound":                      404,
	"StatusMethodNotAllowed":              405,
	"StatusNotAcceptable":                 406,
	"StatusProxyAuthRequired":             407,
	"StatusRequestTimeout":                408,
	"StatusConflict":                      409,
	"StatusGone":                          410,
	"StatusLengthRequired":                411,
	"StatusPreconditionFailed":            412,
	"StatusRequestEntityTooLarge":         413,
	"StatusRequestURITooLong":             414,
	"StatusUnsupportedMediaType":          415,
	"StatusRequestedRangeNotSatisfiable":  416,
	"StatusExpectationFailed":             417,
	"StatusTeapot":                        418,
	"StatusMisdirectedRequest":            421,
	"StatusUnprocessableEntity":           422,
	"StatusLocked":                        423,
	"StatusFailedDependency":              424,
	"StatusTooEarly":                      425,
	"StatusUpgradeRequired":               426,
	"StatusPreconditionRequired":          428,
	"StatusTooManyRequests":               429,
	"StatusRequestHeaderFieldsTooLarge":   431,
	"StatusUnavailableForLegalReasons":    451,
	"StatusInternalServerError":           500,
	"StatusNotImplemented":                501,
	"StatusBadGateway":                    502,
	"StatusServiceUnavailable":            503,
	"StatusGatewayTimeout":                504,
	"StatusHTTPVersionNotSupported":       505,
	"StatusVariantAlsoNegotiates":         506,
	"StatusInsufficientStorage":           507,
	"StatusLoopDetected":                  508,
	"StatusNotExtended":                   510,
	"StatusNetworkAuthenticationRequired": 511,
}
