package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedHierarchy injects an OO graph onto srv:
//
//	interface Animal
//	  ↑ implements        Dog
//	    ↑ extends         Puppy
//	    ↑ extends         ServiceDog
//	  Dog composes Tail
//
// Methods Animal.Speak / Dog.Speak / Puppy.Speak / ServiceDog.Speak
// participate in an EdgeOverrides chain.
func seedHierarchy(t *testing.T, srv *Server) {
	t.Helper()
	g := srv.graph

	add := func(id, name string, kind graph.NodeKind, file string) {
		g.AddNode(&graph.Node{ID: id, Kind: kind, Name: name, FilePath: file, Language: "go"})
	}
	addEdge := func(from, to string, kind graph.EdgeKind, origin string) {
		ed := &graph.Edge{From: from, To: to, Kind: kind, FilePath: "x.go"}
		if origin != "" {
			ed.Origin = origin
		}
		g.AddEdge(ed)
	}

	add("animal.go::Animal", "Animal", graph.KindInterface, "animal.go")
	add("animal.go::Animal.Speak", "Animal.Speak", graph.KindMethod, "animal.go")
	add("dog.go::Dog", "Dog", graph.KindType, "dog.go")
	add("dog.go::Dog.Speak", "Dog.Speak", graph.KindMethod, "dog.go")
	add("tail.go::Tail", "Tail", graph.KindType, "tail.go")
	add("puppy.go::Puppy", "Puppy", graph.KindType, "puppy.go")
	add("puppy.go::Puppy.Speak", "Puppy.Speak", graph.KindMethod, "puppy.go")
	add("service.go::ServiceDog", "ServiceDog", graph.KindType, "service.go")
	add("service.go::ServiceDog.Speak", "ServiceDog.Speak", graph.KindMethod, "service.go")

	addEdge("dog.go::Dog", "animal.go::Animal", graph.EdgeImplements, graph.OriginLSPResolved)
	addEdge("dog.go::Dog", "tail.go::Tail", graph.EdgeComposes, graph.OriginASTResolved)
	addEdge("puppy.go::Puppy", "dog.go::Dog", graph.EdgeExtends, graph.OriginASTResolved)
	addEdge("service.go::ServiceDog", "dog.go::Dog", graph.EdgeExtends, graph.OriginASTInferred)

	addEdge("animal.go::Animal.Speak", "animal.go::Animal", graph.EdgeMemberOf, graph.OriginASTResolved)
	addEdge("dog.go::Dog.Speak", "dog.go::Dog", graph.EdgeMemberOf, graph.OriginASTResolved)
	addEdge("puppy.go::Puppy.Speak", "puppy.go::Puppy", graph.EdgeMemberOf, graph.OriginASTResolved)
	addEdge("service.go::ServiceDog.Speak", "service.go::ServiceDog", graph.EdgeMemberOf, graph.OriginASTResolved)

	addEdge("dog.go::Dog.Speak", "animal.go::Animal.Speak", graph.EdgeOverrides, graph.OriginASTResolved)
	addEdge("puppy.go::Puppy.Speak", "dog.go::Dog.Speak", graph.EdgeOverrides, graph.OriginASTResolved)
	addEdge("service.go::ServiceDog.Speak", "dog.go::Dog.Speak", graph.EdgeOverrides, graph.OriginASTInferred)
}

func callGetClassHierarchy(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_class_hierarchy"
	req.Params.Arguments = args
	res, err := srv.handleGetClassHierarchy(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "unexpected error: %+v", res.Content)
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(textBlock.Text), &out))
	return out
}

