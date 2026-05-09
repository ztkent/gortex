package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runKotlinExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ext := NewKotlinExtractor()
	result, err := ext.Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result.Nodes, result.Edges
}

func TestKotlinAsyncSpawns_LaunchAsync(t *testing.T) {
	src := `package x

import kotlinx.coroutines.*

fun runAll() {
    GlobalScope.launch {
        val deferred = async { compute() }
        deferred.await()
    }
}
`
	_, edges := runKotlinExtract(t, "x/runner.kt", src)

	spawns := edgesByKind(edges, graph.EdgeSpawns)
	wantBuilders := map[string]bool{"unresolved::launch": false, "unresolved::async": false}
	hasAwait := false
	for _, e := range spawns {
		mode, _ := e.Meta["mode"].(string)
		if mode == "coroutine" {
			if _, ok := wantBuilders[e.To]; ok {
				wantBuilders[e.To] = true
			}
		}
		if mode == "async" && e.To == "unresolved::await" {
			hasAwait = true
		}
	}
	for tgt, found := range wantBuilders {
		if !found {
			t.Errorf("expected EdgeSpawns mode=coroutine → %s; got %v", tgt, edgeTargets(spawns))
		}
	}
	if !hasAwait {
		t.Errorf("expected EdgeSpawns mode=async → await; got %v", edgeTargets(spawns))
	}
}

func TestKotlinFunctionShape_ClassLevelGeneric(t *testing.T) {
	src := `package x

class Repo<T : Any>
`
	nodes, _ := runKotlinExtract(t, "x/Repo.kt", src)
	gp := nodesOfKind(nodes, graph.KindGenericParam)
	hasT := false
	for _, n := range gp {
		if n.Name == "T" {
			hasT = true
		}
	}
	if !hasT {
		t.Fatalf("expected KindGenericParam T at class level; got %v", nodeNames(gp))
	}
}

func TestKotlinAsyncSpawns_RunBlocking(t *testing.T) {
	src := `package x

fun blockingMain() = runBlocking {
    println("hi")
}
`
	_, edges := runKotlinExtract(t, "x/main.kt", src)
	spawns := edgesByKind(edges, graph.EdgeSpawns)
	hasRunBlocking := false
	for _, e := range spawns {
		if e.To == "unresolved::runBlocking" {
			hasRunBlocking = true
		}
	}
	if !hasRunBlocking {
		t.Errorf("expected EdgeSpawns → runBlocking; got %v", edgeTargets(spawns))
	}
}
