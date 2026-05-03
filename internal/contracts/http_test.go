package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func makeNodes(filePath string, fns []struct {
	name       string
	start, end int
}) []*graph.Node {
	var nodes []*graph.Node
	for _, f := range fns {
		nodes = append(nodes, &graph.Node{
			ID:        filePath + "::" + f.name,
			Kind:      graph.KindFunction,
			Name:      f.name,
			FilePath:  filePath,
			StartLine: f.start,
			EndLine:   f.end,
		})
	}
	return nodes
}

// TestHTTPExtractor_Go_Gin_HandlerResolution exercises the T1.3 path: when
// the pattern captures the handler identifier (e.g. "listUsers" in
// r.GET("/users", listUsers)) AND that identifier resolves to a function
// node in the same file, the Contract's SymbolID is the handler — not the
// enclosing setupRoutes function. Cross-service traversals landing on the
// provider side then reach business logic, not the router glue.
//
// When the handler doesn't resolve (e.g. lambda, method expr, different
// file) the code falls back to enclosing-symbol behavior — covered by
// TestHTTPExtractor_Go_Gin above, which deliberately omits handler nodes.
func TestHTTPExtractor_Go_Gin_HandlerResolution(t *testing.T) {
	src := []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
	r.POST("/api/users", createUser)
}

func listUsers()  {}
func createUser() {}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"setupRoutes", 5, 8},
		{"listUsers", 10, 10},
		{"createUser", 11, 11},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)

	byPath := map[string]Contract{}
	for _, c := range contracts {
		byPath[c.ID] = c
	}

	get := byPath["http::GET::/api/users"]
	if get.SymbolID != "main.go::listUsers" {
		t.Errorf("GET handler: expected SymbolID=main.go::listUsers (handler), got %q", get.SymbolID)
	}

	post := byPath["http::POST::/api/users"]
	if post.SymbolID != "main.go::createUser" {
		t.Errorf("POST handler: expected SymbolID=main.go::createUser (handler), got %q", post.SymbolID)
	}
}

