package languages

import (
	"fmt"
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/scala"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// ScalaExtractor extracts Scala source files.
type ScalaExtractor struct {
	lang *sitter.Language
}

func NewScalaExtractor() *ScalaExtractor {
	return &ScalaExtractor{lang: scala.GetLanguage()}
}

func (e *ScalaExtractor) Language() string     { return "scala" }
func (e *ScalaExtractor) Extensions() []string { return []string{".scala", ".sc"} }

func (e *ScalaExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "scala",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Walk the AST manually to extract all constructs.
	e.extractAll(root, src, filePath, fileNode, result, seen)

	return result, nil
}

func (e *ScalaExtractor) extractAll(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	walkNodes(root, func(node *sitter.Node) {
		switch node.Type() {
		case "trait_definition":
			e.extractTrait(node, src, filePath, fileNode, result, seen)
		case "class_definition":
			e.extractClass(node, src, filePath, fileNode, result, seen)
		case "object_definition":
			e.extractObject(node, src, filePath, fileNode, result, seen)
		case "import_declaration":
			e.extractImport(node, src, filePath, fileNode, result)
		case "function_definition", "function_declaration":
			// Only extract top-level functions (direct children of compilation_unit).
			if node.Parent() != nil && node.Parent().Type() == "compilation_unit" {
				e.extractTopLevelFunction(node, src, filePath, fileNode, result, seen)
			}
		case "call_expression":
			e.extractCall(node, src, filePath, result)
		}
	})
}

// extractTrait extracts a trait as KindInterface with Meta["methods"].
func (e *ScalaExtractor) extractTrait(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(node.StartPoint().Row) + 1
	endLine := int(node.EndPoint().Row) + 1

	// Collect method names from the template_body.
	var methodNames []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "template_body" {
			for j := 0; j < int(child.ChildCount()); j++ {
				member := child.Child(j)
				if member.Type() == "function_declaration" || member.Type() == "function_definition" {
					mName := scalaFindChildIdentifier(member, src)
					if mName != "" {
						methodNames = append(methodNames, mName)

						// Also emit method nodes and edges for methods inside the trait.
						mID := filePath + "::" + name + "." + mName
						mStartLine := int(member.StartPoint().Row) + 1
						mEndLine := int(member.EndPoint().Row) + 1
						if !seen[mID] {
							seen[mID] = true
							seen[filePath+"::_method_L"+fmt.Sprint(mStartLine)] = true
							result.Nodes = append(result.Nodes, &graph.Node{
								ID: mID, Kind: graph.KindMethod, Name: mName,
								FilePath: filePath, StartLine: mStartLine, EndLine: mEndLine,
								Language: "scala",
								Meta:     map[string]any{"receiver": name},
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: fileNode.ID, To: mID, Kind: graph.EdgeDefines,
								FilePath: filePath, Line: mStartLine,
							})
							result.Edges = append(result.Edges, &graph.Edge{
								From: mID, To: id, Kind: graph.EdgeMemberOf,
								FilePath: filePath, Line: mStartLine,
							})
						}
					}
				}
			}
		}
	}

	traitNode := &graph.Node{
		ID: id, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	}
	if len(methodNames) > 0 {
		traitNode.Meta = map[string]any{"methods": methodNames}
	}
	result.Nodes = append(result.Nodes, traitNode)
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractClass extracts a class (including case class) as KindType.
func (e *ScalaExtractor) extractClass(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
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
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})

	// Extract methods inside the class template_body.
	e.extractMembersFromBody(node, src, filePath, fileNode, id, name, result, seen)
}

// extractObject extracts an object as KindType.
func (e *ScalaExtractor) extractObject(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
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
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})

	// Extract methods inside the object template_body.
	e.extractMembersFromBody(node, src, filePath, fileNode, id, name, result, seen)
}

