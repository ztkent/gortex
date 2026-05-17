package mcp

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// callAnalyzeHealth is the shared harness: build a request with the
// given args, invoke the analyze handler, decode the JSON. Any tool-
// level error fails the test.
func callAnalyzeHealth(t *testing.T, srv *Server, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"kind": "health_score"}
	for k, v := range extra {
		args[k] = v
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "analyze health_score must not error: %+v", res.Content)
	text := res.Content[0].(mcplib.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "json: %s", text)
	return out
}

// addHealthFn drops one function node into the graph with the given
// id/file. Avoids re-using `addFn` from tools_analyze_concurrency_test.go
// to keep this test file self-contained.
func addHealthFn(g *graph.Graph, id, file string, meta map[string]any) *graph.Node {
	n := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: id,
		FilePath: file, StartLine: 1, EndLine: 5,
		Meta: meta,
	}
	g.AddNode(n)
	return n
}

func TestAnalyzeDispatcher_RoutesHealthScore(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "health_score"}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "dispatcher must route kind=health_score; got %v", res)
}

func TestAnalyzeHealthScore_CoverageOnly_ScoresCorrectly(t *testing.T) {
	srv, _ := setupTestServer(t)
	// 100% covered leaf function → coverage 100, complexity 100,
	// no recency / churn → composite weighted only over the two.
	addHealthFn(srv.graph, "lib.go::Full", "lib.go", map[string]any{
		"coverage_pct": 100.0,
	})
	// 0% covered → coverage 0, complexity 100.
	addHealthFn(srv.graph, "lib.go::Empty", "lib.go", map[string]any{
		"coverage_pct": 0.0,
	})

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	require.GreaterOrEqual(t, len(symbols), 2, "expected at least 2 scored symbols")

	byID := map[string]map[string]any{}
	for _, s := range symbols {
		row := s.(map[string]any)
		byID[row["id"].(string)] = row
	}
	require.Contains(t, byID, "lib.go::Full")
	require.Contains(t, byID, "lib.go::Empty")

	// Composite for "Full" should be much higher than for "Empty".
	full := byID["lib.go::Full"]["score"].(float64)
	empty := byID["lib.go::Empty"]["score"].(float64)
	assert.Greater(t, full, empty, "100%% covered must outscore 0%% covered")
	assert.Equal(t, "A", byID["lib.go::Full"]["grade"], "100%% covered leaf → A grade")
	assert.Equal(t, 2, int(byID["lib.go::Full"]["axes_used"].(float64)),
		"coverage + complexity axes should fire")
}

func TestAnalyzeHealthScore_StaleCodeScoresWorse(t *testing.T) {
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	fresh := now - int64((10 * 24 * time.Hour).Seconds())                  // 10 days
	stale := now - int64((time.Duration(800*24) * time.Hour).Seconds())    // 800 days
	dead := now - int64((time.Duration(1500*24) * time.Hour).Seconds())    // 1500 days

	addHealthFn(srv.graph, "lib.go::Fresh", "lib.go", map[string]any{
		"last_authored": map[string]any{"timestamp": fresh, "email": "x@y", "commit": "abc"},
	})
	addHealthFn(srv.graph, "lib.go::Stale", "lib.go", map[string]any{
		"last_authored": map[string]any{"timestamp": stale, "email": "x@y", "commit": "abc"},
	})
	addHealthFn(srv.graph, "lib.go::Dead", "lib.go", map[string]any{
		"last_authored": map[string]any{"timestamp": dead, "email": "x@y", "commit": "abc"},
	})

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	byID := map[string]map[string]any{}
	for _, s := range symbols {
		row := s.(map[string]any)
		byID[row["id"].(string)] = row
	}
	require.Contains(t, byID, "lib.go::Fresh")
	require.Contains(t, byID, "lib.go::Stale")
	require.Contains(t, byID, "lib.go::Dead")

	freshScore := byID["lib.go::Fresh"]["score"].(float64)
	staleScore := byID["lib.go::Stale"]["score"].(float64)
	deadScore := byID["lib.go::Dead"]["score"].(float64)

	assert.Greater(t, freshScore, staleScore, "fresh code must outscore stale")
	assert.Greater(t, staleScore, deadScore, "stale code must outscore dead")
}

