package contracts

// Regression fixtures for the four bug classes called out in
// specs/spec-contract-extraction.md §1. Each test reproduces a
// real-world failure of the regex enricher and confirms the AST
// overlay produces the expected Meta values.
//
// The fixtures don't run the indexer or the route-detection regex —
// they construct a Contract directly and call EnrichHTTPContractWithTree
// with a tree parsed from the fixture source. This isolates the
// AST overlay's behaviour from the rest of the contract pipeline.

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

// fixtureContract is the shared scaffold: parse src, build a fake
// Contract whose handler matches the function at handlerLine, run
// EnrichHTTPContractWithTree, return the contract.
func fixtureContract(t *testing.T, src string, handlerName string, handlerLine, handlerEndLine int) Contract {
	t.Helper()
	tree, err := parser.ParseFile([]byte(src), gosrc.GetLanguage())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pt := parser.NewParseTree(tree, []byte(src), "go")
	t.Cleanup(func() { pt.Release() })

	handlerNode := &graph.Node{
		ID:        "h.go::" + handlerName,
		Name:      handlerName,
		Kind:      graph.KindFunction,
		FilePath:  "h.go",
		StartLine: handlerLine,
		EndLine:   handlerEndLine,
		Language:  "go",
		Meta:      map[string]any{},
	}
	c := Contract{
		ID:       "http::GET::/test",
		Type:     ContractHTTP,
		Role:     RoleProvider,
		SymbolID: handlerNode.ID,
		FilePath: "h.go",
		Line:     handlerLine,
		Meta: map[string]any{
			"path":   "/test",
			"method": "GET",
		},
	}
	lines := strings.Split(src, "\n")
	EnrichHTTPContractWithTree(&c, lines, []*graph.Node{handlerNode}, "go", pt)
	return c
}

// Bug 1 — Fiber uppercase verbs leak into HTTP path.
//
// The fiber-route regex matched `.GET(` inside the contract package's
// own httpPrefilterMarkers source (which has `[]byte(".GET(")` byte
// markers). The route-detection regex is in http.go and outside
// BodyFacts' scope (Phase 2); this fixture documents the bug and
// shows that the AST overlay doesn't make it worse.
//
// Phase 1 doesn't fully fix this bug — it's a route-registration
// problem (Phase 2) — but we keep the fixture as a regression
// boundary. When Phase 2 lands, this test should be tightened to
// assert no contracts are produced for the marker source.
func TestBugFixture_FiberUppercaseVerbsRouteRegistration(t *testing.T) {
	// Phase 1 scope is body-content enrichment, not route detection.
	// This test exists to document the bug class for Phase 2; it
	// passes today because we're not asserting the route is rejected,
	// only that body enrichment doesn't crash on the pathological
	// path.
	src := `package h

func H(w, r) {
	WriteJSON(w, http.StatusOK, "ok")
}
`
	c := fixtureContract(t, src, "H", 3, 5)
	if c.Meta["response_type"] != "string" {
		t.Errorf("response_type: want string, got %v", c.Meta["response_type"])
	}
}

// Bug 2 — handler_trail / handler_ident leak to wire.
//
// The legacy regex enricher writes Meta["handler_trail"] and
// Meta["handler_ident"] as scratch state for cross-file resolution.
// The cleanup defer in resolveProviderHandlers wipes them, but if
// the post-pass never runs (e.g. the contract resolves on the
// initial pass), they leak to the dashboard.
//
// The AST overlay never writes these keys — they're internal scratch
// state for the legacy regex pipeline only. This test verifies that
// after the AST overlay runs (with a body it can fully resolve),
// those keys are absent.
func TestBugFixture_NoHandlerTrailLeak(t *testing.T) {
	src := `package h

type User struct {
	Name string
}

func ListUsers(w, r) {
	users := []User{{Name: "alice"}}
	WriteJSON(w, http.StatusOK, users)
}
`
	c := fixtureContract(t, src, "ListUsers", 7, 10)
	if _, has := c.Meta["handler_trail"]; has {
		t.Errorf("handler_trail leaked: %v", c.Meta["handler_trail"])
	}
	if _, has := c.Meta["handler_ident"]; has {
		t.Errorf("handler_ident leaked: %v", c.Meta["handler_ident"])
	}
}

