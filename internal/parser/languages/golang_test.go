package languages

import (
	"strings"
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
	// fmt.Println is now attributed to the import path so the resolver
	// can render cross-boundary badges instead of a bare `*.Println`.
	assert.Contains(t, targets, "unresolved::extern::fmt::Println")
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

// --- Type environment tests ---

func TestGoExtractor_TypeEnv_ExplicitVar(t *testing.T) {
	src := []byte(`package main

type User struct{}

func (u *User) Save() {}

func main() {
	var user User
	user.Save()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to Save")
	require.NotNil(t, saveCall.Meta, "expected Meta on Save call edge")
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestGoExtractor_TypeEnv_CompositeLiteral(t *testing.T) {
	src := []byte(`package main

type User struct{ Name string }

func (u User) Save() {}

func main() {
	user := User{Name: "Alice"}
	user.Save()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall)
	require.NotNil(t, saveCall.Meta)
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestGoExtractor_TypeEnv_AddressOf(t *testing.T) {
	src := []byte(`package main

type Server struct{}

func (s *Server) Start() {}

func main() {
	srv := &Server{}
	srv.Start()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var startCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Start") {
			startCall = c
			break
		}
	}
	require.NotNil(t, startCall)
	require.NotNil(t, startCall.Meta)
	assert.Equal(t, "Server", startCall.Meta["receiver_type"])
}

func TestGoExtractor_TypeEnv_Constructor(t *testing.T) {
	src := []byte(`package main

type Client struct{}

func NewClient() *Client { return &Client{} }

func (c *Client) Connect() {}

func main() {
	client := NewClient()
	client.Connect()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestGoExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`package main

func getUser() interface{} { return nil }

func main() {
	user := getUser()
	user.Save()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to Save")
	assert.Nil(t, saveCall.Meta, "unknown type should not produce Meta")
}

// --- Tier 2: Chain resolution tests ---

func TestGoExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`package main

type Service struct{}
type User struct{}

func (s *Service) GetUser() *User { return nil }
func (u *User) Save() error { return nil }

func main() {
	svc := &Service{}
	svc.GetUser().Save()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	// Verify return_type is set on GetUser method.
	var getUserNode *graph.Node
	for _, n := range result.Nodes {
		if n.Name == "GetUser" && n.Kind == graph.KindMethod {
			getUserNode = n
			break
		}
	}
	require.NotNil(t, getUserNode)
	assert.Equal(t, "User", getUserNode.Meta["return_type"])

	// Verify chain resolution: svc.GetUser().Save() should have receiver_type=User.
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to Save")
	require.NotNil(t, saveCall.Meta, "Save call should have Meta from chain resolution")
	assert.Equal(t, "User", saveCall.Meta["receiver_type"])
}

func TestGoExtractor_TypeEnv_ChainUnresolvable(t *testing.T) {
	src := []byte(`package main

func getService() interface{} { return nil }

func main() {
	svc := getService()
	svc.Process().Finish()
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var finishCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Finish") {
			finishCall = c
			break
		}
	}
	require.NotNil(t, finishCall)
	assert.Nil(t, finishCall.Meta, "unresolvable chain should not produce Meta")
}

func TestGoExtractor_ReturnType(t *testing.T) {
	src := []byte(`package main

type User struct{}

func NewUser() *User { return &User{} }
func (u *User) Name() string { return "" }
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	for _, n := range result.Nodes {
		switch n.Name {
		case "NewUser":
			assert.Equal(t, "User", n.Meta["return_type"], "NewUser should return User")
		case "Name":
			assert.Nil(t, n.Meta["return_type"], "string return type should be skipped (primitive)")
		}
	}
}

func TestGoExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`package main

// Hello greets the user.
// It also returns nothing.
func Hello() {}

// helper is internal.
func helper() {}

// Server is the HTTP server.
type Server struct{}

// Start brings the server up.
func (s *Server) Start() {}

// MaxRetries is the cap.
const MaxRetries = 3
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	hello := byID["main.go::Hello"]
	require.NotNil(t, hello)
	assert.Equal(t, "Hello greets the user. It also returns nothing.", hello.Meta["doc"])
	assert.Equal(t, "public", hello.Meta["visibility"])

	helper := byID["main.go::helper"]
	require.NotNil(t, helper)
	assert.Equal(t, "helper is internal.", helper.Meta["doc"])
	assert.Equal(t, "package", helper.Meta["visibility"])

	server := byID["main.go::Server"]
	require.NotNil(t, server)
	assert.Equal(t, "Server is the HTTP server.", server.Meta["doc"])
	assert.Equal(t, "public", server.Meta["visibility"])

	start := byID["main.go::Server.Start"]
	require.NotNil(t, start)
	assert.Equal(t, "Start brings the server up.", start.Meta["doc"])
	assert.Equal(t, "public", start.Meta["visibility"])

	mr := byID["main.go::MaxRetries"]
	require.NotNil(t, mr)
	assert.Equal(t, "public", mr.Meta["visibility"])
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

func TestGoExtractor_StructFieldNodes(t *testing.T) {
	src := []byte(`package main

// User holds the basics.
type User struct {
	// Name is the public name.
	Name string
	age  int
	*Base
	pkg.Embedded
}

type Base struct{}
`)
	e := NewGoExtractor()
	result, err := e.Extract("user.go", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	name := byID["user.go::User.Name"]
	require.NotNil(t, name)
	assert.Equal(t, graph.KindField, name.Kind)
	assert.Equal(t, "public", name.Meta["visibility"])
	assert.Equal(t, "string", name.Meta["field_type"])
	assert.Equal(t, "Name is the public name.", name.Meta["doc"])

	age := byID["user.go::User.age"]
	require.NotNil(t, age)
	assert.Equal(t, "package", age.Meta["visibility"])
	assert.Equal(t, "int", age.Meta["field_type"])

	// Embedded pointer field — name is "Base".
	base := byID["user.go::User.Base"]
	require.NotNil(t, base, "embedded pointer field Base missing")

	// Qualified embedded field — name is "Embedded".
	emb := byID["user.go::User.Embedded"]
	require.NotNil(t, emb, "qualified embedded field Embedded missing")

	// MemberOf edges should hop field → owner type.
	memberOf := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	hasNameToUser := false
	for _, edge := range memberOf {
		if edge.From == "user.go::User.Name" && edge.To == "user.go::User" {
			hasNameToUser = true
		}
	}
	if !hasNameToUser {
		t.Fatalf("missing MemberOf edge User.Name → User")
	}
}

func TestGoExtractor_FieldWrites(t *testing.T) {
	src := []byte(`package main

type Server struct {
	port int
	addr string
}

func (s *Server) SetPort(p int) {
	s.port = p
	s.addr += "x"
	s.port++
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("server.go", src)
	require.NoError(t, err)

	writes := edgesOfKind(result.Edges, graph.EdgeWrites)
	if len(writes) < 3 {
		t.Fatalf("expected ≥3 EdgeWrites (one per LHS selector), got %d", len(writes))
	}
	// All writes should originate from SetPort.
	for _, w := range writes {
		if w.From != "server.go::Server.SetPort" {
			t.Errorf("unexpected EdgeWrites source %q (want SetPort)", w.From)
		}
	}
	// Writes target the unresolved field path; the resolver
	// post-pass lands them on the field node.
	hasPort, hasAddr := false, false
	for _, w := range writes {
		if w.To == "unresolved::*.port" {
			hasPort = true
		}
		if w.To == "unresolved::*.addr" {
			hasAddr = true
		}
	}
	if !hasPort {
		t.Fatalf("expected EdgeWrites → unresolved::*.port")
	}
	if !hasAddr {
		t.Fatalf("expected EdgeWrites → unresolved::*.addr")
	}
}

func TestGoExtractor_FieldWrites_TrackedReceiver(t *testing.T) {
	src := []byte(`package main

type Cfg struct{ port int }

func main() {
	c := Cfg{port: 8080}
	c.port = 9090
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	writes := edgesOfKind(result.Edges, graph.EdgeWrites)
	require.GreaterOrEqual(t, len(writes), 1)
	hasReceiverType := false
	for _, w := range writes {
		if rt, _ := w.Meta["receiver_type"].(string); rt == "Cfg" {
			hasReceiverType = true
		}
	}
	if !hasReceiverType {
		t.Fatalf("expected EdgeWrites with receiver_type=Cfg when receiver is tenv-tracked")
	}
}

func TestGoExtractor_FieldWrites_ResolveAfterIndex(t *testing.T) {
	// Verifies the extractor + indexer + resolver chain: a write to
	// `c.port` lands on the field node when the receiver type is
	// known to the type env.
	src := []byte(`package main

type Cfg struct{ port int }

func main() {
	c := Cfg{port: 8080}
	c.port = 9090
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("main.go", src)
	require.NoError(t, err)

	// Field node should exist.
	var fieldID string
	for _, n := range result.Nodes {
		if n.Kind == graph.KindField && n.Name == "port" {
			fieldID = n.ID
		}
	}
	if fieldID != "main.go::Cfg.port" {
		t.Fatalf("expected field node main.go::Cfg.port, got %q", fieldID)
	}

	// Write edge should be unresolved at extraction time and carry
	// receiver_type=Cfg (set from tenv).
	writes := edgesOfKind(result.Edges, graph.EdgeWrites)
	require.Len(t, writes, 1)
	if rt, _ := writes[0].Meta["receiver_type"].(string); rt != "Cfg" {
		t.Fatalf("expected receiver_type=Cfg, got %q", rt)
	}
	assert.Equal(t, "unresolved::*.port", writes[0].To)
}

func TestGoExtractor_GenericTypeParams(t *testing.T) {
	src := []byte(`package x

func Map[T any, R comparable](in []T, f func(T) R) []R { return nil }

func (m *Map[K, V]) Get(k K) V { var v V; return v }

type List[T any] struct {
	items []T
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("x.go", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	mapFn := byID["x.go::Map"]
	require.NotNil(t, mapFn)
	tp, _ := mapFn.Meta["type_params"].([]map[string]string)
	require.Len(t, tp, 2)
	assert.Equal(t, "T", tp[0]["name"])
	assert.Equal(t, "any", tp[0]["bound"])
	assert.Equal(t, "R", tp[1]["name"])
	assert.Equal(t, "comparable", tp[1]["bound"])
}

func TestGoExtractor_ThrowsErrorReturn(t *testing.T) {
	src := []byte(`package x

func DoWork() error { return nil }

func ParseInt(s string) (int, error) { return 0, nil }

func MakeUser(name string) (*User, *MyError) { return nil, nil }

func NoError() string { return "" }

type MyError struct{}
type User struct{}
`)
	e := NewGoExtractor()
	result, err := e.Extract("x.go", src)
	require.NoError(t, err)

	throws := edgesOfKind(result.Edges, graph.EdgeThrows)
	throwsFrom := map[string]string{}
	for _, e := range throws {
		throwsFrom[e.From] = e.To
	}
	assert.Equal(t, "external::error", throwsFrom["x.go::DoWork"], "DoWork should throw external::error")
	assert.Equal(t, "external::error", throwsFrom["x.go::ParseInt"], "ParseInt's last return is error")
	assert.Equal(t, "unresolved::MyError", throwsFrom["x.go::MakeUser"], "MakeUser throws *MyError")

	if _, ok := throwsFrom["x.go::NoError"]; ok {
		t.Fatalf("NoError shouldn't have an EdgeThrows")
	}
}

func TestGoExtractor_ImportNodes(t *testing.T) {
	src := []byte(`package x

import (
	"fmt"
	"context"
	"github.com/zzet/gortex/internal/graph"
	g "github.com/zzet/gortex/internal/graph"
)
`)
	e := NewGoExtractor()
	result, err := e.Extract("x.go", src)
	require.NoError(t, err)

	imports := nodesOfKind(result.Nodes, graph.KindImport)
	require.GreaterOrEqual(t, len(imports), 4)

	byID := map[string]*graph.Node{}
	for _, n := range imports {
		byID[n.ID] = n
	}

	fmtNode := byID["x.go::import::fmt"]
	require.NotNil(t, fmtNode, "fmt import node missing")
	assert.Equal(t, "fmt", fmtNode.Meta["path"])
	assert.Equal(t, false, fmtNode.Meta["is_external"], "fmt is stdlib, not external")

	graphNode := byID["x.go::import::github.com/zzet/gortex/internal/graph"]
	require.NotNil(t, graphNode, "module import node missing")
	assert.Equal(t, true, graphNode.Meta["is_external"], "github.com path should be external")

	// Aliased imports: the same path appears once with rawAlias != ""
	// (the second `g "..."` reuses the same ID since we key by path,
	// not by alias). The alias surfaces on the node Meta when
	// declared; for plain re-imports of the same path we keep the
	// last-write-wins behaviour the indexer expects.
	hasAlias := false
	for _, n := range imports {
		if a, _ := n.Meta["alias"].(string); a == "g" {
			hasAlias = true
		}
	}
	assert.True(t, hasAlias, "aliased import 'g' should surface in Meta")
}

func TestGoExtractor_Complexity(t *testing.T) {
	src := []byte(`package x

func Simple() int { return 1 }

func Branchy(x int) int {
	if x > 0 {
		return 1
	}
	for i := 0; i < x; i++ {
		switch i {
		case 0:
			return 0
		case 1:
			return 1
		}
	}
	return -1
}
`)
	e := NewGoExtractor()
	result, err := e.Extract("x.go", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	// Simple has no branches → no complexity meta.
	simple := byID["x.go::Simple"]
	require.NotNil(t, simple)
	if _, ok := simple.Meta["complexity"]; ok {
		t.Fatalf("Simple should have no complexity meta, got %v", simple.Meta["complexity"])
	}

	// Branchy: 1 (base) + if + for + switch + 2 cases = 6.
	branchy := byID["x.go::Branchy"]
	require.NotNil(t, branchy)
	c, ok := branchy.Meta["complexity"].(int)
	require.True(t, ok, "Branchy should have integer complexity")
	if c < 4 {
		t.Fatalf("Branchy complexity = %d, expected >= 4", c)
	}
}
