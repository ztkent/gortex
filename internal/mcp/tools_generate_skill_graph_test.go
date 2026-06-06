package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestBuildSkillMarkdown_RendersKeySymbols(t *testing.T) {
	syms := []skillSymbol{
		{Name: "Run", Kind: "function", Signature: "func Run() int", RelPath: "main.go", Line: 3, FanIn: 7},
		{Name: "Helper", Kind: "function", Signature: "func Helper() string", RelPath: "helpers.go", Line: 1, FanIn: 1},
	}
	body := buildSkillMarkdown("demo", "desc", []generateSkillRef{{RelPath: "main.go", BytesWritten: 10}}, syms)

	assert.Contains(t, body, "## Key symbols")
	assert.Contains(t, body, "`Run` (function)")
	assert.Contains(t, body, "func Run() int")
	assert.Contains(t, body, "`main.go:3`")
	assert.Contains(t, body, "(7 refs)")
	// References section still present.
	assert.Contains(t, body, "## References")
	// Higher fan-in ranks first.
	assert.Less(t, strings.Index(body, "`Run`"), strings.Index(body, "`Helper`"))
}

func TestBuildSkillMarkdown_NoSymbolsOmitsSection(t *testing.T) {
	body := buildSkillMarkdown("demo", "desc", []generateSkillRef{{RelPath: "x.go"}}, nil)
	assert.NotContains(t, body, "## Key symbols")
	assert.Contains(t, body, "## References")
}

func TestIsSkillSymbolKind(t *testing.T) {
	for _, k := range []string{"function", "method", "type", "interface", "constant", "variable"} {
		assert.True(t, isSkillSymbolKind(graph.NodeKind(k)), k)
	}
	for _, k := range []string{"param", "local", "import", "field", "file", "package"} {
		assert.False(t, isSkillSymbolKind(graph.NodeKind(k)), k)
	}
}

func TestDefaultSkillDescription(t *testing.T) {
	withSyms := defaultSkillDescription("demo", "internal/x", 4, 12)
	assert.Contains(t, withSyms, "12 symbols across 4 files")
	noSyms := defaultSkillDescription("demo", "internal/x", 4, 0)
	assert.NotContains(t, noSyms, "symbols across")
}

func TestTopSkillSymbols(t *testing.T) {
	syms := make([]skillSymbol, 50)
	assert.Len(t, topSkillSymbols(syms, 40), 40)
	assert.Len(t, topSkillSymbols(syms[:5], 40), 5)
}

func TestCollapseWhitespace(t *testing.T) {
	assert.Equal(t, "func F(a int) error", collapseWhitespace("func F(a\n\tint)   error"))
}

// TestGenerateSkill_GraphSymbolsSurface seeds the graph with symbols for
// the bundled files (ranked by inbound references) and proves the
// generated skill leads with a graph-derived Key symbols map.
func TestGenerateSkill_GraphSymbolsSurface(t *testing.T) {
	s := newGenerateSkillTestServer(t)
	srcDir := seedSkillSource(t)

	// File paths as the tool will look them up.
	mainFP := s.repoRelative(filepath.Join(srcDir, "main.go"))
	helpersFP := s.repoRelative(filepath.Join(srcDir, "helpers.go"))

	runID := mainFP + "::Run"
	helperID := helpersFP + "::Helper"
	s.graph.AddNode(&graph.Node{ID: runID, Kind: graph.NodeKind("function"), Name: "Run", FilePath: mainFP, StartLine: 3, Meta: map[string]any{"signature": "func Run() int"}})
	s.graph.AddNode(&graph.Node{ID: helperID, Kind: graph.NodeKind("function"), Name: "Helper", FilePath: helpersFP, StartLine: 1, Meta: map[string]any{"signature": "func Helper() string"}})
	// Give Run more inbound references than Helper so it ranks first.
	s.graph.AddNode(&graph.Node{ID: "caller-a", Kind: graph.NodeKind("function"), Name: "A", FilePath: "other.go", StartLine: 1})
	s.graph.AddNode(&graph.Node{ID: "caller-b", Kind: graph.NodeKind("function"), Name: "B", FilePath: "other.go", StartLine: 2})
	s.graph.AddEdge(&graph.Edge{From: "caller-a", To: runID, Kind: graph.EdgeReturns, FilePath: "other.go", Line: 1})
	s.graph.AddEdge(&graph.Edge{From: "caller-b", To: runID, Kind: graph.EdgeReturns, FilePath: "other.go", Line: 2})
	s.graph.AddEdge(&graph.Edge{From: "caller-a", To: helperID, Kind: graph.EdgeReturns, FilePath: "other.go", Line: 1})

	out := callGenerateSkill(t, s, map[string]any{
		"directory":  srcDir,
		"output_dir": t.TempDir(),
	})

	require.EqualValues(t, 2, out["symbol_count"], "both seeded symbols should be surfaced")

	body, err := os.ReadFile(out["skill_path"].(string))
	require.NoError(t, err)
	md := string(body)
	assert.Contains(t, md, "## Key symbols")
	assert.Contains(t, md, "`Run` (function)")
	assert.Contains(t, md, "func Run() int")
	assert.Contains(t, md, "`main.go:3`")
	// The auto description quotes the graph counts.
	assert.Contains(t, out["description"].(string), "symbols across")
	// Run (fan-in 2) ranks above Helper (fan-in 1).
	assert.Less(t, strings.Index(md, "`Run`"), strings.Index(md, "`Helper`"))
}
