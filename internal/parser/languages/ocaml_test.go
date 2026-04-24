package languages

import (
	"testing"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/ocaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestOCamlAST_Debug(t *testing.T) {
	src := []byte(`
open Printf

type user = { name : string; age : int }

module Config = struct
  let default_port = 8080
  let max_retries = 3
end

let greet name =
  printf "Hello, %s!\n" name

let add x y = x + y

let () =
  greet "world"
`)
	lang := ocaml.GetLanguage()
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

func TestOCamlExtractor_Functions(t *testing.T) {
	src := []byte(`
let greet name =
  Printf.printf "Hello, %s!\n" name

let add x y = x + y

let double = fun x -> x * 2
`)
	e := NewOCamlExtractor()
	result, err := e.Extract("utils.ml", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	names := make([]string, len(funcs))
	for i, f := range funcs {
		names[i] = f.Name
	}

	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")
}

func TestOCamlExtractor_Types(t *testing.T) {
	src := []byte(`
type color = Red | Green | Blue

type 'a tree =
  | Leaf
  | Node of 'a * 'a tree * 'a tree

type user = {
  name : string;
  age : int;
}
`)
	e := NewOCamlExtractor()
	result, err := e.Extract("types.ml", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.GreaterOrEqual(t, len(types), 2)
}

func TestOCamlExtractor_Module(t *testing.T) {
	src := []byte(`
module Config = struct
  let port = 8080
  let host = "localhost"
end
`)
	e := NewOCamlExtractor()
	result, err := e.Extract("config.ml", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	hasConfig := false
	for _, tp := range types {
		if tp.Name == "Config" {
			hasConfig = true
		}
	}
	assert.True(t, hasConfig, "should find Config module")
}

func TestOCamlExtractor_Open(t *testing.T) {
	src := []byte(`
open Printf
open List

let () = printf "hello\n"
`)
	e := NewOCamlExtractor()
	result, err := e.Extract("main.ml", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.GreaterOrEqual(t, len(imports), 2)
}

func TestOCamlExtractor_Variables(t *testing.T) {
	src := []byte(`
let version = "1.0.0"
let max_count = 100
`)
	e := NewOCamlExtractor()
	result, err := e.Extract("consts.ml", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.GreaterOrEqual(t, len(vars), 2)
}