func TestAnalyzeHealthScore_ChurnDecaysScore(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Quiet", "lib.go", nil)
	addHealthFn(srv.graph, "lib.go::Churned", "lib.go", nil)

	// Quiet → no session mods. Churned → 5 mods.
	for range 5 {
		srv.symHistory.Record("lib.go::Churned", false)
	}

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	byID := map[string]map[string]any{}
	for _, s := range symbols {
		row := s.(map[string]any)
		byID[row["id"].(string)] = row
	}

	quiet := byID["lib.go::Quiet"]["score"].(float64)
	churned := byID["lib.go::Churned"]["score"].(float64)
	assert.Greater(t, quiet, churned, "low churn must outscore high churn")
	assert.Equal(t, 5, int(byID["lib.go::Churned"]["session_mods"].(float64)))
}

func TestAnalyzeHealthScore_ComplexityFromFanInOut(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Hub gets 20 incoming calls from spokes.
	addHealthFn(srv.graph, "lib.go::Hub", "lib.go", nil)
	for i := 0; i < 20; i++ {
		spoke := "lib.go::Spoke" + string(rune('A'+i%26))
		addHealthFn(srv.graph, spoke, "lib.go", nil)
		srv.graph.AddEdge(&graph.Edge{
			From: spoke, To: "lib.go::Hub",
			Kind: graph.EdgeCalls, FilePath: "lib.go", Line: 1,
		})
	}

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	var hub map[string]any
	for _, s := range symbols {
		row := s.(map[string]any)
		if row["id"] == "lib.go::Hub" {
			hub = row
			break
		}
	}
	require.NotNil(t, hub, "Hub must appear in the result")
	assert.Equal(t, 20, int(hub["fan_in"].(float64)))
	// High fan-in → complexity_pct drops below 100.
	complexity := hub["complexity_pct"].(float64)
	assert.Less(t, complexity, 100.0, "fan_in=20 must drop complexity health below 100")
}

func TestAnalyzeHealthScore_MissingAxesSkipped(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Bare", "lib.go", nil)

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	var bare map[string]any
	for _, s := range symbols {
		row := s.(map[string]any)
		if row["id"] == "lib.go::Bare" {
			bare = row
			break
		}
	}
	require.NotNil(t, bare)
	// Only complexity axis populated (always-on); coverage/recency/
	// churn are absent.
	assert.Equal(t, 1, int(bare["axes_used"].(float64)))
	_, hasCov := bare["coverage_pct"]
	_, hasRec := bare["recency_pct"]
	_, hasChurn := bare["churn_pct"]
	assert.False(t, hasCov)
	assert.False(t, hasRec)
	assert.False(t, hasChurn)
}

func TestAnalyzeHealthScore_MinAxesFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Bare", "lib.go", nil)
	addHealthFn(srv.graph, "lib.go::Covered", "lib.go", map[string]any{
		"coverage_pct": 80.0,
	})

	// min_axes=2 drops Bare (only complexity), keeps Covered.
	out := callAnalyzeHealth(t, srv, map[string]any{"min_axes": 2.0})
	symbols := out["symbols"].([]any)
	ids := map[string]bool{}
	for _, s := range symbols {
		ids[s.(map[string]any)["id"].(string)] = true
	}
	assert.False(t, ids["lib.go::Bare"], "min_axes=2 must drop Bare")
	assert.True(t, ids["lib.go::Covered"], "Covered (2 axes) must remain")
}

func TestAnalyzeHealthScore_GradeFilter(t *testing.T) {
	srv, _ := setupTestServer(t)
	// A grade: high coverage, no callers.
	addHealthFn(srv.graph, "lib.go::A", "lib.go", map[string]any{
		"coverage_pct": 95.0,
	})
	// F grade: 0% coverage + heavy fan-in to drop the complexity
	// axis below A/B bands. raw = fan_in*2 = 60 → complexity = 25.
	// composite = (0*3 + 25*2) / 5 = 10 → F.
	addHealthFn(srv.graph, "lib.go::F", "lib.go", map[string]any{
		"coverage_pct": 0.0,
	})
	for i := 0; i < 30; i++ {
		spoke := "lib.go::FSpoke" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		addHealthFn(srv.graph, spoke, "lib.go", nil)
		srv.graph.AddEdge(&graph.Edge{
			From: spoke, To: "lib.go::F",
			Kind: graph.EdgeCalls, FilePath: "lib.go", Line: 1,
		})
	}

	out := callAnalyzeHealth(t, srv, map[string]any{"grade": "F"})
	symbols := out["symbols"].([]any)
	require.GreaterOrEqual(t, len(symbols), 1, "expected at least one F-graded symbol")
	for _, s := range symbols {
		row := s.(map[string]any)
		assert.Equal(t, "F", row["grade"], "grade=F filter must keep only F rows; got %v", row["grade"])
	}
}

