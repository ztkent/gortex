package languages

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/dartlang"
)

// DartExtractor extracts Dart source files.
type DartExtractor struct {
	lang *sitter.Language
}

func NewDartExtractor() *DartExtractor {
	return &DartExtractor{lang: dartlang.GetLanguage()}
}

func (e *DartExtractor) Language() string     { return "dart" }
func (e *DartExtractor) Extensions() []string { return []string{".dart"} }

func (e *DartExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "dart",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Classes, enums, mixins, extensions — walk the tree to distinguish types.
	e.extractTypes(root, src, filePath, fileNode, result, seen)

	// Methods inside class/mixin/enum/extension bodies.
	e.extractMethods(root, src, filePath, fileNode, result, seen)

	// Top-level functions (function_signature + function_body at program level).
	e.extractTopLevelFunctions(root, src, filePath, fileNode, result, seen)

	// Top-level variables.
	e.extractTopLevelVariables(root, src, filePath, fileNode, result, seen)

	// Imports.
	e.extractImports(root, src, filePath, fileNode, result)

	// Call sites.
	e.extractCalls(root, src, filePath, result)

	return result, nil
}

// extractTypes walks the root for class_definition, enum_declaration, mixin_declaration, extension_declaration.
func (e *DartExtractor) extractTypes(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	walkNodes(root, func(node *sitter.Node) {
		var name string
		var kind graph.NodeKind

		switch node.Type() {
		case "class_definition":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

			// Check for abstract interface class → KindInterface.
			if e.hasChildType(node, "abstract") && e.hasChildType(node, "interface") {
				kind = graph.KindInterface
			}

		case "enum_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

		case "mixin_declaration":
			// mixin_declaration has identifier as a child, not a named field.
			name = e.findChildIdentifier(node, src)
			if name == "" {
				return
			}
			kind = graph.KindType

		case "extension_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				// Anonymous extension — skip.
				return
			}
			name = nameNode.Content(src)
			kind = graph.KindType

		default:
			return
		}

		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true

		startLine := int(node.StartPoint().Row) + 1
		endLine := int(node.EndPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "dart",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
		})
	})
}

// extractMethods finds method_signature nodes inside class_body, extension_body, enum_body.
func (e *DartExtractor) extractMethods(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	// Collect type body ranges for ownership detection.
	typeBodyRanges := e.collectTypeBodyRanges(root, src)

	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "method_signature" {
			return
		}

		// Must be inside a body (class_body, extension_body, enum_body).
		parent := node.Parent()
		if parent == nil {
			return
		}
		parentType := parent.Type()
		if parentType != "class_body" && parentType != "extension_body" && parentType != "enum_body" {
			return
		}

		name := e.extractMethodName(node, src)
		if name == "" {
			return
		}

		// Find enclosing type name.
		typeName := ""
		startLine := int(node.StartPoint().Row)
		if tn, ok := e.findEnclosingType(typeBodyRanges, startLine); ok {
			typeName = tn
		}

		methodID := filePath + "::" + typeName + "." + name
		if seen[methodID] {
			methodID = filePath + "::" + typeName + "." + name + "_L" + fmt.Sprint(startLine+1)
		}
		if seen[methodID] {
			return
		}
		seen[methodID] = true
		seen[filePath+"::_method_L"+fmt.Sprint(startLine+1)] = true

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: methodID, Kind: graph.KindMethod, Name: name,
			FilePath: filePath, StartLine: startLine + 1, EndLine: int(node.EndPoint().Row) + 1,
			Language: "dart",
			Meta:     map[string]any{"receiver": typeName},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: methodID, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine + 1,
		})
		if typeName != "" {
			typeID := filePath + "::" + typeName
			result.Edges = append(result.Edges, &graph.Edge{
				From: methodID, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: startLine + 1,
			})
		}
	})
}

// extractTopLevelFunctions finds function_signature nodes that are direct children of program
// (followed by function_body).
func (e *DartExtractor) extractTopLevelFunctions(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		if child.Type() != "function_signature" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Content(src)
		startLine := int(child.StartPoint().Row) + 1

		// Check if next sibling is function_body to get end line.
		endLine := int(child.EndPoint().Row) + 1
		if i+1 < int(root.ChildCount()) {
			next := root.Child(i + 1)
			if next.Type() == "function_body" {
				endLine = int(next.EndPoint().Row) + 1
			}
		}

		id := filePath + "::" + name
		if seen[id] {
			id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
		}
		if seen[id] {
			continue
		}
		seen[id] = true

		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: endLine,
			Language: "dart",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
		})
	}
}

// extractTopLevelVariables finds top-level initialized_variable_definition and
// static_final_declaration_list nodes at program level.
func (e *DartExtractor) extractTopLevelVariables(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		switch child.Type() {
		case "initialized_variable_definition":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			name := nameNode.Content(src)
			startLine := int(child.StartPoint().Row) + 1
			id := filePath + "::" + name
			if seen[id] {
				continue
			}
			seen[id] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: id, Kind: graph.KindVariable, Name: name,
				FilePath: filePath, StartLine: startLine, EndLine: int(child.EndPoint().Row) + 1,
				Language: "dart",
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
			})

		case "static_final_declaration_list":
			// Walk children for static_final_declaration nodes.
			for j := 0; j < int(child.ChildCount()); j++ {
				decl := child.Child(j)
				if decl.Type() != "static_final_declaration" {
					continue
				}
				name := e.findChildIdentifier(decl, src)
				if name == "" {
					continue
				}
				startLine := int(decl.StartPoint().Row) + 1
				id := filePath + "::" + name
				if seen[id] {
					continue
				}
				seen[id] = true
				result.Nodes = append(result.Nodes, &graph.Node{
					ID: id, Kind: graph.KindVariable, Name: name,
					FilePath: filePath, StartLine: startLine, EndLine: int(decl.EndPoint().Row) + 1,
					Language: "dart",
				})
				result.Edges = append(result.Edges, &graph.Edge{
					From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: startLine,
				})
			}
		}
	}
}

