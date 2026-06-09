package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func storeAction(g *graph.Graph, id, file, binding, member string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindFunction, Name: member, FilePath: file,
		Meta: map[string]any{"store_factory": binding, "store_member": member},
	})
}

func storeCall(g *graph.Graph, callerID, callerFile, binding, action string) {
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: callerID, FilePath: callerFile})
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*." + action, Kind: graph.EdgeCalls, FilePath: callerFile,
		Meta: map[string]any{"via": "store-factory", "store_binding": binding, "store_action": action},
	})
}

func TestResolveStoreFactoryCalls_SingleBinding(t *testing.T) {
	g := graph.New()
	storeAction(g, "store.ts::useStore.reset@3", "store.ts", "useStore", "reset")
	storeCall(g, "caller.ts::hardReset", "caller.ts", "useStore", "reset")

	n := ResolveStoreFactoryCalls(g)
	if n != 1 {
		t.Fatalf("expected 1 resolved, got %d", n)
	}
	// hardReset should now call the reset action.
	var bound bool
	for _, e := range g.GetOutEdges("caller.ts::hardReset") {
		if e.To == "store.ts::useStore.reset@3" && e.Kind == graph.EdgeCalls {
			bound = true
			if e.Meta["synthesized_by"] != SynthStoreFactory {
				t.Errorf("expected synthesized_by=%s, got %v", SynthStoreFactory, e.Meta["synthesized_by"])
			}
		}
	}
	if !bound {
		t.Errorf("hardReset edge not rebound to the reset action")
	}
}

func TestResolveStoreFactoryCalls_CollisionPrefersSameFile(t *testing.T) {
	g := graph.New()
	// Two stores both bound to `useStore`, each with a reset, in different files.
	storeAction(g, "a.ts::useStore.reset@1", "a.ts", "useStore", "reset")
	storeAction(g, "b.ts::useStore.reset@1", "b.ts", "useStore", "reset")
	// Caller lives in a.ts → must bind to a's reset, never b's.
	storeCall(g, "a.ts::hardReset", "a.ts", "useStore", "reset")

	ResolveStoreFactoryCalls(g)
	for _, e := range g.GetOutEdges("a.ts::hardReset") {
		if e.Kind == graph.EdgeCalls && e.To == "b.ts::useStore.reset@1" {
			t.Fatalf("cross-store mis-bind: hardReset bound to b.ts reset")
		}
	}
	found := false
	for _, e := range g.GetOutEdges("a.ts::hardReset") {
		if e.To == "a.ts::useStore.reset@1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected same-file bind to a.ts reset")
	}
}

func TestResolveStoreFactoryCalls_AmbiguousNotBound(t *testing.T) {
	g := graph.New()
	storeAction(g, "a.ts::useStore.reset@1", "a.ts", "useStore", "reset")
	storeAction(g, "b.ts::useStore.reset@1", "b.ts", "useStore", "reset")
	// Caller in a third file that can't be disambiguated.
	storeCall(g, "c.ts::hardReset", "c.ts", "useStore", "reset")

	ResolveStoreFactoryCalls(g)
	for _, e := range g.GetOutEdges("c.ts::hardReset") {
		if e.Kind == graph.EdgeCalls && (e.To == "a.ts::useStore.reset@1" || e.To == "b.ts::useStore.reset@1") {
			t.Fatalf("ambiguous call should not bind, but bound to %s", e.To)
		}
	}
}

func TestResolveStoreFactoryCalls_Idempotent(t *testing.T) {
	g := graph.New()
	storeAction(g, "store.ts::useStore.reset@3", "store.ts", "useStore", "reset")
	storeCall(g, "caller.ts::hardReset", "caller.ts", "useStore", "reset")

	first := ResolveStoreFactoryCalls(g)
	second := ResolveStoreFactoryCalls(g)
	if first != 1 || second != 1 {
		t.Errorf("expected idempotent resolve count 1/1, got %d/%d", first, second)
	}
}
