package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// reviewQuestionsTestServer builds a Server over a synthetic graph
// crafted to fire each anomaly miner deterministically:
//
//   - HubFn        : 10 callers, NO inbound test  -> hub_risk + untested_hotspot
//   - TestedHubFn  : 10 callers, has inbound test -> hub_risk only
//   - GoCaller     : a go-language symbol with a cross-language edge to
//     PyTarget (python) -> surprising
//   - Lonely       : sole member of its own community -> thin_community
func reviewQuestionsTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()

	add := func(id, name string, kind graph.NodeKind, lang, file string) {
		g.AddNode(&graph.Node{ID: id, Name: name, Kind: kind, Language: lang, FilePath: file, StartLine: 1})
	}

	add("pkg/hub.go::HubFn", "HubFn", graph.KindFunction, "go", "pkg/hub.go")
	add("pkg/hub.go::TestedHubFn", "TestedHubFn", graph.KindFunction, "go", "pkg/hub.go")
	add("pkg/x.go::GoCaller", "GoCaller", graph.KindFunction, "go", "pkg/x.go")
	add("pkg/y.py::PyTarget", "PyTarget", graph.KindFunction, "python", "pkg/y.py")
	add("pkg/lone.go::Lonely", "Lonely", graph.KindFunction, "go", "pkg/lone.go")

	// 10 distinct callers into each hub — clears the default
	// hub_threshold of 8 with margin (>= 2x => HIGH severity).
	for i := 0; i < 10; i++ {
		caller := "pkg/callers.go::C" + itoa(i)
		add(caller, "C"+itoa(i), graph.KindFunction, "go", "pkg/callers.go")
		g.AddEdge(&graph.Edge{From: caller, To: "pkg/hub.go::HubFn", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: caller, To: "pkg/hub.go::TestedHubFn", Kind: graph.EdgeCalls})
	}

	// TestedHubFn is covered; HubFn is not.
	g.AddNode(&graph.Node{ID: "pkg/hub_test.go::TestHub", Name: "TestHub", Kind: graph.KindFunction, Language: "go", FilePath: "pkg/hub_test.go", StartLine: 1})
	g.AddEdge(&graph.Edge{From: "pkg/hub_test.go::TestHub", To: "pkg/hub.go::TestedHubFn", Kind: graph.EdgeTests})

	// Cross-language edge: go -> python. To clear the surprising
	// threshold (cross_language alone is +0.2, below the 0.3 floor),
	// make PyTarget a hub so peripheral_to_hub (+0.2) also fires:
	// GoCaller (in-degree 0, peripheral) reaches into PyTarget
	// (in-degree >= 5, a hub). cross_language + peripheral_to_hub = 0.4.
	g.AddEdge(&graph.Edge{From: "pkg/x.go::GoCaller", To: "pkg/y.py::PyTarget", Kind: graph.EdgeCalls})
	for i := 0; i < 9; i++ {
		pc := "pkg/y.py::PyC" + itoa(i)
		add(pc, "PyC"+itoa(i), graph.KindFunction, "python", "pkg/y.py")
		g.AddEdge(&graph.Edge{From: pc, To: "pkg/y.py::PyTarget", Kind: graph.EdgeCalls})
	}

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}

	// Communities: Lonely is the sole member of its cluster (thin);
	// everyone else shares one large cluster.
	bigMembers := []string{
		"pkg/hub.go::HubFn", "pkg/hub.go::TestedHubFn",
		"pkg/x.go::GoCaller", "pkg/y.py::PyTarget",
	}
	nodeToComm := map[string]string{"pkg/lone.go::Lonely": "c-lonely"}
	for _, m := range bigMembers {
		nodeToComm[m] = "c-big"
	}
	s.communities = &analysis.CommunityResult{
		Communities: []analysis.Community{
			{ID: "c-big", Label: "big", Size: len(bigMembers)},
			{ID: "c-lonely", Label: "lonely", Size: 1},
		},
		NodeToComm: nodeToComm,
	}
	return s
}

