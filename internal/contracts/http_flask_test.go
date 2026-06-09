package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// flaskNode builds a graph node with an explicit kind for the Flask route tests
// (the shared makeNodes helper only emits KindFunction).
func flaskNode(id, name string, kind graph.NodeKind, start int) *graph.Node {
	return &graph.Node{ID: id, Name: name, Kind: kind, FilePath: "app.py", StartLine: start, EndLine: start + 2}
}

func flaskFind(cs []Contract, method, path string) *Contract {
	for i := range cs {
		if cs[i].Meta["method"] == method && cs[i].Meta["path"] == path {
			return &cs[i]
		}
	}
	return nil
}

func TestHTTPExtractor_Python_FlaskRestful_SinglePath(t *testing.T) {
	src := []byte(`from flask_restful import Api, Resource

class UserResource(Resource):
    def get(self, id):
        return {}
    def post(self):
        return {}

api.add_resource(UserResource, '/users/<int:id>')
`)
	nodes := []*graph.Node{
		flaskNode("app.py::UserResource", "UserResource", graph.KindType, 3),
		flaskNode("app.py::UserResource.get", "get", graph.KindMethod, 4),
		flaskNode("app.py::UserResource.post", "post", graph.KindMethod, 6),
	}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)

	get := flaskFind(cs, "GET", "/users/{p1}")
	post := flaskFind(cs, "POST", "/users/{p1}")
	if get == nil || post == nil {
		t.Fatalf("expected GET+POST on /users/{p1}, got %+v", contractPaths(cs))
	}
	if get.SymbolID != "app.py::UserResource.get" {
		t.Errorf("GET SymbolID = %q, want the per-verb method node", get.SymbolID)
	}
	if post.SymbolID != "app.py::UserResource.post" {
		t.Errorf("POST SymbolID = %q, want the per-verb method node", post.SymbolID)
	}
	if get.Meta["framework"] != "flask" {
		t.Errorf("framework = %v, want flask", get.Meta["framework"])
	}
	if names, _ := get.Meta["path_param_names"].([]string); len(names) != 1 || names[0] != "id" {
		t.Errorf("path_param_names = %v, want [id]", get.Meta["path_param_names"])
	}
}

func TestHTTPExtractor_Python_FlaskRestful_MultiPath(t *testing.T) {
	src := []byte(`class ItemResource(Resource):
    def get(self): ...
    def delete(self): ...

api.add_resource(ItemResource, '/items', '/items/<int:id>')
`)
	nodes := []*graph.Node{
		flaskNode("app.py::ItemResource", "ItemResource", graph.KindType, 1),
		flaskNode("app.py::ItemResource.get", "get", graph.KindMethod, 2),
		flaskNode("app.py::ItemResource.delete", "delete", graph.KindMethod, 3),
	}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	want := []struct{ m, p string }{
		{"GET", "/items"}, {"DELETE", "/items"},
		{"GET", "/items/{p1}"}, {"DELETE", "/items/{p1}"},
	}
	for _, w := range want {
		if flaskFind(cs, w.m, w.p) == nil {
			t.Errorf("missing %s %s; got %v", w.m, w.p, contractPaths(cs))
		}
	}
	if n := countFlask(cs); n != 4 {
		t.Errorf("expected 4 flask contracts, got %d: %v", n, contractPaths(cs))
	}
}

func TestHTTPExtractor_Python_FlaskRestful_UnknownClassFallback(t *testing.T) {
	src := []byte(`api.add_resource(PlainClass, '/plain')`)
	cs := (&HTTPExtractor{}).Extract("app.py", src, nil, nil)
	if countFlask(cs) != 1 {
		t.Fatalf("expected 1 GET fallback contract, got %v", contractPaths(cs))
	}
	if flaskFind(cs, "GET", "/plain") == nil {
		t.Errorf("expected GET /plain fallback, got %v", contractPaths(cs))
	}
}