func TestAnalyzeHealthScore_SortedWorstFirst(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::High", "lib.go", map[string]any{
		"coverage_pct": 95.0,
	})
	addHealthFn(srv.graph, "lib.go::Mid", "lib.go", map[string]any{
		"coverage_pct": 50.0,
	})
	addHealthFn(srv.graph, "lib.go::Low", "lib.go", map[string]any{
		"coverage_pct": 5.0,
	})

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	// First three (or however many of ours land first) should be in
	// ascending order — worst first.
	require.GreaterOrEqual(t, len(symbols), 3)
	prev := -1.0
	for _, s := range symbols {
		score := s.(map[string]any)["score"].(float64)
		if prev >= 0 {
			assert.GreaterOrEqual(t, score, prev,
				"rows must be in ascending score order; got %v after %v", score, prev)
		}
		prev = score
	}
}

func TestAnalyzeHealthScore_PathPrefixScopes(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "keep/a.go::Keep", "keep/a.go", map[string]any{
		"coverage_pct": 50.0,
	})
	addHealthFn(srv.graph, "drop/a.go::Drop", "drop/a.go", map[string]any{
		"coverage_pct": 50.0,
	})

	out := callAnalyzeHealth(t, srv, map[string]any{"path_prefix": "keep/"})
	symbols := out["symbols"].([]any)
	for _, s := range symbols {
		row := s.(map[string]any)
		assert.True(t, len(row["file"].(string)) >= 5 && row["file"].(string)[:5] == "keep/",
			"path_prefix=keep/ must drop other paths; got %v", row["file"])
	}
}

func TestAnalyzeHealthScore_LimitTruncates(t *testing.T) {
	srv, _ := setupTestServer(t)
	for i := 0; i < 5; i++ {
		id := "lib.go::Fn" + string(rune('A'+i))
		addHealthFn(srv.graph, id, "lib.go", map[string]any{
			"coverage_pct": float64(i * 20),
		})
	}

	out := callAnalyzeHealth(t, srv, map[string]any{"limit": 2.0})
	total, _ := out["total"].(float64)
	assert.GreaterOrEqual(t, int(total), 5, "total must report pre-truncation count")
	truncated, _ := out["truncated"].(bool)
	assert.True(t, truncated)
	symbols := out["symbols"].([]any)
	assert.Equal(t, 2, len(symbols))
}

func TestAnalyzeHealthScore_GCXEncodesHeader(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Sample", "lib.go", map[string]any{
		"coverage_pct": 75.0,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":   "health_score",
		"format": "gcx",
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "analyze.health_score")
	for _, col := range []string{"score", "grade", "coverage_pct", "complexity_pct", "recency_pct", "churn_pct", "fan_in", "fan_out", "axes_used"} {
		assert.Contains(t, text, col, "GCX header must list column %q", col)
	}
}

func TestAnalyzeHealthScore_CompactOutput(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Sample", "lib.go", map[string]any{
		"coverage_pct": 30.0,
	})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":    "health_score",
		"compact": true,
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "lib.go::Sample")
}

func TestAnalyzeHealthScore_RecencyCurve(t *testing.T) {
	// Direct unit test on the recency curve so future tweaks have
	// a fixed reference. The piecewise shape:
	//   0..30 → 100
	//   30..365 → 100 → 50
	//   365..1095 → 50 → 0
	//   >1095 → 0
	cases := []struct {
		ageDays int
		want    float64
	}{
		{0, 100},
		{30, 100},
		{197, 75},      // halfway through the OK band
		{365, 50},
		{730, 25},      // halfway through the stale band
		{1095, 0},
		{5000, 0},
	}
	for _, c := range cases {
		got := recencyScore(c.ageDays)
		assert.InDelta(t, c.want, got, 0.5,
			"recencyScore(%d) = %.2f, want ~%.2f", c.ageDays, got, c.want)
	}
}

