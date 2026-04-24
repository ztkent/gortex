package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/c"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// qCAll is a single tree-sitter query alternating over every pattern
// the C extractor needs. One tree walk per file replaces the 8
// `parser.RunQuery` calls the previous design made (each of which
// recompiled its query and ran an independent cursor over the whole
// tree). Capture names are disjoint across patterns so the dispatch in
// Extract can branch on which name is set.
const qCAll = `
[
  (function_definition
    declarator: (function_declarator
      declarator: (identifier) @func.name)) @func.def

  (declaration
    declarator: (function_declarator
      declarator: (identifier) @proto.name)) @proto.def

  (struct_specifier
    name: (type_identifier) @struct.name) @struct.def

  (enum_specifier
    name: (type_identifier) @enum.name) @enum.def

  (type_definition
    declarator: (type_identifier) @typedef.name) @typedef.def

  (preproc_include
    path: (_) @include.path) @include.def

  (call_expression
    function: (identifier) @call.name) @call.expr

  (declaration
    declarator: (init_declarator
      declarator: (identifier) @var.name)) @var.def
]
`

// CExtractor extracts C source files into graph nodes and edges.
type CExtractor struct {
	lang *sitter.Language
	qAll *parser.PreparedQuery
}

func NewCExtractor() *CExtractor {
	lang := c.GetLanguage()
	return &CExtractor{
		lang: lang,
		qAll: parser.MustPreparedQuery(qCAll, lang),
	}
}

func (e *CExtractor) Language() string     { return "c" }
func (e *CExtractor) Extensions() []string { return []string{".c", ".h"} }

// --- Deferred match buffers ----------------------------------------

type cDeferredCall struct {
	name string
	line int
}

type cDeferredVar struct {
	name    string
	line    int
	endLine int
}

func (e *CExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "c",
	}
	fileID := fileNode.ID
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)
	var calls []cDeferredCall
	var vars []cDeferredVar

	parser.EachMatch(e.qAll, root, src, func(m parser.QueryResult) {
		switch {

		case m.Captures["func.def"] != nil:
			e.emitFunction(m, filePath, fileID, src, result, seen)

		case m.Captures["proto.def"] != nil:
			e.emitPrototype(m, filePath, fileID, result, seen)

		case m.Captures["struct.def"] != nil:
			e.emitKindType(m, "struct", filePath, fileID, result, seen)

		case m.Captures["enum.def"] != nil:
			e.emitKindType(m, "enum", filePath, fileID, result, seen)

		case m.Captures["typedef.def"] != nil:
			e.emitKindType(m, "typedef", filePath, fileID, result, seen)

		case m.Captures["include.def"] != nil:
			e.emitInclude(m, filePath, fileID, result)

		case m.Captures["call.expr"] != nil:
			expr := m.Captures["call.expr"]
			calls = append(calls, cDeferredCall{
				name: m.Captures["call.name"].Text,
				line: expr.StartLine + 1,
			})

		case m.Captures["var.def"] != nil:
			def := m.Captures["var.def"]
			vars = append(vars, cDeferredVar{
				name:    m.Captures["var.name"].Text,
				line:    def.StartLine + 1,
				endLine: def.EndLine + 1,
			})
		}
	})

	// Globals and call edges both need funcRanges; build once.
	funcRanges := buildFuncRanges(result)

	for _, v := range vars {
		// Skip locals inside function bodies.
		if findEnclosingFunc(funcRanges, v.line) != "" {
			continue
		}
		id := filePath + "::" + v.name
		if seen[id] {
			continue
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: v.name,
			FilePath: filePath, StartLine: v.line, EndLine: v.endLine,
			Language: "c",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: v.line,
		})
	}

	for _, c := range calls {
		callerID := findEnclosingFunc(funcRanges, c.line)
		if callerID == "" {
			continue
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + c.name,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: c.line,
		})
	}

	return result, nil
}

// --- Per-match emit helpers -----------------------------------------

func (e *CExtractor) emitFunction(m parser.QueryResult, filePath, fileID string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["func.name"].Text
	def := m.Captures["func.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c", Meta: map[string]any{
			"signature": strings.TrimSpace(extractCSignature(def.Node, src)),
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CExtractor) emitPrototype(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures["proto.name"].Text
	def := m.Captures["proto.def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c", Meta: map[string]any{
			"signature": strings.TrimSuffix(strings.TrimSpace(def.Text), ";"),
			"prototype": true,
		},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

// emitKindType collapses struct/enum/typedef emission — all produce a
// KindType node with the same shape. The prefix selects the
// capture-name pair.
func (e *CExtractor) emitKindType(m parser.QueryResult, prefix, filePath, fileID string, result *parser.ExtractionResult, seen map[string]bool) {
	name := m.Captures[prefix+".name"].Text
	def := m.Captures[prefix+".def"]
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindType, Name: name,
		FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
		Language: "c",
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
	})
}

func (e *CExtractor) emitInclude(m parser.QueryResult, filePath, fileID string, result *parser.ExtractionResult) {
	pathCap := m.Captures["include.path"]
	// Strip quotes or angle brackets: "foo.h" -> foo.h, <stdio.h> -> stdio.h
	includePath := pathCap.Text
	includePath = strings.Trim(includePath, `"`)
	includePath = strings.Trim(includePath, "<>")
	result.Edges = append(result.Edges, &graph.Edge{
		From:     fileID,
		To:       "unresolved::import::" + includePath,
		Kind:     graph.EdgeImports,
		FilePath: filePath,
		Line:     pathCap.StartLine + 1,
	})
}

// --- Helpers --------------------------------------------------------

// extractCSignature extracts a function signature from its definition node.
// It takes the text up to (but not including) the compound_statement body.
func extractCSignature(node *sitter.Node, src []byte) string {
	fullText := node.Content(src)
	// Find the opening brace of the function body and trim there.
	idx := strings.Index(fullText, "{")
	if idx > 0 {
		return strings.TrimSpace(fullText[:idx])
	}
	return fullText
}