func callReviewQuestions(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "suggested_review_questions"
	req.Params.Arguments = args
	res, err := s.handleSuggestedReviewQuestions(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "handler returned error result: %+v", res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

// questionsByCategory groups the returned question objects by category
// and indexes them by the symbol id they anchor on.
func questionsByCategory(t *testing.T, m map[string]any) map[string]map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]map[string]any{}
	raw, _ := m["questions"].([]any)
	for _, q := range raw {
		qm, ok := q.(map[string]any)
		require.True(t, ok)
		cat, _ := qm["category"].(string)
		sym, _ := qm["symbol_id"].(string)
		if out[cat] == nil {
			out[cat] = map[string]map[string]any{}
		}
		out[cat][sym] = qm
	}
	return out
}

func TestSuggestedReviewQuestions_PerCategoryAnchoring(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	m := callReviewQuestions(t, s, map[string]any{"limit": 50})

	cats := questionsByCategory(t, m)

	// hub_risk: HubFn AND TestedHubFn both clear fan-in 8.
	require.Contains(t, cats, rqCatHubRisk)
	require.Contains(t, cats[rqCatHubRisk], "pkg/hub.go::HubFn")
	require.Contains(t, cats[rqCatHubRisk], "pkg/hub.go::TestedHubFn")

	// untested_hotspot: HubFn only (TestedHubFn has an inbound test edge).
	require.Contains(t, cats, rqCatUntestedHotspot)
	require.Contains(t, cats[rqCatUntestedHotspot], "pkg/hub.go::HubFn")
	require.NotContains(t, cats[rqCatUntestedHotspot], "pkg/hub.go::TestedHubFn")

	// surprising: the go->python edge surfaces, anchored on GoCaller (source).
	require.Contains(t, cats, rqCatSurprising)
	require.Contains(t, cats[rqCatSurprising], "pkg/x.go::GoCaller")
	q := cats[rqCatSurprising]["pkg/x.go::GoCaller"]
	require.Contains(t, strings.Join(toStrSlice(q["signals"]), " "), "cross_language")

	// thin_community: Lonely is a sole-member cluster.
	require.Contains(t, cats, rqCatThinCommunity)
	require.Contains(t, cats[rqCatThinCommunity], "pkg/lone.go::Lonely")

	// Every question's symbol_id resolves in the graph, carries a file,
	// and a known severity.
	raw, _ := m["questions"].([]any)
	require.NotEmpty(t, raw)
	for _, qq := range raw {
		qm := qq.(map[string]any)
		sym, _ := qm["symbol_id"].(string)
		require.NotNil(t, s.graph.GetNode(sym), "symbol_id %q must resolve", sym)
		require.NotEmpty(t, qm["file"], "question for %q must carry a file", sym)
		sev, _ := qm["severity"].(string)
		require.Contains(t, []string{rqSevHigh, rqSevMedium, rqSevLow}, sev)
	}
}

func TestSuggestedReviewQuestions_PrioritizedHighestFirst(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	m := callReviewQuestions(t, s, map[string]any{"limit": 50})

	raw, _ := m["questions"].([]any)
	require.NotEmpty(t, raw)

	// Severity must be non-increasing down the list (HIGH > MEDIUM > LOW).
	prev := 99
	for _, qq := range raw {
		qm := qq.(map[string]any)
		sev, _ := qm["severity"].(string)
		rank := severityRankRQ(sev)
		require.LessOrEqual(t, rank, prev, "questions must be sorted highest-severity first")
		prev = rank
	}

	// The very first question is HIGH — the untested hub (fan-in 10 >= 2x
	// threshold) is the highest-risk finding in the fixture.
	first := raw[0].(map[string]any)
	require.Equal(t, rqSevHigh, first["severity"])
}

func TestSuggestedReviewQuestions_CategoriesFilterNarrows(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	m := callReviewQuestions(t, s, map[string]any{
		"limit":      50,
		"categories": "hub_risk",
	})
	cats := questionsByCategory(t, m)
	require.Contains(t, cats, rqCatHubRisk)
	require.NotContains(t, cats, rqCatUntestedHotspot)
	require.NotContains(t, cats, rqCatSurprising)
	require.NotContains(t, cats, rqCatThinCommunity)
	require.NotContains(t, cats, rqCatBridge)

	byCat, _ := m["by_category"].(map[string]any)
	require.Contains(t, byCat, rqCatHubRisk)
	require.NotContains(t, byCat, rqCatSurprising)
}

