package languages

import (
	"strings"
	"testing"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/kotlin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// TestKotlinAST_Debug dumps the AST to verify node types used in queries.
func TestKotlinAST_Debug(t *testing.T) {
	src := []byte(`package com.example

import kotlin.collections.List

interface Greeter {
    fun greet(name: String): String
}

class HelloGreeter : Greeter {
    override fun greet(name: String): String {
        return "Hello, $name"
    }

    fun helper() {}
}

data class User(val name: String, val age: Int)

object Singleton {
    fun instance(): Singleton = this
}

fun topLevel(): Int {
    println("hello")
    return 42
}

val VERSION = "1.0"
var counter = 0
`)
	lang := kotlin.GetLanguage()
	tree, err := parser.ParseFile(src, lang)
	require.NoError(t, err)
	defer tree.Close()

	root := tree.RootNode()
	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		indent := ""
		for i := 0; i < depth; i++ {
			indent += "  "
		}
		if n.IsNamed() {
			t.Logf("%s%s [%d:%d - %d:%d] %q", indent, n.Type(),
				n.StartPoint().Row, n.StartPoint().Column,
				n.EndPoint().Row, n.EndPoint().Column,
				truncate(n.Content(src), 60))
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func TestKotlinExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class UserService {
    fun getUser(id: String): User {
        return findById(id)
    }

    fun deleteUser(id: String) {
        remove(id)
    }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("UserService.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "getUser")
	assert.Contains(t, names, "deleteUser")

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, edge := range memberEdges {
		assert.Equal(t, "UserService.kt::UserService", edge.To)
	}
}

func TestKotlinExtractor_Interface(t *testing.T) {
	src := []byte(`interface Repository {
    fun findById(id: String): User
    fun save(user: User)
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Repository.kt", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1, "expected 1 interface, got %d", len(ifaces))
	assert.Equal(t, "Repository", ifaces[0].Name)
}

func TestKotlinExtractor_EnumClass(t *testing.T) {
	src := []byte(`enum class Direction {
    NORTH, SOUTH, EAST, WEST
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Direction.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Direction", types[0].Name)
	require.NotNil(t, types[0].Meta, "enum should carry Meta[\"kind\"]=\"enum\"")
	assert.Equal(t, "enum", types[0].Meta["kind"])

	entries := map[string]bool{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["kind"] == "enum_entry" {
			entries[n.Name] = true
		}
	}
	assert.Equal(t, map[string]bool{"NORTH": true, "SOUTH": true, "EAST": true, "WEST": true}, entries)
}

func TestKotlinExtractor_TopLevelFunction(t *testing.T) {
	src := []byte(`fun greet(name: String): String {
    println(name)
    return "Hello, $name"
}

fun add(a: Int, b: Int): Int = a + b
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("utils.kt", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 2)
	names := []string{funcs[0].Name, funcs[1].Name}
	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")

	// Should not be methods.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Empty(t, methods)
}

func TestKotlinExtractor_Imports(t *testing.T) {
	src := []byte(`import kotlin.collections.List
import com.example.service.UserService
import java.util.UUID

fun main() {}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("main.kt", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 3)
}

func TestKotlinExtractor_DataClass(t *testing.T) {
	src := []byte(`data class User(val name: String, val age: Int)
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("User.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "User", types[0].Name)
}

func TestKotlinExtractor_Object(t *testing.T) {
	src := []byte(`object Singleton {
    fun getInstance(): Singleton {
        return this
    }
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("Singleton.kt", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Singleton", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "getInstance", methods[0].Name)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 1)
	assert.Equal(t, "Singleton.kt::Singleton", memberEdges[0].To)
}

func TestKotlinExtractor_TopLevelProperties(t *testing.T) {
	src := []byte(`val VERSION = "1.0"
var counter = 0

class Foo {
    val internal = "hidden"
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("config.kt", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 2, "expected only top-level properties")
	names := []string{vars[0].Name, vars[1].Name}
	assert.Contains(t, names, "VERSION")
	assert.Contains(t, names, "counter")
}

func TestKotlinExtractor_CallSites(t *testing.T) {
	src := []byte(`fun main() {
    println("hello")
    greet("world")
}

fun greet(name: String) {
    println(name)
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("main.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2, "expected at least 2 call edges")

	// Verify call targets contain println and greet.
	targets := make(map[string]bool)
	for _, c := range calls {
		targets[c.To] = true
	}
	assert.True(t, targets["unresolved::*.println"], "missing println call")
	assert.True(t, targets["unresolved::*.greet"], "missing greet call")
}

func TestKotlinExtractor_TypeEnv_ExplicitType(t *testing.T) {
	src := []byte(`class UserService {
    fun save() {}
}

fun main() {
    val svc: UserService = UserService()
    svc.save()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to save")
	require.NotNil(t, saveCall.Meta, "expected Meta on save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestKotlinExtractor_TypeEnv_Constructor(t *testing.T) {
	src := []byte(`class Client {
    fun connect() {}
}

fun main() {
    val client = Client()
    client.connect()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestKotlinExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`fun getService(): Any = TODO()

fun main() {
    val svc = getService()
    svc.process()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.Nil(t, processCall.Meta, "unknown type should not produce Meta")
}

func TestKotlinExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`class Order {
    val id: Int = 0
}

class UserService {
    fun getOrder(): Order {
        return Order()
    }
}

fun main() {
    val svc = UserService()
    svc.getOrder().toString()
}
`)
	e := NewKotlinExtractor()
	result, err := e.Extract("app.kt", src)
	require.NoError(t, err)

	// Verify return_type is set on getOrder method.
	var getOrderNode *graph.Node
	for _, n := range result.Nodes {
		if n.Name == "getOrder" {
			getOrderNode = n
			break
		}
	}
	require.NotNil(t, getOrderNode, "expected a node for getOrder")
	assert.Equal(t, "Order", getOrderNode.Meta["return_type"])

	// Verify chain resolution: svc.getOrder() should resolve to Order.
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var toStringCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "toString") {
			toStringCall = c
			break
		}
	}
	require.NotNil(t, toStringCall, "expected a call edge to toString")
	require.NotNil(t, toStringCall.Meta, "expected Meta on toString call edge")
	assert.Equal(t, "Order", toStringCall.Meta["receiver_type"])
}
