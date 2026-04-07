package languages

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const (
	csharpQClass = `(class_declaration
		name: (identifier) @class.name) @class.def`

	csharpQInterface = `(interface_declaration
		name: (identifier) @iface.name) @iface.def`

	csharpQStruct = `(struct_declaration
		name: (identifier) @struct.name) @struct.def`

	csharpQEnum = `(enum_declaration
		name: (identifier) @enum.name) @enum.def`

	csharpQNamespace = `(namespace_declaration
		name: (_) @ns.name) @ns.def`

	csharpQUsing = `(using_directive (_) @using.path) @using.def`

	csharpQClassMethod = `(class_declaration
		name: (identifier) @class.name
		body: (declaration_list
			(method_declaration
				name: (identifier) @method.name) @method.def))`

	csharpQStructMethod = `(struct_declaration
		name: (identifier) @struct.name
		body: (declaration_list
			(method_declaration
				name: (identifier) @method.name) @method.def))`

	csharpQClassConstructor = `(class_declaration
		name: (identifier) @class.name
		body: (declaration_list
			(constructor_declaration
				name: (identifier) @ctor.name) @ctor.def))`

	csharpQStructConstructor = `(struct_declaration
		name: (identifier) @struct.name
		body: (declaration_list
			(constructor_declaration
				name: (identifier) @ctor.name) @ctor.def))`

	csharpQClassField = `(class_declaration
		name: (identifier) @class.name
		body: (declaration_list
			(field_declaration
				(variable_declaration
					(variable_declarator
						name: (identifier) @field.name))) @field.def))`

	csharpQStructField = `(struct_declaration
		name: (identifier) @struct.name
		body: (declaration_list
			(field_declaration
				(variable_declaration
					(variable_declarator
						name: (identifier) @field.name))) @field.def))`

	csharpQClassProperty = `(class_declaration
		name: (identifier) @class.name
		body: (declaration_list
			(property_declaration
				name: (identifier) @prop.name) @prop.def))`

	csharpQStructProperty = `(struct_declaration
		name: (identifier) @struct.name
		body: (declaration_list
			(property_declaration
				name: (identifier) @prop.name) @prop.def))`

	csharpQIfaceMethod = `(interface_declaration
		name: (identifier) @iface.name
		body: (declaration_list
			(method_declaration
				name: (identifier) @iface.method.name)))`

	csharpQCall = `(invocation_expression
		function: (member_access_expression
			name: (identifier) @call.name)) @call.expr`
)

// CSharpExtractor extracts C# source files.
type CSharpExtractor struct {
	lang *sitter.Language
}

func NewCSharpExtractor() *CSharpExtractor {
	return &CSharpExtractor{lang: csharp.GetLanguage()}
}

func (e *CSharpExtractor) Language() string     { return "csharp" }
func (e *CSharpExtractor) Extensions() []string { return []string{".cs"} }