// extractMembersFromBody extracts function_definition/function_declaration nodes
// from a template_body child as methods with EdgeMemberOf.
func (e *ScalaExtractor) extractMembersFromBody(
	parent *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	ownerID, ownerName string,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	for i := 0; i < int(parent.ChildCount()); i++ {
		child := parent.Child(i)
		if child.Type() != "template_body" {
			continue
		}
		for j := 0; j < int(child.ChildCount()); j++ {
			member := child.Child(j)
			if member.Type() != "function_definition" && member.Type() != "function_declaration" {
				continue
			}
			mName := scalaFindChildIdentifier(member, src)
			if mName == "" {
				continue
			}
			mID := filePath + "::" + ownerName + "." + mName
			mStartLine := int(member.StartPoint().Row) + 1
			mEndLine := int(member.EndPoint().Row) + 1
			if seen[mID] {
				mID = filePath + "::" + ownerName + "." + mName + "_L" + fmt.Sprint(mStartLine)
			}
			if seen[mID] {
				continue
			}
			seen[mID] = true
			seen[filePath+"::_method_L"+fmt.Sprint(mStartLine)] = true
			result.Nodes = append(result.Nodes, &graph.Node{
				ID: mID, Kind: graph.KindMethod, Name: mName,
				FilePath: filePath, StartLine: mStartLine, EndLine: mEndLine,
				Language: "scala",
				Meta:     map[string]any{"receiver": ownerName},
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: fileNode.ID, To: mID, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: mStartLine,
			})
			result.Edges = append(result.Edges, &graph.Edge{
				From: mID, To: ownerID, Kind: graph.EdgeMemberOf,
				FilePath: filePath, Line: mStartLine,
			})
		}
	}
}

// extractImport extracts an import_declaration, building the path from identifier children.
func (e *ScalaExtractor) extractImport(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult,
) {
	var parts []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			parts = append(parts, child.Content(src))
		}
	}
	if len(parts) == 0 {
		return
	}
	importPath := strings.Join(parts, "/")
	startLine := int(node.StartPoint().Row) + 1
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: "unresolved::import::" + importPath,
		Kind: graph.EdgeImports, FilePath: filePath, Line: startLine,
	})
}

// extractTopLevelFunction extracts a function defined at the top level (not in a class/object/trait).
func (e *ScalaExtractor) extractTopLevelFunction(
	node *sitter.Node, src []byte, filePath string, fileNode *graph.Node,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	name := scalaFindChildIdentifier(node, src)
	if name == "" {
		return
	}
	startLine := int(node.StartPoint().Row) + 1
	lineKey := filePath + "::_method_L" + fmt.Sprint(startLine)
	if seen[lineKey] {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		id = filePath + "::" + name + "_L" + fmt.Sprint(startLine)
	}
	if seen[id] {
		return
	}
	seen[id] = true
	endLine := int(node.EndPoint().Row) + 1
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: "scala",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// extractCall extracts a call_expression.
func (e *ScalaExtractor) extractCall(
	node *sitter.Node, src []byte, filePath string,
	result *parser.ExtractionResult,
) {
	// The callee is the first child — either an identifier or a field_expression.
	if node.ChildCount() == 0 {
		return
	}
	callee := node.Child(0)
	var callName string
	switch callee.Type() {
	case "identifier":
		callName = callee.Content(src)
	case "field_expression":
		// field_expression has children: object, ".", field_name (identifier)
		// The last identifier child is the method name.
		for i := int(callee.ChildCount()) - 1; i >= 0; i-- {
			fc := callee.Child(i)
			if fc.Type() == "identifier" {
				callName = fc.Content(src)
				break
			}
		}
	default:
		return
	}
	if callName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	funcRanges := buildFuncRanges(result)
	callerID := findEnclosingFunc(funcRanges, startLine)
	if callerID == "" {
		return
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From: callerID, To: "unresolved::*." + callName,
		Kind: graph.EdgeCalls, FilePath: filePath, Line: startLine,
	})
}

// scalaFindChildIdentifier finds the first direct child of type "identifier"
// and returns its text content.
func scalaFindChildIdentifier(node *sitter.Node, src []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}