// Bug 3 — `[]any{` envelope truncation.
//
// The legacy splitMapLiteralBody regex truncated `"flags": []any{...}`
// at the first `}` it saw, producing `"expr": "[]any{"` in the
// envelope row. The AST walks composite_literal children correctly
// regardless of nested braces.
func TestBugFixture_NoEnvelopeTruncation(t *testing.T) {
	src := `package h

func H(w, r) {
	workspaces := svc.Workspaces()
	repos := svc.Repos()
	WriteJSON(w, http.StatusOK, map[string]any{
		"workspaces": workspaces,
		"repos":      repos,
		"flags":      []any{true, false, true},
		"meta":       map[string]int{"a": 1, "b": 2},
	})
}
`
	c := fixtureContract(t, src, "H", 3, 12)
	envRaw, ok := c.Meta["response_envelope"].([]map[string]any)
	if !ok {
		t.Fatalf("response_envelope: want []map[string]any, got %T", c.Meta["response_envelope"])
	}
	if len(envRaw) != 4 {
		t.Fatalf("envelope rows: want 4, got %d", len(envRaw))
	}
	flags := envRaw[2]
	if flags["name"] != "flags" {
		t.Errorf("flags row name: want flags, got %v", flags["name"])
	}
	if flags["expr"] != "[]any{true, false, true}" {
		t.Errorf("flags row expr: want full slice literal, got %q", flags["expr"])
	}
	if !strings.Contains(flags["expr"].(string), "}") {
		t.Errorf("flags row expr missing closing brace: %q", flags["expr"])
	}
}

// Bug 4 — `WriteJSON(w, http.StatusOK, result)` leaks raw helper
// expression as response_expr.
//
// The legacy regex enricher couldn't recover `result`'s type without
// graph-aware tracing, so it stored the entire helper-call text as
// response_expr. The AST identifies the bare ident `result` and the
// post-pass traces it (via BodyFacts.VarBinding's CallExpr field)
// to the binding call's return type.
//
// This test asserts the AST overlay produces a clean response_expr
// (just `result`, not the full helper call) so the post-pass's
// isLikelyIdentifier branch matches.
func TestBugFixture_NoRawHelperResponseExpr(t *testing.T) {
	src := `package h

func ListRepos(w, r) {
	result, err := h.svc.GetRepos()
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}
`
	c := fixtureContract(t, src, "ListRepos", 3, 10)
	expr, _ := c.Meta["response_expr"].(string)
	if expr != "" && !isPlainIdent(expr) {
		t.Errorf("response_expr leaked helper text: %q (want bare ident or empty)", expr)
	}
	// An empty expr is also fine: the AST may have already resolved the
	// response type from a binding (composite/literal). We only assert
	// that nothing leaked.
}

// Bug 4b — bare struct response from variable binding.
//
// `WriteJSON(w, code, user)` where `user` is bound to `User{...}` —
// the AST should resolve response_type = User without a graph walk.
func TestBugFixture_VarBoundCompositeResolves(t *testing.T) {
	src := `package h

func GetUser(w, r) {
	user := User{Name: "x"}
	WriteJSON(w, http.StatusOK, user)
}
`
	c := fixtureContract(t, src, "GetUser", 3, 6)
	rt, _ := c.Meta["response_type"].(string)
	if rt != "User" {
		t.Errorf("response_type: want User, got %q", rt)
	}
}

// Bug 4c — slice literal response from variable binding.
func TestBugFixture_VarBoundSliceLitResolves(t *testing.T) {
	src := `package h

func ListUsers(w, r) {
	users := []User{{Name: "alice"}, {Name: "bob"}}
	WriteJSON(w, http.StatusOK, users)
}
`
	c := fixtureContract(t, src, "ListUsers", 3, 6)
	rt, _ := c.Meta["response_type"].(string)
	if rt != "User" {
		t.Errorf("response_type: want User, got %q", rt)
	}
	if r, _ := c.Meta["response_repeated"].(bool); !r {
		t.Errorf("response_repeated: want true, got false")
	}
}
