package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/bash"
)

const (
	bashQFunction = `(function_definition
		name: (word) @func.name) @func.def`

	bashQVariable = `(variable_assignment
		name: (variable_name) @var.name) @var.def`

	bashQCommand = `(command
		name: (command_name) @cmd.name) @cmd.expr`
)

// BashExtractor extracts Bash/Shell source files. Three small queries
// with an inter-pass ordering constraint (funcRanges must be built
// before command attribution), so we precompile each query at init
// but don't merge into an alternation.
type BashExtractor struct {
	lang      *sitter.Language
	qFunction *parser.PreparedQuery
	qVariable *parser.PreparedQuery
	qCommand  *parser.PreparedQuery
}

func NewBashExtractor() *BashExtractor {
	lang := bash.GetLanguage()
	return &BashExtractor{
		lang:      lang,
		qFunction: parser.MustPreparedQuery(bashQFunction, lang),
		qVariable: parser.MustPreparedQuery(bashQVariable, lang),
		qCommand:  parser.MustPreparedQuery(bashQCommand, lang),
	}
}

func (e *BashExtractor) Language() string     { return "bash" }
func (e *BashExtractor) Extensions() []string { return []string{".sh", ".bash", ".zsh"} }

func (e *BashExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
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
		Language: "bash",
	}
	result.Nodes = append(result.Nodes, fileNode)

	seen := make(map[string]bool)

	// Functions.
	parser.EachMatch(e.qFunction, root, src, func(m parser.QueryResult) {
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
			Language: "bash", Meta: map[string]any{"signature": name + "()"},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	})

	// Top-level variable assignments.
	parser.EachMatch(e.qVariable, root, src, func(m parser.QueryResult) {
		name := m.Captures["var.name"].Text
		def := m.Captures["var.def"]
		// Only top-level: parent is program.
		if def.Node == nil || def.Node.Parent() == nil || def.Node.Parent().Type() != "program" {
			return
		}
		id := filePath + "::" + name
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: name,
			FilePath: filePath, StartLine: def.StartLine + 1, EndLine: def.EndLine + 1,
			Language: "bash",
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileNode.ID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: def.StartLine + 1,
		})
	})

	// Command calls — extract source/dot imports and general call sites.
	// funcRanges depends on the function pass above.
	funcRanges := buildFuncRanges(result)

	parser.EachMatch(e.qCommand, root, src, func(m parser.QueryResult) {
		cmdName := m.Captures["cmd.name"].Text
		expr := m.Captures["cmd.expr"]

		// Source/dot imports: `source foo.sh` or `. foo.sh`
		if cmdName == "source" || cmdName == "." {
			// The argument is the second child of the command node.
			cmdNode := expr.Node
			if cmdNode != nil && cmdNode.NamedChildCount() >= 2 {
				arg := cmdNode.NamedChild(1)
				if arg != nil {
					importPath := arg.Content(src)
					// Strip quotes if present.
					importPath = strings.Trim(importPath, "\"'")
					result.Edges = append(result.Edges, &graph.Edge{
						From: fileNode.ID, To: "unresolved::import::" + importPath,
						Kind: graph.EdgeImports, FilePath: filePath, Line: expr.StartLine + 1,
					})
				}
			}
			return
		}

		// Regular command call.
		callerID := findEnclosingFunc(funcRanges, expr.StartLine+1)
		if callerID == "" {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: callerID, To: "unresolved::" + cmdName,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: expr.StartLine + 1,
		})
	})

	return result, nil
}