func TestAnalyzeHealthScore_GradeBands(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{100, "A"},
		{85.01, "A"},
		{84.99, "B"},
		{70, "B"},
		{55, "C"},
		{40, "D"},
		{39.99, "F"},
		{0, "F"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, scoreGrade(c.score),
			"scoreGrade(%v) = %v, want %v", c.score, scoreGrade(c.score), c.want)
	}
}

func TestAnalyzeHealthScore_BlameTimestampAcceptsFloat(t *testing.T) {
	// Snapshot round-trips land int64 as float64; the analyzer
	// must accept both shapes.
	srv, _ := setupTestServer(t)
	now := time.Now().Unix()
	addHealthFn(srv.graph, "lib.go::Int", "lib.go", map[string]any{
		"last_authored": map[string]any{"timestamp": now},
	})
	addHealthFn(srv.graph, "lib.go::Float", "lib.go", map[string]any{
		"last_authored": map[string]any{"timestamp": float64(now)},
	})

	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	byID := map[string]map[string]any{}
	for _, s := range symbols {
		row := s.(map[string]any)
		byID[row["id"].(string)] = row
	}
	require.Contains(t, byID, "lib.go::Int")
	require.Contains(t, byID, "lib.go::Float")
	// Both should have populated recency axis.
	_, intHasRec := byID["lib.go::Int"]["recency_pct"]
	_, floatHasRec := byID["lib.go::Float"]["recency_pct"]
	assert.True(t, intHasRec, "int64 timestamp must populate recency axis")
	assert.True(t, floatHasRec, "float64 timestamp must populate recency axis")
}

func TestAnalyzeHealthScore_KindsFilterRespected(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::Fn", "lib.go", map[string]any{
		"coverage_pct": 50.0,
	})
	// Add a method node.
	srv.graph.AddNode(&graph.Node{
		ID: "lib.go::T.M", Kind: graph.KindMethod, Name: "M",
		FilePath: "lib.go", StartLine: 5, EndLine: 10,
		Meta: map[string]any{"coverage_pct": 50.0},
	})
	// Add a type node — should be excluded by default.
	srv.graph.AddNode(&graph.Node{
		ID: "lib.go::T", Kind: graph.KindType, Name: "T",
		FilePath: "lib.go", StartLine: 1, EndLine: 4,
		Meta: map[string]any{"coverage_pct": 99.0},
	})

	// Default kinds = function,method → type T excluded.
	out := callAnalyzeHealth(t, srv, nil)
	symbols := out["symbols"].([]any)
	for _, s := range symbols {
		row := s.(map[string]any)
		assert.NotEqual(t, "lib.go::T", row["id"], "type T must be excluded by default")
	}

	// kinds=type → only T survives.
	outType := callAnalyzeHealth(t, srv, map[string]any{"kinds": "type"})
	ids := map[string]bool{}
	for _, s := range outType["symbols"].([]any) {
		ids[s.(map[string]any)["id"].(string)] = true
	}
	assert.True(t, ids["lib.go::T"], "kinds=type must keep T")
	assert.False(t, ids["lib.go::Fn"], "kinds=type must drop the function")
}

// recencyScore sanity: regressions on the boundary days would change
// the broader test fixtures; pin them explicitly so a tweak fails
// loudly.
func TestRecencyScore_NaNNotProduced(t *testing.T) {
	for d := 0; d <= 2000; d += 50 {
		got := recencyScore(d)
		assert.False(t, math.IsNaN(got), "recencyScore(%d) returned NaN", d)
	}
}

func TestAnalyzeHealthScore_DistributionAlwaysPresent(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "lib.go::A", "lib.go", map[string]any{"coverage_pct": 95.0})
	addHealthFn(srv.graph, "lib.go::B", "lib.go", map[string]any{"coverage_pct": 50.0})
	addHealthFn(srv.graph, "lib.go::C", "lib.go", map[string]any{"coverage_pct": 10.0})

	out := callAnalyzeHealth(t, srv, nil)
	dist, ok := out["distribution"].(map[string]any)
	require.True(t, ok, "distribution block must always ride on the response")
	for _, key := range []string{"mean", "median", "std_dev", "gini", "grade_counts", "total"} {
		_, has := dist[key]
		assert.True(t, has, "distribution.%s missing", key)
	}
	// total is the row count BEFORE truncation.
	total, _ := dist["total"].(float64)
	assert.GreaterOrEqual(t, int(total), 3)
}