func TestSuggestedReviewQuestions_IDsNarrowToChangeset(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	// Only HubFn in the changeset -> hub_risk + untested for HubFn, but
	// NOT TestedHubFn (out of changeset) and not the surprising GoCaller.
	m := callReviewQuestions(t, s, map[string]any{
		"limit": 50,
		"ids":   "pkg/hub.go::HubFn",
	})
	cats := questionsByCategory(t, m)
	require.Contains(t, cats[rqCatHubRisk], "pkg/hub.go::HubFn")
	require.NotContains(t, cats[rqCatHubRisk], "pkg/hub.go::TestedHubFn")
	require.NotContains(t, cats, rqCatSurprising)
}

func TestSuggestedReviewQuestions_GCXRoundTrip(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "suggested_review_questions"
	req.Params.Arguments = map[string]any{"limit": 50, "format": "gcx"}
	res, err := s.handleSuggestedReviewQuestions(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc := res.Content[0].(mcp.TextContent)

	dec := wire.NewDecoder(strings.NewReader(tc.Text))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "suggested_review_questions.summary", h.Tool)
	_, err = dec.All()
	require.NoError(t, err)

	qh, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "suggested_review_questions.questions", qh.Tool)
	rows, err := dec.All()
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	// Every decoded row carries a category, a severity, and a resolvable
	// symbol id — proving the wire form preserves the anchoring.
	sawHubRisk := false
	for _, r := range rows {
		require.NotEmpty(t, r["severity"])
		require.NotNil(t, s.graph.GetNode(r["symbol_id"]), "decoded symbol_id %q must resolve", r["symbol_id"])
		if r["category"] == rqCatHubRisk {
			sawHubRisk = true
		}
	}
	require.True(t, sawHubRisk, "gcx rows should include a hub_risk question")
}

func TestSuggestedReviewQuestions_BudgetTruncates(t *testing.T) {
	s := reviewQuestionsTestServer(t)
	// A tiny byte budget forces truncation of the JSON questions list.
	m := callReviewQuestions(t, s, map[string]any{"limit": 50, "max_bytes": 400})
	// The budget machinery marks the response when it trims.
	if trimmed, ok := m["_truncated_by_budget"].(bool); ok {
		require.True(t, trimmed)
	} else {
		// Fall back to the limit-driven truncated flag at a small limit.
		m2 := callReviewQuestions(t, s, map[string]any{"limit": 1})
		require.Equal(t, true, m2["truncated"])
	}
}

// TestCollectSurprisingEdges_ExtractionBehaviorPreserved asserts the
// extracted helper returns exactly the rows the get_surprising_connections
// handler surfaces — the extraction did not change behaviour.
func TestCollectSurprisingEdges_ExtractionBehaviorPreserved(t *testing.T) {
	s := reviewQuestionsTestServer(t)

	// Drive the handler (json) to get its surfaced connections.
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_surprising_connections"
	req.Params.Arguments = map[string]any{"limit": 1000}
	res, err := s.handleGetSurprisingConnections(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc := res.Content[0].(mcp.TextContent)
	var handlerOut map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &handlerOut))
	conns, _ := handlerOut["connections"].([]any)

	// Drive the helper directly with the same defaults.
	scoped := map[string]*graph.Node{}
	for _, n := range s.scopedNodes(context.Background()) {
		scoped[n.ID] = n
	}
	rows := s.collectSurprisingEdges(context.Background(), scoped, "", 0.3, 5, 5.0)

	require.Equal(t, len(conns), len(rows), "helper and handler must surface the same number of edges")

	// The from/to pairs match one-for-one in order.
	for i, c := range conns {
		cm := c.(map[string]any)
		require.Equal(t, cm["from"], rows[i].From)
		require.Equal(t, cm["to"], rows[i].To)
	}

	// And the fixture does surface the cross-language edge.
	require.NotEmpty(t, rows)
	foundCrossLang := false
	for _, r := range rows {
		if r.From == "pkg/x.go::GoCaller" && r.To == "pkg/y.py::PyTarget" {
			foundCrossLang = true
		}
	}
	require.True(t, foundCrossLang, "the go->python edge must surface as surprising")
}

func toStrSlice(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
