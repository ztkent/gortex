package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runCSharpExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ext := NewCSharpExtractor()
	result, err := ext.Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result.Nodes, result.Edges
}

func TestCSharpAsyncSpawns_AwaitInvocation(t *testing.T) {
	src := `using System.Threading.Tasks;

public class Svc {
    public async Task<User> Load(int id) {
        var u = await FetchUser(id);
        var r = await client.Query();
        return u;
    }
}
`
	_, edges := runCSharpExtract(t, "x/Svc.cs", src)

	spawns := edgesByKind(edges, graph.EdgeSpawns)
	want := map[string]bool{"unresolved::FetchUser": false, "unresolved::Query": false}
	for _, e := range spawns {
		if mode, _ := e.Meta["mode"].(string); mode != "async" {
			continue
		}
		if _, ok := want[e.To]; ok {
			want[e.To] = true
		}
	}
	for tgt, found := range want {
		if !found {
			t.Errorf("expected EdgeSpawns mode=async → %s; got %v", tgt, edgeTargets(spawns))
		}
	}
}

func TestCSharpFunctionShape_MethodParamsAndReturn(t *testing.T) {
	src := `using System.Threading.Tasks;

public class Svc {
	public Task<User> Lookup(int id, AuthCtx ctx) { return null; }
}
`
	nodes, edges := runCSharpExtract(t, "x/Svc.cs", src)

	params := nodesOfKind(nodes, graph.KindParam)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d: %v", len(params), nodeNames(params))
	}

	typed := edgesByKind(edges, graph.EdgeTypedAs)
	hasAuthCtx := false
	for _, e := range typed {
		if e.To == "unresolved::AuthCtx" {
			hasAuthCtx = true
		}
	}
	if !hasAuthCtx {
		t.Errorf("expected EdgeTypedAs → AuthCtx; got %v", edgeTargets(typed))
	}

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → User (Task unwrapped); got %v", edgeTargets(returns))
	}
}

func TestCSharpFunctionShape_ClassLevelGeneric(t *testing.T) {
	src := `public class Repo<T> {}
`
	nodes, _ := runCSharpExtract(t, "x/Repo.cs", src)
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

func TestCSharpFunctionShape_ConstructorParams(t *testing.T) {
	src := `public class Svc {
	public Svc(IRepo repo, IAuth auth) {}
}
`
	_, edges := runCSharpExtract(t, "x/Svc.cs", src)
	params := edgesByKind(edges, graph.EdgeParamOf)
	hasCtor := false
	for _, e := range params {
		if e.To == "x/Svc.cs::Svc.<init>" {
			hasCtor = true
		}
	}
	if !hasCtor {
		t.Errorf("expected EdgeParamOf for ctor; got %v", edgeTargets(params))
	}
	typed := edgesByKind(edges, graph.EdgeTypedAs)
	hasIRepo := false
	for _, e := range typed {
		if e.To == "unresolved::IRepo" {
			hasIRepo = true
		}
	}
	if !hasIRepo {
		t.Errorf("expected EdgeTypedAs → IRepo; got %v", edgeTargets(typed))
	}
}

func TestCSharpAsyncSpawns_TaskRun(t *testing.T) {
	src := `using System.Threading.Tasks;

public class Bg {
    public void Kick() {
        Task.Run(() => Worker());
        ThreadPool.QueueUserWorkItem(_ => Worker());
    }
}
`
	_, edges := runCSharpExtract(t, "x/Bg.cs", src)
	spawns := edgesByKind(edges, graph.EdgeSpawns)
	hasTaskRun := false
	hasThreadPool := false
	for _, e := range spawns {
		if e.To == "unresolved::Task.Run" {
			hasTaskRun = true
		}
		if e.To == "unresolved::ThreadPool.QueueUserWorkItem" {
			hasThreadPool = true
		}
	}
	if !hasTaskRun {
		t.Errorf("expected EdgeSpawns → Task.Run; got %v", edgeTargets(spawns))
	}
	if !hasThreadPool {
		t.Errorf("expected EdgeSpawns → ThreadPool.QueueUserWorkItem; got %v", edgeTargets(spawns))
	}
}
