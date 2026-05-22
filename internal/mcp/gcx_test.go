package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func newTestNode(id, name string, kind graph.NodeKind, path string, line int) *graph.Node {
	return &graph.Node{
		ID:        id,
		Name:      name,
		Kind:      kind,
		FilePath:  path,
		StartLine: line,
		EndLine:   line + 5,
		Meta:      map[string]any{"signature": "func " + name + "()"},
	}
}

func TestEncodeSearchSymbols_HeaderAndRows(t *testing.T) {
	nodes := []*graph.Node{
		newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10),
		newTestNode("b.go::Bar", "Bar", graph.KindMethod, "b.go", 20),
	}
	payload, err := encodeSearchSymbols(nodes, 2, 10)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "search_symbols", h.Tool)
	require.Equal(t, []string{"id", "kind", "name", "path", "path_abs", "line", "sig", "is_test", "test_role", "test_runner"}, h.Fields)
	require.Equal(t, "2", h.Meta["total"])
	require.Equal(t, "false", h.Meta["truncated"])

	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "function", rows[0]["kind"])
	require.Equal(t, "Foo", rows[0]["name"])
	require.Equal(t, "10", rows[0]["line"])
	require.Equal(t, "func Foo()", rows[0]["sig"])
	require.Equal(t, "", rows[0]["path_abs"], "path_abs column present, empty when the node carries no resolved absolute path")
	require.Equal(t, "false", rows[0]["is_test"])
	require.Equal(t, "", rows[0]["test_role"])
}

func TestEncodeSearchSymbols_RespectsLimitAndTruncation(t *testing.T) {
	nodes := make([]*graph.Node, 5)
	for i := range nodes {
		nodes[i] = newTestNode("x.go::N", "N", graph.KindFunction, "x.go", i)
	}
	payload, err := encodeSearchSymbols(nodes, 5, 3)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "true", h.Meta["truncated"])
	rows, _ := dec.All()
	require.Len(t, rows, 3)
}

func TestEncodeSearchSymbols_SkipsFileAndImport(t *testing.T) {
	nodes := []*graph.Node{
		newTestNode("f.go", "f.go", graph.KindFile, "f.go", 1),
		newTestNode("f.go::Foo", "Foo", graph.KindFunction, "f.go", 5),
		newTestNode("f.go::imp", "imp", graph.KindImport, "f.go", 2),
	}
	payload, err := encodeSearchSymbols(nodes, 3, 10)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, _ = dec.Header()
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Foo", rows[0]["name"])
}

func TestEncodeGetSymbolSource_EmbeddedNewlinesRoundTrip(t *testing.T) {
	node := newTestNode("f.go::Foo", "Foo", graph.KindFunction, "f.go", 10)
	src := "func Foo() {\n\tfmt.Println(\"x\\ty\")\n}"
	payload, err := encodeGetSymbolSource(node, src, 9, "etag123", "")
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_symbol_source", h.Tool)
	require.Equal(t, "etag123", h.Meta["etag"])

	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, src, rows[0]["source"])
	require.Equal(t, "9", rows[0]["from_line"])
	require.Equal(t, "etag123", rows[0]["etag"])
}

func TestEncodeBatchSymbols_IncludeSource(t *testing.T) {
	rows := []map[string]any{
		{
			"id":         "a.go::Foo",
			"kind":       graph.KindFunction,
			"name":       "Foo",
			"file_path":  "a.go",
			"start_line": 10,
			"end_line":   20,
			"signature":  "func Foo()",
			"source":     "func Foo() {}",
		},
		{
			"id":    "x.go::Missing",
			"error": "symbol not found",
		},
	}
	payload, err := encodeBatchSymbols(rows, true)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Contains(t, h.Fields, "source")
	require.Contains(t, h.Fields, "error")
	got, err := dec.All()
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "func Foo()", got[0]["sig"])
	require.Equal(t, "symbol not found", got[1]["error"])
}

