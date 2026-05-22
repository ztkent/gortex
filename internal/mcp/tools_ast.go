package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
)

// registerASTTools wires the `search_ast` MCP tool: a structural,
// graph-aware code search powered by tree-sitter queries.
//
// Two surfaces, exposed through one tool:
//   1. Bundled detectors (`detector: "<name>"`) — pre-baked rules
//      for high-signal anti-patterns. Cross-language by design;
//      one detector ships per-language patterns and the engine
//      picks the right one per file.
//   2. Raw tree-sitter S-expression patterns (`pattern: "..."`,
//      `language: "..."`) for callers who want full power. The
//      pattern syntax is tree-sitter's standard query language —
//      capture nodes with `@name`, anchor with `@match`, predicates
//      `(#eq? @x "literal")` / `(#match? @x "regex")`.
//
// Beyond ast-grep's surface, every match is enriched with
//   - `symbol_id` / `symbol_name` — the enclosing function/method/
//     closure resolved from the graph at result time.
//   - graph-aware filters: scope by `path_prefix`, `language`,
//     `repo` / `project` / `ref`, and `min_fan_in_of_enclosing_func`.
//   - `excludes_tests` defaulting to true for detectors so test
//     fixtures don't drown real findings.
func (s *Server) registerASTTools() {
	s.addTool(
		mcp.NewTool("search_ast",
			mcp.WithDescription(buildSearchASTDescription()),
			mcp.WithString("pattern", mcp.Description("Tree-sitter S-expression query. Capture nodes with `@name`, anchor the match span with `@match`. Predicates: `(#eq? @x \"literal\")`, `(#match? @x \"regex\")`. Mutually exclusive with `detector`.")),
			mcp.WithString("detector", mcp.Description("Bundled rule name. Run with no args (or with `detector: \"\"`) and check the description for the canonical list. Mutually exclusive with `pattern`.")),
			mcp.WithString("language", mcp.Description("Restrict pattern matching to a single language (\"go\", \"python\", \"javascript\", \"typescript\", \"ruby\", \"java\", \"kotlin\", \"scala\", \"rust\", \"elixir\", \"php\", \"c\", \"cpp\", \"csharp\", \"bash\"). Required when `pattern` is set; ignored for detectors (the detector decides which languages to scan).")),
			mcp.WithString("path_prefix", mcp.Description("Restrict the file set to graph paths under this prefix (e.g. `internal/payment/`).")),
			mcp.WithBoolean("exclude_tests", mcp.Description("Drop matches in test files (`_test.go`, `*.spec.ts`, `tests/`, …). Defaults to true for detectors, false for raw patterns.")),
			mcp.WithNumber("min_fan_in_of_enclosing_func", mcp.Description("Only return matches whose enclosing function has at least this many incoming edges (callers + references). Useful for narrowing audits to load-bearing code paths.")),
			mcp.WithNumber("limit", mcp.Description("Maximum matches to return (default: 50)")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleSearchAST,
	)
}

// buildSearchASTDescription assembles a tool description that
// enumerates every bundled detector with its severity and supported
// languages. Hand-rolled at call time so adding a detector to
// astquery.detectors.go automatically updates the agent-visible
// docs without a parallel doc edit.
func buildSearchASTDescription() string {
	var b strings.Builder
	b.WriteString("Structural, graph-aware code search. ")
	b.WriteString("Run a tree-sitter pattern (`pattern: \"...\"`) or a bundled detector (`detector: \"<name>\"`) ")
	b.WriteString("across every indexed file in scope. Each result is enriched with the enclosing function's `symbol_id` ")
	b.WriteString("so you can chain straight into `find_usages`, `verify_change`, or `apply_code_action`.\n\n")
	b.WriteString("Graph-aware filters that ast-grep can't express: `path_prefix`, `repo`/`project`/`ref`, `min_fan_in_of_enclosing_func`. ")
	b.WriteString("Test files are excluded by default for detectors; opt back in via `exclude_tests: false`.\n\n")
	b.WriteString("**Bundled detectors:**\n")
	for _, d := range astquery.DescribeDetectors() {
		fmt.Fprintf(&b, "- `%s` (%s) — %s [%s]\n",
			d.Name, d.Severity, d.Description, strings.Join(d.Languages, ", "))
	}
	b.WriteString("\n**Raw pattern syntax:** standard tree-sitter S-expression queries. Anchor the match span with `@match`. ")
	b.WriteString("Predicates: `(#eq? @x \"literal\")`, `(#match? @x \"regex\")`. ")
	b.WriteString("Example: `((call_expression function: (identifier) @fn) @match (#eq? @fn \"panic\"))` finds every direct panic() call.")
	return b.String()
}

// handleSearchAST is the MCP entry point. It builds the target file
// list from the graph (applying scope predicates), wires a graph-
// backed SymbolLookup, runs the engine, and applies post-match graph
// filters (currently `min_fan_in_of_enclosing_func`) before
// returning. Stays single-pass over the graph's KindFile nodes so
// even very large indexes don't pay multiple O(n) walks.
func (s *Server) handleSearchAST(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pattern := strings.TrimSpace(stringArg(args, "pattern"))
	detector := strings.TrimSpace(stringArg(args, "detector"))
	if pattern == "" && detector == "" {
		return mcp.NewToolResultError("search_ast: either `pattern` or `detector` is required (call with no args to see the bundled detector list in the tool description)"), nil
	}
	if pattern != "" && detector != "" {
		return mcp.NewToolResultError("search_ast: `pattern` and `detector` are mutually exclusive"), nil
	}

	language := strings.ToLower(strings.TrimSpace(stringArg(args, "language")))
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))
	limit := intArg(args, "limit", 0)
	excludeTests, excludeTestsSet := boolArg(args, "exclude_tests")
	minFanIn := intArg(args, "min_fan_in_of_enclosing_func", 0)

	if pattern != "" && language == "" {
		return mcp.NewToolResultError("search_ast: `language` is required when using a raw `pattern`"), nil
	}

	allowedRepos, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	targets, err := s.buildASTTargets(language, pathPrefix, allowedRepos)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Build a per-file enclosing-symbol index lazily. Each file
	// is a small list of function/method/closure nodes; the
	// lookup is amortised by caching the per-file index on first
	// hit. The graph walk is single-pass so even big indexes
	// pay it once per `search_ast` call.
	fileSymbols := s.buildFileSymbolIndex(targets)
	lookup := func(graphPath string, line int) (string, string) {
		idx := fileSymbols[graphPath]
		if idx == nil {
			return "", ""
		}
		return idx.find(line)
	}

	opts := astquery.Options{
		Pattern:      pattern,
		Detector:     detector,
		Language:     language,
		Targets:      targets,
		SymbolLookup: lookup,
		Resolver:     astquery.DefaultLanguageResolver,
		Limit:        limit,
	}
	// Honor explicit override; otherwise let the engine apply
	// its per-mode default (true for detectors, false for raw
	// patterns).
	if excludeTestsSet {
		opts.ExcludeTests = excludeTests
	} else if detector != "" {
		opts.ExcludeTests = true
	}

	res, runErr := astquery.Run(ctx, opts)
	if runErr != nil {
		return mcp.NewToolResultError(runErr.Error()), nil
	}

	if minFanIn > 0 {
		res.Matches = filterByMinFanIn(s.graph, res.Matches, minFanIn)
		res.Total = len(res.Matches)
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"matches":      res.Matches,
		"total":        res.Total,
		"truncated":    res.Truncated,
		"files_walked": res.FilesWalked,
		"errors":       res.Errors,
	})
}