func TestHTTPExtractor_Go_Gin(t *testing.T) {
	src := []byte(`package main

import "github.com/gin-gonic/gin"

func setupRoutes(r *gin.Engine) {
	r.GET("/api/users", listUsers)
	r.POST("/api/users", createUser)
	r.GET("/api/users/:id", getUser)
	r.DELETE("/api/users/:id", deleteUser)
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"setupRoutes", 5, 10},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)

	if len(contracts) != 4 {
		t.Fatalf("expected 4 contracts, got %d", len(contracts))
	}

	// Check first contract.
	c := contracts[0]
	if c.Type != ContractHTTP {
		t.Errorf("expected type http, got %s", c.Type)
	}
	if c.Role != RoleProvider {
		t.Errorf("expected role provider, got %s", c.Role)
	}
	if c.Meta["method"] != "GET" {
		t.Errorf("expected method GET, got %s", c.Meta["method"])
	}
	if c.Meta["path"] != "/api/users" {
		t.Errorf("expected path /api/users, got %s", c.Meta["path"])
	}
	if c.ID != "http::GET::/api/users" {
		t.Errorf("expected ID http::GET::/api/users, got %s", c.ID)
	}
	if c.SymbolID != "main.go::setupRoutes" {
		t.Errorf("expected symbol main.go::setupRoutes, got %s", c.SymbolID)
	}
	if c.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", c.Confidence)
	}

	// Check path param normalisation. Param names collapse to
	// positional {p1}, {p2}, ... so a provider's {id} and a
	// consumer's {userId} hash to the same contract ID.
	c3 := contracts[2]
	if c3.Meta["path"] != "/api/users/{p1}" {
		t.Errorf("expected normalised path /api/users/{p1}, got %s", c3.Meta["path"])
	}
}

func TestHTTPExtractor_TypeScript_Express(t *testing.T) {
	src := []byte(`
import express from 'express';
const app = express();

function registerRoutes() {
  app.get('/api/products', listProducts);
  app.post('/api/products', createProduct);
  app.get('/api/products/:id', getProduct);
}
`)
	nodes := makeNodes("routes.ts", []struct {
		name       string
		start, end int
	}{
		{"registerRoutes", 5, 9},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("routes.ts", src, nodes, nil)

	if len(contracts) != 3 {
		t.Fatalf("expected 3 contracts, got %d", len(contracts))
	}

	c := contracts[0]
	if c.Meta["framework"] != "express" {
		t.Errorf("expected framework express, got %s", c.Meta["framework"])
	}
	if c.Meta["method"] != "GET" {
		t.Errorf("expected method GET, got %s", c.Meta["method"])
	}
	if c.Meta["path"] != "/api/products" {
		t.Errorf("expected path /api/products, got %s", c.Meta["path"])
	}
}

func TestHTTPExtractor_Python_FastAPI(t *testing.T) {
	src := []byte(`
from fastapi import FastAPI
app = FastAPI()

@app.get("/api/items")
def list_items():
    return []

@app.post("/api/items")
def create_item(item: Item):
    return item

@app.get("/api/items/{item_id}")
def get_item(item_id: int):
    return item_id
`)
	nodes := makeNodes("main.py", []struct {
		name       string
		start, end int
	}{
		{"list_items", 6, 7},
		{"create_item", 10, 11},
		{"get_item", 14, 15},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.py", src, nodes, nil)

	if len(contracts) != 3 {
		t.Fatalf("expected 3 contracts, got %d", len(contracts))
	}

	if contracts[0].Meta["method"] != "GET" {
		t.Errorf("expected GET, got %s", contracts[0].Meta["method"])
	}
	if contracts[0].Meta["path"] != "/api/items" {
		t.Errorf("expected /api/items, got %s", contracts[0].Meta["path"])
	}
	if contracts[0].Meta["framework"] != "fastapi/flask" {
		t.Errorf("expected fastapi/flask, got %s", contracts[0].Meta["framework"])
	}
}

func TestHTTPExtractor_Java_Spring(t *testing.T) {
	src := []byte(`
@RestController
@RequestMapping("/api")
public class UserController {

    @GetMapping("/users")
    public List<User> listUsers() {
        return userService.findAll();
    }

    @PostMapping("/users")
    public User createUser(@RequestBody User user) {
        return userService.save(user);
    }

    @DeleteMapping("/users/{id}")
    public void deleteUser(@PathVariable Long id) {
        userService.delete(id);
    }
}
`)
	nodes := makeNodes("UserController.java", []struct {
		name       string
		start, end int
	}{
		{"listUsers", 6, 8},
		{"createUser", 11, 13},
		{"deleteUser", 16, 18},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("UserController.java", src, nodes, nil)

	// Should detect @RequestMapping + @GetMapping + @PostMapping + @DeleteMapping
	if len(contracts) < 3 {
		t.Fatalf("expected at least 3 contracts, got %d", len(contracts))
	}

	// Find the GetMapping contract.
	found := false
	for _, c := range contracts {
		if c.Meta["method"] == "GET" && c.Meta["path"] == "/users" {
			found = true
			if c.Meta["framework"] != "spring" {
				t.Errorf("expected spring framework, got %s", c.Meta["framework"])
			}
		}
	}
	if !found {
		t.Error("did not find GET /users contract from @GetMapping")
	}
}

func TestHTTPExtractor_Consumers(t *testing.T) {
	src := []byte(`package main

import "net/http"

func callAPI() {
	http.Get("http://service-b/api/users")
	http.NewRequest("POST", "http://service-b/api/orders", nil)
}
`)
	nodes := makeNodes("client.go", []struct {
		name       string
		start, end int
	}{
		{"callAPI", 5, 8},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("client.go", src, nodes, nil)

	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}

	for _, c := range contracts {
		if c.Role != RoleConsumer {
			t.Errorf("expected consumer role, got %s", c.Role)
		}
	}
}

func TestHTTPExtractor_JS_Consumers(t *testing.T) {
	src := []byte(`
async function fetchData() {
  const users = await fetch('/api/users');
  const result = await axios.post('/api/orders', data);
}
`)
	nodes := makeNodes("api.js", []struct {
		name       string
		start, end int
	}{
		{"fetchData", 2, 5},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("api.js", src, nodes, nil)

	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}

	// fetch is confidence 0.7, axios is 0.9
	for _, c := range contracts {
		if c.Role != RoleConsumer {
			t.Errorf("expected consumer, got %s", c.Role)
		}
	}
}

func TestHTTPExtractor_Python_Consumers(t *testing.T) {
	src := []byte(`
import requests

def call_service():
    resp = requests.get("http://service/api/users")
    resp2 = requests.post("http://service/api/orders")
`)
	nodes := makeNodes("client.py", []struct {
		name       string
		start, end int
	}{
		{"call_service", 4, 6},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("client.py", src, nodes, nil)

	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}
}

func TestNormalizeHTTPPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// Path parameters are rewritten to positional names ({p1},
		// {p2}, ...) so cross-repo contract IDs match regardless of
		// whether provider and consumer teams picked different names
		// for the same slot — the well-known `{wid}` vs `{workspaceId}`
		// mismatch that used to appear as two orphan contracts.
		{"gin-style param", "/users/:id", "/users/{p1}"},
		{"typed angle param", "/users/<int:id>", "/users/{p1}"},
		{"canonical brace param", "/users/{id}", "/users/{p1}"},
		{"multiple params rename positionally", "/workspaces/{wid}/tags/{id}", "/workspaces/{p1}/tags/{p2}"},
		{"consumer name variant hashes the same", "/workspaces/{workspaceId}/tags/{id}", "/workspaces/{p1}/tags/{p2}"},
		{"trailing slash + quotes", `"/api/v1/items/"`, "/api/v1/items"},
		{"missing leading slash", "api/users", "/api/users"},
		{"root", "/", "/"},

		// T1.2: scheme+authority stripping.
		{"http scheme", "http://api.example.com/v1/users", "/v1/users"},
		{"https scheme with port", "https://api.example.com:443/v1/users", "/v1/users"},
		{"scheme only", "http://api.example.com", "/"},

		// T1.1: JS/TS template-literal base-URL stripping.
		{"leading tpl placeholder", "${API_URL}/v1/tucks", "/v1/tucks"},
		{"leading slash then placeholder", "/${TUCK_API_URL}/v1/tucks", "/v1/tucks"},
		{"dotted placeholder", "${process.env.API_URL}/v1/users", "/v1/users"},
		{"inline placeholder becomes param", "/v1/users/${id}", "/v1/users/{p1}"},
		{"base + inline param", "${BASE}/v1/users/${id}/tags", "/v1/users/{p1}/tags"},
		{"tpl inside host", "https://${HOST}/v1/users", "/v1/users"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeHTTPPath(tt.input)
			if got != tt.expected {
				t.Errorf("NormalizeHTTPPath(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// TestHTTPExtractor_Go_StdlibMux_NoLegacyDuplicate guards against the
// regression where both the Go 1.22+ pattern and the legacy HandleFunc
// pattern matched the same route, producing duplicate contracts
// (http::POST::/v1/tucks AND http::ANY::/POST /v1/tucks for a single
// mux.HandleFunc("POST /v1/tucks", h)). The legacy pattern's path
// capture now requires a leading "/", which the 1.22+ form never has
// (it starts with the verb), so only the specific extractor fires.
func TestHTTPExtractor_Go_StdlibMux_NoLegacyDuplicate(t *testing.T) {
	src := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /v1/tucks", h.CreateTuck)
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 7},
		{"CreateTuck", 9, 9},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)

	seen := map[string]int{}
	for _, c := range contracts {
		seen[c.ID]++
	}
	if seen["http::ANY::/POST /v1/tucks"] > 0 {
		t.Errorf("legacy HandleFunc pattern double-matched the 1.22 form: %v", seen)
	}
	if seen["http::POST::/v1/tucks"] != 1 {
		t.Errorf("expected exactly 1 http::POST::/v1/tucks contract, got %d (all: %v)",
			seen["http::POST::/v1/tucks"], seen)
	}
}

// TestHTTPExtractor_Go_HandlerThroughMiddleware guards against the
// regression where routes registered as
//
//	mux.HandleFunc("POST /v1/tucks", WithAuth(auth, h.CreateTuck))
//
// landed with SymbolID = "WithAuth" (the middleware wrapper) or the
// enclosing RegisterRoutes function, rather than the actual handler
// h.CreateTuck. The extractor now walks the balanced-paren tail of
// the call expression, picks the innermost identifier that resolves
// to a function/method in the same file — correctly landing on
// CreateTuck.
func TestHTTPExtractor_Go_HandlerThroughMiddleware(t *testing.T) {
	src := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /v1/tucks", WithAuth(auth, h.CreateTuck))
	mux.HandleFunc("GET /v1/tucks/{id}", WithAuth(auth, h.GetTuck))
	mux.HandleFunc("DELETE /v1/tucks/{id}", WithAuth(auth, WithAudit(audit, h.DeleteTuck)))
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 9},
		{"CreateTuck", 12, 12},
		{"GetTuck", 14, 14},
		{"DeleteTuck", 16, 16},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)

	bySymbol := map[string]string{}
	for _, c := range contracts {
		if c.Role != RoleProvider {
			continue
		}
		bySymbol[c.ID] = c.SymbolID
	}
	want := map[string]string{
		"http::POST::/v1/tucks":        "main.go::CreateTuck",
		"http::GET::/v1/tucks/{p1}":    "main.go::GetTuck",
		"http::DELETE::/v1/tucks/{p1}": "main.go::DeleteTuck",
	}
	for id, wantSym := range want {
		gotSym, ok := bySymbol[id]
		if !ok {
			t.Errorf("missing provider contract %s", id)
			continue
		}
		if gotSym != wantSym {
			t.Errorf("%s: SymbolID want %s, got %s (dropped to enclosing or wrapper name)",
				id, wantSym, gotSym)
		}
	}
}

// TestHTTPExtractor_Dart_Consumers covers T2.1: Dart HTTP client patterns
// (dio and package:http) now produce consumer contracts. Exercised via
// short snippets resembling the shape of tuck_app's TuckApiClient
// methods, including Dart's bare-$id interpolation style that
// NormalizeHTTPPath collapses to a positional {p1}.
// TestHTTPExtractor_Go_StdlibMux_1_22 covers the Go 1.22+ stdlib mux
// pattern where the HTTP method is embedded in the pattern string as
// "METHOD /path". Without splitting it, the contract ID ended up as
// "http::ANY::/DELETE /v1/..." which never pairs with a consumer
// "http::DELETE::/v1/...". core-api uses this pattern on 40+ routes;
// this is the regression that kept them all orphan.
func TestHTTPExtractor_Go_StdlibMux_1_22(t *testing.T) {
	src := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("GET /v1/tucks", h.ListTucks)
	mux.HandleFunc("POST /v1/tucks", h.CreateTuck)
	mux.HandleFunc("DELETE /v1/tucks/{id}", h.DeleteTuck)
	mux.HandleFunc("PATCH /v1/tucks/{id}/progress", h.UpdateProgress)
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 10},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)

	byID := make(map[string]Contract)
	for _, c := range contracts {
		byID[c.ID] = c
	}

	want := []struct {
		id, method, path string
	}{
		{"http::GET::/v1/tucks", "GET", "/v1/tucks"},
		{"http::POST::/v1/tucks", "POST", "/v1/tucks"},
		{"http::DELETE::/v1/tucks/{p1}", "DELETE", "/v1/tucks/{p1}"},
		{"http::PATCH::/v1/tucks/{p1}/progress", "PATCH", "/v1/tucks/{p1}/progress"},
	}
	for _, w := range want {
		c, ok := byID[w.id]
		if !ok {
			t.Errorf("missing contract %s; have: %v", w.id, keysOf(byID))
			continue
		}
		if c.Role != RoleProvider {
			t.Errorf("%s: role want provider, got %s", w.id, c.Role)
		}
		if c.Meta["method"] != w.method {
			t.Errorf("%s: method want %s, got %v", w.id, w.method, c.Meta["method"])
		}
		if c.Meta["path"] != w.path {
			t.Errorf("%s: path want %s, got %v", w.id, w.path, c.Meta["path"])
		}
	}
}

// TestHTTPExtractor_Go_StdlibMux_Subtree pins the trailing-slash
// rewrite. `mux.HandleFunc("POST /v1/tools/", h)` is Go's net/http
// subtree-match form — every POST under /v1/tools/ hits this handler.
// The contract ID must encode that as a parametric tail so it pairs
// with consumers calling per-route paths like /v1/tools/{name} from a
// sibling repo in the same workspace.
func TestHTTPExtractor_Go_StdlibMux_Subtree(t *testing.T) {
	src := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /v1/tools/", h.handleToolCall)
	mux.HandleFunc("GET /v1/static/", h.handleStatic)
	mux.HandleFunc("GET /v1/health", h.handleHealth)
	mux.Handle("/", root)
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 10},
	})
	ext := &HTTPExtractor{}
	contracts := ext.Extract("main.go", src, nodes, nil)
	byID := make(map[string]Contract)
	for _, c := range contracts {
		byID[c.ID] = c
	}

	// Subtree handlers get a parametric tail and a meta.subtree marker.
	for _, id := range []string{"http::POST::/v1/tools/{p1}", "http::GET::/v1/static/{p1}"} {
		c, ok := byID[id]
		if !ok {
			t.Errorf("missing subtree contract %s; have: %v", id, keysOf(byID))
			continue
		}
		if c.Meta["subtree"] != true {
			t.Errorf("%s: meta.subtree want true, got %v", id, c.Meta["subtree"])
		}
	}

	// Literal-path handlers stay literal — no parametric tail, no
	// subtree marker.
	if c, ok := byID["http::GET::/v1/health"]; !ok {
		t.Errorf("literal contract /v1/health was rewritten; have: %v", keysOf(byID))
	} else if c.Meta["subtree"] == true {
		t.Errorf("/v1/health: meta.subtree should be unset, got true")
	}

	// Root-only Handle("/", root) must NOT become "/{p1}" — that's
	// not a subtree, it's the catchall route.
	if _, ok := byID["http::ANY::/{p1}"]; ok {
		t.Errorf("root path / was wrongly rewritten as subtree; have: %v", keysOf(byID))
	}
}

// TestHTTPExtractor_Go_Subtree_PairsWithParametricConsumer is the
// end-to-end pin for the cross-repo bridge that motivated the
// trailing-slash rewrite. A `gortex` provider declares
// `mux.HandleFunc("POST /v1/tools/", h)` (subtree) and a `web`
// consumer in the same workspace calls `fetch('/v1/tools/${name}')`
// (parametric). Before the fix the IDs were `/v1/tools` vs
// `/v1/tools/{p1}` and the matcher kept both as orphans. After the
// fix they share `/v1/tools/{p1}` and pair as a CrossRepo link.
func TestHTTPExtractor_Go_Subtree_PairsWithParametricConsumer(t *testing.T) {
	providerSrc := []byte(`package main

import "net/http"

func wire(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("POST /v1/tools/", h.handleToolCall)
}
`)
	providerNodes := makeNodes("server.go", []struct {
		name       string
		start, end int
	}{
		{"wire", 5, 7},
	})

	consumerSrc := []byte(`async function callTool(name) {
  return fetch(` + "`/v1/tools/${name}`" + `, { method: 'POST' });
}
`)
	consumerNodes := makeNodes("api.ts", []struct {
		name       string
		start, end int
	}{
		{"callTool", 1, 3},
	})

	ext := &HTTPExtractor{}
	provContracts := ext.Extract("server.go", providerSrc, providerNodes, nil)
	consContracts := ext.Extract("api.ts", consumerSrc, consumerNodes, nil)

	reg := NewRegistry()
	for _, c := range provContracts {
		c.RepoPrefix = "gortex"
		c.WorkspaceID = "gortex"
		c.ProjectID = "gortex"
		reg.Add(c)
	}
	for _, c := range consContracts {
		c.RepoPrefix = "web"
		c.WorkspaceID = "gortex"
		c.ProjectID = "gortex"
		reg.Add(c)
	}

	result := Match(reg)

	var paired *CrossLink
	for i, m := range result.Matched {
		if m.ContractID == "http::POST::/v1/tools/{p1}" {
			paired = &result.Matched[i]
			break
		}
	}
	if paired == nil {
		t.Fatalf("expected POST /v1/tools/{p1} pair; matches=%d, orphan_providers=%d, orphan_consumers=%d",
			len(result.Matched), len(result.OrphanProviders), len(result.OrphanConsumers))
	}
	if !paired.CrossRepo {
		t.Errorf("expected CrossRepo=true (provider in gortex, consumer in web)")
	}
	if paired.Provider.RepoPrefix != "gortex" || paired.Consumer.RepoPrefix != "web" {
		t.Errorf("wrong repo wiring: provider=%s consumer=%s",
			paired.Provider.RepoPrefix, paired.Consumer.RepoPrefix)
	}
}

// keysOf returns the keys of a map of Contracts for failure diagnostics.
func keysOf(m map[string]Contract) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestHTTPExtractor_Dart_Consumers(t *testing.T) {
	src := []byte(`class TuckApiClient {
  final Dio _dio;
  TuckApiClient(this._dio);

  Future<void> createTuck(Map<String, dynamic> data) async {
    await _dio.post('/v1/tucks', data: data);
  }

  Future<void> deleteTuck(String id) async {
    await _dio.delete('/v1/tucks/$id');
  }

  Future<String> listHealth() async {
    final r = await http.get(Uri.parse('/v1/health'));
    return r.body;
  }
}
`)
	nodes := makeNodes("client.dart", []struct {
		name       string
		start, end int
	}{
		{"createTuck", 5, 7},
		{"deleteTuck", 9, 11},
		{"listHealth", 13, 16},
	})

	ext := &HTTPExtractor{}
	contracts := ext.Extract("client.dart", src, nodes, nil)

	want := map[string]string{
		"http::POST::/v1/tucks":        "client.dart::createTuck",
		"http::DELETE::/v1/tucks/{p1}": "client.dart::deleteTuck",
		"http::GET::/v1/health":        "client.dart::listHealth",
	}
	got := map[string]string{}
	for _, c := range contracts {
		if c.Role != RoleConsumer {
			t.Errorf("expected role consumer for %s, got %s", c.ID, c.Role)
		}
		got[c.ID] = c.SymbolID
	}
	for id, sym := range want {
		if got[id] != sym {
			t.Errorf("missing/mismatched consumer contract:\n  want %s → %s\n  got  %s → %s",
				id, sym, id, got[id])
		}
	}
}

func TestHTTPExtractor_SupportedLanguages(t *testing.T) {
	ext := &HTTPExtractor{}
	langs := ext.SupportedLanguages()

	// Spot-check the specific languages rather than the count so adding
	// a new language's patterns (T2.1 added dart) doesn't force an
	// unrelated test update.
	want := []string{"go", "typescript", "javascript", "python", "java", "dart"}
	set := make(map[string]bool, len(langs))
	for _, l := range langs {
		set[l] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("SupportedLanguages missing %q; got %v", w, langs)
		}
	}
}

func TestHTTPExtractor_NoSymbol(t *testing.T) {
	// When there are no enclosing functions, SymbolID should be empty.
	src := []byte(`r.GET("/api/test", handler)`)
	contracts := (&HTTPExtractor{}).Extract("main.go", src, nil, nil)
	if len(contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(contracts))
	}
	if contracts[0].SymbolID != "" {
		t.Errorf("expected empty symbol ID, got %s", contracts[0].SymbolID)
	}
}

// Regression: the fiber pattern's `.VERB(` anchor used to match verb names
// embedded in string literals — e.g. the prefilter marker `[]byte(".GET(")`
// — and the `[^"`+"`"+`]+` path capture would overshoot the closing quote
// and chew source code until the next quote on the next line, emitting
// contracts whose "path" was a chunk of source. Requiring the captured path
// to start with `/` rejects these false positives.
func TestHTTPExtractor_Go_Fiber_NoMatchOnVerbInsideStringLiteral(t *testing.T) {
	src := []byte("package contracts\n" +
		"\n" +
		"var markers = [][]byte{\n" +
		"\t[]byte(\".GET(\"),\n" +
		"\t[]byte(\".POST(\"),\n" +
		"\t[]byte(\".PUT(\"),\n" +
		"\t[]byte(\".DELETE(\"),\n" +
		"\t[]byte(\".PATCH(\"),\n" +
		"}\n")
	contracts := (&HTTPExtractor{}).Extract("markers.go", src, nil, nil)
	for _, c := range contracts {
		if c.Meta["framework"] == "fiber" {
			t.Errorf("fiber pattern matched a verb literal inside a string: id=%q path=%q", c.ID, c.Meta["path"])
		}
	}
}

// And the positive case: a real fiber registration still produces a contract.
func TestHTTPExtractor_Go_Fiber_RealRoute(t *testing.T) {
	src := []byte("package main\n" +
		"\n" +
		"import \"github.com/gofiber/fiber/v2\"\n" +
		"\n" +
		"func register(app *fiber.App) {\n" +
		"\tapp.GET(\"/v1/users\", listUsers)\n" +
		"\tapp.DELETE(\"/v1/users/:id\", deleteUser)\n" +
		"}\n")
	contracts := (&HTTPExtractor{}).Extract("main.go", src, nil, nil)
	byID := map[string]Contract{}
	for _, c := range contracts {
		byID[c.ID] = c
	}
	if _, ok := byID["http::GET::/v1/users"]; !ok {
		t.Errorf("expected http::GET::/v1/users; got %v", byID)
	}
	if _, ok := byID["http::DELETE::/v1/users/{p1}"]; !ok {
		t.Errorf("expected http::DELETE::/v1/users/{p1}; got %v", byID)
	}
}
