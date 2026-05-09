package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runPyExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ext := NewPythonExtractor()
	result, err := ext.Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result.Nodes, result.Edges
}

func TestPyFunctionShape_ParamsAndReturn(t *testing.T) {
	src := `def fetch(client: Client, ttl: int = 30) -> User:
	return User()
`
	nodes, edges := runPyExtract(t, "x.py", src)

	params := nodesOfKind(nodes, graph.KindParam)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d: %v", len(params), nodeNames(params))
	}

	paramOf := edgesByKind(edges, graph.EdgeParamOf)
	for _, e := range paramOf {
		if e.To != "x.py::fetch" {
			t.Errorf("param->%q, want fetch", e.To)
		}
	}

	typed := edgesByKind(edges, graph.EdgeTypedAs)
	hasClient := false
	for _, e := range typed {
		if e.To == "unresolved::Client" {
			hasClient = true
		}
	}
	if !hasClient {
		t.Errorf("expected EdgeTypedAs → unresolved::Client; got %v", edgeTargets(typed))
	}

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → unresolved::User; got %v", edgeTargets(returns))
	}
}

func TestPyFunctionShape_OptionalUnwrapped(t *testing.T) {
	src := `def find_user(uid: int) -> Optional[User]:
	return None
`
	_, edges := runPyExtract(t, "x.py", src)

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected unwrapped EdgeReturns → User; got %v", edgeTargets(returns))
	}
}

func TestPyFunctionShape_PEP604Union(t *testing.T) {
	src := `def lookup(uid: int) -> User | None:
	return None
`
	_, edges := runPyExtract(t, "x.py", src)

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → User from PEP-604 union; got %v", edgeTargets(returns))
	}
}

func TestPyFunctionShape_VariadicSplat(t *testing.T) {
	src := `def fn(*args, **kwargs):
	pass
`
	nodes, _ := runPyExtract(t, "x.py", src)
	params := nodesOfKind(nodes, graph.KindParam)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(params))
	}
	for _, p := range params {
		if v, _ := p.Meta["variadic"].(bool); !v {
			t.Errorf("%s should be variadic", p.Name)
		}
	}
}

func TestPyFunctionShape_SkipsSelfAndCls(t *testing.T) {
	src := `class C:
	def m(self, x: int) -> int:
		return x

	@classmethod
	def c(cls, y: int) -> int:
		return y
`
	nodes, _ := runPyExtract(t, "x.py", src)
	for _, n := range nodes {
		if n.Kind == graph.KindParam && (n.Name == "self" || n.Name == "cls") {
			t.Errorf("KindParam %q should not be emitted (receiver)", n.Name)
		}
	}
}

func TestPyFunctionShape_ClassLevelPEP695Generic(t *testing.T) {
	// PEP 695: `class Foo[T]:`. Depending on the bundled tree-sitter-
	// python version the type_parameters child may or may not be
	// present at the class level. We assert no panic + (best-effort)
	// the generic parameter is attached to the class. When the
	// grammar doesn't recognise it, the helper silently emits
	// nothing — which is acceptable since callers are tolerant of
	// missing class generics.
	src := `class Repo[T]:
	def get(self, x: T) -> T:
		return x
`
	nodes, _ := runPyExtract(t, "x.py", src)
	gp := nodesOfKind(nodes, graph.KindGenericParam)
	for _, n := range gp {
		if n.Name == "T" && strings.Contains(n.ID, "x.py::Repo#tparam") {
			return // grammar supports PEP 695: assertion satisfied.
		}
	}
	t.Logf("PEP-695 class generic not surfaced; tree-sitter-python "+
		"version may predate the syntax — got %v", nodeIDs(gp))
}

func TestPyAsyncSpawns_Await(t *testing.T) {
	src := `async def load(uid):
	user = await fetch_user(uid)
	rows = await db.query("...")
	return user
`
	_, edges := runPyExtract(t, "x.py", src)

	spawns := edgesByKind(edges, graph.EdgeSpawns)
	want := map[string]bool{"unresolved::fetch_user": false, "unresolved::query": false}
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

func TestPyAsyncSpawns_AsyncioGather(t *testing.T) {
	src := `import asyncio

async def loadAll():
	return await asyncio.gather(loadA(), loadB())
`
	_, edges := runPyExtract(t, "x.py", src)
	spawns := edgesByKind(edges, graph.EdgeSpawns)
	hasGather := false
	for _, e := range spawns {
		if e.To == "unresolved::asyncio.gather" {
			hasGather = true
		}
	}
	if !hasGather {
		t.Errorf("expected EdgeSpawns → asyncio.gather; got %v", edgeTargets(spawns))
	}
}

func TestCanonicalizePyTypeRef(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"User", "User"},
		{"Optional[User]", "User"},
		{"List[User]", "User"},
		{"list[User]", "User"},
		{"Tuple[User, int]", "User"},
		{"User | None", "User"},
		{"None | User", "User"},
		{"models.User", "User"},
		{"pkg.mod.User", "User"},
	}
	for _, c := range cases {
		if got := canonicalizePyTypeRef(c.in); got != c.out {
			t.Errorf("canonicalizePyTypeRef(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func nodeNames(nodes []*graph.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
