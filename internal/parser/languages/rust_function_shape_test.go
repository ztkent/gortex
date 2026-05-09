package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runRustExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ext := NewRustExtractor()
	result, err := ext.Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result.Nodes, result.Edges
}

func TestRustFunctionShape_FreeFn(t *testing.T) {
	src := `fn lookup(client: &Client, id: u64) -> Option<User> {
    None
}
`
	nodes, edges := runRustExtract(t, "src/lib.rs", src)

	params := nodesOfKind(nodes, graph.KindParam)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(params))
	}

	typed := edgesByKind(edges, graph.EdgeTypedAs)
	hasClient := false
	for _, e := range typed {
		if e.To == "unresolved::Client" {
			hasClient = true
		}
	}
	if !hasClient {
		t.Errorf("expected EdgeTypedAs → Client (& stripped); got %v", edgeTargets(typed))
	}

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → User (Option unwrapped); got %v", edgeTargets(returns))
	}
}

func TestRustFunctionShape_GenericFn(t *testing.T) {
	src := `fn first<T: Iterator>(it: T) -> Option<T::Item> { None }
`
	nodes, edges := runRustExtract(t, "src/lib.rs", src)

	gp := nodesOfKind(nodes, graph.KindGenericParam)
	hasT := false
	for _, n := range gp {
		if n.Name == "T" {
			hasT = true
			if b, _ := n.Meta["bound"].(string); b == "" {
				t.Errorf("KindGenericParam T missing bound annotation")
			}
		}
	}
	if !hasT {
		t.Errorf("expected KindGenericParam T; got %v", nodeNames(gp))
	}

	memberOf := edgesByKind(edges, graph.EdgeMemberOf)
	hasMember := false
	for _, e := range memberOf {
		if e.From == "src/lib.rs::first#tparam:T" && e.To == "src/lib.rs::first" {
			hasMember = true
		}
	}
	if !hasMember {
		t.Errorf("expected KindGenericParam → first EdgeMemberOf")
	}
}

func TestRustFunctionShape_SkipsSelfParam(t *testing.T) {
	src := `impl User {
    fn name(&self) -> &str { "" }
}
`
	nodes, _ := runRustExtract(t, "src/lib.rs", src)
	for _, n := range nodes {
		if n.Kind == graph.KindParam && n.Name == "self" {
			t.Errorf("KindParam %q should not be emitted (self receiver)", n.Name)
		}
	}
}

func TestRustFunctionShape_ResultTypeUnwrapsToOk(t *testing.T) {
	src := `fn load(path: &str) -> Result<User, std::io::Error> { unimplemented!() }
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → User (Result success arm); got %v", edgeTargets(returns))
	}
}

func TestRustAsyncSpawns_DotAwait(t *testing.T) {
	src := `async fn load(client: &Client) -> Option<User> {
    let u = client.fetch_user().await;
    let r = lookup(1).await;
    None
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	spawns := edgesByKind(edges, graph.EdgeSpawns)
	want := map[string]bool{"unresolved::fetch_user": false, "unresolved::lookup": false}
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

func TestRustAsyncSpawns_TokioSpawn(t *testing.T) {
	src := `async fn driver() {
    tokio::spawn(async move { worker().await; });
}
`
	_, edges := runRustExtract(t, "src/lib.rs", src)
	spawns := edgesByKind(edges, graph.EdgeSpawns)
	hasTokio := false
	for _, e := range spawns {
		if e.To == "unresolved::tokio::spawn" {
			hasTokio = true
		}
	}
	if !hasTokio {
		t.Errorf("expected EdgeSpawns → tokio::spawn; got %v", edgeTargets(spawns))
	}
}

func TestRustFunctionShape_StructGeneric(t *testing.T) {
	src := `pub struct Box<T> { inner: T }
`
	nodes, edges := runRustExtract(t, "src/lib.rs", src)
	gp := nodesOfKind(nodes, graph.KindGenericParam)
	hasT := false
	for _, n := range gp {
		if n.Name == "T" {
			hasT = true
		}
	}
	if !hasT {
		t.Fatalf("expected KindGenericParam T from struct; got %v", nodeNames(gp))
	}
	hasMember := false
	for _, e := range edges {
		if e.Kind == graph.EdgeMemberOf && e.From == "src/lib.rs::Box#tparam:T" && e.To == "src/lib.rs::Box" {
			hasMember = true
		}
	}
	if !hasMember {
		t.Errorf("expected struct generic EdgeMemberOf")
	}
}

func TestRustFunctionShape_TraitGeneric(t *testing.T) {
	src := `pub trait Repo<E> { fn list(&self) -> Vec<E>; }
`
	nodes, _ := runRustExtract(t, "src/lib.rs", src)
	gp := nodesOfKind(nodes, graph.KindGenericParam)
	hasE := false
	for _, n := range gp {
		if n.Name == "E" {
			hasE = true
		}
	}
	if !hasE {
		t.Errorf("expected KindGenericParam E from trait; got %v", nodeNames(gp))
	}
}

func TestCanonicalizeRustTypeRef(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"User", "User"},
		{"&User", "User"},
		{"&mut User", "User"},
		{"Box<User>", "User"},
		{"Arc<Mutex<User>>", "User"},
		{"Vec<User>", "User"},
		{"Option<User>", "User"},
		{"Result<User, std::io::Error>", "User"},
		{"impl Iterator", "Iterator"},
		{"dyn Display", "Display"},
		{"crate::models::User", "User"},
	}
	for _, c := range cases {
		if got := canonicalizeRustTypeRef(c.in); got != c.out {
			t.Errorf("canonicalizeRustTypeRef(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}
