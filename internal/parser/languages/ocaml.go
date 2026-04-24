package languages

import (
	"fmt"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/ocaml"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// OCamlExtractor extracts OCaml source files.
type OCamlExtractor struct {
	lang *sitter.Language
}

func NewOCamlExtractor() *OCamlExtractor {
	return &OCamlExtractor{lang: ocaml.GetLanguage()}
}

func (e *OCamlExtractor) Language() string     { return "ocaml" }
func (e *OCamlExtractor) Extensions() []string { return []string{".ml", ".mli"} }

func (e *OCamlExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "ocaml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST to extract definitions.
	e.walkExtract(root, src, filePath, fileNode, result, seen, "")

	// Call sites.
	funcRanges := buildFuncRanges(result)
	e.extractCalls(root, src, filePath, result, funcRanges)

	return result, nil
}

func (e *OCamlExtractor) walkExtract(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "value_definition":
			e.extractValueDef(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "type_definition":
			e.extractTypeDef(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "module_definition":
			e.extractModuleDef(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "module_type_definition":
			e.extractModuleTypeDef(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "open_module":
			e.extractOpen(child, src, filePath, fileNode, result)

		case "class_definition":
			e.extractClassDef(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "external":
			e.extractExternal(child, src, filePath, fileNode, result, seen, modulePrefix)

		case "value_specification":
			// .mli signature files
			e.extractValueSpec(child, src, filePath, fileNode, result, seen, modulePrefix)
		}
	}
}

// extractValueDef handles `let name = ...` and `let rec name ... = ...`
func (e *OCamlExtractor) extractValueDef(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child == nil {
			continue
		}

		if child.Type() == "let_binding" {
			name := ""
			kind := graph.KindFunction
			hasParams := false

			// Walk let_binding children to find pattern (name) and check for parameters.
			for j := 0; j < int(child.NamedChildCount()); j++ {
				part := child.NamedChild(j)
				if part == nil {
					continue
				}
				switch part.Type() {
				case "value_name", "value_pattern":
					name = part.Content(src)
				case "parameter":
					hasParams = true
				case "fun_expression", "function_expression":
					hasParams = true
				}
			}

			if name == "" || name == "_" {
				continue
			}

			if !hasParams {
				kind = graph.KindVariable
			}

			qualName := name
			if modulePrefix != "" {
				qualName = modulePrefix + "." + name
			}

			id := filePath + "::" + qualName
			if seen[id] {
				continue
			}
			seen[id] = true

			startLine := int(child.StartPoint().Row) + 1
			endLine := int(child.EndPoint().Row) + 1

			n := &graph.Node{
				ID: id, Kind: kind, Name: name,
				FilePath: filePath, StartLine: startLine, EndLine: endLine,
				Language: "ocaml",
			}
			if hasParams {
				n.Meta = map[string]any{"signature": "let " + name + " ..."}
			}
			result.Nodes = append(result.Nodes, n)
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: startLine,
			})
		}
	}
}

// extractTypeDef handles `type name = ...`
func (e *OCamlExtractor) extractTypeDef(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child == nil || child.Type() != "type_binding" {
			continue
		}

		name := ""
		for j := 0; j < int(child.NamedChildCount()); j++ {
			part := child.NamedChild(j)
			if part != nil && (part.Type() == "type_constructor" || part.Type() == "type_variable") {
				name = part.Content(src)
				break
			}
		}

		if name == "" {
			// Try first named child as name.
			if child.NamedChildCount() > 0 {
				name = child.NamedChild(0).Content(src)
			}
		}

		if name == "" {
			continue
		}

		qualName := name
		if modulePrefix != "" {
			qualName = modulePrefix + "." + name
		}

		id := filePath + "::" + qualName
		if seen[id] {
			continue
		}
		seen[id] = true

		startLine := int(child.StartPoint().Row) + 1
		endLine := int(child.EndPoint().Row) + 1

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "ocaml",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}
}

// extractModuleDef handles `module Name = struct ... end`
// AST: module_definition → module_binding → module_name + structure
func (e *OCamlExtractor) extractModuleDef(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	// Find module_binding child.
	for i := 0; i < int(node.NamedChildCount()); i++ {
		binding := node.NamedChild(i)
		if binding == nil || binding.Type() != "module_binding" {
			continue
		}

		name := ""
		for j := 0; j < int(binding.NamedChildCount()); j++ {
			child := binding.NamedChild(j)
			if child != nil && child.Type() == "module_name" {
				name = child.Content(src)
				break
			}
		}
		if name == "" {
			continue
		}

		qualName := name
		if modulePrefix != "" {
			qualName = modulePrefix + "." + name
		}

		id := filePath + "::" + qualName
		if seen[id] {
			continue
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "ocaml",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})

		// Recurse into module body (structure node) for nested definitions.
		for j := 0; j < int(binding.NamedChildCount()); j++ {
			child := binding.NamedChild(j)
			if child != nil && (child.Type() == "structure" || child.Type() == "struct_expression") {
				e.walkExtract(child, src, filePath, fileNode, result, seen, qualName)
			}
		}
	}
}

