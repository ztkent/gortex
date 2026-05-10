package forest

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// extractByTags runs the grammar's tags.scm and translates
// nvim-treesitter capture conventions into Gortex graph nodes/edges.
//
// Capture conventions (from
// https://docs.rs/tree-sitter-tags / nvim-treesitter):
//
//	@definition.{function,method,class,interface,struct,union,enum,
//	            module,constant,variable,field,macro,type,parameter}
//	@reference.{call,implementation,type}
//	@name        — the name node bound inside a @definition.* match
//
// Two passes: emit every definition first, then attribute each
// @reference.call to its enclosing function (or fall back to the
// file node when the call is module-level). Calling buildFuncRanges
// after the definitions are committed lets the second pass run a
// simple span-containment lookup without re-parsing.
func (e *Extractor) extractByTags(
	root *sitter.Node, src []byte, filePath string, fileNode *graph.Node, result *parser.ExtractionResult,
) {
	seen := make(map[string]bool)

	type pendingCall struct {
		name string
		line int
	}
	var calls []pendingCall

	parser.EachMatch(e.tagsQ, root, src, func(qr parser.QueryResult) {
		var (
			defKind   graph.NodeKind
			defNode   *parser.CapturedNode
			isRefCall bool
			refLine   int
		)
		nameCap := qr.Captures["name"]

		for capName, captured := range qr.Captures {
			switch {
			case strings.HasPrefix(capName, "definition."):
				if defNode == nil {
					defKind = mapDefinitionKind(capName)
					defNode = captured
				}
			case capName == "reference.call", capName == "reference.function":
				// Two captures cover the same intent across grammars:
				// nvim-treesitter convention is `@reference.call`,
				// but Elm and a few others use `@reference.function`
				// for call-site name references. Treat both as
				// calls; the actual identifier comes from @name.
				isRefCall = true
				refLine = captured.StartLine + 1
			}
		}

		if defNode != nil && defKind != "" && nameCap != nil {
			emitDefinition(filePath, fileNode, e.language, defKind, nameCap, defNode, seen, result)
			return
		}
		if isRefCall && nameCap != nil {
			calls = append(calls, pendingCall{name: strings.TrimSpace(nameCap.Text), line: refLine})
		}
	})

	if len(calls) == 0 {
		return
	}
	ranges := buildFuncRanges(result)
	for _, c := range calls {
		callerID := findEnclosingFunc(ranges, c.line)
		if callerID == "" {
			callerID = fileNode.ID
		}
		// Skip self-edges (e.g. recursive function calling itself).
		if strings.HasSuffix(callerID, "::"+c.name) {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}
}

// mapDefinitionKind maps an nvim-treesitter @definition.* capture
// name to a Gortex graph.NodeKind. Unknown buckets fall back to
// KindFunction (the most common signature shape).
func mapDefinitionKind(capName string) graph.NodeKind {
	switch strings.TrimPrefix(capName, "definition.") {
	case "function", "macro":
		return graph.KindFunction
	case "method":
		return graph.KindMethod
	case "class", "struct", "union", "enum", "type":
		return graph.KindType
	case "interface", "trait":
		return graph.KindInterface
	case "constant":
		return graph.KindConstant
	case "variable":
		return graph.KindVariable
	case "field":
		return graph.KindField
	case "module", "namespace":
		return graph.KindPackage
	case "parameter":
		// Skip parameters at signature-only depth — the indexer
		// would otherwise flood the graph with one node per arg.
		return ""
	default:
		return graph.KindFunction
	}
}

// emitDefinition is shared by tags.scm and walker paths. Skips empty
// names, deduplicates by ID, and emits an EdgeDefines from the file.
func emitDefinition(
	filePath string,
	fileNode *graph.Node,
	language string,
	kind graph.NodeKind,
	nameCap *parser.CapturedNode,
	bodyCap *parser.CapturedNode,
	seen map[string]bool,
	result *parser.ExtractionResult,
) {
	name := strings.TrimSpace(nameCap.Text)
	if name == "" || kind == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	startLine := bodyCap.StartLine + 1
	endLine := bodyCap.EndLine + 1
	if startLine == 0 {
		startLine = nameCap.StartLine + 1
	}
	if endLine == 0 {
		endLine = nameCap.EndLine + 1
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: kind, Name: name,
		FilePath: filePath, StartLine: startLine, EndLine: endLine,
		Language: language,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: id, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: startLine,
	})
}