func TestEncodeSubGraph_NodesAndEdgesSections(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Bar", "Bar", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Foo", To: "b.go::Bar", Kind: "calls", Confidence: 0.9, Origin: "ast_resolved"},
		},
		TotalNodes: 2,
	}
	payload, err := encodeSubGraph("get_callers", sg)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h1, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_callers.nodes", h1.Tool)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 2)

	h2, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_callers.edges", h2.Tool)
	edges, err := dec.All()
	require.NoError(t, err)
	require.Len(t, edges, 1)
	require.Equal(t, "calls", edges[0]["kind"])
	require.Equal(t, "ast_resolved", edges[0]["origin"])
}

func TestEncodeFindUsages_OneRowPerEdge(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{
			newTestNode("a.go::Caller", "Caller", graph.KindFunction, "a.go", 10),
			newTestNode("b.go::Target", "Target", graph.KindFunction, "b.go", 20),
		},
		Edges: []*graph.Edge{
			{From: "a.go::Caller", To: "b.go::Target", Kind: "calls", Origin: "lsp_resolved", Confidence: 1.0},
		},
	}
	payload, err := encodeFindUsages(sg)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	_, err = dec.Header()
	require.NoError(t, err)
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "a.go::Caller", rows[0]["from"])
	require.Equal(t, "b.go::Target", rows[0]["to"])
	require.Equal(t, "Caller", rows[0]["from_name"])
	require.Equal(t, "10", rows[0]["from_line"])
}

func TestEncodeAnalyze_DeadCode(t *testing.T) {
	items := []deadCodeItem{
		{ID: "a.go::Unused", Kind: "function", Name: "Unused", Path: "a.go", Line: 42, Reason: "no incoming edges"},
	}
	payload, err := encodeAnalyze("dead_code", items)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "analyze.dead_code", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Unused", rows[0]["name"])
	require.Equal(t, "no incoming edges", rows[0]["reason"])
}

func TestEncodeAnalyze_UnknownKindFallsBackToGeneric(t *testing.T) {
	payload, err := encodeAnalyze("weird", map[string]any{"x": 1})
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "analyze.weird", h.Tool)
}

func TestEncodeContractsList_FlattensByRepoAndPromotesMethodPath(t *testing.T) {
	rows := []contracts.Contract{
		{
			ID:         "http::GET::/search",
			Type:       contracts.ContractHTTP,
			Role:       contracts.RoleProvider,
			SymbolID:   "cmd/api/main.go::realMain",
			FilePath:   "sapi-backend/cmd/api/main.go",
			Line:       531,
			RepoPrefix: "sapi-backend",
			Confidence: 0.9,
			Meta:       map[string]any{"method": "GET", "path": "/search", "framework": "gin/echo/chi"},
		},
		{
			ID:         "dep::github.com/FindHotel/raa-sdk",
			Type:       contracts.ContractDependency,
			Role:       contracts.RoleConsumer,
			FilePath:   "sapi-backend/go.mod",
			Line:       21,
			RepoPrefix: "sapi-backend",
			Confidence: 1,
			Meta:       map[string]any{"module": "github.com/FindHotel/raa-sdk", "version": "v0.102.0"},
		},
	}
	payload, err := encodeContractsList(rows, len(rows))
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "contracts.list", h.Tool)
	require.Equal(t, "2", h.Meta["total"])
	require.Equal(t, contractFields, h.Fields)

	got, err := dec.All()
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.Equal(t, "http", got[0]["type"])
	require.Equal(t, "provider", got[0]["role"])
	require.Equal(t, "sapi-backend", got[0]["repo"])
	require.Equal(t, "GET", got[0]["method"])
	require.Equal(t, "/search", got[0]["path"])
	require.Equal(t, "531", got[0]["line"])
	require.Equal(t, "framework=gin/echo/chi", got[0]["meta"], "method/path must be excluded from meta column")

	require.Equal(t, "dependency", got[1]["type"])
	require.Equal(t, "", got[1]["method"])
	require.Equal(t, "", got[1]["path"])
	require.Equal(t, "module=github.com/FindHotel/raa-sdk;version=v0.102.0", got[1]["meta"])
}

