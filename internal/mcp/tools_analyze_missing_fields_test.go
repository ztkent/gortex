package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// seedMissingFieldsFixture builds a type Foo with fields {A, B, C}
// and two instantiators:
//   - good() references A, B, C — should NOT be flagged.
//   - bad()  references A only  — missing {B, C}.
//
// Field references go through EdgeReferences (function → field
// node); membership goes through EdgeMemberOf (field → type);
// instantiation goes through EdgeInstantiates (function → type).
func seedMissingFieldsFixture(t *testing.T) *Server {
	t.Helper()
	g := graph.New()

	g.AddNode(&graph.Node{ID: "pkg.Foo", Name: "Foo", Kind: graph.KindType, FilePath: "pkg/foo.go"})
	for _, name := range []string{"A", "B", "C"} {
		fid := "pkg.Foo." + name
		g.AddNode(&graph.Node{ID: fid, Name: name, Kind: graph.KindField, FilePath: "pkg/foo.go"})
		g.AddEdge(&graph.Edge{From: fid, To: "pkg.Foo", Kind: graph.EdgeMemberOf})
	}

	g.AddNode(&graph.Node{ID: "pkg.good", Name: "good", Kind: graph.KindFunction, FilePath: "pkg/good.go", StartLine: 5})
	g.AddNode(&graph.Node{ID: "pkg.bad", Name: "bad", Kind: graph.KindFunction, FilePath: "pkg/bad.go", StartLine: 10})

	g.AddEdge(&graph.Edge{From: "pkg.good", To: "pkg.Foo", Kind: graph.EdgeInstantiates})
	g.AddEdge(&graph.Edge{From: "pkg.bad", To: "pkg.Foo", Kind: graph.EdgeInstantiates})

	// good references A + B + C.
	for _, name := range []string{"A", "B", "C"} {
		g.AddEdge(&graph.Edge{From: "pkg.good", To: "pkg.Foo." + name, Kind: graph.EdgeReferences})
	}
	// bad references A only.
	g.AddEdge(&graph.Edge{From: "pkg.bad", To: "pkg.Foo.A", Kind: graph.EdgeReferences})

	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callMissingFields(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyzeConstructorsMissingFields(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestMissingFields_FlagsBadCallerOnly(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	out := callMissingFields(t, s, map[string]any{})

	sites, _ := out["sites"].([]any)
	require.Len(t, sites, 1, "good caller populates everything; only bad is flagged")
	row := sites[0].(map[string]any)
	assert.Equal(t, "pkg.bad", row["function_id"])
	assert.Equal(t, "pkg.Foo", row["type_id"])
	missing, _ := row["missing_fields"].([]any)
	got := []string{}
	for _, m := range missing {
		got = append(got, m.(string))
	}
	assert.ElementsMatch(t, []string{"B", "C"}, got)
}

func TestMissingFields_MinMissingFilter(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	out := callMissingFields(t, s, map[string]any{"min_missing": 3})
	sites, _ := out["sites"].([]any)
	assert.Empty(t, sites, "bad is missing 2 fields; min_missing=3 filters it out")
}

func TestMissingFields_TypeFilter(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	// Add a second type that's also missing fields.
	s.graph.AddNode(&graph.Node{ID: "pkg.Bar", Name: "Bar", Kind: graph.KindType, FilePath: "pkg/bar.go"})
	s.graph.AddNode(&graph.Node{ID: "pkg.Bar.X", Name: "X", Kind: graph.KindField, FilePath: "pkg/bar.go"})
	s.graph.AddEdge(&graph.Edge{From: "pkg.Bar.X", To: "pkg.Bar", Kind: graph.EdgeMemberOf})
	s.graph.AddEdge(&graph.Edge{From: "pkg.bad", To: "pkg.Bar", Kind: graph.EdgeInstantiates})

	out := callMissingFields(t, s, map[string]any{"type_id": "pkg.Foo"})
	sites, _ := out["sites"].([]any)
	for _, site := range sites {
		assert.Equal(t, "pkg.Foo", site.(map[string]any)["type_id"])
	}
}

func TestMissingFields_NullableFieldSkipped(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	// Mark field C as nullable — `bad` will only be missing B now.
	nodeC := s.graph.GetNode("pkg.Foo.C")
	require.NotNil(t, nodeC)
	nodeC.Meta = map[string]any{"nullable": true}

	out := callMissingFields(t, s, map[string]any{})
	sites, _ := out["sites"].([]any)
	require.Len(t, sites, 1)
	missing, _ := sites[0].(map[string]any)["missing_fields"].([]any)
	got := []string{}
	for _, m := range missing {
		got = append(got, m.(string))
	}
	assert.ElementsMatch(t, []string{"B"}, got, "nullable field C not flagged")
}

func TestMissingFields_OmitemptyTagSkipped(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	nodeC := s.graph.GetNode("pkg.Foo.C")
	require.NotNil(t, nodeC)
	nodeC.Meta = map[string]any{"json_tag": `json:"c,omitempty"`}

	out := callMissingFields(t, s, map[string]any{})
	sites, _ := out["sites"].([]any)
	require.Len(t, sites, 1)
	missing, _ := sites[0].(map[string]any)["missing_fields"].([]any)
	for _, m := range missing {
		assert.NotEqual(t, "C", m.(string), "omitempty field C skipped")
	}
}

func TestMissingFields_PathPrefixFilter(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	out := callMissingFields(t, s, map[string]any{"path_prefix": "vendor/"})
	sites, _ := out["sites"].([]any)
	assert.Empty(t, sites, "path_prefix excludes both the type and its instantiators")
}

func TestMissingFields_AccuracyAdmitsHeuristic(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	out := callMissingFields(t, s, map[string]any{})
	assert.Equal(t, "heuristic", out["accuracy"], "result is explicitly heuristic, not authoritative")
}

func TestMissingFields_IntegrationViaDispatch(t *testing.T) {
	s := seedMissingFieldsFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": "constructors_missing_fields"}
	res, err := s.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, "analyze kind=constructors_missing_fields must dispatch")
}

func TestIsNullableField_AllForms(t *testing.T) {
	cases := map[string]map[string]any{
		"nullable":      {"nullable": true},
		"optional":      {"optional": true},
		"omitempty":     {"json_tag": `json:"x,omitempty"`},
	}
	for name, meta := range cases {
		n := &graph.Node{Meta: meta}
		assert.True(t, isNullableField(n), name)
	}
	assert.False(t, isNullableField(&graph.Node{}), "no meta → not nullable")
	assert.False(t, isNullableField(&graph.Node{Meta: map[string]any{"nullable": false}}), "explicit false → not nullable")
}