// buildASTTargets walks the graph's KindFile nodes once and assembles
// the `Target` list the engine expects, applying language /
// path_prefix / repo filters before any tree-sitter parse fires.
//
// Path resolution: KindFile nodes carry repo-prefixed paths; the
// engine needs absolute paths to read file bytes, so we resolve via
// `s.resolveGraphPath` (which knows the repo roots).
func (s *Server) buildASTTargets(language, pathPrefix string, allowedRepos map[string]bool) ([]astquery.Target, error) {
	if s.graph == nil {
		return nil, fmt.Errorf("search_ast: no graph available")
	}
	out := make([]astquery.Target, 0, 256)
	for _, n := range s.graph.AllNodes() {
		if n.Kind != graph.KindFile {
			continue
		}
		if allowedRepos != nil && n.RepoPrefix != "" && !allowedRepos[n.RepoPrefix] {
			continue
		}
		if language != "" && !strings.EqualFold(n.Language, language) {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		abs, err := s.resolveNodePath(n)
		if err != nil {
			// Indexed file whose repo we can't currently
			// resolve (rare; happens during an in-flight
			// repo eviction). Skip rather than fail the run.
			continue
		}
		out = append(out, astquery.Target{
			AbsPath:   abs,
			GraphPath: n.FilePath,
			Language:  strings.ToLower(n.Language),
		})
	}
	// Stable order so identical inputs produce identical outputs
	// across daemon restarts. Cheap; the file list is bounded.
	sort.Slice(out, func(i, j int) bool { return out[i].GraphPath < out[j].GraphPath })
	return out, nil
}

// filterByMinFanIn drops matches whose enclosing symbol has fewer
// than `min` incoming edges. Without an enclosing symbol, the
// match is preserved (we'd otherwise silently swallow file-level
// matches that legitimately have no caller graph).
func filterByMinFanIn(g *graph.Graph, matches []astquery.Match, min int) []astquery.Match {
	if g == nil || min <= 0 {
		return matches
	}
	cache := make(map[string]int, len(matches))
	out := matches[:0]
	for _, m := range matches {
		if m.SymbolID == "" {
			out = append(out, m)
			continue
		}
		fanIn, ok := cache[m.SymbolID]
		if !ok {
			fanIn = len(g.GetInEdges(m.SymbolID))
			cache[m.SymbolID] = fanIn
		}
		if fanIn >= min {
			out = append(out, m)
		}
	}
	return out
}

// boolArg returns (value, set) — set is false when the caller didn't
// pass the key, so we can distinguish "unset" from "explicitly false".
func boolArg(args map[string]any, key string) (bool, bool) {
	raw, ok := args[key]
	if !ok {
		return false, false
	}
	if v, ok := raw.(bool); ok {
		return v, true
	}
	return false, false
}

// intArg pulls an int from the args map with a default. Tolerates
// the float64 unmarshalling MCP / JSON does on numeric values.
func intArg(args map[string]any, key string, def int) int {
	raw, ok := args[key]
	if !ok {
		return def
	}
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}
