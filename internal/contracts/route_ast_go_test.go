package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	gosrc "github.com/zzet/gortex/internal/parser/tsitter/golang"
)

func TestGoRouteAST_BasicRoutes(t *testing.T) {
	src := []byte(`package h

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /users", listUsers)
	mux.HandleFunc("/legacy", legacyHandler)
}

func chiRoutes(r chi.Router) {
	r.Get("/api/users", h.List)
	r.Post("/api/users", h.Create)
}

func fiberRoutes(app *fiber.App) {
	app.GET("/v1/items", listItems)
	app.DELETE("/v1/items/:id", deleteItem)
}
`)
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	matches := detectGoRoutesAST(tree.RootNode(), src)
	if len(matches) != 6 {
		t.Fatalf("matches: want 6, got %d (%+v)", len(matches), matches)
	}

	want := []struct{ method, path, framework string }{
		{"GET", "/users", "net/http"},
		{"ANY", "/legacy", "net/http"},
		{"GET", "/api/users", "gin/echo/chi"}, // r.Get(...) → TitleCase → gin/echo/chi label
		{"POST", "/api/users", "gin/echo/chi"},
		{"GET", "/v1/items", "fiber"},
		{"DELETE", "/v1/items/:id", "fiber"},
	}
	for i, w := range want {
		if i >= len(matches) {
			t.Errorf("missing match[%d]: %+v", i, w)
			continue
		}
		got := matches[i]
		if got.method != w.method || got.path != w.path || got.framework != w.framework {
			t.Errorf("match[%d]: want %s %s (%s), got %s %s (%s)",
				i, w.method, w.path, w.framework,
				got.method, got.path, got.framework)
		}
	}
}

// THE Fiber bug fixture: `[]byte(".GET(")` must NOT produce a route.
// This is the original failure that motivated the entire spec — the
// regex at http.go:97 matched `.GET(` inside its own prefilter-marker
// source, emitting a contract whose path contained the substring
// "[]byte(". The AST detector structurally rejects it: a string
// literal inside a type-conversion expression is not a route
// registration's path argument.
func TestGoRouteAST_FiberSelfReflexiveBugStructurallyImpossible(t *testing.T) {
	src := []byte(`package contracts

// httpPrefilterMarkers is the per-language substring prefilter.
var httpPrefilterMarkers = map[string][][]byte{
	"go": {
		[]byte("Handle"),
		[]byte(".GET("),    // fiber uppercase verbs
		[]byte(".POST("),
		[]byte(".PUT("),
		[]byte(".DELETE("),
	},
}
`)
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	matches := detectGoRoutesAST(tree.RootNode(), src)
	if len(matches) != 0 {
		t.Fatalf("AST detector matched non-routes: %+v", matches)
	}
}

func TestGoRouteAST_ConsumerHttpGetNotMatched(t *testing.T) {
	// http.Get(url) is a CONSUMER; the regex avoids it via a negative
	// look-behind on `r/g/e/...`. The AST detector handles it via
	// isHTTPStdlibConsumer.
	src := []byte(`package h
func fetch() {
	_, _ = http.Get("https://example.com")
}
`)
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	matches := detectGoRoutesAST(tree.RootNode(), src)
	if len(matches) != 0 {
		t.Fatalf("matched http.Get consumer: %+v", matches)
	}
}

// HTTPExtractor end-to-end: when the tree is provided, the AST
// detector runs and produces the expected contracts; the bug fixture
// produces zero contracts.
func TestHTTPExtractor_TreeAware_BugFixtureProducesNoBogusRoutes(t *testing.T) {
	src := []byte(`package contracts
var markers = [][]byte{
	[]byte(".GET("),
	[]byte(".POST("),
}
`)
	tree, err := parser.ParseFile(src, gosrc.GetLanguage())
	if err != nil {
		t.Fatal(err)
	}
	pt := parser.NewParseTree(tree, src, "go")
	defer pt.Release()

	ext := &HTTPExtractor{}
	out := ext.ExtractWithTree("contracts/markers.go", src, []*graph.Node{}, []*graph.Edge{}, pt)
	for _, c := range out {
		// Any route whose path contains marker text is the bug
		// recurring.
		if path, _ := c.Meta["path"].(string); path != "" {
			if containsAny(path, []string{"[]byte(", "GET(", "POST("}) {
				t.Errorf("bug-fixture produced bogus contract: id=%s path=%q", c.ID, path)
			}
		}
	}
	if t.Failed() {
		t.Logf("%d contracts produced", len(out))
	}
}

func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
