package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestCppConversionRank(t *testing.T) {
	v := cppShape{}
	ptr := cppShape{isPointer: true}
	ellipsis := cppShape{}
	cases := []struct {
		arg, param   string
		ash, psh     cppShape
		want         int
	}{
		{"int", "int", v, v, 0},          // exact
		{"int", "int", v, ptr, cppRankInf}, // value≠pointer (shape-aware)
		{"int", "int", ptr, ptr, 0},      // pointer exact
		{"char", "int", v, v, 1},         // integral promotion
		{"bool", "int", v, v, 1},         // bool→int promotion
		{"int", "double", v, v, 2},       // arithmetic standard
		{"null", "int", v, ptr, 2},       // nullptr→T*
		{"null", "bool", v, v, 3},        // nullptr→bool (worse than →T*)
		{"int", "...", v, ellipsis, 5},   // ellipsis
		{"string", "int", v, v, cppRankInf}, // mismatch
	}
	for _, c := range cases {
		if got := cppConversionRank(c.arg, c.param, c.ash, c.psh); got != c.want {
			t.Errorf("rank(%q→%q) = %d, want %d", c.arg, c.param, got, c.want)
		}
	}
}

func cppFn(id, params, shapes string, req int) *graph.Node {
	m := map[string]any{"cpp_sig": "1"}
	if params != "" {
		m["cpp_param_types"] = params
	}
	if shapes != "" {
		m["cpp_param_shapes"] = shapes
	}
	if req > 0 {
		m["cpp_req_params"] = req
	}
	return &graph.Node{ID: id, Kind: graph.KindFunction, Name: "process", Meta: m}
}

func TestResolveCppOverload_ArithmeticSelection(t *testing.T) {
	intFn := cppFn("f::process#int", "int", "v", 1)
	dblFn := cppFn("f::process#double", "double", "v", 1)
	cands := []*graph.Node{intFn, dblFn}

	if got := ResolveCppOverload([]string{"double"}, cands); got != dblFn {
		t.Errorf("double arg should pick process(double), got %v", got)
	}
	if got := ResolveCppOverload([]string{"int"}, cands); got != intFn {
		t.Errorf("int arg should pick process(int), got %v", got)
	}
	if got := ResolveCppOverload([]string{"char"}, cands); got != intFn {
		t.Errorf("char arg should promote to process(int), got %v", got)
	}
}

func TestResolveCppOverload_ShapeDistinguishesPointer(t *testing.T) {
	valFn := cppFn("f::process#int", "int", "v", 1)
	ptrFn := cppFn("f::process#intptr", "int", "p", 1)
	// A value int literal must not match the int* overload.
	if got := ResolveCppOverload([]string{"int"}, []*graph.Node{valFn, ptrFn}); got != valFn {
		t.Errorf("int value arg should pick the value overload, got %v", got)
	}
}

func TestResolveCppOverload_Arity(t *testing.T) {
	zero := cppFn("f::process#0", "", "", 0)
	one := cppFn("f::process#1", "int", "v", 1)
	if got := ResolveCppOverload([]string{"int"}, []*graph.Node{zero, one}); got != one {
		t.Errorf("1 arg should pick the 1-param overload, got %v", got)
	}
	if got := ResolveCppOverload(nil, []*graph.Node{zero, one}); got != zero {
		t.Errorf("0 args should pick the 0-param overload, got %v", got)
	}
}

func TestResolveCppOverload_DefaultsAndVariadic(t *testing.T) {
	// process(int, int = 0): req 1, total 2.
	def := cppFn("f::process#def", "int,int", "v,v", 1)
	if got := ResolveCppOverload([]string{"int"}, []*graph.Node{def}); got != def {
		t.Errorf("1 arg must satisfy a 2-param-with-default overload, got %v", got)
	}
	// variadic process(int, ...): req 1.
	vfn := cppFn("f::process#var", "int", "v", 1)
	vfn.Meta["cpp_variadic"] = "1"
	if got := ResolveCppOverload([]string{"int", "double", "char"}, []*graph.Node{vfn}); got != vfn {
		t.Errorf("variadic overload must accept extra args, got %v", got)
	}
}

func TestResolveCppOverload_AmbiguousSuppressed(t *testing.T) {
	// Two overloads the arg ranks equally well against → ambiguous → nil.
	a := cppFn("f::process#a", "int", "v", 1)
	b := cppFn("f::process#b", "int", "v", 1)
	if got := ResolveCppOverload([]string{"int"}, []*graph.Node{a, b}); got != nil {
		t.Errorf("equally-ranked overloads must suppress (nil), got %v", got)
	}
	// No arg hints + 2 viable → suppress.
	if got := ResolveCppOverload(nil, []*graph.Node{cppFn("x", "int", "v", 1), cppFn("y", "double", "v", 1)}); got != nil {
		t.Errorf("no hints with 2 viable must suppress, got %v", got)
	}
}

func TestResolveCppOverload_NoSignatureDegrade(t *testing.T) {
	// Candidates without extracted signatures → degrade (nil) to the cascade.
	a := &graph.Node{ID: "x", Kind: graph.KindFunction, Name: "process"}
	b := &graph.Node{ID: "y", Kind: graph.KindFunction, Name: "process"}
	if got := ResolveCppOverload([]string{"int"}, []*graph.Node{a, b}); got != nil {
		t.Errorf("no signature metadata must degrade to nil, got %v", got)
	}
}