// extractImports finds import_or_export nodes and extracts the URI.
func (e *DartExtractor) extractImports(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "import_or_export" {
			return
		}

		// Extract URI from the import text.
		text := node.Content(src)
		uri := extractDartImportURI(text)
		if uri == "" {
			return
		}

		startLine := int(node.StartPoint().Row) + 1
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: "unresolved::import::" + uri,
			Kind: graph.EdgeImports, FilePath: filePath, Line: startLine,
		})
	})
}

// extractCalls finds function/method call sites.
// In Dart's tree-sitter grammar, calls appear as:
//   expression_statement: identifier selector(argument_part) ...
//   e.g. print('hello') → identifier "print", selector "(…)" with argument_part
// We detect an identifier followed by a selector sibling that contains an argument_part.
func (e *DartExtractor) extractCalls(
	root *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult,
) {
	funcRanges := buildFuncRanges(result)

	walkNodes(root, func(node *sitter.Node) {
		if node.Type() != "identifier" {
			return
		}

		// Check if next sibling is a selector containing argument_part,
		// or directly an argument_part/arguments.
		next := node.NextSibling()
		if next == nil {
			return
		}

		isCall := false
		switch next.Type() {
		case "selector":
			// selector may contain argument_part (direct call) like print(...)
			for j := 0; j < int(next.ChildCount()); j++ {
				if next.Child(j).Type() == "argument_part" {
					isCall = true
					break
				}
			}
		case "argument_part", "arguments":
			isCall = true
		}

		if !isCall {
			return
		}

		name := node.Content(src)
		line := int(node.StartPoint().Row) + 1
		callerID := findEnclosingFunc(funcRanges, line)
		if callerID == "" {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::*." + name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
		})
	})
}

// --- helpers ---

func (e *DartExtractor) hasChildType(node *sitter.Node, typeName string) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.Child(i).Type() == typeName {
			return true
		}
	}
	return false
}

func (e *DartExtractor) findChildIdentifier(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// extractMethodName extracts the name from a method_signature node.
// method_signature wraps function_signature, getter_signature, setter_signature,
// constructor_signature, operator_signature, factory_constructor_signature.
func (e *DartExtractor) extractMethodName(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "function_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "getter_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "setter_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return "set " + nameNode.Content(src)
			}
		case "constructor_signature":
			nameNode := child.ChildByFieldName("name")
			if nameNode != nil {
				return nameNode.Content(src)
			}
		case "factory_constructor_signature":
			return e.findChildIdentifier(child, src)
		case "operator_signature":
			// operator + binary_operator child
			for j := 0; j < int(child.ChildCount()); j++ {
				c := child.Child(j)
				if c.Type() == "binary_operator" || c.Type() == "tilde_operator" {
					return "operator " + strings.TrimSpace(c.Content(src))
				}
			}
		}
	}
	return ""
}

type dartTypeRange struct {
	typeName  string
	startLine int // 0-based
	endLine   int // 0-based
}

func (e *DartExtractor) collectTypeBodyRanges(root *sitter.Node, src []byte) []dartTypeRange {
	var ranges []dartTypeRange
	walkNodes(root, func(node *sitter.Node) {
		var name string
		switch node.Type() {
		case "class_definition":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		case "enum_declaration":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		case "mixin_declaration":
			name = e.findChildIdentifier(node, src)
		case "extension_declaration":
			n := node.ChildByFieldName("name")
			if n != nil {
				name = n.Content(src)
			}
		default:
			return
		}
		if name == "" {
			return
		}
		ranges = append(ranges, dartTypeRange{
			typeName:  name,
			startLine: int(node.StartPoint().Row),
			endLine:   int(node.EndPoint().Row),
		})
	})
	return ranges
}

func (e *DartExtractor) findEnclosingType(ranges []dartTypeRange, line int) (string, bool) {
	best := ""
	bestSize := int(^uint(0) >> 1)
	for _, r := range ranges {
		if line >= r.startLine && line <= r.endLine {
			size := r.endLine - r.startLine
			if size < bestSize {
				bestSize = size
				best = r.typeName
			}
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// extractDartImportURI extracts the URI string from an import/export statement text.
// e.g. "import 'package:flutter/material.dart';" → "package:flutter/material.dart"
func extractDartImportURI(text string) string {
	// Find content between quotes.
	for _, q := range []byte{'\'', '"'} {
		start := strings.IndexByte(text, q)
		if start < 0 {
			continue
		}
		end := strings.IndexByte(text[start+1:], q)
		if end < 0 {
			continue
		}
		return text[start+1 : start+1+end]
	}
	return ""
}