func TestGiniCoefficient_BoundaryCases(t *testing.T) {
	// Empty / all-zero — both produce 0.
	assert.Equal(t, 0.0, giniCoefficient(nil))
	assert.Equal(t, 0.0, giniCoefficient([]float64{0, 0, 0}))
	// Perfect equality — every value identical.
	assert.InDelta(t, 0.0, giniCoefficient([]float64{50, 50, 50, 50}), 1e-9)
	// Maximal inequality — one value owns the entire mass.
	g := giniCoefficient([]float64{0, 0, 0, 100})
	assert.Greater(t, g, 0.5, "highly concentrated distribution must produce a high Gini")
}

func TestAnalyzeHealthScore_RollupFile(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "a.go::Fn1", "a.go", map[string]any{"coverage_pct": 90.0})
	addHealthFn(srv.graph, "a.go::Fn2", "a.go", map[string]any{"coverage_pct": 80.0})
	addHealthFn(srv.graph, "b.go::Fn1", "b.go", map[string]any{"coverage_pct": 10.0})

	out := callAnalyzeHealth(t, srv, map[string]any{"roll_up": "file"})
	rollup, ok := out["rollup"].([]any)
	require.True(t, ok)
	assert.Equal(t, "file", out["scope"])

	byKey := map[string]map[string]any{}
	for _, r := range rollup {
		row := r.(map[string]any)
		byKey[row["key"].(string)] = row
	}
	require.Contains(t, byKey, "a.go")
	require.Contains(t, byKey, "b.go")
	// a.go has two high-coverage symbols → higher avg than b.go.
	aAvg := byKey["a.go"]["avg_score"].(float64)
	bAvg := byKey["b.go"]["avg_score"].(float64)
	assert.Greater(t, aAvg, bAvg, "a.go avg must exceed b.go avg")
	assert.Equal(t, float64(2), byKey["a.go"]["symbols"])

	// Distribution still rides on the response in rollup mode.
	_, hasDist := out["distribution"]
	assert.True(t, hasDist)

	// Rollup sort: worst file first (b.go before a.go).
	first := rollup[0].(map[string]any)
	assert.Equal(t, "b.go", first["key"])
}

func TestAnalyzeHealthScore_RollupRepo(t *testing.T) {
	srv, _ := setupTestServer(t)
	// File nodes give the analyzer something to look at — without
	// a matching KindFile node the repo lookup falls back to the
	// path's first component, which is what we want to verify too.
	srv.graph.AddNode(&graph.Node{ID: "alpha/a.go", Kind: graph.KindFile, FilePath: "alpha/a.go", RepoPrefix: "alpha"})
	srv.graph.AddNode(&graph.Node{ID: "beta/b.go", Kind: graph.KindFile, FilePath: "beta/b.go", RepoPrefix: "beta"})

	addHealthFn(srv.graph, "alpha/a.go::Fn", "alpha/a.go", map[string]any{"coverage_pct": 90.0})
	addHealthFn(srv.graph, "beta/b.go::Fn", "beta/b.go", map[string]any{"coverage_pct": 10.0})

	out := callAnalyzeHealth(t, srv, map[string]any{"roll_up": "repo"})
	rollup := out["rollup"].([]any)
	keys := map[string]bool{}
	for _, r := range rollup {
		keys[r.(map[string]any)["key"].(string)] = true
	}
	assert.True(t, keys["alpha"], "alpha repo missing")
	assert.True(t, keys["beta"], "beta repo missing")
}

func TestAnalyzeHealthScore_RollupRejectsInvalidScope(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":    "health_score",
		"roll_up": "community",
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, res.IsError, "invalid roll_up scope must produce an error result")
}

func TestAnalyzeHealthScore_RollupGCX(t *testing.T) {
	srv, _ := setupTestServer(t)
	addHealthFn(srv.graph, "a.go::Fn", "a.go", map[string]any{"coverage_pct": 80.0})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":    "health_score",
		"roll_up": "file",
		"format":  "gcx",
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "analyze.health_score.rollup")
	for _, col := range []string{"scope", "key", "avg_score", "min_score", "max_score", "symbols", "count_a", "count_f"} {
		assert.Contains(t, text, col, "GCX rollup header must list %q", col)
	}
}
