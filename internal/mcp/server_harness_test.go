package mcp

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// newFullTestServer builds a Server through the real NewServer
// constructor — every tool registered, every broadcaster wired — over
// a tiny two-node graph. Use it for tests that exercise registration,
// the tool surface, prompts, or notifications; newTestServer (a
// partial struct literal) is for narrow handler-unit tests.
func newFullTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Bar", Name: "Bar", Kind: graph.KindFunction, FilePath: "pkg/foo.go"})
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Baz", Name: "Baz", Kind: graph.KindMethod, FilePath: "pkg/foo.go"})
	eng := query.NewEngine(g)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}
