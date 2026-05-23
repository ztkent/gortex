package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPlanTurn_EmptyTask_Errors(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "plan_turn", map[string]any{})
	assert.True(t, result.IsError, "expected error when task is missing")
}

func TestPlanTurn_AlwaysRecommendsSmartContextFirst(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "plan_turn", map[string]any{
		"task": "refactor the helper function",
	})
	require.False(t, result.IsError)

	var resp struct {
		RecommendedCalls []recommendedCall `json:"recommended_calls"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))

	require.Greater(t, len(resp.RecommendedCalls), 0)
	assert.Equal(t, "smart_context", resp.RecommendedCalls[0].Tool,
		"smart_context must be the first recommendation — the cheapest and broadest opening move")
	assert.NotEmpty(t, resp.RecommendedCalls[0].Why)
}

func TestPlanTurn_RespectsMaxCalls(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "plan_turn", map[string]any{
		"task":      "add rate limit to helper",
		"max_calls": 2,
	})
	require.False(t, result.IsError)

	var resp struct {
		RecommendedCalls []recommendedCall `json:"recommended_calls"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))
	assert.LessOrEqual(t, len(resp.RecommendedCalls), 2)
}

func TestPlanTurn_IncludesCandidates(t *testing.T) {
	srv, _ := setupTestServer(t)

	// The fixture defines "helper" and "main"; keyword "helper" must hit
	// at least one candidate.
	result := callTool(t, srv, "plan_turn", map[string]any{
		"task": "modify helper behavior",
	})
	require.False(t, result.IsError)

	var resp struct {
		TopCandidates    []string          `json:"top_candidates"`
		CandidateCount   int               `json:"candidate_count"`
		RecommendedCalls []recommendedCall `json:"recommended_calls"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))

	assert.Greater(t, len(resp.TopCandidates), 0, "expected at least one BM25 candidate for 'helper'")
	assert.Equal(t, len(resp.TopCandidates), resp.CandidateCount)

	// With candidates present, we should see a get_editing_context call
	// for the top candidate's file.
	var sawEditingContext bool
	for _, rec := range resp.RecommendedCalls {
		if rec.Tool == "get_editing_context" {
			sawEditingContext = true
			_, hasPath := rec.Args["path"]
			assert.True(t, hasPath, "get_editing_context recommendation must include path arg")
		}
	}
	assert.True(t, sawEditingContext, "expected get_editing_context recommendation when candidates exist")
}

func TestPlanTurn_NoCandidates_StillEmitsFallbacks(t *testing.T) {
	srv, _ := setupTestServer(t)

	result := callTool(t, srv, "plan_turn", map[string]any{
		"task": "xyzzynothingmatchesthis",
	})
	require.False(t, result.IsError)

	var resp struct {
		TopCandidates    []string          `json:"top_candidates"`
		RecommendedCalls []recommendedCall `json:"recommended_calls"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &resp))

	// No candidates — but plan_turn must still suggest openings so the
	// agent doesn't hit a dead end.
	assert.GreaterOrEqual(t, len(resp.RecommendedCalls), 2)
	assert.Equal(t, "smart_context", resp.RecommendedCalls[0].Tool)

	// search_symbols fallback should appear somewhere in the recommendations.
	var sawSearch bool
	for _, rec := range resp.RecommendedCalls {
		if rec.Tool == "search_symbols" {
			sawSearch = true
		}
	}
	assert.True(t, sawSearch, "expected search_symbols fallback when no BM25 candidates")
}

// TestExtractKeywords_PrioritisesIdentifierShape pins the regression
// where `find every caller of renderToolsSearchResult` produced
// recommendations centred on unrelated `findLocked` / `findDeclaration`
// methods because the BM25 budget filled on "find" before the actual
// identifier got a chance. After the fix, identifier-shape tokens
// surface first and English verbs ("find", "caller") are stopped.
func TestExtractKeywords_PrioritisesIdentifierShape(t *testing.T) {
	got := extractKeywords("find every caller of renderToolsSearchResult")
	require.NotEmpty(t, got)
	// Identifier-shape token must be first so the BM25 budget anchors
	// on what the user actually typed.
	require.Equal(t, "renderToolsSearchResult", got[0],
		"identifier-shape keyword must lead, got %v", got)
	// Verb-shape "find" / "every" / "caller" must be stop-listed.
	for _, kw := range got {
		require.NotEqual(t, "find", strings.ToLower(kw))
		require.NotEqual(t, "every", strings.ToLower(kw))
		require.NotEqual(t, "caller", strings.ToLower(kw))
	}
}

// TestExtractKeywords_PlainWordsStillSurfaceWhenNoIdentifier covers
// the fallback: when the task has no identifier-shape token, plain
// english keywords still come through (e.g. "trace error logger").
func TestExtractKeywords_PlainWordsStillSurfaceWhenNoIdentifier(t *testing.T) {
	got := extractKeywords("trace the error logger")
	require.Contains(t, got, "trace")
	require.Contains(t, got, "error")
	require.Contains(t, got, "logger")
}

func TestIsCallableKind(t *testing.T) {
	callable := []graph.NodeKind{
		graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface,
	}
	for _, k := range callable {
		assert.True(t, isCallableKind(k), "kind %s should be callable", k)
	}
	nonCallable := []graph.NodeKind{
		graph.KindFile, graph.KindImport, graph.KindVariable, graph.KindPackage,
	}
	for _, k := range nonCallable {
		assert.False(t, isCallableKind(k), "kind %s must not be callable", k)
	}
}