func (e *CSharpExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "csharp",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Namespaces.
	matches, _ := parser.RunQuery(csharpQNamespace, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["ns.name"].Text
		def := m.Captures["ns.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindPackage, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Classes.
	matches, _ = parser.RunQuery(csharpQClass, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["class.name"].Text
		def := m.Captures["class.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Interfaces.
	matches, _ = parser.RunQuery(csharpQInterface, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["iface.name"].Text
		def := m.Captures["iface.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindInterface, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Interface method names into Meta["methods"].
	ifaceMethodMatches, _ := parser.RunQuery(csharpQIfaceMethod, e.lang, root, src)
	ifaceMethods := make(map[string][]string)
	for _, m := range ifaceMethodMatches {
		ifaceName := m.Captures["iface.name"].Text
		methodName := m.Captures["iface.method.name"].Text
		ifaceMethods[ifaceName] = append(ifaceMethods[ifaceName], methodName)
	}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindInterface {
			if methods, ok := ifaceMethods[n.Name]; ok {
				if n.Meta == nil {
					n.Meta = make(map[string]any)
				}
				n.Meta["methods"] = methods
			}
		}
	}

	// Structs.
	matches, _ = parser.RunQuery(csharpQStruct, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["struct.name"].Text
		def := m.Captures["struct.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Enums.
	matches, _ = parser.RunQuery(csharpQEnum, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["enum.name"].Text
		def := m.Captures["enum.def"]
		id := filePath + "::" + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	}

	// Methods in classes.
	e.extractMethods(filePath, src, root, result, seen, csharpQClassMethod, "class.name")

	// Methods in structs.
	e.extractMethods(filePath, src, root, result, seen, csharpQStructMethod, "struct.name")

	// Constructors in classes.
	e.extractConstructors(filePath, src, root, result, seen, csharpQClassConstructor, "class.name")

	// Constructors in structs.
	e.extractConstructors(filePath, src, root, result, seen, csharpQStructConstructor, "struct.name")

	// Fields in classes.
	e.extractFields(filePath, src, root, result, seen, csharpQClassField, "class.name", "field")

	// Fields in structs.
	e.extractFields(filePath, src, root, result, seen, csharpQStructField, "struct.name", "field")

	// Properties in classes.
	e.extractFields(filePath, src, root, result, seen, csharpQClassProperty, "class.name", "prop")

	// Properties in structs.
	e.extractFields(filePath, src, root, result, seen, csharpQStructProperty, "struct.name", "prop")

	// Using directives.
	matches, _ = parser.RunQuery(csharpQUsing, e.lang, root, src)
	for _, m := range matches {
		path := m.Captures["using.path"]
		importPath := strings.ReplaceAll(path.Text, ".", "/")
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + importPath,
			Kind: graph.EdgeImports, FilePath: filePath, Line: path.StartLine + 1,
		})
	}

	// Call sites.
	funcRanges := buildFuncRanges(result)
	matches, _ = parser.RunQuery(csharpQCall, e.lang, root, src)
	for _, m := range matches {
		name := m.Captures["call.name"].Text
		expr := m.Captures["call.expr"]
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::*." + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	}

	return result, nil
}

func (e *CSharpExtractor) extractMethods(
	filePath string, src []byte, root *sitter.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	query string, ownerCapture string,
) {
	matches, _ := parser.RunQuery(query, e.lang, root, src)
	for _, m := range matches {
		ownerName := m.Captures[ownerCapture].Text
		name := m.Captures["method.name"].Text
		def := m.Captures["method.def"]
		id := filePath + "::" + ownerName + "." + name
		if seen[id] {
			id = filePath + "::" + ownerName + "." + name + "_L" + fmt.Sprint(def.StartLine+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
			Meta:     map[string]any{"receiver": ownerName},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		ownerID := filePath + "::" + ownerName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

func (e *CSharpExtractor) extractConstructors(
	filePath string, src []byte, root *sitter.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	query string, ownerCapture string,
) {
	matches, _ := parser.RunQuery(query, e.lang, root, src)
	for _, m := range matches {
		ownerName := m.Captures[ownerCapture].Text
		def := m.Captures["ctor.def"]
		id := filePath + "::" + ownerName + ".<init>"
		if seen[id] {
			id = filePath + "::" + ownerName + ".<init>_L" + fmt.Sprint(def.StartLine+1)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: ownerName + ".<init>",
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
			Meta:     map[string]any{"receiver": ownerName},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		ownerID := filePath + "::" + ownerName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}

func (e *CSharpExtractor) extractFields(
	filePath string, src []byte, root *sitter.Node,
	result *parser.ExtractionResult, seen map[string]bool,
	query string, ownerCapture string, fieldCapture string,
) {
	matches, _ := parser.RunQuery(query, e.lang, root, src)
	for _, m := range matches {
		ownerName := m.Captures[ownerCapture].Text
		name := m.Captures[fieldCapture+".name"].Text
		def := m.Captures[fieldCapture+".def"]
		id := filePath + "::" + ownerName + "." + name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "csharp",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
		ownerID := filePath + "::" + ownerName
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: def.StartLine + 1,
		})
	}
}
