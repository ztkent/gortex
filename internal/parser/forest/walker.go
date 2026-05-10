package forest

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// extractByWalker is the fallback for grammars that do not ship a
// tags.scm. It walks every named node in the parse tree and matches
// kind names against a small set of suffix/prefix heuristics that
// catch the conventional tree-sitter naming pattern
// `<thing>_definition` / `<thing>_declaration` / `<thing>_specifier`.
//
// This is naive on purpose. For the long tail (~440 grammars without
// tags.scm) it produces good-enough signature-only extraction without
// hand-tuning queries per language. Languages where the heuristic
// underfits get a tags.scm contribution upstream or a bespoke
// extractor in internal/parser/languages.
func (e *Extractor) extractByWalker(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node, result *parser.ExtractionResult,
) {
	seen := make(map[string]bool)

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kind := classifyKind(n.Type()); kind != "" {
			if name := nodeName(n, src); name != "" {
				e.emitWalkerNode(filePath, fileNode, kind, name, n, seen, result)
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
}

// emitWalkerNode is the walker's adaptation of emitDefinition — it
// builds the same shape but takes raw sitter.Node positions rather
// than CapturedNode.
func (e *Extractor) emitWalkerNode(
	filePath string, fileNode *graph.Node, kind graph.NodeKind, name string,
	n *sitter.Node, seen map[string]bool, result *parser.ExtractionResult,
) {
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := int(n.StartPoint().Row) + 1
	endLine := int(n.EndPoint().Row) + 1

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: e.language,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

// classifyKind maps a tree-sitter node kind name to a graph.NodeKind
// using suffix matching on the conventional tree-sitter rule names.
// Returns "" for nodes we don't extract at signature-only depth.
//
// The order matters: longer / more specific suffixes are checked
// before generic ones. "function_declaration" beats "_declaration".
func classifyKind(t string) graph.NodeKind {
	if t == "" {
		return ""
	}

	// Methods first — `method_*` is more specific than `function_*`,
	// and a method declaration shouldn't fall through to function.
	switch {
	case hasAnySuffix(t, "method_definition", "method_declaration", "method_signature", "method_spec"):
		return graph.KindMethod
	case hasAnySuffix(t, "function_definition", "function_declaration", "function_signature", "function_spec", "function_item"):
		return graph.KindFunction
	case hasAnySuffix(t, "class_definition", "class_declaration", "class_specifier"):
		return graph.KindType
	case hasAnySuffix(t, "interface_definition", "interface_declaration"):
		return graph.KindInterface
	case hasAnySuffix(t, "trait_definition", "trait_declaration"):
		return graph.KindInterface
	case hasAnySuffix(t, "struct_definition", "struct_declaration", "struct_specifier", "struct_item"):
		return graph.KindType
	case hasAnySuffix(t, "enum_definition", "enum_declaration", "enum_specifier", "enum_item"):
		return graph.KindType
	case hasAnySuffix(t, "union_definition", "union_declaration", "union_specifier"):
		return graph.KindType
	case hasAnySuffix(t, "type_definition", "type_declaration", "type_alias_declaration", "type_alias", "type_item"):
		return graph.KindType
	case hasAnySuffix(t, "module_definition", "module_declaration", "namespace_definition", "namespace_declaration"):
		return graph.KindPackage
	case hasAnySuffix(t, "constant_declaration", "const_declaration", "const_item"):
		return graph.KindConstant
	case hasAnySuffix(t, "variable_declaration", "var_declaration"):
		return graph.KindVariable
	case hasAnySuffix(t, "field_declaration", "field_definition"):
		return graph.KindField
	case hasAnySuffix(t, "macro_definition", "macro_declaration"):
		return graph.KindFunction
	}
	return ""
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if s == suf || strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// nodeName tries the conventional `name:` field first, then falls
// back to the first identifier-like child. Returns "" if neither is
// present (anonymous functions, unnamed structs).
func nodeName(n *sitter.Node, src []byte) string {
	if name := n.ChildByFieldName("name"); name != nil {
		return strings.TrimSpace(name.Content(src))
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		t := c.Type()
		if strings.Contains(t, "identifier") || t == "name" || t == "type_identifier" {
			return strings.TrimSpace(c.Content(src))
		}
	}
	return ""
}
