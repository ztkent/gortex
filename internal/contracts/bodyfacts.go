package contracts

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Origin labels the confidence with which a Binding was derived.
// Mirrors the tier vocabulary in spec-semantic.md so the dashboard
// and downstream consumers see a single set of labels regardless of
// which subsystem populated the binding.
type Origin string

const (
	OriginTextMatched Origin = "text_matched"  // legacy regex enricher
	OriginASTInferred Origin = "ast_inferred"  // tree-sitter fact, name-based type lookup
	OriginASTResolved Origin = "ast_resolved"  // tree-sitter fact, resolved against the file's graph
	OriginLSPResolved Origin = "lsp_resolved"  // compiler-grade (go/types or LSP)
)

// BindingKind labels the syntactic shape of a local-variable binding
// in a handler body. The contract pipeline uses Kind to decide whether
// to walk the graph for a return type, treat the value as a primitive,
// or render the raw expression.
type BindingKind string

const (
	BindingUnknown     BindingKind = ""             // no binding found
	BindingMethodCall  BindingKind = "method_call"  // x := h.svc.GetRepos(...)
	BindingFuncCall    BindingKind = "func_call"    // x := makeFoo(...)
	BindingComposite   BindingKind = "composite"    // x := Foo{...}
	BindingSliceLit    BindingKind = "slice_lit"    // x := []Foo{...}
	BindingMakeSlice   BindingKind = "make_slice"   // x := make([]Foo, ...)
	BindingMakeMap     BindingKind = "make_map"     // x := make(map[K]V, ...)
	BindingMapLit      BindingKind = "map_lit"      // x := map[K]V{...}
	BindingPathValue   BindingKind = "path_value"   // x := r.PathValue("id")
	BindingFormValue   BindingKind = "form_value"   // x := r.FormValue("id")
	BindingHeaderValue BindingKind = "header_value" // x := r.Header.Get("X-Foo")
	BindingQueryGet    BindingKind = "query_get"    // x := r.URL.Query().Get("k")
	BindingStringLit   BindingKind = "string_lit"
	BindingIntLit      BindingKind = "int_lit"
	BindingFloatLit    BindingKind = "float_lit"
	BindingBoolLit     BindingKind = "bool_lit"
)

// Binding is the per-variable fact a contract enricher reads instead
// of running regexes. Subsumes findVarType (schema_enrich_go.go),
// traceVarTypeFromBodyWithShape (indexer.go), and bindLiteralTypeFromBody
// (indexer.go) — all three are deleted in phase 1.
type Binding struct {
	Kind     BindingKind
	TypeID   string // graph type-node ID when resolvable; bare type name otherwise
	Repeated bool   // slice-typed binding (renders [Foo] in the dashboard)
	Pointer  bool   // pointer-typed binding
	CallExpr string // "h.svc.GetRepos" for method_call/func_call; empty otherwise
	RawExpr  string // exact source span of the RHS, for fallback display
	Line     int    // 1-based line where the binding's RHS appears
	Origin   Origin
}

// IsZero reports whether the binding is empty (no fact found).
func (b Binding) IsZero() bool { return b.Kind == BindingUnknown }

// ResponseCall describes one JSON-response helper invocation in a
// handler body — WriteJSON, respondJSON, json.NewEncoder().Encode, or
// a framework method like c.JSON(code, value).
type ResponseCall struct {
	Helper       string  // "WriteJSON" | "respondJSON" | "Encode" | "JSON" | …
	StatusArg    *Node   // status-code argument (may be nil for helpers without one)
	StatusCode   int     // resolved status code; 0 if not resolvable
	StatusKnown  bool    // distinguishes 0 = unresolved from 0 = legitimate StatusOK
	ValueArg     *Node   // value argument (the body)
	ValueExpr    string  // trimmed source span of the value argument
	Line         int     // 1-based line of the call
}

// RequestBinding describes one call that binds the request body to a
// typed variable: json.NewDecoder(r.Body).Decode(&req), c.BodyParser(&req),
// c.ShouldBindJSON(&req), json.Unmarshal(body, &req).
//
// VarName is the bound variable name (without the leading & or *).
// CompositeType, if non-empty, is the literal type used at the call
// site (e.g. `Decode(&Request{})` → "Request"); the caller can use
// this directly without going through VarBinding.
type RequestBinding struct {
	Helper        string // "Decode" | "BodyParser" | "ShouldBindJSON" | "Bind" | "Unmarshal"
	VarName       string // bound variable name (no &/*)
	CompositeType string // type literal at the binding call, if any
	Line          int
}

// KeyValue is one parsed entry from a Go map composite literal —
// `"data": workspaces` becomes `{Key: "data", ValueExpr: "workspaces"}`.
type KeyValue struct {
	Key       string
	ValueExpr string
	ValueNode *Node
}

// Node is an opaque AST handle. Wraps a tree-sitter node so consumers
// don't have to import the sitter package directly.
type Node struct {
	inner *sitter.Node
	src   []byte
}

// NewNode wraps a sitter node + source for use by BodyFacts callers.
// Returns nil for a nil input.
func NewNode(n *sitter.Node, src []byte) *Node {
	if n == nil {
		return nil
	}
	return &Node{inner: n, src: src}
}

// Inner returns the underlying tree-sitter node. Internal use only —
// avoid reaching past BodyFacts methods in non-extractor code.
func (n *Node) Inner() *sitter.Node {
	if n == nil {
		return nil
	}
	return n.inner
}

