package languages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func runTSExtract(t *testing.T, path, src string) ([]*graph.Node, []*graph.Edge) {
	t.Helper()
	ext := NewTypeScriptExtractor()
	result, err := ext.Extract(path, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return result.Nodes, result.Edges
}

func TestTSFunctionShape_FunctionParamsAndReturn(t *testing.T) {
	src := `function greet(name: string, age: number): User {
	return { name, age };
}
`
	_, edges := runTSExtract(t, "src/a.ts", src)

	// EdgeParamOf for both params.
	paramEdges := edgesByKind(edges, graph.EdgeParamOf)
	if len(paramEdges) != 2 {
		t.Fatalf("expected 2 EdgeParamOf, got %d", len(paramEdges))
	}
	for _, e := range paramEdges {
		if e.To != "src/a.ts::greet" {
			t.Errorf("ParamOf target = %q, want greet", e.To)
		}
	}

	// EdgeTypedAs is omitted for primitives (string, number).
	// We only emit the named-type bindings — verifying behaviour
	// consistent with Promise / Array unwrapping below.

	// EdgeReturns to User.
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

func TestTSFunctionShape_GenericTypeParam(t *testing.T) {
	src := `function identity<T>(x: T): T { return x; }
`
	nodes, edges := runTSExtract(t, "src/g.ts", src)

	// KindGenericParam node for T, EdgeMemberOf back to function.
	var gpID string
	for _, n := range nodes {
		if n.Kind == graph.KindGenericParam && n.Name == "T" {
			gpID = n.ID
		}
	}
	if gpID == "" {
		t.Fatalf("KindGenericParam T missing")
	}
	hasMember := false
	for _, e := range edges {
		if e.Kind == graph.EdgeMemberOf && e.From == gpID && e.To == "src/g.ts::identity" {
			hasMember = true
		}
	}
	if !hasMember {
		t.Errorf("KindGenericParam → identity EdgeMemberOf missing")
	}
}

func TestTSFunctionShape_ClassMethod(t *testing.T) {
	src := `class UserService {
	getById(id: number): User | null { return null; }
}
`
	_, edges := runTSExtract(t, "src/svc.ts", src)

	params := edgesByKind(edges, graph.EdgeParamOf)
	hasGetById := false
	for _, e := range params {
		if e.To == "src/svc.ts::UserService.getById" {
			hasGetById = true
		}
	}
	if !hasGetById {
		t.Errorf("expected EdgeParamOf → UserService.getById; targets=%v", edgeTargets(params))
	}

	// Union return type emits one edge per non-primitive branch:
	// "User | null" → EdgeReturns → unresolved::User (null is primitive).
	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected EdgeReturns → unresolved::User from union; got %v", edgeTargets(returns))
	}
}

func TestTSFunctionShape_VariadicAndOptional(t *testing.T) {
	src := `function fn(a: string, b?: number, ...rest: string[]) {}
`
	nodes, _ := runTSExtract(t, "src/v.ts", src)
	params := nodesOfKind(nodes, graph.KindParam)
	if len(params) != 3 {
		t.Fatalf("expected 3 params, got %d", len(params))
	}
	var rest *graph.Node
	for _, p := range params {
		if p.Name == "rest" {
			rest = p
		}
	}
	if rest == nil {
		t.Fatalf("rest param missing")
	}
	if v, _ := rest.Meta["variadic"].(bool); !v {
		t.Errorf("rest.Meta.variadic = false; want true")
	}
}

func TestTSFunctionShape_ArrayAndPromiseReturnUnwrapped(t *testing.T) {
	src := `function loadAll(): Promise<User[]> { return null as any; }
`
	_, edges := runTSExtract(t, "src/p.ts", src)
	returns := edgesByKind(edges, graph.EdgeReturns)
	hasUser := false
	for _, e := range returns {
		if e.To == "unresolved::User" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Errorf("expected unwrapped EdgeReturns → unresolved::User; got %v", edgeTargets(returns))
	}
}

func TestTSFunctionShape_ArrowFieldNestJsControllerStyle(t *testing.T) {
	// NestJS controllers and route registries set arrow-shaped fields
	// inside an object — the params/returns should still get
	// function-shape edges so cross-file refactors land properly.
	src := `export const api = {
	health: async (req: Request): Promise<Health> => buildHealth(req),
};
`
	_, edges := runTSExtract(t, "src/api.ts", src)

	paramEdges := edgesByKind(edges, graph.EdgeParamOf)
	hasReq := false
	for _, e := range paramEdges {
		if e.To != "" && (e.To == "src/api.ts::api.health@2" || // colocated id
			e.To == "src/api.ts::api.health") {
			hasReq = true
		}
	}
	if !hasReq {
		t.Errorf("expected EdgeParamOf for arrow-field method; got %v", edgeTargets(paramEdges))
	}

	returns := edgesByKind(edges, graph.EdgeReturns)
	hasHealth := false
	for _, e := range returns {
		if e.To == "unresolved::Health" {
			hasHealth = true
		}
	}
	if !hasHealth {
		t.Errorf("expected EdgeReturns → unresolved::Health (Promise unwrapped); got %v", edgeTargets(returns))
	}
}

func TestTSAsyncSpawns_AwaitedCall(t *testing.T) {
	src := `async function load(id: string) {
	const u = await fetchUser(id);
	const r = await this.repo.find(id);
	return u;
}
`
	_, edges := runTSExtract(t, "src/a.ts", src)

	spawns := edgesByKind(edges, graph.EdgeSpawns)
	wantTargets := map[string]bool{"unresolved::fetchUser": false, "unresolved::find": false}
	for _, e := range spawns {
		if mode, _ := e.Meta["mode"].(string); mode != "async" {
			continue
		}
		if _, ok := wantTargets[e.To]; ok {
			wantTargets[e.To] = true
		}
	}
	for tgt, found := range wantTargets {
		if !found {
			t.Errorf("expected EdgeSpawns mode=async → %s; got %v", tgt, edgeTargets(spawns))
		}
	}
}

func TestTSAsyncSpawns_PromiseAll(t *testing.T) {
	src := `async function loadAll() {
	return await Promise.all([loadA(), loadB()]);
}
`
	_, edges := runTSExtract(t, "src/p.ts", src)

	spawns := edgesByKind(edges, graph.EdgeSpawns)
	hasPromiseAll := false
	for _, e := range spawns {
		if e.To == "unresolved::Promise.all" {
			hasPromiseAll = true
		}
	}
	if !hasPromiseAll {
		t.Errorf("expected EdgeSpawns → Promise.all; got %v", edgeTargets(spawns))
	}
}

func TestTSAsyncSpawns_NestedFunctionScopeRespected(t *testing.T) {
	src := `function outer() {
	function inner() {
		return foo();
	}
	return inner;
}
`
	_, edges := runTSExtract(t, "src/n.ts", src)
	// `foo` is called by inner, NOT awaited, so no spawn edge.
	for _, e := range edgesByKind(edges, graph.EdgeSpawns) {
		t.Errorf("unexpected EdgeSpawns %v", e.To)
	}
}

func TestTSFieldAccess_Writes(t *testing.T) {
	src := `class Server {
	private port: number = 0;
	private addr: string = "";
	configure(p: number) {
		this.port = p;
		this.addr += "x";
		this.port++;
	}
}
`
	_, edges := runTSExtract(t, "src/srv.ts", src)
	writes := edgesByKind(edges, graph.EdgeWrites)
	hasPort, hasAddr := false, false
	for _, e := range writes {
		if e.To == "unresolved::*.port" {
			hasPort = true
		}
		if e.To == "unresolved::*.addr" {
			hasAddr = true
		}
	}
	if !hasPort || !hasAddr {
		t.Errorf("expected EdgeWrites for port and addr; got %v", edgeTargets(writes))
	}
}

func TestTSFieldAccess_Reads(t *testing.T) {
	src := `class Server {
	private port: number = 0;
	snapshot(): number {
		return this.port;
	}
}
`
	_, edges := runTSExtract(t, "src/srv.ts", src)
	reads := edgesByKind(edges, graph.EdgeReads)
	hasPort := false
	for _, e := range reads {
		if e.To == "unresolved::*.port" {
			hasPort = true
		}
	}
	if !hasPort {
		t.Errorf("expected EdgeReads → unresolved::*.port; got %v", edgeTargets(reads))
	}
}

func TestTSFieldAccess_AugmentedAssignReadsAndWrites(t *testing.T) {
	src := `class Counter {
	count: number = 0;
	bump() {
		this.count += 1;
	}
}
`
	_, edges := runTSExtract(t, "src/c.ts", src)
	writes := edgesByKind(edges, graph.EdgeWrites)
	reads := edgesByKind(edges, graph.EdgeReads)
	hasWrite, hasRead := false, false
	for _, e := range writes {
		if e.To == "unresolved::*.count" {
			hasWrite = true
		}
	}
	for _, e := range reads {
		if e.To == "unresolved::*.count" {
			hasRead = true
		}
	}
	if !hasWrite || !hasRead {
		t.Errorf("expected EdgeWrites + EdgeReads on count for `+= ` op; got W=%v R=%v",
			edgeTargets(writes), edgeTargets(reads))
	}
}

func TestCanonicalizeTSTypeRef(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"User", "User"},
		{"User[]", "User"},
		{"Promise<User>", "User"},
		{"Promise<User[]>", "User"},
		{"ReadonlyArray<string>", "string"},
		{"readonly User[]", "User"},
		{"(User)", "User"},
	}
	for _, c := range cases {
		if got := canonicalizeTSTypeRef(c.in); got != c.out {
			t.Errorf("canonicalizeTSTypeRef(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestSplitTSUnionType(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"User", []string{"User"}},
		{"User | null", []string{"User", "null"}},
		{": User | null | undefined", []string{"User", "null", "undefined"}},
		{"Map<string, User | null>", []string{"Map<string, User | null>"}}, // top-level only
		{"Promise<User> | Error", []string{"Promise<User>", "Error"}},
		// Intersection types split too — each component is a type the
		// value simultaneously satisfies.
		{"Props & RefAttributes", []string{"Props", "RefAttributes"}},
		{"A & B | C", []string{"A", "B", "C"}},
		{"Map<A & B, C>", []string{"Map<A & B, C>"}}, // intersection nested in generic stays whole
		// Arrow function return types: the `>` in `=>` must not underflow
		// depth and defeat the later top-level `|` split.
		{"(x: number) => string | null", []string{"(x: number) => string", "null"}},
		// Empty / stray-delimiter members are dropped, not emitted blank.
		{"User |", []string{"User"}},
	}
	for _, c := range cases {
		got := splitTSUnionType(c.in)
		if !sliceEq(got, c.want) {
			t.Errorf("splitTSUnionType(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// Overflow guard: a pathological branch count past maxTSUnionMembers
	// is treated as opaque — the splitter returns nil so no per-branch
	// EdgeReturns flood the graph.
	t.Run("overflow_guard", func(t *testing.T) {
		members := make([]string, 0, maxTSUnionMembers+5)
		for i := range maxTSUnionMembers + 5 {
			members = append(members, fmt.Sprintf("'lit%d'", i))
		}
		big := strings.Join(members, " | ")
		if got := splitTSUnionType(big); got != nil {
			t.Errorf("splitTSUnionType(<%d-member union>) = %v, want nil (overflow)", len(members), got)
		}
		// Exactly at the cap is still split.
		atCap := strings.Join(members[:maxTSUnionMembers], " | ")
		if got := splitTSUnionType(atCap); len(got) != maxTSUnionMembers {
			t.Errorf("splitTSUnionType(<%d-member union>) = %d parts, want %d", maxTSUnionMembers, len(got), maxTSUnionMembers)
		}
	})
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nodesOfKind / edgesByKind helpers used by extractor tests are in
// other test files; we redeclare nothing here.
func edgesByKind(edges []*graph.Edge, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}
