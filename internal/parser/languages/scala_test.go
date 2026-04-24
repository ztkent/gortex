package languages

import (
	"fmt"
	"testing"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/scala"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestScalaAST_Debug(t *testing.T) {
	src := []byte(`import scala.collection.mutable

trait Repository {
  def findById(id: String): Option[User]
  def save(user: User): Unit
}

class UserService(repo: Repository) {
  def getUser(id: String): Option[User] = {
    repo.findById(id)
  }
}

object Main {
  def main(args: Array[String]): Unit = {
    println("Hello")
  }
}

case class User(name: String, email: String)
`)
	lang := scala.GetLanguage()
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
				truncate(n.Content(src), 80))
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
}

func TestScalaExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class UserService(repo: Repository) {
  def getUser(id: String): Option[User] = {
    repo.findById(id)
  }

  def deleteUser(id: String): Unit = {
    repo.delete(id)
  }
}
`)
	e := NewScalaExtractor()
	result, err := e.Extract("UserService.scala", src)
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
		assert.Equal(t, "UserService.scala::UserService", edge.To)
	}
}

func TestScalaExtractor_Trait(t *testing.T) {
	src := []byte(`trait Repository {
  def findById(id: String): Option[User]
  def save(user: User): Unit
}
`)
	e := NewScalaExtractor()
	result, err := e.Extract("Repository.scala", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1, "expected 1 interface (trait), got %d", len(ifaces))
	assert.Equal(t, "Repository", ifaces[0].Name)

	// Trait should have method names in Meta.
	meta := ifaces[0].Meta
	require.NotNil(t, meta)
	methodNames, ok := meta["methods"]
	require.True(t, ok, "expected Meta[\"methods\"] on trait")
	methodList, ok := methodNames.([]string)
	require.True(t, ok)
	assert.Contains(t, methodList, "findById")
	assert.Contains(t, methodList, "save")
}

func TestScalaExtractor_Object(t *testing.T) {
	src := []byte(`object Main {
  def main(args: Array[String]): Unit = {
    println("Hello")
  }

  def helper(): Int = 42
}
`)
	e := NewScalaExtractor()
	result, err := e.Extract("Main.scala", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Main", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "main")
	assert.Contains(t, names, "helper")

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, edge := range memberEdges {
		assert.Equal(t, "Main.scala::Main", edge.To)
	}
}

func TestScalaExtractor_Imports(t *testing.T) {
	src := []byte(`import scala.collection.mutable
import java.util.UUID
import com.example.service.UserService

object App {
  def run(): Unit = {}
}
`)
	e := NewScalaExtractor()
	result, err := e.Extract("App.scala", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 3)
}

func TestScalaExtractor_CaseClass(t *testing.T) {
	src := []byte(`case class User(name: String, email: String)
`)
	e := NewScalaExtractor()
	result, err := e.Extract("User.scala", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "User", types[0].Name)
}

func TestScalaExtractor_CallSites(t *testing.T) {
	src := []byte(`object Main {
  def main(args: Array[String]): Unit = {
    println("Hello")
    greet("world")
  }

  def greet(name: String): Unit = {
    println(name)
  }
}
`)
	e := NewScalaExtractor()
	result, err := e.Extract("Main.scala", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2, "expected at least 2 call edges")

	targets := make(map[string]bool)
	for _, c := range calls {
		targets[c.To] = true
	}
	assert.True(t, targets["unresolved::*.println"], "missing println call")
	assert.True(t, targets["unresolved::*.greet"], "missing greet call")
}

// Unused import to suppress compiler error for fmt if needed.
var _ = fmt.Sprint
