package mcp

import (
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestToolsSearch_TruncationAdvisesExactNameRetry covers the under-return
// signal: when a broad query matches more deferred tools than max_results,
// the result reports omitted_count and the text advises a select: retry so
// the agent does not silently lose the tool it was looking for.
func TestToolsSearch_TruncationAdvisesExactNameRetry(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)

	// "memories" matches the whole memory tool family — a deliberately
	// broad query. Capped at 1 result, the rest must be reported.
	result := callToolsSearch(t, srv, map[string]any{
		"query":       "memories",
		"max_results": 1,
		"promote":     false,
	})
	require.False(t, result.IsError)

	body := decodeStructured(t, result)
	require.LessOrEqual(t, len(body.Tools), 1, "max_results must cap the returned schemas")
	require.Positive(t, body.OmittedCount, "a broad query past the cap must report omitted_count")

	text := result.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "select:", "truncated result text must advise a select:<exact-name> retry")
	require.Contains(t, text, "more tool(s) matched", "truncated result text must name the under-return")
}

// TestToolsSearch_NoMatchAdvisesExactNameRetry covers the zero-result
// path: an agent that mistyped a keyword is pointed at select:.
func TestToolsSearch_NoMatchAdvisesExactNameRetry(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	result := callToolsSearch(t, srv, map[string]any{"query": "xyzzyplugh"})
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text
	require.Contains(t, text, "select:", "a no-match result must still point at the select: escape hatch")
}

// TestToolsSearch_SelectBytesBudgetTruncates pins the wire-budget
// floor: when a select:<many-tools> query would push the rendered
// response past defaultMaxBytes the handler trims the tail and
// reports OmittedCount, instead of returning a 50KB+ blob the stdio
// bridge spills to disk.
func TestToolsSearch_SelectBytesBudgetTruncates(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	// Trim helper directly so the test doesn't depend on a specific
	// tool family's catalogue size. Build a payload that would exceed
	// the budget without trimming.
	bulkySchema := []byte(`{"type":"object","properties":{`)
	for i := 0; i < 200; i++ {
		bulkySchema = append(bulkySchema, []byte(`"prop`)...)
		bulkySchema = append(bulkySchema, byte('A'+(i%26)))
		bulkySchema = append(bulkySchema, []byte(`":{"type":"string","description":"padding padding padding padding padding padding padding"},`)...)
	}
	bulkySchema = append(bulkySchema, []byte(`}}`)...)
	entries := make([]toolsSearchEntry, 0, 50)
	for i := 0; i < 50; i++ {
		entries = append(entries, toolsSearchEntry{
			Name:        fmt.Sprintf("entry_%d", i),
			Description: "padding padding padding padding padding padding padding padding padding padding",
			InputSchema: bulkySchema,
		})
	}
	kept, omitted := trimToolsSearchEntries(entries, defaultMaxBytes)
	require.Greater(t, omitted, 0, "with a 50-entry list and bulky schemas, the budget must drop something")
	require.Less(t, len(kept), len(entries))
	// Worst case ceiling: the kept-bytes sum stays under the budget
	// plus one over-budget entry (the floor case).
	totalKeptBytes := 0
	for _, e := range kept {
		totalKeptBytes += len(e.Name) + len(e.Description) + len(e.InputSchema)
	}
	require.LessOrEqual(t, totalKeptBytes, defaultMaxBytes+len(kept[0].InputSchema))
}

// TestToolsSearch_WholeQueryNameMatchRanksFirst pins the ranking audit:
// a query that is a deferred tool's name verbatim surfaces that tool as
// the top hit, even when other tools share one of the query tokens.
func TestToolsSearch_WholeQueryNameMatchRanksFirst(t *testing.T) {
	t.Setenv("GORTEX_LAZY_TOOLS", "1")
	srv, _ := setupTestServer(t)
	require.Contains(t, srv.lazy.DeferredNames(), "taint_paths")

	result := callToolsSearch(t, srv, map[string]any{
		"query":       "taint_paths",
		"max_results": 5,
		"promote":     false,
	})
	require.False(t, result.IsError)

	body := decodeStructured(t, result)
	require.NotEmpty(t, body.Tools)
	require.Equal(t, "taint_paths", body.Tools[0].Name,
		"an exact tool-name query must rank that tool first")
}
