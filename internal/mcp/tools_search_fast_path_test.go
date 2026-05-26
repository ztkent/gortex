package mcp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// recordingBackend is a search.Backend that counts how many times the
// engine called into Search, VectorChannelOnly, and
// SearchSymbolBundles. The identifier-shape fast path test reads these
// counters to assert the handler skipped the vector channel and skipped
// the combined-OR fan-out.
//
// Implements search.Backend, search.ChannelSearcher,
// search.SymbolBundleSearcherBackend, and the VectorChannelOnly
// duck-typed interface the engine queries on the bundle-bypass path.
type recordingBackend struct {
	hits             []search.SearchResult
	nodes            map[string]*graph.Node
	searchCalls      atomic.Int32
	bundleCalls      atomic.Int32
	vectorOnlyCalls  atomic.Int32
	channelCalls     atomic.Int32
	lastQueries      []string
	queriesMu        atomic.Pointer[[]string]
}

func newRecordingBackend(nodes map[string]*graph.Node, hits []search.SearchResult) *recordingBackend {
	rb := &recordingBackend{hits: hits, nodes: nodes}
	empty := []string{}
	rb.queriesMu.Store(&empty)
	return rb
}

func (rb *recordingBackend) recordQuery(q string) {
	for {
		oldPtr := rb.queriesMu.Load()
		newList := append([]string(nil), *oldPtr...)
		newList = append(newList, q)
		if rb.queriesMu.CompareAndSwap(oldPtr, &newList) {
			return
		}
	}
}

func (rb *recordingBackend) queries() []string {
	return *rb.queriesMu.Load()
}

func (rb *recordingBackend) Add(id string, fields ...string) {}
func (rb *recordingBackend) Remove(id string)                {}
func (rb *recordingBackend) Count() int                      { return len(rb.hits) }
func (rb *recordingBackend) Close()                          {}

func (rb *recordingBackend) Search(query string, limit int) []search.SearchResult {
	rb.searchCalls.Add(1)
	rb.recordQuery(query)
	return rb.hits
}

func (rb *recordingBackend) SearchChannels(query string, limit int) ([]search.SearchResult, []string) {
	rb.channelCalls.Add(1)
	rb.recordQuery(query)
	return rb.hits, nil
}

func (rb *recordingBackend) VectorChannelOnly(query string, limit int) ([]string, search.ChannelTimings) {
	rb.vectorOnlyCalls.Add(1)
	return nil, search.ChannelTimings{}
}

// SearchSymbolBundles satisfies the bundle interface so the engine
// takes the bundle fast path on this backend. Edges are nil — the
// rerank tolerates an empty edge cache (it'll fall back to per-node
// fetches via Graph, but for the test we just care that the call
// signature flows through).
func (rb *recordingBackend) SearchSymbolBundles(query string, limit int) []search.SymbolBundle {
	rb.bundleCalls.Add(1)
	rb.recordQuery(query)
	if len(rb.hits) == 0 {
		return nil
	}
	out := make([]search.SymbolBundle, 0, len(rb.hits))
	for _, h := range rb.hits {
		n := rb.nodes[h.ID]
		if n == nil {
			continue
		}
		out = append(out, search.SymbolBundle{Node: n, Score: h.Score})
	}
	return out
}

// identifierFastPathTestServer wires a Server around the recording backend so a
// search_symbols call can be inspected for vector / expansion fan-out
// activity.
func identifierFastPathTestServer(t *testing.T, names []string) (*Server, *recordingBackend) {
	t.Helper()
	g := graph.New()
	nodes := make(map[string]*graph.Node, len(names))
	hits := make([]search.SearchResult, 0, len(names))
	for i, n := range names {
		id := "pkg/" + n + ".go::" + n
		node := &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: n,
			FilePath: "pkg/" + n + ".go", StartLine: i + 1, EndLine: i + 5, Language: "go",
		}
		g.AddNode(node)
		nodes[id] = node
		hits = append(hits, search.SearchResult{ID: id, Score: 1.0 / float64(i+1)})
	}
	rb := newRecordingBackend(nodes, hits)
	eng := query.NewEngine(g)
	eng.SetSearch(rb)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv, rb
}

// TestSearchSymbols_IdentifierFastPath_SkipsVectorAndExpansion is the
// behavioural guard for the QueryClassSymbol / Path / Signature fast
// path. Three contracts must hold:
//
//  1. The vector channel (VectorChannelOnly on the bundle path,
//     SearchChannels on the legacy path) is NEVER called.
//  2. Only the primary query reaches the backend — no combined-OR
//     fan-out gets emitted (no second Search / Bundle call carrying
//     a concatenated expansion-term string).
//  3. The query_class echoed back in the response matches what the
//     handler actually treated the query as.
//
// "NewServer" is the canonical identifier-shape probe (PascalCase, no
// whitespace, no separator) — classifies as QueryClassSymbol.
func TestSearchSymbols_IdentifierFastPath_SkipsVectorAndExpansion(t *testing.T) {
	srv, rb := identifierFastPathTestServer(t, []string{"NewServer", "NewClient", "StartServer", "Server"})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": "NewServer", "limit": 10}
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "search errored: %v", res.Content)

	// Contract 1: no vector channel call. The bundle path's
	// VectorChannelOnly is the production-shape probe; SearchChannels
	// is the legacy fallback. Neither may fire for an identifier query.
	require.Equal(t, int32(0), rb.vectorOnlyCalls.Load(),
		"identifier fast path must not call VectorChannelOnly; queries=%v", rb.queries())
	require.Equal(t, int32(0), rb.channelCalls.Load(),
		"identifier fast path must not call SearchChannels; queries=%v", rb.queries())

	// Contract 2: only the primary query reaches the backend. Bundle
	// path: one call to SearchSymbolBundles with the bare query.
	// Fallback Search may also fire (zero candidates → fallback tier),
	// but the combined-OR expansion call is the regression to guard
	// against — no Search/Bundle query carries a multi-token expansion
	// payload like "NewServer StartServer Server …".
	require.Equal(t, int32(1), rb.bundleCalls.Load(),
		"primary bundle call should fire exactly once; queries=%v", rb.queries())
	for _, q := range rb.queries() {
		require.Equal(t, "NewServer", q,
			"only the original query is allowed to reach the backend on the identifier fast path; saw %q in %v", q, rb.queries())
	}

	// Contract 3: response echoes the class.
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Equal(t, "symbol", resp["query_class"],
		"response must echo the classified query_class")
}

// TestSearchSymbols_ConceptQuery_DoesNotEngageFastPath is the negative
// guard: a natural-language query (concept class) keeps the legacy
// pipeline — vector channel allowed, expansion allowed. Without this
// the fast-path optimisation could silently swallow concept queries.
func TestSearchSymbols_ConceptQuery_DoesNotEngageFastPath(t *testing.T) {
	srv, rb := identifierFastPathTestServer(t, []string{"AuthMiddleware", "ValidateToken", "ParseConfig", "Helper"})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	// Multi-word natural-language query → QueryClassConcept.
	req.Params.Arguments = map[string]any{"query": "where do we validate the user token auth", "limit": 10}
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "search errored: %v", res.Content)

	// Concept queries MUST still let the engine fan out to the vector
	// channel — the bundle's VectorChannelOnly call fires on the
	// bundle hot path. Anything that prevented this would silently
	// downgrade the natural-language search experience.
	require.GreaterOrEqual(t, rb.vectorOnlyCalls.Load(), int32(1),
		"concept query must still pull the vector channel; queries=%v", rb.queries())

	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Equal(t, "concept", resp["query_class"],
		"NL query must classify as concept")
}
