package mcp

// degrade_shapes.go registers per-tool graceful-degradation policies.
// Shapes live here (not next to the handlers) so the priority logic
// is auditable in one place — drift between policies is the most
// likely failure mode, and a single file makes it obvious when, say,
// `get_callers` and `find_usages` start prioritising rows differently
// without a deliberate reason.
//
// The three tiers, applied in reverse order under pressure:
//
//   1. Keep — the structural / semantic answer the agent asked for.
//   2. Drop second — useful context that survives if budget allows.
//   3. Drop first — high-noise / low-signal rows agents almost never
//      need on the first response (params, closures, generic params,
//      param_of edges, value_flow, typed_as, etc.).

func init() {
	// File-shaped tools: nodes/edges of every kind. Functions and
	// types are the structural answer; params and closures are noise
	// in the first response.
	fileShape := DegradeShape{
		MetaStrip: []string{"doc", "meta"},
		TierFunc:  symbolKindTier,
	}
	registerDegradeShape("get_file_summary", fileShape)
	registerDegradeShape("get_editing_context", fileShape)

	// Subgraph-returning tools: the graph topology IS the answer, so
	// edge tier matters more than node kind. Drop low-confidence /
	// noise edges first; preserve calls / implements / extends.
	subGraphShape := DegradeShape{
		MetaStrip: []string{"doc", "meta"},
		TierFunc:  edgeKindTier,
	}
	registerDegradeShape("find_usages", subGraphShape)
	registerDegradeShape("get_callers", subGraphShape)
	registerDegradeShape("get_call_chain", subGraphShape)
	registerDegradeShape("get_dependencies", subGraphShape)
	registerDegradeShape("get_dependents", subGraphShape)
	registerDegradeShape("find_implementations", subGraphShape)
	registerDegradeShape("find_overrides", subGraphShape)
	registerDegradeShape("get_class_hierarchy", subGraphShape)
	registerDegradeShape("get_cluster", subGraphShape)

	// search_symbols / winnow_symbols: ranked lists, every row has a
	// `kind` column. Same node-kind tiers as file-shaped tools.
	searchShape := DegradeShape{
		MetaStrip: []string{"doc"},
		TierFunc:  symbolKindTier,
	}
	registerDegradeShape("search_symbols", searchShape)
	registerDegradeShape("winnow_symbols", searchShape)
	registerDegradeShape("prefetch_context", searchShape)

	// contracts list: prefer non-test, non-dependency rows. The
	// verbose response_shape / request_shape strings are the meta
	// to strip first; agents rarely need the full schema for
	// "what contracts exist" questions.
	registerDegradeShape("contracts", DegradeShape{
		MetaStrip: []string{"response_shape", "request_shape", "schema", "provider_schema", "consumer_schema"},
		TierFunc:  contractTier,
	})

	// smart_context: the entry symbol and its closest neighbours are
	// tier-1; tests and cross-repo links are tier-3 noise unless the
	// task touches them.
	registerDegradeShape("smart_context", DegradeShape{
		MetaStrip: []string{"doc", "meta"},
		TierFunc:  smartContextTier,
	})

	// batch_symbols: rows are the answer; meta-strip is the only
	// useful lever (no per-row tier — caller already curated the IDs).
	registerDegradeShape("batch_symbols", DegradeShape{
		MetaStrip: []string{"doc", "meta"},
	})

	// get_symbol_source: single-symbol response. Source is the
	// answer — only meta and per-symbol doc are stripable.
	registerDegradeShape("get_symbol_source", DegradeShape{
		MetaStrip: []string{"doc", "meta"},
	})
}

// symbolKindTier classifies a row by its `kind` field for tools that
// return mixed nodes (functions, types, params, …) and edges.
//
//   - tier 1 (keep): function, method, type, interface, contract,
//     calls, implements, extends, defines, member_of
//   - tier 2 (drop second): field, constant, variable, enum_member,
//     import, references, instantiates, provides, consumes, returns
//   - tier 3 (drop first): param, closure, generic_param, file,
//     param_of, typed_as, returns_to, value_flow, arg_of, captures
func symbolKindTier(row map[string]any) int {
	kind, _ := row["kind"].(string)
	switch kind {
	case "function", "method", "type", "interface", "contract",
		"calls", "implements", "extends", "defines", "member_of":
		return 1
	case "field", "constant", "variable", "enum_member", "import",
		"references", "instantiates", "provides", "consumes",
		"returns", "annotated", "throws", "queries":
		return 2
	case "param", "closure", "generic_param", "file",
		"param_of", "typed_as", "returns_to", "value_flow",
		"arg_of", "captures", "reads", "writes":
		return 3
	}
	// Coverage-extension kinds (module, table, column, config_key,
	// flag, event, todo, …) sit in tier 2 by default — useful but
	// not the structural answer most callers ask for.
	return 2
}

// edgeKindTier is symbolKindTier specialised for subgraph payloads
// where the `edges` list is the primary signal. It also folds in the
// `min_tier` confidence — text_matched edges drop before
// ast_inferred, which drop before lsp_resolved.
func edgeKindTier(row map[string]any) int {
	// Confidence column wins when present — false positives drop first.
	if origin, ok := row["origin"].(string); ok {
		switch origin {
		case "text_matched":
			return 3
		case "ast_inferred":
			return 2
		}
	}
	if label, ok := row["confidence_label"].(string); ok {
		switch label {
		case "INFERRED":
			return 2
		}
	}
	// Otherwise fall back to kind-based tiering.
	return symbolKindTier(row)
}

// contractTier prioritises non-test, non-dependency contracts. Test
// fixtures and vendored deps blow up the contracts list on real
// codebases without telling the agent anything actionable.
func contractTier(row map[string]any) int {
	if v, ok := row["is_test_only"].(bool); ok && v {
		return 3
	}
	if t, _ := row["type"].(string); t == "dependency" {
		return 3
	}
	// Meta substring check: vendored paths.
	if file, _ := row["file"].(string); file != "" {
		switch {
		case containsAny(file, "vendor/", "node_modules/", ".venv/", "Pods/"):
			return 3
		}
	}
	return 1
}

// smartContextTier keeps the entry symbol + its direct dependencies
// at tier 1; tests and cross-repo links are tier 3 unless the row
// indicates the task explicitly touches them.
func smartContextTier(row map[string]any) int {
	role, _ := row["role"].(string)
	switch role {
	case "test", "cross_repo":
		return 3
	case "caller", "callee":
		return 2
	}
	return 1
}

// containsAny reports whether s contains any of the given
// substrings. Cheap loop instead of pulling in regexp.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