// Text returns the UTF-8 source span the node covers.
func (n *Node) Text() string {
	if n == nil || n.inner == nil {
		return ""
	}
	return n.inner.Content(n.src)
}

// Kind returns the tree-sitter node kind ("identifier", "call_expression", …).
func (n *Node) Kind() string {
	if n == nil || n.inner == nil {
		return ""
	}
	return n.inner.Type()
}

// Line returns the 1-based line of the node start.
func (n *Node) Line() int {
	if n == nil || n.inner == nil {
		return 0
	}
	return int(n.inner.StartPoint().Row) + 1
}

// BodyFacts is the structured per-handler view a contract enricher
// AND the post-pass resolvers read instead of running regexes.
// Implementations are per-language; the Go implementation is phase 1.
// Other languages register nopBodyFacts; their phase ships a real
// implementation without changes to consumers.
type BodyFacts interface {
	// VarBinding returns the binding kind and inferred type for a
	// local variable in the handler body. Returns a zero Binding if
	// no fact was found.
	VarBinding(name string) Binding

	// ResponseCalls returns every call to a JSON-response helper in
	// the body, in source order.
	ResponseCalls() []ResponseCall

	// MapLiteralEntries returns the keyed children of a composite
	// map literal at `node`. Used to walk envelope shapes without
	// the brace-balancing splitMapLiteralBody used to do.
	MapLiteralEntries(node *Node) []KeyValue

	// StatusWrites returns every WriteHeader call's status code.
	StatusWrites() []int

	// QueryReads returns the names of every URL/form/header query
	// accessor key in the body.
	QueryReads() []string

	// RequestBindings returns every call that binds the request body
	// to a typed variable: Decode/BodyParser/ShouldBind*/Bind/Unmarshal.
	// Used by the AST overlay to set request_type without going
	// through findVarType.
	RequestBindings() []RequestBinding
}

// BodyFactsFactory builds a BodyFacts view for one handler.
type BodyFactsFactory func(tree *parser.ParseTree, handler *graph.Node) BodyFacts

// bodyFactsByLang maps a language code to the factory that produces
// per-handler BodyFacts. Languages without a registered factory get
// nopBodyFacts via FactoryForLang.
var bodyFactsByLang = map[string]BodyFactsFactory{}

// RegisterBodyFactsFactory registers a per-language factory. Called
// from per-language init() in bodyfacts_<lang>.go files.
func RegisterBodyFactsFactory(lang string, factory BodyFactsFactory) {
	bodyFactsByLang[lang] = factory
}

// FactoryForLang returns the registered factory for lang, or a factory
// that always returns nopBodyFacts when no implementation exists.
func FactoryForLang(lang string) BodyFactsFactory {
	if f, ok := bodyFactsByLang[lang]; ok {
		return f
	}
	return newNopBodyFacts
}

// MakeBodyFacts is a convenience wrapper that handles the nil-tree
// case and dispatches to the registered factory.
func MakeBodyFacts(tree *parser.ParseTree, handler *graph.Node) BodyFacts {
	if tree == nil || tree.Tree() == nil || handler == nil {
		return nopBodyFacts{}
	}
	factory := FactoryForLang(tree.Lang())
	return factory(tree, handler)
}

// BindingResolver is the optional upgrade hook the AST overlay
// consults when present (set via SetBindingResolver below). When
// the resolver returns (typeName, true), the overlay stamps the
// binding with Origin = OriginLSPResolved instead of OriginASTInferred.
//
// goanalysis.Provider implements this via LookupTypeAtLine. The
// indexer wires it in when --semantic is enabled and the provider
// has run for the repo.
type BindingResolver interface {
	// LookupTypeAtLine returns the resolved type name at the given
	// 1-based line in the file. Returns ("", false) when the line
	// has no resolvable typed declaration.
	LookupTypeAtLine(filePath string, line int) (string, bool)
}

// activeBindingResolver is the optional upgrade resolver. nil when
// no provider has been wired in; the overlay falls back to the
// tree-sitter-only path in that case.
var activeBindingResolver BindingResolver

// SetBindingResolver registers the LookupTypeAtLine implementation
// the AST overlay should consult. Called by the indexer once the
// goanalysis (or other semantic) provider has loaded type info.
// Pass nil to clear.
func SetBindingResolver(r BindingResolver) {
	activeBindingResolver = r
}

// CurrentBindingResolver returns the wired-in resolver or nil.
func CurrentBindingResolver() BindingResolver { return activeBindingResolver }

// nopBodyFacts is the no-op implementation used for languages without
// a real BodyFacts factory. Returns "unbound" / empty for everything,
// which causes the legacy regex enricher to run in fallback mode.
type nopBodyFacts struct{}

func newNopBodyFacts(_ *parser.ParseTree, _ *graph.Node) BodyFacts {
	return nopBodyFacts{}
}

func (nopBodyFacts) VarBinding(string) Binding              { return Binding{} }
func (nopBodyFacts) ResponseCalls() []ResponseCall          { return nil }
func (nopBodyFacts) MapLiteralEntries(*Node) []KeyValue     { return nil }
func (nopBodyFacts) StatusWrites() []int                    { return nil }
func (nopBodyFacts) QueryReads() []string                   { return nil }
func (nopBodyFacts) RequestBindings() []RequestBinding      { return nil }