// extractModuleTypeDef handles `module type Name = sig ... end`
func (e *OCamlExtractor) extractModuleTypeDef(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	name := ""
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child != nil && child.Type() == "module_type_name" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return
	}

	qualName := name
	if modulePrefix != "" {
		qualName = modulePrefix + "." + name
	}

	id := filePath + "::" + qualName
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "ocaml",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractOpen handles `open ModuleName`
func (e *OCamlExtractor) extractOpen(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child != nil && (child.Type() == "module_path" || child.Type() == "module_name" || child.Type() == "extended_module_path") {
			moduleName := child.Content(src)
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: "unresolved::import::" + moduleName,
				Kind: graph.EdgeImports, FilePath: filePath, Line: int(node.StartPoint().Row) + 1,
			})
			return
		}
	}
}

// extractClassDef handles `class name = object ... end`
func (e *OCamlExtractor) extractClassDef(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child == nil || child.Type() != "class_binding" {
			continue
		}

		name := ""
		for j := 0; j < int(child.NamedChildCount()); j++ {
			part := child.NamedChild(j)
			if part != nil && (part.Type() == "class_name" || part.Type() == "value_name") {
				name = part.Content(src)
				break
			}
		}

		if name == "" {
			continue
		}

		qualName := name
		if modulePrefix != "" {
			qualName = modulePrefix + "." + name
		}

		id := filePath + "::" + qualName
		if seen[id] {
			continue
		}
		seen[id] = true

		startLine := int(child.StartPoint().Row) + 1

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(child.EndPoint().Row) + 1,
			Language: "ocaml",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})

		// Extract methods from class body.
		e.extractMethods(child, src, filePath, fileNode, result, seen, qualName)
	}
}

// extractMethods extracts method definitions from a class body.
func (e *OCamlExtractor) extractMethods(
	classNode *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, className string,
) {
	walkNodes(classNode, func(node *sitter.Node) {
		if node.Type() != "method_definition" {
			return
		}
		name := ""
		for i := 0; i < int(node.NamedChildCount()); i++ {
			child := node.NamedChild(i)
			if child != nil && child.Type() == "method_name" {
				name = child.Content(src)
				break
			}
		}
		if name == "" {
			return
		}

		id := filePath + "::" + className + "." + name
		if seen[id] {
			return
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
			Language: "ocaml",
			Meta:     map[string]any{"receiver": className},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: filePath + "::" + className, Kind: graph.EdgeMemberOf,
			FilePath: filePath, Line: startLine,
		})
	})
}

// extractExternal handles `external name : type = "c_function"`
func (e *OCamlExtractor) extractExternal(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	name := ""
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child != nil && child.Type() == "value_name" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return
	}

	qualName := name
	if modulePrefix != "" {
		qualName = modulePrefix + "." + name
	}

	id := filePath + "::" + qualName
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "ocaml",
		Meta:     map[string]any{"signature": fmt.Sprintf("external %s : ...", name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractValueSpec handles `val name : type` in .mli files
func (e *OCamlExtractor) extractValueSpec(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool, modulePrefix string,
) {
	name := ""
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child != nil && child.Type() == "value_name" {
			name = child.Content(src)
			break
		}
	}
	if name == "" {
		return
	}

	qualName := name
	if modulePrefix != "" {
		qualName = modulePrefix + "." + name
	}

	id := filePath + "::" + qualName
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
		Language: "ocaml",
		Meta:     map[string]any{"signature": fmt.Sprintf("val %s : ...", name)},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractCalls walks the AST for function application nodes.
func (e *OCamlExtractor) extractCalls(
	root *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult, funcRanges []funcRange,
) {
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "application" {
			return
		}

		// The first child of an application is the function being called.
		if node.NamedChildCount() == 0 {
			return
		}
		funcNode := node.NamedChild(0)
		if funcNode == nil {
			return
		}

		var callName string
		switch funcNode.Type() {
		case "value_path", "value_name":
			callName = funcNode.Content(src)
		case "field_get_expression":
			// Module.function or record.field
			callName = funcNode.Content(src)
		default:
			return
		}

		if callName == "" {
			return
		}

		line := int(node.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			return
		}

		// Check if it's a qualified call (Module.function).
		if strings.Contains(callName, ".") {
			parts := strings.SplitN(callName, ".", 2)
			if len(parts) == 2 {
				result.Edges = append(result.Edges, &graph.Edge{
					From: callerID, To: "unresolved::*." + parts[1],
					Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
				})
				return
			}
		}

		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + callName,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	})
}