func TestEncodeContractsCheck_EmitsThreeSections(t *testing.T) {
	provider := contracts.Contract{
		ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "a/provider.go", Line: 10, RepoPrefix: "a",
	}
	consumer := contracts.Contract{
		ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		FilePath: "b/consumer.go", Line: 20, RepoPrefix: "b",
	}
	orphanProv := contracts.Contract{
		ID: "http::GET::/dead", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		FilePath: "a/dead.go", Line: 5, RepoPrefix: "a", Meta: map[string]any{"method": "GET", "path": "/dead"},
	}
	orphanCons := contracts.Contract{
		ID: "http::GET::/nowhere", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
		FilePath: "c/lost.go", Line: 8, RepoPrefix: "c",
	}
	result := contracts.MatchResult{
		Matched: []contracts.CrossLink{
			{ContractID: "http::GET::/x", Provider: provider, Consumer: consumer, CrossRepo: true},
		},
		OrphanProviders: []contracts.Contract{orphanProv},
		OrphanConsumers: []contracts.Contract{orphanCons},
	}
	payload, err := encodeContractsCheck(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h1, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.matched", h1.Tool)
	require.Equal(t, "1", h1.Meta["count"])
	matched, err := dec.All()
	require.NoError(t, err)
	require.Len(t, matched, 1)
	require.Equal(t, "http::GET::/x", matched[0]["contract_id"])
	require.Equal(t, "true", matched[0]["cross_repo"])
	require.Equal(t, "a", matched[0]["provider_repo"])
	require.Equal(t, "b", matched[0]["consumer_repo"])

	h2, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.orphan_providers", h2.Tool)
	orphans, err := dec.All()
	require.NoError(t, err)
	require.Len(t, orphans, 1)
	require.Equal(t, "GET", orphans[0]["method"])
	require.Equal(t, "/dead", orphans[0]["path"])

	h3, err := dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "contracts.check.orphan_consumers", h3.Tool)
	cons, err := dec.All()
	require.NoError(t, err)
	require.Len(t, cons, 1)
	require.Equal(t, "c", cons[0]["repo"])
}

