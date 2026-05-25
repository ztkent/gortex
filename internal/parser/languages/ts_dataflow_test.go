package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// runTSLocalExtract is a thin adapter over the package's runTSExtract
// (declared in ts_function_shape_test.go) that returns the nodes and
// edges as a single struct convenient for the binding assertions
// below.
type tsLocalFixture struct {
	nodes []*graph.Node
	edges []*graph.Edge
}

func runTSLocalExtract(t *testing.T, fileName, src string) tsLocalFixture {
	t.Helper()
	nodes, edges := runTSExtract(t, "pkg/"+fileName, src)
	return tsLocalFixture{nodes: nodes, edges: edges}
}

// TestEmitTSLocalBindings_LetConstVar covers the headline case:
// `let`, `const`, `var` declarations each produce a KindLocal node
// anchored to the enclosing function via EdgeMemberOf, with a
// function-relative offset ID so the binding stays stable across
// edits above the function.
func TestEmitTSLocalBindings_LetConstVar(t *testing.T) {
	src := `function handler(req: any): string {
	const raw = req.headers.authorization;
	let token = raw.replace("Bearer ", "");
	var fallback = "anon";
	return token || fallback;
}
`
	result := runTSLocalExtract(t, "auth.ts", src)
	owner := "pkg/auth.ts::handler"

	locals := map[string]*graph.Node{}
	for _, n := range result.nodes {
		if n.Kind == graph.KindLocal {
			locals[n.Name] = n
		}
	}
	for _, want := range []string{"raw", "token", "fallback"} {
		n, ok := locals[want]
		require.Truef(t, ok, "missing KindLocal %q; got %v", want, mapKeys(locals))
		assert.Equal(t, graph.KindLocal, n.Kind)
		assert.Equal(t, "pkg/auth.ts", n.FilePath)
		assert.Truef(t, strings.HasPrefix(n.ID, owner+"#local:"+want+"@+"),
			"local %q ID must be function-relative; got %q", want, n.ID)
	}

	// Every local must have an EdgeMemberOf back to the owner.
	memberFor := map[string]string{}
	for _, e := range result.edges {
		if e.Kind == graph.EdgeMemberOf {
			memberFor[e.From] = e.To
		}
	}
	for _, n := range locals {
		assert.Equal(t, owner, memberFor[n.ID],
			"local %q must own-link to enclosing function", n.Name)
	}
}

// TestEmitTSLocalBindings_DestructurePatterns ensures the walker
// handles object and array destructure patterns — common in JS/TS
// codebases (`const { foo, bar: aliased } = obj`).
func TestEmitTSLocalBindings_DestructurePatterns(t *testing.T) {
	src := `function unpack(obj: any) {
	const { foo, bar: aliased } = obj;
	const [first, second] = obj.list;
}
`
	result := runTSLocalExtract(t, "unpack.ts", src)
	names := map[string]bool{}
	for _, n := range result.nodes {
		if n.Kind == graph.KindLocal {
			names[n.Name] = true
		}
	}
	for _, want := range []string{"foo", "aliased", "first", "second"} {
		assert.Truef(t, names[want], "missing KindLocal for destructure %q; got %v", want, names)
	}
}

// TestEmitTSLocalBindings_ForOfBinding covers for-of induction vars
// — the parser's other binding-introduction site beyond plain
// declarations.
func TestEmitTSLocalBindings_ForOfBinding(t *testing.T) {
	src := `function each(items: any[]) {
	for (const item of items) {
		const inner = item.value;
	}
}
`
	result := runTSLocalExtract(t, "each.ts", src)
	names := map[string]bool{}
	for _, n := range result.nodes {
		if n.Kind == graph.KindLocal {
			names[n.Name] = true
		}
	}
	assert.True(t, names["item"], "for-of induction var must be materialised")
	assert.True(t, names["inner"], "binding inside the loop body must be materialised")
}

// TestEmitTSLocalBindings_NestedFunctionsScopeIsolated guards the
// walker against descending into nested functions (their bindings
// belong to their own scope, not the outer function's).
func TestEmitTSLocalBindings_NestedFunctionsScopeIsolated(t *testing.T) {
	src := `function outer() {
	const x = 1;
	function inner() {
		const y = 2;
	}
}
`
	result := runTSLocalExtract(t, "nested.ts", src)
	outerOwner := "pkg/nested.ts::outer"
	memberOwners := map[string]string{}
	for _, e := range result.edges {
		if e.Kind == graph.EdgeMemberOf {
			memberOwners[e.From] = e.To
		}
	}
	for _, n := range result.nodes {
		if n.Kind != graph.KindLocal {
			continue
		}
		switch n.Name {
		case "x":
			assert.Equal(t, outerOwner, memberOwners[n.ID],
				"outer's local must own-link to outer")
		case "y":
			assert.NotEqual(t, outerOwner, memberOwners[n.ID],
				"inner's local must NOT own-link to outer — different scope")
		}
	}
}

// TestEmitTSLocalBindings_FunctionRelativeOffsetIsStable mirrors the
// Go regression at #76: adding a line above the function must NOT
// shift any local-binding ID inside it.
func TestEmitTSLocalBindings_FunctionRelativeOffsetIsStable(t *testing.T) {
	orig := `function f() {
	const x = 1;
	const y = 2;
}
`
	shifted := `// header
// header
// header
function f() {
	const x = 1;
	const y = 2;
}
`
	collect := func(t *testing.T, src string) map[string]struct{} {
		t.Helper()
		ids := map[string]struct{}{}
		for _, n := range runTSLocalExtract(t, "stable.ts", src).nodes {
			if n.Kind == graph.KindLocal {
				ids[n.ID] = struct{}{}
			}
		}
		return ids
	}
	a := collect(t, orig)
	b := collect(t, shifted)
	assert.NotEmpty(t, a)
	assert.Equal(t, a, b,
		"local IDs must stay stable when only lines ABOVE the function move")
}

func mapKeys(m map[string]*graph.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
