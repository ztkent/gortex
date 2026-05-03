package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

// makeGoBodyFacts is a test helper: parses Go source via tree-sitter
// and constructs a goBodyFacts for the function whose name matches.
// Returns the BodyFacts and a cleanup that releases the tree.
func makeGoBodyFacts(t *testing.T, src string, funcName string, startLine int) (BodyFacts, func()) {
	t.Helper()
	lang := gosrc.GetLanguage()
	tree, err := parser.ParseFile([]byte(src), lang)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pt := parser.NewParseTree(tree, []byte(src), "go")
	handler := &graph.Node{
		ID:        "test.go::" + funcName,
		Name:      funcName,
		Kind:      graph.KindFunction,
		FilePath:  "test.go",
		StartLine: startLine,
		Language:  "go",
	}
	bf := newGoBodyFacts(pt, handler)
	return bf, func() { pt.Release() }
}

func TestGoBodyFacts_VarBindingMethodCall(t *testing.T) {
	src := `package h

func ListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := h.svc.ListAll()
	if err != nil {
		WriteJSON(w, http.StatusInternalServerError, err)
		return
	}
	WriteJSON(w, http.StatusOK, repos)
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "ListRepos", 3)
	defer cleanup()
	b := bf.VarBinding("repos")
	if b.Kind != BindingMethodCall {
		t.Fatalf("repos kind: want method_call, got %q", b.Kind)
	}
	if b.CallExpr != "h.svc.ListAll" {
		t.Fatalf("repos callExpr: want %q, got %q", "h.svc.ListAll", b.CallExpr)
	}
}

func TestGoBodyFacts_VarBindingComposite(t *testing.T) {
	src := `package h
func H(w, r) {
	user := User{Name: "x"}
	users := []User{{Name: "y"}, {Name: "z"}}
	ptr := &User{}
	count := 0
	pi := 3.14
	flag := true
	name := r.PathValue("id")
	hdr := r.Header.Get("X-Foo")
	q := r.URL.Query().Get("k")
	form := r.FormValue("name")
	_ = make([]User, 0, 10)
	_ = make(map[string]int)
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()

	cases := []struct {
		name     string
		kind     BindingKind
		typeID   string
		repeated bool
		pointer  bool
	}{
		{"user", BindingComposite, "User", false, false},
		{"users", BindingSliceLit, "User", true, false},
		{"ptr", BindingComposite, "User", false, true},
		{"count", BindingIntLit, "int", false, false},
		{"pi", BindingFloatLit, "float64", false, false},
		{"flag", BindingBoolLit, "bool", false, false},
		{"name", BindingPathValue, "string", false, false},
		{"hdr", BindingHeaderValue, "string", false, false},
		{"q", BindingQueryGet, "string", false, false},
		{"form", BindingFormValue, "string", false, false},
	}
	for _, tc := range cases {
		b := bf.VarBinding(tc.name)
		if b.Kind != tc.kind {
			t.Errorf("%s kind: want %q, got %q", tc.name, tc.kind, b.Kind)
		}
		if b.TypeID != tc.typeID {
			t.Errorf("%s typeID: want %q, got %q", tc.name, tc.typeID, b.TypeID)
		}
		if b.Repeated != tc.repeated {
			t.Errorf("%s repeated: want %v, got %v", tc.name, tc.repeated, b.Repeated)
		}
		if b.Pointer != tc.pointer {
			t.Errorf("%s pointer: want %v, got %v", tc.name, tc.pointer, b.Pointer)
		}
	}
}

func TestGoBodyFacts_ResponseCalls(t *testing.T) {
	src := `package h
func H(w, r) {
	users := svc.List()
	WriteJSON(w, http.StatusOK, users)
	c.JSON(http.StatusCreated, payload)
	json.NewEncoder(w).Encode(out)
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()
	calls := bf.ResponseCalls()
	if len(calls) < 3 {
		t.Fatalf("response calls: want >=3, got %d", len(calls))
	}

	wj := findCall(calls, "WriteJSON")
	if wj == nil {
		t.Fatal("missing WriteJSON")
	}
	if wj.StatusCode != 200 || !wj.StatusKnown {
		t.Errorf("WriteJSON status: want 200, got %d (known=%v)", wj.StatusCode, wj.StatusKnown)
	}
	if wj.ValueExpr != "users" {
		t.Errorf("WriteJSON value: want users, got %q", wj.ValueExpr)
	}

	cj := findCall(calls, "JSON")
	if cj == nil {
		t.Fatal("missing JSON")
	}
	if cj.StatusCode != 201 {
		t.Errorf("c.JSON status: want 201, got %d", cj.StatusCode)
	}
	if cj.ValueExpr != "payload" {
		t.Errorf("c.JSON value: want payload, got %q", cj.ValueExpr)
	}

	enc := findCall(calls, "Encode")
	if enc == nil {
		t.Fatal("missing Encode")
	}
	if enc.ValueExpr != "out" {
		t.Errorf("Encode value: want out, got %q", enc.ValueExpr)
	}
}

func TestGoBodyFacts_MapLiteralEntries_NestedSlices(t *testing.T) {
	// This is the "[]any{ truncation" bug fixture: a multi-key
	// envelope where one value is a nested slice literal containing
	// `}` characters that the old splitMapLiteralBody truncated on.
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
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()

	calls := bf.ResponseCalls()
	wj := findCall(calls, "WriteJSON")
	if wj == nil {
		t.Fatal("missing WriteJSON")
	}
	if wj.ValueArg == nil {
		t.Fatal("WriteJSON value node nil")
	}
	if wj.ValueArg.Kind() != "composite_literal" {
		t.Fatalf("envelope kind: want composite_literal, got %q", wj.ValueArg.Kind())
	}
	entries := bf.MapLiteralEntries(wj.ValueArg)
	if len(entries) != 4 {
		t.Fatalf("envelope entries: want 4, got %d (%v)", len(entries), entries)
	}
	keys := []string{}
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	want := []string{"workspaces", "repos", "flags", "meta"}
	for i, k := range want {
		if i >= len(keys) || keys[i] != k {
			t.Errorf("envelope key[%d]: want %q, got %v", i, k, keys)
			break
		}
	}
	// Critical: the "flags" value is `[]any{true, false, true}`.
	// The old splitMapLiteralBody regex truncated this at the first
	// `}` it saw, producing `"expr": "[]any{"`. The AST walk gets the
	// full expression.
	flags := entries[2]
	if flags.ValueExpr != "[]any{true, false, true}" {
		t.Errorf("flags expr: want full slice literal, got %q", flags.ValueExpr)
	}
}

func TestGoBodyFacts_StatusWrites(t *testing.T) {
	src := `package h