func TestEncodeEditingContext_FourSectionsWithFileMeta(t *testing.T) {
	file := map[string]any{"id": "pkg/foo.go", "language": "go"}
	defines := []map[string]any{
		{"id": "pkg/foo.go::Foo", "kind": "function", "name": "Foo", "start_line": 10, "signature": "func Foo()"},
	}
	imports := []map[string]any{
		{"id": "external::fmt", "external": true},
	}
	calledBy := []map[string]any{
		{"id": "pkg/bar.go::Bar", "name": "Bar", "file_path": "pkg/bar.go", "start_line": 5},
	}
	calls := []map[string]any{
		{"id": "pkg/baz.go::Baz", "name": "Baz", "file_path": "pkg/baz.go", "start_line": 3},
	}
	payload, err := encodeEditingContext(file, defines, imports, calledBy, calls, "etag-xyz", "")
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.defines", h.Tool)
	require.Equal(t, "etag-xyz", h.Meta["etag"])
	require.Equal(t, "go", h.Meta["language"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "func Foo()", rows[0]["sig"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.imports", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "true", rows[0]["external"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.called_by", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "pkg/bar.go", rows[0]["path"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "get_editing_context.calls", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Baz", rows[0]["name"])
}

func TestEncodeSmartContext_OmitsEmptySections(t *testing.T) {
	result := map[string]any{
		"task": "add a tool",
		"relevant_symbols": []map[string]any{
			{"id": "a.go::Foo", "kind": "function", "name": "Foo", "file_path": "a.go", "start_line": 10, "signature": "func Foo()"},
		},
		"related_test_files": []string{"a_test.go"},
		"files_to_edit":      []string{"a.go", "a_test.go"},
	}
	payload, err := encodeSmartContext(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "smart_context.symbols", h.Tool)
	require.Equal(t, "1", h.Meta["count"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "Foo", rows[0]["name"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "smart_context.tests", h.Tool, "cross_repo/entry_file/callers/callees must be skipped when empty")
	rows, _ = dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "a_test.go", rows[0]["path"])

	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "smart_context.files", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 2)
}

func TestEncodeSmartContext_IncludesAllSectionsWhenPopulated(t *testing.T) {
	result := map[string]any{
		"task": "trace auth",
		"relevant_symbols": []map[string]any{
			{"id": "auth.go::Check", "kind": "function", "name": "Check", "file_path": "auth.go", "start_line": 20},
		},
		"cross_repo_dependencies": []map[string]any{
			{"id": "sdk.go::Auth", "kind": "type", "name": "Auth", "file_path": "sdk/auth.go", "repo_prefix": "sdk", "edge_kind": "calls"},
		},
		"entry_file_symbols": []string{"function Check (line 20)"},
		"callers":            []string{"main.go::main"},
		"callees":            []string{"auth.go::verify"},
		"related_test_files": []string{"auth_test.go"},
		"files_to_edit":      []string{"auth.go"},
	}
	payload, err := encodeSmartContext(result)
	require.NoError(t, err)

	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	tools := []string{}
	if h, err := dec.Header(); err == nil {
		tools = append(tools, h.Tool)
		_, _ = dec.All()
		for {
			h2, err := dec.NextSection()
			if err != nil {
				break
			}
			tools = append(tools, h2.Tool)
			_, _ = dec.All()
		}
	}
	require.Equal(t, []string{
		"smart_context.symbols",
		"smart_context.cross_repo",
		"smart_context.entry_file",
		"smart_context.callers",
		"smart_context.callees",
		"smart_context.tests",
		"smart_context.files",
	}, tools)
}

func TestRequestedFormat_CoversCompactAndFormatArgs(t *testing.T) {
	f := wire.ParseFormat("gcx")
	require.Equal(t, wire.FormatGCX, f)
	require.Equal(t, wire.FormatText, wire.ParseFormat("compact"))
	require.Equal(t, wire.FormatJSON, wire.ParseFormat(""))
}

func TestEncodePrefetchContext_RoundTrip(t *testing.T) {
	n := newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10)
	cands := []prefetchCandidate{{
		Node:            n,
		ID:              n.ID,
		Kind:            string(n.Kind),
		FilePath:        n.FilePath,
		StartLine:       n.StartLine,
		Reason:          "matches task keyword",
		Confidence:      0.825,
		SearchRelevance: 0.9,
		GraphProximity:  0.5,
		CommunityBonus:  0.0,
	}}
	payload, err := encodePrefetchContext(cands, 1, false, false)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Equal(t, "prefetch_context", h.Tool)
	require.Equal(t, "1", h.Meta["total"])
	require.Equal(t, "false", h.Meta["truncated"])
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "matches task keyword", rows[0]["reason"])
	require.Equal(t, "0.825", rows[0]["confidence"])
}

// TestEncodePrefetchContext_IncludeSourceAddsField pins the schema
// flip when include_source is set — the encoder must add a `source`
// column rather than emit it as a meta field, so a decoder iterating
// rows sees the source inline.
func TestEncodePrefetchContext_IncludeSourceAddsField(t *testing.T) {
	n := newTestNode("a.go::Foo", "Foo", graph.KindFunction, "a.go", 10)
	cands := []prefetchCandidate{{
		Node:      n,
		ID:        n.ID,
		Kind:      string(n.Kind),
		FilePath:  n.FilePath,
		StartLine: n.StartLine,
		Source:    "func Foo() {}\n",
	}}
	payload, err := encodePrefetchContext(cands, 1, false, true)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Contains(t, h.Fields, "source")
	rows, _ := dec.All()
	require.Equal(t, "func Foo() {}\n", rows[0]["source"])
}

func TestEncodeChangeImpact_SummaryAndEntries(t *testing.T) {
	result := map[string]any{
		"risk":                 analysis.RiskHigh,
		"summary":              "high blast radius",
		"total_affected":       2,
		"cross_repo_impact":    false,
		"affected_processes":   []string{"checkout", "billing"},
		"affected_communities": []string{"core"},
		"test_files":           []string{"foo_test.go"},
		"by_depth": map[int][]analysis.ImpactEntry{
			1: {{ID: "a.go::Foo", Name: "Foo", Kind: "function", FilePath: "a.go", Line: 10, EdgeConfidence: 0.95, ConfidenceLabel: "EXTRACTED"}},
			2: {{ID: "b.go::Bar", Name: "Bar", Kind: "method", FilePath: "b.go", Line: 22}},
		},
		"cross_community_warning": "",
		"community_note":          "change is community-local",
	}
	payload, err := encodeChangeImpact(result)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))

	// Section 1 — summary.
	h, _ := dec.Header()
	require.Equal(t, "explain_change_impact.summary", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, string(analysis.RiskHigh), rows[0]["risk"])
	require.Equal(t, "checkout,billing", rows[0]["processes"])

	// Section 2 — entries.
	h, err = dec.NextSection()
	require.NoError(t, err)
	require.Equal(t, "explain_change_impact.entries", h.Tool)
	rows, _ = dec.All()
	require.Len(t, rows, 2)
	require.Equal(t, "1", rows[0]["depth"])
	require.Equal(t, "a.go::Foo", rows[0]["id"])
	require.Equal(t, "2", rows[1]["depth"])
}

