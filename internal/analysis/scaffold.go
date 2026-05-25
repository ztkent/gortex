package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// SourceReader provides access to symbol source code for scaffolding.
// ResolveFilePath maps a graph file path (repo-prefixed in multi-repo mode,
// repo-relative in single-repo mode) to an absolute filesystem path. It
// returns "" when the path can't be anchored to any indexed repo, which
// callers must surface as an error rather than silently opening a file
// relative to the daemon's process CWD.
//
// This interface avoids a circular dependency with the indexer package.
type SourceReader interface {
	Graph() graph.Store
	ResolveFilePath(graphPath string) string
}

// ScaffoldEdit describes a single code insertion for scaffolding.
type ScaffoldEdit struct {
	FilePath      string `json:"file_path"`
	InsertionLine int    `json:"insertion_line"`
	Code          string `json:"code"`
	Reason        string `json:"reason"` // "new_symbol" | "registration" | "test_stub"
}

// ScaffoldResult is the output of code scaffolding generation.
type ScaffoldResult struct {
	Edits []ScaffoldEdit `json:"edits"`
	Notes []string       `json:"notes,omitempty"`
}

// GenerateScaffold generates code scaffolding based on an existing symbol pattern.
// It reads the example's source, substitutes the new name, generates registration
// code from depth-1 callers, and creates a test stub if a test pattern exists.
func GenerateScaffold(engine *query.Engine, reader SourceReader, exampleID, newName string) (*ScaffoldResult, error) {
	g := reader.Graph()

	// Look up the example symbol
	node := g.GetNode(exampleID)
	if node == nil {
		return nil, fmt.Errorf("symbol not found: %s", exampleID)
	}

	if node.StartLine <= 0 || node.EndLine <= 0 {
		return nil, fmt.Errorf("symbol has no line range: %s", exampleID)
	}

	// Step 2: Read the example's source code from disk. Resolve via the
	// SourceReader so multi-repo paths anchor to the correct repo root
	// rather than the daemon process CWD.
	absPath := reader.ResolveFilePath(node.FilePath)
	if absPath == "" {
		return nil, fmt.Errorf("could not resolve abs path for %s", node.FilePath)
	}
	source, err := readSourceLines(absPath, node.StartLine, node.EndLine)
	if err != nil {
		return nil, fmt.Errorf("failed to read source for %s: %w", exampleID, err)
	}

	result := &ScaffoldResult{}

	// Perform name substitution
	substituted := substituteSymbolName(source, node.Name, newName)

	// Add the new symbol edit — insert after the example symbol
	result.Edits = append(result.Edits, ScaffoldEdit{
		FilePath:      node.FilePath,
		InsertionLine: node.EndLine + 1,
		Code:          substituted,
		Reason:        "new_symbol",
	})

	// Find callers at depth 1 for registration code
	callerSG := engine.GetCallers(exampleID, query.QueryOptions{Depth: 1, Limit: 50})
	callerNodes := filterCallerNodes(callerSG, exampleID)

	if len(callerNodes) > 0 {
		regEdit := generateRegistrationCode(g, callerNodes, node, newName)
		if regEdit != nil {
			result.Edits = append(result.Edits, *regEdit)
		}
	} else {
		result.Notes = append(result.Notes, "no callers found at depth 1; registration/wiring code omitted")
	}

	// Look for test pattern
	testEdit := generateTestStub(g, reader, node, newName)
	if testEdit != nil {
		result.Edits = append(result.Edits, *testEdit)
	} else {
		result.Notes = append(result.Notes, "no test pattern found for example symbol; test stub omitted")
	}

	return result, nil
}

// readSourceLines reads lines [startLine, endLine] (1-indexed) from an
// absolute file path.
func readSourceLines(absPath string, startLine, endLine int) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > len(lines) {
		return "", fmt.Errorf("start line %d exceeds file length %d", startLine, len(lines))
	}

	selected := lines[startLine-1 : endLine]
	return strings.Join(selected, "\n"), nil
}

