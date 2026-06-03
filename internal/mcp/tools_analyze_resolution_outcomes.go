package mcp

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// Structured resolver-suppression taxonomy. When the resolver leaves a
// call / reference edge on an `unresolved::` placeholder it records no
// reason — an agent only sees that the edge is unresolved, not *why*.
// This analyzer reconstructs the why from the graph: for each unresolved
// edge it looks up the name's definition candidates and classifies the
// outcome. The reasons reflect Gortex's name-based resolver model (not a
// C++ overload set), so the taxonomy is honest about how this resolver
// actually gives up.
const (
	// outcomeAmbiguousMultiMatch: two or more same-name, same-language
	// definitions exist — the resolver could not pick one and punted.
	outcomeAmbiguousMultiMatch = "ambiguous_multi_match"
	// outcomeCandidateOutOfScope: exactly one same-language definition
	// exists but the edge stayed unresolved — it was outside the caller's
	// resolution scope (cross-package guard, reachability prune, or a
	// receiver-type mismatch).
	outcomeCandidateOutOfScope = "candidate_out_of_scope"
	// outcomeCrossLanguageOnly: the only definitions of this name are in a
	// different language family, so the language gate suppressed the link.
	outcomeCrossLanguageOnly = "cross_language_only"
	// outcomeStubOnly: the name matches only stub / external-placeholder
	// nodes — no real definition is indexed.
	outcomeStubOnly = "stub_only"
	// outcomeNoDefinition: no definition of this name exists in the graph
	// at all — a genuinely external or un-indexed target.
	outcomeNoDefinition = "no_definition"
)

// handleAnalyzeResolutionOutcomes classifies every unresolved call /
// reference edge by the structured reason the resolver gave up, and
// returns a per-reason rollup plus example rows. Optional `reason`
// filters to one outcome; optional `limit` caps the example rows.
func (s *Server) handleAnalyzeResolutionOutcomes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	reasonFilter := strings.TrimSpace(stringArg(args, "reason"))
	limit := intArg(args, "limit", 50)

	type row struct {
		From       string `json:"from"`
		To         string `json:"to"`
		Kind       string `json:"edge_kind"`
		Name       string `json:"name"`
		Reason     string `json:"reason"`
		Candidates int    `json:"candidates"`
	}

	// Collect unresolved edges + the From IDs (for language lookup).
	type pending struct {
		edge *graph.Edge
		name string
	}
	var todo []pending
	fromIDs := map[string]struct{}{}
	for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
		for e := range s.graph.EdgesByKind(kind) {
			if e == nil || !graph.IsUnresolvedTarget(e.To) {
				continue
			}
			name := graph.UnresolvedName(e.To)
			if name == "" {
				continue
			}
			// A receiver-qualified placeholder (`unresolved::*.foo`) keeps
			// its method name after the dot; normalise to the bare name.
			if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
				name = name[i+1:]
			}
			todo = append(todo, pending{edge: e, name: name})
			if e.From != "" {
				fromIDs[e.From] = struct{}{}
			}
		}
	}
	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	fromNodes := s.graph.GetNodesByIDs(fromList)

	byReason := map[string]int{}
	var rows []row
	// Cache name→candidate classification so repeated unresolved names
	// (very common — every call to the same missing function) classify once.
	type cand struct {
		sameLangMin int // realSameLang count is recomputed per caller lang, so cache the raw split
	}
	_ = cand{}
	for _, p := range todo {
		fromLang := ""
		if n := fromNodes[p.edge.From]; n != nil {
			fromLang = n.Language
		}
		reason, ncand := s.classifyUnresolved(p.name, fromLang)
		byReason[reason]++
		if reasonFilter != "" && reason != reasonFilter {
			continue
		}
		if len(rows) < limit {
			rows = append(rows, row{
				From: p.edge.From, To: p.edge.To, Kind: string(p.edge.Kind),
				Name: p.name, Reason: reason, Candidates: ncand,
			})
		}
	}

	if isCompact(req) {
		var b strings.Builder
		reasons := make([]string, 0, len(byReason))
		for r := range byReason {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool { return byReason[reasons[i]] > byReason[reasons[j]] })
		for _, r := range reasons {
			b.WriteString(r)
			b.WriteString(": ")
			b.WriteString(strconv.Itoa(byReason[r]))
			b.WriteByte('\n')
		}
		if len(byReason) == 0 {
			b.WriteString("no unresolved edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	total := 0
	for _, n := range byReason {
		total += n
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"by_reason": byReason,
		"total":     total,
		"rows":      rows,
	})
}

// classifyUnresolved returns the structured suppression reason for an
// unresolved name relative to the caller's language, plus the number of
// real (non-stub) definition candidates considered.
func (s *Server) classifyUnresolved(name, fromLang string) (reason string, candidates int) {
	var realSameLang, realOtherLang, stubs int
	for _, n := range s.graph.FindNodesByName(name) {
		if n == nil {
			continue
		}
		if graph.IsStub(n.ID) {
			stubs++
			continue
		}
		if !nodeIsDefinitionKind(n.Kind) {
			continue
		}
		if fromLang != "" && n.Language != "" && !sameLanguageFamily(fromLang, n.Language) {
			realOtherLang++
			continue
		}
		realSameLang++
	}
	switch {
	case realSameLang >= 2:
		return outcomeAmbiguousMultiMatch, realSameLang
	case realSameLang == 1:
		return outcomeCandidateOutOfScope, 1
	case realOtherLang >= 1:
		return outcomeCrossLanguageOnly, realOtherLang
	case stubs >= 1:
		return outcomeStubOnly, 0
	default:
		return outcomeNoDefinition, 0
	}
}

// nodeIsDefinitionKind reports whether a node kind is a callable / type
// definition an unresolved call or reference could legitimately bind to.
func nodeIsDefinitionKind(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant, graph.KindField:
		return true
	}
	return false
}

// sameLanguageFamily folds the TS/JS pair so a cross-file TS→JS reference
// is not mis-reported as a cross-language suppression.
func sameLanguageFamily(a, b string) bool {
	if a == b {
		return true
	}
	norm := func(l string) string {
		switch l {
		case "javascript", "typescript", "tsx", "jsx":
			return "jsts"
		}
		return l
	}
	return norm(a) == norm(b)
}