// TestEncodeChangeImpact_ContractsSection emits the contract-impact
// counters when the JSON path attaches `contract_impact`. Skipping
// this section when the field is absent is also pinned here.
func TestEncodeChangeImpact_ContractsSection(t *testing.T) {
	result := map[string]any{
		"risk":           analysis.RiskMedium,
		"summary":        "ok",
		"total_affected": 0,
		"by_depth":       map[int][]analysis.ImpactEntry{},
		"contract_impact": &contractImpact{
			Affected: []contractImpactEntry{{ContractID: "x"}},
			Breaking: 1,
			Warning:  2,
			Info:     3,
		},
	}
	payload, err := encodeChangeImpact(result)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	// Read summary, then advance past entries to reach contracts.
	_, _ = dec.Header()
	_, _ = dec.All()
	_, err = dec.NextSection() // entries
	require.NoError(t, err)
	_, _ = dec.All()
	h, err := dec.NextSection() // contracts
	require.NoError(t, err)
	require.Equal(t, "explain_change_impact.contracts", h.Tool)
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "1", rows[0]["breaking"])
	require.Equal(t, "1", rows[0]["affected"])
}

func TestEncodeCheckGuards_RowsAndMeta(t *testing.T) {
	violations := []analysis.GuardViolation{
		{RuleName: "no_cross_layer", Kind: "boundary", Description: "mcp imports daemon"},
	}
	payload, err := encodeCheckGuards(violations, false)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	h, _ := dec.Header()
	require.Equal(t, "check_guards", h.Tool)
	require.Equal(t, "1", h.Meta["total"])
	rows, _ := dec.All()
	require.Len(t, rows, 1)
	require.Equal(t, "no_cross_layer", rows[0]["rule_name"])
	// Description is a row value (tab-delimited) so it can carry
	// spaces; pin that here so a future schema rev can't accidentally
	// move it into meta.
	require.Equal(t, "mcp imports daemon", rows[0]["description"])

	// No-rules path: status is a single-token meta flag (the wire
	// header parser splits on raw spaces, so multi-word values would
	// corrupt the header).
	payload, err = encodeCheckGuards(nil, true)
	require.NoError(t, err)
	dec = wire.NewDecoder(strings.NewReader(string(payload)))
	h, err = dec.Header()
	require.NoError(t, err)
	require.Equal(t, "0", h.Meta["total"])
	require.Equal(t, "no_rules_configured", h.Meta["status"])
}

func TestEncodeFeedbackQuery_AllSectionsEmittedEvenWhenEmpty(t *testing.T) {
	stats := map[string]any{
		"total_entries": 5,
		"accuracy":      0.8,
		"most_useful":   []any{},
		"most_missed":   []any{},
		"most_demoted":  []any{},
	}
	payload, err := encodeFeedbackQuery(stats)
	require.NoError(t, err)
	dec := wire.NewDecoder(strings.NewReader(string(payload)))
	tools := []string{}
	h, err := dec.Header()
	require.NoError(t, err)
	tools = append(tools, h.Tool)
	_, _ = dec.All()
	for {
		h, err := dec.NextSection()
		if err != nil {
			break
		}
		tools = append(tools, h.Tool)
		_, _ = dec.All()
	}
	require.Equal(t, []string{
		"feedback.summary",
		"feedback.most_useful",
		"feedback.most_missed",
		"feedback.most_demoted",
	}, tools)
}
