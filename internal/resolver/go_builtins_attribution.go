package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// goBuiltinFuncs is the complete set of pre-declared Go built-in
// functions. Source: https://pkg.go.dev/builtin (functions section).
// Kept in sync with the language spec — when a new builtin lands
// (e.g. clear / min / max in Go 1.21) add it here.
var goBuiltinFuncs = map[string]struct{}{
	"append": {}, "cap": {}, "clear": {}, "close": {}, "complex": {},
	"copy": {}, "delete": {}, "imag": {}, "len": {}, "make": {},
	"max": {}, "min": {}, "new": {}, "panic": {}, "print": {},
	"println": {}, "real": {}, "recover": {},
}

// goBuiltinTypes is the complete set of pre-declared Go built-in
// types. Source: https://pkg.go.dev/builtin (types section).
var goBuiltinTypes = map[string]struct{}{
	"any": {}, "bool": {}, "byte": {}, "comparable": {},
	"complex64": {}, "complex128": {}, "error": {},
	"float32": {}, "float64": {},
	"int": {}, "int8": {}, "int16": {}, "int32": {}, "int64": {},
	"rune": {}, "string": {},
	"uint": {}, "uint8": {}, "uint16": {}, "uint32": {}, "uint64": {},
	"uintptr": {},
}

// goBuiltinConsts is the set of pre-declared Go constants (true,
// false, iota, nil). Mostly emitted for completeness — `true` /
// `false` rarely show up as unresolved edge targets in practice
// because the parser handles them inline.
var goBuiltinConsts = map[string]struct{}{
	"true": {}, "false": {}, "iota": {}, "nil": {},
}

// attributeGoBuiltins rewrites `unresolved::<name>` edges whose name
// is a Go language intrinsic onto the canonical `builtin::go::*` ID,
// and materialises a single KindBuiltin node per unique builtin so
// the rewritten edges land at a real graph node instead of a
// rel-table FK stub. Mirrors the existing builtin::py / builtin::ts
// classifier in internal/resolver/builtins.go but completes the
// pattern by also creating nodes for the targets — so
// `find_usages(builtin::go::type::float64)` answers "every variable
// typed as float64 in this codebase", and the Ladybug stub
// inflation drops by ~50k rows on a gortex-scale Go codebase.
//
// Three ID namespaces under `builtin::go::`:
//
//	functions: builtin::go::<name>          (append, len, make, ...)
//	types:     builtin::go::type::<name>    (string, int, float64, ...)
//	constants: builtin::go::const::<name>   (true, false, iota, nil)
//
// Functions get the shortest namespace because their fan-in is the
// biggest and the shorter ID is what most downstream `find_usages`
// queries will type.
func (r *Resolver) attributeGoBuiltins() {
	materialised := map[string]struct{}{}
	var batch []graph.EdgeReindex

	// Every edge kind a builtin can be the target of. Type-system
	// edges (typed_as / returns) carry type references; call /
	// arg-of / value-flow carry function or const references.
	for _, k := range []graph.EdgeKind{
		graph.EdgeCalls,
		graph.EdgeReferences,
		graph.EdgeReads,
		graph.EdgeArgOf,
		graph.EdgeValueFlow,
		graph.EdgeReturnsTo,
		graph.EdgeTypedAs,
		graph.EdgeReturns,
		graph.EdgeInstantiates,
		graph.EdgeCaptures,
		graph.EdgeThrows,
	} {
		for e := range r.graph.EdgesByKind(k) {
			if old := r.tryAttributeGoBuiltin(e, materialised); old != "" {
				batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
			}
		}
	}
	if len(batch) > 0 {
		r.graph.ReindexEdges(batch)
	}
}

// tryAttributeGoBuiltin checks if e.To is `unresolved::<bareName>`
// where bareName is a Go builtin and the source language is Go (the
// source is inside a Go function / file). On a match it materialises
// the target node (once per unique ID), rewrites e.To, and returns
// the old To value for the batched reindex. Returns "" when the edge
// is left alone.
func (r *Resolver) tryAttributeGoBuiltin(e *graph.Edge, materialised map[string]struct{}) string {
	if e == nil || !strings.HasPrefix(e.To, "unresolved::") {
		return ""
	}
	name := strings.TrimPrefix(e.To, "unresolved::")
	if name == "" || strings.ContainsAny(name, ".*:#") {
		return ""
	}
	// Only attribute when the source is Go. Without this guard a
	// Python reference to a local named `len` would get re-targeted
	// at Go's builtin `len`, which would be obviously wrong.
	if !r.fromIsGo(e.From) {
		return ""
	}
	newID, kind, builtinKind := goBuiltinTarget(name)
	if newID == "" {
		return ""
	}
	if _, ok := materialised[newID]; !ok {
		// AddNode is idempotent on ID, so even a second
		// concurrent pass would not duplicate the row.
		r.graph.AddNode(&graph.Node{
			ID:       newID,
			Kind:     kind,
			Name:     name,
			Language: "go",
			Meta: map[string]any{
				"builtin":      true,
				"builtin_kind": builtinKind,
			},
		})
		materialised[newID] = struct{}{}
	}
	oldTo := e.To
	e.To = newID
	return oldTo
}

// goBuiltinTarget classifies a bare identifier as one of Go's
// intrinsics. Returns the canonical builtin::go:: ID, the NodeKind
// to materialise it under (always KindBuiltin), and a meta tag
// recording which subspace (func / type / const) it belongs to.
// Returns ("", "", "") when the name is not a Go builtin.
func goBuiltinTarget(name string) (id string, kind graph.NodeKind, builtinKind string) {
	if _, ok := goBuiltinFuncs[name]; ok {
		return "builtin::go::" + name, graph.KindBuiltin, "func"
	}
	if _, ok := goBuiltinTypes[name]; ok {
		return "builtin::go::type::" + name, graph.KindBuiltin, "type"
	}
	if _, ok := goBuiltinConsts[name]; ok {
		return "builtin::go::const::" + name, graph.KindBuiltin, "const"
	}
	return "", "", ""
}

// fromIsGo reports whether the source endpoint of an edge sits
// inside Go code. Uses the From's enclosing function (via the same
// suffix-stripping helper bare-name binding uses) — Go is the only
// language whose IDs follow the `file.go::Func` convention with a
// `.go` extension, so a path-based check is both cheap and reliable.
func (r *Resolver) fromIsGo(fromID string) bool {
	owner := enclosingFunctionForBinding(fromID)
	if owner == "" {
		return false
	}
	if i := strings.Index(owner, "::"); i > 0 {
		// `pkg/foo.go::Func` shape — peek at the file extension.
		head := owner[:i]
		if strings.HasSuffix(head, ".go") {
			return true
		}
	}
	// Fall back to looking up the owner node and checking its
	// Language. More expensive but covers edge cases where the ID
	// doesn't follow the `.go::Func` pattern.
	if n := r.graph.GetNode(owner); n != nil && n.Language == "go" {
		return true
	}
	return false
}
