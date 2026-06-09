package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/review"
)

// critiqueTestServer builds a minimal MCP server with a bare graph — enough to
// register and dispatch critique_review, which takes its findings from the
// `findings` argument and needs no working tree.
func critiqueTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	return NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil)
}

// critiqueFindingsJSON is a three-finding prior-review payload in the same wire
// shape `review` / `review_pack` emit.
const critiqueFindingsJSON = `[
	{"rule":"nil-deref","severity":"error","category":"nil-deref","file":"app/svc.go","line":12,"message":"p may be nil"},
	{"rule":"style-nit","severity":"warning","category":"style","file":"app/svc.go","line":20,"message":"prefer fmt.Errorf"},
	{"rule":"doc-todo","severity":"info","category":"doc","file":"app/util.go","line":4,"message":"missing doc comment"}
]`

type critiqueOut struct {
	Verdict   string `json:"verdict"`
	Summary   string `json:"summary"`
	KeptCount int    `json:"kept_count"`
	Total     int    `json:"total"`
	Uncertain int    `json:"uncertain"`
	LLMUsed   bool   `json:"llm_used"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Kept      []struct {
		Rule string `json:"rule"`
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"kept"`
	Dropped []struct {
		Rule            string `json:"rule"`
		CritiqueVerdict string `json:"critique_verdict"`
		CritiqueReason  string `json:"critique_reason"`
	} `json:"dropped"`
}

func decodeCritique(t *testing.T, res *mcplib.CallToolResult) critiqueOut {
	t.Helper()
	require.False(t, res.IsError, "errored: %s", toolText(res))
	var out critiqueOut
	require.NoError(t, json.Unmarshal([]byte(toolText(res)), &out))
	return out
}

// TestCritiqueReview_ToolRegistered confirms the tool wired up.
func TestCritiqueReview_ToolRegistered(t *testing.T) {
	srv := critiqueTestServer(t)
	require.NotNil(t, srv.MCPServer().GetTool("critique_review"),
		"critique_review must be registered")
}

// TestCritiqueReview_DropsOneFinding feeds a prior-review findings payload and a
// stubbed critique LLM that drops the middle finding; the tool returns the kept
// set, the dropped finding with its reason, and a revised verdict.
func TestCritiqueReview_DropsOneFinding(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[
				{"index":0,"verdict":"keep","reason":"genuine nil deref"},
				{"index":1,"verdict":"drop","reason":"style nit, not a defect"},
				{"index":2,"verdict":"keep","reason":"valid doc gap"}
			]`, nil
		}
	}

	out := decodeCritique(t, callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
	}))

	require.True(t, out.LLMUsed)
	require.Equal(t, 3, out.Total)
	require.Equal(t, 2, out.KeptCount)
	require.Len(t, out.Kept, 2)
	require.Len(t, out.Dropped, 1)
	require.Equal(t, "style-nit", out.Dropped[0].Rule)
	require.Equal(t, "drop", out.Dropped[0].CritiqueVerdict)
	require.Equal(t, "style nit, not a defect", out.Dropped[0].CritiqueReason)

	// An error finding survived → verdict stays BLOCK.
	require.Equal(t, string(review.VerdictBlock), out.Verdict)
}

// TestCritiqueReview_DisabledLLMKeepsAll proves that with no LLM service and no
// override, the tool returns a structured 'llm not configured' result (not a Go
// error) and drops nothing.
func TestCritiqueReview_DisabledLLMKeepsAll(t *testing.T) {
	srv := critiqueTestServer(t)
	// No llmService, no override → gen is nil.

	res := callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
	})
	require.False(t, res.IsError, "disabled LLM is a structured result, not an error")
	out := decodeCritique(t, res)
	require.False(t, out.LLMUsed)
	require.Empty(t, out.Dropped)
	require.Contains(t, strings.ToLower(out.Summary), "llm not configured")
}

// TestCritiqueReview_GarbageLLMKeepsAll proves an unparseable model response is a
// no-op pass-through: every finding kept, nothing dropped, no error.
func TestCritiqueReview_GarbageLLMKeepsAll(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return "these all look fine, no structured output here", nil
		}
	}

	out := decodeCritique(t, callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
	}))

	require.False(t, out.LLMUsed, "garbage response must not count as an LLM critique")
	require.Equal(t, 3, out.KeptCount)
	require.Empty(t, out.Dropped)
	require.Equal(t, string(review.VerdictBlock), out.Verdict)
}

// TestCritiqueReview_RevisedVerdictDowngrade proves dropping the only error
// finding downgrades the revised verdict.
func TestCritiqueReview_RevisedVerdictDowngrade(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[{"index":0,"verdict":"drop","reason":"checked above"}]`, nil
		}
	}

	out := decodeCritique(t, callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
	}))

	require.Len(t, out.Dropped, 1)
	require.Equal(t, "nil-deref", out.Dropped[0].Rule)
	// Worst remaining is the warning → REVIEW.
	require.Equal(t, string(review.VerdictReview), out.Verdict)
}

// TestCritiqueReview_GCXRoundTrip proves the gcx encoder runs and emits the
// summary + kept + dropped sections.
func TestCritiqueReview_GCXRoundTrip(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[{"index":1,"verdict":"drop","reason":"cosmetic"}]`, nil
		}
	}

	res := callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
		"format":   "gcx",
	})
	require.False(t, res.IsError, "gcx errored: %s", toolText(res))
	text := toolText(res)
	require.Contains(t, text, "critique_review.summary")
	require.Contains(t, text, "critique_review.kept")
	require.Contains(t, text, "critique_review.dropped")
}

// TestCritiqueReview_TOONRoundTrip proves the toon path runs.
func TestCritiqueReview_TOONRoundTrip(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[{"index":0,"verdict":"keep","reason":""}]`, nil
		}
	}

	res := callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": critiqueFindingsJSON,
		"format":   "toon",
	})
	require.False(t, res.IsError, "toon errored: %s", toolText(res))
	require.NotEmpty(t, toolText(res))
}

// TestCritiqueReview_MaxBytesBudget proves the byte budget caps the response and
// stamps truncation metadata.
func TestCritiqueReview_MaxBytesBudget(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) {
			return `[{"index":0,"verdict":"keep","reason":""}]`, nil
		}
	}

	res := callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings":  critiqueFindingsJSON,
		"max_bytes": float64(200),
	})
	require.False(t, res.IsError)
	require.LessOrEqual(t, len(toolText(res)), 4096, "the response respects a tight byte budget")
}

// TestCritiqueReview_InvalidFindingsJSON returns a structured error for malformed
// findings input.
func TestCritiqueReview_InvalidFindingsJSON(t *testing.T) {
	srv := critiqueTestServer(t)
	srv.critiqueLLMGenOverride = func() review.LLMGen {
		return func(_ context.Context, _ string, _ int) (string, error) { return `[]`, nil }
	}

	res := callToolByName(t, srv, context.Background(), "critique_review", map[string]any{
		"findings": "{ not json",
	})
	require.True(t, res.IsError)
	require.Contains(t, strings.ToLower(toolText(res)), "invalid findings json")
}
