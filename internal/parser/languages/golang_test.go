package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestGoExtractor_SimpleFunction(t *testing.T) {
	src := []byte(`package main

func Hello() {
	fmt.Println("hello")
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "Hello", funcs[0].Name)
	assert.Equal(t, "main.go::Hello", funcs[0].ID)
	assert.Equal(t, 3, funcs[0].StartLine)
	assert.Equal(t, "func Hello()", funcs[0].Meta["signature"])
}

func TestGoExtractor_Method(t *testing.T) {
	src := []byte(`package main

type Server struct{}

func (s *Server) Start() error {
	return nil
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("server.go", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "Start", methods[0].Name)
	assert.Equal(t, "server.go::Server.Start", methods[0].ID)
	assert.Equal(t, "Server", methods[0].Meta["receiver"])

	// Check member_of edge.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 1)
	assert.Equal(t, "server.go::Server.Start", memberEdges[0].From)
	assert.Equal(t, "server.go::Server", memberEdges[0].To)
}

func TestGoExtractor_Struct(t *testing.T) {
	src := []byte(`package main

type Config struct {
	Port int
	Host string
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("config.go", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Config", types[0].Name)
}

func TestGoExtractor_Interface(t *testing.T) {
	src := []byte(`package store

type Repository interface {
	FindByID(id string) (*User, error)
	Save(u *User) error
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("store.go", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Repository", ifaces[0].Name)

	// Verify method names are extracted into Meta.
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok, "Meta[\"methods\"] should be []string")
	assert.Len(t, methods, 2)
	assert.Contains(t, methods, "FindByID")
	assert.Contains(t, methods, "Save")
}

func TestGoExtractor_EmptyInterface(t *testing.T) {
	src := []byte(`package main

type Any interface{}
`)
	e := NewGoExtractor()
	result, err := e.Extract("any.go", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Any", ifaces[0].Name)

	// Empty interface should have empty methods slice (not nil).
	methods, ok := ifaces[0].Meta["methods"]
	require.True(t, ok)
	// The slice should be nil or empty.
	if ms, ok := methods.([]string); ok {
		assert.Empty(t, ms)
	}
}

func TestGoExtractor_Imports(t *testing.T) {
	src := []byte(`package main

import (
	"fmt"
	"os"
	mylib "github.com/me/lib"
)
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 3)

	targets := make([]string, len(imports))
	for i, e := range imports {
		targets[i] = e.To
	}
	assert.Contains(t, targets, "unresolved::import::fmt")
	assert.Contains(t, targets, "unresolved::import::os")
	assert.Contains(t, targets, "unresolved::import::github.com/me/lib")
}

func TestGoExtractor_CallSites(t *testing.T) {
	src := []byte(`package main

import "fmt"

func Hello() {
	fmt.Println("hello")
	helper()
}

func helper() {}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	require.GreaterOrEqual(t, len(calls), 2)

	var targets []string
	for _, c := range calls {
		targets = append(targets, c.To)
	}
	assert.Contains(t, targets, "unresolved::helper")
	assert.Contains(t, targets, "unresolved::*.Println")
}

func TestGoExtractor_Variables(t *testing.T) {
	src := []byte(`package main

var MaxRetries int

const DefaultPort = 8080
`)
	e := NewGoExtractor()
	result, err := e.Extract("config.go", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 2)

	names := []string{vars[0].Name, vars[1].Name}
	assert.Contains(t, names, "MaxRetries")
	assert.Contains(t, names, "DefaultPort")
}

func TestGoExtractor_FullFile(t *testing.T) {
	src := []byte(`package auth

import (
	"context"
	"errors"
)

type Claims struct {
	UserID string
	Exp    int64
}

func ValidateToken(ctx context.Context, raw string) (*Claims, error) {
	if raw == "" {
		return nil, errors.New("empty token")
	}
	return &Claims{UserID: "123"}, nil
}

func (c *Claims) IsExpired() bool {
	return false
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("auth/token.go", src)
	require.NoError(t, err)

	// File node.
	files := nodesOfKind(result.Nodes, graph.KindFile)
	require.Len(t, files, 1)

	// Type.
	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Claims", types[0].Name)

	// Function.
	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "ValidateToken", funcs[0].Name)

	// Method.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "IsExpired", methods[0].Name)

	// Imports.
	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 2)
}

func TestGoExtractor_TypeAlias(t *testing.T) {
	src := []byte(`package main

type ID = string
type Handler func(w http.ResponseWriter, r *http.Request)
`)
	e := NewGoExtractor()
	result, err := e.Extract("types.go", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 2)
	names := []string{types[0].Name, types[1].Name}
	assert.Contains(t, names, "ID")
	assert.Contains(t, names, "Handler")
}

// --- helpers ---

func nodesOfKind(nodes []*graph.Node, kind graph.NodeKind) []*graph.Node {
	var out []*graph.Node
	for _, n := range nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func edgesOfKind(edges []*graph.Edge, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}