func nodeIDsFromResult(out map[string]any) []string {
	rawNodes, _ := out["nodes"].([]any)
	ids := make([]string, 0, len(rawNodes))
	for _, raw := range rawNodes {
		if m, ok := raw.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func TestGetClassHierarchy_Down(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	out := callGetClassHierarchy(t, srv, map[string]any{
		"id":        "animal.go::Animal",
		"direction": "down",
		"depth":     5,
	})
	ids := nodeIDsFromResult(out)
	assert.Contains(t, ids, "dog.go::Dog")
	assert.Contains(t, ids, "puppy.go::Puppy")
	assert.Contains(t, ids, "service.go::ServiceDog")
	assert.NotContains(t, ids, "tail.go::Tail")
}

func TestGetClassHierarchy_Up(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	out := callGetClassHierarchy(t, srv, map[string]any{
		"id":        "puppy.go::Puppy",
		"direction": "up",
	})
	ids := nodeIDsFromResult(out)
	assert.Contains(t, ids, "dog.go::Dog")
	assert.Contains(t, ids, "animal.go::Animal")
	assert.Contains(t, ids, "tail.go::Tail")
}

func TestGetClassHierarchy_BothDefault(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	// No `direction` arg → default "both".
	out := callGetClassHierarchy(t, srv, map[string]any{
		"id": "dog.go::Dog",
	})
	ids := nodeIDsFromResult(out)
	assert.Contains(t, ids, "animal.go::Animal")
	assert.Contains(t, ids, "tail.go::Tail")
	assert.Contains(t, ids, "puppy.go::Puppy")
	assert.Contains(t, ids, "service.go::ServiceDog")
}

func TestGetClassHierarchy_IncludeMethods(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	out := callGetClassHierarchy(t, srv, map[string]any{
		"id":              "animal.go::Animal",
		"direction":       "down",
		"include_methods": true,
		"depth":           5,
	})
	ids := nodeIDsFromResult(out)
	assert.Contains(t, ids, "animal.go::Animal.Speak")
	assert.Contains(t, ids, "dog.go::Dog.Speak")
	assert.Contains(t, ids, "puppy.go::Puppy.Speak")
	assert.Contains(t, ids, "service.go::ServiceDog.Speak")
}

func TestGetClassHierarchy_MinTier(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	// ServiceDog -extends-> Dog has Origin=ast_inferred; Puppy -extends-> Dog
	// is ast_resolved. min_tier=ast_resolved should drop the ServiceDog edge.
	out := callGetClassHierarchy(t, srv, map[string]any{
		"id":        "dog.go::Dog",
		"direction": "down",
		"min_tier":  graph.OriginASTResolved,
	})
	rawEdges, _ := out["edges"].([]any)
	for _, e := range rawEdges {
		m := e.(map[string]any)
		from, _ := m["from"].(string)
		assert.NotEqual(t, "service.go::ServiceDog", from, "ast_inferred edge should be filtered out by min_tier=ast_resolved")
	}
}

func TestGetClassHierarchy_BadDirection(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_class_hierarchy"
	req.Params.Arguments = map[string]any{
		"id":        "animal.go::Animal",
		"direction": "sideways",
	}
	res, err := srv.handleGetClassHierarchy(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected validation error for bad direction")
}

func TestGetClassHierarchy_MissingID(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_class_hierarchy"
	req.Params.Arguments = map[string]any{}
	res, err := srv.handleGetClassHierarchy(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "expected error when id is missing")
}

func TestGetClassHierarchy_GCXFormat(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedHierarchy(t, srv)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_class_hierarchy"
	req.Params.Arguments = map[string]any{
		"id":        "animal.go::Animal",
		"direction": "down",
		"format":    "gcx",
	}
	res, err := srv.handleGetClassHierarchy(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	// GCX1 envelopes start with "GCX1 " and the encodeSubGraph helper
	// emits a node section then an edge section, both tagged with the
	// tool name.
	require.True(t, strings.HasPrefix(text, "GCX1 "), "expected GCX1 envelope, got %q", text[:min(len(text), 32)])
	require.Contains(t, text, "tool=get_class_hierarchy")
	require.Contains(t, text, "dog.go::Dog")
}

func TestGetClassHierarchy_RegisteredAndScoped(t *testing.T) {
	srv, _ := setupTestServer(t)
	scope, ok := srv.toolScopes.get("get_class_hierarchy")
	require.True(t, ok, "get_class_hierarchy must be registered with a scope")
	assert.Equal(t, ScopeRepo, scope, "expected ScopeRepo for get_class_hierarchy")
}