func H(w, r) {
	w.WriteHeader(http.StatusBadRequest)
	w.WriteHeader(404)
	w.WriteHeader(http.StatusOK)
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()
	got := bf.StatusWrites()
	want := []int{400, 404, 200}
	if len(got) != len(want) {
		t.Fatalf("status writes: want %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("status[%d]: want %d, got %d", i, w, got[i])
		}
	}
}

func TestGoBodyFacts_QueryReads(t *testing.T) {
	src := `package h
func H(w, r) {
	a := r.URL.Query().Get("a")
	b := r.FormValue("b")
	c := r.Header.Get("X-C")
	_ = r.PostFormValue("d")
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()
	keys := bf.QueryReads()
	want := []string{"a", "b", "X-C", "d"}
	if len(keys) != len(want) {
		t.Fatalf("query reads: want %v, got %v", want, keys)
	}
	for i, w := range want {
		if keys[i] != w {
			t.Errorf("query[%d]: want %q, got %q", i, w, keys[i])
		}
	}
}

func TestGoBodyFacts_VarBindingMultiReturn(t *testing.T) {
	// `repos, err := svc.List()` — first LHS gets the call binding;
	// the second LHS (err) gets the same CallExpr but no resolved
	// type so the post-pass doesn't attribute repos's type to err.
	src := `package h
func H() {
	repos, err := svc.List()
	_ = err
	_ = repos
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()
	r := bf.VarBinding("repos")
	if r.Kind != BindingMethodCall || r.CallExpr != "svc.List" {
		t.Errorf("repos: want method_call svc.List, got %v", r)
	}
	e := bf.VarBinding("err")
	if e.Kind != BindingMethodCall {
		t.Errorf("err: want method_call (no type), got %v", e)
	}
}

func TestGoBodyFacts_RequestBindings(t *testing.T) {
	src := `package h
func H(w, r) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { return }
}
`
	bf, cleanup := makeGoBodyFacts(t, src, "H", 2)
	defer cleanup()
	rbs := bf.RequestBindings()
	if len(rbs) != 1 {
		t.Fatalf("request bindings: want 1, got %d (%+v)", len(rbs), rbs)
	}
	if rbs[0].Helper != "Decode" || rbs[0].VarName != "req" {
		t.Errorf("first binding: want Decode/req, got %+v", rbs[0])
	}

	// chase var binding for "req" — should have the declared type.
	b := bf.VarBinding("req")
	if b.TypeID != "CreateRequest" {
		t.Errorf("VarBinding(req): want CreateRequest, got %q", b.TypeID)
	}
}

func TestGoBodyFacts_RequestBindings_FrameworkVariants(t *testing.T) {
	src := `package h
func F(c *fiber.Ctx) {
	var f FiberReq
	c.BodyParser(&f)
}
func G(c *gin.Context) {
	var g GinReq
	c.ShouldBindJSON(&g)
}
func E(c echo.Context) {
	var e EchoReq
	c.Bind(&e)
}
func U() {
	var u UnmarshalReq
	json.Unmarshal(body, &u)
}
func A() {
	var anon AnonReq
	_ = anon
	json.NewDecoder(r.Body).Decode(&AnonReq{})
}
`
	cases := []struct {
		fn    string
		line  int
		want  string
		varN  string
	}{
		{"F", 2, "BodyParser", "f"},
		{"G", 6, "ShouldBindJSON", "g"},
		{"E", 10, "Bind", "e"},
		{"U", 14, "Unmarshal", "u"},
	}
	for _, tc := range cases {
		bf, cleanup := makeGoBodyFacts(t, src, tc.fn, tc.line)
		defer cleanup()
		rbs := bf.RequestBindings()
		if len(rbs) != 1 {
			t.Errorf("%s: want 1 binding, got %d", tc.fn, len(rbs))
			continue
		}
		if rbs[0].Helper != tc.want || rbs[0].VarName != tc.varN {
			t.Errorf("%s: want %s/%s, got %+v", tc.fn, tc.want, tc.varN, rbs[0])
		}
	}

	// Anonymous composite: Decode(&AnonReq{})
	bfA, cleanup := makeGoBodyFacts(t, src, "A", 18)
	defer cleanup()
	rbs := bfA.RequestBindings()
	if len(rbs) != 1 {
		t.Fatalf("A: want 1 binding, got %d", len(rbs))
	}
	if rbs[0].CompositeType != "AnonReq" {
		t.Errorf("A: want CompositeType=AnonReq, got %+v", rbs[0])
	}
}

func findCall(calls []ResponseCall, helper string) *ResponseCall {
	for i := range calls {
		if calls[i].Helper == helper {
			return &calls[i]
		}
	}
	return nil
}
