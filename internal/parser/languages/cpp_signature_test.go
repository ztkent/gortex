package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func cppNodeMeta(t *testing.T, src, nodeID string) map[string]any {
	t.Helper()
	res, err := NewCppExtractor().Extract("t.cpp", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, n := range res.Nodes {
		if n.ID == nodeID {
			return n.Meta
		}
	}
	t.Fatalf("node %q not found; nodes: %v", nodeID, func() []string {
		var ids []string
		for _, n := range res.Nodes {
			if n.Kind == graph.KindFunction || n.Kind == graph.KindMethod {
				ids = append(ids, n.ID)
			}
		}
		return ids
	}())
	return nil
}

func TestExtractCppSignature_FreeFunctions(t *testing.T) {
	src := `void f(int x, double y) { }
void g(int x, int y) { }
`
	fm := cppNodeMeta(t, src, "t.cpp::f")
	if fm["cpp_param_types"] != "int,double" {
		t.Errorf("f param types = %v, want int,double", fm["cpp_param_types"])
	}
	if fm["cpp_sig"] != "1" {
		t.Errorf("f must carry cpp_sig marker")
	}
}

func TestExtractCppSignature_Shapes(t *testing.T) {
	src := `struct Foo {};
void h(const Foo& a, Foo* b) { }
`
	m := cppNodeMeta(t, src, "t.cpp::h")
	// const Foo& → cl (const lvalue-ref); Foo* → p
	if m["cpp_param_shapes"] != "cl,p" {
		t.Errorf("h param shapes = %v, want cl,p", m["cpp_param_shapes"])
	}
	if m["cpp_param_types"] != "Foo,Foo" {
		t.Errorf("h param types = %v, want Foo,Foo", m["cpp_param_types"])
	}
}

func TestExtractCppSignature_DefaultsVariadicVoid(t *testing.T) {
	defm := cppNodeMeta(t, "void d(int x, int y = 0) { }\n", "t.cpp::d")
	if defm["cpp_param_types"] != "int,int" {
		t.Errorf("d types = %v", defm["cpp_param_types"])
	}
	if got := defm["cpp_req_params"]; got != 1 {
		t.Errorf("d req params = %v, want 1 (one default)", got)
	}

	varm := cppNodeMeta(t, "void v(int x, ...) { }\n", "t.cpp::v")
	if varm["cpp_variadic"] != "1" {
		t.Errorf("v must be variadic, meta=%v", varm)
	}

	voidm := cppNodeMeta(t, "void e(void) { }\n", "t.cpp::e")
	if _, has := voidm["cpp_param_types"]; has {
		t.Errorf("e(void) must have zero params, got %v", voidm["cpp_param_types"])
	}
	if voidm["cpp_sig"] != "1" {
		t.Errorf("e must still carry cpp_sig marker")
	}
}
