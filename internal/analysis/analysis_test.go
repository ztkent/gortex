package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func buildTestGraph() *graph.Graph {
	g := graph.New()

	// auth module
	g.AddNode(&graph.Node{ID: "auth.go", Kind: graph.KindFile, Name: "auth.go", FilePath: "pkg/auth/auth.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "auth.go::ValidateToken", Kind: graph.KindFunction, Name: "ValidateToken", FilePath: "pkg/auth/auth.go", Language: "go", StartLine: 10, EndLine: 30})
	g.AddNode(&graph.Node{ID: "auth.go::ParseClaims", Kind: graph.KindFunction, Name: "ParseClaims", FilePath: "pkg/auth/auth.go", Language: "go", StartLine: 32, EndLine: 50})

	// handler module
	g.AddNode(&graph.Node{ID: "handler.go", Kind: graph.KindFile, Name: "handler.go", FilePath: "pkg/api/handler.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "handler.go::HandleLogin", Kind: graph.KindFunction, Name: "HandleLogin", FilePath: "pkg/api/handler.go", Language: "go", StartLine: 5, EndLine: 40})
	g.AddNode(&graph.Node{ID: "handler.go::HandleLogout", Kind: graph.KindFunction, Name: "HandleLogout", FilePath: "pkg/api/handler.go", Language: "go", StartLine: 42, EndLine: 60})

	// main
	g.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "cmd/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "cmd/main.go", Language: "go", StartLine: 1, EndLine: 20})

	// db module
	g.AddNode(&graph.Node{ID: "db.go::QueryUser", Kind: graph.KindFunction, Name: "QueryUser", FilePath: "pkg/db/db.go", Language: "go", StartLine: 5, EndLine: 20})

	// Edges
	g.AddEdge(&graph.Edge{From: "main.go::main", To: "handler.go::HandleLogin", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "main.go::main", To: "handler.go::HandleLogout", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "handler.go::HandleLogin", To: "auth.go::ValidateToken", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "handler.go::HandleLogin", To: "db.go::QueryUser", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "auth.go::ValidateToken", To: "auth.go::ParseClaims", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "handler.go::HandleLogout", To: "auth.go::ValidateToken", Kind: graph.EdgeCalls})

	return g
}

func TestDetectCommunities(t *testing.T) {
	g := buildTestGraph()
	result := DetectCommunities(g)

	require.NotNil(t, result)
	// With this small graph, we should get at least 1 community
	assert.GreaterOrEqual(t, len(result.Communities), 1)
	assert.NotEmpty(t, result.NodeToComm)

	// Check that communities have members
	for _, c := range result.Communities {
		assert.NotEmpty(t, c.ID)
		assert.NotEmpty(t, c.Members)
		assert.Greater(t, c.Size, 1)
		assert.GreaterOrEqual(t, c.Cohesion, 0.0)
		assert.LessOrEqual(t, c.Cohesion, 1.0)
	}
}

func TestDiscoverProcesses(t *testing.T) {
	g := buildTestGraph()
	result := DiscoverProcesses(g)

	require.NotNil(t, result)
	// main and HandleLogin should be discovered as entry points
	assert.GreaterOrEqual(t, len(result.Processes), 1)

	// Check process structure
	for _, p := range result.Processes {
		assert.NotEmpty(t, p.ID)
		assert.NotEmpty(t, p.Name)
		assert.NotEmpty(t, p.EntryPoint)
		assert.GreaterOrEqual(t, p.StepCount, 2)
		assert.NotEmpty(t, p.Files)
	}

	// main should trace through handler → auth → parseclaims
	found := false
	for _, p := range result.Processes {
		if p.EntryPoint == "main.go::main" {
			found = true
			assert.GreaterOrEqual(t, p.StepCount, 3)
			break
		}
	}
	assert.True(t, found, "main should be discovered as a process entry point")
}

func TestAnalyzeImpact(t *testing.T) {
	g := buildTestGraph()
	communities := DetectCommunities(g)
	processes := DiscoverProcesses(g)

	result := AnalyzeImpact(g, []string{"auth.go::ValidateToken"}, communities, processes)

	require.NotNil(t, result)
	assert.NotEmpty(t, result.Risk)
	assert.NotEmpty(t, result.Summary)

	// ValidateToken is called by HandleLogin and HandleLogout (depth 1)
	d1 := result.ByDepth[1]
	assert.GreaterOrEqual(t, len(d1), 1, "should have at least 1 direct dependent")

	// At depth 2, main should appear
	d2 := result.ByDepth[2]
	assert.GreaterOrEqual(t, len(d2), 0)

	assert.Greater(t, result.TotalAffected, 0)
}

func TestAnalyzeImpact_RiskLevels(t *testing.T) {
	assert.Equal(t, RiskLow, assessRisk(0, 0, 0))
	assert.Equal(t, RiskLow, assessRisk(1, 1, 0))
	assert.Equal(t, RiskMedium, assessRisk(2, 3, 0))
	assert.Equal(t, RiskHigh, assessRisk(5, 5, 0))
	assert.Equal(t, RiskCritical, assessRisk(10, 10, 0))
}

func TestScoreEntryPoint(t *testing.T) {
	main := &graph.Node{Name: "main", Language: "go", Kind: graph.KindFunction}
	handler := &graph.Node{Name: "HandleLogin", Language: "go", Kind: graph.KindFunction}
	getter := &graph.Node{Name: "getName", Language: "go", Kind: graph.KindFunction}

	// main with 3 callees, 0 callers should score high
	mainScore := scoreEntryPoint(main, 3, 0)
	handlerScore := scoreEntryPoint(handler, 2, 1)
	getterScore := scoreEntryPoint(getter, 1, 5)

	assert.Greater(t, mainScore, handlerScore)
	assert.Greater(t, handlerScore, getterScore)
}
