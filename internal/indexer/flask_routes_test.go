package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestFlaskRestful_EndToEnd indexes a real flask-restful app and asserts the
// add_resource route becomes a first-class KindContract node with an
// EdgeHandlesRoute edge from the specific per-verb method node — proving the
// extractor → commitContracts → graph pipeline works end to end.
func TestFlaskRestful_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	src := `from flask import Flask
from flask_restful import Api, Resource

app = Flask(__name__)
api = Api(app)

class UserResource(Resource):
    def get(self, id):
        return {"user": id}

    def post(self):
        return {}

api.add_resource(UserResource, '/users/<int:id>')
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.py"), []byte(src), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	get := g.GetNode("http::GET::/users/{p1}")
	post := g.GetNode("http::POST::/users/{p1}")
	require.NotNil(t, get, "expected a GET route node from add_resource")
	require.NotNil(t, post, "expected a POST route node from add_resource")

	// The GET route must be handled by the resource's get() method node.
	var handler *graph.Node
	for _, e := range g.GetInEdges("http::GET::/users/{p1}") {
		if e.Kind == graph.EdgeHandlesRoute {
			handler = g.GetNode(e.From)
		}
	}
	require.NotNil(t, handler, "expected an EdgeHandlesRoute into the GET route")
	require.Equal(t, "get", handler.Name, "GET route should be handled by the get() method, not the class")
}