// substituteSymbolName replaces whole-word occurrences of oldName with newName in source code.
// Uses word-boundary matching to avoid corrupting identifiers that contain oldName
// as a substring (e.g., replacing "Extract" should not affect "GoExtractor").
func substituteSymbolName(source, oldName, newName string) string {
	pattern := `\b` + regexp.QuoteMeta(oldName) + `\b`
	re := regexp.MustCompile(pattern)
	return re.ReplaceAllString(source, newName)
}

// filterCallerNodes returns caller nodes from a SubGraph, excluding the example itself.
func filterCallerNodes(sg *query.SubGraph, exampleID string) []*graph.Node {
	var callers []*graph.Node
	for _, n := range sg.Nodes {
		if n.ID == exampleID {
			continue
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		callers = append(callers, n)
	}
	return callers
}

// generateRegistrationCode creates a registration/wiring edit by analyzing how
// the example symbol is called by its depth-1 callers.
func generateRegistrationCode(g graph.Store, callers []*graph.Node, example *graph.Node, newName string) *ScaffoldEdit {
	if len(callers) == 0 {
		return nil
	}

	// Use the first caller as the registration pattern.
	// Read the caller's call edges to find how it references the example.
	caller := callers[0]

	// Find the specific call edge from caller to example
	var callLine int
	outEdges := g.GetOutEdges(caller.ID)
	for _, e := range outEdges {
		if e.To == example.ID && e.Kind == graph.EdgeCalls {
			callLine = e.Line
			break
		}
	}

	// Generate a registration line that mirrors the call pattern
	regCode := fmt.Sprintf("// TODO: Register %s following the pattern of %s\n// See %s at line %d for the registration pattern",
		newName, example.Name, caller.FilePath, callLine)

	insertionLine := caller.EndLine
	if callLine > 0 {
		insertionLine = callLine + 1
	}

	return &ScaffoldEdit{
		FilePath:      caller.FilePath,
		InsertionLine: insertionLine,
		Code:          regCode,
		Reason:        "registration",
	}
}

// generateTestStub creates a test stub edit by finding the test file and test
// functions associated with the example symbol.
func generateTestStub(g graph.Store, reader SourceReader, example *graph.Node, newName string) *ScaffoldEdit {
	testFilePath := deriveTestFilePath(example.FilePath)

	// Check if the test file exists on disk. Resolve abs path through
	// the reader so multi-repo paths anchor to the correct root.
	absTestPath := reader.ResolveFilePath(testFilePath)
	if absTestPath == "" {
		return nil
	}
	if _, err := os.Stat(absTestPath); err != nil {
		return nil
	}

	// Look for test functions in the test file that reference the example
	testNodes := g.GetFileNodes(testFilePath)
	var testExample *graph.Node
	for _, n := range testNodes {
		if n.Kind != graph.KindFunction {
			continue
		}
		// Match test functions that contain the example's name
		if strings.Contains(n.Name, example.Name) {
			testExample = n
			break
		}
	}

	if testExample == nil {
		return nil
	}

	// Read the test function source and substitute the name
	testSource, err := readSourceLines(absTestPath, testExample.StartLine, testExample.EndLine)
	if err != nil {
		return nil
	}

	substituted := substituteSymbolName(testSource, example.Name, newName)

	return &ScaffoldEdit{
		FilePath:      testFilePath,
		InsertionLine: testExample.EndLine + 1,
		Code:          substituted,
		Reason:        "test_stub",
	}
}

// deriveTestFilePath converts a source file path to its corresponding test file path.
// e.g., "internal/analysis/foo.go" → "internal/analysis/foo_test.go"
func deriveTestFilePath(filePath string) string {
	ext := filepath.Ext(filePath)
	base := strings.TrimSuffix(filePath, ext)

	switch ext {
	case ".go":
		return base + "_test.go"
	case ".ts":
		return base + ".test.ts"
	case ".js":
		return base + ".test.js"
	case ".py":
		dir := filepath.Dir(filePath)
		name := filepath.Base(filePath)
		return filepath.Join(dir, "test_"+name)
	default:
		return base + "_test" + ext
	}
}
