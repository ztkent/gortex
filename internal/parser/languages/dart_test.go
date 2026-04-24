package languages

import (
	"testing"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/dartlang"
)

// TestDartAST_Debug dumps the AST to verify node types used in queries.
func TestDartAST_Debug(t *testing.T) {
	src := []byte(`import 'package:flutter/material.dart';

abstract class Animal {
  String get name;
  void speak();
}

class Dog extends Animal {
  @override
  String get name => 'Dog';

  @override
  void speak() {
    print('Woof!');
  }

  void fetch(String item) {
    print('Fetching $item');
  }
}

enum Color { red, green, blue }

mixin Swimming {
  void swim() {
    print('Swimming!');
  }
}

extension StringExt on String {
  String capitalize() {
    return '${this[0].toUpperCase()}${substring(1)}';
  }
}

void main() {
  final dog = Dog();
  dog.speak();
  dog.fetch('ball');
}

const version = '1.0.0';
`)
	lang := dartlang.GetLanguage()
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

func TestDartExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class UserService {
  Future<User> getUser(String id) async {
    return await findById(id);
  }

  void deleteUser(String id) {
    remove(id);
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("user_service.dart", src)
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
		assert.Equal(t, "user_service.dart::UserService", edge.To)
	}

	// Methods must span signature + body, not the declaration line
	// alone. A one-line span (end == start) breaks source viewers
	// and the shape extractor.
	for _, m := range methods {
		if m.EndLine <= m.StartLine {
			t.Errorf("method %s has end_line (%d) <= start_line (%d) — body span missing",
				m.Name, m.EndLine, m.StartLine)
		}
	}
	// Exact expected spans given the fixture above. Changing the
	// fixture is fine; changing these numbers without is a regression.
	byName := map[string]*graph.Node{}
	for _, m := range methods {
		byName[m.Name] = m
	}
	if m := byName["getUser"]; m != nil {
		assert.Equal(t, 2, m.StartLine, "getUser start")
		assert.Equal(t, 4, m.EndLine, "getUser end (through closing brace)")
	}
	if m := byName["deleteUser"]; m != nil {
		assert.Equal(t, 6, m.StartLine, "deleteUser start")
		assert.Equal(t, 8, m.EndLine, "deleteUser end (through closing brace)")
	}
}

func TestDartExtractor_AbstractClass(t *testing.T) {
	src := []byte(`abstract class Repository {
  Future<User> findById(String id);
  Future<void> save(User user);
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("repository.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Repository", types[0].Name)
}

func TestDartExtractor_TopLevelFunction(t *testing.T) {
	src := []byte(`void greet(String name) {
  print('Hello, $name');
}

int add(int a, int b) => a + b;
`)
	e := NewDartExtractor()
	result, err := e.Extract("utils.dart", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 2)
	names := []string{funcs[0].Name, funcs[1].Name}
	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Empty(t, methods)
}

func TestDartExtractor_Enum(t *testing.T) {
	src := []byte(`enum Status {
  active,
  inactive,
  pending;

  String get label => name.toUpperCase();
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("status.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Status", types[0].Name)
}

func TestDartExtractor_Mixin(t *testing.T) {
	src := []byte(`mixin Logging {
  void log(String message) {
    print('[LOG] $message');
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("logging.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Logging", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "log", methods[0].Name)
}

func TestDartExtractor_Extension(t *testing.T) {
	src := []byte(`extension NumberParsing on String {
  int toInt() {
    return int.parse(this);
  }

  double toDouble() {
    return double.parse(this);
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("extensions.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "NumberParsing", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "toInt")
	assert.Contains(t, names, "toDouble")
}

func TestDartExtractor_Imports(t *testing.T) {
	src := []byte(`import 'dart:async';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
export 'src/widget.dart';

void main() {}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 4)
}

func TestDartExtractor_CallSites(t *testing.T) {
	src := []byte(`void main() {
  print('hello');
  greet('world');
}

void greet(String name) {
  print(name);
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2, "expected at least 2 call edges")

	targets := make(map[string]bool)
	for _, c := range calls {
		targets[c.To] = true
	}
	assert.True(t, targets["unresolved::*.print"], "missing print call")
	assert.True(t, targets["unresolved::*.greet"], "missing greet call")
}

func TestDartExtractor_FlutterWidget(t *testing.T) {
	src := []byte(`import 'package:flutter/material.dart';

class MyApp extends StatelessWidget {
  const MyApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      home: Scaffold(
        body: Center(
          child: Text('Hello Flutter'),
        ),
      ),
    );
  }
}

void main() {
  runApp(const MyApp());
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "MyApp", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "main", funcs[0].Name)
}
