package graph

import "testing"

func boundaryGraph() *Graph {
	g := New()
	for _, id := range []string{"A", "B", "C"} {
		g.AddNode(&Node{ID: id, Kind: KindMethod, Name: id})
	}
	// A implements an interface method → caller-side dispatch boundary.
	g.AddEdge(&Edge{From: "A", To: "iface::I.M", Kind: EdgeImplements, Origin: OriginASTResolved})
	// B calls an unresolved (dynamic-dispatch) target → callee-side floor.
	g.AddEdge(&Edge{From: "B", To: "unresolved::handler", Kind: EdgeCalls, Origin: OriginASTInferred})
	// C calls a stdlib stub → listed but not floor-making.
	g.AddEdge(&Edge{From: "C", To: "stdlib::fmt.Println", Kind: EdgeCalls, Origin: OriginASTResolved})
	return g
}

func TestCalleeBoundaries(t *testing.T) {
	g := boundaryGraph()
	bs := CalleeBoundaries(g, []string{"B", "C"}, 0)
	if len(bs) != 2 {
		t.Fatalf("expected 2 callee boundaries, got %d: %+v", len(bs), bs)
	}
	byReason := map[BoundaryReason]EpistemicBoundary{}
	for _, b := range bs {
		byReason[b.Reason] = b
		if b.Direction != "callees" {
			t.Errorf("expected callees direction, got %s", b.Direction)
		}
	}
	if d, ok := byReason[BoundaryDynamicDispatch]; !ok || d.Target != "handler" {
		t.Errorf("expected dynamic_dispatch boundary on 'handler', got %+v", byReason)
	}
	if _, ok := byReason[BoundaryStub]; !ok {
		t.Errorf("expected stub boundary for stdlib call, got %+v", byReason)
	}
}

func TestCallerBoundaries(t *testing.T) {
	g := boundaryGraph()
	bs := CallerBoundaries(g, []string{"A"}, 0)
	if len(bs) != 1 {
		t.Fatalf("expected 1 caller boundary, got %d: %+v", len(bs), bs)
	}
	b := bs[0]
	if b.Reason != BoundaryInterfaceDispatch || b.Direction != "callers" {
		t.Errorf("unexpected boundary: %+v", b)
	}
	// A method that implements nothing has no caller boundary.
	if got := CallerBoundaries(g, []string{"B"}, 0); len(got) != 0 {
		t.Errorf("expected no boundary for non-implementing node, got %+v", got)
	}
}

func TestLowerBoundCaveat(t *testing.T) {
	cases := []struct {
		name string
		bs   []EpistemicBoundary
		want bool
	}{
		{"empty", nil, false},
		{"dynamic", []EpistemicBoundary{{Reason: BoundaryDynamicDispatch}}, true},
		{"interface", []EpistemicBoundary{{Reason: BoundaryInterfaceDispatch}}, true},
		{"stub only", []EpistemicBoundary{{Reason: BoundaryStub}}, false},
		{"external only", []EpistemicBoundary{{Reason: BoundaryExternal}}, false},
		{"mixed", []EpistemicBoundary{{Reason: BoundaryStub}, {Reason: BoundaryDynamicDispatch}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LowerBoundCaveat(tc.bs); got != tc.want {
				t.Errorf("LowerBoundCaveat = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassifyDroppedTarget(t *testing.T) {
	cases := []struct {
		target string
		kind   EdgeKind
		reason BoundaryReason
		ok     bool
	}{
		{"unresolved::Foo", EdgeCalls, BoundaryDynamicDispatch, true},
		{"external::lodash.map", EdgeCalls, BoundaryExternal, true},
		{"stdlib::fmt.Println", EdgeCalls, BoundaryStub, true},
		{"pkg/x.go::Real", EdgeCalls, "", false},
	}
	for _, tc := range cases {
		reason, ok := ClassifyDroppedTarget(tc.target, tc.kind)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("ClassifyDroppedTarget(%q) = (%q,%v), want (%q,%v)", tc.target, reason, ok, tc.reason, tc.ok)
		}
	}
}
