package contracts

import (
	"testing"
)

// stubResolver is a BindingResolver that returns predetermined types.
type stubResolver struct {
	results map[int]string // line → type
}

func (r *stubResolver) LookupTypeAtLine(filePath string, line int) (string, bool) {
	t, ok := r.results[line]
	return t, ok
}

// TestBindingResolver_OverridesGraphWalk confirms the resolver hook is
// consulted and its result is preferred over the graph walk. The
// indexer's lookupVarTypeForContract calls CurrentBindingResolver
// before falling back to the graph; this test exercises the contracts
// package's wiring.
func TestBindingResolver_WireUp(t *testing.T) {
	prev := CurrentBindingResolver()
	t.Cleanup(func() { SetBindingResolver(prev) })

	r := &stubResolver{results: map[int]string{42: "Foo"}}
	SetBindingResolver(r)
	got := CurrentBindingResolver()
	if got != r {
		t.Fatalf("CurrentBindingResolver: want stub, got %v", got)
	}
	tn, ok := got.LookupTypeAtLine("anywhere.go", 42)
	if !ok || tn != "Foo" {
		t.Errorf("stub lookup: want (Foo, true), got (%q, %v)", tn, ok)
	}
}

func TestBindingResolver_NilWhenUnset(t *testing.T) {
	prev := CurrentBindingResolver()
	t.Cleanup(func() { SetBindingResolver(prev) })

	SetBindingResolver(nil)
	if CurrentBindingResolver() != nil {
		t.Errorf("CurrentBindingResolver after nil: want nil")
	}
}