func TestHTTPExtractor_Python_FlaskRestful_NoVerbMethods(t *testing.T) {
	src := []byte(`class HelperResource(Resource):
    def helper(self): ...

api.add_resource(HelperResource, '/helper')
`)
	nodes := []*graph.Node{
		flaskNode("app.py::HelperResource", "HelperResource", graph.KindType, 1),
		flaskNode("app.py::HelperResource.helper", "helper", graph.KindMethod, 2),
	}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	get := flaskFind(cs, "GET", "/helper")
	if get == nil || countFlask(cs) != 1 {
		t.Fatalf("expected 1 GET fallback, got %v", contractPaths(cs))
	}
	if get.SymbolID != "app.py::HelperResource" {
		t.Errorf("fallback SymbolID = %q, want the class node", get.SymbolID)
	}
}

func TestHTTPExtractor_Python_AddURLRule_ViewFuncMethods(t *testing.T) {
	src := []byte(`def handler(): ...

app.add_url_rule('/regular', view_func=handler, methods=['POST', 'PUT'])
`)
	nodes := []*graph.Node{flaskNode("app.py::handler", "handler", graph.KindFunction, 1)}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	post := flaskFind(cs, "POST", "/regular")
	put := flaskFind(cs, "PUT", "/regular")
	if post == nil || put == nil {
		t.Fatalf("expected POST+PUT on /regular, got %v", contractPaths(cs))
	}
	if post.SymbolID != "app.py::handler" {
		t.Errorf("POST SymbolID = %q, want app.py::handler", post.SymbolID)
	}
}

func TestHTTPExtractor_Python_AddURLRule_DefaultGET(t *testing.T) {
	src := []byte(`def h(): ...
app.add_url_rule('/x', view_func=h)
`)
	nodes := []*graph.Node{flaskNode("app.py::h", "h", graph.KindFunction, 1)}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	if flaskFind(cs, "GET", "/x") == nil || countFlask(cs) != 1 {
		t.Errorf("expected 1 GET /x, got %v", contractPaths(cs))
	}
}

func TestHTTPExtractor_Python_AddURLRule_PositionalViewFunc(t *testing.T) {
	src := []byte(`def h(): ...
app.add_url_rule('/y', h)
`)
	nodes := []*graph.Node{flaskNode("app.py::h", "h", graph.KindFunction, 1)}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	got := flaskFind(cs, "GET", "/y")
	if got == nil {
		t.Fatalf("expected GET /y, got %v", contractPaths(cs))
	}
	if got.SymbolID != "app.py::h" {
		t.Errorf("SymbolID = %q, want app.py::h", got.SymbolID)
	}
}

func TestHTTPExtractor_Python_AddURLRule_NoViewFunc(t *testing.T) {
	src := []byte(`app.add_url_rule('/z')`)
	cs := (&HTTPExtractor{}).Extract("app.py", src, nil, nil)
	if countFlask(cs) != 0 {
		t.Errorf("expected 0 contracts without view_func, got %v", contractPaths(cs))
	}
}

func TestHTTPExtractor_Python_FlaskRestful_Multiline(t *testing.T) {
	src := []byte(`class UserResource(Resource):
    def get(self): ...

api.add_resource(
    UserResource,
    '/users',
)
`)
	nodes := []*graph.Node{
		flaskNode("app.py::UserResource", "UserResource", graph.KindType, 1),
		flaskNode("app.py::UserResource.get", "get", graph.KindMethod, 2),
	}
	cs := (&HTTPExtractor{}).Extract("app.py", src, nodes, nil)
	if flaskFind(cs, "GET", "/users") == nil {
		t.Errorf("expected multiline add_resource to resolve GET /users, got %v", contractPaths(cs))
	}
}

func countFlask(cs []Contract) int {
	n := 0
	for _, c := range cs {
		if c.Meta["framework"] == "flask" {
			n++
		}
	}
	return n
}

func contractPaths(cs []Contract) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Meta["method"].(string)+" "+c.Meta["path"].(string))
	}
	return out
}
