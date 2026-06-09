package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// callTargetName returns the resolved target's Name for the EdgeCalls edge from
// `caller` to a function named `callee` (the resolved overload's enclosing
// node's distinguishing meta).
func cppCallTarget(t *testing.T, g *graph.Graph, caller string) *graph.Node {
	t.Helper()
	for _, n := range g.AllNodes() {
		if n.Name != caller {
			continue
		}
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			if tn := g.GetNode(e.To); tn != nil && tn.Name == "process" {
				return tn
			}
		}
	}
	return nil
}

// TestCppOverload_ResolvesArithmetic indexes overloaded C++ functions and
// asserts a call with a double literal resolves to process(double), and an int
// literal to process(int) — in-engine, no clangd.
func TestCppOverload_ResolvesArithmetic(t *testing.T) {
	g := indexFixture(t, map[string]string{
		"o.cpp": `void process(int x) { }
void process(double x) { }

void runDouble() { process(3.14); }
void runInt() { process(42); }
`,
	})

	dbl := cppCallTarget(t, g, "runDouble")
	if dbl == nil {
		t.Fatal("runDouble's process() call did not resolve")
	}
	if pt, _ := dbl.Meta["cpp_param_types"].(string); pt != "double" {
		t.Errorf("runDouble should bind process(double), got param_types=%q (node %s)", pt, dbl.ID)
	}

	in := cppCallTarget(t, g, "runInt")
	if in == nil {
		t.Fatal("runInt's process() call did not resolve")
	}
	if pt, _ := in.Meta["cpp_param_types"].(string); pt != "int" {
		t.Errorf("runInt should bind process(int), got param_types=%q (node %s)", pt, in.ID)
	}
}
